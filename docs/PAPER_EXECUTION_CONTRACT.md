# Paper execution contract

**Contract version: `1.1.0`** (`EXECUTION_CONTRACT_VERSION` in `services/prediction/app/strategy.py`).

> **1.1.0 (2026-08-23)** — two changes to the LIVE MAPPING, both of which invalidate every stored
> verdict by construction (§4.3): §2 gained **bar completeness** (a forming bar is not a bar to act
> on) and §3 gained the **execution quote's `asOf` requirement** (a fill dated before the bar it
> reconciles is not a fill). §4 gate 1 additionally refuses an **empty** provenance, not only the
> literal string `synthetic`. Nothing in §1 changed: the rule, the label, the features, the model
> parameters, the thresholds and the backtest math are all untouched.
>
> **2026-08-25 — three changes, NONE of which bumps the version, and here is why for each.**
> §5.3 now *states* that the daily snapshot is the **pre-trade** mark of the bar's close and is
> independent of `CONFIGS` ordering (the engine's tick is two-phase); that documents accounting the
> rule already implied, and §5 is unversioned by the paragraph below. §5.9 adds the **three-store
> reconciliation** — a desynced config refuses to change its position — which only ever REFUSES; it
> can never cause a position the gates would not already have allowed. §3.5's `asOf` check is
> tightened to compare **instants against the bar's end on intraday timeframes**; that IS §3, but it
> is strictly stricter, and it changes nothing at all for `1D` — the only timeframe any shipped
> config, any trained record, or any stored verdict uses. Bumping over it would invalidate every
> verdict for a rule change that cannot have applied to any of them. If an intraday config is ever
> configured, that is the moment to revisit this line.
>
> **2026-08-25 — completed feature frames.** The prediction service now requests
> `completedOnly=true` from analysis for every train/evaluate/predict frame and stamps trained
> records `dataPolicy="completed-bars-v1"`. This implements §2.1 consistently; it does not change
> the rule, so the execution-contract version stays 1.1.0. Evaluator methodology is bumped to
> `portfolio-v4`, however, and old/unstamped models are refused until retrained: evidence earned
> while a forming candle could enter the frame is not reusable.
>
> **§5 (accounting) added 2026-08-23 WITHOUT a version bump, deliberately.** §§1–3 state the trading
> RULE; §5 states how the RESULT of that rule is counted. A stored verdict is a statement about the
> rule, and the rule is untouched — the same bars produce the same positions at the same thresholds
> under the same costs. Bumping `EXECUTION_CONTRACT_VERSION` would invalidate every verdict over a
> non-change, and would teach the next reader to treat a stale-verdict signal as noise. **The rule is
> versioned; the bookkeeping is not.** §3.3 and §3.4 are superseded by §5 and say so in place.

This document is **normative**. Two independent implementations claim to run the same strategy — the
Python walk-forward backtest in `services/prediction` and the Go paper engine in `paper/` — and a
comparison between them is only worth reading if they are running the same rule. This file states
that rule once. Both implementations are held to it, and any divergence between this document,
`services/prediction/app/backtest.py` and `paper/engine.go` is a **defect in whichever of the three
moved**, not a difference of opinion.

It exists because they had drifted. Before this contract the backtest re-decided its position every
bar while the paper engine entered once and held H bars, and the comments in `paper/engine.go` and
`paper/config.go` asserted the opposite — that the hold "mirrors the backtest's fixed-horizon exit
exactly". The backtest has no fixed-horizon exit. Three different strategies (H-bar label, next-bar
payoff, H-bar hold) were being compared to each other as though they were one.

Nothing here executes an order, reaches a broker, or moves money. The paper engine **simulates**;
`mode="paper"` journal rows are kept strictly separate from anything else. That is invariant #2 and
this document does not touch it.

---

## 1. Strategy semantics

The model is an **H-bar-direction classifier used as a per-bar allocation rule**. Those are two
different things and the distinction is the whole of §1.3.

### 1.1 The rule

At each bar close `t`:

1. Build the feature row **lagged one bar**: `X_t = features_{t-1}`. The decision at `t` therefore
   uses only data that closed at `t-1` or earlier (`features.build_dataset`, `latest_feature_row`).
2. Score it: `p = p(up | X_t)`.
3. Set the **target position**:

   | condition | target |
   |---|---|
   | `p >= upper` | `+1` (long) |
   | `p <= lower` **and** the record was backtested with `allowShort=true` | `-1` (short) |
   | otherwise | `0` (flat) |

   `upper`/`lower` are the record's `report.thresholds` (defaults 0.55 / 0.45,
   `model.DEFAULT_UPPER` / `DEFAULT_LOWER`). A `-1` target is **never** produced from a record whose
   `allowShort` is false: low `p(up)` there means "do not be long", not "be short"
   (`model.derive_direction`).
4. Hold that position from close `t` to close `t+1`, earning the close-to-close simple return
   `ret_next[t] = close_{t+1}/close_t - 1`.
5. At `t+1`, re-decide from scratch. There is no timer, no fixed hold, and no exit rule other than
   "the next bar's target is different".

Costs: `cost_bps` (commission + slippage, `COMMISSION_BPS + SLIPPAGE_BPS`, default 6 bps) is charged
on `|Δposition|` at every position change — so a flat→long open pays 1×, a long→short flip pays 2×,
and a held position pays nothing. This is exactly `backtest.net_returns`:

