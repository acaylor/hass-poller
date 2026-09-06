package engine

import (
	"context"
	"log"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"hass-poller/internal/filter"
	"hass-poller/internal/ha"
	"hass-poller/internal/store"
)

var (
	pollTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "hapoller_poll_total",
		Help: "Total poll cycles by result.",
	}, []string{"result"})

	cycleDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "hapoller_cycle_duration_seconds",
		Help:    "Duration of each poll cycle.",
		Buckets: prometheus.DefBuckets,
	})

	rowsInserted = promauto.NewCounter(prometheus.CounterOpts{
		Name: "hapoller_rows_inserted_total",
		Help: "Total rows inserted into TimescaleDB.",
	})

	entitiesSeen = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "hapoller_entities_seen",
		Help: "Number of entities seen in last poll.",
	})

	entitiesSkipped = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "hapoller_entities_skipped",
		Help: "Number of entities skipped (unchanged) in last poll.",
	})
)

// StatesFetcher is the subset of the Home Assistant client the engine relies on.
type StatesFetcher interface {
	FetchStates(ctx context.Context) ([]ha.State, error)
}

// MeasurementStore is the subset of the TimescaleDB store the engine relies on.
type MeasurementStore interface {
	InsertMeasurements(ctx context.Context, m []store.Measurement) (int64, error)
	Healthy(ctx context.Context) bool
}

type entityState struct {
	value float64
	unit  string
}

// ShouldWrite reports whether this is a first observation, a unit change, or
// a numeric change beyond epsilon.
func ShouldWrite(current, last, epsilon float64, firstObservation bool, currentUnit, lastUnit string) bool {
	if firstObservation || currentUnit != lastUnit {
		return true
	}
	return math.Abs(current-last) > epsilon
}

type Engine struct {
	haClient         StatesFetcher
	entityFilter     *filter.GlobFilter
	store            MeasurementStore
	pollInterval     time.Duration
	logger           *log.Logger
	epsilonDefault   float64
	epsilonOverrides map[string]float64
	state            map[string]entityState
	lastPoll         atomic.Pointer[time.Time]
}

func New(
	haClient StatesFetcher,
	entityFilter *filter.GlobFilter,
	store MeasurementStore,
	pollInterval time.Duration,
	epsilonDefault float64,
	epsilonOverrides map[string]float64,
	logger *log.Logger,
) *Engine {
	return &Engine{
		haClient:         haClient,
		entityFilter:     entityFilter,
		store:            store,
		pollInterval:     pollInterval,
		logger:           logger,
		epsilonDefault:   epsilonDefault,
		epsilonOverrides: epsilonOverrides,
		state:            make(map[string]entityState),
	}
}

func (e *Engine) epsilonFor(entityID string) float64 {
	if eps, ok := e.epsilonOverrides[entityID]; ok {
		return eps
	}
	return e.epsilonDefault
}

// LastSuccessfulPoll returns the time of the last successful poll (implements HealthChecker).
func (e *Engine) LastSuccessfulPoll() time.Time {
	if last := e.lastPoll.Load(); last != nil {
		return *last
	}
	return time.Time{}
}

// DBHealthy checks if the database connection is healthy (implements HealthChecker).
func (e *Engine) DBHealthy(ctx context.Context) bool {
	return e.store.Healthy(ctx)
}

func (e *Engine) Run(ctx context.Context) {
	// Run first cycle immediately.
	e.poll(ctx)

	for {
		// Align to the next poll interval boundary.
		now := time.Now()
		next := now.Truncate(e.pollInterval).Add(e.pollInterval)
		delay := time.Until(next)

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
			e.poll(ctx)
		}
	}
}

func (e *Engine) poll(ctx context.Context) {
	if err := e.runCycle(ctx); err != nil {
		e.logger.Printf("poll cycle failed: %v", err)
		pollTotal.WithLabelValues("error").Inc()
	} else {
		pollTotal.WithLabelValues("success").Inc()
	}
}

func (e *Engine) runCycle(ctx context.Context) error {
	started := time.Now()
	states, err := e.haClient.FetchStates(ctx)
	if err != nil {
		return err
	}

	ts := time.Now().UTC()
	measurements := make([]store.Measurement, 0, len(states))
	seen := 0
	matched := 0
	numeric := 0
	skipped := 0

	for _, s := range states {
		seen++
		if !e.entityFilter.Allowed(s.EntityID) {
			continue
		}
		matched++

		value, ok := ParseNumericState(s.State)
		if !ok {
			continue
		}
		numeric++

		last, exists := e.state[s.EntityID]
		eps := e.epsilonFor(s.EntityID)
		if !ShouldWrite(value, last.value, eps, !exists, s.Attributes.UnitOfMeasurement, last.unit) {
			skipped++
			continue
		}

		measurements = append(measurements, store.Measurement{
			Timestamp: ts,
			EntityID:  s.EntityID,
			Value:     value,
			Unit:      s.Attributes.UnitOfMeasurement,
		})
	}

	inserted, err := e.store.InsertMeasurements(ctx, measurements)
	if err != nil {
		return err
	}

	// Only committed measurements may suppress future writes.
	for _, m := range measurements {
		e.state[m.EntityID] = entityState{value: m.Value, unit: m.Unit}
	}

	duration := time.Since(started)

	// Update metrics.
	cycleDuration.Observe(duration.Seconds())
	rowsInserted.Add(float64(inserted))
	entitiesSeen.Set(float64(seen))
	entitiesSkipped.Set(float64(skipped))
	now := time.Now()
	e.lastPoll.Store(&now)

	e.logger.Printf(
		"poll complete duration=%s seen=%d matched=%d numeric=%d skipped=%d inserted=%d",
		duration.Round(time.Millisecond),
		seen,
		matched,
		numeric,
		skipped,
		inserted,
	)

	return nil
}

// ParseNumericState converts a Home Assistant state string to a float, rejecting
// blank, unknown/unavailable, non-numeric, and non-finite values.
func ParseNumericState(raw string) (float64, bool) {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, false
	}

	if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, false
	}

	return parsed, true
}
