package engine

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"testing"
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
			got := ShouldWrite(tt.current, tt.last, tt.epsilon, tt.firstObservation)
			if got != tt.want {
				t.Errorf("ShouldWrite(%v, %v, %v, %v) = %v, want %v",
					tt.current, tt.last, tt.epsilon, tt.firstObservation, got, tt.want)
			}
		})
	}
}

func TestParseNumericState(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		want  float64
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
			got, ok := parseNumericState(tt.raw)
			if ok != tt.wantOK {
				t.Errorf("parseNumericState(%q) ok = %v, want %v", tt.raw, ok, tt.wantOK)
				return
			}
			if ok && got != tt.want {
				t.Errorf("parseNumericState(%q) = %v, want %v", tt.raw, got, tt.want)
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
			state("binary_sensor.door", "on", ""),         // not in allowlist
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

func TestTryRunCycle_SkipsWhenCycleAlreadyRunning(t *testing.T) {
	started := make(chan struct{}, 1)
	block := make(chan struct{})
	fetcher := &fakeFetcher{
		states:  []ha.State{state("sensor.temp", "1", "")},
		started: started,
		block:   block,
	}
	st := &fakeStore{}
	e := testEngine(fetcher, st, []string{"sensor.*"}, nil, 0, nil)

	// Kick off cycle 1 in the background; it will block inside FetchStates.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.tryRunCycle(context.Background())
	}()

	// Wait until cycle 1 has acquired the running lock and entered FetchStates.
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("cycle 1 never started")
	}

	// Cycle 2: should bail immediately because the mutex is held.
	e.tryRunCycle(context.Background())
	if got := fetcher.Calls(); got != 1 {
		t.Errorf("FetchStates called %d times, want 1 (second call should bail)", got)
	}

	// Release cycle 1 and let it finish.
	close(block)
	wg.Wait()

	if got := fetcher.Calls(); got != 1 {
		t.Errorf("after release, FetchStates calls = %d, want 1", got)
	}
	if n := len(st.LastBatch()); n != 1 {
		t.Errorf("cycle 1 should have inserted 1 row, got %d", n)
	}
}

func TestRun_ReturnsOnContextCancel(t *testing.T) {
	fetcher := &fakeFetcher{states: []ha.State{state("sensor.temp", "1", "")}}
	st := &fakeStore{}
	e := testEngine(fetcher, st, []string{"sensor.*"}, nil, 0, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- e.Run(ctx)
	}()

	// Wait for the immediate first cycle to complete, then cancel.
	deadline := time.Now().Add(time.Second)
	for fetcher.Calls() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned err = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

func TestTryRunCycle_LogsAndCountsErrors(t *testing.T) {
	// tryRunCycle is the only entry that swallows runCycle errors. Exercise both
	// the error and success branches to make sure neither panics and that the
	// running mutex is released either way.
	fetcher := &fakeFetcher{err: errors.New("boom")}
	st := &fakeStore{}
	e := testEngine(fetcher, st, []string{"sensor.*"}, nil, 0, nil)

	e.tryRunCycle(context.Background())          // error branch
	e.tryRunCycle(context.Background())          // mutex must have been released

	if fetcher.Calls() != 2 {
		t.Errorf("FetchStates called %d times, want 2 (mutex must release on error)", fetcher.Calls())
	}
}

func TestRun_TickInvokesAdditionalCycles(t *testing.T) {
	fetcher := &fakeFetcher{states: []ha.State{state("sensor.temp", "1", "")}}
	st := &fakeStore{}
	e := New(
		fetcher,
		filter.NewGlobFilter([]string{"sensor.*"}, nil),
		st,
		20*time.Millisecond, // short interval so the ticker branch fires
		0, nil,
		log.New(io.Discard, "", 0),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()

	<-done
	if fetcher.Calls() < 2 {
		t.Errorf("FetchStates calls = %d, want >= 2 (immediate + at least one tick)", fetcher.Calls())
	}
}

func TestDBHealthy_DelegatesToStore(t *testing.T) {
	st := &fakeStore{healthy: true}
	e := testEngine(&fakeFetcher{}, st, []string{"sensor.*"}, nil, 0, nil)
	if !e.DBHealthy(context.Background()) {
		t.Error("DBHealthy = false, want true")
	}

	st2 := &fakeStore{healthy: false}
	e2 := testEngine(&fakeFetcher{}, st2, []string{"sensor.*"}, nil, 0, nil)
	if e2.DBHealthy(context.Background()) {
		t.Error("DBHealthy = true, want false")
	}
}
