package engine

import (
	"context"
	"sync"

	"hass-poller/internal/ha"
	"hass-poller/internal/store"
)

// fakeFetcher returns canned HA state responses. If block is non-nil, FetchStates
// signals once on started and then receives from block before returning. This is
// used to drive cycle-contention tests deterministically.
type fakeFetcher struct {
	mu      sync.Mutex
	states  []ha.State
	err     error
	calls   int
	started chan struct{}
	block   chan struct{}
}

func (f *fakeFetcher) FetchStates(ctx context.Context) ([]ha.State, error) {
	f.mu.Lock()
	f.calls++
	started := f.started
	block := f.block
	states := f.states
	err := f.err
	f.mu.Unlock()

	if started != nil {
		started <- struct{}{}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return states, err
}

func (f *fakeFetcher) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeStore captures inserted measurements and lets tests inject failures.
type fakeStore struct {
	mu        sync.Mutex
	batches   [][]store.Measurement
	insertErr error
	healthy   bool
}

func (s *fakeStore) InsertMeasurements(ctx context.Context, m []store.Measurement) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.insertErr != nil {
		return 0, s.insertErr
	}
	// Copy to insulate caller-mutated slices.
	batch := make([]store.Measurement, len(m))
	copy(batch, m)
	s.batches = append(s.batches, batch)
	return int64(len(batch)), nil
}

func (s *fakeStore) Healthy(ctx context.Context) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.healthy
}

func (s *fakeStore) Batches() [][]store.Measurement {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]store.Measurement, len(s.batches))
	copy(out, s.batches)
	return out
}

func (s *fakeStore) LastBatch() []store.Measurement {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.batches) == 0 {
		return nil
	}
	return s.batches[len(s.batches)-1]
}
