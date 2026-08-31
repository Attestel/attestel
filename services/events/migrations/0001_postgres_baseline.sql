-- PostgreSQL baseline for the global, non-user-owned event corpus.
--
-- This supersedes the former SQLite migration chain after the D-26 storage decision was amended.
-- PostgreSQL stores instants and structured payloads natively; the public HTTP JSON contract is
-- unchanged by the storage-engine cutover.

CREATE TABLE source_documents (
  id            TEXT PRIMARY KEY,
  content_hash  TEXT NOT NULL UNIQUE,
  provider      TEXT NOT NULL,
  source_tier   TEXT NOT NULL,
  url           TEXT NOT NULL,
  title         TEXT NOT NULL,
  excerpt       TEXT NOT NULL DEFAULT '',
  body          TEXT,
  published_at  TIMESTAMPTZ NOT NULL,
  first_seen_at TIMESTAMPTZ NOT NULL,
  retrieved_at  TIMESTAMPTZ NOT NULL,
  raw_tickers   JSONB NOT NULL DEFAULT '[]'::jsonb,
  ingest_run_id TEXT NOT NULL,
  macro_key     TEXT,
  CHECK (source_tier IN ('official','professional','discussion')),
  CHECK (body IS NULL OR source_tier = 'official'),
  CHECK (jsonb_typeof(raw_tickers) = 'array')
);
CREATE INDEX idx_source_documents_published  ON source_documents (published_at);
CREATE INDEX idx_source_documents_first_seen ON source_documents (first_seen_at);
CREATE INDEX idx_source_documents_provider   ON source_documents (provider, published_at);
CREATE INDEX idx_source_documents_asof       ON source_documents (published_at, first_seen_at);
CREATE INDEX idx_source_documents_macro_key  ON source_documents (macro_key);
CREATE INDEX idx_source_documents_raw_tickers ON source_documents USING GIN (raw_tickers);

CREATE TABLE ingest_runs (
  sequence    BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
  id          TEXT PRIMARY KEY,
  started_at  TIMESTAMPTZ NOT NULL,
  finished_at TIMESTAMPTZ,
  providers   JSONB NOT NULL DEFAULT '[]'::jsonb,
  tickers     JSONB NOT NULL DEFAULT '[]'::jsonb,
  fetched     INTEGER NOT NULL DEFAULT 0,
  inserted    INTEGER NOT NULL DEFAULT 0,
  deduped     INTEGER NOT NULL DEFAULT 0,
  degraded    JSONB NOT NULL DEFAULT '[]'::jsonb,
  error       TEXT,
  CHECK (jsonb_typeof(providers) = 'array'),
  CHECK (jsonb_typeof(tickers) = 'array'),
  CHECK (jsonb_typeof(degraded) = 'array')
);
CREATE INDEX idx_ingest_runs_started ON ingest_runs (started_at DESC, sequence DESC);

CREATE TABLE events (
  id                 TEXT PRIMARY KEY,
  event_type         TEXT NOT NULL,
  title              TEXT NOT NULL,
  summary            TEXT NOT NULL DEFAULT '',
  why_it_matters     TEXT,
  key_facts          JSONB NOT NULL DEFAULT '[]'::jsonb,
  occurred_at        TIMESTAMPTZ NOT NULL,
  published_at       TIMESTAMPTZ NOT NULL,
  first_seen_at      TIMESTAMPTZ NOT NULL,
  source_tier        TEXT NOT NULL,
  official_confirmed SMALLINT NOT NULL DEFAULT 0,
  importance         REAL NOT NULL DEFAULT 0,
  novelty            REAL NOT NULL DEFAULT 0,
  document_count     INTEGER NOT NULL DEFAULT 0,
  cluster_key        TEXT NOT NULL,
  enrichment_state   TEXT NOT NULL DEFAULT 'raw',
  model_used         TEXT,
  prompt_version     TEXT,
  schema_version     TEXT,
  enriched_at        TIMESTAMPTZ,
  reasoning_mode     TEXT,
  enrichment_as_of   TIMESTAMPTZ,
  CHECK (event_type IN ('earnings_result','earnings_guidance','analyst_revision','product_launch',
                        'management_change','ma_transaction','regulatory_action','legal_action',
                        'supply_chain','capital_return','financing','partnership','macro_release',
                        'central_bank','sector_event','other')),
  CHECK (source_tier IN ('official','professional','discussion')),
  CHECK (enrichment_state IN ('raw','enriched','failed')),
  CHECK (official_confirmed IN (0,1)),
  CHECK (importance >= 0 AND importance <= 1),
  CHECK (novelty >= 0 AND novelty <= 1),
  CHECK (jsonb_typeof(key_facts) = 'array')
);
CREATE INDEX idx_events_asof        ON events (published_at, first_seen_at);
CREATE INDEX idx_events_occurred    ON events (occurred_at);
CREATE INDEX idx_events_type        ON events (event_type, published_at);
CREATE INDEX idx_events_importance  ON events (importance);
CREATE INDEX idx_events_cluster_key ON events (cluster_key);

