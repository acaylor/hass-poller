CREATE EXTENSION IF NOT EXISTS timescaledb;

CREATE TABLE IF NOT EXISTS ha_numeric (
  ts        timestamptz      NOT NULL,
  entity_id text             NOT NULL,
  value     double precision NOT NULL,
  unit      text             NULL
);

SELECT create_hypertable('ha_numeric', 'ts', if_not_exists => TRUE);

CREATE INDEX IF NOT EXISTS ha_numeric_entity_ts_desc
ON ha_numeric (entity_id, ts DESC);

ALTER TABLE ha_numeric SET (
  timescaledb.compress,
  timescaledb.compress_segmentby = 'entity_id'
);

SELECT add_compression_policy('ha_numeric', INTERVAL '7 days', if_not_exists => TRUE);

SELECT add_retention_policy('ha_numeric', INTERVAL '90 days', if_not_exists => TRUE);

-- Versions before 0.3.0 stored the rollups as continuous aggregates holding
-- arithmetic means of recorded changes. Copy them aside so the tables below can
-- take their names; the copies are merged and dropped further down.
DO $$
DECLARE
  rollup text;
BEGIN
  FOREACH rollup IN ARRAY ARRAY['ha_numeric_1h', 'ha_numeric_1d'] LOOP
    IF EXISTS (SELECT 1 FROM timescaledb_information.continuous_aggregates
               WHERE view_schema = current_schema() AND view_name = rollup) THEN
      EXECUTE format('CREATE TABLE %I AS SELECT bucket, entity_id, avg, min, max, n FROM %I',
                     rollup || '_legacy', rollup);
      EXECUTE format('DROP MATERIALIZED VIEW %I', rollup);
    END IF;
  END LOOP;
END
$$;

-- Time-weighted rollups with one row per entity, unit, and bucket, so values in
-- different units are never aggregated together. seconds is how much of the
-- bucket had a known value in that unit.
CREATE TABLE IF NOT EXISTS ha_numeric_1h (
  bucket    timestamptz      NOT NULL,
  entity_id text             NOT NULL,
  unit      text             NULL,
  avg       double precision NOT NULL,
  min       double precision NOT NULL,
  max       double precision NOT NULL,
  n         bigint           NOT NULL,
  seconds   double precision NULL,
  UNIQUE NULLS NOT DISTINCT (entity_id, unit, bucket)
);

SELECT create_hypertable('ha_numeric_1h', 'bucket',
  chunk_time_interval => INTERVAL '30 days', if_not_exists => TRUE);

SELECT add_retention_policy('ha_numeric_1h', INTERVAL '1 year', if_not_exists => TRUE);

CREATE TABLE IF NOT EXISTS ha_numeric_1d (
  bucket    timestamptz      NOT NULL,
  entity_id text             NOT NULL,
  unit      text             NULL,
  avg       double precision NOT NULL,
  min       double precision NOT NULL,
  max       double precision NOT NULL,
  n         bigint           NOT NULL,
  seconds   double precision NULL,
  UNIQUE NULLS NOT DISTINCT (entity_id, unit, bucket)
);

SELECT create_hypertable('ha_numeric_1d', 'bucket',
  chunk_time_interval => INTERVAL '1 year', if_not_exists => TRUE);
-- No retention policy on ha_numeric_1d: kept forever.

-- A raw row's value holds until the entity's next row, whatever its unit, or for
-- at most hold when no row follows. The poller rewrites unchanged values every
-- 15 minutes, so hold only expires when the entity stops reporting numeric
-- values. Each unit's values are aggregated separately.
CREATE OR REPLACE FUNCTION ha_numeric_time_weighted(
  width       interval,
  range_start timestamptz,
  range_end   timestamptz,
  hold        interval DEFAULT INTERVAL '1 hour'
)
RETURNS TABLE (
  bucket    timestamptz,
  entity_id text,
  unit      text,
  avg       double precision,
  min       double precision,
  max       double precision,
  n         bigint,
  seconds   double precision
)
LANGUAGE sql STABLE
SET timezone = 'UTC'
AS $$
  WITH segments AS (
    SELECT r.entity_id, r.value, r.unit, r.ts AS seg_start,
           least(lead(r.ts) OVER w, r.ts + hold, range_end) AS seg_end
    FROM ha_numeric r
    WHERE r.ts >= range_start - hold AND r.ts < range_end
    WINDOW w AS (PARTITION BY r.entity_id ORDER BY r.ts)
  ),
  pieces AS (
    SELECT s.entity_id, s.value, s.unit, b.bucket,
           s.seg_start >= b.bucket AS recorded_here,
           extract(epoch FROM least(s.seg_end, b.bucket + width)
                              - greatest(s.seg_start, b.bucket))::double precision AS secs
    FROM segments s
    CROSS JOIN LATERAL generate_series(
      time_bucket(width, greatest(s.seg_start, range_start)),
      s.seg_end - INTERVAL '1 microsecond',
      width
    ) AS b(bucket)
    WHERE s.seg_end > greatest(s.seg_start, range_start)
  )
  SELECT p.bucket, p.entity_id, p.unit,
         sum(p.value * p.secs) / sum(p.secs), min(p.value), max(p.value),
         count(*) FILTER (WHERE p.recorded_here), sum(p.secs)
  FROM pieces p
  GROUP BY p.bucket, p.entity_id, p.unit
