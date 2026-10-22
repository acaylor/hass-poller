# Architecture

`hass-poller` is a single-binary Go service that periodically pulls numeric sensor state from a Home Assistant instance and writes it to TimescaleDB for long-term storage and visualization.

The diagram source is at [`diagrams/architecture.d2`](diagrams/architecture.d2); the rendered version is [`diagrams/d2.svg`](diagrams/d2.svg).

## Components

| Package | Responsibility |
|---|---|
| `cmd/ha-timescale-poller` | Application composition: load config, wire dependencies, install signal handlers. |
| `internal/config` | Parse environment variables and an optional YAML file for per-entity epsilon overrides. |
| `internal/engine` | Poll loop, scheduling aligned to wall-clock boundaries, numeric parsing, change detection. |
| `internal/filter` | Allowlist/blocklist filtering using `path.Match` glob syntax. |
| `internal/ha` | HTTP client for the Home Assistant `/api/states` endpoint (bearer-token auth, configurable timeout). |
| `internal/store` | TimescaleDB writer: `pgxpool` connection pool, schema migration on startup, batched inserts via `pgx.CopyFrom`. |
| `internal/httpserver` | Operational HTTP server exposing `/healthz` and Prometheus `/metrics`. |

## Data flow

1. The engine wakes on each `POLL_INTERVAL` tick (default 1 minute), aligned to wall-clock boundaries.
2. It calls the HA client, which performs a single `GET /api/states` request and returns every entity's current state.
3. Each state is run through the filter (allowlist + blocklist) and parsed as a float. Non-numeric values are silently dropped.
4. A change-detection step compares the parsed value against the last value written for that entity. If the absolute delta is within the per-entity epsilon (or the global default), the value is skipped unless its 15-minute heartbeat is due.
5. Surviving values are batched and inserted into `ha_numeric` with `pgx.CopyFrom`.
6. Counters and gauges are updated in the Prometheus registry; the HTTP server exposes them on `/metrics`.

## Database schema

The schema is embedded in the binary (`schema.sql`) and applied on startup. It is idempotent — every `CREATE` and policy registration uses `IF NOT EXISTS` / `if_not_exists`.

### Tables and rollups

| Object | Granularity | Retention | Notes |
|---|---|---|---|
| `ha_numeric` | changes and 15-minute heartbeats | 90 days | Hypertable. Compressed after 7 days. Segment-by `entity_id`. |
| `ha_numeric_1h` | hourly time-weighted `avg`/`min`/`max` per entity and unit | 1 year | Hypertable refreshed every 15 minutes for completed buckets in the last day. |
| `ha_numeric_1d` | daily (UTC) time-weighted `avg`/`min`/`max` per entity and unit | forever | Hypertable refreshed hourly for completed buckets in the last 2 days. |

### Tiered retention rationale

Recent data is queried interactively at full resolution; mid-range and historical queries don't need per-minute granularity. The tiered design keeps storage bounded while preserving long-term trend data:

- **Raw** is dropped after 90 days, after which detailed reconstruction is no longer possible.
- **Hourly** is dropped after 1 year. Sufficient for most "compare to last year" queries.
- **Daily** is kept forever. Daily aggregates for ~120 entities × 365 days × 10 years totals ~440K rows — a few hundred MB at most.

Rollup buckets are computed while raw data is still present. Once a daily bucket is written it persists in `ha_numeric_1d` after the underlying raw rows are dropped.

### Rollup semantics

Raw rows are irregular samples, so the rollups weight each value by how long it was in effect rather than averaging the rows. A row's value holds until the entity's next row, or for at most one hour if no row follows. The poller and `backfill` rewrite unchanged values every 15 minutes, so constant sensors appear in every bucket. Entities that stop reporting numeric values (removed, `unavailable`, poller outage) drop out of the rollups an hour after their last row. Values carry across bucket boundaries, so a value written at 10:59 contributes to the 11:00 bucket too.

| Column | Meaning |
|---|---|
| `bucket` | Bucket start (UTC). |
| `unit` | The unit of the values aggregated in this row. `NULL` for unitless sensors. |
| `avg` | Time-weighted mean over the covered part of the bucket. |
| `min` / `max` | Extremes of the values in effect during the bucket, including a value carried in from before it. |
| `n` | Raw rows recorded within the bucket. |
| `seconds` | Seconds of the bucket with a known value in this unit. Less than the bucket width when an entity started, stopped, or changed unit. |

