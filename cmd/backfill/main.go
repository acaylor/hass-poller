// Command backfill repairs gaps in ha_numeric using Home Assistant recorder
// history. See docs/BACKFILL.md for usage and retention limits.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"hass-poller/internal/config"
	"hass-poller/internal/engine"
	"hass-poller/internal/filter"
	"hass-poller/internal/ha"
	"hass-poller/internal/store"
)

func main() {
	logger := log.New(os.Stdout, "", log.LstdFlags)

	startFlag := flag.String("start", "", "gap start time, RFC3339 (required)")
	endFlag := flag.String("end", "", "gap end time, RFC3339 (default: now)")
	replace := flag.Bool("replace", false, "atomically replace rows in [start,end) for entities with replacement data")
	noEpsilon := flag.Bool("no-epsilon", false, "insert every recorded change without epsilon suppression")
	noRefresh := flag.Bool("no-refresh", false, "skip refreshing the hourly and daily rollups")
	dryRun := flag.Bool("dry-run", false, "fetch and report counts without writing")
	flag.Parse()

	if *startFlag == "" {
		logger.Fatal("-start is required (RFC3339, e.g. 2026-06-20T00:00:00Z)")
	}
	start, err := time.Parse(time.RFC3339, *startFlag)
	if err != nil {
		logger.Fatalf("parse -start: %v", err)
	}
	end := time.Now().UTC()
	if *endFlag != "" {
		if end, err = time.Parse(time.RFC3339, *endFlag); err != nil {
			logger.Fatalf("parse -end: %v", err)
		}
	}
	if !end.After(start) {
		logger.Fatalf("-end (%s) must be after -start (%s)", end, start)
	}

	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("load config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	haClient := ha.NewClient(cfg.HABaseURL, cfg.HAToken, cfg.HTTPTimeout)
	entityFilter := filter.NewGlobFilter(cfg.EntityAllowlist, cfg.EntityBlocklist)

	// HA's history API requires an explicit filter_entity_id, so enumerate the
	// current entities and keep the ones matching the same glob filter the poller
	// uses. (Entities that existed only during the gap and were since removed
	// won't appear here, but that's rare for sensors.)
	states, err := haClient.FetchStates(ctx)
	if err != nil {
		logger.Fatalf("fetch states for entity list: %v", err)
	}
	var entityIDs []string
	for _, s := range states {
		if entityFilter.Allowed(s.EntityID) {
			entityIDs = append(entityIDs, s.EntityID)
		}
	}
	if len(entityIDs) == 0 {
		logger.Fatalf("no entities matched the allow/block filter; nothing to backfill")
	}
	logger.Printf("backfilling %d entities", len(entityIDs))

	logger.Printf("fetching history %s .. %s", start.Format(time.RFC3339), end.Format(time.RFC3339))
	// Stage rows in the database as each day is processed, then apply them in
	// one transaction.
	var db *store.Store
	var staging *store.Staging
	if !*dryRun {
		if db, err = store.New(ctx, cfg.PGDSN); err != nil {
			logger.Fatalf("connect to postgres: %v", err)
		}
		defer db.Close()
		if staging, err = db.BeginStaging(ctx, start, end); err != nil {
			logger.Fatalf("stage measurements: %v", err)
		}
		defer staging.Rollback()
	}

	fetch := func(chunkStart, chunkEnd time.Time) ([]ha.HistoryState, error) {
		var history []ha.HistoryState
		for i := 0; i < len(entityIDs); i += entityBatchSize {
			batch := entityIDs[i:min(i+entityBatchSize, len(entityIDs))]
			chunk, err := haClient.FetchHistory(ctx, chunkStart, chunkEnd, batch)
			if err != nil {
				return nil, err
			}
			history = append(history, chunk...)
		}
		logger.Printf("  %s: %d raw state updates", chunkStart.Format("2006-01-02"), len(history))
		return history, nil
	}
	write := func(measurements []store.Measurement) error {
		if staging == nil {
			return nil
		}
		return staging.Add(ctx, measurements)
	}
	builder := newMeasurementBuilder(entityFilter, start, cfg, *noEpsilon)
	prepared, err := backfillChunks(start, end, fetch, builder, write)
	if err != nil {
		logger.Fatalf("backfill: %v", err)
	}
	logger.Printf("prepared %d measurements", prepared)

	if *dryRun {
		logger.Printf("dry-run: nothing written")
		return
	}
	if prepared == 0 {
		logger.Printf("no measurements to insert")
		return
	}
	inserted, err := staging.Commit(ctx, *replace)
	if err != nil {
		logger.Fatalf("insert measurements: %v", err)
	}
	logger.Printf("inserted %d rows", inserted)

	if *noRefresh {
		logger.Printf("skipping rollup refresh (run ha_numeric_refresh_rollup manually)")
		return
	}
	if err := db.RefreshRollups(ctx, start, end); err != nil {
		logger.Fatalf("refresh rollups: %v", err)
	}
	logger.Printf("refreshed hourly and daily rollups for the range")
}

// entityBatchSize keeps history requests under HA's request-line length limit.
const entityBatchSize = 40

// backfillChunks fetches history a day at a time and writes each day's
// measurements before fetching the next, so memory is bounded by one day of
// history rather than the whole range. It returns the measurement count.
func backfillChunks(
	start, end time.Time,
	fetch func(chunkStart, chunkEnd time.Time) ([]ha.HistoryState, error),
	builder *measurementBuilder,
	write func([]store.Measurement) error,
) (int, error) {
	prepared := 0
	for chunkStart := start; chunkStart.Before(end); chunkStart = chunkStart.Add(24 * time.Hour) {
		chunkEnd := chunkStart.Add(24 * time.Hour)
		if chunkEnd.After(end) {
			chunkEnd = end
		}
		history, err := fetch(chunkStart, chunkEnd)
		if err != nil {
			return prepared, fmt.Errorf("fetch history %s..%s: %w",
				chunkStart.Format(time.RFC3339), chunkEnd.Format(time.RFC3339), err)
		}
		if chunkStart.After(start) {
			// Each later day repeats the state in effect at its start.
			kept := history[:0]
			for _, h := range history {
				if !h.LastUpdated.Equal(chunkStart) {
					kept = append(kept, h)
				}
			}
			history = kept
		}
		measurements := builder.add(history, chunkEnd)
		if err := write(measurements); err != nil {
			return prepared, fmt.Errorf("write measurements: %w", err)
		}
		prepared += len(measurements)
	}
	return prepared, nil
}

// measurementBuilder applies the same allow/block filtering, numeric parsing,
// and per-entity epsilon change detection as the live poller. It is fed
// consecutive chunks of history and keeps each entity's last written value
// between them, so epsilon compares against the last written value across
// chunk boundaries. Records are ordered and timestamped by last_updated so
// attribute-only unit changes keep their time. The latest record before start
// is the value in effect at start. Like the poller, the current value is
// written every engine.HeartbeatInterval after the last write, even when
// epsilon suppressed it, until the entity's next written record.
type measurementBuilder struct {
	entityFilter *filter.GlobFilter
	start        time.Time
	cfg          config.Config
	noEpsilon    bool
	entities     map[string]*entityWalk
}

type entityWalk struct {
	last     store.Measurement // last written
	current  store.Measurement // latest numeric value, written or not
	held     bool
	previous ha.HistoryState
}

func newMeasurementBuilder(entityFilter *filter.GlobFilter, start time.Time, cfg config.Config, noEpsilon bool) *measurementBuilder {
	return &measurementBuilder{
		entityFilter: entityFilter,
		start:        start,
		cfg:          cfg,
		noEpsilon:    noEpsilon,
		entities:     map[string]*entityWalk{},
	}
}

// add returns the measurements for history records before until, which must
// not precede the previous call's until.
func (b *measurementBuilder) add(history []ha.HistoryState, until time.Time) []store.Measurement {
	// Group by entity, preserving order, then sort each group by time.
	byEntity := map[string][]ha.HistoryState{}
	for _, h := range history {
		if !b.entityFilter.Allowed(h.EntityID) {
			continue
		}
		byEntity[h.EntityID] = append(byEntity[h.EntityID], h)
		if b.entities[h.EntityID] == nil {
			b.entities[h.EntityID] = &entityWalk{}
		}
	}

	var out []store.Measurement
	for entityID, w := range b.entities {
		series := byEntity[entityID]
		sort.SliceStable(series, func(i, j int) bool {
			return series[i].LastUpdated.Before(series[j].LastUpdated)
		})

		eps := b.cfg.EpsilonDefault
		if e, ok := b.cfg.EpsilonOverrides[entityID]; ok {
			eps = e
		}

		heartbeat := func(until time.Time) {
			for w.held && w.last.Timestamp.Add(engine.HeartbeatInterval).Before(until) {
				w.current.Timestamp = w.last.Timestamp.Add(engine.HeartbeatInterval)
				w.last = w.current
				out = append(out, w.last)
			}
		}
		for i, h := range series {
			h.LastUpdated = h.LastUpdated.UTC()
			if h.LastUpdated.Before(b.start) {
				if i+1 < len(series) && !series[i+1].LastUpdated.After(b.start) {
					continue // A later record is in effect at start.
				}
				h.LastUpdated = b.start
			}
			if !h.LastUpdated.Before(until) {
				break
			}
			if h == w.previous {
				continue
			}
			w.previous = h
			heartbeat(h.LastUpdated)
			value, ok := engine.ParseNumericState(h.State)
			if !ok {
				w.held = false
				continue
			}
			w.current = store.Measurement{Timestamp: h.LastUpdated, EntityID: entityID, Value: value, Unit: h.Unit}
			if w.held && !b.noEpsilon && !engine.ShouldWrite(value, w.last.Value, eps, false, h.Unit, w.last.Unit) {
				continue
			}
			w.last = w.current
			w.held = true
			out = append(out, w.last)
		}
		heartbeat(until)
	}

	return out
}
