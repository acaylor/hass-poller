// Command backfill repairs gaps in ha_numeric using Home Assistant recorder
// history. See docs/BACKFILL.md for usage and retention limits.
package main

import (
	"context"
	"flag"
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
	noRefresh := flag.Bool("no-refresh", false, "skip refreshing continuous aggregates")
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

	// Fetch a day at a time (to bound payload size) and in batches of entities
	// (to stay under HA's request-line length limit).
	const entityBatchSize = 40
	logger.Printf("fetching history %s .. %s", start.Format(time.RFC3339), end.Format(time.RFC3339))
	var history []ha.HistoryState
	for chunkStart := start; chunkStart.Before(end); chunkStart = chunkStart.Add(24 * time.Hour) {
		chunkEnd := chunkStart.Add(24 * time.Hour)
		if chunkEnd.After(end) {
			chunkEnd = end
		}
		dayCount := 0
		for i := 0; i < len(entityIDs); i += entityBatchSize {
			batch := entityIDs[i:min(i+entityBatchSize, len(entityIDs))]
			chunk, err := haClient.FetchHistory(ctx, chunkStart, chunkEnd, batch)
			if err != nil {
				logger.Fatalf("fetch history %s..%s: %v", chunkStart.Format(time.RFC3339), chunkEnd.Format(time.RFC3339), err)
			}
			for _, h := range chunk {
				// Each later day repeats the state in effect at its start.
				if chunkStart.After(start) && h.LastUpdated.Equal(chunkStart) {
					continue
				}
				history = append(history, h)
				dayCount++
			}
		}
		logger.Printf("  %s: %d raw state updates", chunkStart.Format("2006-01-02"), dayCount)
	}

	measurements := buildMeasurements(history, entityFilter, start, end, cfg, *noEpsilon)
	logger.Printf("prepared %d measurements across %d state updates", len(measurements), len(history))

	if *dryRun {
		logger.Printf("dry-run: nothing written")
		return
	}
	if len(measurements) == 0 {
		logger.Printf("no measurements to insert")
		return
	}

	db, err := store.New(ctx, cfg.PGDSN)
	if err != nil {
		logger.Fatalf("connect to postgres: %v", err)
	}
	defer db.Close()

	var inserted int64
	if *replace {
		inserted, err = db.ReplaceMeasurements(ctx, start, end, measurements)
	} else {
		inserted, err = db.InsertMeasurements(ctx, measurements)
	}
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
	logger.Printf("refreshed hourly and daily aggregates for the range")
}

// buildMeasurements applies the same allow/block filtering, numeric parsing, and
// per-entity epsilon change detection as the live poller, walking each entity's
// history in chronological order so epsilon compares against the last written
// value. Records are ordered and timestamped by last_updated so attribute-only
// unit changes keep their time. Only records within [start, end) are kept; the
// latest earlier record is the value in effect at start. Like the poller, an unchanged value is written
// again every engine.HeartbeatInterval until the entity's next record.
func buildMeasurements(
	history []ha.HistoryState,
	entityFilter *filter.GlobFilter,
	start, end time.Time,
	cfg config.Config,
	noEpsilon bool,
) []store.Measurement {
	// Group by entity, preserving order, then sort each group by time.
	byEntity := map[string][]ha.HistoryState{}
	for _, h := range history {
		if !entityFilter.Allowed(h.EntityID) {
			continue
		}
		byEntity[h.EntityID] = append(byEntity[h.EntityID], h)
	}

	var out []store.Measurement
	for entityID, series := range byEntity {
		sort.SliceStable(series, func(i, j int) bool {
			return series[i].LastUpdated.Before(series[j].LastUpdated)
		})

		eps := cfg.EpsilonDefault
		if e, ok := cfg.EpsilonOverrides[entityID]; ok {
			eps = e
		}

		var last store.Measurement
		held := false
		heartbeat := func(until time.Time) {
			for held && last.Timestamp.Add(engine.HeartbeatInterval).Before(until) {
				last.Timestamp = last.Timestamp.Add(engine.HeartbeatInterval)
				out = append(out, last)
			}
		}
		var previous ha.HistoryState
		for i, h := range series {
			h.LastUpdated = h.LastUpdated.UTC()
			if h.LastUpdated.Before(start) {
				if i+1 < len(series) && !series[i+1].LastUpdated.After(start) {
					continue // A later record is in effect at start.
				}
				h.LastUpdated = start
			}
			if !h.LastUpdated.Before(end) {
				break
			}
			if h == previous {
				continue
			}
			previous = h
			heartbeat(h.LastUpdated)
			value, ok := engine.ParseNumericState(h.State)
			if !ok {
				held = false
				continue
			}
			if held && !noEpsilon && !engine.ShouldWrite(value, last.Value, eps, false, h.Unit, last.Unit) {
				continue
			}
			last = store.Measurement{Timestamp: h.LastUpdated, EntityID: entityID, Value: value, Unit: h.Unit}
			held = true
			out = append(out, last)
		}
		heartbeat(end)
	}

	return out
}