```
prev[t]      = positions[t-1]           (0 before the first bar)
turnover[t]  = |positions[t] - prev[t]|
net[t]       = positions[t] * ret_next[t] - (cost_bps/10000) * turnover[t]
```

**The walk-forward gate validates this rule as executed.** `model.train_and_backtest` derives
positions from out-of-fold probabilities with `derive_positions` — the same three-way map as step 3 —
and pays them with `run_backtest(ret_next, ...)` — the same next-bar payoff as step 4. `report.passed`
is a statement about *this* rule and about nothing else. A paper engine running a different rule is
not being validated by that gate, whatever its own numbers look like.

### 1.2 What a "trade" is

A trade is **a transition into a new nonzero position** — `entries[t] = (positions[t] != prev[t]) and
(positions[t] != 0)` in `net_returns`, counted as `numTrades` in `run_backtest`. Consequences, which
both implementations must share:

- Holding the same nonzero position for nine bars is **one** trade, not nine.
- A flip (long→short) is **two** events at one reconciliation: it closes one trade and opens another.
- Going flat closes a trade and opens nothing.

`numTrades` and the live closed-trade count therefore mean the same thing: position *episodes*.

### 1.3 The label/payoff asymmetry — a known modeling choice, not a bug to fix here

The label is the sign of the **H-bar** forward return
(`features.build_dataset`: `y_t = 1[close_{t+H}/close_t - 1 > 0]`), while the payoff accounted above
is the **next bar's** return. The classifier is asked "will price be higher in H bars?" and its answer
is spent on a one-bar bet, re-taken every bar.

This is **deliberate and recorded as an open research question**, not a licence to change either
half. It is defensible — an H-bar directional view held continuously is a legitimate way to express a
slow signal, and re-deciding every bar is what makes the position series honest about acting on new
information — but it is an assumption, and it is the most likely explanation for any live/backtest
divergence that is not simple edge decay.

**This contract does not change the label, the features, the model parameters, the thresholds, or the
backtest math.** `horizon` remains a property of the model — it selects which trained record
`/predict` serves and names what the classifier was trained to predict. It is **not** a holding
period, and no implementation may treat it as one.

Open question, for whoever picks it up: measure a label horizon of 1 against the current H, on real
data, through `app.evaluate`. Until that has been run, H stays where it is.

---

## 2. What a "bar" is

**A bar is a trading session as served by the analysis service for the configured timeframe.** It is
never a wall-clock interval.

- The bar sequence is whatever `analysis` returns for `(ticker, timeframe)` — the same frames the
  model trained on and the same ones the chart draws.
- Weekends, holidays and halts simply produce **no bar**. Nothing decides, nothing is held "for a
  day", and no clock advances on the strategy's behalf.
