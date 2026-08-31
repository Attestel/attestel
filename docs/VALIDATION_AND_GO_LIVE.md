# Validation & go-live checklist

**Scope (clarified 2026-07-31 by Step 12).** This document is about **one thing**: whether the
`prediction` service's directional signal is trustworthy enough for the *user's own* money. It is
not the product's beta gate.

| Question | Document |
|---|---|
| Is the *directional signal* validated enough to act on? | **this file** |
| Is the *research product* coherent and safe enough for a closed beta? | [`research-os/BETA_READINESS_REPORT.md`](research-os/BETA_READINESS_REPORT.md) |

Both must be satisfied independently, and neither implies the other: a passing beta gate says nothing
about the signal's edge, and a passing backtest says nothing about whether the research workflow is
safe to put in front of testers. Stage 0 below predates the seven-destination IA — read "dashboard"
as Today plus Research → Snapshot.

The build is essentially done. This is the discipline that decides whether the signal ever touches
real money — and how much. Work top to bottom; **do not skip to the bottom.**

> Not financial advice. This is a personal decision framework for your own tool. Markets can take
> more than you put in. Size so that being wrong is survivable and boring.

---

## Stage 0 — It actually runs (one evening)
- [ ] `docker compose up` brings **every** service up healthy: analysis, llm, prediction, gateway,
      alerts, journal, (paper, once built), web. Check each `/health`.
- [ ] Real keys in `.env` (Alpaca, Alpha Vantage, Marketaux) — dashboard shows **real** bars,
      quotes, earnings, news (provenance badges say live, not synthetic/seed).
- [ ] Ollama running with qwen — the read is model-generated, not the stub.
- [ ] Ticker + timeframe switching works; the header quote refreshes.

## Stage 1 — Honest backtest (before any signal is believed)

**The fast way to run this stage is the offline evaluation harness** — a batch CLI that pools trades
across a whole universe of liquid names and prints a single verdict: **EDGE / NO EDGE / INCONCLUSIVE**
(or **SUSPECT** when the Sharpe is implausibly high). It is research-only: no buy/sell output, no
orders, no money.

```bash
make evaluate                 # docker: universe x horizons x walk-forward x permutations (long-running)
make evaluate-quick           # a fast smoke pass on a handful of tickers
# or locally, against a running analysis on :8001:
cd services/prediction && python -m app.evaluate
```

What it does, and why each piece is there:
- **Real data only.** Before doing anything it checks `source`/`sourceIsSynthetic`. If *any* ticker
  resolves to the synthetic ~$162 seed, it prints `SYNTHETIC DATA — cannot validate` and **exits
  non-zero** — it never produces a verdict on synthetic data. (Same for a totally unreachable
  analysis service: `NO DATA`, non-zero exit.)
- **No look-ahead.** It reuses the exact leakage-safe feature/label/walk-forward code the signal
  uses, and reserves an **untouched final holdout** that no fold trains on. Costs (commission +
  slippage bps) are subtracted from every trade.
- **Pooled for power.** A single ticker can't answer "is there an edge"; the harness pools
  out-of-sample trades across the whole universe, then reports the **walk-forward folds and the
  untouched holdout separately**.
- **Baselines that can embarrass the model.** It compares against **buy-and-hold** (risk-adjusted)
  and against a **permutation null** — it shuffles the model's position timing hundreds of times to
  build a distribution of "what random timing would have earned," and reports a **p-value**.
- **Honest verdict.** `EDGE` is granted only if **all** of: positive holdout expectancy after costs,
  `0 < Sharpe ≤ 3`, beats buy-and-hold risk-adjusted, permutation `p < 0.05`, and enough pooled
  trades. A **Sharpe > 3 is flagged SUSPECT** (probable leakage), never celebrated. Otherwise the
  honest answer is **NO EDGE** — and "there is no tradeable edge here" is a real, valuable result,
  not a failure. Reports are written to `data/eval/report-<UTCstamp>.{md,json}`.

> **A verdict of `EDGE` is a *necessary* condition, not a green light.** It means the model earned
> the right to be **paper-traded** (Stage 2 below) for several weeks. It never authorizes real money
> on its own. `NO EDGE` / `INCONCLUSIVE` / `SUSPECT` mean **stop** (or gather more data) — do not tune
> until it "works," that's how you fool yourself.

### Event study — post-earnings drift (a second, independent alpha source)

