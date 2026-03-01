---
title: Home Assistant → TimescaleDB Poller (Go) — Implementation Plan
date: 2026-03-01
status: draft
---

## Goal

Build a small Go service that polls Home Assistant once per minute, extracts **numeric sensor values**, and writes them to **PostgreSQL + TimescaleDB**. It should:

- Store data long-term efficiently (Timescale compression + rollups).
- Avoid pointless writes by **writing only on change** (epsilon-based deduplication).
- Be boring to operate: one container or one binary, clear logs, simple config.

Non-goals:
- Storing non-numeric states.
- Multiple ingestion cadences (we poll at 1-minute intervals).
- Modifying Home Assistant's own recorder schema.
- Heartbeat writes — we accept that constant sensors produce no rows between changes. Grafana "fill: previous" handles the visual gap.

---

## High-level architecture

- **Poller** (this project):
  - HTTP GET `/api/states` from HA once per minute.
  - Filter entities via allowlist/blocklist with glob matching.
  - Filter numeric sensors (parse state to float64).
  - Decide whether to write (epsilon change detection, in-memory).
  - Batch insert into Timescale hypertable via `pgx.CopyFrom`.

- **TimescaleDB**:
  - `ha_numeric` hypertable holds raw points.
  - Compression policy for older chunks.
  - Optional retention policy for raw.
  - Continuous aggregate `ha_numeric_1h` for long-range Grafana queries.

- **Grafana**:
  - For last N days: query `ha_numeric`.
  - For longer: query `ha_numeric_1h`.

---

## Data model

### Raw measurements hypertable

Columns:
- `ts` (timestamptz) — timestamp when polled
- `entity_id` (text) — e.g. `sensor.kitchen_temperature`
- `value` (double precision)
- `unit` (text nullable)

Indexes:
- `(entity_id, ts DESC)` for Grafana.

Schema SQL (embedded in binary, run on startup with `IF NOT EXISTS` guards):

```sql
CREATE TABLE IF NOT EXISTS ha_numeric (
  ts        timestamptz NOT NULL,
  entity_id text        NOT NULL,
  value     double precision NOT NULL,
  unit      text        NULL
);

SELECT create_hypertable('ha_numeric', 'ts', if_not_exists => TRUE);

CREATE INDEX IF NOT EXISTS ha_numeric_entity_ts_desc
ON ha_numeric (entity_id, ts DESC);

ALTER TABLE ha_numeric SET (
  timescaledb.compress,
  timescaledb.compress_segmentby = 'entity_id'
);

-- compress chunks older than 7 days
SELECT add_compression_policy('ha_numeric', INTERVAL '7 days', if_not_exists => TRUE);

-- optional: keep raw for 90 days (tune later)
SELECT add_retention_policy('ha_numeric', INTERVAL '90 days', if_not_exists => TRUE);
```

### Continuous aggregate for hour-level history

```sql
CREATE MATERIALIZED VIEW IF NOT EXISTS ha_numeric_1h
WITH (timescaledb.continuous) AS
SELECT
  time_bucket('1 hour', ts) AS bucket,
  entity_id,
  avg(value) AS avg,
  min(value) AS min,
  max(value) AS max,
  count(*)   AS n
FROM ha_numeric
GROUP BY bucket, entity_id;

SELECT add_continuous_aggregate_policy('ha_numeric_1h',
  start_offset => INTERVAL '7 days',
  end_offset   => INTERVAL '1 hour',
  schedule_interval => INTERVAL '15 minutes'
);
```

---

## Entity filtering (allowlist + blocklist with globs)

Entities are filtered in two stages:

1. **Allowlist** (`ENTITY_ALLOWLIST`): Only entities matching at least one glob pattern are considered. Default: `sensor.*` (all sensors).
2. **Blocklist** (`ENTITY_BLOCKLIST`): Entities matching any blocklist glob are excluded, even if they match the allowlist.

