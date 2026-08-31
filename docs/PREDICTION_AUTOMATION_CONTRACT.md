# Prediction automation contract

This contract defines the narrow production controller in
`services/prediction/app/autonomy.py`. It automates candidate qualification mechanics; it does not
automate model deployment or permission to trade.

## 1. Trigger

The supervised process polls because it must notice new data, but wall-clock time never creates a
trial. A config becomes eligible only after `PREDICTION_AUTOMATION_MIN_NEW_BARS` (default 5) newer
completed bars exist after the latest active/candidate `dataThrough` cutoff. The feature endpoint is
called with `completedOnly=true`. A synthetic, empty, or unparseable frame refuses the config.

`PREDICTION_AUTOMATION_CONFIGS` is the exact config set. When empty, `PAPER_CONFIGS` is used. The
controller is off unless `PREDICTION_AUTOMATION_ENABLED=true`.

## 2. Bounded trials

One PostgreSQL lease serializes controller passes. A unique
`(ticker,timeframe,horizon,trigger_bar)` reservation prevents duplicate training after a restart or
replica race. Every reservation becomes a durable success or failure row.

Training invokes the existing `/train/{ticker}` path. It inherits `allowShort` from the active
model and supplies no threshold or cost override. Training creates an immutable candidate and never
changes `/predict`'s active pointer.

`PREDICTION_AUTOMATION_MAX_TRIALS` defaults to 3 per config. Failed attempts spend the budget too.
This is deliberate protection against repeatedly fitting/evaluating until chance produces a passing
result. Raising or resetting that budget is an operator decision and must not happen automatically.

## 3. Evaluation

After one or more candidates are created, the controller starts one batch through the existing
parameter-free `POST /evaluate/run`. The runner's global single-flight lock remains authoritative;
if a manual price/PEAD run holds it, the candidates wait. The controller captures the exact run and
per-config verdict onto each trial. `NO EDGE`, `INCONCLUSIVE`, and evaluator refusals are evidence,
not reasons to retry with different thresholds.

## 4. Prospective paired shadow

After evaluation finishes—even when its honest result is `NO EDGE`, `INCONCLUSIVE`, or an
infrastructure refusal—the frozen challenger and the frozen active model may enter research-only
shadowing. Neither version is served from a new pointer and neither can write the official paper
book.

On each new completed real bar, both versions score the same lagged feature row. The prior pair is
then settled against the same next-bar close. Each side pays the `costBps` from its own immutable
record on its own target turnover. The observation insert and prior settlement are one PostgreSQL
transaction and `(trial_id, bar_time)` is idempotent.

Results remain `unmeasured` below `PREDICTION_SHADOW_MIN_PAIRED_BARS` (default 20). Reaching the
floor records only `candidate-ahead`, `champion-ahead`, or `tied` on compounded paired net returns.
None of those labels is an `EDGE` verdict or permission to promote. A trial stops at the floor so an
operator reviews a predeclared window rather than waiting until a preferred result appears.

## 5. Hard boundaries

The controller cannot:

- promote or roll back a model;
- change a strategy threshold, cost, shorting policy, universe, or evaluator parameter;
- invoke Qwen or any LLM;
- write to the official paper ledger, start/reset its clock, or place any order;
- train from synthetic or still-forming bars;
- create more than the fixed trial budget.

Promotion remains the authenticated, audited operator route. Shadow evidence cannot move the active
pointer and is not currently a promotion gate; the first production cycles remain human-reviewed.

## 6. Operations

`GET /automation/status` on the prediction service returns the controller lease, policy, trial
ledger and paired shadow summary. Signed-in operators use the read-only gateway route
`GET /api/prediction-automation/status` or **Settings → Preferences → Challenger automation**. None
of those reads starts work.

The generic stack remains zero-configuration: with the flag false, the supervised process exits
successfully and the prediction API behaves exactly as before. PostgreSQL is required when the
controller is enabled; file fallback is intentionally not treated as durable experiment state.
