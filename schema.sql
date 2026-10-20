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

-- Linear unit conversions used by the rollups: base = value * base_scale + base_offset.
-- Units sharing a dimension are interconvertible. Existing rows are never
-- overwritten, so local additions and corrections survive restarts.
CREATE TABLE IF NOT EXISTS ha_unit (
  unit        text             PRIMARY KEY,
  dimension   text             NOT NULL,
  base_scale  double precision NOT NULL CHECK (base_scale <> 0),
  base_offset double precision NOT NULL DEFAULT 0
);

INSERT INTO ha_unit (unit, dimension, base_scale, base_offset) VALUES
  ('°C', 'temperature', 1, 0),
  ('°F', 'temperature', 5.0 / 9, -160.0 / 9),
  ('K', 'temperature', 1, -273.15),
  ('mW', 'power', 1e-3, 0), ('W', 'power', 1, 0), ('kW', 'power', 1e3, 0),
  ('MW', 'power', 1e6, 0), ('GW', 'power', 1e9, 0), ('TW', 'power', 1e12, 0),
  ('BTU/h', 'power', 1055.05585262 / 3600, 0),
  ('mWh', 'energy', 1e-3, 0), ('Wh', 'energy', 1, 0), ('kWh', 'energy', 1e3, 0),
  ('MWh', 'energy', 1e6, 0), ('GWh', 'energy', 1e9, 0), ('TWh', 'energy', 1e12, 0),
  ('J', 'energy', 1.0 / 3600, 0), ('kJ', 'energy', 1e3 / 3600, 0),
  ('MJ', 'energy', 1e6 / 3600, 0), ('GJ', 'energy', 1e9 / 3600, 0),
  ('cal', 'energy', 4.184 / 3600, 0), ('kcal', 'energy', 4.184e3 / 3600, 0),
  ('Mcal', 'energy', 4.184e6 / 3600, 0), ('Gcal', 'energy', 4.184e9 / 3600, 0),
  ('VA', 'apparent_power', 1, 0), ('kVA', 'apparent_power', 1e3, 0),
  ('var', 'reactive_power', 1, 0), ('kvar', 'reactive_power', 1e3, 0),
  ('Pa', 'pressure', 1, 0), ('hPa', 'pressure', 100, 0), ('kPa', 'pressure', 1e3, 0),
  ('bar', 'pressure', 1e5, 0), ('cbar', 'pressure', 1e3, 0), ('mbar', 'pressure', 100, 0),
  ('mmHg', 'pressure', 133.322387415, 0), ('inHg', 'pressure', 3386.389, 0),
  ('psi', 'pressure', 6894.757293168, 0),
  ('mm', 'distance', 1e-3, 0), ('cm', 'distance', 1e-2, 0), ('m', 'distance', 1, 0),
  ('km', 'distance', 1e3, 0), ('in', 'distance', 0.0254, 0), ('ft', 'distance', 0.3048, 0),
  ('yd', 'distance', 0.9144, 0), ('mi', 'distance', 1609.344, 0), ('nmi', 'distance', 1852, 0),
  ('m/s', 'speed', 1, 0), ('km/h', 'speed', 1 / 3.6, 0), ('mph', 'speed', 0.44704, 0),
  ('ft/s', 'speed', 0.3048, 0), ('kn', 'speed', 1852.0 / 3600, 0), ('mm/s', 'speed', 1e-3, 0),
  ('mm/h', 'speed', 1e-3 / 3600, 0), ('in/h', 'speed', 0.0254 / 3600, 0),
  ('mm/d', 'speed', 1e-3 / 86400, 0), ('in/d', 'speed', 0.0254 / 86400, 0),
  ('mL', 'volume', 1e-3, 0), ('L', 'volume', 1, 0), ('m³', 'volume', 1e3, 0),
  ('ft³', 'volume', 28.316846592, 0), ('CCF', 'volume', 2831.6846592, 0),
  ('gal', 'volume', 3.785411784, 0), ('fl. oz.', 'volume', 0.0295735295625, 0),
  ('mL/s', 'volume_flow_rate', 0.06, 0), ('L/s', 'volume_flow_rate', 60, 0),
  ('L/min', 'volume_flow_rate', 1, 0), ('L/h', 'volume_flow_rate', 1.0 / 60, 0),
  ('m³/s', 'volume_flow_rate', 6e4, 0), ('m³/h', 'volume_flow_rate', 1e3 / 60, 0),
  ('ft³/min', 'volume_flow_rate', 28.316846592, 0), ('gal/min', 'volume_flow_rate', 3.785411784, 0),
  ('µg', 'mass', 1e-6, 0), ('mg', 'mass', 1e-3, 0), ('g', 'mass', 1, 0), ('kg', 'mass', 1e3, 0),
  ('oz', 'mass', 28.349523125, 0), ('lb', 'mass', 453.59237, 0), ('st', 'mass', 6350.29318, 0),
  ('µs', 'duration', 1e-6, 0), ('ms', 'duration', 1e-3, 0), ('s', 'duration', 1, 0),
  ('min', 'duration', 60, 0), ('h', 'duration', 3600, 0), ('d', 'duration', 86400, 0),
  ('w', 'duration', 604800, 0),
  ('mA', 'current', 1e-3, 0), ('A', 'current', 1, 0),
  ('µV', 'voltage', 1e-6, 0), ('mV', 'voltage', 1e-3, 0), ('V', 'voltage', 1, 0),
  ('kV', 'voltage', 1e3, 0), ('MV', 'voltage', 1e6, 0),
  ('Hz', 'frequency', 1, 0), ('kHz', 'frequency', 1e3, 0), ('MHz', 'frequency', 1e6, 0),
  ('GHz', 'frequency', 1e9, 0),
  ('bit', 'information', 0.125, 0), ('kbit', 'information', 125, 0),
  ('Mbit', 'information', 1.25e5, 0), ('Gbit', 'information', 1.25e8, 0),
  ('B', 'information', 1, 0), ('kB', 'information', 1e3, 0), ('MB', 'information', 1e6, 0),
  ('GB', 'information', 1e9, 0), ('TB', 'information', 1e12, 0), ('PB', 'information', 1e15, 0),
  ('KiB', 'information', 1024, 0), ('MiB', 'information', 1048576, 0),
  ('GiB', 'information', 1073741824, 0), ('TiB', 'information', 1099511627776, 0),
  ('PiB', 'information', 1125899906842624, 0),
  ('bit/s', 'data_rate', 0.125, 0), ('kbit/s', 'data_rate', 125, 0),
  ('Mbit/s', 'data_rate', 1.25e5, 0), ('Gbit/s', 'data_rate', 1.25e8, 0),
  ('B/s', 'data_rate', 1, 0), ('kB/s', 'data_rate', 1e3, 0), ('MB/s', 'data_rate', 1e6, 0),
  ('GB/s', 'data_rate', 1e9, 0), ('KiB/s', 'data_rate', 1024, 0),
  ('MiB/s', 'data_rate', 1048576, 0), ('GiB/s', 'data_rate', 1073741824, 0)