The **event-study harness** applies the *same* discipline to a different signal: **post-earnings
drift (PEAD)** — does a strong earnings surprise predict drift over the next 10–20 sessions?

```bash
make evaluate-events          # docker (needs ALPHAVANTAGE_API_KEY for earnings history)
make collect-estimates        # local/docker path for one bounded forward snapshot pass
cd services/prediction && python -m app.evaluate_events
```

Production has no shell. Run the collector from **Settings → Preferences → Forward estimate
snapshots → Capture due estimates**. The button uses `EVAL_ADMIN_UIDS`, takes no parameters and
shows the durable run status plus captured, existing, actual-refresh and provider-error counts.

- **No look-ahead, by construction.** SUE = (reportedEPS − estimatedEPS) / std(**prior** surprises);
  entry is the **open of the first session AFTER `reportedDate`**, so all earnings info is already
  public (asserted). It trades only **strong** surprises (|SUE| in the top/bottom tercile),
  direction = sign(SUE), holds H sessions, subtracts round-trip costs.
- **Holdout-first gates.** Split on unique entry dates before selecting SUE terciles, freeze the
  report-era thresholds, and judge a date-aligned 1/N portfolio on stock-minus-SPY daily returns.
  The untouched holdout owns expectancy, Sharpe, matched-long and circular-shift significance;
  significance is corrected for the number of horizons. Sample floors can only return INCONCLUSIVE.
- **Refuses honestly.** No provider key and no PostgreSQL payload → refusal; synthetic stock or
  benchmark prices → refusal. Provider payloads persist with hashes and fetch provenance. The
  historical estimate vintage remains descriptive. A bounded daily collector captures exact-period
  T−7/T−1 consensus snapshots before the report date and refreshes actuals afterward. The evaluator
  derives provenance per event and uses only the forward-verified sample once enough prior-only SUE
  observations exist; no environment flag can promote historical rows.
- **Reads the SURVIVORSHIP-BIAS warning at the top of every report.** The universe is *today's* names;
  delisted/acquired/bankrupt names are missing, so any positive drift is an **upper bound**. Drift is
  also weaker in heavily-covered mega-caps — a broader / smaller-cap universe is a fairer test.
- **Test 2 (AI marginal value)** asks whether combining SUE with an AI **tone** read of the event-time
  text improves Test 1 on the *same* events. It is honest about data limits: event-time text is
  scarce, so it runs only on the retrievable subset, reports the **sample size**, is **INCONCLUSIVE**
  below `EVENT_MIN`, and **accumulates forward** in PostgreSQL. It is disabled by default until the
  no-AI baseline and a forward event-time corpus exist.

Run it from **Settings → Preferences → PEAD event study** (same `EVAL_ADMIN_UIDS`, manual/no
parameters) or the CLI. Its report is deliberately separate from price verdicts and paper gate 4.
Same rule as above: an `EDGE` verdict here is research evidence, never a green light for real money.

The manual, per-config version of the same discipline (useful for drilling into one ticker):
- [ ] `POST /train` for each (ticker, timeframe, horizon) you care about, on **real** data. This
      creates an immutable **candidate**; it does not replace the active model.
- [ ] Read each backtest report and apply the sanity gates:
  - [ ] `trainedOnSynthetic == false`.
  - [ ] Sharpe is in a believable band — **> 3 means a bug** (leakage/bias), not a win. If you see
        it, stop and find the leak.
  - [ ] Positive expectancy **after** costs, and `numTrades` ≥ ~30 in the report.
  - [ ] The signal survives the untouched final holdout, not just the CV folds.
- [ ] Expect modest: ~53–56% hit-rate, Sharpe under ~1.5 is a *good* honest result. If everything
      is ~50% / negative expectancy, the honest conclusion is **there is no tradeable edge here** —
      that's a real, valuable finding. Don't tune until it "works"; that's how you fool yourself.

## Stage 2 — Paper-trade (several weeks, no money)

**This is the actual go-live checklist for the paper engine. Work it top to bottom.** Each step is a
precondition for the next. `GET /paper/readiness` evaluates the complete launch checklist, and the
confirmed reset consumes that same result; there is no separate, weaker start path. A refusal is the
design working, not an outage.

The normative rule is [`PAPER_EXECUTION_CONTRACT.md`](PAPER_EXECUTION_CONTRACT.md): a per-bar
allocation rule, four fail-closed gates (§4), and one simulated book keeping score the way the
evaluator does (§5). Nothing in this stage executes an order or touches a broker.

