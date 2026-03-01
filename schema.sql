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
