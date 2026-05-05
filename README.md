# hass-poller

A small Go service that polls Home Assistant every minute, extracts numeric sensor values, and writes them to TimescaleDB for long-term storage and Grafana visualization.

## Features

- Polls HA `/api/states` on a configurable interval (default 1 minute), aligned to wall-clock boundaries
- Entity filtering via **allowlist** and **blocklist** with glob pattern matching
- **Epsilon-based change detection** to avoid writing unchanged values (configurable per-entity)
- Batch inserts via `pgx.CopyFrom` for efficient writes
- TimescaleDB compression (7 days) and tiered retention: raw 90 days, hourly 1 year, daily kept forever
- Prometheus metrics at `/metrics` and health check at `/healthz`
- Graceful shutdown with in-flight write flush
- Single binary, distroless Docker image

## Quick start

1. Create a [long-lived access token](https://www.home-assistant.io/docs/authentication/#your-account-profile) in Home Assistant.

2. Start the stack:

```bash
export HA_TOKEN="your-token-here"
docker compose up -d
```

3. Verify it's working:

```bash
# Check health
curl http://localhost:8080/healthz

# Check logs
docker compose logs poller
```

The poller will immediately fetch all numeric `sensor.*` entities from HA and begin writing to TimescaleDB.

## Configuration

All configuration is via environment variables:

| Variable | Required | Default | Description |
|---|---|---|---|
| `HA_BASE_URL` | Yes | — | Home Assistant URL (e.g. `https://homeassistant.local:8123`) |
| `HA_TOKEN` | Yes | — | Long-lived access token |
| `PG_DSN` | Yes | — | PostgreSQL connection string |
| `POLL_INTERVAL` | No | `1m` | How often to poll HA |
| `HTTP_TIMEOUT` | No | `10s` | HTTP client timeout for HA API calls |
| `ENTITY_ALLOWLIST` | No | `sensor.*` | Comma-separated glob patterns to include |
| `ENTITY_BLOCKLIST` | No | _(empty)_ | Comma-separated glob patterns to exclude |
| `EPSILON_DEFAULT` | No | `0` | Minimum change threshold to trigger a write |
| `HTTP_LISTEN_ADDR` | No | `:8080` | Address for health/metrics HTTP server |
| `CONFIG_FILE` | No | _(empty)_ | Path to YAML config file for per-entity epsilon overrides |
| `LOG_LEVEL` | No | `info` | Log verbosity |

### Entity filtering

Entities are filtered in two stages using Go's [`path.Match`](https://pkg.go.dev/path#Match) glob syntax:

1. **Allowlist** — entity must match at least one pattern (default: `sensor.*`)
2. **Blocklist** — entity is excluded if it matches any pattern, even if it passed the allowlist

```bash
ENTITY_ALLOWLIST=sensor.*,binary_sensor.*
ENTITY_BLOCKLIST=sensor.energy_*,sensor.*_linkquality
```

### Epsilon overrides

To suppress writes from noisy sensors, set a per-entity epsilon threshold via a YAML config file:

```yaml
# config.yaml
epsilon_overrides:
  "sensor.kitchen_temperature": 0.05
  "sensor.outdoor_humidity": 0.1
```

```bash
CONFIG_FILE=/etc/hapoller/config.yaml
```

With `EPSILON_DEFAULT=0` (strict equality), a value is only written when it differs from the last written value. With a non-zero epsilon, the value must change by more than the threshold.

## Database schema

The schema is embedded in the binary and applied automatically on startup. It creates:

- **`ha_numeric`** — hypertable for raw measurements (`ts`, `entity_id`, `value`, `unit`); compressed after 7 days, dropped after 90 days
- **`ha_numeric_1h`** — continuous aggregate with hourly `avg`, `min`, `max`, `count` per entity; dropped after 1 year
- **`ha_numeric_1d`** — continuous aggregate with daily `avg`, `min`, `max`, `count` per entity; **kept forever** (no retention policy)

The tiered design means recent data has full resolution, mid-range queries (months) use the hourly rollup, and long-range historical queries (years+) use the daily rollup. Daily aggregates are computed from raw data within the 90-day window and persist after raw chunks are dropped.

## Grafana queries

Add a PostgreSQL datasource pointing to your TimescaleDB instance, then use these queries:

**Raw data (recent):**

```sql
SELECT ts AS time, value
FROM ha_numeric
WHERE entity_id = 'sensor.kitchen_temperature'
  AND $__timeFilter(ts)
ORDER BY ts;
```

**Hourly rollup (months):**

```sql
SELECT bucket AS time, avg, min, max
FROM ha_numeric_1h
WHERE entity_id = 'sensor.kitchen_temperature'
  AND $__timeFilter(bucket)
ORDER BY bucket;
```

**Daily rollup (long-range, years):**

```sql
SELECT bucket AS time, avg, min, max
FROM ha_numeric_1d
WHERE entity_id = 'sensor.kitchen_temperature'
  AND $__timeFilter(bucket)
ORDER BY bucket;
```

Use Grafana's "fill: previous" or "connect null values" setting to handle gaps between change-only writes.

## Backups

The `backups/` directory is gitignored. To take a logical backup of the warehouse database while the stack is running:

```bash
mkdir -p backups
docker exec hass-poller-timescaledb-1 pg_dump -U postgres -d warehouse -Fp \
  | gzip > "backups/warehouse-$(date +%Y%m%d-%H%M%S).sql.gz"
```

For consistency, stop the poller first so no writes happen during the dump:

```bash
docker compose stop poller
docker exec hass-poller-timescaledb-1 pg_dump -U postgres -d warehouse -Fp \
  | gzip > "backups/warehouse-$(date +%Y%m%d-%H%M%S).sql.gz"
docker compose start poller
```

`pg_dump` will print warnings about circular foreign-key constraints on TimescaleDB's internal `hypertable`, `chunk`, and `continuous_agg` catalog tables. These are expected and do not indicate a problem with your data.

To restore into an empty database:

```bash
gunzip -c backups/warehouse-YYYYMMDD-HHMMSS.sql.gz \
  | docker exec -i hass-poller-timescaledb-1 psql -U postgres -d warehouse
```

For a TimescaleDB-aware restore (recommended for migrations between major versions), see the [official restore guide](https://docs.timescale.com/self-hosted/latest/backup-and-restore/pg-dump-and-restore/).

## Endpoints

| Path | Description |
|---|---|
| `/healthz` | Returns 200 if last poll was within 2 minutes and DB is reachable |
| `/metrics` | Prometheus metrics (`hapoller_poll_total`, `hapoller_cycle_duration_seconds`, `hapoller_rows_inserted_total`, `hapoller_entities_seen`, `hapoller_entities_skipped`) |

## Project layout

```
cmd/ha-timescale-poller/main.go   # entrypoint
internal/config/                   # environment + YAML config loading
internal/engine/                   # poll loop, change detection, scheduling
internal/filter/                   # allowlist/blocklist glob matching
internal/ha/                       # Home Assistant API client
internal/httpserver/               # /healthz + /metrics server
internal/store/                    # pgxpool, CopyFrom inserts, schema migration
schema.sql                         # TimescaleDB schema (embedded via go:embed)
schema.go                          # go:embed directive
Dockerfile                         # multi-stage build (distroless runtime)
docker-compose.yml                 # poller + TimescaleDB
```

## Building

```bash
# Binary
go build -o ha-timescale-poller ./cmd/ha-timescale-poller

# Docker
docker build -t ha-timescale-poller .
```

## Running without Docker

```bash
export HA_BASE_URL=https://homeassistant.local:8123
export HA_TOKEN=your-token
export PG_DSN=postgres://user:pass@localhost:5432/warehouse?sslmode=disable

./ha-timescale-poller
```

Or via systemd:

```ini
[Unit]
Description=Home Assistant TimescaleDB Poller
After=network.target

[Service]
ExecStart=/usr/local/bin/ha-timescale-poller
Restart=always
EnvironmentFile=/etc/ha-timescale-poller.env

[Install]
WantedBy=multi-user.target
```

## License

MIT
