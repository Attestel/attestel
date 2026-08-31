-- Durable, bounded candidate-training/evaluation trials.
--
-- The controller may create immutable challengers and run the existing price evaluator. It may
-- never move model_deployments: promotion remains an authenticated operator action.

CREATE TABLE IF NOT EXISTS automation_controller (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    lease_token TEXT,
    lease_expires_at TIMESTAMPTZ,
    last_poll_at TIMESTAMPTZ,
    last_error TEXT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO automation_controller(singleton)
VALUES (TRUE)
ON CONFLICT (singleton) DO NOTHING;

CREATE TABLE IF NOT EXISTS automation_trials (
    id BIGSERIAL PRIMARY KEY,
    ticker TEXT NOT NULL,
    timeframe TEXT NOT NULL,
    horizon INTEGER NOT NULL,
    champion_model_version TEXT,
    candidate_model_version TEXT,
    strategy_version TEXT,
    trigger_bar TEXT NOT NULL,
    data_through TEXT,
    status TEXT NOT NULL CHECK (status IN (
        'reserved', 'trained', 'evaluating', 'evaluated',
        'training-failed', 'evaluation-failed'
    )),
    evaluation_started_at TIMESTAMPTZ,
    evaluation_finished_at TIMESTAMPTZ,
    evaluation JSONB,
    error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (ticker, timeframe, horizon, trigger_bar),
    FOREIGN KEY (ticker, timeframe, horizon, candidate_model_version)
        REFERENCES model_versions(ticker, timeframe, horizon, model_version)
);

CREATE INDEX IF NOT EXISTS automation_trials_config_created_idx
    ON automation_trials(ticker, timeframe, horizon, created_at DESC);

CREATE INDEX IF NOT EXISTS automation_trials_status_idx
    ON automation_trials(status, created_at);
