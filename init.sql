-- ===================== METRICS TABLE =====================
-- Schema expected by xk6-output-timescaledb extension
-- This table is written to automatically by k6 during test runs
CREATE TABLE IF NOT EXISTS metrics (
    time        TIMESTAMPTZ     NOT NULL,
    metric      VARCHAR(128)    NOT NULL,
    tags        JSONB,
    value       FLOAT           NOT NULL
);

-- Convert to hypertable for time-series optimization
SELECT create_hypertable('metrics', 'time', if_not_exists => TRUE);

-- Indexes for fast Grafana queries
CREATE INDEX IF NOT EXISTS idx_metrics_time       ON metrics (time DESC);
CREATE INDEX IF NOT EXISTS idx_metrics_metric_time ON metrics (metric, time DESC);
CREATE INDEX IF NOT EXISTS idx_metrics_tags        ON metrics USING GIN (tags);

-- Compress data older than 7 days to save disk space
ALTER TABLE metrics SET (
    timescaledb.compress,
    timescaledb.compress_orderby = 'time DESC',
    timescaledb.compress_segmentby = 'metric'
);

SELECT add_compression_policy('metrics', INTERVAL '7 days', if_not_exists => TRUE);


-- ===================== TEST RUNS TABLE =====================
-- One row per completed test run, written by Go summaryHandler
-- Created here so it exists on first boot (Go also creates it but this is a safety net)
CREATE TABLE IF NOT EXISTS test_runs (
    id SERIAL PRIMARY KEY,
    script_name TEXT,
    status TEXT,
    start_time TIMESTAMPTZ,
    end_time TIMESTAMPTZ,

    -- Request metrics
    total_requests INT,
    failed_requests INT,
    request_rate FLOAT,

    -- Latency metrics (milliseconds)
    avg_latency FLOAT,
    p50_latency FLOAT,
    p95_latency FLOAT,
    p99_latency FLOAT,
    min_latency FLOAT,
    max_latency FLOAT,

    -- Timing breakdown
    avg_blocked FLOAT,
    avg_connecting FLOAT,
    avg_tls_connecting FLOAT,
    avg_sending FLOAT,
    avg_waiting FLOAT,
    avg_receiving FLOAT,

    -- Data transfer
    total_data_sent BIGINT,
    total_data_received BIGINT,

    -- Virtual Users
    max_vus INT,

    -- Iterations
    total_iterations INT,
    avg_iter_duration FLOAT,

    -- Checks
    checks_passed INT,
    checks_failed INT,

    -- Errors
    total_errors INT,
    error_rate FLOAT,
    dropped_iterations INT,

    -- Raw summary for debugging
    raw_summary JSONB
);