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
curl http://localhost:8080/healthz
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

```bash
ENTITY_ALLOWLIST=sensor.*,binary_sensor.*
ENTITY_BLOCKLIST=sensor.energy_*,sensor.*_linkquality
```

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md#filtering) for filter semantics.

### Epsilon overrides

Suppress writes from noisy sensors via a YAML config file:

```yaml
# config.yaml
epsilon_overrides:
  "sensor.kitchen_temperature": 0.05
  "sensor.outdoor_humidity": 0.1
```

```bash
CONFIG_FILE=/etc/hapoller/config.yaml
```

## Endpoints

| Path | Description |
|---|---|
| `/healthz` | Returns 200 if last poll was within 2 minutes and DB is reachable |
| `/metrics` | Prometheus metrics (`hapoller_poll_total`, `hapoller_cycle_duration_seconds`, `hapoller_rows_inserted_total`, `hapoller_entities_seen`, `hapoller_entities_skipped`) |

## Grafana queries

Point a PostgreSQL datasource at your TimescaleDB instance. Pick the table that matches your time range:

```sql
-- Recent (raw, 90 days):
SELECT ts AS time, value FROM ha_numeric
WHERE entity_id = 'sensor.kitchen_temperature' AND $__timeFilter(ts) ORDER BY ts;

-- Months (hourly rollup, 1 year):
SELECT bucket AS time, avg, min, max FROM ha_numeric_1h
WHERE entity_id = 'sensor.kitchen_temperature' AND $__timeFilter(bucket) ORDER BY bucket;

-- Years (daily rollup, kept forever):
SELECT bucket AS time, avg, min, max FROM ha_numeric_1d
WHERE entity_id = 'sensor.kitchen_temperature' AND $__timeFilter(bucket) ORDER BY bucket;
```

Use Grafana's "fill: previous" or "connect null values" to handle gaps between change-only writes.

## Backups

The `backups/` directory is gitignored. Quick `pg_dump` snapshot while the stack is running:

```bash
mkdir -p backups
docker compose stop poller
docker exec hass-poller-timescaledb-1 pg_dump -U postgres -d warehouse -Fp \
  | gzip > "backups/warehouse-$(date +%Y%m%d-%H%M%S).sql.gz"
docker compose start poller
```

To restore into an empty database:

```bash
gunzip -c backups/warehouse-YYYYMMDD-HHMMSS.sql.gz \
  | docker exec -i hass-poller-timescaledb-1 psql -U postgres -d warehouse
```

`pg_dump` warnings about circular foreign-key constraints on TimescaleDB's internal catalog tables are expected. For TimescaleDB-aware restores between major versions, see the [official guide](https://docs.timescale.com/self-hosted/latest/backup-and-restore/pg-dump-and-restore/).

## Documentation

- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — components, data flow, schema details
- [`docs/DEVELOPMENT.md`](docs/DEVELOPMENT.md) — build, run, test
- [`CONTRIBUTING.md`](CONTRIBUTING.md) — branch naming, PR workflow, release process
- [`CHANGELOG.md`](CHANGELOG.md) — release notes

## License

MIT
