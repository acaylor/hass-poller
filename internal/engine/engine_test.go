package engine

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"
	"testing/synctest"
	"time"

	"hass-poller/internal/filter"
	"hass-poller/internal/ha"
)

// testEngine builds an Engine wired up with the supplied fakes. Defaults are
// chosen so callers only need to override what their case cares about.
func testEngine(
	fetcher StatesFetcher,
	st MeasurementStore,
	allow, block []string,
	epsDefault float64,
	overrides map[string]float64,
) *Engine {
	return New(
		fetcher,
		filter.NewGlobFilter(allow, block),
		st,
		time.Minute,
		epsDefault,
		overrides,
		log.New(io.Discard, "", 0),
	)
}

func state(id, val, unit string) ha.State {
	return ha.State{
		EntityID:   id,
		State:      val,
		Attributes: ha.Attributes{UnitOfMeasurement: unit},
	}
}

func TestEpsilonFor(t *testing.T) {
	e := &Engine{
		epsilonDefault: 0.01,
		epsilonOverrides: map[string]float64{
			"sensor.kitchen_temperature": 0.05,
			"sensor.outdoor_humidity":    0.0, // explicit zero override
		},
	}
	tests := []struct {
		name     string
		entityID string
		want     float64
	}{
		{"override applies", "sensor.kitchen_temperature", 0.05},
		{"explicit zero override is honored", "sensor.outdoor_humidity", 0.0},
		{"missing entity falls back to default", "sensor.unrelated", 0.01},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := e.epsilonFor(tt.entityID); got != tt.want {
				t.Errorf("epsilonFor(%q) = %v, want %v", tt.entityID, got, tt.want)
			}
		})
	}
}

func TestEpsilonFor_NilOverrides(t *testing.T) {
	e := &Engine{epsilonDefault: 0.5}
	if got := e.epsilonFor("sensor.anything"); got != 0.5 {
		t.Errorf("epsilonFor with nil overrides = %v, want 0.5", got)
	}
}

func TestShouldWrite(t *testing.T) {
	tests := []struct {
		name             string
		current          float64
		last             float64
		epsilon          float64
		firstObservation bool
		want             bool
	}{
		// First observation always writes
		{"first observation", 20.5, 0, 0, true, true},
		{"first observation with epsilon", 20.5, 0, 1.0, true, true},

		// Strict equality (epsilon=0)
		{"same value eps=0", 20.5, 20.5, 0, false, false},
		{"different value eps=0", 20.5, 20.4, 0, false, true},
		{"tiny change eps=0", 20.5, 20.500000001, 0, false, true},

		// With epsilon threshold
		{"change below epsilon", 20.5, 20.48, 0.05, false, false},
		{"change at epsilon boundary", 20.5, 20.0, 0.5, false, false},
		{"change above epsilon", 20.5, 20.0, 0.05, false, true},
		{"negative change below epsilon", 20.45, 20.48, 0.05, false, false},
		{"negative change above epsilon", 20.0, 20.5, 0.05, false, true},

		// Zero values
		{"zero to zero", 0, 0, 0, false, false},
		{"zero to nonzero", 0.1, 0, 0, false, true},
		{"zero first observation", 0, 0, 0, true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShouldWrite(tt.current, tt.last, tt.epsilon, tt.firstObservation, "", "")
			if got != tt.want {
				t.Errorf("ShouldWrite(%v, %v, %v, %v) = %v, want %v",
					tt.current, tt.last, tt.epsilon, tt.firstObservation, got, tt.want)
			}
		})
	}
}

