package engine

import (
	"context"
	"errors"
	"log"
	"math"
	"strconv"
	"strings"
	"time"

	"hass-poller/internal/filter"
	"hass-poller/internal/ha"
	"hass-poller/internal/store"
)

type Engine struct {
	haClient     *ha.Client
	entityFilter *filter.GlobFilter
	store        *store.Store
	pollInterval time.Duration
	logger       *log.Logger
}

func New(
	haClient *ha.Client,
	entityFilter *filter.GlobFilter,
	store *store.Store,
	pollInterval time.Duration,
	logger *log.Logger,
) *Engine {
	return &Engine{
		haClient:     haClient,
		entityFilter: entityFilter,
		store:        store,
		pollInterval: pollInterval,
		logger:       logger,
	}
}

func (e *Engine) Run(ctx context.Context) error {
	if err := e.runCycle(ctx); err != nil {
		e.logger.Printf("poll cycle failed: %v", err)
	}

	ticker := time.NewTicker(e.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return ctx.Err()
		case <-ticker.C:
			if err := e.runCycle(ctx); err != nil {
				e.logger.Printf("poll cycle failed: %v", err)
			}
		}
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

	e.logger.Printf(
		"poll complete duration=%s seen=%d matched=%d numeric=%d inserted=%d",
		time.Since(started).Round(time.Millisecond),
		seen,
		matched,
		numeric,
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
