-- Phase 1 — durable automation lane state and run history.
--
-- WHY THIS LIVES HERE. `services/events` is the only database owner in the platform, and the
-- automation dispatcher must be able to answer "is this lane already running somewhere else?"
-- across PROCESSES and across CONTAINERS. A file lock cannot do that (`services/llm` and
-- `services/events` are separate images), and an in-memory flag cannot survive a restart. A row
-- with a lease token and an expiry can do both, and it is the same shape `alerts` already uses for
-- the re-synthesis queue.
--
-- WHAT THIS IS NOT. There is no interval column that a service reads and turns into a timer. The
-- cadence lives in the environment and is evaluated by an operator-invoked one-shot dispatcher;
-- `next_eligible_at` is a RECORD of when a lane last ran plus its configured minimum interval, so
-- a dispatcher invocation can skip a lane that is not due yet. Nothing in the codebase wakes up on
-- its own to read it. See CLAUDE.md invariant #4 and EVENT_CONTRACTS.md §9.22.

CREATE TABLE automation_lanes (
  lane              TEXT PRIMARY KEY,
  -- Lease. `lease_token` is opaque; only the holder of the exact token may complete the run, so a
  -- worker whose lease expired and was taken by a newer dispatcher cannot overwrite the newer
  -- result. Restart-safe: an abandoned lease simply expires.
  lease_token       TEXT,
  lease_run_id      TEXT,
  lease_expires_at  TIMESTAMPTZ,
  -- Cadence bookkeeping.
  next_eligible_at  TIMESTAMPTZ,
  -- Freshness / last outcome.
  last_started_at   TIMESTAMPTZ,
  last_finished_at  TIMESTAMPTZ,
  last_status       TEXT,
  last_success_at   TIMESTAMPTZ,
  last_failure_at   TIMESTAMPTZ,
  last_error        TEXT,
  consecutive_failures INTEGER NOT NULL DEFAULT 0,
  updated_at        TIMESTAMPTZ NOT NULL,
  CHECK (last_status IS NULL OR last_status IN ('running','success','degraded','failure','skipped'))
);

CREATE TABLE automation_runs (
  sequence         BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
  id               TEXT PRIMARY KEY,
  lane             TEXT NOT NULL,
  trigger          TEXT NOT NULL DEFAULT 'operator',
  started_at       TIMESTAMPTZ NOT NULL,
  completed_at     TIMESTAMPTZ,
  status           TEXT NOT NULL DEFAULT 'running',
  records_read     INTEGER NOT NULL DEFAULT 0,
  records_written  INTEGER NOT NULL DEFAULT 0,
  records_skipped  INTEGER NOT NULL DEFAULT 0,
  queue_depth      INTEGER,
  last_error       TEXT,
  detail           JSONB NOT NULL DEFAULT '{}'::jsonb,
  CHECK (status IN ('running','success','degraded','failure','skipped')),
  CHECK (records_read >= 0 AND records_written >= 0 AND records_skipped >= 0),
  CHECK (jsonb_typeof(detail) = 'object')
);
CREATE INDEX idx_automation_runs_lane ON automation_runs (lane, started_at DESC, sequence DESC);
CREATE INDEX idx_automation_runs_started ON automation_runs (started_at DESC, sequence DESC);
