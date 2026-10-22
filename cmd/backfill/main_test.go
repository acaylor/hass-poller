package main

import (
	"reflect"
	"sort"
	"testing"
	"time"

	"hass-poller/internal/config"
	"hass-poller/internal/engine"
	"hass-poller/internal/filter"
	"hass-poller/internal/ha"
	"hass-poller/internal/store"
)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// buildMeasurements processes history as a single chunk ending at end.
func buildMeasurements(history []ha.HistoryState, f *filter.GlobFilter, start, end time.Time, cfg config.Config, noEpsilon bool) []store.Measurement {
	return newMeasurementBuilder(f, start, cfg, noEpsilon).add(history, end)
}

// withoutHeartbeats drops rows that repeat the entity's previous row one
// heartbeat interval later, leaving the recorded changes.
func withoutHeartbeats(measurements []store.Measurement) []store.Measurement {
	var out []store.Measurement
	last := map[string]store.Measurement{}
	for _, m := range measurements {
		prev, ok := last[m.EntityID]
		last[m.EntityID] = m
		if ok && m.Value == prev.Value && m.Unit == prev.Unit &&
			m.Timestamp.Sub(prev.Timestamp) == engine.HeartbeatInterval {
			continue
		}
		out = append(out, m)
	}
	return out
}

func TestBuildMeasurements(t *testing.T) {
	start := ts("2026-06-20T00:00:00Z")
	end := ts("2026-06-21T00:00:00Z")

	history := []ha.HistoryState{
		// In allowlist, changing values.
		{EntityID: "sensor.temp", State: "20.0", Unit: "°C", LastUpdated: ts("2026-06-20T01:00:00Z")},
		{EntityID: "sensor.temp", State: "20.0", Unit: "°C", LastUpdated: ts("2026-06-20T02:00:00Z")}, // unchanged -> epsilon skip
		{EntityID: "sensor.temp", State: "22.0", Unit: "°C", LastUpdated: ts("2026-06-20T03:00:00Z")}, // changed -> keep
		// Out-of-range entry must be excluded.
		{EntityID: "sensor.temp", State: "99.0", Unit: "°C", LastUpdated: ts("2026-06-22T00:00:00Z")},
		// Blocked entity must be excluded.
		{EntityID: "sensor.energy_meter", State: "5.0", Unit: "kWh", LastUpdated: ts("2026-06-20T01:00:00Z")},
		// Non-numeric must be excluded.
		{EntityID: "sensor.mode", State: "heat", LastUpdated: ts("2026-06-20T01:00:00Z")},
	}

	f := filter.NewGlobFilter([]string{"sensor.*"}, []string{"sensor.energy_*"})
	cfg := config.Config{EpsilonDefault: 0}

	got := withoutHeartbeats(buildMeasurements(history, f, start, end, cfg, false))

	if len(got) != 2 {
		t.Fatalf("got %d measurements, want 2: %+v", len(got), got)
	}
	if got[0].EntityID != "sensor.temp" || got[0].Value != 20.0 {
		t.Errorf("got[0] = %+v, want sensor.temp 20.0", got[0])
	}
	if got[1].Value != 22.0 || got[1].Unit != "°C" {
		t.Errorf("got[1] = %+v, want 22.0 °C", got[1])
	}
}

func TestBuildMeasurements_EpsilonOverrideAndNoEpsilon(t *testing.T) {
	start := ts("2026-06-20T00:00:00Z")
	end := ts("2026-06-21T00:00:00Z")

	history := []ha.HistoryState{
		{EntityID: "sensor.noisy", State: "10.00", LastUpdated: ts("2026-06-20T01:00:00Z")},
		{EntityID: "sensor.noisy", State: "10.02", LastUpdated: ts("2026-06-20T02:05:00Z")}, // within epsilon 0.1
		{EntityID: "sensor.noisy", State: "10.30", LastUpdated: ts("2026-06-20T03:05:00Z")}, // beyond epsilon
	}
	f := filter.NewGlobFilter([]string{"sensor.*"}, nil)
	cfg := config.Config{
		EpsilonDefault:   0,
		EpsilonOverrides: map[string]float64{"sensor.noisy": 0.1},
	}

	// Count the records written at their own time; a suppressed value is
	// only written later, by a heartbeat.
	recorded := func(measurements []store.Measurement) int {
		n := 0
		for _, m := range measurements {
			for _, h := range history {
				if m.Timestamp.Equal(h.LastUpdated) {
					n++
				}
			}
		}
		return n
	}
	withEps := buildMeasurements(history, f, start, end, cfg, false)
	if got := recorded(withEps); got != 2 {
		t.Fatalf("with epsilon: got %d, want 2 (first + jump past 0.1)", got)
	}

	noEps := buildMeasurements(history, f, start, end, cfg, true)
	if got := recorded(noEps); got != 3 {
		t.Fatalf("no-epsilon: got %d, want 3 (every change)", got)
	}
}

