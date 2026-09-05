# Backfilling after an outage

If the poller or its database is down for a while, you lose the rows that would
have been written during the gap. Because Home Assistant keeps its own state
history independently of this service, you can usually recover that window. The
`backfill` command (in [`cmd/backfill`](../cmd/backfill)) pulls recorded state
changes from Home Assistant and writes them into `ha_numeric`, then refreshes the
hourly and daily rollups for the affected range.

This runbook is deployment-agnostic: it does not assume how or where your
Postgres/TimescaleDB instance runs. Adapt the connection details and the "how to
run the command" section to your environment.

## How it works

`backfill` reuses the live poller's configuration and logic — the same entity
allow/block filtering, numeric parsing, and epsilon change-detection — so the
rows it writes are consistent with what the poller produces. The differences:

- **Source.** It calls Home Assistant's history API
  (`GET /api/history/period`) instead of `/api/states`.
- **Timestamps.** Rows use Home Assistant's real `last_changed` time, so the
  backfilled window is actually *higher resolution* than the poller's
  once-per-`POLL_INTERVAL` snapshots. Expect the backfilled range to look denser
  than the periods around it; this is more accurate, not a bug. If you would
  rather it blend in, run with a non-zero `EPSILON_DEFAULT` to suppress small
  changes.
- **Aggregate refresh.** After inserting, it calls
  `refresh_continuous_aggregate` on `ha_numeric_1h` and `ha_numeric_1d` for the
  full hourly and daily buckets touched by the range. The automatic refresh policies only cover the recent window
  (`start_offset` of 7 days), so backfilled history would otherwise never reach
  the rollups.

## Data sources and their limits

