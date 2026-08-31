-- Discovery Scout — durable, versioned company-level research leads.
--
-- Scout ranks evidence already held by the platform. It is not an order queue, a prediction
-- table, or a model artefact: no direction, target, expected return, or recommendation column
-- exists. `components` and `evidence` make every rank auditable and recomputable under the stored
-- score version.

CREATE TABLE scout_runs (
  sequence          BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
  id                TEXT PRIMARY KEY,
  score_version     TEXT NOT NULL,
  universe_version  TEXT NOT NULL,
  universe          JSONB NOT NULL,
  as_of             TIMESTAMPTZ NOT NULL,
  started_at        TIMESTAMPTZ NOT NULL,
  completed_at      TIMESTAMPTZ,
  status            TEXT NOT NULL DEFAULT 'running',
  candidate_count   INTEGER NOT NULL DEFAULT 0,
  coverage          JSONB NOT NULL DEFAULT '{}'::jsonb,
  degraded          JSONB NOT NULL DEFAULT '[]'::jsonb,
  error             TEXT,
  CHECK (status IN ('running','success','failure')),
  CHECK (candidate_count >= 0),
  CHECK (jsonb_typeof(universe) = 'array'),
  CHECK (jsonb_typeof(coverage) = 'object'),
  CHECK (jsonb_typeof(degraded) = 'array')
);
CREATE INDEX idx_scout_runs_as_of ON scout_runs (as_of DESC, sequence DESC);
CREATE INDEX idx_scout_runs_status ON scout_runs (status, as_of DESC, sequence DESC);

CREATE TABLE scout_candidates (
  run_id              TEXT NOT NULL REFERENCES scout_runs(id) ON DELETE CASCADE,
  ticker              TEXT NOT NULL,
  rank                INTEGER NOT NULL,
  attention_score     DOUBLE PRECISION NOT NULL,
  attention_band      TEXT NOT NULL,
  components          JSONB NOT NULL,
  why_now             TEXT NOT NULL,
  evidence            JSONB NOT NULL,
  related_tickers     JSONB NOT NULL DEFAULT '[]'::jsonb,
  latest_evidence_at  TIMESTAMPTZ NOT NULL,
  source_tiers        JSONB NOT NULL DEFAULT '[]'::jsonb,
  data_state          TEXT NOT NULL DEFAULT 'live',
  PRIMARY KEY (run_id, ticker),
  UNIQUE (run_id, rank),
  CHECK (rank > 0),
  CHECK (attention_score >= 0 AND attention_score <= 1),
  CHECK (attention_band IN ('high_attention','monitor','emerging')),
  CHECK (data_state IN ('live','insufficient')),
  CHECK (jsonb_typeof(components) = 'object'),
  CHECK (jsonb_typeof(evidence) = 'array'),
  CHECK (jsonb_typeof(related_tickers) = 'array'),
  CHECK (jsonb_typeof(source_tiers) = 'array')
);
CREATE INDEX idx_scout_candidates_rank ON scout_candidates (run_id, rank);
CREATE INDEX idx_scout_candidates_ticker ON scout_candidates (ticker, run_id);