func TestBuildMeasurements_RangeAndDuplicateBoundaries(t *testing.T) {
	start := ts("2026-06-20T00:00:00Z")
	end := start.Add(24 * time.Hour)
	first := ha.HistoryState{EntityID: "sensor.temp", State: "20", Unit: "°C", LastUpdated: start}
	history := []ha.HistoryState{
		{EntityID: "sensor.temp", State: "20", Unit: "°C", LastUpdated: start.Add(-time.Hour)},
		first, first, // Repeated boundary record from adjacent history requests.
		{EntityID: "sensor.temp", State: "20", Unit: "°F", LastUpdated: start.Add(time.Hour)},
		{EntityID: "sensor.temp", State: "21", Unit: "°F", LastUpdated: end},
	}
	for _, noEpsilon := range []bool{false, true} {
		got := withoutHeartbeats(buildMeasurements(history, filter.NewGlobFilter(nil, nil), start, end, config.Config{}, noEpsilon))
		if len(got) != 2 || !got[0].Timestamp.Equal(start) || got[1].Unit != "°F" {
			t.Fatalf("no-epsilon=%v: got %+v, want first in-range row and unit change", noEpsilon, got)
		}
	}
}

func TestBuildMeasurements_HeartbeatsAndStartValue(t *testing.T) {
	start := ts("2026-06-20T00:10:00Z")
	end := ts("2026-06-20T01:20:00Z")
	history := []ha.HistoryState{
		// Only the latest record before start is in effect at start.
		{EntityID: "sensor.temp", State: "18", Unit: "°C", LastUpdated: ts("2026-06-19T22:00:00Z")},
		{EntityID: "sensor.temp", State: "19", Unit: "°C", LastUpdated: ts("2026-06-19T23:00:00Z")},
		{EntityID: "sensor.temp", State: "20", Unit: "°C", LastUpdated: ts("2026-06-20T00:30:00Z")},
		// A non-numeric state stops heartbeats until the next numeric value.
		{EntityID: "sensor.temp", State: "unavailable", LastUpdated: ts("2026-06-20T00:50:00Z")},
		{EntityID: "sensor.temp", State: "20", Unit: "°C", LastUpdated: ts("2026-06-20T01:10:00Z")},
	}
	got := buildMeasurements(history, filter.NewGlobFilter(nil, nil), start, end, config.Config{}, false)

	want := []store.Measurement{
		{Timestamp: start, EntityID: "sensor.temp", Value: 19, Unit: "°C"},
		{Timestamp: ts("2026-06-20T00:25:00Z"), EntityID: "sensor.temp", Value: 19, Unit: "°C"},
		{Timestamp: ts("2026-06-20T00:30:00Z"), EntityID: "sensor.temp", Value: 20, Unit: "°C"},
		{Timestamp: ts("2026-06-20T00:45:00Z"), EntityID: "sensor.temp", Value: 20, Unit: "°C"},
		{Timestamp: ts("2026-06-20T01:10:00Z"), EntityID: "sensor.temp", Value: 20, Unit: "°C"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestBuildMeasurements_HeartbeatWritesSuppressedValue(t *testing.T) {
	start := ts("2026-06-20T00:00:00Z")
	end := ts("2026-06-20T00:50:00Z")
	history := []ha.HistoryState{
		{EntityID: "sensor.temp", State: "20", Unit: "°C", LastUpdated: start},
		// Within epsilon of the last written value, so only a heartbeat writes it.
		{EntityID: "sensor.temp", State: "20.5", Unit: "°C", LastUpdated: ts("2026-06-20T00:01:00Z")},
		{EntityID: "sensor.temp", State: "20.9", Unit: "°C", LastUpdated: ts("2026-06-20T00:20:00Z")},
	}
	got := buildMeasurements(history, filter.NewGlobFilter(nil, nil), start, end, config.Config{EpsilonDefault: 1}, false)

	want := []store.Measurement{
		{Timestamp: start, EntityID: "sensor.temp", Value: 20, Unit: "°C"},
		{Timestamp: ts("2026-06-20T00:15:00Z"), EntityID: "sensor.temp", Value: 20.5, Unit: "°C"},
		{Timestamp: ts("2026-06-20T00:30:00Z"), EntityID: "sensor.temp", Value: 20.9, Unit: "°C"},
		{Timestamp: ts("2026-06-20T00:45:00Z"), EntityID: "sensor.temp", Value: 20.9, Unit: "°C"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestBuildMeasurements_UnitUpdateWithEarlierLastChanged(t *testing.T) {
	start := ts("2026-06-20T00:00:00Z")
	end := ts("2026-06-20T00:30:00Z")
	// Decoded from history whose in-range unit update kept a last_changed from
	// before start; FetchHistory timestamps it by last_updated.
	history := []ha.HistoryState{
		{EntityID: "sensor.temp", State: "20", Unit: "°C", LastUpdated: start},
		{EntityID: "sensor.temp", State: "20", Unit: "°F", LastUpdated: ts("2026-06-20T00:10:00Z")},
	}
	got := buildMeasurements(history, filter.NewGlobFilter(nil, nil), start, end, config.Config{}, false)
	want := []store.Measurement{
		{Timestamp: start, EntityID: "sensor.temp", Value: 20, Unit: "°C"},
		{Timestamp: ts("2026-06-20T00:10:00Z"), EntityID: "sensor.temp", Value: 20, Unit: "°F"},
		{Timestamp: ts("2026-06-20T00:25:00Z"), EntityID: "sensor.temp", Value: 20, Unit: "°F"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestBackfillChunks_MatchesSinglePass(t *testing.T) {
	start := ts("2026-06-20T06:00:00Z")
	end := ts("2026-06-23T00:00:00Z")
	// Recorded updates; the fake history API prepends the state in effect at
	// each request's start, timestamped at that start, as Home Assistant does.
	recorded := []ha.HistoryState{
		{EntityID: "sensor.temp", State: "20", Unit: "°C", LastUpdated: ts("2026-06-19T12:00:00Z")},
		{EntityID: "sensor.temp", State: "21", Unit: "°C", LastUpdated: ts("2026-06-20T23:50:00Z")},
		{EntityID: "sensor.temp", State: "21", Unit: "°F", LastUpdated: ts("2026-06-21T08:00:00Z")},
		{EntityID: "sensor.temp", State: "unavailable", LastUpdated: ts("2026-06-22T01:00:00Z")},
		{EntityID: "sensor.temp", State: "70", Unit: "°F", LastUpdated: ts("2026-06-22T03:00:00Z")},
		{EntityID: "sensor.power", State: "5", Unit: "W", LastUpdated: ts("2026-06-20T07:00:00Z")},
	}
	fetch := func(chunkStart, chunkEnd time.Time) ([]ha.HistoryState, error) {
		var out []ha.HistoryState
		inEffect := map[string]ha.HistoryState{}
		for _, h := range recorded {
			if !h.LastUpdated.After(chunkStart) {
				h.LastUpdated = chunkStart
				inEffect[h.EntityID] = h
			}
		}
		for _, h := range inEffect {
			out = append(out, h)
		}
		for _, h := range recorded {
			if h.LastUpdated.After(chunkStart) && h.LastUpdated.Before(chunkEnd) {
				out = append(out, h)
			}
		}
		return out, nil
	}
	f := filter.NewGlobFilter(nil, nil)

	for _, noEpsilon := range []bool{false, true} {
		var chunked []store.Measurement
		var chunks int
		_, err := backfillChunks(start, end, fetch, newMeasurementBuilder(f, start, config.Config{}, noEpsilon),
			func(m []store.Measurement) error {
				chunks++
				chunked = append(chunked, m...)
				return nil
			})
		if err != nil {
			t.Fatal(err)
		}
		whole, _ := fetch(start, end)
		single := buildMeasurements(whole, f, start, end, config.Config{}, noEpsilon)
		sortMeasurements(chunked)
		sortMeasurements(single)
		if chunks != 3 || !reflect.DeepEqual(chunked, single) {
			t.Fatalf("no-epsilon=%v: %d chunks gave\n%+v\nsingle pass gave\n%+v", noEpsilon, chunks, chunked, single)
		}
	}
}

func sortMeasurements(m []store.Measurement) {
	sort.Slice(m, func(i, j int) bool {
		if m[i].EntityID != m[j].EntityID {
			return m[i].EntityID < m[j].EntityID
		}
		return m[i].Timestamp.Before(m[j].Timestamp)
	})
}