func TestParseNumericState(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		want   float64
		wantOK bool
	}{
		{"integer", "42", 42, true},
		{"float", "20.5", 20.5, true},
		{"negative", "-3.14", -3.14, true},
		{"zero", "0", 0, true},
		{"whitespace", "  20.5  ", 20.5, true},

		// Rejected values
		{"unknown", "unknown", 0, false},
		{"unavailable", "unavailable", 0, false},
		{"Unknown uppercase", "Unknown", 0, false},
		{"empty", "", 0, false},
		{"text", "on", 0, false},
		{"mixed", "20.5°C", 0, false},
		{"NaN", "NaN", 0, false},
		{"+Inf", "+Inf", 0, false},
		{"-Inf", "-Inf", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseNumericState(tt.raw)
			if ok != tt.wantOK {
				t.Errorf("ParseNumericState(%q) ok = %v, want %v", tt.raw, ok, tt.wantOK)
				return
			}
			if ok && got != tt.want {
				t.Errorf("ParseNumericState(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestRunCycle_FiltersAndParses(t *testing.T) {
	fetcher := &fakeFetcher{
		states: []ha.State{
			state("sensor.kitchen_temperature", "20.5", "°C"),
			state("sensor.outdoor_humidity", "55", "%"),
			state("sensor.broken", "unavailable", ""),
			state("sensor.text", "on", ""),
			state("sensor.energy_total", "123.4", "kWh"), // blocked
			state("binary_sensor.door", "on", ""),        // not in allowlist
		},
	}
	st := &fakeStore{}
	e := testEngine(fetcher, st,
		[]string{"sensor.*"},
		[]string{"sensor.energy_*"},
		0, nil,
	)

	if err := e.runCycle(context.Background()); err != nil {
		t.Fatalf("runCycle: %v", err)
	}

	batch := st.LastBatch()
	if len(batch) != 2 {
		t.Fatalf("inserted %d rows, want 2: %+v", len(batch), batch)
	}

	got := map[string]float64{}
	for _, m := range batch {
		got[m.EntityID] = m.Value
	}
	if got["sensor.kitchen_temperature"] != 20.5 {
		t.Errorf("kitchen_temperature value = %v, want 20.5", got["sensor.kitchen_temperature"])
	}
	if got["sensor.outdoor_humidity"] != 55 {
		t.Errorf("outdoor_humidity value = %v, want 55", got["sensor.outdoor_humidity"])
	}
	if e.LastSuccessfulPoll().IsZero() {
		t.Error("lastPoll not updated on success")
	}
}

func TestRunCycle_EpsilonSkipsUnchanged(t *testing.T) {
	fetcher := &fakeFetcher{
		states: []ha.State{state("sensor.temp", "20.5", "°C")},
	}
	st := &fakeStore{}
	e := testEngine(fetcher, st, []string{"sensor.*"}, nil, 0, nil)

	// First cycle: writes (first observation).
	if err := e.runCycle(context.Background()); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if n := len(st.LastBatch()); n != 1 {
		t.Fatalf("cycle 1 inserted %d, want 1", n)
	}

	// Second cycle, same value: nothing new written.
	if err := e.runCycle(context.Background()); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	batches := st.Batches()
	if len(batches) != 2 {
		t.Fatalf("expected 2 batches, got %d", len(batches))
	}
	if len(batches[1]) != 0 {
		t.Errorf("cycle 2 should be empty, got %+v", batches[1])
	}
}

func TestRunCycle_EpsilonOverrideApplied(t *testing.T) {
	fetcher := &fakeFetcher{
		states: []ha.State{state("sensor.temp", "20.5", "°C")},
	}
	st := &fakeStore{}
	overrides := map[string]float64{"sensor.temp": 0.1}
	e := testEngine(fetcher, st, []string{"sensor.*"}, nil, 0, overrides)

	if err := e.runCycle(context.Background()); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}

	// Tiny change below the per-entity epsilon → skipped.
	fetcher.states = []ha.State{state("sensor.temp", "20.55", "°C")}
	if err := e.runCycle(context.Background()); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if got := len(st.Batches()[1]); got != 0 {
		t.Errorf("change of 0.05 with eps=0.1 should be skipped, got %d rows", got)
	}

	// Change above the per-entity epsilon → written.
	fetcher.states = []ha.State{state("sensor.temp", "21.0", "°C")}
	if err := e.runCycle(context.Background()); err != nil {
		t.Fatalf("cycle 3: %v", err)
	}
	if got := len(st.Batches()[2]); got != 1 {
		t.Errorf("change of 0.5 with eps=0.1 should write, got %d rows", got)
	}
}

func TestRunCycle_FetchErrorPropagates(t *testing.T) {
	fetchErr := errors.New("ha unreachable")
	fetcher := &fakeFetcher{err: fetchErr}
	st := &fakeStore{}
	e := testEngine(fetcher, st, []string{"sensor.*"}, nil, 0, nil)

	err := e.runCycle(context.Background())
	if !errors.Is(err, fetchErr) {
		t.Errorf("runCycle err = %v, want %v", err, fetchErr)
	}
	if len(st.Batches()) != 0 {
		t.Errorf("store should not be called on fetch failure, got %d batches", len(st.Batches()))
	}
	if !e.LastSuccessfulPoll().IsZero() {
		t.Error("lastPoll should not advance on fetch failure")
	}
}

func TestRunCycle_InsertErrorPropagates(t *testing.T) {
	insertErr := errors.New("db down")
	fetcher := &fakeFetcher{states: []ha.State{state("sensor.temp", "20.5", "°C")}}
	st := &fakeStore{insertErr: insertErr}
	e := testEngine(fetcher, st, []string{"sensor.*"}, nil, 0, nil)

	err := e.runCycle(context.Background())
	if !errors.Is(err, insertErr) {
		t.Errorf("runCycle err = %v, want %v", err, insertErr)
	}
	if !e.LastSuccessfulPoll().IsZero() {
		t.Error("lastPoll should not advance on insert failure")
	}
}

func TestRunCycle_TimestampIsRecent(t *testing.T) {
	fetcher := &fakeFetcher{states: []ha.State{state("sensor.temp", "20.5", "°C")}}
	st := &fakeStore{}
	e := testEngine(fetcher, st, []string{"sensor.*"}, nil, 0, nil)

	before := time.Now().UTC()
	if err := e.runCycle(context.Background()); err != nil {
		t.Fatalf("runCycle: %v", err)
	}
	after := time.Now().UTC()

	batch := st.LastBatch()
	if len(batch) != 1 {
		t.Fatalf("want 1 row, got %d", len(batch))
	}
	ts := batch[0].Timestamp
	if ts.Before(before) || ts.After(after) {
		t.Errorf("timestamp %v outside [%v, %v]", ts, before, after)
	}
	if batch[0].Unit != "°C" {
		t.Errorf("Unit = %q, want °C", batch[0].Unit)
	}
}

func TestRun_ImmediateAlignedPollingAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		time.Sleep(10 * time.Second) // Start away from a minute boundary.
		fetcher := &fakeFetcher{states: []ha.State{state("sensor.temp", "1", "")}}
		e := testEngine(fetcher, &fakeStore{}, nil, nil, 0, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() { e.Run(ctx); close(done) }()
		synctest.Wait()
		if fetcher.calls.Load() != 1 {
			t.Fatalf("expected immediate poll, got %d calls", fetcher.calls.Load())
		}
		time.Sleep(49 * time.Second)
		synctest.Wait()
		if fetcher.calls.Load() != 1 {
			t.Fatal("polled before the interval boundary")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if fetcher.calls.Load() != 2 {
			t.Fatalf("expected poll at minute boundary, got %d calls", fetcher.calls.Load())
		}
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("Run did not return after cancellation")
		}
	})
}