- Bar identity is the bar's own timestamp: `YYYY-MM-DD` for daily, UNIX seconds for intraday
  (`analysis`'s `_fmt_time`). "A new bar" means *strictly newer than the last timestamp acted on* —
  a comparison between two timestamps, never between a timestamp and `time.Now()`.

### 2.1 A bar must be COMPLETE before it may be decided on *(added in 1.1.0)*

Strictly-newer is necessary and **not sufficient**. `/candles?limit=1` serves the newest bar the
provider *has*, and during a session that bar is still **forming**: its close is not its close, and
its timestamp is already strictly newer than yesterday's. The engine used to act on it, which meant
deciding at a price that had not happened yet — and then *never re-deciding*, because the cursor had
moved past it. One decision per bar, taken on data that does not exist yet, is worse than no
decision.

A bar is complete when the session it names is over:

| timeframe | complete when |
|---|---|
| `1D` | the current UTC **date** is strictly after the bar's date |
| `1H` / `15m` / `5m` | `now >= barStart + barDuration` |

`barDuration` (`paper/bar.go`) is the **only** legitimate per-timeframe seconds arithmetic in this
service. It judges whether a session has FINISHED; it never derives a position lifetime, a hold or an
exit. `barSeconds`-as-a-lifetime is what this contract removed and it is not coming back.

An incomplete newest bar is **"no new bar yet"**: no decision is recorded, the cursor does not move,
and the next tick asks again. Fail-closed — an unreadable bar timestamp is *not* complete, because
"we cannot tell whether this session is over" must never resolve to "act on it".

The same rule applies before the paper engine sees a signal. Prediction requests
`/analysis/{ticker}/features?completedOnly=true`; analysis removes an incomplete last row before
training, evaluation and live scoring. A model record must carry `dataPolicy="completed-bars-v1"`,
and its evaluator verdict must carry the same policy. Missing or older policy identities are stale,
not grandfathered.

`PAPER_BAR_SECONDS > 0` bypasses the completeness check along with the new-bar check. It is the
documented demo/testing fast-forward, and it is `0` everywhere real.

The old `barSeconds` calendar-day arithmetic (`1D → 86400s`) is gone from position lifetime entirely.
It counted Saturdays. `PAPER_BAR_SECONDS` survives with a **redefined meaning**: when `> 0`, every
`EVAL_INTERVAL` tick is counted as a new bar so a demo or test can fast-forward. It is a testing
override, it does not change the rule, and it is `0` everywhere real.

---

## 3. Live mapping

One decision per config per bar.

1. **Detect the bar.** On each `EVAL_INTERVAL` tick the engine asks `analysis` for the latest bar for
   `(ticker, timeframe)`. If that timestamp is not strictly newer than the config's persisted
   `lastBarActedOn`, the tick ends: no fetch, no decision, no log line. `EVAL_INTERVAL` is a *polling*
   rate, not a decision rate — polling faster does not trade more.
2. **Derive the target.** Fetch `/predict` for the config, apply §1.1 step 3: `Buy → long`,
   `Sell → short`, `Hold → flat`. A null signal is an **absence**, not a flat target: it produces no
   action at all (see §3.1).
3. **Check the gates** (§4). A refusal is recorded against the bar and the tick ends.
4. **Reconcile** against the current position, at the first **usable** quote observed after that bar
   exists. A quote is usable only when all of the following hold — see §3.5:

   | current → target | action |
   |---|---|
   | flat → flat | none |
   | X → X (same side) | hold — no journal write, no cost |
   | flat → long/short | **open**: one `journalCreate` |
   | long/short → flat | **close**: one `journalCloseExit` at the quote |
   | long → short (or short → long) | **flip**: `journalCloseExit` **then** `journalCreate`, in that order, at the same reconciliation and the same quote |

5. **Record.** The engine marks `lastBarActedOn` and persists the decision — bar timestamp, target,
   action, and, when nothing happened, which gate refused and why.

Journal writes are unchanged: `mode="paper"`, `origin="signal"`, the dedicated system-user credential
of D-20 (`paper/auth.go`). With no `AUTH_SECRET` the journal refuses the write, the engine logs it and
keeps running, and `book.recording` reports `false` — a book that records nothing must say so rather
than present an empty table as "no signals fired".

### 3.1 Idempotency and failure

`lastBarActedOn` advances **only when a decision was actually reached** for that bar — including a
deliberate refusal, which is a decision. It does **not** advance when an upstream was unreachable
(prediction down, quote unavailable while an action was needed), so the engine retries on the next
tick within the same bar. A restart mid-bar re-reads `paper.documents` (`engine_state`), sees `lastBarActedOn` already at
the current bar, and does nothing: **acting twice on one bar is a defect.**

### 3.2 Provenance recorded on every trade

Each opened journal trade carries both halves of the mapping:

- the **executed quote** — price, and the quote's `source` **and `asOf`**, on `attachedSignal`;
- the **bar timestamp it was decided on** — `attachedSignal.decidedOnBar`;
- the **model's label horizon** — `attachedSignal.horizon`.

so that a fill can always be traced back to the decision that caused it, and a fill whose bar and
quote disagree is visible rather than inferred.

`horizon` is on that list because **`(ticker, timeframe)` is not a config key.** Two horizons of one
ticker are two different models with two different track records, and grouping closed trades by the
pair credits each with the union of both. `/paper/comparison` groups by `ticker:timeframe:horizon`. A
**legacy** trade written before this block existed carries no horizon; it is attributed only when
exactly one configured `(ticker, timeframe)` could have produced it, and otherwise **excluded and
counted** in the payload's `unattributed` field. A validation number built on a guess is not a
validation.

### 3.5 The execution quote must be usable *(added in 1.1.0)*

`analysis /quote` serves `asOf` and `source`. The engine used to parse the price and drop both, so a
quote of unknown provenance and unknown age was indistinguishable from a fresh real one. A quote may
stand as a fill only when:

- **it has a price** (`> 0`);
- **its `source` is non-empty** and is not `"synthetic"` — an empty source is not a clean provenance,
  it is the absence of one;
- **it has a parseable `asOf`**, and that `asOf` is **not dated before the bar it reconciles**. A
  fill older than the decision it executes is not a fill.

**The `asOf` comparison is per timeframe** *(tightened 2026-08-25)*. It compared calendar DAYS for
every timeframe, which is right for `1D` and wrong for everything else: on a `15m` frame a quote
stamped 09:35 is the same calendar day as a bar that closed at 15:45, so a fill from six hours
*before* the decision passed a check whose entire claim is that it would not. For an **intraday**
timeframe (`1H`, `15m`, `5m`) the quote's parsed instant must be **at or after the decided bar's
END** (`start + barDuration`) — the moment the close it reconciles actually existed. `1D` keeps the
date-level rule, because a daily session has no wall-clock end this service computes (§2.1 judges it
over by date, and so does this). An intraday bar with no instant to compare against **refuses**, and
an unparseable `asOf` still refuses on every timeframe.

A failure here is **retryable**: the position does not move, the bar is **not** consumed, and the
next tick tries again with a fresher quote.

**This applies to closes too, and that is deliberate.** Closing to flat is exempt from the four
*gates* (§4) — a gate must never trap an open position — but execution-price integrity is not a gate
exemption. When the close-path quote fails any check above, the close is **deferred**: the position is
kept, the decision is recorded as `close deferred: <reason>`, the bar is not consumed, and it is
retried next tick. It is visible in `GET /paper/status`.

**A deferral can persist indefinitely offline.** That is correct. An open position with a truthful
"could not price the exit" is a better book than a closed one at an invented price: the deferral is
recoverable, a fabricated P&L is not.

### 3.6 Crash windows in reconciliation *(added in 1.1.0)*

Two failure modes that both end in a live paper trade nobody reconciles:

- **Duplicate open.** `journalCreate` returning and engine state being persisted were two separate
  moments; a crash between them left a trade the engine had no memory of, and the next bar opened a
  second one. State is now persisted **immediately** after `journalCreate` returns, and before any
  open the engine checks the journal's open `mode="paper"` trades for an **orphan** matching this
  config and **adopts** it instead of creating a second. Adoption is conservative in both directions:
  only when this config is flat, only a trade no other config's state owns, matched on
  `attachedSignal.horizon` when present, and — for a legacy trade without one — only when exactly one
  configured `(ticker, timeframe)` could have produced it. An unattributable orphan is left alone and
  logged, never guessed at.
- **"Gone" confused with "unreachable".** A failed close used to drop the engine's bookkeeping
  unconditionally. A `404`/`410` means the trade really is not there (deleted, or an external reset)
  and dropping is right; **a timeout or a 5xx means the journal is temporarily unreachable and the
  position is still open**, so the position is KEPT, a non-consuming decision is recorded, and the
  close is retried next tick. A temporary journal blip must never orphan a live paper trade.

### 3.3 Costs — recorded here, ACCOUNTED in §5 *(superseded 2026-08-23)*

`attachedSignal.costBps` records the cost assumption the model's backtest was run under, served
straight from the record (`report.costBps`). It is still recorded exactly as described, and it is now
also **spent**: §5.2 charges that same `cost_bps` on the traded notional at every fill in the ledger,
and freezes it onto the lot so a later retrain cannot re-price a fee already charged.

*What this paragraph used to say, and no longer does:* "Actual fee and slippage accounting in live
paper stats is deliberately out of scope here… live and backtest expectancy are not directly
comparable." That was true of the JOURNAL's raw entry-to-exit P&L, which is unchanged and is still
gross of fees — the per-episode counting stats in the comparison payload still carry that caveat. It
is **no longer true of the book**: the ledger's daily portfolio return is net of every fee it paid,
and §5.4 says exactly which pairs are now like-for-like and which are not.

### 3.4 Units — see §5.4 *(superseded 2026-08-23)*

The table that stood here said that only hit rate was close to apples-to-apples and that unifying the
units was priority-2 work. **That work is §5.** The comparison payload still serves a `units` block
rather than prose nobody reads, and it still labels every field — but three pairs are now genuinely
like-for-like (daily portfolio Sharpe, expectancy, drawdown) and the per-trade columns are labelled
as the counting stats they are. The current table is **§5.4**; this heading is kept so nothing links
to a section that vanished.

---

## 4. Gates

All of the following must hold before the engine may **open** or **flip into** a position. Closing an
existing position to flat on an explicit signal is always allowed — a gate must never be able to trap
an open position. Evaluation is fail-closed in every direction: a missing file, an unparseable field
or an unreachable service **refuses**, and the refusal reason is carried in the status payload.

| # | Gate | Requirement |
|---|---|---|
| 1 | **No synthetic anywhere, and no UNKNOWN anywhere** | the quote's `source` is non-empty and `!= "synthetic"`; the latest bar's `source` is non-empty and its `sourceIsSynthetic == false`; the record's `trainedOnSynthetic == false`; the current feature frame not synthetic (`/predict`'s `currentData.synthetic`) |
| 2 | **Fresh data** | the latest bar is at most `PAPER_MAX_BAR_AGE_SESSIONS` sessions (default 3) behind today, **and** the record's `dataThrough` is at most `PAPER_MAX_MODEL_AGE_SESSIONS` sessions (default 10) behind the latest bar |
| 3 | **Passing backtest** | `report.passed == true` |
| 4 | **Evaluator verdict** | a persisted verdict of exactly `EDGE` exists for this (ticker, timeframe, horizon), and its `strategyVersion` equals the prediction service's live one |