### 2.1 Real market data configured
- [ ] Price/OHLCV keys in `.env` (`TIINGO_API_KEY` for `analysis`, or a keyless yfinance path that
      actually reaches the network). Confirm at `GET /candles/NVDA` that `sourceIsSynthetic` is
      `false` and `source` names a real provider.
- [ ] `GET /quote/NVDA` returns a price with a **non-empty `source`** and a **parseable `asOf`**.
      Gate 1 refuses an empty provenance and §3.5 refuses a fill dated before the bar it reconciles;
      an unknown source is not a clean one.

### 2.2 Train a candidate for every config on real data
- [ ] `POST /api/train/{ticker}?timeframe=…&horizon=…` (through the gateway — the in-app path, and
      the only one available on the shell-less deploy) for each `(ticker, timeframe, horizon)` in
      `PAPER_CONFIGS`, against live providers. Settings → Preferences has the same control, and so
      does the signal band. `POST /train` directly on the prediction service is the equivalent when
      you do have a shell. The result is a challenger version and serving remains unchanged.
- [ ] **`trainedOnSynthetic: false` and `dataPolicyCurrent: true` in EVERY record.** A passing
      backtest of a model fitted on invented prices—or on a forming candle—is not evidence.
- [ ] Do not expect the candidate to appear at `/predict` yet. Promotion happens only after §2.3
      produces matching pooled evidence.

### 2.3 Run the evaluator on real data

**In the app is the primary path**, because the production deploy (`deploy/supervisord.conf` + the
root `Dockerfile`) is a single container with **no shell and no docker access** — the harness was
CLI-only, so a verdict could not be produced there at all and this step could not be performed.

- [ ] **Settings → Preferences → "Edge evaluation" → Run edge evaluation**
      (`POST /api/evaluate/run`; the status card polls `GET /api/evaluate/status` while it runs).
      Requires a signed-in session **and** membership of `EVAL_ADMIN_UIDS` — which is **empty by
      default, meaning nobody**. Set at least one auth user id (`GET /auth/me` → `user.id`, never an
      email) before you need this, or the button answers 403 and says so.
- [ ] **Optional bounded automation:** set `PREDICTION_AUTOMATION_ENABLED=true` to let the supervised
      prediction controller create an immutable challenger after its fixed completed-bar trigger,
      start this same parameter-free price evaluator, and collect paired prospective shadow evidence.
      Watch **Settings → Preferences → Challenger automation**. The three-attempt per-config budget,
      evaluator parameters, and 20-paired-bar shadow floor are fixed before results are seen.
- [ ] **The run takes no parameters, deliberately.** It uses this deployment's own `EVAL_*`
      environment, so the verdict it mints carries the deployment's own strategy version and can
      never be a custom-parameter run masquerading as the default strategy (contract §4.3). A query
      string or body is refused with 400 naming that rule, not ignored.
- [ ] One run at a time (an atomic lock under `EVAL_OUT_DIR`); a second request answers 409 with the
      running job's status. The trigger is either the explicit operator action above or the optional
      bounded prediction controller—never a browser read, LLM loop, or parameter search. The harness
      is CPU-heavy and `/predict` will be slower while it runs; that is an accepted trade on one box.
- [ ] Read the outcome **as the exit code's meaning, not as success/failure**: `0` = a verdict was
      produced (**including `NO EDGE` / `INCONCLUSIVE` / `SUSPECT` — those are results**), `2` =
      refused on synthetic data, `3` = nothing fetched. The card renders all three honestly and
      shows the run log's tail.
- [ ] **Alternative, when you do have a shell:** `cd services/prediction && python -m app.evaluate`
      (or `make evaluate`) against live providers. Identical run, identical refusals — the endpoint
      starts exactly this process and adds nothing to it.
- [ ] Read the report from the evaluator status card (`prediction.artifacts` in production). It
      writes one verdict row per `(ticker, timeframe,horizon)` into `prediction.verdicts`; the card lists each one with
      the `current` flag gate 4 actually spends, so you can see from the app whether it would pass.
- [ ] Then check `GET /paper/status` (§2.6): the gates are what turn a verdict into permission, and
      they are the only thing that does.

### 2.4 The verdict is honestly `EDGE`, then the candidate is promoted—or the strategy is shelved
- [ ] The verdict for **each horizon** is `EDGE`, and its `strategyVersion` matches the candidate's
      own `strategy_version(cost_bps, upper, lower, allow_short)`, and its `method` is the current
      `portfolio-v4`; its evidence and data policy must also be current.