func TestRunCycle_RetriesFailedWrites(t *testing.T) {
	for _, initial := range []string{"", "19"} {
		t.Run("previous value="+initial, func(t *testing.T) {
			fetcher := &fakeFetcher{states: []ha.State{state("sensor.temp", initial, "°C")}}
			st := &fakeStore{}
			e := testEngine(fetcher, st, []string{"sensor.*"}, nil, 0, nil)
			if err := e.runCycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			lastPoll := e.LastSuccessfulPoll()
			fetcher.states = []ha.State{state("sensor.temp", "20", "°C")}
			st.insertErr = errors.New("database unavailable")
			if err := e.runCycle(context.Background()); !errors.Is(err, st.insertErr) {
				t.Fatalf("got %v", err)
			}
			if !e.LastSuccessfulPoll().Equal(lastPoll) {
				t.Fatal("failed insert advanced last poll")
			}
			st.insertErr = nil
			if err := e.runCycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			batch := st.LastBatch()
			if len(batch) != 1 || batch[0].Value != 20 {
				t.Fatalf("failed write was not retried: %+v", batch)
			}
			if err := e.runCycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(st.LastBatch()) != 0 {
				t.Fatal("successful retry should suppress unchanged values")
			}
		})
	}
}

func TestRunCycle_UnitChangeWritesUnchangedValue(t *testing.T) {
	fetcher := &fakeFetcher{states: []ha.State{state("sensor.temp", "20", "°C")}}
	st := &fakeStore{}
	e := testEngine(fetcher, st, nil, nil, 1, nil)
	for _, unit := range []string{"°C", "°F", ""} {
		fetcher.states = []ha.State{state("sensor.temp", "20", unit)}
		if err := e.runCycle(context.Background()); err != nil {
			t.Fatal(err)
		}
		batch := st.LastBatch()
		if len(batch) != 1 || batch[0].Unit != unit {
			t.Fatalf("unit change lost: %+v", batch)
		}
	}
}

func TestShouldWrite_UnitChanges(t *testing.T) {
	for _, tt := range []struct {
		current, last string
		want          bool
	}{
		{"°C", "°C", false}, {"°F", "°C", true}, {"", "°C", true}, {"°C", "", true},
	} {
		if got := ShouldWrite(20, 20, 1, false, tt.current, tt.last); got != tt.want {
			t.Errorf("unit %q -> %q: got %v, want %v", tt.last, tt.current, got, tt.want)
		}
	}
}