Sessions are approximated **conservatively** by counting weekdays, which over-counts across holidays —
i.e. it declares data stale slightly early. That is the correct direction to be wrong in.

### 4.0 An empty provenance is not a clean one *(added in 1.1.0)*

Gate 1 used to refuse only the **literal string** `"synthetic"`. Anything else passed — including an
empty `source`, which is not a clean provenance but the absence of one. A gate whose whole claim is
that it fails closed must not have "unknown" as its default-pass case. A **non-empty, non-synthetic**
source still passes: this narrows nothing except the unknown case.

### 4.1 Synthetic is REJECTED, not marked

Previously the engine opened on a synthetic quote or a synthetically-trained model and set an
`EntrySynthetic` flag. That is a label on a lie. When this rule landed, shipped records demonstrated
the danger: synthetic-trained models could still carry `passed: true`. `EntrySynthetic` remains in
the persisted state for legacy display of positions opened under the old rule; **it is unreachable
for new opens.**

### 4.2 `report.passed` is necessary, no longer sufficient

`passed` is a per-model, single-ticker, in-pipeline check (`backtest.is_passing`: positive
expectancy, `0 < Sharpe ≤ 3`, ≥30 trades). It is a floor, not a finding. The engine used to trade on
it alone; it now also requires gate 4.

### 4.3 The evaluator verdict

`services/prediction/app/evaluate.py` is the honest Stage-1 test: it pools out-of-sample trades across
a universe of ~30 liquid names, refuses outright on synthetic data, subtracts costs, reserves an
untouched holdout, and compares against both buy-and-hold and a permutation null before granting
`EDGE`. The engine never consulted it. Now it must.