- [ ] Review the challenger in Settings → Preferences. Promotion remains disabled until its
      real-data, data-policy, strategy-identity, clean walk-forward and evaluator gates all pass.
      Promotion is an audited pointer change; a failed candidate never overwrites the active
      version. After promotion, `/predict` serves that exact `modelVersion` and the matching
      evaluation block.
- [ ] For the first production cycles, also review the autonomous trial's paired prospective shadow
      result. `candidate-ahead` is supporting research only, not an `EDGE` verdict or permission to
      promote. Promotion and the official paper-clock reset stay manual and audited.
- [ ] The promoted record's `dataThrough` is within `PAPER_MAX_MODEL_AGE_SESSIONS` (default 10)
      sessions of the latest bar. Gate 2 refuses a stale active model even on fresh data.
- [ ] **Any verdict stored under `portfolio-v2` or `portfolio-v3` is no longer spendable.** v3 added
      hard sample-sufficiency floors; v4 adds the `completed-bars-v1` evidence identity so a verdict
      cannot be earned from a still-forming live candle. Re-run §2.3 after retraining; do not edit a
      stored record.
- [ ] **The verdict states the sample it rests on.** Every horizon block and every verdict record
      carries a `sufficiency` block, and a run that does not clear these floors returns
      `INCONCLUSIVE` with the shortfall named. None of them can produce `EDGE`:

      | knob | default | what it floors |
      |---|---:|---|
      | `EVAL_MIN_DATES` | 252 | portfolio observation dates in the pooled out-of-sample span |
      | `EVAL_MIN_HOLDOUT_DATES` | 60 | dates in the untouched holdout no fold trained on |
      | `EVAL_MIN_TICKERS` | 10 | evaluated streams at that horizon |
      | `EVAL_MIN_COVERAGE` | 0.7 | evaluated / configured tickers |

      Tickers that **failed to fetch or were skipped** for too little history count **against**
      coverage and are **named** in the checklist line, so a verdict from a shrunken cohort says so
      on its face. These values are also **hard live spendability floors**: `EvalConfig.from_env`
      rejects lower overrides, and `evaluation.current` independently verifies both the thresholds
      used and the actual observations stored in `sufficiency`. Raising a floor is legitimate;
      lowering one cannot mint a verdict gate 4 accepts.

> **State this plainly, because it is the most likely outcome and the whole point of the exercise: a
> persistent `NO EDGE` or `INCONCLUSIVE` result on the corrected statistics means DISABLE, not tune.**
> The pooled evaluator is date-aligned, cost-subtracted, holdout-reserved and permutation-tested
> precisely so that a negative answer is trustworthy. "There is no tradeable edge here" is a real,
> valuable finding. Re-running with different thresholds until one passes is not a fifth gate; it is
> the failure mode all four exist to prevent. **Do not hand-write a verdict file, relax `evaluate.py`,
> or add a bypass flag.**

### 2.5 The engine can record what it does
- [ ] `AUTH_SECRET` is set for the `paper` service, so D-20 journal recording works. Without it the
      journal answers 401 to every write, and **a position the engine cannot record is a position it
      does not take** — so it opens nothing.
- [ ] `GET /paper/status` → `book.records` shows `journal: true` and `ledger: true`. A dead recorder
      must never read as "no signals fired"; the payload states which record is alive.

### 2.6 All four gates pass
- [ ] `GET /paper/status`: for every config, all four gates report `ok: true` —
      `no-synthetic-data`, `fresh-data`, `backtest-passed`, `evaluator-verdict`.
- [ ] When one refuses, the reason is in the payload. Fix the cause; never the gate.

### 2.6a The configured scope is exact
- [ ] In Paper Trading, choose **Edit scope**, enter the exact experiment configs (for example
      `NVDA:1D:5,GOOGL:1D:5`) and save. This writes the durable paper configuration; removing a
      deployment environment value does not erase a config already persisted in PostgreSQL.
- [ ] Re-run **Check launch readiness** and confirm every listed config is intended. Changing this
      scope after day 0 invalidates the official clock because it changes the portfolio's `N`.

### 2.6b The three stores agree
- [ ] `GET /paper/status`: every config's `sync` block reads `consistent: true` with
      `pendingBookings: 0`, and the run-level `reconciliation.desyncedConfigs` is empty (contract
      §5.9). A **desynced** config refuses to change its position and names the mismatch; it keeps
      marking, so the book stays honest while the bookkeeping is repaired.
