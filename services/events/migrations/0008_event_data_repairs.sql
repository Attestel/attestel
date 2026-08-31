-- One-time, versioned deterministic repairs for event data whose source rows already exist.
--
-- Schema migrations cannot safely encode provider-specific headline/entity rules: the canonical
-- alias table and word-boundary logic live in app.entities. The Python repair runner records its
-- version and result here transactionally, so every deployment applies a repair at most once and
-- concurrent replicas cannot race it.

CREATE TABLE event_data_repairs (
  version     TEXT PRIMARY KEY,
  applied_at  TIMESTAMPTZ NOT NULL,
  details     JSONB NOT NULL DEFAULT '{}'::jsonb,
  CHECK (jsonb_typeof(details) = 'object')
);
