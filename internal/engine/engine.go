package engine

import (
	"context"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"hass-poller/internal/filter"
	"hass-poller/internal/ha"
	"hass-poller/internal/httpserver"
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

// ShouldWrite determines if a measurement should be written based on epsilon change detection.
// Returns true for first observations or when the value has changed beyond the epsilon threshold.
func ShouldWrite(current, last float64, epsilon float64, firstObservation bool) bool {
	if firstObservation {
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
	lastPoll         httpserver.AtomicTime
	running          sync.Mutex
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
	return e.lastPoll.Load()
}

// DBHealthy checks if the database connection is healthy (implements HealthChecker).
func (e *Engine) DBHealthy(ctx context.Context) bool {
	return e.store.Healthy(ctx)
}

func (e *Engine) Run(ctx context.Context) error {
	// Run first cycle immediately.
	e.tryRunCycle(ctx)

	for {
		// Align to next minute boundary.
		now := time.Now()
		next := now.Truncate(e.pollInterval).Add(e.pollInterval)
		delay := time.Until(next)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
			e.tryRunCycle(ctx)
		}
	}
}

func (e *Engine) tryRunCycle(ctx context.Context) {
	if !e.running.TryLock() {
		e.logger.Printf("skipping poll cycle: previous cycle still running")
		return
	}
	defer e.running.Unlock()

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

		value, ok := parseNumericState(s.State)
		if !ok {
			continue
		}
		numeric++

		last, exists := e.state[s.EntityID]
		eps := e.epsilonFor(s.EntityID)
		if !ShouldWrite(value, last.value, eps, !exists) {
			skipped++
			continue
		}

		e.state[s.EntityID] = entityState{value: value, unit: s.Attributes.UnitOfMeasurement}

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

	duration := time.Since(started)

	// Update metrics.
	cycleDuration.Observe(duration.Seconds())
	rowsInserted.Add(float64(inserted))
	entitiesSeen.Set(float64(numeric))
	entitiesSkipped.Set(float64(skipped))
	e.lastPoll.Store(time.Now())

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

func parseNumericState(raw string) (float64, bool) {
	value := strings.TrimSpace(strings.ToLower(raw))
	if value == "" || value == "unknown" || value == "unavailable" {
		return 0, false
	}

	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, false
	}

	if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, false
	}

	return parsed, true
}
