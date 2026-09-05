package engine

import (
	"context"
	"sync/atomic"

	"hass-poller/internal/ha"
	"hass-poller/internal/store"
)

type fakeFetcher struct {
	states []ha.State
	err    error
	calls  atomic.Int64
}

func (f *fakeFetcher) FetchStates(ctx context.Context) ([]ha.State, error) {
	f.calls.Add(1)
	return f.states, f.err
}

// fakeStore captures inserted measurements and lets tests inject failures.
type fakeStore struct {
	batches   [][]store.Measurement
	insertErr error
	healthy   bool
}

func (s *fakeStore) InsertMeasurements(ctx context.Context, m []store.Measurement) (int64, error) {
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
	return s.healthy
}

func (s *fakeStore) Batches() [][]store.Measurement {
	out := make([][]store.Measurement, len(s.batches))
	copy(out, s.batches)
	return out
}

func (s *fakeStore) LastBatch() []store.Measurement {
	if len(s.batches) == 0 {
		return nil
	}
	return s.batches[len(s.batches)-1]
}
