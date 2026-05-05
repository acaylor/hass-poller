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
4. A change-detection step compares the parsed value against the last value written for that entity. If the absolute delta is within the per-entity epsilon (or the global default), the value is skipped.
5. Surviving values are batched and inserted into `ha_numeric` with `pgx.CopyFrom`.
6. Counters and gauges are updated in the Prometheus registry; the HTTP server exposes them on `/metrics`.

## Database schema

The schema is embedded in the binary (`schema.sql`) and applied on startup. It is idempotent — every `CREATE` and policy registration uses `IF NOT EXISTS` / `if_not_exists`.

### Tables and continuous aggregates

| Object | Granularity | Retention | Notes |
|---|---|---|---|
| `ha_numeric` | per-poll (~1 min) | 90 days | Hypertable. Compressed after 7 days. Segment-by `entity_id`. |
| `ha_numeric_1h` | hourly `avg`/`min`/`max`/`count` per entity | 1 year | Continuous aggregate. Refresh policy: `start_offset=7 days`, `end_offset=1 hour`. |
| `ha_numeric_1d` | daily `avg`/`min`/`max`/`count` per entity | forever | Continuous aggregate. Refresh policy: `start_offset=7 days`, `end_offset=1 day`. |

### Tiered retention rationale

Recent data is queried interactively at full resolution; mid-range and historical queries don't need per-minute granularity. The tiered design keeps storage bounded while preserving long-term trend data:

- **Raw** is dropped after 90 days, after which detailed reconstruction is no longer possible.
- **Hourly** is dropped after 1 year. Sufficient for most "compare to last year" queries.
- **Daily** is kept forever. Daily aggregates for ~120 entities × 365 days × 10 years totals ~440K rows — a few hundred MB at most.

Continuous aggregates are materialized when raw data is still present. Once a daily bucket is materialized it persists in `ha_numeric_1d` even after the underlying raw rows are dropped, because queries against the aggregate read materialized state, not raw chunks.

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
- Otherwise it is skipped.

`EPSILON_DEFAULT=0` (the default) means strict inequality — every change is written. A non-zero epsilon is useful for noisy floating-point sensors where small fluctuations would otherwise produce excessive writes.

## Operational endpoints

| Path | Purpose |
|---|---|
| `/healthz` | Returns 200 if the last successful poll was within 2 minutes and the database is reachable. Returns 5xx otherwise. |
| `/metrics` | Prometheus exposition: `hapoller_poll_total`, `hapoller_cycle_duration_seconds`, `hapoller_rows_inserted_total`, `hapoller_entities_seen`, `hapoller_entities_skipped`. |

## Failure modes

- **HA unreachable** — the poll fails fast with a logged error; the next tick retries. `/healthz` flips to unhealthy after 2 minutes of failures.
- **TimescaleDB unreachable** — the engine logs and skips the write phase but continues polling. Metrics distinguish "fetched" from "inserted".
- **Schema drift** — `schema.sql` migrations are idempotent. Renames or destructive changes need a manual one-shot migration; this is not currently automated.
- **Graceful shutdown** — on SIGTERM/SIGINT the engine completes the in-flight poll and flushes pending writes before exiting.
