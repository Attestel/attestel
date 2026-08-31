-- Phase 2D — the filing that stated a scheduled event.
--
-- A Calendar entry derived from an 8-K must remain traceable to the filing it came from, and to the
-- exact sentence in it. Without this table the derived date would be indistinguishable from a date
-- an aggregator supplied — which is precisely the confusion the source-precedence order exists to
-- prevent, and it would make the entry unauditable.
--
-- The relationship is many-to-many on purpose: one filing can announce several events, and one
-- event can be stated by more than one filing (an announcement and a later confirmation).

CREATE TABLE scheduled_event_documents (
  event_id     TEXT NOT NULL REFERENCES scheduled_events(id) ON DELETE CASCADE,
  -- NOT a foreign key to source_documents. D-29's retention sweep DELETES professional and
  -- discussion documents on a schedule, and an official document may be removed by a future policy
  -- change; a cascade would then silently erase the Calendar entry's provenance. The id and the URL
  -- are kept verbatim so the link survives the evidence, degrading to "this came from filing X at
  -- this URL" rather than to nothing.
  document_id  TEXT NOT NULL,
  document_url TEXT NOT NULL DEFAULT '',
  provider     TEXT NOT NULL DEFAULT '',
  -- The sentence the date was read out of, capped. This is what makes a derived date reviewable by
  -- a human without re-fetching the filing.
  evidence     TEXT NOT NULL DEFAULT '',
  linked_at    TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (event_id, document_id)
);
CREATE INDEX idx_scheduled_event_documents_doc ON scheduled_event_documents (document_id);
