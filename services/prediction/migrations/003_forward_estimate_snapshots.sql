CREATE TABLE IF NOT EXISTS earnings_estimate_snapshots (
    ticker TEXT NOT NULL,
    fiscal_date DATE NOT NULL,
    expected_report_date DATE NOT NULL,
    stage TEXT NOT NULL CHECK (stage IN ('t_minus_7', 't_minus_1')),
    provider TEXT NOT NULL,
    payload JSONB NOT NULL,
    payload_sha256 TEXT NOT NULL,
    consensus_eps DOUBLE PRECISION NOT NULL,
    estimate_high DOUBLE PRECISION,
    estimate_low DOUBLE PRECISION,
    analyst_count INTEGER,
    captured_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (ticker, fiscal_date, stage, payload_sha256),
    CHECK (captured_at < (expected_report_date::timestamp AT TIME ZONE 'UTC'))
);

CREATE INDEX IF NOT EXISTS earnings_estimate_snapshots_event_idx
    ON earnings_estimate_snapshots(ticker, fiscal_date, captured_at DESC);

CREATE INDEX IF NOT EXISTS earnings_estimate_snapshots_report_idx
    ON earnings_estimate_snapshots(expected_report_date, ticker);
