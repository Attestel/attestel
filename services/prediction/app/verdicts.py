"""Persisted evaluator verdicts — the bridge from the offline harness to the live services.

`app.evaluate` produces the honest Stage-1 answer (EDGE / NO EDGE / INCONCLUSIVE / SUSPECT) by
pooling out-of-sample trades across a whole universe. Until now that answer lived only in a
timestamped report nothing read: the paper engine traded on the per-model `report.passed` flag and
never learned that the pooled evaluation said NO EDGE.

This module persists the verdict in a shape a service can look up — one record per evaluated
(ticker, timeframe, horizon) — and serves it as the `evaluation` block on /predict and /backtest.

IT DECIDES NOTHING. The verdict logic lives in `evaluate.decide_verdict` and is untouched; this only
writes down what that returned and hands it back. A missing record is `None` — never a default, never
an optimistic guess. Fail-closed is the caller's job and every caller does it.

THE VERDICT IS POOLED ACROSS THE UNIVERSE, AND JUDGED PER HORIZON. The harness answers "is there an
edge in this strategy across the universe, at this label horizon" — not "does NVDA have an edge".
Storing it per (ticker, timeframe, horizon) records WHICH configs that pooled run covered; the
`scope` field says so on every record, so nobody later reads a per-ticker claim into it.

WHAT `current` MEANS, AND WHY IT IS COMPUTED PER RECORD. A verdict may be spent only by the strategy
it was made about. Five things have to match:

  * the STRATEGY VERSION, recomputed from the SERVED RECORD'S OWN parameters — `report.thresholds`,
    `report.costBps`, `report.allowShort` — never from module defaults. `/train` accepts `allowShort`
    per record and the evaluator reads EVAL_* overrides, so "the service's defaults" is not a fact
    about any particular record. A record missing any of those fields is `current: false`: an
    identity we cannot compute is not an identity that matches.
  * the METHOD tag: a verdict produced by a superseded evaluation methodology (see
    `EVALUATION_METHOD`) is about numbers this repo no longer computes, so it may not be spent.
  * the stored SUFFICIENCY thresholds and actual sample both clear the hard live evidence floors.
    Evaluator environment knobs may make a run stricter, never weaker than live spendability.
  * the DATA POLICY stamped on both the evaluator verdict and the served model. Both must name the
    current completed-bar frame; a current model cannot launder an old unstamped verdict.
  * the record actually existing and parsing, which is the pre-existing fail-closed behaviour.

Production: one row in ``prediction.verdicts``. File fallback:
{EVAL_OUT_DIR}/verdicts/{TICKER}_{TF}_{H}.json.
"""
from __future__ import annotations

import json
import math
import os

from .config import EVAL_OUT_DIR
from . import strategy as _strategy
from .features import FEATURE_FRAME_POLICY
from . import db

VERDICTS_DIRNAME = "verdicts"

# What the paper engine is allowed to trade on (contract §4, gate 4). Everything else — including
# INCONCLUSIVE and a missing verdict — refuses.
TRADEABLE_VERDICT = "EDGE"

# The evaluator's POOLING METHODOLOGY, stamped on every report and every verdict record.
#
# "portfolio-v2" was the date-aligned equal-weight portfolio (`evaluate.portfolio_returns`). It
# replaced "concat-v1", which spliced every (ticker, horizon) net-return stream end to end and
# computed Sharpe, drawdown, equity and the permutation null on the splice — treating the same
# trading day across 30 correlated names as 30 independent sequential observations and drawing an
# equity curve of a series that never existed. Those numbers cannot be compared with these, so a
# verdict carrying the old tag (or no tag) is not `current` under this one.
#
# "portfolio-v3" (2026-08-25) keeps that pooling math EXACTLY and adds SAMPLE-SUFFICIENCY GATES to
# `evaluate.decide_verdict`: minimum portfolio dates, minimum holdout dates, minimum evaluated
# streams, and minimum ticker coverage — with failed and skipped tickers counting against coverage
# and named in the checklist. Every one of them can only produce INCONCLUSIVE; none is a new route
# to EDGE.
#
# THE TAG IS BUMPED ANYWAY, AND THAT IS THE POINT. `strategy_version()` is untouched — the strategy
# did not change, so a verdict minted under v2 is about the same rule. But it was minted under
# WEAKER SUFFICIENCY RULES: it could rest on forty portfolio dates and four of thirty configured
# tickers and say nothing about either. Leaving it `current` would let exactly the verdicts these
# gates exist to stop be SPENT by the paper engine's gate 4. Bumping the method tag makes every
# stored v2 verdict un-spendable until it is re-earned under the new floors.
# "portfolio-v4" (2026-08-25) additionally requires the analysis feature frame to contain COMPLETED
# bars only. v3 could include today's still-forming daily candle when run during the session, while
# the paper engine correctly refused to act on that candle. No v3 verdict may be spent by the
# corrected pipeline.
EVALUATION_METHOD = "portfolio-v4"