- [ ] `sync.journalChecked` and `sync.ledgerChecked` are both `true`. A missing ledger, an
      unreachable journal, or a non-durable engine-state write makes `consistent: false` and blocks
      position changes.
- [ ] A non-zero `pendingBookings` is a fill the ledger owes. It is retried at the start of every
      tick and is idempotent by `(trade id, kind)`. If it does not clear, the ledger is refusing it
      for a reason worth reading in the logs — do not reset to make it go away.

### 2.7 The ledger is initialized and marking
- [ ] `GET /paper/ledger` returns the book: `startingCash` = `PAPER_STARTING_CASH`, `sizing` says
      **`equity/N`** (NOT "DEPRECATED FALLBACK"), and `snapshots.n` is growing by one per completed
      trading date.
- [ ] `gapDates` is empty, or every entry has a real explanation. A gap is a date the book could not
      mark — a synthetic or missing bar — and it is recorded rather than smoothed over.
- [ ] `metrics.dailySharpeAnnualized` will be `null` until **20 snapshot days** exist. That is
      correct: a Sharpe from nineteen observations is not a small number, it is not a number.

### 2.8 START THE CLOCK — `POST /paper/reset`, once, successfully
**`POST /paper/reset` is the OFFICIAL EXPERIMENT START.** Run it exactly once, after everything above
passes and before you start counting days. Everything the engine did while you were configuring it —
half-gated decisions, positions opened under a model you have since retrained, snapshots of a book
whose `N` has changed — is setup, not evidence, and leaving it in the series makes the first weeks of
the experiment unreadable.

- [ ] Click **Check launch readiness**. `GET /paper/readiness` must say `ready: true`; it checks the
      real bar clock, durable stores, journal recording, real/fresh inputs, all four gates, validated
      signal and execution-quote integrity for every config. A reset cannot bypass a failed check.
- [ ] `POST /paper/reset` with `confirm=true` and a signed-in session (the panel's
      **"Reset — start the experiment"** button does both).
- [ ] **It must return `200` with `ok: true`.** The reset deletes the journal's paper trades **first** and aborts on
      any failure: if even one deletion fails it answers `502`, lists what failed and what was
      already deleted, and leaves the engine state and book untouched but **fail-closed** because the
      stores no longer agree. A ledger or engine-state persistence failure answers `500` with
      `ok: false`; it never declares an official start. Fix the named store and run it again — the
      journal cleanup is idempotent over trades it already deleted.
- [ ] Confirm the result: `deletedPaperTrades` matches what you expected, `engineStateReset: true`,
      `ledgerReset: true`, and `officialStartedAt` is present. The old PostgreSQL ledger generation
      remains archived and queryable; its fills and snapshots are never deleted (contract §5.7).
- [ ] `GET /paper/ledger` must repeat `officialStartedAt` and the exact `officialConfigs`. That
      durable timestamp—not a manually remembered date—is the official clock. The first snapshot is
      still reserved for a real completed-bar mark; reset never fabricates a market observation.
- [ ] From here on, **do not casually change the scope.** Removing a config that holds a position is
      refused with `409`; any successful scope change durably stops the official clock. Start a new
      clean generation only after the changed scope passes readiness again.

### 2.9 Then wait. Weeks, not days.
- [ ] Use Journal → Experiments or `GET /paper/dashboard` as the monitoring surface. Its `clock`,
      `integrity`, `sample`, and `result` dimensions are separate on purpose: a running clock can
      still have degraded evidence, and a healthy experiment can still be statistically unjudged.
- [ ] `decisionHistory.total` counts settled completed bars only. Repeated data/quote/journal retries
      remain visible in current status but do not inflate the evidence sample. Every settled event
      must name its experiment generation and, when prediction evidence was available, its model and
      strategy versions.
- [ ] `ledger.series` carries dated equity and drawdown. A gap row has null measurements and a
      reason; never treat it as zero or carry the previous equity forward.
- [ ] **~30 closed position episodes per config** before the per-trade columns mean anything, and
      **≥ 20 daily snapshots** before the portfolio Sharpe exists at all.
- [ ] Restated for the **per-bar rule**: a "trade" is a position *episode*, not a bar
      (contract §1.2). Under a per-bar allocation rule the model re-decides daily but only *changes*
      its position when the target flips, so episodes accumulate at the rate the signal changes its
      mind — typically far slower than one a day. On a daily timeframe, **30 episodes is realistically
      several months, not several weeks**, and a config whose signal is stable can take longer still.
      There is no way to speed this up that does not also destroy the evidence.