CREATE TABLE event_state_history (
  event_id           TEXT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
  as_of              TIMESTAMPTZ NOT NULL,
  importance         REAL NOT NULL,
  novelty            REAL NOT NULL,
  document_count     INTEGER NOT NULL,
  source_tier        TEXT NOT NULL,
  official_confirmed SMALLINT NOT NULL,
  PRIMARY KEY (event_id, as_of),
  CHECK (source_tier IN ('official','professional','discussion')),
  CHECK (official_confirmed IN (0,1))
);

CREATE TABLE event_documents (
  event_id     TEXT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
  document_id  TEXT NOT NULL,
  url          TEXT NOT NULL,
  provider     TEXT NOT NULL,
  source_tier  TEXT NOT NULL,
  published_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (event_id, document_id),
  CHECK (source_tier IN ('official','professional','discussion'))
);
CREATE INDEX idx_event_documents_document ON event_documents (document_id);

CREATE TABLE event_tickers (
  event_id            TEXT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
  ticker              TEXT NOT NULL,
  relevance           REAL NOT NULL DEFAULT 0,
  is_primary          SMALLINT NOT NULL DEFAULT 0,
  potential_direction TEXT,
  potential_strength  REAL,
  expected_horizon    TEXT,
  affected_dimensions JSONB NOT NULL DEFAULT '[]'::jsonb,
  transmission        TEXT,
  PRIMARY KEY (event_id, ticker),
  CHECK (is_primary IN (0,1)),
  CHECK (relevance >= 0 AND relevance <= 1),
  CHECK (potential_direction IS NULL OR
         potential_direction IN ('positive','negative','neutral','unclear')),
  CHECK (potential_strength IS NULL OR (potential_strength >= 0 AND potential_strength <= 1)),
  CHECK (expected_horizon IS NULL OR
         expected_horizon IN ('intraday','short_term','medium_term','long_term')),
  CHECK (jsonb_typeof(affected_dimensions) = 'array')
);
CREATE INDEX idx_event_tickers_ticker ON event_tickers (ticker, event_id);

CREATE TABLE macro_events (
  id                      TEXT PRIMARY KEY,
  event_id                TEXT REFERENCES events(id) ON DELETE SET NULL,
  series                  TEXT NOT NULL,
  release_at              TIMESTAMPTZ NOT NULL,
  actual                  REAL,
  expected                REAL,
  previous                REAL,
  surprise                REAL,
  unit                    TEXT NOT NULL DEFAULT '',
  expectation_source      TEXT,
  expectation_captured_at TIMESTAMPTZ,
  UNIQUE (series, release_at),
  CHECK (expectation_source IS NULL OR expectation_source IN ('fmp','none')),
  CHECK (surprise IS NULL OR (actual IS NOT NULL AND expected IS NOT NULL
         AND expectation_captured_at IS NOT NULL AND expectation_captured_at < release_at))
);
CREATE INDEX idx_macro_events_series  ON macro_events (series, release_at);
CREATE INDEX idx_macro_events_release ON macro_events (release_at);

CREATE TABLE provider_budget (
  provider      TEXT NOT NULL,
  date          DATE NOT NULL,
  calls         INTEGER NOT NULL DEFAULT 0,
  "limit"       INTEGER NOT NULL DEFAULT 0,
  blocked_until TIMESTAMPTZ,
  last_error    TEXT,
  PRIMARY KEY (provider, date),
  CHECK (calls >= 0),
  CHECK ("limit" >= 0)
);

CREATE TABLE scheduled_events (
  id             TEXT PRIMARY KEY,
  occurrence_key TEXT NOT NULL UNIQUE,
  kind           TEXT NOT NULL,
  ticker         TEXT,
  series         TEXT,
  scheduled_at   TIMESTAMPTZ NOT NULL,
  confirmed      SMALLINT NOT NULL DEFAULT 0,
  source         TEXT NOT NULL,
  first_seen_at  TIMESTAMPTZ NOT NULL,
  title          TEXT NOT NULL DEFAULT '',
  description    TEXT NOT NULL DEFAULT '',
  importance     TEXT NOT NULL DEFAULT 'medium',
  status         TEXT NOT NULL DEFAULT 'scheduled',
  source_tier    TEXT NOT NULL DEFAULT 'professional',
  source_url     TEXT,
  timezone       TEXT NOT NULL DEFAULT 'UTC',
  local_time     TEXT NOT NULL DEFAULT '',
  previous       REAL,
  expected       REAL,
  actual         REAL,
  surprise       REAL,
  unit           TEXT NOT NULL DEFAULT '',
  updated_at     TIMESTAMPTZ,
  CHECK (confirmed IN (0,1)),
  CHECK (source_tier IN ('official','professional','discussion'))
);
CREATE INDEX idx_scheduled_events_window ON scheduled_events (scheduled_at, kind);
CREATE INDEX idx_scheduled_events_ticker ON scheduled_events (ticker, scheduled_at);