# Non-configurable spendability policy. Evaluator knobs may be raised for a stricter experiment,
# but lowering them cannot mint an EDGE the live paper gate accepts. Both the thresholds used by
# the run and the observations it actually collected must meet these floors.
EVIDENCE_FLOORS = {
    "minDates": 252,
    "minHoldoutDates": 60,
    "minTickers": 10,
    "minCoverage": 0.70,
}


def evidence_issues(sufficiency: dict | None) -> list[str]:
    """Why a stored evidence block is not spendable under the hard policy."""
    if not isinstance(sufficiency, dict):
        return ["the verdict carries no sample-sufficiency evidence"]

    checks = (
        ("minDates", EVIDENCE_FLOORS["minDates"]),
        ("nDates", EVIDENCE_FLOORS["minDates"]),
        ("minHoldoutDates", EVIDENCE_FLOORS["minHoldoutDates"]),
        ("holdoutDates", EVIDENCE_FLOORS["minHoldoutDates"]),
        ("minTickers", EVIDENCE_FLOORS["minTickers"]),
        ("nStreams", EVIDENCE_FLOORS["minTickers"]),
        ("minCoverage", EVIDENCE_FLOORS["minCoverage"]),
        ("coverage", EVIDENCE_FLOORS["minCoverage"]),
    )
    out: list[str] = []
    for name, floor in checks:
        try:
            value = float(sufficiency[name])
        except (KeyError, TypeError, ValueError):
            out.append(f"{name} is missing or not numeric")
            continue
        if not math.isfinite(value):
            out.append(f"{name} is not finite")
        elif value < floor:
            out.append(f"{name}={value:g} is below the hard floor {floor:g}")

    try:
        configured = int(sufficiency["configuredTickers"])
        streams = int(sufficiency["nStreams"])
        coverage = float(sufficiency["coverage"])
        if not math.isfinite(coverage):
            out.append("coverage is not finite")
        elif configured < streams or configured <= 0:
            out.append("configuredTickers is inconsistent with nStreams")
        elif abs(coverage - streams / configured) > 0.00011:
            out.append("coverage is inconsistent with nStreams/configuredTickers")
    except (KeyError, TypeError, ValueError, ZeroDivisionError):
        out.append("ticker coverage inputs are incomplete")
    return out


def _key(ticker: str, timeframe: str, horizon: int) -> str:
    return f"{ticker.upper()}_{timeframe}_{horizon}"


def verdict_dir(out_dir: str | None = None) -> str:
    return os.path.join(out_dir or EVAL_OUT_DIR, VERDICTS_DIRNAME)


def verdict_path(ticker: str, timeframe: str, horizon: int, out_dir: str | None = None) -> str:
    return os.path.join(verdict_dir(out_dir), _key(ticker, timeframe, horizon) + ".json")


def expected_strategy_version(report: dict | None) -> str | None:
    """The strategy version a stored verdict must carry to be spendable by THIS record.

    Computed from the record's own report — the thresholds, cost and shorting flag its positions
    were actually derived under. Returns None (=> `current: false`, fail closed) when any of them is
    missing or unreadable: a record that does not say what it was validated under cannot be matched
    against a verdict that does.
    """
    if not isinstance(report, dict):
        return None
    thresholds = report.get("thresholds")
    cost_bps = report.get("costBps")
    allow_short = report.get("allowShort")
    if not isinstance(thresholds, dict) or cost_bps is None or not isinstance(allow_short, bool):
        return None
    upper, lower = thresholds.get("upper"), thresholds.get("lower")
    if upper is None or lower is None:
        return None
    try:
        return _strategy.strategy_version(float(cost_bps), float(upper), float(lower), allow_short)
    except (TypeError, ValueError):
        return None


