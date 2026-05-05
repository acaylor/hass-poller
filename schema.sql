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
GROUP BY bucket, entity_id
WITH NO DATA;

SELECT add_continuous_aggregate_policy('ha_numeric_1h',
  start_offset => INTERVAL '7 days',
  end_offset   => INTERVAL '1 hour',
  schedule_interval => INTERVAL '15 minutes',
  if_not_exists => TRUE
);

SELECT add_retention_policy('ha_numeric_1h', INTERVAL '1 year', if_not_exists => TRUE);

CREATE MATERIALIZED VIEW IF NOT EXISTS ha_numeric_1d
WITH (timescaledb.continuous) AS
SELECT
  time_bucket('1 day', ts) AS bucket,
  entity_id,
  avg(value) AS avg,
  min(value) AS min,
  max(value) AS max,
  count(*)   AS n
FROM ha_numeric
GROUP BY bucket, entity_id
WITH NO DATA;

SELECT add_continuous_aggregate_policy('ha_numeric_1d',
  start_offset => INTERVAL '7 days',
  end_offset   => INTERVAL '1 day',
  schedule_interval => INTERVAL '1 hour',
  if_not_exists => TRUE
);
-- No retention policy on ha_numeric_1d: kept forever.
