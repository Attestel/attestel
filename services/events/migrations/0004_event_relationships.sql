-- Phase 3 — which companies a scheduled event bears on, and why.
--
-- One event affects several tickers, and the WAY it affects each one is different: NVIDIA's own
-- earnings is `direct` for NVDA, `supplier` for its memory suppliers, `competitor` for AMD and
-- `sector` for the rest of the semis complex. A CPI print is `macro` for everything. Those are not
-- the same claim and must not be flattened into one "related" flag.
--
-- THE VOCABULARY IS CLOSED, IN SQL. A relationship type outside the seven is rejected by the
-- database, not by a code path someone can forget to call — this column is what a portfolio
-- exposure calculation groups by, and a free-text type would let a typo silently create an eighth
-- category that no downstream code handles.
--
-- EVERY ROW CARRIES ITS OWN PROVENANCE. `reason` says why in plain language and `source` says where
-- that came from: `derived` (computed from the event's own fields), `reference` (a stored,
-- configured reference relationship), or `evidence` (an official document that states it). There is
-- no fourth value and specifically no `model` — nothing here may be generated.

CREATE TABLE event_relationships (
  event_id        TEXT NOT NULL REFERENCES scheduled_events(id) ON DELETE CASCADE,
  ticker          TEXT NOT NULL,
  relationship    TEXT NOT NULL,
  reason          TEXT NOT NULL DEFAULT '',
  -- Where the relationship itself came from. NOT where the event came from.
  source          TEXT NOT NULL DEFAULT 'derived',
  source_ref      TEXT NOT NULL DEFAULT '',
  -- Write-once. A point-in-time read asks "what did we know at cutoff T", and a restamped
  -- first_seen_at would answer with today's knowledge — the same rule scheduled_events already
  -- lives by.
  first_seen_at   TIMESTAMPTZ NOT NULL,
  -- The as-of boundary this relationship became effective from. Defaults to first_seen_at; a
  -- reference relationship may declare an earlier one when the configuration states when it began.
  effective_from  TIMESTAMPTZ NOT NULL,
  -- Coarse, qualitative, and deliberately NOT a number. A float here would be read as a
  -- probability and it is not one.
  relevance_band  TEXT NOT NULL DEFAULT 'secondary',
  calc_version    TEXT NOT NULL,
  updated_at      TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (event_id, ticker, relationship),
  CHECK (relationship IN (
    'direct','sector','supplier','customer','competitor','macro','factor'
  )),
  CHECK (source IN ('derived','reference','evidence')),
  CHECK (relevance_band IN ('primary','secondary','contextual'))
);
CREATE INDEX idx_event_relationships_ticker ON event_relationships (ticker, event_id);
CREATE INDEX idx_event_relationships_asof   ON event_relationships (first_seen_at);
