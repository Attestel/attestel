# Paper-test dashboard contract

Version: `paper-dashboard-v1`

The paper-test dashboard is the monitoring surface for the platform validation engine's current
experiment generation. It is a simulation: it has no broker, places no order, moves no money and
does not decide whether real money is safe to deploy.

## Source of truth

`GET /paper/dashboard?days=252` composes one bounded response under one `asOf` timestamp from:

- `paper.documents`: enabled configs, bar cursors, current positions and last decisions;
- `paper.fills`: append-only simulated book fills for the current generation;
- `paper.snapshots`: real complete-book marks and explicit gap records;
- `paper.decision_events`: settled completed-bar decisions for the current generation;
- `journal.trades`, `mode=paper`: position episodes used by the existing counting statistics;
- the prediction service's active record and evaluator evidence used by `/paper/comparison`.

The handler reads the experiment generation and configured scope before and after composition. If a
reset or scope change crosses the read, it retries once and then returns `409`; it never serves
equity from one experiment shape beside decisions from another.

`GET /paper/experiments` lists the active generation and PostgreSQL reset archives with their
snapshot, gap, fill and decision counts. It is an index only; the live dashboard remains scoped to
the current generation, so archived evidence cannot be mistaken for current evidence.

## Decision history

A decision event is written only when a completed bar is settled and the bar cursor advances. Its
key is `(generation, config, bar_unix)`. PostgreSQL writes the event and updated engine-state
document in the same transaction.

Transient data, quote, journal and synchronization failures do not advance the bar. They remain
visible in `/paper/status`, but repeated polling attempts are not appended to experiment history and
cannot inflate gate-refusal counts.

Every event retains the model and strategy versions used for that decision when a prediction record
was available.

## Status dimensions

The dashboard reports four independent factual dimensions rather than one overloaded verdict:

| Dimension | Values | Meaning |
|---|---|---|
| `clock` | `not-started`, `running` | Whether durable official day 0 exists |
| `integrity` | `healthy`, `degraded` | Whether stores, marks and recording are complete and consistent |
| `sample` | `empty`, `collecting`, `measurable` | Progress toward the existing 20-snapshot Sharpe floor |
| `result` | `unjudged`, `tracking`, `divergence` | Like-for-like evaluator comparison state, when available |

`tracking` requires the evaluator-portfolio reference and a measurable live daily Sharpe.
`model-backtest` fallback statistics remain visible with their caveat but cannot produce the
`tracking` label. Existing sign-safe divergence logic remains authoritative.

None of these values means "ready for real money".

## Series and coverage

The dated series includes equity, cash, realized/unrealized P&L, exposure, daily return and
drawdown. Returns and drawdown are calculated over the full current generation before the requested
display tail is applied, so changing `days` cannot change a point's value.

A gap row carries a date and reason with every numeric measurement set to `null`. A missing mark is
never zero and never a carried-forward price.

Coverage is:

```text
measured snapshot dates / (measured snapshot dates + explicit gap dates)
```

It describes attempted complete-book marks, not an exchange-calendar estimate.

## UI ownership

The dashboard lives at Journal → Experiments and continues to label the current book as the shared
platform validation engine's book until D-20's per-user target is implemented. Champion promotion
and rollback remain in Settings. The dashboard observes the active champion and never trains,
promotes, tunes or changes a threshold.
