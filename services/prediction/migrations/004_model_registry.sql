CREATE TABLE IF NOT EXISTS model_versions (
    ticker TEXT NOT NULL,
    timeframe TEXT NOT NULL,
    horizon INTEGER NOT NULL,
    model_version TEXT NOT NULL,
    parent_model_version TEXT,
    strategy_version TEXT,
    model_blob BYTEA NOT NULL,
    record JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (ticker, timeframe, horizon, model_version)
);

CREATE INDEX IF NOT EXISTS model_versions_config_created_idx
    ON model_versions(ticker, timeframe, horizon, created_at DESC);

CREATE TABLE IF NOT EXISTS model_deployments (
    ticker TEXT NOT NULL,
    timeframe TEXT NOT NULL,
    horizon INTEGER NOT NULL,
    active_model_version TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (ticker, timeframe, horizon),
    FOREIGN KEY (ticker, timeframe, horizon, active_model_version)
        REFERENCES model_versions(ticker, timeframe, horizon, model_version)
);

CREATE TABLE IF NOT EXISTS model_promotion_events (
    id BIGSERIAL PRIMARY KEY,
    ticker TEXT NOT NULL,
    timeframe TEXT NOT NULL,
    horizon INTEGER NOT NULL,
    from_model_version TEXT,
    to_model_version TEXT NOT NULL,
    action TEXT NOT NULL CHECK (action IN ('promote', 'rollback', 'legacy-import')),
    actor_uid TEXT NOT NULL,
    reason TEXT NOT NULL,
    evidence JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (ticker, timeframe, horizon, to_model_version)
        REFERENCES model_versions(ticker, timeframe, horizon, model_version)
);

-- Import the pre-registry serving row without changing which model /predict serves. The old table
-- remains as a compatibility mirror during deployment; promotion keeps it pointed at the active
-- version so a process from the previous release cannot accidentally serve a candidate.
INSERT INTO model_versions(
    ticker, timeframe, horizon, model_version, strategy_version, model_blob, record, created_at
)
SELECT
    ticker,
    timeframe,
    horizon,
    COALESCE(
        NULLIF(record->>'modelVersion', ''),
        'legacy-' || substr(md5(ticker || ':' || timeframe || ':' || horizon::text || ':' || updated_at::text), 1, 16)
    ),
    NULLIF(record->>'strategyVersion', ''),
    model_blob,
    record,
    updated_at
FROM models
ON CONFLICT (ticker, timeframe, horizon, model_version) DO NOTHING;

INSERT INTO model_deployments(ticker, timeframe, horizon, active_model_version, updated_at)
SELECT
    ticker,
    timeframe,
    horizon,
    COALESCE(
        NULLIF(record->>'modelVersion', ''),
        'legacy-' || substr(md5(ticker || ':' || timeframe || ':' || horizon::text || ':' || updated_at::text), 1, 16)
    ),
    updated_at
FROM models
ON CONFLICT (ticker, timeframe, horizon) DO NOTHING;

INSERT INTO model_promotion_events(
    ticker, timeframe, horizon, from_model_version, to_model_version,
    action, actor_uid, reason, evidence, created_at
)
SELECT
    d.ticker,
    d.timeframe,
    d.horizon,
    NULL,
    d.active_model_version,
    'legacy-import',
    'migration',
    'Imported the model that was serving before immutable model versioning',
    jsonb_build_object('sourceTable', 'models'),
    d.updated_at
FROM model_deployments d
WHERE NOT EXISTS (
    SELECT 1
    FROM model_promotion_events e
    WHERE e.ticker = d.ticker
      AND e.timeframe = d.timeframe
      AND e.horizon = d.horizon
      AND e.to_model_version = d.active_model_version
      AND e.action = 'legacy-import'
);