- The evaluator writes, alongside its report, one verdict record per evaluated
  `(ticker, timeframe, horizon)` to `prediction.verdicts` (the no-database fallback is
  `{EVAL_OUT_DIR}/verdicts/{TICKER}_{TF}_{H}.json`), carrying the
  **pooled** verdict, `evaluatedAt`, the source report filename, and the `strategyVersion` of the run.
  The verdict is pooled across the whole universe — the record says so in its `scope` field, and
  storing it per config means "this config is covered by that pooled run", never "this ticker was
  judged on its own".
- `strategy_version(cost_bps, upper, lower, allow_short)` (`services/prediction/app/strategy.py`) is
  a stable hash over two groups of constituent: the **code** identity (`EXECUTION_CONTRACT_VERSION`,
  `MODEL_FEATURES`, `LGB_PARAMS`) and the **parameter** identity (the cost assumption, both
  thresholds, and `allowShort`). Change any of them and every stored verdict goes `current: false` —
  because it was made about a strategy that no longer exists. **`EXECUTION_CONTRACT_VERSION` names
  this document**: revising the rule in §1, §2 or §3 invalidates existing verdicts by construction,
  which is the point.

  **Every parameter is an ARGUMENT, not a default, and `allowShort` is one of them** *(1.1.0)*. The
  function used to hash the module defaults and omit `allowShort` entirely, while `evaluate.py`'s
  `EvalConfig` reads `EVAL_COST_BPS`, `EVAL_UPPER`, `EVAL_LOWER` and `EVAL_ALLOW_SHORT` and derives
  every position it judges from them. A run at 0 bps, 0.90/0.10, shorting enabled therefore wrote a
  verdict **stamped as the default 6 bps long-only strategy**, and `/predict` served it
  `current: true` — a verdict about one strategy, spent by another. `allowShort` belongs in the hash
  because it changes the POSITIONS: at the same probabilities, a long-only run is flat where a
  shorting run is `-1`. There is no zero-argument form; `default_strategy_version()` exists for the
  one caller (`/health`) that genuinely means "this service's defaults" and says so in its name.

- **`current` is computed per SERVED RECORD, from that record's own report.** `/train` accepts
  `allowShort` per record, so "the service's defaults" is not a fact about any particular record. The
  expected version comes from `report.thresholds`, `report.costBps` and `report.allowShort`; a record
  missing any of them is `current: false` — an identity that cannot be computed is not an identity
  that matches. The verdict record also carries the evaluator's **methodology tag** (currently
  `"method": "portfolio-v4"`) and the completed-bar data-policy identity; `current` requires both,
  so evidence produced by a superseded methodology or data frame can never be spent.

- **The verdict is judged PER HORIZON.** Two horizons of one ticker are two views of the same
  underlying returns; pooling them into one number counts every market move twice. Each label horizon
  is evaluated as its own portfolio and gets its own verdict, and the verdict file for
  `(ticker, timeframe, horizon)` carries that horizon's answer. The report's headline verdict is the
  **most conservative** across horizons and can never read more permissive than any of them.
- `/predict` and `/backtest` serve an `evaluation` block —
  `{verdict, evaluatedAt, strategyVersion, current}` — or `null` when no verdict file exists.
- **No evaluator gate was loosened and no verdict logic was changed** to make this work.

### 4.4 What this means today, and why it is correct

**At the latest production audit no `EDGE` verdict existed.** The persisted result was `NO EDGE`, so
gate 4 correctly refused the configured tickers. Deploying this completed-bar correction also makes
every pre-v4 verdict and every unstamped model stale; each intended config must be retrained and the
pooled evaluator rerun before launch readiness can become green.

**The engine trades nothing. That is the intended, correct behaviour** — and it is *visible*, per
config, in `GET /paper/status` and in each comparison row, never silent. Producing an `EDGE` verdict
is an **operator action on real data** (run `python -m app.evaluate` against live providers), not a
code change. Do not manufacture a verdict file, relax `evaluate.py`, or add a bypass flag; each of
those converts "we have not validated this" into "we have", which is the one failure mode this whole
service exists to prevent.

---

## 5. Accounting — how both sides keep score

**Added 2026-08-23. `EXECUTION_CONTRACT_VERSION` is NOT bumped by this section, deliberately.**
§§1–3 state the trading RULE — what position is taken, when, and at what price. This section states
how the RESULT of that rule is counted. A verdict from `evaluate.py` is a statement about the rule,
and the rule has not changed: the same bars produce the same positions at the same thresholds under
the same costs. Bumping the version would invalidate every stored verdict over a non-change, which
is worse than useless — it would train the next person to treat a stale-verdict signal as noise.
The rule is versioned; the bookkeeping is not.

Until this section existed the two sides kept score differently and the comparison could only be a
disclaimer. Live P&L was the journal's raw entry-to-exit move on a fixed $10,000 notional, gross of
fees; live Sharpe was per-TRADE and un-annualized; the evaluator judged a date-aligned equal-weight
1/N portfolio (`evaluate.portfolio_returns`). None of those numbers was wrong. They were answers to
different questions, so "live diverges from backtest" could not be said, or denied, honestly.

### 5.1 The accounting atom

Implemented by `backtest.net_returns` and **unchanged** by this section:

```
prev[t]     = positions[t-1]                     (0 before the first bar)
turnover[t] = |positions[t] - prev[t]|
net[t]      = positions[t] * ret_next[t] - (cost_bps/10000) * turnover[t]
equity      = cumprod(1 + net)
a "trade"   = a transition INTO a nonzero position   (§1.2)
```

