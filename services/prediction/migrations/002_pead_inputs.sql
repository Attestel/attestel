CREATE TABLE IF NOT EXISTS earnings_payloads (
    ticker TEXT NOT NULL,
    provider TEXT NOT NULL,
    payload JSONB NOT NULL,
    payload_sha256 TEXT NOT NULL,
    vintage_status TEXT NOT NULL,
    coverage_start DATE,
    coverage_end DATE,
    event_count INTEGER NOT NULL DEFAULT 0,
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (ticker, payload_sha256)
);

CREATE TABLE IF NOT EXISTS earnings_event_texts (
    ticker TEXT NOT NULL,
    reported_date DATE NOT NULL,
    source TEXT NOT NULL,
    text_body TEXT NOT NULL,
    text_sha256 TEXT NOT NULL,
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (ticker, reported_date, source, text_sha256)
);

CREATE INDEX IF NOT EXISTS earnings_payloads_ticker_updated_idx
    ON earnings_payloads(ticker, updated_at DESC);

CREATE INDEX IF NOT EXISTS earnings_event_texts_lookup_idx
    ON earnings_event_texts(ticker, reported_date, updated_at DESC);
