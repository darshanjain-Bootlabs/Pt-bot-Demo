CREATE TABLE IF NOT EXISTS metrics (
  time TIMESTAMPTZ NOT NULL,
  script TEXT NOT NULL,
  requests INT,
  avg_latency FLOAT8,
  p95_latency FLOAT8
);

SELECT create_hypertable('metrics', 'time', if_not_exists => TRUE);

CREATE INDEX IF NOT EXISTS idx_metrics_script_time ON metrics (script, time DESC);
CREATE INDEX IF NOT EXISTS idx_metrics_time ON metrics (time DESC);

ALTER TABLE metrics SET (
  timescaledb.compress,
  timescaledb.compress_orderby = 'time DESC'
);

SELECT add_compression_policy('metrics', INTERVAL '7 days', if_not_exists => TRUE);
