"""Strategy identity — what, exactly, was validated.

A stored evaluator verdict is a statement about a *specific strategy*: this feature set, these model
parameters, these thresholds, this cost assumption, and this execution contract. Change any of them
and the verdict is a statement about something that no longer exists. `strategy_version(...)` makes that
mechanical instead of a matter of memory: it is a stable hash over the constituents, so a verdict
recorded under an older one is served as `current: false` rather than quietly believed.

EXECUTION_CONTRACT_VERSION names `docs/PAPER_EXECUTION_CONTRACT.md` — the normative statement of the
per-bar allocation rule that BOTH the Python backtest and the Go paper engine implement. Revising the
rule means bumping it, which invalidates every stored verdict by construction. That is the point: a
verdict earned by one execution rule may not be spent by another.

The hash is deliberately NOT over the trained model or the data. Retraining on fresh bars does not
change the strategy; it changes an instance of it. Data freshness is a separate gate (contract §4,
gate 2), enforced against `dataThrough`.

THE PARAMETERS ARE ARGUMENTS, NOT DEFAULTS — and that is a fix, not a style choice. `strategy_version`
used to hash the MODULE DEFAULTS (6 bps, 0.55/0.45) and to omit `allowShort` entirely, while
`evaluate.EvalConfig` reads `EVAL_COST_BPS`, `EVAL_UPPER`, `EVAL_LOWER` and `EVAL_ALLOW_SHORT` and
derives every position it judges from them. An evaluation run at 0 bps, 0.90/0.10, shorting enabled
therefore wrote a verdict STAMPED AS the default long-only 6 bps strategy, and `/predict` served it
`current: true` — a verdict about one strategy, spent by another, which is the single thing gate 4
exists to prevent. There is deliberately no zero-argument form: every caller states which strategy it
means. `default_strategy_version()` exists for the one caller that genuinely means "this service's
defaults" (/health), and says so in its name.
"""
from __future__ import annotations

import hashlib
import json

from .config import COMMISSION_BPS, SLIPPAGE_BPS
from .features import MODEL_FEATURES
from .model import DEFAULT_LOWER, DEFAULT_UPPER, LGB_PARAMS

# Names docs/PAPER_EXECUTION_CONTRACT.md. Bump on any change to the rule stated there (§1 semantics
# or §3 live mapping) — not for edits to the prose.
#
# 1.1.0 (2026-08-23): §2 gained bar COMPLETENESS (a forming bar is not a bar to act on) and §3 gained
# the execution quote's `asOf` requirement (a fill dated before the bar it reconciles is not a fill).
# Both change the live mapping, so every stored verdict made under 1.0.0 goes stale by construction.
EXECUTION_CONTRACT_VERSION = "1.1.0"

# The single definition of the gated backtest's cost assumption (commission + slippage, bps, charged
# on |Δposition|). main.py serves it and it is hashed below, so the number the model was validated
# under and the number the paper engine records can never be two different numbers.
COST_BPS = COMMISSION_BPS + SLIPPAGE_BPS

# The service default for shorting. `/train` takes `allowShort` per record and `evaluate` reads
# EVAL_ALLOW_SHORT, so this is only ever the DEFAULT — never an assumption made about a record.
DEFAULT_ALLOW_SHORT = False


def strategy_inputs(cost_bps: float, upper: float, lower: float, allow_short: bool) -> dict:
    """The constituents of ONE strategy identity, in a JSON-stable shape (also served under /health
    for the service defaults, so an operator can see WHY two versions differ, not just that they do).

    Two groups, both of which change what was validated:
      * CODE identity — the execution contract, the feature set, the model hyper-parameters;
      * PARAMETER identity — the cost assumption, the two thresholds, and whether shorts were
        derivable at all. `allowShort` belongs here because it changes the POSITIONS: at the same
        probabilities, a long-only run is flat where a shorting run is -1.
    """
    return {
        "executionContract": EXECUTION_CONTRACT_VERSION,
        "features": list(MODEL_FEATURES),
        "lgbParams": {k: v for k, v in sorted(LGB_PARAMS.items())},
        "thresholds": {"upper": float(upper), "lower": float(lower)},
        "costBps": float(cost_bps),
        "allowShort": bool(allow_short),
    }


def strategy_version(cost_bps: float, upper: float, lower: float, allow_short: bool) -> str:
    """Stable, deterministic id for the strategy described by these constituents. Same inputs ->
    same string, across processes and runs (no time, no randomness, no dict-ordering dependence).

    Every parameter is REQUIRED. A zero-argument form would silently mean "the defaults", which is
    exactly how a custom evaluation run came to be stamped as the default strategy.
    """
    blob = json.dumps(
        strategy_inputs(cost_bps, upper, lower, allow_short),
        sort_keys=True, separators=(",", ":"), default=str,
    )
    return "sv1-" + hashlib.sha256(blob.encode("utf-8")).hexdigest()[:16]


def default_strategy_version() -> str:
    """The version of the strategy this service's DEFAULTS describe. Named rather than implied: it
    is what /health reports, and it is not what any particular trained record was validated under
    (a record carries its own thresholds/cost/allowShort — see `verdicts.expected_strategy_version`).
    """
    return strategy_version(COST_BPS, DEFAULT_UPPER, DEFAULT_LOWER, DEFAULT_ALLOW_SHORT)