$$;

-- Recompute every completed rollup bucket that raw rows in
-- [range_start, range_end) contribute to, including the buckets their values
-- hold into after range_end. A bucket of an entity is replaced only when the
-- entity has raw rows that can contribute to it, so rollups whose raw data
-- has expired are kept. A NULL hold uses the rollup job's configured hold.
CREATE OR REPLACE FUNCTION ha_numeric_refresh_rollup(
  rollup      regclass,
  width       interval,
  range_start timestamptz,
  range_end   timestamptz,
  hold        interval DEFAULT NULL
)
RETURNS bigint
LANGUAGE plpgsql
SET timezone = 'UTC'
AS $$
DECLARE
  lo timestamptz := time_bucket(width, range_start);
  hi timestamptz;
  written bigint;
BEGIN
  IF hold IS NULL THEN
    SELECT (j.config->>'hold')::interval INTO hold
    FROM timescaledb_information.jobs j
    JOIN pg_class c ON c.oid = rollup
    JOIN pg_namespace ns ON ns.oid = c.relnamespace
    WHERE j.proc_name = 'ha_numeric_rollup_job' AND j.proc_schema = ns.nspname
      AND j.config->>'rollup' = c.relname;
    hold := coalesce(hold, INTERVAL '1 hour');
  END IF;
  hi := time_bucket(width, range_end + hold);
  IF hi < range_end + hold THEN
    hi := hi + width;
  END IF;
  hi := least(hi, time_bucket(width, now()));
  IF hi <= lo THEN
    RETURN 0;
  END IF;
  -- Serialize the scheduled job and backfill refreshes of the same rollup.
  PERFORM pg_advisory_xact_lock(rollup::oid::bigint);
  -- A raw row contributes to a bucket when its value holds into the bucket.
  EXECUTE format('DELETE FROM %s t WHERE t.bucket >= $1 AND t.bucket < $2 AND EXISTS (
                    SELECT 1 FROM ha_numeric r WHERE r.entity_id = t.entity_id
                    AND r.ts > t.bucket - $3 AND r.ts < t.bucket + $4)', rollup)
    USING lo, hi, hold, width;
  EXECUTE format('INSERT INTO %s (bucket, entity_id, unit, avg, min, max, n, seconds)
                  SELECT * FROM ha_numeric_time_weighted($1, $2, $3, $4)', rollup)
    USING width, lo, hi, hold;
  GET DIAGNOSTICS written = ROW_COUNT;
  RETURN written;
END
$$;

-- Scheduled refresh of the completed buckets within config.lookback.
CREATE OR REPLACE PROCEDURE ha_numeric_rollup_job(job_id int, config jsonb)
LANGUAGE plpgsql
AS $$
DECLARE
  width interval := (config->>'width')::interval;
BEGIN
  PERFORM ha_numeric_refresh_rollup(
    (config->>'rollup')::regclass,
    width,
    now() - (config->>'lookback')::interval,
    time_bucket(width, now(), 'UTC'),
    (config->>'hold')::interval
  );
END
$$;

SELECT add_job('ha_numeric_rollup_job', INTERVAL '15 minutes',
  config => '{"rollup": "ha_numeric_1h", "width": "1 hour", "lookback": "1 day"}')
WHERE NOT EXISTS (SELECT 1 FROM timescaledb_information.jobs
                  WHERE proc_schema = current_schema() AND proc_name = 'ha_numeric_rollup_job'
                    AND config->>'rollup' = 'ha_numeric_1h');

SELECT add_job('ha_numeric_rollup_job', INTERVAL '1 hour',
  config => '{"rollup": "ha_numeric_1d", "width": "1 day", "lookback": "2 days"}')
WHERE NOT EXISTS (SELECT 1 FROM timescaledb_information.jobs
                  WHERE proc_schema = current_schema() AND proc_name = 'ha_numeric_rollup_job'
                    AND config->>'rollup' = 'ha_numeric_1d');

-- Finish migrating pre-0.3.0 rollups: recompute everything raw data still
-- covers, then keep each legacy row (seconds IS NULL, unit unknown) whose entity
-- and bucket were not recomputed.
DO $$
DECLARE
  rollup text;
  width interval;
  raw_start timestamptz := (SELECT min(ts) FROM ha_numeric);
BEGIN
  FOR rollup, width IN VALUES ('ha_numeric_1h', INTERVAL '1 hour'), ('ha_numeric_1d', INTERVAL '1 day') LOOP
    IF to_regclass(rollup || '_legacy') IS NOT NULL THEN
      IF raw_start IS NOT NULL THEN
        PERFORM ha_numeric_refresh_rollup(rollup::regclass, width, raw_start,
                                          time_bucket(width, now(), 'UTC'));
      END IF;
      EXECUTE format('INSERT INTO %1$I (bucket, entity_id, unit, avg, min, max, n, seconds)
                      SELECT l.bucket, l.entity_id, NULL, l.avg, l.min, l.max, l.n, NULL FROM %2$I l
                      WHERE NOT EXISTS (SELECT 1 FROM %1$I r
                                        WHERE r.entity_id = l.entity_id AND r.bucket = l.bucket)',
                     rollup, rollup || '_legacy');
      EXECUTE format('DROP TABLE %I', rollup || '_legacy');
    END IF;
  END LOOP;
END
$$;
