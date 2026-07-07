// Command backfill repairs gaps in the ha_numeric table after an outage by
// pulling recorded state changes from Home Assistant's history API and writing
// them to TimescaleDB.
//
// It reuses the same configuration (HA_BASE_URL, HA_TOKEN, PG_DSN,
// ENTITY_ALLOWLIST/BLOCKLIST, EPSILON_*) and the same filtering, numeric
// parsing, and change-detection logic as the live poller, so backfilled rows
// match what the poller would have written — except the timestamps come from
// Home Assistant's real last_changed, which is higher fidelity than the
// poller's once-a-minute snapshots.
//
// The history source is bounded by HA's recorder retention (purge_keep_days,
// default 10 days). For gaps older than that, use long-term statistics instead.
//
// Usage:
//
//	backfill -start 2026-06-20T00:00:00Z -end 2026-06-22T12:00:00Z [flags]
//
// Flags:
//
//	-start       gap start, RFC3339 (required)
//	-end         gap end, RFC3339 (default: now)
//	-replace     delete existing rows in [start,end) before inserting
//	-no-epsilon  insert every recorded change without epsilon suppression
//	-no-refresh  skip refreshing the hourly/daily continuous aggregates
//	-dry-run     fetch and report counts without writing
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"sort"
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
	replace := flag.Bool("replace", false, "delete existing rows in [start,end) before inserting")
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

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	haClient := ha.NewClient(cfg.HABaseURL, cfg.HAToken, cfg.HTTPTimeout)
	entityFilter := filter.NewGlobFilter(cfg.EntityAllowlist, cfg.EntityBlocklist)

	db, err := store.New(ctx, cfg.PGDSN)
	if err != nil {
		logger.Fatalf("connect to postgres: %v", err)
	}
	defer db.Close()

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
			history = append(history, chunk...)
			dayCount += len(chunk)
		}
		logger.Printf("  %s: %d raw state changes", chunkStart.Format("2006-01-02"), dayCount)
	}

	measurements := buildMeasurements(history, entityFilter, start, end, cfg, *noEpsilon)
	logger.Printf("prepared %d measurements across %d state changes", len(measurements), len(history))

	if *dryRun {
		logger.Printf("dry-run: nothing written")
		return
	}
	if len(measurements) == 0 {
		logger.Printf("no measurements to insert")
		return
	}

	if *replace {
		deleted, err := db.DeleteRange(ctx, start, end)
		if err != nil {
			logger.Fatalf("delete existing range: %v", err)
		}
		logger.Printf("deleted %d existing rows in range", deleted)
	}

	inserted, err := db.InsertMeasurements(ctx, measurements)
	if err != nil {
		logger.Fatalf("insert measurements: %v", err)
	}
	logger.Printf("inserted %d rows", inserted)

	if *noRefresh {
		logger.Printf("skipping continuous aggregate refresh (run refresh_continuous_aggregate manually)")
		return
	}
	if err := db.RefreshContinuousAggregates(ctx, start, end); err != nil {
		logger.Fatalf("refresh continuous aggregates: %v", err)
	}
	logger.Printf("refreshed hourly and daily aggregates for the range")
}

// buildMeasurements applies the same allow/block filtering, numeric parsing, and
// per-entity epsilon change detection as the live poller, walking each entity's
// history in chronological order so epsilon compares against the last written
// value. Only changes within [start, end) are kept.
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
		sort.Slice(series, func(i, j int) bool {
			return series[i].LastChanged.Before(series[j].LastChanged)
		})

		eps := cfg.EpsilonDefault
		if e, ok := cfg.EpsilonOverrides[entityID]; ok {
			eps = e
		}

		var last float64
		first := true
		for _, h := range series {
			value, ok := engine.ParseNumericState(h.State)
			if !ok {
				continue
			}
			if !noEpsilon && !engine.ShouldWrite(value, last, eps, first) {
				continue
			}
			last = value
			first = false

			ts := h.LastChanged.UTC()
			if ts.Before(start) || !ts.Before(end) {
				continue
			}
			out = append(out, store.Measurement{
				Timestamp: ts,
				EntityID:  entityID,
				Value:     value,
				Unit:      h.Unit,
			})
		}
	}

	return out
}
