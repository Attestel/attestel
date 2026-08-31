-- Prospective paired shadow evidence for autonomous trials.
-- Each row is a decision at one completed real bar; it is settled exactly once by the next bar.

ALTER TABLE automation_trials
    DROP CONSTRAINT IF EXISTS automation_trials_status_check;

ALTER TABLE automation_trials
    ADD CONSTRAINT automation_trials_status_check CHECK (status IN (
        'reserved', 'trained', 'evaluating', 'evaluated',
        'training-failed', 'evaluation-failed',
        'shadowing', 'shadow-complete', 'shadow-failed'
    ));

CREATE TABLE IF NOT EXISTS automation_shadow_observations (
    trial_id BIGINT NOT NULL REFERENCES automation_trials(id),
    bar_time TEXT NOT NULL,
    close DOUBLE PRECISION NOT NULL CHECK (close > 0),
    source TEXT NOT NULL,
    candidate_target SMALLINT NOT NULL CHECK (candidate_target IN (-1, 0, 1)),
    champion_target SMALLINT NOT NULL CHECK (champion_target IN (-1, 0, 1)),
    candidate_probability DOUBLE PRECISION NOT NULL CHECK (
        candidate_probability >= 0 AND candidate_probability <= 1
    ),
    champion_probability DOUBLE PRECISION NOT NULL CHECK (
        champion_probability >= 0 AND champion_probability <= 1
    ),
    candidate_turnover DOUBLE PRECISION NOT NULL CHECK (candidate_turnover >= 0),
    champion_turnover DOUBLE PRECISION NOT NULL CHECK (champion_turnover >= 0),
    candidate_cost_bps DOUBLE PRECISION NOT NULL CHECK (candidate_cost_bps >= 0),
    champion_cost_bps DOUBLE PRECISION NOT NULL CHECK (champion_cost_bps >= 0),
    next_bar_time TEXT,
    next_close DOUBLE PRECISION CHECK (next_close > 0),
    candidate_net_return DOUBLE PRECISION,
    champion_net_return DOUBLE PRECISION,
    settled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (trial_id, bar_time)
);

CREATE INDEX IF NOT EXISTS automation_shadow_trial_bar_idx
    ON automation_shadow_observations(trial_id, bar_time DESC);