This formula is **normative**, and it is the one thing Go and Python are held to identically. They
cannot share code, so they share **fixtures**: `testdata/accounting/*.json` state inputs and expected
outputs to fixed precision, and both suites consume them —
`services/prediction/tests/test_accounting_fixtures.py` against `net_returns`/`run_backtest`, and
`paper/accounting_fixtures_test.go` against the ledger. The fixtures were **not** generated by either
implementation (that would enshrine its bugs as truth); one scenario is hand-computed in its own
`handComputation` block so neither program is the authority.

### 5.2 The live portfolio mapping

**One book**, opening balance `PAPER_STARTING_CASH` (simulated money; there is no account, no broker
and no balance anybody can withdraw). Equal-weight **1/N** across the N enabled configs, mirroring
the evaluator's portfolio definition:

- A new position's notional is **`equity_at_entry / N`**, where N is the enabled config count **at
  that moment**. N changes when `/paper/config` is rewritten; the fill records the N it was sized
  against, because a position opened under three configs still holds a third of the book after a
  fourth is added.
- **No intra-hold rebalancing.** The backtest charges turnover only on position *changes*, and so
  does the ledger. Rebalancing an open position would be turnover nothing was validated under.
- **Fees**: the SAME `cost_bps` the config's model was validated under (`report.costBps`, already
  recorded on the trade at entry per §3.3), charged on the **notional actually traded** at every
  fill — entries, exits, and **both legs of a flip**. The rate is frozen onto the lot at entry, so a
  later retrain at a different cost cannot retroactively re-price a fee already charged.
- **The identity**: `equity == startingCash + realized + unrealized` at all times, where
  `unrealized` is net of the entry fee already paid. An engine whose components do not sum to its own
  equity is not one; it is asserted in the tests.

There is **no fixed-notional fallback**. If the ledger cannot be initialized, the service remains
available for diagnosis but every position change is blocked. A book sized by a different rule is
not an out-of-sample test of the evaluator's portfolio.

### 5.3 Marking

One equity snapshot per **date**, at each new **completed** bar, marked at **that bar's close** —
never at a quote. A quote is an execution price observed at some other moment; a book marked at one
is not reproducible from the bar series anybody else can fetch.

- A snapshot is written only when **every enabled config** has a real mark for that date. A book
  valued on a partial set of closes is valued on a portfolio nobody held.
- **A synthetic or missing bar produces NO snapshot — a recorded GAP**, surfaced as `gapDates`. A gap
  is the absence of a measurement, never a zero and never the previous close carried forward.
- A date the book moves past while still incomplete becomes a gap rather than pending forever.
- **Marking happens BEFORE the bar's reconciliation**, so a snapshot for date *t* values the position
  that was held over bar *t-1* — which is exactly what the atom's `equity[t-1]` is. Marking after
  reconciling would fold bar *t*'s fee into bar *t-1*'s return.
- **THE DAILY SNAPSHOT IS THE PRE-TRADE MARK OF THAT BAR'S CLOSE, AND IT DOES NOT DEPEND ON CONFIG
  ORDER** *(clarified 2026-08-25)*. The engine's tick is **two-phase**: phase 1 submits every enabled
  config's mark and trades nothing; phase 2 runs every config's decision. A date's snapshot settles
  on the mark that completes it — which is now always in phase 1 — so no fill from that tick can land
  inside it, whichever order `CONFIGS` happens to be written in.

  This clarifies the accounting, not the trading rule, so `EXECUTION_CONTRACT_VERSION` is **not**
  bumped (see the header note on §5). It is written down because the previous implementation
  violated it silently: the engine marked and then reconciled each config in turn, so the snapshot
  was taken after every earlier config had traded that bar and before every later one had, and
  reordering `CONFIGS` produced a different equity curve from identical market data. For a system
  whose primary output is that curve, a measurement that depends on a configuration string is not a
  measurement. `paper/reconcile_test.go::TestSnapshotsAreIdenticalUnderEitherConfigOrder` runs one
  fixture tick under both orders and requires byte-identical snapshots.

  **What is still order-dependent, and is not this:** within phase 2, each new position takes
  `equity/N` from the equity the *previous* fill left, so two configs opening in the same tick get
  slightly different notionals depending on who went first. That is a property of §5.2's sequential
  `equity_at_entry` sizing, not of the snapshot, and changing it would change §5.2's accounting. It
  is recorded in `GAPS.md` rather than fixed here.

### 5.4 The comparability table

| statistic | live paper | reference | comparable? |
|---|---|---|---|
| **daily portfolio Sharpe** | annualized (×√252) from the book's daily snapshot returns; **`null` below 20 snapshot days** | the evaluator's date-aligned 1/N portfolio Sharpe | **yes** — same statistic, same kind of series |
| **expectancy** | mean **net** daily portfolio return, after fees on every fill | `backtest.expectancy` — mean net return per bar | **yes** for a 1D config |
| **max drawdown** | largest peak-to-trough decline of the book's equity curve | `backtest.maxDrawdown` on the backtest equity curve | **yes** |
| hit rate | share of closed **episodes** with `pnlPct > 0` | share of **bars** whose position matched the next bar's sign | no — different denominators |
| per-trade expectancy | mean % move per closed episode, **gross of fees** | — | no — a counting stat |
| trade count | closed position episodes | `numTrades` | yes — both are episodes (§1.2) |