ON CONFLICT (unit) DO NOTHING;

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

-- Time-weighted rollups. Each row reports the unit in effect at the end of its
-- bucket; earlier values in other units are converted when ha_unit can, and
-- otherwise left out. seconds is how much of the bucket had a known value.
CREATE TABLE IF NOT EXISTS ha_numeric_1h (
  bucket    timestamptz      NOT NULL,
  entity_id text             NOT NULL,
  unit      text             NULL,
  avg       double precision NOT NULL,
  min       double precision NOT NULL,
  max       double precision NOT NULL,
  n         bigint           NOT NULL,
  seconds   double precision NULL,
  PRIMARY KEY (entity_id, bucket)
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
  PRIMARY KEY (entity_id, bucket)
);

SELECT create_hypertable('ha_numeric_1d', 'bucket',
  chunk_time_interval => INTERVAL '1 year', if_not_exists => TRUE);
-- No retention policy on ha_numeric_1d: kept forever.

-- A raw row's value holds until the entity's next row, or for at most hold when
-- no row follows. The poller rewrites unchanged values every 15 minutes, so
-- hold only expires when the entity stops reporting numeric values.
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
    SELECT r.entity_id, r.value, coalesce(r.unit, '') AS unit, r.ts AS seg_start,
           least(lead(r.ts) OVER w, r.ts + hold, range_end) AS seg_end
    FROM ha_numeric r
    WHERE r.ts >= range_start - hold AND r.ts < range_end
    WINDOW w AS (PARTITION BY r.entity_id ORDER BY r.ts)
  ),
  pieces AS (
    SELECT s.entity_id, s.value, s.unit, b.bucket,
           greatest(s.seg_start, b.bucket) AS piece_start,
           least(s.seg_end, b.bucket + width) AS piece_end,
           s.seg_start >= b.bucket AS recorded_here
    FROM segments s
    CROSS JOIN LATERAL generate_series(
      time_bucket(width, greatest(s.seg_start, range_start)),
      s.seg_end - INTERVAL '1 microsecond',
      width
    ) AS b(bucket)
    WHERE s.seg_end > greatest(s.seg_start, range_start)
  ),
  targeted AS (
    SELECT p.*,
           last_value(p.unit) OVER (
             PARTITION BY p.entity_id, p.bucket ORDER BY p.piece_start
             ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING
           ) AS target_unit
    FROM pieces p
  ),
  converted AS (
    SELECT t.entity_id, t.bucket, t.target_unit, t.recorded_here,
           CASE WHEN t.unit = t.target_unit THEN t.value
                ELSE (t.value * fu.base_scale + fu.base_offset - tu.base_offset) / tu.base_scale
           END AS value,
           extract(epoch FROM t.piece_end - t.piece_start)::double precision AS secs
    FROM targeted t
    LEFT JOIN ha_unit fu ON fu.unit = t.unit
    LEFT JOIN ha_unit tu ON tu.unit = t.target_unit
    WHERE t.unit = t.target_unit OR fu.dimension = tu.dimension
  )
  SELECT c.bucket, c.entity_id, nullif(c.target_unit, ''),
         sum(c.value * c.secs) / sum(c.secs), min(c.value), max(c.value),
         count(*) FILTER (WHERE c.recorded_here), sum(c.secs)
  FROM converted c
  GROUP BY c.bucket, c.entity_id, c.target_unit
