-- Phase 4 — what the market did after a stored event, recorded once and never recomputed.
--
-- TWO TABLES, BECAUSE THERE ARE TWO KINDS OF FACT.
--
-- `event_reactions` holds what is true of the (event, ticker) pair regardless of horizon: when the
-- event was, which market session it landed in, and the reference close every window is measured
-- from. `event_reaction_windows` holds one row per horizon, because a 1-day window can be resolved
-- while a 20-day window is still maturing — and the difference between "resolved to nothing" and
-- "not resolved yet" is the whole point of the `state` column.
--
-- WHY `state` IS NOT NULLABILITY. A NULL `raw_return` could mean the window has not matured, or
-- that the bars are missing, or that the source was synthetic and disqualified. Those are three
-- different facts and an aggregate that cannot tell them apart is an aggregate over an unknown
-- denominator. `state` + `missing_reason` say which, always.
--
-- WHY THE BARS ARE NAMED. `reference_ts`, `end_ts` and `bars_used` record exactly which stored bars
-- produced each number, so a result can be re-derived years later — and so a look-ahead bug is
-- VISIBLE rather than merely absent from a test.

CREATE TABLE event_reactions (
  event_id           TEXT NOT NULL REFERENCES scheduled_events(id) ON DELETE CASCADE,
  ticker             TEXT NOT NULL,
  event_at           TIMESTAMPTZ NOT NULL,
  -- before_market | regular | after_market | non_trading_day. Deterministic from the event's own
  -- timestamp in US/Eastern; it decides which close the windows are measured from.
  session            TEXT NOT NULL,
  -- The last stored close STRICTLY BEFORE the event could have moved the price.
  reference_ts       TEXT,
  reference_close    DOUBLE PRECISION,
  reference_source   TEXT NOT NULL DEFAULT '',
  -- A window whose bars came from synthetic data is recorded and EXCLUDED from every aggregate.
  -- Recorded rather than dropped so "why is the sample smaller than the event count" is answerable.
  synthetic          SMALLINT NOT NULL DEFAULT 0,
  -- Pre-event baselines the change ratios are measured against.
  pre_volume_avg     DOUBLE PRECISION,
  pre_range_avg      DOUBLE PRECISION,
  benchmark_ticker   TEXT NOT NULL DEFAULT '',
  calc_version       TEXT NOT NULL,
  captured_at        TIMESTAMPTZ NOT NULL,
  updated_at         TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (event_id, ticker),
  CHECK (session IN ('before_market','regular','after_market','non_trading_day')),
  CHECK (synthetic IN (0,1))
);
CREATE INDEX idx_event_reactions_ticker ON event_reactions (ticker, event_at DESC);

CREATE TABLE event_reaction_windows (
  event_id         TEXT NOT NULL,
  ticker           TEXT NOT NULL,
  horizon          TEXT NOT NULL,
  state            TEXT NOT NULL DEFAULT 'pending',
  missing_reason   TEXT,
  end_ts           TEXT,
  end_close        DOUBLE PRECISION,
  raw_return       DOUBLE PRECISION,
  -- NULL when the benchmark's bars are unavailable. NEVER 0.0: a missing benchmark and a benchmark
  -- that went nowhere are different facts, and an excess return computed against a zero we invented
  -- is a fabricated number wearing a real one's clothes.
  benchmark_return DOUBLE PRECISION,
  excess_return    DOUBLE PRECISION,
  volume_change    DOUBLE PRECISION,
  range_change     DOUBLE PRECISION,
  bars_used        INTEGER,
  bar_source       TEXT NOT NULL DEFAULT '',
  synthetic        SMALLINT NOT NULL DEFAULT 0,
  resolved_at      TIMESTAMPTZ,
  updated_at       TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (event_id, ticker, horizon),
  FOREIGN KEY (event_id, ticker) REFERENCES event_reactions(event_id, ticker) ON DELETE CASCADE,
  CHECK (horizon IN ('1d','5d','20d')),
  CHECK (state IN ('pending','resolved','unavailable')),
  CHECK (synthetic IN (0,1))
);
CREATE INDEX idx_event_reaction_windows_state ON event_reaction_windows (state, horizon);