Blocklist takes precedence over allowlist.

Glob matching uses Go's `path.Match` semantics (e.g. `*` matches any sequence of non-separator characters). Examples:

```yaml
# env vars (comma-separated)
ENTITY_ALLOWLIST=sensor.*,binary_sensor.*
ENTITY_BLOCKLIST=sensor.energy_*,sensor.*_linkquality

# or in config file
entity_allowlist:
  - "sensor.*"
  - "binary_sensor.*"
entity_blocklist:
  - "sensor.energy_*"
  - "sensor.*_linkquality"
```

After glob filtering, non-numeric states (`unknown`, `unavailable`, non-parseable strings) are still discarded.

---

## Change detection (epsilon)

We use in-memory epsilon-based change detection to avoid writing identical values every minute.

### In-memory state map

Per `entity_id`, track:

* `lastValue` (float64)
* `lastUnit` (string)

On each poll, for each filtered numeric entity:

1. **First observation** — always write (entity not yet in map).
2. **Change write** — write if `abs(value - lastValue) >= epsilon`.
3. **Otherwise** — skip (value unchanged within epsilon).

### Epsilon configuration

* `EPSILON_DEFAULT` (default `0.0` — strict equality, write on any change).
* Per-entity overrides via config file for noisy sensors:

```yaml
epsilon_overrides:
  "sensor.kitchen_temperature": 0.05
  "sensor.outdoor_humidity": 0.1
```

### Restart behavior

On process restart the in-memory map is empty, so every entity gets one "first observation" write. This is harmless — it's at most one extra row per entity and immediately re-establishes the baseline.

No external state store (Redis, etc.) is needed. The in-memory map is cheap, simple, and the restart cost is negligible.

---

## Polling approach (Home Assistant)

### Endpoint

* `GET /api/states`

Headers:

* `Authorization: Bearer <HA_TOKEN>`
* `Content-Type: application/json`

Behavior:

* Make a single request per minute.
* Parse JSON array of state objects.
* Extract:

  * `entity_id` (string)
  * `state` (string, parse float)
  * `attributes.unit_of_measurement` (string optional)
  * `attributes.state_class` (string optional — used to identify and optionally skip `total_increasing` entities like energy meters)

### Filtering pipeline

1. Apply allowlist globs → keep matches.
2. Apply blocklist globs → remove matches.
3. Parse `state` to float64 → discard non-numeric.
4. Apply epsilon change detection → skip unchanged.

---

## Insert strategy (Timescale)

### Batch inserts with CopyFrom

Every poll cycle, build a slice of rows to insert and write with `pgx.CopyFrom`:

* Single round-trip, binary protocol — simpler and faster than batched INSERTs for this use case.
* Use `pgxpool` for connection pooling.

```go
_, err := pool.CopyFrom(ctx,
    pgx.Identifier{"ha_numeric"},
    []string{"ts", "entity_id", "value", "unit"},
    pgx.CopyFromRows(rows),
)
```

### Idempotency / duplicates

No unique constraint on `(entity_id, ts)`. Epsilon-based dedup makes duplicates unlikely, and a constraint would complicate compression. Prefer visibility over silent dedup early on.

---

## Reliability and operations

### Scheduling

* Tick every minute, aligned to wall clock.
* Align to the next minute boundary to keep timestamps clean:

  * `next = now.Truncate(time.Minute).Add(time.Minute)`

### Timeouts and retries

* HTTP client timeout: 10s

* If HA call fails:

  * log error
  * try again next minute

* DB insert fail:

  * log error
  * do not crash the process
  * skip and continue (tolerate occasional miss, log it clearly)

### Backpressure / runaway

* If processing takes longer than 60s:

  * log warning with duration
  * skip overlapping cycles (no concurrent polls)

### Graceful shutdown

* On SIGTERM/SIGINT: cancel context, wait for in-flight DB write to complete (with a 5s deadline), then exit.
* Flush pending writes before shutting down — don't discard a completed poll cycle.