CREATE TABLE scheduled_event_history (
  id                 TEXT PRIMARY KEY,
  event_id           TEXT NOT NULL REFERENCES scheduled_events(id) ON DELETE CASCADE,
  observed_at        TIMESTAMPTZ NOT NULL,
  change_type        TEXT NOT NULL,
  prior_scheduled_at TIMESTAMPTZ,
  scheduled_at       TIMESTAMPTZ NOT NULL,
  prior_status       TEXT,
  status             TEXT NOT NULL,
  confirmed          SMALLINT NOT NULL DEFAULT 0,
  source             TEXT NOT NULL,
  source_tier        TEXT NOT NULL,
  source_url         TEXT,
  title              TEXT NOT NULL DEFAULT '',
  description        TEXT NOT NULL DEFAULT '',
  importance         TEXT NOT NULL DEFAULT 'medium',
  timezone           TEXT NOT NULL DEFAULT 'UTC',
  local_time         TEXT NOT NULL DEFAULT '',
  previous           REAL,
  expected           REAL,
  actual             REAL,
  surprise           REAL,
  unit               TEXT NOT NULL DEFAULT '',
  CHECK (change_type IN (
    'created','rescheduled','cancelled','status_changed','source_upgraded','released','updated'
  )),
  CHECK (confirmed IN (0,1)),
  CHECK (source_tier IN ('official','professional','discussion'))
);
CREATE INDEX idx_scheduled_event_history_event
  ON scheduled_event_history (event_id, observed_at, id);

CREATE TABLE predictions (
  id                   TEXT PRIMARY KEY,
  ticker               TEXT NOT NULL,
  as_of                TIMESTAMPTZ NOT NULL,
  created_at           TIMESTAMPTZ NOT NULL,
  experiment           TEXT NOT NULL,
  split                TEXT NOT NULL,
  model_used           TEXT NOT NULL,
  quantization         TEXT,
  reasoning_mode       TEXT NOT NULL,
  prompt_version       TEXT NOT NULL,
  schema_version       TEXT NOT NULL,
  tool_schema_version  TEXT NOT NULL,
  generation_settings  JSONB NOT NULL DEFAULT '{}'::jsonb,
  bars_ref             JSONB NOT NULL DEFAULT '{}'::jsonb,
  technical_state_hash TEXT NOT NULL,
  earnings_snapshot_id TEXT,
  tool_calls           JSONB NOT NULL DEFAULT '[]'::jsonb,
  forecast             JSONB NOT NULL,
  confidence_bucket    TEXT NOT NULL,
  thesis               TEXT NOT NULL DEFAULT '',
  invalidation         TEXT NOT NULL DEFAULT '',
  CHECK (experiment IN ('A','B','C','D','E','F')),
  CHECK (split IN ('dev','validation','test')),
  CHECK (reasoning_mode IN ('thinking','non_thinking')),
  CHECK (confidence_bucket IN ('low','medium','high')),
  CHECK (jsonb_typeof(generation_settings) = 'object'),
  CHECK (jsonb_typeof(bars_ref) = 'object'),
  CHECK (jsonb_typeof(tool_calls) = 'array'),
  CHECK (jsonb_typeof(forecast) = 'object')
);
CREATE INDEX idx_predictions_ticker_asof ON predictions (ticker, as_of);
CREATE INDEX idx_predictions_experiment  ON predictions (experiment, split, as_of);
CREATE INDEX idx_predictions_as_of       ON predictions (as_of);

CREATE TABLE prediction_evidence (
  prediction_id TEXT NOT NULL REFERENCES predictions(id) ON DELETE CASCADE,
  kind          TEXT NOT NULL,
  ref_id        TEXT NOT NULL,
  PRIMARY KEY (prediction_id, kind, ref_id),
  CHECK (kind IN ('event','macro_event'))
);
CREATE INDEX idx_prediction_evidence_ref ON prediction_evidence (kind, ref_id);

CREATE TABLE outcomes (
  prediction_id         TEXT NOT NULL REFERENCES predictions(id) ON DELETE CASCADE,
  horizon               TEXT NOT NULL,
  realized_return       REAL,
  benchmark_return      REAL,
  sector_return         REAL,
  excess_return         REAL,
  realized_vol          REAL,
  regime_label          TEXT,
  resolved_at           TIMESTAMPTZ,
  -- Analysis bar keys may be date-only (daily) or full instants (intraday); preserve that source
  -- identity verbatim instead of coercing a daily key to an invented midnight instant.
  resolved_from_bars_ts TEXT,
  resolved_from_source  TEXT,
  PRIMARY KEY (prediction_id, horizon),
  CHECK (horizon IN ('1d','5d','20d','60d')),
  CHECK (regime_label IS NULL OR regime_label IN ('bull','bear','sideways','high_vol'))
);
