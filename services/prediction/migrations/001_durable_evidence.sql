CREATE TABLE IF NOT EXISTS models (
    ticker TEXT NOT NULL,
    timeframe TEXT NOT NULL,
    horizon INTEGER NOT NULL,
    model_blob BYTEA NOT NULL,
    record JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (ticker, timeframe, horizon)
);

CREATE TABLE IF NOT EXISTS verdicts (
    ticker TEXT NOT NULL,
    timeframe TEXT NOT NULL,
    horizon INTEGER NOT NULL,
    record JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (ticker, timeframe, horizon)
);

CREATE TABLE IF NOT EXISTS artifacts (
    name TEXT PRIMARY KEY,
    media_type TEXT NOT NULL,
    payload BYTEA NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