Each row aggregates the values of one entity in one unit; values in different units are never combined or converted. Home Assistant changes a sensor's unit when its display unit or the unit system is changed, or when an integration changes the unit it reports. A bucket in which the unit changed has one row per unit, and the old unit's value stops counting at the first row in the new unit. The key is `(entity_id, unit, bucket)`, with `NULL` units treated as equal.

The rollups are plain tables recomputed by the `ha_numeric_refresh_rollup(rollup, width, start, end)` function. It rewrites every completed bucket that raw rows in `[start, end)` contribute to, including the buckets those values hold into after `end`. Only the rows of entities with raw data in the range are replaced, so rollups whose raw data has expired survive a refresh over their range. Two TimescaleDB jobs run `ha_numeric_rollup_job` for the recent buckets, and `backfill` refreshes the buckets it touches. A bucket is first written once it has completed, so the current hour and day are not yet in the rollups. To change the one-hour hold, set `hold` in both jobs' config with `alter_job`; `backfill` refreshes use the same value.

Versions before 0.3.0 stored the rollups as continuous aggregates of arithmetic means. Startup migrates them: buckets still covered by raw data are recomputed, and every entity's bucket that could not be recomputed is kept with `seconds` and `unit` set to `NULL` to mark it as a legacy arithmetic mean.

### Compression

`ha_numeric` is configured with `timescaledb.compress` and `compress_segmentby = entity_id`. The compression policy compresses chunks older than 7 days. Compression is essentially free for this workload because most entities emit slowly-changing numeric values that compress well.

## Filtering

Two-stage glob filtering using Go's `path.Match`:

1. **Allowlist** (`ENTITY_ALLOWLIST`, default `sensor.*`) — entity must match at least one pattern.
2. **Blocklist** (`ENTITY_BLOCKLIST`) — entity is excluded if it matches any pattern, even if it passed the allowlist.

`path.Match` `*` matches any run of non-`/` characters, so patterns like `sensor.*_signal_level` correctly match across the dot in entity IDs.

## Change detection

Each entity has an effective epsilon (per-entity override or `EPSILON_DEFAULT`). On every poll, the parsed value is compared against the last *written* value for that entity:

- If the absolute delta is `> epsilon`, the value is written and becomes the new "last written".
- A unit change is written even when the numeric value stays the same.
- A value is written regardless when 15 minutes have passed since the entity's last write. This heartbeat keeps the rollups' one-hour hold from expiring for constant sensors.
- Otherwise it is skipped.

The cache advances only after the database insert succeeds.

`EPSILON_DEFAULT=0` (the default) means strict inequality — every change is written. A non-zero epsilon is useful for noisy floating-point sensors where small fluctuations would otherwise produce excessive writes.

## Operational endpoints

| Path | Purpose |
|---|---|
| `/healthz` | Returns 200 if the last successful poll was within `max(2 minutes, 2 × POLL_INTERVAL)` and the database is reachable. Returns 5xx otherwise. |
| `/metrics` | Prometheus exposition: `hapoller_poll_total`, `hapoller_cycle_duration_seconds`, `hapoller_rows_inserted_total`, `hapoller_entities_seen`, `hapoller_entities_skipped`. |

## Failure modes

- **HA unreachable** — the poll fails fast with a logged error; the next tick retries. `/healthz` flips to unhealthy once the last successful poll exceeds `max(2 minutes, 2 × POLL_INTERVAL)`.
- **TimescaleDB unreachable or blocked** — each insert must finish within `DB_WRITE_TIMEOUT` (default 30 seconds), so a write waiting on a lock cannot stall polling. The engine logs the failed insert and keeps the last successfully written values, so subsequent polls retry values that remain changed.
- **Schema drift** — `schema.sql` migrations are idempotent. Renames or destructive changes need a manual one-shot migration; this is not currently automated.
- **Shutdown** — SIGTERM/SIGINT cancels the poll context, including an in-flight fetch or insert. There is no pending-write queue to flush. Active health and metrics requests get up to 5 seconds to finish.