def write_verdict(
    ticker: str,
    timeframe: str,
    horizon: int,
    *,
    verdict: str,
    evaluated_at: str,
    report_file: str,
    strategy_version: str,
    out_dir: str | None = None,
    scope: str = "pooled-universe",
    method: str = EVALUATION_METHOD,
    sufficiency: dict | None = None,
    data_policy: str | None = None,
) -> str:
    """Persist one verdict record. Returns the path written.

    `strategy_version` is passed IN by the caller and must be the one computed from the evaluation
    run's OWN config (`evaluate._write_verdicts`), not the service defaults — a custom run may never
    masquerade as the default strategy. `method` names the pooling methodology the numbers behind
    this verdict were produced by. `data_policy` deliberately defaults to None: only a caller that
    explicitly knows which feature frame it evaluated may mint current evidence.
    """
    path = verdict_path(ticker, timeframe, horizon, out_dir)
    record = {
        "ticker": ticker.upper(),
        "timeframe": timeframe,
        "horizon": horizon,
        "verdict": verdict,
        "evaluatedAt": evaluated_at,
        "report": os.path.basename(report_file) if report_file else None,
        "strategyVersion": strategy_version,
        "method": method,
        "dataPolicy": data_policy,
        "scope": scope,
        # The SAMPLE this verdict rests on (evaluate.sufficiency): portfolio dates, holdout dates,
        # evaluated streams, coverage, and the tickers that failed or were skipped. A verdict is a
        # claim about evidence; a record that does not carry the size of that evidence cannot be
        # audited later, and this one could not before "portfolio-v3".
        "sufficiency": sufficiency,
        "note": (
            "Pooled across the evaluation universe at this label horizon — this record says the run "
            "COVERED this (ticker, timeframe, horizon), not that this ticker was judged on its own."
        ),
    }
    if db.enabled():
        db.save_verdict(ticker, timeframe, horizon, record)
    else:
        os.makedirs(os.path.dirname(path), exist_ok=True)
        tmp = path + ".tmp"
        with open(tmp, "w") as f:
            json.dump(record, f, indent=2)
        os.replace(tmp, path)
    return path


def load_verdict(ticker: str, timeframe: str, horizon: int, out_dir: str | None = None) -> dict | None:
    """The stored record, or None. An unreadable or malformed file is None — a verdict nobody can
    parse is a verdict nobody has."""
    if db.enabled():
        return db.load_verdict(ticker, timeframe, horizon)
    try:
        with open(verdict_path(ticker, timeframe, horizon, out_dir)) as f:
            record = json.load(f)
    except (OSError, json.JSONDecodeError):
        return None
    return record if isinstance(record, dict) else None


def list_verdicts(out_dir: str | None = None) -> list[tuple[str, dict | None]]:
    """Return logical filenames and records from the configured durable backend."""
    if db.enabled():
        rows = db.list_verdicts()
        return [
            (_key(str(r.get("ticker", "")), str(r.get("timeframe", "")), int(r.get("horizon", 0))) + ".json", r)
            for r in rows
        ]
    directory = verdict_dir(out_dir)
    try:
        names = sorted(n for n in os.listdir(directory) if n.endswith(".json"))
    except OSError:
        return []
    rows: list[tuple[str, dict | None]] = []
    for name in names:
        try:
            with open(os.path.join(directory, name)) as f:
                record = json.load(f)
        except (OSError, json.JSONDecodeError):
            rows.append((name, None))
            continue
        rows.append((name, record if isinstance(record, dict) else None))
    return rows


def evaluation_block(
    ticker: str,
    timeframe: str,
    horizon: int,
    report: dict | None = None,
    out_dir: str | None = None,
    data_policy: str | None = None,
) -> dict | None:
    """The `evaluation` block served on /predict and /backtest, or None when no verdict exists.

    `report` is the SERVED RECORD'S report — the source of the parameters the expected strategy
    version is computed from. Passing None (or a report missing thresholds/costBps/allowShort) makes
    the block `current: false`, which is the fail-closed answer: we could not establish that the
    stored verdict describes the strategy this record runs. `data_policy` also defaults to None for
    the same reason: a caller that omits the served model's policy does not establish a match.
    """
    record = load_verdict(ticker, timeframe, horizon, out_dir)
    if record is None:
        return None
    stored = record.get("strategyVersion")
    expected = expected_strategy_version(report)
    method = record.get("method")
    sufficiency = record.get("sufficiency")
    issues = evidence_issues(sufficiency)
    evidence_current = not issues
    stored_data_policy = record.get("dataPolicy")
    data_policy_current = (
        stored_data_policy == FEATURE_FRAME_POLICY
        and data_policy == FEATURE_FRAME_POLICY
        and stored_data_policy == data_policy
    )
    current = (
        bool(stored)
        and bool(expected)
        and stored == expected
        and method == EVALUATION_METHOD
        and evidence_current
        and data_policy_current
    )
    return {
        "verdict": record.get("verdict"),
        "evaluatedAt": record.get("evaluatedAt"),
        "strategyVersion": stored,
        # Served so a refusal is legible: "these two strings differ" is actionable, "current: false"
        # on its own is not.
        "expectedStrategyVersion": expected,
        "method": method,
        "expectedMethod": EVALUATION_METHOD,
        "sufficiency": sufficiency,
        "evidenceCurrent": evidence_current,
        "evidenceIssues": issues,
        "dataPolicy": stored_data_policy,
        "servedDataPolicy": data_policy,
        "expectedDataPolicy": FEATURE_FRAME_POLICY,
        "dataPolicyCurrent": data_policy_current,
        "current": current,
        "scope": record.get("scope"),
        "report": record.get("report"),
    }