The **counting** stats (hit rate, per-episode expectancy, trade counts) stay per-decision and stay
labelled. They come from the journal's raw entry-to-exit move and are gross of fees. Those caveats
survive this change because they are still true; only the ones this change made false were removed.

**Two caveats on the Sharpe row, both real:**

1. The evaluator's portfolio Sharpe is **pooled across ~30 names**. It says what the strategy did
   across the universe, not what it did on the configs this book trades.
2. `verdicts.evaluation_block` serves the verdict and the report's *filename*, not its statistics,
   and `paper/` has no access to the evaluator's output directory. So the comparison currently falls
   back to **the model backtest's annualized per-BAR Sharpe with the unit caveat kept**, and the
   payload's `portfolio.reference.source` says which number it used. That is the honest second-best,
   not a silent substitution.

### 5.5 How the ledger is held to the atom

`paper/` does not get a Go copy of `net_returns`. A second translation of one formula agreeing with
the first proves nothing about the book. Instead the Go suite drives a **real ledger** bar by bar and
re-derives the per-bar net-return series from the fills and marks the ledger itself wrote down
(`paper/accounting.go`):

```
net[t] = (position P&L over bar t) / (position market value at the START of bar t)
         - SUM over the bar's fills of (fee / traded notional)
```

Both terms are exact: the first is `pos[t] * ret_next[t]` because the denominator is the *current*
market value rather than the entry notional, and the second is `cost_bps/10000` per LEG, where the
leg count **is** the turnover (1 open, 1 close, 2 flip, 0 hold). A fee booked at the wrong bar, a
missing leg of a flip, a lot sized against the wrong equity or marked at the wrong price all break
the derivation. **If the ledger cannot reproduce the atom from its own bookkeeping, the ledger is
wrong — not the fixture.**

### 5.6 Where the dollar book and the atom legitimately differ

The derived **return** series is the atom exactly. The book's **dollar equity curve** is not a
rescaling of it, and this document does not claim it is.

Compounding the atom implies the position is rebalanced back to 1× exposure at every bar. The book
does not rebalance — §5.2 forbids it, because a real book cannot rebalance for free. So it holds a
**fixed share count** through an episode and its exposure **drifts**: a short that gains becomes a
smaller fraction of a larger book. Written out exactly:

```
E[t+1]/E[t] - 1  ==  w[t]*r[t] - fees[t]/E[t]
```

where `w[t]` is the signed exposure fraction actually carried into bar *t*. That is the atom with
`pos[t]` replaced by the exposure that really existed. The drift starts at **zero**: on the bar a
position is opened from flat, `w[t] == pos[t]` and `fees[t]/E[t] == cost_bps/10000 × turnover[t]`, so
that bar's return *is* `net[t]` to the last decimal.

Both statements are asserted exactly in `paper/accounting_fixtures_test.go`
(`TestTheBooksDailyReturnsAreExposureTimesTheAtom`, `TestAtEveryEntryBarTheBookIsTheAtomExactly`).
The drift is **first-order in the return, not in the cost** — an earlier draft bounded it by
`cost_bps × movement` and the short fixture exceeded that by an order of magnitude. It is therefore
stated as an identity rather than as a bound: a bound that has to be widened until the test passes is
not a measurement.

### 5.7 Durability

`paper.fills` and `paper.snapshots` are append-only within a ledger generation;
`paper.documents` (`ledger_state`) is the fast-path cache. **The fill row is committed BEFORE the
state is mutated**, so a crash in that window leaves a fill the state has not applied — which
`openLedger` detects (`lastFillSeq` is behind) and repairs by replaying. Every field needed to replay
a fill is on the fill record. The no-database local/test fallback preserves the equivalent
`fills.jsonl` / `snapshots.jsonl` / `ledger.json` protocol.

`POST /paper/reset` starts a new generation and records the transition in `paper.resets`; prior
fills and snapshots remain queryable under their previous generation. A reset is an operator action
on a validation book, and the record of what it did before reset is never deleted.

### 5.9 The three stores, and what happens when they disagree *(added 2026-08-25)*

One simulated position is recorded in three places: the **journal** (`mode="paper"` trades — what was
decided), the **ledger** (the fill, its fee, its lot), and the **engine's own state** (the bar cursor
and the side it believes it holds). `openPosition` writes the journal trade first and books the
ledger fill second, so that the fill can carry the trade id it belongs to — which means a ledger
failure leaves a position the journal holds and the book does not.

Before this section, that failure was a log line claiming "the book and the engine now disagree and
`/paper/status` says so". **`/paper/status` did not say so**: nothing compared the three stores, the
lost fill was never retried, and the engine kept trading a config whose book was already wrong.

1. **Durable pending bookings.** Before an open is offered to the ledger, its exact intent is
   persisted in the engine's state file — config, kind, side, trade id, price, bar, `N`, `costBps`,
   quantity and notional — and cleared only after the ledger accepts it. A failed fill is retried at
   the **start of every tick** from those original values, never from current equity/N. Booking is **idempotent by
   `(trade id, kind)`**: the ledger refuses a second fill for a pair it already holds, and that set
   is rebuilt by the replay in §5.7, so a crash between the ledger accepting a retry and the pending
   record being cleared cannot double-book.