$$;

-- Recompute every rollup bucket overlapping [range_start, range_end).
CREATE OR REPLACE FUNCTION ha_numeric_refresh_rollup(
  rollup      regclass,
  width       interval,
  range_start timestamptz,
  range_end   timestamptz,
  hold        interval DEFAULT INTERVAL '1 hour'
)
RETURNS bigint
LANGUAGE plpgsql
SET timezone = 'UTC'
AS $$
DECLARE
  lo timestamptz := time_bucket(width, range_start);
  hi timestamptz := time_bucket(width, range_end);
  written bigint;
BEGIN
  IF hi < range_end THEN
    hi := hi + width;
  END IF;
  -- Serialize the scheduled job and backfill refreshes of the same rollup.
  PERFORM pg_advisory_xact_lock(rollup::oid::bigint);
  EXECUTE format('DELETE FROM %s WHERE bucket >= $1 AND bucket < $2', rollup) USING lo, hi;
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
    coalesce((config->>'hold')::interval, INTERVAL '1 hour')
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

-- Finish migrating pre-0.3.0 rollups: keep the legacy rows (seconds IS NULL,
-- unit unknown) only where raw data no longer exists, and recompute the rest.
DO $$
DECLARE
  rollup text;
  width interval;
  raw_start timestamptz := (SELECT min(ts) FROM ha_numeric);
BEGIN
  FOR rollup, width IN VALUES ('ha_numeric_1h', INTERVAL '1 hour'), ('ha_numeric_1d', INTERVAL '1 day') LOOP
    IF to_regclass(rollup || '_legacy') IS NOT NULL THEN
      EXECUTE format('INSERT INTO %I (bucket, entity_id, unit, avg, min, max, n, seconds)
                      SELECT bucket, entity_id, NULL, avg, min, max, n, NULL FROM %I
                      WHERE $1 IS NULL OR bucket < time_bucket($2, $1, ''UTC'')
                      ON CONFLICT DO NOTHING', rollup, rollup || '_legacy')
        USING raw_start, width;
      IF raw_start IS NOT NULL THEN
        PERFORM ha_numeric_refresh_rollup(rollup::regclass, width, raw_start,
                                          time_bucket(width, now(), 'UTC'));
      END IF;
      EXECUTE format('DROP TABLE %I', rollup || '_legacy');
    END IF;
  END LOOP;
END
$$;
