-- Early Opportunity Radar — immutable, completed-bar research setup snapshots.
--
-- A candidate is deliberately NOT a prediction, recommendation, order instruction, or paper
-- position. `setup_score` orders deterministic evidence under a named detector version; it is not
-- a probability. `state` makes late moves explicit (`extended`) and records when a previously
-- visible setup loses its evidence (`invalidated`).

CREATE TABLE opportunity_runs (
  sequence            BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
  id                  TEXT PRIMARY KEY,
  detector_version    TEXT NOT NULL,
  universe_version    TEXT NOT NULL,
  universe            JSONB NOT NULL,
  benchmark            TEXT NOT NULL,
  data_fingerprint    TEXT NOT NULL,
  as_of               TIMESTAMPTZ NOT NULL,
  started_at          TIMESTAMPTZ NOT NULL,
  completed_at        TIMESTAMPTZ,
  status              TEXT NOT NULL DEFAULT 'running',
  candidate_count     INTEGER NOT NULL DEFAULT 0,
  coverage            JSONB NOT NULL DEFAULT '{}'::jsonb,
  degraded            JSONB NOT NULL DEFAULT '[]'::jsonb,
  error               TEXT,
  UNIQUE (detector_version, universe_version, data_fingerprint),
  CHECK (status IN ('running','success','failure')),
  CHECK (candidate_count >= 0),
  CHECK (jsonb_typeof(universe) = 'array'),
  CHECK (jsonb_typeof(coverage) = 'object'),
  CHECK (jsonb_typeof(degraded) = 'array')
);
CREATE INDEX idx_opportunity_runs_as_of
  ON opportunity_runs (as_of DESC, sequence DESC);
CREATE INDEX idx_opportunity_runs_status
  ON opportunity_runs (status, as_of DESC, sequence DESC);

CREATE TABLE opportunity_candidates (
  run_id               TEXT NOT NULL REFERENCES opportunity_runs(id) ON DELETE CASCADE,
  ticker               TEXT NOT NULL,
  rank                 INTEGER NOT NULL,
  state                TEXT NOT NULL,
  previous_state       TEXT,
  setup_score          DOUBLE PRECISION NOT NULL,
  bar_time             DATE NOT NULL,
  first_seen_at        TIMESTAMPTZ NOT NULL,
  state_changed_at     TIMESTAMPTZ NOT NULL,
  source               TEXT NOT NULL,
  facts                JSONB NOT NULL,
  components           JSONB NOT NULL,
  evidence_context     JSONB NOT NULL DEFAULT '{}'::jsonb,
  reason               TEXT NOT NULL,
  risk_flags           JSONB NOT NULL DEFAULT '[]'::jsonb,
  PRIMARY KEY (run_id, ticker),
  UNIQUE (run_id, rank),
  CHECK (rank > 0),
  CHECK (state IN ('emerging','confirmed','extended','invalidated')),
  CHECK (previous_state IS NULL OR previous_state IN
    ('emerging','confirmed','extended','invalidated')),
  CHECK (setup_score >= 0 AND setup_score <= 1),
  CHECK (jsonb_typeof(facts) = 'object'),
  CHECK (jsonb_typeof(components) = 'object'),
  CHECK (jsonb_typeof(evidence_context) = 'object'),
  CHECK (jsonb_typeof(risk_flags) = 'array')
);
CREATE INDEX idx_opportunity_candidates_rank
  ON opportunity_candidates (run_id, rank);
CREATE INDEX idx_opportunity_candidates_ticker
  ON opportunity_candidates (ticker, bar_time DESC, run_id);