| Source | Resolution | Retention | Used by |
|---|---|---|---|
| HA recorder history | every state change | `recorder.purge_keep_days` (default **10 days**) | `backfill` (this tool) |
| HA long-term statistics | hourly mean/min/max | kept indefinitely (sensors with a `state_class`) | not yet automated — see [Gaps older than recorder retention](#gaps-older-than-recorder-retention) |

The practical takeaway: **`backfill` can only recover gaps that still exist in
Home Assistant's recorder.** Check your `purge_keep_days` before relying on it.
The raw `ha_numeric` retention policy (default 90 days) also bounds how far back
a raw backfill is worth doing — older data should live only in the rollups.

## Prerequisites

`backfill` reads the **same environment variables as the poller** (see the
[README configuration table](../README.md#configuration)). At minimum:

- `HA_BASE_URL`, `HA_TOKEN` — to read history.
- `PG_DSN` — to write rows and refresh aggregates.
- `ENTITY_ALLOWLIST` / `ENTITY_BLOCKLIST`, `EPSILON_DEFAULT`, `CONFIG_FILE` —
  set these to the **same values your poller uses** so the backfill matches.

You also need a way to run SQL against your database (`psql`, Adminer, your
cloud provider's console, `kubectl exec`, etc.) to scope the gap and verify the
result. The queries below are standard SQL — run them with whatever client you
already use.

## Procedure

### 1. Find the gap

Look for the last row before the outage and the first row after it. Run against
your database with any client:

```sql
-- Most recent data and how stale it is:
SELECT max(ts) AS latest, now() - max(ts) AS staleness FROM ha_numeric;

-- Row counts per hour over the suspected window; missing/zero hours are the gap:
SELECT date_trunc('hour', ts) AS hour, count(*) AS rows
FROM ha_numeric
WHERE ts > now() - interval '48 hours'
GROUP BY 1 ORDER BY 1;

-- Exact boundaries (adjust the pivot timestamp to sit inside the gap):
SELECT max(ts) AS last_before_gap FROM ha_numeric WHERE ts < 'PIVOT';
SELECT min(ts) AS first_after_gap FROM ha_numeric WHERE ts > 'PIVOT';
```

Pick a `-start` just **after** `last_before_gap` and an `-end` just **before**
`first_after_gap`. Keeping the window strictly inside the gap avoids creating
duplicate rows (see [Caveats](#caveats)).

### 2. Dry run

A dry run fetches from Home Assistant and reports how many rows *would* be
written, without touching the database. Always do this first to confirm Home
Assistant still has the window:

```bash
backfill -start 2026-01-20T16:16:00Z -end 2026-01-22T02:34:00Z -dry-run
```

If the counts are surprisingly low or zero, the gap likely predates your
recorder's `purge_keep_days`.

### 3. Backfill

Drop `-dry-run` to perform the write. The command inserts the rows and refreshes
the aggregates for the range:

```bash
backfill -start 2026-01-20T16:16:00Z -end 2026-01-22T02:34:00Z
```

### 4. Verify

Re-run the per-hour query from step 1 — the gap hours should now be populated —
and confirm the rollups were refreshed:

```sql
SELECT count(*) FROM ha_numeric_1h WHERE bucket >= time_bucket('1 hour', 'START'::timestamptz) AND bucket < 'END';
SELECT count(*) FROM ha_numeric_1d WHERE bucket >= time_bucket('1 day', 'START'::timestamptz) AND bucket < 'END';
```

Refresh windows expand to complete UTC buckets because TimescaleDB excludes partial
buckets from a refresh. See the [TimescaleDB refresh API](https://github.com/timescale/docs/blob/latest/api/continuous-aggregates/refresh_continuous_aggregate.md).

## Flags

| Flag | Description |
|---|---|
| `-start` | Gap start, RFC3339 (required) |
| `-end` | Gap end, RFC3339 (default: now) |
| `-replace` | Atomically replace rows in `[start, end)` for entities with replacement data |
| `-no-epsilon` | Insert every recorded change without epsilon suppression |
| `-no-refresh` | Skip refreshing the continuous aggregates |
| `-dry-run` | Fetch and report counts without writing |

## Running the command

Use whichever fits your deployment. In every case the binary reads its config
from the environment, exactly like the poller.

### From source

```bash
# Set HA_BASE_URL, HA_TOKEN, PG_DSN (and any filter/epsilon vars) in the env first.
go run ./cmd/backfill -start <RFC3339> -end <RFC3339> -dry-run
```

### Docker

The published image ships both binaries. Override the entrypoint:

```bash
docker run --rm \
  -e HA_BASE_URL -e HA_TOKEN -e PG_DSN \
  --entrypoint /usr/local/bin/backfill \
  ghcr.io/acaylor/hass-poller:<tag> \
  -start <RFC3339> -end <RFC3339> -dry-run
```

If your Postgres runs in the same Compose stack, run it on that network and use
the service hostname in `PG_DSN`.

### Kubernetes (one-shot Job)

Reuse the same Secrets/ConfigMaps your poller Deployment already references, so
no credentials are handled manually. Replace the placeholders:

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: hass-poller-backfill
spec:
  backoffLimit: 0          # no retries: re-running could double-insert (see Caveats)
  ttlSecondsAfterFinished: 86400
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: backfill
          image: ghcr.io/acaylor/hass-poller:<tag>
          command: ["/usr/local/bin/backfill"]
          args: ["-start", "<RFC3339>", "-end", "<RFC3339>"]
          envFrom:
            - secretRef:
                name: <your-poller-env-secret>   # must provide HA_TOKEN, PG_DSN, ...
          env:
            - name: HA_BASE_URL
              value: <your-ha-url>
```

## Caveats

- **Recorder retention.** `backfill` can only recover what is still in Home
  Assistant's recorder (`purge_keep_days`, default 10 days). Check it first.
- **No unique key.** `ha_numeric` has no unique constraint, so overlapping a
  range the poller already wrote will create **duplicate rows**. Scope `-start`/
  `-end` strictly inside the gap, or use `-replace` to replace those entities atomically.
- **Replacement scope.** `-replace` deletes and inserts in one transaction.
  An insert failure rolls back the deletion. Entities excluded by the filters
  or with no numeric replacement rows are left untouched.
- **Compression.** TimescaleDB compresses chunks older than the compression
  policy window (default 7 days). Inserting into — or `-replace`-deleting from —
  a compressed chunk may fail unless you `decompress_chunk()` it first. Recent
  outages (within the compression window) are unaffected.
- **Raw retention.** A raw backfill older than the `ha_numeric` retention policy
  (default 90 days) will be purged again. Beyond that window, only the rollups
  are meaningful, and continuous aggregates cannot be inserted into directly.
- **Resolution.** Backfilled rows come from Home Assistant's change events, so
  the window is denser than the poller's interval snapshots. Use
  `EPSILON_DEFAULT` to thin it if uniformity matters.
- **Removed entities.** The tool enumerates entities from Home Assistant's
  *current* `/api/states` (the history API requires an explicit entity filter).
  An entity that existed only during the gap and has since been removed from
  Home Assistant won't be backfilled.

## Gaps older than recorder retention

Once a gap falls outside `recorder.purge_keep_days`, the per-change history is
gone, but Home Assistant retains **long-term statistics** (hourly mean/min/max,
kept indefinitely) for any sensor with a `state_class`. These map onto the
`ha_numeric_1h` rollup rather than the raw table. Importing them is not yet
automated by this tool; it requires Home Assistant's
`recorder/statistics_during_period` WebSocket API. Track this as a future
enhancement if you need to recover older gaps.