### Metrics and health

Expose Prometheus metrics (optional but recommended):

* poll success/fail counter
* cycle duration histogram
* rows inserted per cycle
* entities seen / skipped
* last successful poll time

Health endpoint:

* `/healthz` returns 200 if:

  * last successful HA poll within 2 minutes
  * DB pool is healthy (simple `SELECT 1` cached check)

---

## Config

Use environment variables (12-factor friendly):

* `HA_BASE_URL` (e.g. `http://homeassistant.local:8123`)
* `HA_TOKEN` (long-lived token)
* `POLL_INTERVAL` (default `1m`)
* `EPSILON_DEFAULT` (default `0`)
* `ENTITY_ALLOWLIST` (comma-separated globs, default `sensor.*`)
* `ENTITY_BLOCKLIST` (comma-separated globs, default empty)
* `PG_DSN` (e.g. `postgres://user:pass@host:5432/warehouse?sslmode=disable`)
* `HTTP_LISTEN_ADDR` (default `:8080`)
* `LOG_LEVEL` (info/debug)

Optional YAML config file for per-entity epsilon overrides (path set via `CONFIG_FILE` env var):

```yaml
epsilon_overrides:
  "sensor.kitchen_temperature": 0.05
  "sensor.outdoor_humidity": 0.1
```

---

## Project layout (Go)

```
cmd/ha-timescale-poller/main.go
internal/config/          # env + optional YAML parsing
internal/ha/              # HA HTTP client + JSON parsing
internal/filter/          # allowlist/blocklist glob matching
internal/store/           # pgxpool + CopyFrom insert + schema migration
internal/engine/          # poll loop, change detection, scheduling
internal/httpserver/       # /healthz + /metrics
schema.sql                # embedded via go:embed, run on startup
Dockerfile
docker-compose.yml        # poller + TimescaleDB
```

Dependencies:

* `github.com/jackc/pgx/v5/pgxpool`
* `github.com/caarlos0/env/v10` (env parsing)
* `gopkg.in/yaml.v3` (optional config file)
* `github.com/prometheus/client_golang/prometheus` (optional)

---

## Implementation steps

### Phase 1 — MVP (end-to-end working)

1. `go mod init`, set up project layout.
2. Embed schema SQL, run on startup to create hypertable.
3. Implement HA client: GET `/api/states`, parse JSON into structs.
4. Implement entity filtering: allowlist/blocklist with glob matching.
5. Implement numeric filter: parse `state` to float64, skip failures.
6. Implement store: pgxpool connect, `CopyFrom` insert.
7. Wire up poll loop: tick every minute, fetch → filter → insert.
8. Add `docker-compose.yml` with TimescaleDB + poller.

Acceptance criteria:

* You see rows in `ha_numeric`.
* Only entities matching allowlist (and not blocklist) appear.
* Non-numeric states are excluded.

### Phase 2 — Change detection (epsilon)

1. Add in-memory state map keyed by entity_id.
2. Implement epsilon compare + first-observation logic.
3. Add per-entity epsilon overrides via config file.
4. Add logs: rows inserted, skipped unchanged, entities seen.

Acceptance criteria:

* Constant sensors produce no repeated rows.
* Noisy sensors respect their epsilon threshold.
* On restart, one row per entity is written immediately.

### Phase 3 — Operational hardening

1. Add `/healthz` endpoint.
2. Add Prometheus `/metrics` endpoint.
3. Align polling to minute boundaries.
4. Add timeouts + retry logic for HA/DB.
5. Add graceful shutdown (context cancel, flush pending writes, drain).
6. Add backpressure: skip overlapping cycles, log warnings.

Acceptance criteria:

* Service stays up, restarts cleanly, reports health.
* No concurrent poll cycles.

### Phase 4 — Storage optimization