2. **Reconciliation, at startup and once per tick.** Engine side/trade id vs the ledger's lot vs the
   journal's open paper trades. An **unreachable journal, missing ledger, or non-durable engine
   state is a blocking mismatch**, never agreement. The mirror case — the journal holds an open trade a *flat* config could own — is the
   ORPHAN of §3.6 and is left to `adoptOrphan`, which is its designed repair.
3. **A desynced config refuses to change its position**, surfaced exactly like a gate refusal with
   the mismatch named ("the engine holds trade X but the ledger's lot is trade Y"). **Marking and
   snapshots continue** — a book that stopped measuring because its bookkeeping disagreed would lose
   exactly the days somebody will want to read afterwards. The bar is **not** consumed, so once the
   stores agree the decision can still be acted on within the same bar.
4. **The book catches up to a position it never booked only from exact evidence.** An orphan trade
   written by the current engine carries quantity, notional, N-at-entry and validated cost on the
   journal record, so it can be adopted and replayed exactly. A legacy position missing any of that
   evidence stays desynced for reset or operator repair; current equity/N is never used to invent
   its historical fill. Catch-up fires **only** when the
   `(trade id, open)` fill has never been booked, so a lot that was opened, booked and then closed
   can never be re-opened by it; that case stays a desync for an operator to resolve.

`GET /paper/status` carries a `sync` block per config — `{consistent, pendingBookings, detail,
mismatches, journalChecked, ledgerChecked}` — plus a run-level `reconciliation` roll-up.

### 5.10 Settled-decision evidence *(added 2026-08-26)*

`paper.decision_events` is the append-only explanation of what the engine decided on each consumed
completed bar. Its idempotency key is `(experiment generation, config, bar unix)`. The event and the
engine state containing the advanced cursor are one PostgreSQL transaction, so neither can claim a
bar the other did not consume. Model and strategy versions travel with the event when `/predict`
was available.

An upstream or synchronization failure with `advance=false` is an operational retry, not another
observation. It remains the config's current `lastDecision` but is not appended to the experiment
history. `GET /paper/dashboard` keeps those two meanings separate and refuses to compose across a
generation change.

### 5.11 Experimental shadow evidence *(added 2026-08-31; does not change the official rule)*

The official book remains fail-closed behind every gate in §4. Separately, the paper service stores
the **first** Buy/Sell/Hold response and contemporaneous quote it observes for each config and
completed bar under `experimental-shadow-v1`, even when the evaluator verdict is `NO EDGE`. This is
forward evidence, not permission to trade and not a second way around a gate.

The observation key is `(contract_version, config, signal_bar_unix)`. Retries use `ON CONFLICT DO
NOTHING`; they cannot replace the first quote with a later, more favourable one. Completed real bars
settle immutable outcomes after 1, 3, 5 and 10 bars. Buy and Sell episodes are scored directionally
and pay one entry plus one exit cost at the record's own `costBps`; Hold remains flat and receives no
subjective correctness label. A source-less, synthetic, missing, or temporally invalid entry/exit is
retained but excluded from metrics.

Every episode uses a fixed `$10,000` explanatory notional. Episodes may overlap, so their P&L is an
inspectable per-call counterfactual, **not** a portfolio balance. The official equal-weight ledger in
§5.2 remains the only account-like simulation comparable with the evaluator. Official resets and
promotion actions never delete, rewrite, unlock, or reinterpret shadow evidence.

### 5.8 What the ledger does NOT do

It **creates nothing to score**. It records the engine's decisions and marks the result; it opens no
position, satisfies no gate, and cannot cause a trade. All four gates of §4 still refuse every
shipped config today, so the book holds nothing, and its equity is its opening balance. That is the
correct state, and `GET /paper/ledger` says so rather than presenting an empty book as a flat month.

One coupling remains and is stated rather than hidden: **a position the journal refuses to record is
a position the engine does not take** (§3), so with no `AUTH_SECRET` the engine opens nothing and the
book books no fills. The ledger itself is unaffected — it initializes, marks every real bar close,
snapshots, and serves its statistics with no credential of any kind — and `book.records` reports the
two independently, so a dead journal recorder can never read as "no signals fired". Decoupling the
*position* from the journal write would be a change to §3 and would require a version bump.

---

## 6. What would falsify comparability

Any of these means the live/backtest comparison has stopped being a comparison, and the fix is to
repair the divergence — not to reword the note above the table:

- `paper/engine.go` holds a position across a bar on which the target changed, or exits a position for
  any reason other than a changed target (a timer, a horizon count, a stop, an age). *(A deferred
  close under §3.5 is not an exception: the target still governs, the fill is merely not yet
  priceable, and the decision is recorded as deferred rather than silently skipped.)*
- `paper/engine.go` acts on a bar that has not closed, or fills at a quote whose provenance is unknown
  or whose `asOf` predates the bar it reconciles.
- `backtest.py` acquires an exit rule, a holding period, or a payoff other than the next bar's
  close-to-close return.
- The two disagree about what a trade is (§1.2) — e.g. one counts every held bar.
- The two disagree about thresholds, `allowShort`, or cost treatment for a given record.
- The paper engine acts on something the gate never validated: a different feature lag, a different
  probability (calibrated instead of raw — direction stays on the **raw** probability, per
  `main.predict`), or a model record other than the one the verdict covers.
- This document changes without both implementations changing with it, or either implementation
  changes without `EXECUTION_CONTRACT_VERSION` being bumped.

The comparison table is allowed to say "live diverges from backtest". It is not allowed to say that
when the real answer is "these two programs are running different strategies".