- [ ] Read `/paper/comparison`. The **`portfolio` block** is the like-for-like comparison (contract
      §5.4): live daily portfolio Sharpe against the evaluator's portfolio Sharpe when it is served,
      else against the model backtest's annualized Sharpe with the unit caveat kept — the payload's
      `portfolio.reference.source` says which. The per-trade columns above it are counting stats in
      different denominators and are gross of fees; compare them as counts, not as returns.
- [ ] If live materially underperforms → the edge didn't generalize. Back to Stage 1 or shelve that
      config. **This is the single most important gate**, and shelving is a legitimate outcome.

### 2.10 Persistence — do not lose the evidence

Weeks of paper history is the whole asset of this stage. With `DATABASE_URL` configured, the
experiment-critical records are PostgreSQL-only in production:

| what | PostgreSQL owner |
|---|---|
| immutable model versions, active pointers and promotion/rollback audit | `prediction.model_versions`, `prediction.model_deployments`, `prediction.model_promotion_events` (`prediction.models` is the rolling-deploy compatibility mirror) |
| eval, event-study, calibration and ablation reports/verdicts | `prediction.artifacts`, `prediction.verdicts` |
| PEAD raw earnings revisions, forward consensus snapshots and event-time text | `prediction.earnings_payloads`, `prediction.earnings_estimate_snapshots`, `prediction.earnings_event_texts` |
| daily LLM reads, committee snapshots, transcripts and personas | `llm.snapshots` |
| engine state and pending bookings | `paper.documents` (`engine_state`) |
| ledger state, fills, snapshots and reset history | `paper.documents`, `paper.fills`, `paper.snapshots`, `paper.resets` |
| journal trades | `journal.trades` |
| theses, evidence, decisions, portfolios, onboarding and saved reviews | `journal.documents` |
| product analytics | `journal.analytics_events` |
| alert rules, events, read state and thesis-resynthesis queue | `alerts.*` |
| tester submissions and triage state | `feedback.feedback` |
| accounts and preferences | `auth.users`, `auth.settings` |

- [ ] The managed PostgreSQL add-on has durable storage and backups enabled.
- [ ] Restart prediction, llm, paper, journal, alerts, feedback and auth; verify their records remain
      available. The opt-in repository tests (`*_TEST_DATABASE_URL`) exercise this close/reopen path.
- [ ] `POST /paper/reset` starts a new ledger generation. Prior fills/snapshots stay queryable under
      their previous generation and `paper.resets` records the transition; reset is not deletion.
- [ ] `/data` is still used for bounded runner logs, the cross-process model lease,
      explicit no-database development fallback, and one-time JSON import. It is not the source of
      truth for any record in the table above.

## Stage 3 — Go live small (only if Stages 1–2 pass)
- [ ] Decide a **fixed, small** position size and a total capital cap you can lose without it
      mattering. Write it down.
- [ ] Pre-commit **kill criteria** before you place a trade: e.g. stop trading a model if live
      hit-rate drops below X over the next N trades, or drawdown exceeds Y%. Put the numbers here:
      - Kill if: __________________________________________
- [ ] Every real order is placed **by you** in your broker. The app never executes.
- [ ] Log every real trade in the journal (mode=live) with the signal that was live at entry.

## Stage 4 — Monitor for decay (ongoing)
- [ ] Weekly: live journal stats vs the model's backtest. Edges decay and regimes shift (the 2026
      research showed a solid model break during the AI rally). Expect it; watch for it.
- [ ] Create refresh candidates only on a precommitted completed-bar schedule; re-run Stage 1 gates
      and explicitly promote. Do not repeatedly tune or fit against the same evaluation window.
- [ ] If a model trips its kill criteria, stop trading it and go back to paper. No exceptions,
      no "it'll come back" — that's how small losses become big ones.

---

## Honest bottom line
The tool is excellent at gathering evidence, structuring a read, watching for conditions, and
recording what happened. Whether the **prediction signal has a real, tradeable edge is unknown until
Stages 1–2 say so on your data** — and it's entirely possible the honest answer is "no." Either
outcome is a win for you: a validated small edge to trade carefully, or the knowledge to *not* risk
money on noise. The worst outcome is skipping this and trading a confident-looking backtest that was
lying. Don't do that.