1. Add continuous aggregate `ha_numeric_1h`.
2. Add compression + retention policies (tune).
3. Grafana dashboards use:

   * raw for short ranges
   * 1h aggregate for long ranges

Acceptance criteria:

* Long-range dashboards are fast.
* Raw storage doesn't grow unbounded (if retention enabled).

---

## Deployment

### Docker Compose (recommended)

```yaml
services:
  timescaledb:
    image: timescale/timescaledb:latest-pg16
    environment:
      POSTGRES_PASSWORD: changeme
      POSTGRES_DB: warehouse
    volumes:
      - tsdb-data:/var/lib/postgresql/data
    ports:
      - "5432:5432"

  poller:
    build: .
    depends_on:
      - timescaledb
    environment:
      HA_BASE_URL: http://homeassistant.local:8123
      HA_TOKEN: ${HA_TOKEN}
      PG_DSN: postgres://postgres:changeme@timescaledb:5432/warehouse?sslmode=disable
    restart: unless-stopped

volumes:
  tsdb-data:
```

### Dockerfile

Multi-stage build:
* Build: Go builder image
* Runtime: distroless or alpine (minimal attack surface)

### systemd (alternative)

* `ExecStart=/usr/local/bin/ha-timescale-poller`
* `Restart=always`
* `EnvironmentFile=/etc/ha-timescale-poller.env`

---

## Testing plan

Priority order:

1. **Entity glob matching** — table-driven tests for allowlist/blocklist interaction, edge cases.
2. **Change detection decision function** — given `(currentValue, lastValue, epsilon)` → `shouldWrite`. Table-driven tests covering: first observation, exact match, within epsilon, outside epsilon.
3. **Numeric parsing + filtering** — `unknown`, `unavailable`, empty string, valid floats, integers.
4. **Integration test** (optional): run against a local Postgres in CI, insert a few points, query count.

---

## Grafana query examples

Recent (raw):

```sql
SELECT
  ts AS time,
  value
FROM ha_numeric
WHERE entity_id = 'sensor.kitchen_temperature'
  AND $__timeFilter(ts)
ORDER BY ts;
```

Note: Use Grafana's "connect null values" or "fill: previous" to handle gaps between change-only writes.

Hourly (rollup):

```sql
SELECT
  bucket AS time,
  avg
FROM ha_numeric_1h
WHERE entity_id = 'sensor.kitchen_temperature'
  AND $__timeFilter(bucket)
ORDER BY bucket;
```

---

## Tuning recommendations (starting values)

* `POLL_INTERVAL=1m`
* `EPSILON_DEFAULT=0.0` (raise to 0.01–0.05 if jitter becomes annoying)
* `ENTITY_ALLOWLIST=sensor.*`
* `ENTITY_BLOCKLIST=sensor.energy_*` (energy meters are better served by HA's own energy dashboard)
* Compression after 7 days
* Raw retention 90 days (optional; increase if you want raw longer)

---

## Risks and mitigations

* **Huge JSON payload from `/api/states`**:

  * Mitigation: still fine at 1/min; do not store attributes; filter early.
* **Sensor jitter causing too many writes**:

  * Mitigation: per-entity epsilon overrides.
* **HA restart / temporary failures**:

  * Mitigation: timeouts, health endpoint, restart policy. Skip and retry next cycle.
* **DB growth**:

  * Mitigation: compression + hourly rollups + retention policy.
* **Monotonically increasing sensors (energy meters)**:

  * Mitigation: blocklist `sensor.energy_*` by default. These change every poll and are better handled by HA's energy dashboard.
* **Process restart loses in-memory state**:

  * Acceptable: one extra write per entity on restart. No external state store needed.

---

## Definition of done

* Service runs for weeks without intervention.
* Timescale stores numeric sensor history with write-on-change (epsilon dedup).
* Entity filtering works via allowlist + blocklist with glob matching.
* Grafana dashboards are fast for both recent and long-term views.
* Deployment is one command (`docker compose up`), config via env vars.
