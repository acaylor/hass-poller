package main

import (
	"testing"
	"time"

	"hass-poller/internal/config"
	"hass-poller/internal/filter"
	"hass-poller/internal/ha"
)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestBuildMeasurements(t *testing.T) {
	start := ts("2026-06-20T00:00:00Z")
	end := ts("2026-06-21T00:00:00Z")

	history := []ha.HistoryState{
		// In allowlist, changing values.
		{EntityID: "sensor.temp", State: "20.0", Unit: "°C", LastChanged: ts("2026-06-20T01:00:00Z")},
		{EntityID: "sensor.temp", State: "20.0", Unit: "°C", LastChanged: ts("2026-06-20T02:00:00Z")}, // unchanged -> epsilon skip
		{EntityID: "sensor.temp", State: "22.0", Unit: "°C", LastChanged: ts("2026-06-20T03:00:00Z")}, // changed -> keep
		// Out-of-range entry must be excluded.
		{EntityID: "sensor.temp", State: "99.0", Unit: "°C", LastChanged: ts("2026-06-22T00:00:00Z")},
		// Blocked entity must be excluded.
		{EntityID: "sensor.energy_meter", State: "5.0", Unit: "kWh", LastChanged: ts("2026-06-20T01:00:00Z")},
		// Non-numeric must be excluded.
		{EntityID: "sensor.mode", State: "heat", LastChanged: ts("2026-06-20T01:00:00Z")},
	}

	f := filter.NewGlobFilter([]string{"sensor.*"}, []string{"sensor.energy_*"})
	cfg := config.Config{EpsilonDefault: 0}

	got := buildMeasurements(history, f, start, end, cfg, false)

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
		{EntityID: "sensor.noisy", State: "10.00", LastChanged: ts("2026-06-20T01:00:00Z")},
		{EntityID: "sensor.noisy", State: "10.02", LastChanged: ts("2026-06-20T02:00:00Z")}, // within epsilon 0.1
		{EntityID: "sensor.noisy", State: "10.30", LastChanged: ts("2026-06-20T03:00:00Z")}, // beyond epsilon
	}
	f := filter.NewGlobFilter([]string{"sensor.*"}, nil)
	cfg := config.Config{
		EpsilonDefault:   0,
		EpsilonOverrides: map[string]float64{"sensor.noisy": 0.1},
	}

	withEps := buildMeasurements(history, f, start, end, cfg, false)
	if len(withEps) != 2 {
		t.Fatalf("with epsilon: got %d, want 2 (first + jump past 0.1)", len(withEps))
	}

	noEps := buildMeasurements(history, f, start, end, cfg, true)
	if len(noEps) != 3 {
		t.Fatalf("no-epsilon: got %d, want 3 (every change)", len(noEps))
	}
}

func TestBuildMeasurements_RangeAndDuplicateBoundaries(t *testing.T) {
	start := ts("2026-06-20T00:00:00Z")
	end := start.Add(24 * time.Hour)
	first := ha.HistoryState{EntityID: "sensor.temp", State: "20", Unit: "°C", LastChanged: start}
	history := []ha.HistoryState{
		{EntityID: "sensor.temp", State: "20", Unit: "°C", LastChanged: start.Add(-time.Hour)},
		first, first, // Repeated boundary record from adjacent history requests.
		{EntityID: "sensor.temp", State: "20", Unit: "°F", LastChanged: start.Add(time.Hour)},
		{EntityID: "sensor.temp", State: "21", Unit: "°F", LastChanged: end},
	}
	for _, noEpsilon := range []bool{false, true} {
		got := buildMeasurements(history, filter.NewGlobFilter(nil, nil), start, end, config.Config{}, noEpsilon)
		if len(got) != 2 || !got[0].Timestamp.Equal(start) || got[1].Unit != "°F" {
			t.Fatalf("no-epsilon=%v: got %+v, want first in-range row and unit change", noEpsilon, got)
		}
	}
}
