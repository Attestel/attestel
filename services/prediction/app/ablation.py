"""The A–F ablation ladder driver (contract §5.1) — Wave 3 Lane 3C.

    python -m app.ablation          # or: make ablate

This is a BATCH CLI in the same posture as `app.evaluate` and `app.evaluate_events`: it produces no
buy/sell output, executes nothing, and answers one question — **does adding an input class to the
analyst's context change what it gets right?** It answers it by running the identical cutoff set
through each rung, writing a §5 prediction record per (cutoff × ticker × rung) into the events
service's point-in-time store, and reading back the outcomes that store resolved from stored bars.

WAVE 5A LANDED THE LADDER; THE CLI NOW EXECUTES IT.
---------------------------------------------------
The command assembles each rung, calls the analyst, writes §5 records, and then evaluates them.
It is built to run on a prod box that has a real model. **No checked-in verdict was produced by
one.** Every number this programme had measured before the first production run
came from an injected fake or `stub:offline`, which has never disagreed with the schema, never
emitted an unserved number, never triggered an override branch and never taken a second to answer.
`assert_real_runtime` below is the code form of that discipline: this driver REFUSES to produce a
verdict against `stub:offline`, because a ladder measured against a fixture would license the
`direction` word (§9.20) on nothing.

WHAT THIS FILE IS
-----------------
The DRIVER and the full metric battery: directional accuracy, precision/recall by direction,
abstention coverage, Brier (through the out-of-sample calibration in `ablation_calibration.py`),
rank IC, return under a fixed execution rule, max drawdown / Sortino / Sharpe, and turnover with
costs. Every metric comes out of `compute_metrics`, a records-in / numbers-out helper that touches
no I/O.

SIZED IN RUNS, NOT IN WALL-CLOCK
--------------------------------
`plan_runs()` reports the ladder's cost in GENERATIONS, because nobody has measured a per-generation
cost yet. When 5C's prod smoke reports `secondsPerAnalystRun`, the ladder's duration is that number
times this one — a multiplication, not a redesign.

RUNG STATUS — MEASURED, NOT ASSUMED (read this before reporting a ladder result)
--------------------------------------------------------------------------------
* **A** is not built and cannot be: §5.1 marks it "(future, requires a VL model)", and §10 puts a
  VL/vision path outside this contract entirely.
* **B–E** are runnable as soon as the analyst path answers.
* **F IS NOW RUNNABLE, and this is a revision of Wave 2's finding — re-measured at the Wave 5 tree
  rather than carried forward.** Wave 2 flagged F unrunnable because "the market/sector half does
  not exist anywhere in the tree", and at Wave 2 that was true. It is no longer: Wave 3 Lane 3A's
  `gateway/context.go` serves a `marketContext` block (the §9.26 `SPY` benchmark proxy through the
  analysis service, plus realised volatility computed from that same reading), and Wave 1's
  `services/events/app/predictions.py` resolves `benchmark_return` and `sector_return` against
  `sector_etf_for(ticker)`. Both are point-in-time.

  **What is still absent is NAMED, not swallowed.** `marketContext` carries `null` for breadth and
  for implied volatility — this deployment has no VIX or advance/decline source, and §9.26's answer
  was a proxy, not a feed. So F runs, and `build_bundle` records `partialInputs` when an input class
  arrives with no non-null dimension. A rung whose own input class came back entirely empty is
  REFUSED for that (cutoff, ticker) rather than reported as F: Wave 2's reasoning was right — "a
  rung that quietly drops half its inputs and still reports as F would attribute a result to a cause
  it did not have" — and what has changed is that the inputs now arrive, not that the reasoning was
  wrong.

THE REFUSAL IS THE OUTPUT
-------------------------
Two refusals, and each one produces no verdict rather than a qualified one:

* If any outcome in the run was resolved against a synthetic bar, this driver prints
  `SYNTHETIC DATA — cannot validate` and exits non-zero. Not a verdict, not a partial verdict, not a
  verdict annotated as synthetic. The check is a LOCAL query over the `resolved_from_source` the
  events store already recorded (§9.25) — the store refuses to resolve against a synthetic bar in
  the first place (§9.46), and this is the second gate that makes the refusal visible in the report.
* If the analyst path was served by `stub:offline` (or any other `stub:*` label), the run is refused
  before a single record is written. Fail loudly rather than produce a meaningless number.

THE VERDICT IS SERVED, NOT LEFT ON DISK (§9.61 binding 5)
----------------------------------------------------------
`write_verdict` writes `{EVAL_OUT_DIR}/ablation/verdict.json` in the shape
`services/llm/app/ablation_verdict.py` reads and the analyst envelope stamps onto each §7 object as
`ablationVerdict: {validated, rung, horizon}`. Producing the number is not the exit criterion;
the browser being able to read it is. `validated: false` is written, and is a real answer distinct
from an absent verdict (§9.61 binding 4).
"""
from __future__ import annotations

import json
import hashlib
import math
import os
import sys
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone

import requests

from . import db as _db

# =================================================================================================
# The ladder (contract §5.1) — the source document letters the rungs two different ways in two
# different sections; the contract already resolved it and this table is that resolution.
# =================================================================================================

RUNG_INPUTS: dict[str, tuple[str, ...]] = {
    "A": ("chart_image",),
    "B": ("ohlcv",),
    "C": ("ohlcv", "indicators"),
    "D": ("ohlcv", "indicators", "company_events"),
    "E": ("ohlcv", "indicators", "company_events", "earnings_estimates_surprise"),
    "F": ("ohlcv", "indicators", "company_events", "earnings_estimates_surprise",
          "macro_fed", "market_sector"),
}

RUNNABLE = "runnable"
NOT_BUILT = "not_built"
NOT_RUNNABLE = "not_runnable"

RUNG_STATUS: dict[str, tuple[str, str]] = {
    "A": (NOT_BUILT, "requires a VL model (contract §5.1); a VL/vision path is outside §10"),
    "B": (RUNNABLE, ""),
    "C": (RUNNABLE, ""),
    "D": (RUNNABLE, ""),
    "E": (RUNNABLE, ""),
    # F flipped to RUNNABLE at Wave 5A, deliberately and with the measurement behind it in the
    # module header. The reason string is KEPT and rewritten rather than blanked, because "F is
    # runnable" without "and here is what is still null in it" is the silently-degraded bundle Wave
    # 2 refused, wearing a different label.
    "F": (
        RUNNABLE,
        "F is E + macro/Fed + market/sector, and BOTH halves now exist point-in-time: `GET /macro` "
        "carries `surprise`, and Wave 3A's `gateway/context.go` serves `marketContext` (the §9.26 "
        "SPY benchmark proxy plus realised volatility) alongside `benchmark_return`/`sector_return` "
        "in the events store. Breadth and implied volatility remain null in this deployment — there "
        "is no VIX or advance/decline source — so a bundle whose `market_sector` class arrives with "
        "no non-null dimension is refused for that (cutoff, ticker) rather than reported as F.",
    ),
}

#: Input classes whose ABSENCE defines the rung. If one of these arrives empty, the rung ran without
#: the very thing that distinguishes it from the rung below, and the result would be attributed to a
#: cause it did not have. Keyed by rung; checked in `build_bundle`.
RUNG_DEFINING_INPUT: dict[str, str] = {
    "B": "ohlcv",
    "C": "indicators",
    "D": "company_events",
    "E": "earnings_estimates_surprise",
    "F": "market_sector",
}

# §Conventions: map keys use the short form.
HORIZONS = ("1d", "5d", "20d", "60d")
HORIZON_BARS = {"1d": 1, "5d": 5, "20d": 20, "60d": 60}
TRADING_DAYS_PER_YEAR = 252

# A call is an abstention when the analyst said so or refused to take a side. Both spellings count:
# `unclear` is not a quieter `neutral`, and neither is a direction.
ABSTAIN_DIRECTIONS = ("neutral", "unclear")

# --- splits (contract §5, locked decision 3) -------------------------------------------------------
# DUPLICATED DELIBERATELY from services/events/app/predictions.py. The two services share no code —
# separate images, separate requirements, no shared package in this repo — so the constants are
# stated twice and the STORE IS THE AUTHORITY: it rejects a write whose `split` disagrees with its
# own `split_for(asOf)`. A drift between these two files therefore fails loudly on the first write
# rather than silently mislabelling a record.
SPLIT_VERSION = "split@1"
SPLIT_DEV_END = "2024-01-01T00:00:00Z"
SPLIT_VALIDATION_END = "2025-07-01T00:00:00Z"

# --- verdicts (docs/VALIDATION_AND_GO_LIVE.md — binding, not advisory) -----------------------------
# The vocabulary is EXACTLY these four words. No new verdict, no softening, no "promising".
VERDICT_MEANING = {
    "EDGE": (
        "On this rung, this split and this cutoff set, the non-abstained calls were directionally "
        "right more often than not AND carried a positive mean excess return over the benchmark. "
        "This is a NECESSARY condition, never a green light: it says the rung earned the right to "
        "be examined further, not that it should be traded. See docs/VALIDATION_AND_GO_LIVE.md."
    ),
    "NO EDGE": (
        "The calls do not clear the bar on this rung. 'This input class does not help' is a real "
        "and valuable result — it is the answer the ladder exists to be able to give. Do NOT tune "
        "the rung until it passes; that is how you fool yourself."
    ),
    "INCONCLUSIVE": (
        "There are too few resolved outcomes to say anything. The sample size is reported below. "
        "Widen the cutoff set or the universe and re-run; do not read the numbers as a result."
    ),
    "SUSPECT": (
        "The realised Sharpe is implausibly high (>3). In this domain that is almost always "
        "look-ahead leakage or a bug, not an edge. Stop and find the leak."
    ),
}
SHARPE_SUSPECT_ABOVE = 3.0

# Exit codes, mirroring app.evaluate so an operator reads one convention across all three harnesses.
EXIT_OK = 0                 # a verdict was produced — EDGE, NO EDGE, INCONCLUSIVE or SUSPECT
EXIT_SYNTHETIC = 2          # refused: an outcome was resolved against a synthetic bar
EXIT_NO_DATA = 3            # refused: nothing to measure (store unreachable, analyst unreachable)
EXIT_RUNG_UNAVAILABLE = 4   # refused: a rung that is not built or not runnable was requested
EXIT_CUTOFF_MISMATCH = 5    # refused: two rungs were compared across different cutoff sets
EXIT_STUB_RUNTIME = 6       # refused: the analyst was served by a stub, so nothing was measured

DEFAULT_MIN_SAMPLES = 30    # below this, INCONCLUSIVE with the size reported

SYNTHETIC_SOURCE = "synthetic"

#: `llm.py::_stub_label`'s convention. A PREFIX, so `stub:quota` is refused as firmly as
#: `stub:offline` — both are fixtures, and a gate that knew only one label would license a claim
#: about the model from the other.
STUB_PREFIX = "stub:"

#: One-way transaction cost applied by the fixed execution rule, in basis points. 6 bps mirrors
#: `EVENT_COST_BPS` in `.env.example` so the ladder and the event study cost a trade the same way.
#: It is a STATED assumption, printed in every report: a return computed at zero cost is a return
#: nobody could have earned.
DEFAULT_COST_BPS = 6.0

#: Sortino's target return. Zero, not the benchmark: the excess return is already relative to the
#: benchmark (§9.26), and subtracting it twice would flatter every number here.
SORTINO_TARGET = 0.0

#: Model generations per analyst run: orchestrator + 3 specialists + an optional news-conflict pass
#: + the final analyst, each with ONE firmer retry available on structural failure. `gateway/
#: analyst.go` states the same upper bound ("up to eight sequential local-model generations") in its
#: timeout rationale. It is the UPPER bound, so `plan_runs` sizes the ladder pessimistically — the
#: mistake that costs a prod box a day is under-estimating, not over-estimating.
GENERATIONS_PER_ANALYST_RUN = 8


# =================================================================================================
# Config
# =================================================================================================


def _env(key: str, default: str) -> str:
    value = os.getenv(key)
    return value if value is not None and value.strip() else default


@dataclass
class AblationConfig:
    cutoffs: list[str]
    universe: list[str]
    rungs: list[str]
    split: str
    events_url: str
    llm_url: str
    out_dir: str
    analysis_url: str = "http://analysis:8001"
    min_samples: int = DEFAULT_MIN_SAMPLES
    timeout: float = 30.0
    cost_bps: float = DEFAULT_COST_BPS
    #: The split the calibration is FITTED on. It must differ from `split`, which is the split being
    #: scored — `ablation_calibration.apply_to` refuses the leak, and this is where the operator's
    #: two choices are recorded side by side so the refusal is legible before it fires.
    calibration_split: str = "dev"
    #: Wave 5C's seam, as ladder configuration. `False` is available for a dry run of the PLUMBING
    #: and it is loud in the report; it may never produce a verdict a `direction` word is gated on.
    require_real_runtime: bool = True
    analyst_timeout: float = 600.0

    @classmethod
    def from_env(cls) -> "AblationConfig":
        cutoffs = [c.strip() for c in _env("ABLATE_CUTOFFS", "").split(",") if c.strip()]
        universe = [t.strip().upper() for t in _env("ABLATE_UNIVERSE", "NVDA").split(",") if t.strip()]
        rungs = [r.strip().upper() for r in _env("ABLATE_RUNGS", "B,C,D,E").split(",") if r.strip()]
        return cls(
            cutoffs=cutoffs,
            universe=universe,
            rungs=rungs,
            split=_env("ABLATE_SPLIT", "dev"),
            events_url=_env("EVENTS_URL", "http://events:8004").rstrip("/"),
            llm_url=_env("LLM_URL", "http://llm:8002").rstrip("/"),
            out_dir=_env("EVAL_OUT_DIR", "data/eval"),
            analysis_url=_env("ANALYSIS_URL", "http://analysis:8001").rstrip("/"),
            min_samples=int(_env("ABLATE_MIN_SAMPLES", str(DEFAULT_MIN_SAMPLES))),
            cost_bps=float(_env("ABLATE_COST_BPS", str(DEFAULT_COST_BPS))),
            calibration_split=_env("ABLATE_CALIBRATION_SPLIT", "dev"),
            # Opting OUT takes an explicit `false`. The default is the strict one, because the
            # expensive mistake is a verdict nobody realises came from a fixture.
            require_real_runtime=_env("ABLATE_REQUIRE_REAL_RUNTIME", "true").lower() != "false",
            analyst_timeout=float(_env("ABLATE_ANALYST_TIMEOUT", "600")),
        )

    @property
    def verdict_dir(self) -> str:
        """§9.61's path, and the one `services/llm/app/ablation_verdict.py` reads."""
        return os.path.join(self.out_dir, "ablation")


# =================================================================================================
# The runtime gate — Wave 5C's seam, as the ladder's precondition
# =================================================================================================


class StubRuntimeRefused(Exception):
    """The analyst was served by a stub. A ladder measured against a fixture produces a verdict that
    licenses the `direction` word on nothing, so the run stops rather than continuing quietly."""


def runtime_of(answer: dict) -> str:
    """The runtime that served ONE analyst answer.

    Reads Wave 5C's `runtime.served` first — it is the field that exists to answer exactly this —
    and falls back to `identity.modelUsed`, which carries `stub:offline` on the same path for any
    caller (or fixture) predating the `runtime` block.
    """
    served = str(((answer or {}).get("runtime") or {}).get("served") or "").strip()
    if served:
        return served
    return str(((answer or {}).get("identity") or {}).get("modelUsed") or "").strip() or "unknown"


def is_stub_runtime(label: str) -> bool:
    return str(label or "").strip().lower().startswith(STUB_PREFIX)


def assert_real_runtime(answer: dict) -> str:
    """Return the runtime label, or raise. Called on every analyst answer before it becomes a
    record — one stub answer in a thousand real ones is still a fabricated row in the ladder."""
    label = runtime_of(answer)
    if is_stub_runtime(label):
        raise StubRuntimeRefused(
            f"the analyst was served by {label!r}. `stub:offline` is a FIXTURE: it has never "
            "disagreed with the schema, never emitted an unserved number, never triggered an "
            "override branch and never taken a second to answer. A ladder measured against it "
            "would produce a verdict that licenses the `direction` word (§9.20) on nothing. "
            "Configure MODEL_RUNTIME_URL + MODEL_RUNTIME_MODEL on the llm service and re-run."
        )
    return label


def runtime_precheck(cfg: AblationConfig, *, get=None) -> tuple[bool, str]:
    """`(ok, detail)` — ask the llm service whether a runtime is configured, BEFORE any generation.

    A pre-flight, not a substitute for `assert_real_runtime`: a configured runtime can still fall
    back to the stub mid-run when the endpoint drops. Both gates exist because they fail at
    different times, and only the second one sees what actually served each record.
    """
    fetch = get or (lambda url: requests.get(url, timeout=cfg.timeout))
    try:
        payload = fetch(f"{cfg.llm_url}/health").json() or {}
    except Exception as exc:  # noqa: BLE001 — an unreachable service is a refusal, not a crash
        return False, f"{cfg.llm_url}/health did not answer: {type(exc).__name__}: {exc}"
    runtime = ((payload.get("llm") or payload).get("runtime") or {})
    if runtime.get("configured"):
        return True, f"runtime {runtime.get('name')!r} · model {runtime.get('model')!r}"
    refusals = runtime.get("refusals") or []
    return False, " ".join(str(r) for r in refusals) or "no model runtime is configured"


# =================================================================================================
# Sizing — in RUNS, because nobody has measured a per-run cost yet
# =================================================================================================


def plan_runs(cfg: AblationConfig, rungs: list[str] | None = None,
              *, generations_per_run: int = GENERATIONS_PER_ANALYST_RUN) -> dict:
    """The ladder's cost, in analyst runs and in model generations.

    WHY NOT WALL-CLOCK. Nobody has observed this model generating anything, so a minutes estimate
    here would be a number with no measurement behind it — precisely what this wave exists to stop
    producing. 5C's prod smoke reports `secondsPerAnalystRun`; multiply.
    """
    requested = [r.strip().upper() for r in (rungs or cfg.rungs) if str(r).strip()]
    in_split = cutoffs_for_split(cfg.cutoffs, cfg.split)
    runs_per_rung = len(in_split) * len(cfg.universe)
    runs = runs_per_rung * len(requested)
    return {
        "rungs": requested,
        "cutoffsRequested": len(cfg.cutoffs),
        "cutoffsInSplit": len(in_split),
        "universe": len(cfg.universe),
        "split": cfg.split,
        "runsPerRung": runs_per_rung,
        "analystRuns": runs,
        "generationsPerRun": generations_per_run,
        "modelGenerations": runs * generations_per_run,
        "wallClock": "unmeasured — multiply `analystRuns` by 5C's `secondsPerAnalystRun`",
    }


# =================================================================================================
# Stage 1 — splits and the cutoff set
# =================================================================================================


def split_for(as_of: str) -> str:
    """Deterministic, total, disjoint. Two boundaries, three windows, no cutoff in two of them."""
    stamp = _iso(as_of)
    if stamp < SPLIT_DEV_END:
        return "dev"
    if stamp < SPLIT_VALIDATION_END:
        return "validation"
    return "test"


def _iso(value) -> str:
    text = str(value or "").strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    parsed = datetime.fromisoformat(text)
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def cutoff_fingerprint(cutoffs) -> str:
    """A stable identity for a cutoff SET, order-insensitive.

    Comparing two rungs run over different cutoffs is comparing two different questions. The
    fingerprint makes that comparison refusable rather than merely inadvisable.
    """
    normalised = sorted({_iso(c) for c in cutoffs})
    digest = hashlib.sha256("|".join(normalised).encode("utf-8")).hexdigest()[:16]
    return f"{len(normalised)}@{digest}"


def assert_identical_cutoff_set(per_rung: dict[str, list[str]]) -> str:
    """Every rung runs against the IDENTICAL cutoff set (§5.1). Returns the shared fingerprint."""
    fingerprints = {rung: cutoff_fingerprint(cuts) for rung, cuts in per_rung.items()}
    distinct = set(fingerprints.values())
    if len(distinct) > 1:
        raise CutoffSetMismatch(
            "rungs were run against different cutoff sets, so no comparison between them is "
            f"meaningful: {fingerprints}"
        )
    return distinct.pop() if distinct else cutoff_fingerprint([])


class CutoffSetMismatch(Exception):
    """Two rungs, two cutoff sets. Refused rather than reported."""


def cutoffs_for_split(cutoffs, split: str) -> list[str]:
    """The subset of a cutoff set belonging to one split. Splits never mix — not here, not anywhere."""
    return [_iso(c) for c in cutoffs if split_for(c) == split]


# =================================================================================================
# Stage 2 — the rung's input bundle
# =================================================================================================


def rung_status(rung: str) -> tuple[str, str]:
    key = str(rung or "").strip().upper()
    if key not in RUNG_STATUS:
        return NOT_BUILT, f"{rung!r} is not a rung; §5.1 defines A–F"
    return RUNG_STATUS[key]


def build_bundle(rung: str, ticker: str, as_of: str, *, fetch) -> dict:
    """The input bundle for one (rung, ticker, cutoff).

    `fetch(kind, ticker, as_of)` is injected so the driver has exactly one seam onto the rest of the
    platform and the ladder can be exercised offline. Every input class named in `RUNG_INPUTS` is
    requested; a class that returns `None` is recorded as ABSENT in the bundle rather than dropped,
    because "the rung ran without its own inputs" is the single most important thing a later reader
    of a ladder result needs to know.
    """
    status, reason = rung_status(rung)
    if status != RUNNABLE:
        raise RungUnavailable(f"rung {rung}: {reason}")
    bundle = {"rung": rung, "ticker": ticker, "asOf": _iso(as_of), "inputs": {}, "absent": [],
              "partialInputs": []}
    for kind in RUNG_INPUTS[rung]:
        value = fetch(kind, ticker, bundle["asOf"])
        if value is None:
            bundle["absent"].append(kind)
        else:
            bundle["inputs"][kind] = value
            if _is_all_null(value):
                # PRESENT BUT EMPTY. `marketContext` with every dimension null is the shape Wave 2
                # was worried about: it type-checks, it serialises, and it carries no information.
                bundle["partialInputs"].append(kind)

    defining = RUNG_DEFINING_INPUT.get(rung)
    if defining and (defining in bundle["absent"] or defining in bundle["partialInputs"]):
        # The class that DEFINES this rung arrived empty, so this run is rung (rung-1) wearing the
        # wrong label. Refused for this (cutoff, ticker) — a result attributed to a cause it never
        # saw is worse than a gap in the grid, and the gap is recorded either way.
        raise RungInputEmpty(
            f"rung {rung}'s defining input class {defining!r} arrived "
            f"{'absent' if defining in bundle['absent'] else 'present but entirely null'} for "
            f"{ticker} at {bundle['asOf']}. Running it anyway would report rung {rung} for a "
            "bundle that is indistinguishable from the rung below it."
        )
    return bundle


def _is_all_null(value) -> bool:
    """True when a value carries no information: `{}`, `[]`, or a mapping whose every leaf is None.

    Not a general emptiness test — a mapping with a `0` or a `False` in it is INFORMATION, and
    treating a legitimate zero as an absence is how a real measurement becomes a missing one.
    """
    if value is None:
        return True
    if isinstance(value, dict):
        return not value or all(_is_all_null(v) for v in value.values())
    if isinstance(value, (list, tuple)):
        return not value or all(_is_all_null(v) for v in value)
    return False


class RungUnavailable(Exception):
    """A rung that is not built (A) was requested."""


class RungInputEmpty(Exception):
    """A runnable rung's defining input class arrived absent or entirely null for one cell of the
    grid. The cell is skipped and reported; the run continues."""


# =================================================================================================
# Stage 3 — run a rung and write §5 records
# =================================================================================================


@dataclass
class RunReport:
    rung: str
    split: str
    cutoffs: list[str]
    written: int = 0
    skipped: list[dict] = field(default_factory=list)
    #: Which runtime served each record, counted. A run that reports `{"prod-a": 240}` is
    #: attributable; one that reports `{"prod-a": 200, "stub:offline": 40}` is two experiments
    #: pooled, and the report has to be able to say so.
    runtimes: dict[str, int] = field(default_factory=dict)


def run_rung(
    rung: str,
    cfg: AblationConfig,
    *,
    analyst,
    store,
    fetch,
    now: str | None = None,
) -> RunReport:
    """One rung over the whole (cutoff × ticker) grid, writing one §5 record each.

    `analyst(bundle) -> dict | None` is the analyst path (Lane 3B's `POST /analyst` by default).
    `store.post(record)` is the events service. Both are injected: this function performs no I/O of
    its own, which is what lets Wave 5A swap either side without touching the ladder logic.

    A record is written ONLY when the analyst returned both a forecast and an `identity` block. The
    driver never invents identity — a record whose model, prompt version or generation settings are
    guessed cannot be reproduced, and an unreproducible record in the ladder is worse than a gap.
    """
    report = RunReport(rung=rung, split=cfg.split, cutoffs=list(cfg.cutoffs))
    stamp = now or datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    try:
        existing = {
            (str(row.get("ticker") or "").upper(), _iso(row.get("asOf")))
            for row in store.list(experiment=rung, split=cfg.split)
            if row.get("ticker") and row.get("asOf")
        }
    except Exception:  # noqa: BLE001 — write-only test seams and old stores are still supported
        existing = set()
    for as_of in cfg.cutoffs:
        cutoff = _iso(as_of)
        if split_for(cutoff) != cfg.split:
            # Splits are time-disjoint and immutable; a cutoff outside the requested split belongs
            # to a different question and is not quietly folded in.
            report.skipped.append({"asOf": cutoff, "reason": "cutoff_outside_split"})
            continue
        for ticker in cfg.universe:
            if (ticker.upper(), cutoff) in existing:
                report.skipped.append(
                    {"asOf": cutoff, "ticker": ticker, "reason": "already_recorded"}
                )
                continue
            try:
                bundle = build_bundle(rung, ticker, cutoff, fetch=fetch)
            except RungUnavailable:
                raise
            except RungInputEmpty as exc:
                # Named separately from a generic bundle failure: "the rung's own input class was
                # empty here" is a finding about coverage, not a transport error, and a reader of
                # the report needs to be able to count them.
                report.skipped.append(
                    {"asOf": cutoff, "ticker": ticker, "reason": "rung_defining_input_empty",
                     "detail": str(exc)}
                )
                continue
            except Exception as exc:  # noqa: BLE001 — one bad bundle must not end the run
                report.skipped.append(
                    {"asOf": cutoff, "ticker": ticker, "reason": f"bundle: {type(exc).__name__}"}
                )
                continue
            try:
                answer = analyst(bundle)
            except Exception as exc:  # noqa: BLE001 — one transport failure is a named grid gap
                report.skipped.append(
                    {"asOf": cutoff, "ticker": ticker,
                     "reason": f"analyst: {type(exc).__name__}", "detail": str(exc)}
                )
                continue
            if not answer or not answer.get("forecast") or not answer.get("identity"):
                report.skipped.append(
                    {"asOf": cutoff, "ticker": ticker, "reason": "analyst_returned_nothing_usable"}
                )
                continue
            # THE RUNTIME GATE, per answer. Raised, not skipped: one stub answer means the endpoint
            # is down or misconfigured, and the remaining cells would be fixtures too. Failing
            # loudly on the first one costs a minute; discovering it in the verdict costs the run.
            if cfg.require_real_runtime:
                runtime_label = assert_real_runtime(answer)
            else:
                runtime_label = runtime_of(answer)
            report.runtimes[runtime_label] = report.runtimes.get(runtime_label, 0) + 1
            record = {
                "ticker": ticker,
                "asOf": cutoff,
                "createdAt": max(stamp, cutoff),
                "experiment": rung,
                "split": cfg.split,
                "identity": answer["identity"],
                "evidence": record_evidence(bundle, answer.get("evidence") or {}),
                "forecast": answer["forecast"],
            }
            try:
                store.post(record)
            except Exception as exc:  # noqa: BLE001 — a rejected write is reported, never retried blind
                report.skipped.append(
                    {"asOf": cutoff, "ticker": ticker, "reason": f"store: {exc}"}
                )
                continue
            report.written += 1
    return report


def _ids_from(value, *, prefixes: tuple[str, ...]) -> list[str]:
    """Collect stable ids from one served input class without inventing references."""
    if isinstance(value, dict):
        rows = value.get("events") or value.get("macro") or value.get("items") or []
    elif isinstance(value, list):
        rows = value
    else:
        rows = []
    out: list[str] = []
    for row in rows:
        if not isinstance(row, dict):
            continue
        value_id = str(row.get("id") or row.get("eventId") or "")
        if value_id.startswith(prefixes) and value_id not in out:
            out.append(value_id)
    return out


def record_evidence(bundle: dict, analyst_evidence: dict) -> dict:
    """Build the store's §5 evidence block from the exact deterministic bundle sent to Qwen.

    The analyst response owns the tool trace. It does not own bar references or hashes; those are
    computed here from the inputs so an omitted or model-authored reference cannot enter the
    evaluation store.
    """
    inputs = bundle.get("inputs") or {}
    ohlcv = inputs.get("ohlcv") if isinstance(inputs.get("ohlcv"), dict) else {}
    bars = ohlcv.get("bars") or []
    last = bars[-1] if bars and isinstance(bars[-1], dict) else {}
    technical = inputs.get("indicators", inputs.get("ohlcv"))
    canonical = json.dumps(technical, sort_keys=True, separators=(",", ":"), default=str)
    evidence = {
        "barsRef": {
            "ticker": bundle.get("ticker"),
            "timeframe": ohlcv.get("timeframe") or "1D",
            "lastTs": last.get("time") or ohlcv.get("asOf") or bundle.get("asOf"),
        },
        "technicalStateHash": hashlib.sha256(canonical.encode("utf-8")).hexdigest(),
        "eventIds": _ids_from(inputs.get("company_events"), prefixes=("evt_",)),
        "macroEventIds": _ids_from(inputs.get("macro_fed"), prefixes=("mac_", "evt_")),
        "toolCalls": list(analyst_evidence.get("toolCalls") or []),
    }
    earnings_ids = _ids_from(inputs.get("earnings_estimates_surprise"), prefixes=("evt_",))
    if earnings_ids:
        evidence["earningsSnapshotId"] = earnings_ids[0]
    return evidence


# =================================================================================================
# Stage 4 — read the records back
# =================================================================================================


class PredictionStoreClient:
    """The events service, over HTTP. The prediction service has no database access."""

    def __init__(self, base_url: str, timeout: float = 30.0):
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout

    def post(self, record: dict) -> dict:
        response = requests.post(
            f"{self.base_url}/predictions", json=record, timeout=self.timeout
        )
        if response.status_code != 200:
            raise RuntimeError(f"POST /predictions -> HTTP {response.status_code}: {response.text[:200]}")
        return response.json().get("prediction") or {}

    def list(self, *, experiment: str, split: str, limit: int = 500) -> list[dict]:
        response = requests.get(
            f"{self.base_url}/predictions",
            params={"experiment": experiment, "split": split, "limit": limit},
            timeout=self.timeout,
        )
        if response.status_code != 200:
            raise RuntimeError(f"GET /predictions -> HTTP {response.status_code}")
        return response.json().get("predictions") or []

    def backfill(self, *, experiment: str) -> dict:
        """Resolve every mature outcome for this rung from the events service's stored-bar path."""
        response = requests.post(
            f"{self.base_url}/outcomes/backfill",
            json={"experiment": experiment},
            timeout=self.timeout,
        )
        if response.status_code != 200:
            raise RuntimeError(
                f"POST /outcomes/backfill -> HTTP {response.status_code}: {response.text[:200]}"
            )
        payload = response.json()
        return payload if isinstance(payload, dict) else {}


NON_EARNINGS_EVENT_TYPES = (
    "product_launch", "management_change", "ma_transaction", "regulatory_action",
    "legal_action", "supply_chain", "capital_return", "financing", "partnership",
    "sector_event", "other",
)
EARNINGS_EVENT_TYPES = ("earnings_result", "earnings_guidance", "analyst_revision")

# The benchmark and sector series are deterministic point-in-time reads from the analysis service;
# no provider or live quote is selected here. Unknown tickers use the broad-market proxy only.
SECTOR_PROXY = {
    "NVDA": "SMH", "AMD": "SMH", "AVGO": "SMH", "QCOM": "SMH",
    "AAPL": "XLK", "MSFT": "XLK", "CRM": "XLK", "ORCL": "XLK", "ADBE": "XLK",
    "GOOGL": "XLC", "META": "XLC", "NFLX": "XLC", "DIS": "XLC",
    "AMZN": "XLY", "TSLA": "XLY", "HD": "XLY",
    "JPM": "XLF", "V": "XLF", "MA": "XLF",
    "XOM": "XLE", "CVX": "XLE",
    "UNH": "XLV", "JNJ": "XLV",
    "COST": "XLP", "PEP": "XLP", "KO": "XLP", "WMT": "XLP", "PG": "XLP",
    "BA": "XLI", "CAT": "XLI",
}


def _window_start(as_of: str, days: int) -> str:
    stamp = datetime.fromisoformat(_iso(as_of).replace("Z", "+00:00"))
    return (stamp - timedelta(days=days)).strftime("%Y-%m-%dT%H:%M:%SZ")


class AblationInputClient:
    """Fetch each rung input from point-in-time platform APIs only."""

    def __init__(self, analysis_url: str, events_url: str, timeout: float = 30.0):
        self.analysis_url = analysis_url.rstrip("/")
        self.events_url = events_url.rstrip("/")
        self.timeout = timeout

    def _get(self, url: str, params: dict) -> dict:
        response = requests.get(url, params=params, timeout=self.timeout)
        if response.status_code != 200:
            raise RuntimeError(f"GET {url} -> HTTP {response.status_code}: {response.text[:200]}")
        payload = response.json()
        if not isinstance(payload, dict):
            raise RuntimeError(f"GET {url} returned {type(payload).__name__}, not an object")
        return payload

    @staticmethod
    def _real_price(payload: dict, *, bars: bool = False) -> dict | None:
        if payload.get("sourceIsSynthetic") is True or payload.get("coverage") == "insufficient":
            return None
        required = payload.get("bars") if bars else (
            payload.get("price") or payload.get("indicators") or payload.get("structure")
        )
        return payload if required not in (None, [], {}) else None

    def _technical(self, ticker: str, as_of: str) -> dict | None:
        payload = self._get(
            f"{self.analysis_url}/technical-context/{ticker}",
            {"timeframe": "1D", "as_of": as_of},
        )
        return self._real_price(payload)

    def _events(self, ticker: str, as_of: str, types: tuple[str, ...], days: int) -> dict | None:
        payload = self._get(
            f"{self.events_url}/events",
            {"tickers": ticker, "types": ",".join(types), "since": _window_start(as_of, days),
             "as_of": as_of, "limit": 100},
        )
        return payload if payload.get("events") else None

    def fetch(self, kind: str, ticker: str, as_of: str):
        ticker = ticker.strip().upper()
        if kind == "ohlcv":
            payload = self._get(
                f"{self.analysis_url}/candles/{ticker}",
                {"timeframe": "1D", "limit": 260, "as_of": as_of},
            )
            return self._real_price(payload, bars=True)
        if kind == "indicators":
            return self._technical(ticker, as_of)
        if kind == "company_events":
            return self._events(ticker, as_of, NON_EARNINGS_EVENT_TYPES, 365)
        if kind == "earnings_estimates_surprise":
            payload = self._events(ticker, as_of, EARNINGS_EVENT_TYPES, 730)
            if payload is None:
                return None
            # An earnings-labelled event is not automatically an estimates/surprise input. Require
            # the served event text/facts to carry that comparison; otherwise rung E would be D
            # under another name.
            text = json.dumps(payload, sort_keys=True, default=str).lower()
            return payload if any(token in text for token in
                                  ("estimate", "consensus", "expected", "surprise", "beat", "miss")) else None
        if kind == "macro_fed":
            payload = self._get(
                f"{self.events_url}/macro", {"as_of": as_of, "limit": 100},
            )
            return payload if payload.get("macro") else None
        if kind == "market_sector":
            benchmark = self._technical("SPY", as_of)
            sector_ticker = SECTOR_PROXY.get(ticker)
            sector = self._technical(sector_ticker, as_of) if sector_ticker else None
            # Both halves define this input class. A SPY-only payload is market context, not
            # market+sector context, and must not be reported as rung F.
            if benchmark is None or sector is None:
                return None
            return {
                "benchmark": benchmark,
                "sector": {"ticker": sector_ticker, "technical": sector} if sector_ticker else None,
            }
        raise ValueError(f"unknown ablation input class {kind!r}")


def context_from_bundle(bundle: dict) -> dict:
    """Translate the A–F input-class names into the analyst's canonical evidence keys."""
    inputs = bundle.get("inputs") or {}
    context = {
        "ticker": bundle.get("ticker"), "asOf": bundle.get("asOf"), "subscriptions": [],
    }
    aliases = {
        "ohlcv": "candles",
        "indicators": "technicalContext",
        "company_events": "events",
        "earnings_estimates_surprise": "earnings",
        "macro_fed": "macro",
        "market_sector": "marketContext",
    }
    for source, target in aliases.items():
        if source in inputs:
            context[target] = inputs[source]
    # The same selected inputs are also exposed through the envelope tool used by the orchestrator
    # and final analyst. This is not extra evidence: it is a second view over the exact same bundle.
    context["tickerContext"] = {
        "ticker": bundle.get("ticker"),
        **({"technical": inputs["indicators"]} if "indicators" in inputs else {}),
        **({"recentEvents": inputs["company_events"]} if "company_events" in inputs else {}),
        **({"earnings": inputs["earnings_estimates_surprise"]}
           if "earnings_estimates_surprise" in inputs else {}),
        **({"macro": inputs["macro_fed"]} if "macro_fed" in inputs else {}),
    }
    return context


class AnalystHTTPClient:
    """Call the on-demand analyst endpoint with one already-cutoff-scoped ablation bundle."""

    def __init__(self, base_url: str, timeout: float = 600.0):
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout

    def analyse(self, bundle: dict) -> dict:
        response = requests.post(
            f"{self.base_url}/analyst",
            json={"ticker": bundle["ticker"], "asOf": bundle["asOf"],
                  "context": context_from_bundle(bundle)},
            timeout=self.timeout,
        )
        if response.status_code != 200:
            raise RuntimeError(
                f"POST /analyst -> HTTP {response.status_code}: {response.text[:200]}"
            )
        payload = response.json()
        if not isinstance(payload, dict):
            raise RuntimeError("POST /analyst returned a non-object response")
        return payload


def collect_records(store, *, experiment: str, split: str) -> list[dict]:
    """Read one rung's records for one split.

    Locked decision 2, at the driver's edge: BOTH predicates are required and the call raises without
    them, so no read in this file can return a set that spans two rungs or two splits.
    """
    if not experiment:
        raise ValueError("collect_records requires an `experiment` (contract §5.1): a set spanning "
                         "two rungs produces a statistic that belongs to neither")
    if not split:
        raise ValueError("collect_records requires a `split` (locked decision 3): pooling dev with "
                         "test destroys the only untouched evaluation window there is")
    return store.list(experiment=experiment, split=split)


# =================================================================================================
# Stage 5 — metrics: records in, numbers out. No I/O. Wave 5A extends HERE.
# =================================================================================================


def _mean(values: list[float]) -> float | None:
    return sum(values) / len(values) if values else None


def _stdev(values: list[float]) -> float | None:
    if len(values) < 2:
        return None
    mean = sum(values) / len(values)
    return math.sqrt(sum((v - mean) ** 2 for v in values) / (len(values) - 1))


def sharpe(returns: list[float], horizon: str) -> float | None:
    """Annualised Sharpe of the realised per-call excess returns at one horizon.

    `None`, never 0.0, when there is no dispersion to divide by: a Sharpe of 0 is a claim about the
    distribution, and one observation supports no such claim.
    """
    mean = _mean(returns)
    sd = _stdev(returns)
    if mean is None or not sd:
        return None
    periods_per_year = TRADING_DAYS_PER_YEAR / HORIZON_BARS[horizon]
    return (mean / sd) * math.sqrt(periods_per_year)


def sortino(returns: list[float], horizon: str, *, target: float = SORTINO_TARGET) -> float | None:
    """Annualised Sortino — mean excess over the downside deviation only.

    `None` when nothing fell below the target. That is NOT an infinite Sortino: a sample with no
    losses is a sample too small or a window too kind, and printing `inf` (or a large number) there
    is how a lucky run gets read as a robust one.
    """
    mean = _mean(returns)
    if mean is None:
        return None
    downside = [r - target for r in returns if r < target]
    if not downside:
        return None
    deviation = math.sqrt(sum(d ** 2 for d in downside) / len(downside))
    if not deviation:
        return None
    periods_per_year = TRADING_DAYS_PER_YEAR / HORIZON_BARS[horizon]
    return ((mean - target) / deviation) * math.sqrt(periods_per_year)


def max_drawdown(returns: list[float]) -> float | None:
    """Peak-to-trough of the compounded equity path, as a POSITIVE fraction. `None` on an empty set.

    Compounded, not summed: a −50% followed by a +50% is not flat, and a ladder that reported it as
    flat would understate exactly the risk this metric exists to surface.
    """
    if not returns:
        return None
    equity = 1.0
    peak = 1.0
    worst = 0.0
    for r in returns:
        equity *= (1.0 + r)
        peak = max(peak, equity)
        worst = max(worst, (peak - equity) / peak if peak else 0.0)
    return worst


def rank_ic(pairs: list[tuple[float, float]]) -> float | None:
    """Spearman rank correlation between the model's `score` and the realised return.

    The question directional accuracy cannot answer: does the model's own ORDERING of its calls
    line up with what happened? A pipeline can be right 55% of the time and rank its convictions
    backwards, and `confidenceBucket` is ordinal precisely so that this is checkable.

    Hand-rolled with tie-averaged ranks, no scipy — `requirements.txt` has none and this file has
    kept to the stdlib since Wave 3. `None` when there is no dispersion on either side: a constant
    score correlates with nothing, and 0.0 would read as "measured, and there is no relationship".
    """
    if len(pairs) < 2:
        return None
    xs = _ranks([p[0] for p in pairs])
    ys = _ranks([p[1] for p in pairs])
    mean_x, mean_y = _mean(xs), _mean(ys)
    num = sum((x - mean_x) * (y - mean_y) for x, y in zip(xs, ys))
    den_x = math.sqrt(sum((x - mean_x) ** 2 for x in xs))
    den_y = math.sqrt(sum((y - mean_y) ** 2 for y in ys))
    if not den_x or not den_y:
        return None
    return num / (den_x * den_y)


def _ranks(values: list[float]) -> list[float]:
    """Tie-averaged ranks. Ties matter here: `score` takes six distinct values at most
    (±0.25/0.55/0.85 plus 0.0), so a competition-rank would distort every ladder result."""
    order = sorted(range(len(values)), key=lambda i: values[i])
    out = [0.0] * len(values)
    i = 0
    while i < len(order):
        j = i
        while j + 1 < len(order) and values[order[j + 1]] == values[order[i]]:
            j += 1
        average = (i + j) / 2.0 + 1.0
        for k in range(i, j + 1):
            out[order[k]] = average
        i = j + 1
    return out


def execution_rule(entries: list[tuple[float, float]], *, cost_bps: float) -> dict:
    """Return under a FIXED, stated execution rule — and it is stated here rather than tuned.

    THE RULE: take a unit long on `bullish`, a unit short on `bearish`, nothing on an abstention;
    hold exactly the horizon; pay `cost_bps` one way on every position CHANGE. No sizing by
    confidence, no stops, no leverage, no compounding into the next position. It is deliberately the
    dullest rule that could work, because the ladder's question is "does this input class add
    information", and a rule with parameters would let the parameters answer it.

    `entries` is `[(position, excessReturn)]` in cutoff order, position ∈ {-1, 0, +1}.

    NOTHING HERE PLACES AN ORDER. This is a simulation over resolved historical outcomes, in a
    service that has no venue, no credentials and no execution path of any kind (invariant #2).
    """
    if not entries:
        return {"trades": 0, "turnover": None, "costBps": cost_bps, "grossReturns": [],
                "netReturns": [], "meanNetReturn": None, "totalCost": 0.0}
    cost = cost_bps / 10_000.0
    gross: list[float] = []
    net: list[float] = []
    total_cost = 0.0
    changes = 0
    previous = 0.0
    for position, excess in entries:
        if position != previous:
            changes += 1
        # A round trip costs twice: once to reach the new position, once to leave it. Charging one
        # side would make every result look better than any of it could have been traded.
        charged = abs(position - previous) * cost
        total_cost += charged
        g = position * excess
        gross.append(g)
        net.append(g - charged)
        previous = position
    return {
        "trades": sum(1 for p, _ in entries if p != 0),
        # Position changes per opportunity — the honest denominator, since an abstention is an
        # opportunity the rule declined rather than one that did not exist.
        "turnover": changes / len(entries),
        "costBps": cost_bps,
        "grossReturns": gross,
        "netReturns": net,
        "meanGrossReturn": _mean(gross),
        "meanNetReturn": _mean(net),
        "totalCost": total_cost,
    }


def _position_of(entry: dict) -> float:
    if _abstained(entry):
        return 0.0
    direction = _direction_of(entry)
    return 1.0 if direction == "bullish" else -1.0 if direction == "bearish" else 0.0


def _direction_of(entry: dict) -> str:
    return str(entry.get("direction") or "unclear")


def _abstained(entry: dict) -> bool:
    return bool(entry.get("abstain")) or _direction_of(entry) in ABSTAIN_DIRECTIONS


def compute_metrics(records: list[dict], *, min_samples: int = DEFAULT_MIN_SAMPLES,
                    cost_bps: float = DEFAULT_COST_BPS,
                    calibration=None) -> dict:
    """The whole metric surface of this lane, computed from records alone.

    Deliberately minimal and 5A-extensible: directional accuracy per horizon, precision and recall by
    direction, ABSTENTION COVERAGE (the fraction of neutral/unclear calls, and accuracy conditional
    on not abstaining), and realised excess return per horizon.

    Two rules that are easy to get wrong and expensive to get wrong:

    * A pipeline that ALWAYS abstains has an abstention coverage of 1.0 and a directional accuracy of
      `None`. It does not have an accuracy of 0, and it is not `NO EDGE` — it made no calls. Refusing
      to guess is a legitimate output and the metrics must be able to represent it.
    * An outcome with no `excessReturn` is excluded from the return statistics rather than counted as
      0. A missing benchmark is not a zero benchmark (§9.26).

    WAVE 5A ADDS the rest of doc §11.2's battery: rank IC (does the model's own ordering of its
    convictions line up with what happened?), return under the fixed execution rule with costs,
    turnover, max drawdown, Sortino — and Brier, which is computed ONLY through an out-of-sample
    calibration. `calibration` is `ablation_calibration.Calibration | None`; `None` leaves
    `brier: null` and `brierBasis: "not calibrated"`, because the alternative is to invent a
    probability out of an ordinal bucket and score it, which would produce a number indistinguishable
    from a real one.
    """
    experiments = {r.get("experiment") for r in records}
    splits = {r.get("split") for r in records}
    if len(experiments) > 1 or len(splits) > 1:
        raise ValueError(
            f"records span more than one rung or split ({sorted(experiments)} x {sorted(splits)}); "
            "a metric over a mixed set belongs to neither"
        )

    # Records in CUTOFF ORDER, so the execution rule's position path and its turnover describe a
    # sequence rather than whatever order the store happened to return.
    ordered = sorted(records, key=lambda r: str(r.get("asOf") or ""))

    per_horizon: dict[str, dict] = {}
    for horizon in HORIZONS:
        calls = 0
        abstentions = 0
        correct = 0
        scored = 0
        excess: list[float] = []
        rank_pairs: list[tuple[float, float]] = []
        path: list[tuple[float, float]] = []
        brier_rows: list[tuple[float, bool]] = []
        confusion = {d: {"predicted": 0, "correct": 0, "actual": 0} for d in ("bullish", "bearish")}
        for rec in ordered:
            entry = ((rec.get("forecast") or {}).get("horizons") or {}).get(horizon)
            if not isinstance(entry, dict):
                continue
            outcome = (rec.get("outcomes") or {}).get(horizon) or {}
            calls += 1
            if _abstained(entry):
                abstentions += 1
            if not outcome.get("resolved"):
                continue
            realized = outcome.get("realizedReturn")
            if realized is None:
                continue
            actual = "bullish" if realized > 0 else "bearish" if realized < 0 else "flat"
            if actual in confusion:
                confusion[actual]["actual"] += 1

            # The execution path and the rank IC include ABSTENTIONS, before the abstention filter
            # below. An abstention is a position of zero and a score of zero: it is a decision the
            # rule made and the ordering has to account for, and dropping it would measure the
            # pipeline's calls while pretending its silences never happened.
            if outcome.get("excessReturn") is not None:
                path.append((_position_of(entry), float(outcome["excessReturn"])))
            score = entry.get("score")
            if isinstance(score, (int, float)) and not isinstance(score, bool):
                rank_pairs.append((float(score), float(realized)))

            if _abstained(entry):
                continue
            predicted = _direction_of(entry)
            scored += 1
            if calibration is not None and actual != "flat":
                probability = _calibrated_probability(calibration, rec, entry, horizon)
                if probability is not None:
                    brier_rows.append((probability, predicted == actual))
            if predicted in confusion:
                confusion[predicted]["predicted"] += 1
                if predicted == actual:
                    confusion[predicted]["correct"] += 1
            if predicted == actual:
                correct += 1
            if outcome.get("excessReturn") is not None:
                excess.append(float(outcome["excessReturn"]))

        execution = execution_rule(path, cost_bps=cost_bps)

        by_direction = {}
        for direction, counts in confusion.items():
            by_direction[direction] = {
                "precision": (counts["correct"] / counts["predicted"]) if counts["predicted"] else None,
                "recall": (counts["correct"] / counts["actual"]) if counts["actual"] else None,
                "predicted": counts["predicted"],
                "actual": counts["actual"],
            }

        per_horizon[horizon] = {
            "calls": calls,
            "abstentions": abstentions,
            # 1.0 for a pipeline that always abstains — a number, not a crash.
            "abstentionCoverage": (abstentions / calls) if calls else None,
            "scored": scored,
            "directionalAccuracy": (correct / scored) if scored else None,
            "byDirection": by_direction,
            "meanExcessReturn": _mean(excess),
            "excessReturnSamples": len(excess),
            "sharpe": sharpe(excess, horizon),
            # ---- Wave 5A: the rest of doc §11.2's battery -------------------------------------
            # Rank IC over EVERY call with a score, abstentions included — see the note above.
            "rankIC": rank_ic(rank_pairs),
            "rankICSamples": len(rank_pairs),
            "sortino": sortino(excess, horizon),
            "maxDrawdown": max_drawdown(execution["netReturns"]),
            "execution": {k: v for k, v in execution.items()
                          if k not in ("grossReturns", "netReturns")},
            # Brier is `None` unless an out-of-sample calibration supplied the probabilities. The
            # basis string travels with it so a reader never has to guess which it is.
            "brier": _brier_of(brier_rows),
            "brierSamples": len(brier_rows),
            "brierBasis": ("out-of-sample calibration fitted on "
                           f"{getattr(calibration, 'fitted_on_split', None)!r}"
                           if calibration is not None else
                           "not calibrated — no probability exists to score, and inventing one "
                           "from an ordinal bucket would produce a number that looks real"),
        }

    return {
        "experiment": next(iter(experiments), None),
        "split": next(iter(splits), None),
        "records": len(records),
        "minSamples": min_samples,
        "costBps": cost_bps,
        "calibrated": calibration is not None,
        "perHorizon": per_horizon,
        # WHICH RUNTIME PRODUCED THE RECORDS THIS METRIC DESCRIBES. Counted from the stored
        # `identity.modelUsed`, so it survives the round trip through the events store — the
        # `runtime` block travels on the live envelope, not on the persisted §5 record.
        "runtimes": runtimes_in(records),
    }


def _calibrated_probability(calibration, record: dict, entry: dict, horizon: str):
    """Delegate to `ablation_calibration`, imported lazily so this module keeps working (and its
    tests keep running) when only the metric half is exercised."""
    from .ablation_calibration import probability_for

    return probability_for(calibration, record, entry, horizon)


def _brier_of(rows: list[tuple[float, bool]]) -> float | None:
    """`None`, never 0.0, on an empty set: 0.0 is a PERFECT Brier score and would read as the best
    possible result for a rung that produced no scorable call at all."""
    if not rows:
        return None
    return sum((p - (1.0 if hit else 0.0)) ** 2 for p, hit in rows) / len(rows)


def runtimes_in(records: list[dict]) -> dict[str, int]:
    """`{runtime label: count}` over stored records, from `identity.modelUsed`."""
    counts: dict[str, int] = {}
    for rec in records:
        label = str(((rec.get("identity") or {}).get("modelUsed") or "unknown")).strip()
        counts[label] = counts.get(label, 0) + 1
    return counts


def stub_records(records: list[dict]) -> list[dict]:
    """Every record whose analyst answer came from a stub — the read-back twin of
    `assert_real_runtime`, and the reason a run cannot be laundered by restarting it halfway.

    `run_rung` refuses at write time, but a store can hold records written by an earlier run, by a
    different operator, or before the runtime was configured. `evaluate_rung` reads the store, so it
    checks the store, exactly as `synthetic_outcomes` does for §9.46.
    """
    return [
        {"predictionId": rec.get("id"), "ticker": rec.get("ticker"), "asOf": rec.get("asOf"),
         "modelUsed": (rec.get("identity") or {}).get("modelUsed")}
        for rec in records
        if is_stub_runtime(str((rec.get("identity") or {}).get("modelUsed") or ""))
    ]


# =================================================================================================
# Stage 6 — the refusal and the verdict
# =================================================================================================


def synthetic_outcomes(records: list[dict]) -> list[dict]:
    """Every outcome in the run whose resolving candle was synthetic — a LOCAL check (§9.25).

    The `resolved_from_source` the events store recorded travels with the record, so this is a scan
    over data already in hand, not a second cross-service call. A substring test, not equality:
    `/candles` labels a mixed window with every distinct source joined by '+', so an equality test
    against 'synthetic' would miss 'yfinance+synthetic'.
    """
    found = []
    for rec in records:
        for horizon, outcome in (rec.get("outcomes") or {}).items():
            source = str((outcome or {}).get("resolvedFromSource") or "").lower()
            if SYNTHETIC_SOURCE in source:
                found.append({
                    "predictionId": rec.get("id"),
                    "ticker": rec.get("ticker"),
                    "horizon": horizon,
                    "resolvedFromSource": outcome.get("resolvedFromSource"),
                })
    return found


def verdict_for(metrics: dict) -> tuple[str, list[str]]:
    """`EDGE | NO EDGE | INCONCLUSIVE | SUSPECT` and the reasons behind it.

    The vocabulary is fixed by docs/VALIDATION_AND_GO_LIVE.md and is not extended here. Order
    matters: an implausible Sharpe is reported as SUSPECT even when every other gate passes, because
    a great-looking result that is lying is the worst outcome of all.
    """
    reasons: list[str] = []
    pooled_scored = sum(h["scored"] for h in metrics["perHorizon"].values())
    pooled_calls = sum(h["calls"] for h in metrics["perHorizon"].values())
    pooled_abstentions = sum(h["abstentions"] for h in metrics["perHorizon"].values())
    if pooled_calls and pooled_abstentions == pooled_calls:
        # Checked BEFORE the sample-size gate: "every call abstained" and "too few calls" are
        # different findings, and the first is the more useful one. Refusing to take a side is a
        # legitimate output, not a failure to reach a sample size.
        reasons.append(
            f"every one of the {pooled_calls} calls abstained (neutral/unclear): there is nothing "
            "to be right or wrong about"
        )
        return "INCONCLUSIVE", reasons
    if pooled_scored < metrics["minSamples"]:
        reasons.append(
            f"{pooled_scored} scored calls across all horizons, below the named minimum of "
            f"{metrics['minSamples']}"
        )
        return "INCONCLUSIVE", reasons

    sharpes = [h["sharpe"] for h in metrics["perHorizon"].values() if h["sharpe"] is not None]
    if any(s > SHARPE_SUSPECT_ABOVE for s in sharpes):
        reasons.append(
            f"realised Sharpe {max(sharpes):.2f} exceeds {SHARPE_SUSPECT_ABOVE} — in this domain "
            "that is leakage or a bug far more often than an edge"
        )
        return "SUSPECT", reasons

    accuracies = [
        h["directionalAccuracy"] for h in metrics["perHorizon"].values()
        if h["directionalAccuracy"] is not None
    ]
    excesses = [
        h["meanExcessReturn"] for h in metrics["perHorizon"].values()
        if h["meanExcessReturn"] is not None
    ]
    if not accuracies:
        reasons.append("every call abstained: there is nothing to be right or wrong about")
        return "INCONCLUSIVE", reasons

    beats_coin = all(a > 0.5 for a in accuracies)
    positive_excess = bool(excesses) and all(e > 0 for e in excesses)
    if not beats_coin:
        reasons.append("directional accuracy is not above 0.5 on every horizon")
    if not positive_excess:
        reasons.append("mean excess return over the benchmark is not positive on every horizon")
    if beats_coin and positive_excess:
        reasons.append("every horizon beat a coin flip and carried a positive mean excess return")
        return "EDGE", reasons
    return "NO EDGE", reasons


# =================================================================================================
# Stage 7 — the report
# =================================================================================================


def render_markdown(result: dict) -> str:
    lines = [
        f"# Ablation ladder — rung {result['experiment']} · split {result['split']}",
        "",
        f"- Generated: {result['generatedAt']}",
        f"- Cutoff set: `{result['cutoffFingerprint']}` ({len(result['cutoffs'])} cutoffs)",
        f"- Universe: {', '.join(result['universe'])}",
        f"- Split version: `{SPLIT_VERSION}`",
        f"- Records: {result['metrics']['records']}",
        f"- Runtimes that produced these records: "
        f"`{result['metrics'].get('runtimes') or {}}`",
        f"- Execution-rule cost: {result.get('costBps', DEFAULT_COST_BPS)} bps per side",
        "",
    ]
    if result.get("stubRefusal"):
        lines += [
            "## STUB-SERVED RECORDS — cannot validate",
            "",
            "No verdict is produced. Records in this set were produced by `stub:offline` or another "
            "`stub:*` label, which is a FIXTURE: it has never disagreed with the schema, never "
            "emitted an unserved number and never triggered an override branch. A verdict measured "
            "against it would license the `direction` word (§9.20) on nothing.",
            "",
        ]
        for row in result["stubRefusal"][:20]:
            lines.append(f"- `{row['predictionId']}` {row['ticker']} {row['asOf']} "
                         f"⇐ {row['modelUsed']}")
        lines.append("")
        return "\n".join(lines)

    if result.get("refusal"):
        lines += [
            "## SYNTHETIC DATA — cannot validate",
            "",
            "No verdict is produced. Outcomes in this run were resolved against synthetic bars, and "
            "a number computed on a synthetic bar is a fabricated result that looks exactly like a "
            "real one (contract §9.46).",
            "",
        ]
        for row in result["refusal"][:20]:
            lines.append(f"- `{row['predictionId']}` {row['ticker']} {row['horizon']} "
                         f"⇐ {row['resolvedFromSource']}")
        lines.append("")
        return "\n".join(lines)

    lines += [
        f"## Verdict: {result['verdict']}",
        "",
        VERDICT_MEANING[result["verdict"]],
        "",
        "**`EDGE` is a necessary condition, never a green light** "
        "(docs/VALIDATION_AND_GO_LIVE.md).",
        "",
    ]
    for reason in result["reasons"]:
        lines.append(f"- {reason}")
    lines += ["", "## Per horizon", "",
              "| horizon | calls | abstained | coverage | scored | accuracy | mean excess | Sharpe |",
              "|---|---|---|---|---|---|---|---|"]
    for horizon, h in result["metrics"]["perHorizon"].items():
        lines.append(
            f"| {horizon} | {h['calls']} | {h['abstentions']} | {_fmt(h['abstentionCoverage'])} | "
            f"{h['scored']} | {_fmt(h['directionalAccuracy'])} | {_fmt(h['meanExcessReturn'])} | "
            f"{_fmt(h['sharpe'])} |"
        )

    lines += ["", "## Risk, ranking and the execution rule", "",
              "| horizon | rank IC | Sortino | max DD | turnover | mean net | total cost | Brier |",
              "|---|---|---|---|---|---|---|---|"]
    for horizon, h in result["metrics"]["perHorizon"].items():
        ex = h.get("execution") or {}
        lines.append(
            f"| {horizon} | {_fmt(h.get('rankIC'))} | {_fmt(h.get('sortino'))} | "
            f"{_fmt(h.get('maxDrawdown'))} | {_fmt(ex.get('turnover'))} | "
            f"{_fmt(ex.get('meanNetReturn'))} | {_fmt(ex.get('totalCost'))} | "
            f"{_fmt(h.get('brier'))} |"
        )
    first = next(iter(result["metrics"]["perHorizon"].values()), {})
    lines += [
        "",
        f"Brier basis: {first.get('brierBasis', 'not calibrated')}.",
        "",
        "The execution rule is FIXED and stated, not tuned: unit long on `bullish`, unit short on "
        "`bearish`, flat on an abstention, held exactly the horizon, "
        f"{result.get('costBps', DEFAULT_COST_BPS)} bps charged on every position change. It is "
        "deliberately the dullest rule that could work — the ladder's question is whether an input "
        "class adds information, and a rule with parameters would let the parameters answer it. "
        "**Nothing here places an order** (invariant #2).",
        "",
    ]

    if result.get("calibration"):
        cal = result["calibration"]
        lines += [
            "## Calibration (out-of-sample)",
            "",
            f"- Fitted on `{cal.get('fittedOnSplit')}`, scored on `{cal.get('scoredSplit')}`",
            f"- Scored {cal.get('scored')} of {cal.get('considered')} calls "
            f"(coverage {_fmt(cal.get('coverage'))})",
            f"- Expected calibration error: {_fmt(cal.get('expectedCalibrationError'))}",
            f"- Brier {_fmt(cal.get('brier'))} · skill score {_fmt(cal.get('brierSkillScore'))}",
            "",
            "Coverage is the honesty number: a good ECE over a small calibrated subset is not a "
            "calibrated pipeline. Buckets below the sample floor stay uncalibrated rather than "
            "being smoothed.",
            "",
        ]

    lines += [
        "",
        "_Research tool. No buy/sell output, no orders, no money. Rung A is not built (requires a "
        "VL model). Rung F IS runnable as of Wave 5A — both halves of its input class now exist "
        "point-in-time — but breadth and implied volatility are null in this deployment, and a "
        "bundle whose `market_sector` class is entirely null is refused rather than reported as F._",
        "",
    ]
    return "\n".join(lines)


def _fmt(value) -> str:
    return "—" if value is None else f"{value:.4f}"


# =================================================================================================
# Stage 7b — THE SERVED VERDICT (§9.61). Producing the number is not the exit criterion.
# =================================================================================================

#: The one filename `services/llm/app/ablation_verdict.py` reads. Stated in both files and asserted
#: by a test in each, because a path agreed by comment is a path that drifts.
VERDICT_FILENAME = "verdict.json"

#: The only verdict that opens §9.20's gate. `EDGE` is a NECESSARY condition and never a green
#: light — what it licenses is the rendering of one word, beside its track record, on a rung and
#: horizon that earned it.
VALIDATING_VERDICT = "EDGE"

#: The verdicts that constitute a MEASUREMENT, and are therefore the only ones written to the served
#: file. This distinction is §9.61 binding 4 in code and it is easy to get wrong:
#:
#:   * `validated: false` means "the ablation RAN and did not license the direction". Evidence.
#:   * an ABSENT verdict means "no ablation has run for this rung and horizon". Not evidence.
#:
#: `INCONCLUSIVE` is "there were too few resolved outcomes to say anything" and `SUSPECT` is "these
#: numbers are probably a leak — stop". Neither is a measurement, so neither may be served as
#: `validated: false`: doing so would tell the browser the ladder had answered when it had not.
MEASURED_VERDICTS = ("EDGE", "NO EDGE")


def horizon_verdicts(result: dict) -> dict:
    """Per horizon: `{validated, rung, horizon}` — §9.61 binding 1's shape, binding 2's content.

    PER HORIZON, because §9.20 gates per rung AND horizon. A single run-level boolean would license
    the `direction` word on a 60-day thesis because a 1-day one cleared the bar, which binding 3
    calls opening the gate on a coincidence.

    `validated: false` IS WRITTEN. Binding 4: the ablation ran and did not license the direction —
    a real answer, and not the same as the absent verdict that means it never ran.
    """
    rung = result.get("experiment")
    verdict = result.get("verdict")
    out: dict[str, dict] = {}
    for horizon, h in (result.get("metrics") or {}).get("perHorizon", {}).items():
        accuracy = h.get("directionalAccuracy")
        excess = h.get("meanExcessReturn")
        validated = bool(
            verdict == VALIDATING_VERDICT
            and accuracy is not None and accuracy > 0.5
            and excess is not None and excess > 0
            and h.get("scored", 0) > 0
        )
        out[horizon] = {
            "validated": validated,
            "rung": rung,
            "horizon": horizon,
            # Everything below is EVIDENCE, not part of §9.61's three-key contract. The client
            # matches on the three; a human reading the file needs to see what stood behind them.
            "verdict": verdict,
            "split": result.get("split"),
            "scored": h.get("scored"),
            "directionalAccuracy": accuracy,
            "meanExcessReturn": excess,
            "cutoffFingerprint": result.get("cutoffFingerprint"),
            "generatedAt": result.get("generatedAt"),
            "runtimes": (result.get("metrics") or {}).get("runtimes"),
        }
    return out


def write_verdict(result: dict, verdict_dir: str) -> str | None:
    """Write (or merge into) `{verdict_dir}/verdict.json`, the file the llm service serves from.

    MERGED, not overwritten: the ladder is run one rung at a time and a rung's verdict must not
    delete the rung above it. Keyed by `rung|horizon`.

    Returns `None` when the run produced no MEASUREMENT. That covers three cases and they are all
    the same case: a refusal (synthetic data, a stub-served record), `INCONCLUSIVE` (too few
    resolved outcomes to say anything), and `SUSPECT` (the numbers are probably a leak). None of
    them may be written as `validated: false`, because `false` is the answer "we measured and it did
    not clear the bar" — and we did not measure. §9.61 binding 4 exists to keep those two apart, and
    writing an unmeasured `false` would destroy the distinction at the only place it is observable.
    """
    if result.get("refusal") or result.get("stubRefusal") or result.get("plumbingOnly"):
        return None
    if result.get("verdict") not in MEASURED_VERDICTS:
        return None
    path = os.path.join(verdict_dir, VERDICT_FILENAME)
    existing: dict = {}
    if _db.enabled():
        raw = _db.load_artifact("ablation-verdict.json")
        if raw:
            try:
                existing = (json.loads(raw).get("verdicts") or {})
            except (TypeError, ValueError):
                existing = {}
    else:
        os.makedirs(verdict_dir, exist_ok=True)
        try:
            with open(path, encoding="utf-8") as fh:
                loaded = json.load(fh)
                existing = loaded.get("verdicts") or {}
        except (OSError, ValueError):
            existing = {}
    for horizon, row in horizon_verdicts(result).items():
        existing[f"{row['rung']}|{horizon}"] = row
    payload = {
        "schema": "ablation-verdict@1",
        "writtenAt": result.get("generatedAt"),
        "splitVersion": SPLIT_VERSION,
        "note": (
            "Read by services/llm/app/ablation_verdict.py and stamped onto each §7 object as "
            "`ablationVerdict: {validated, rung, horizon}` (contract §9.61). An ABSENT entry means "
            "no ablation has run for that rung and horizon; `validated: false` means one ran and "
            "did not license the direction. Both withhold the word; only one is evidence."
        ),
        "verdicts": existing,
    }
    if _db.enabled():
        encoded = json.dumps(payload, indent=2, sort_keys=True).encode()
        _db.save_artifact("ablation-verdict.json", "application/json", encoded)
    else:
        with open(path, "w", encoding="utf-8") as fh:
            json.dump(payload, fh, indent=2, sort_keys=True)
    return path


def write_report(result: dict, out_dir: str) -> dict:
    """Mirrors where `app.evaluate` already writes: data/eval/ablation-*.{md,json}."""
    stem = f"ablation-{result['experiment']}-{result['split']}-{result['generatedAt'].replace(':', '')}"
    md_path = os.path.join(out_dir, f"{stem}.md")
    json_path = os.path.join(out_dir, f"{stem}.json")
    markdown = render_markdown(result)
    encoded = json.dumps(result, indent=2, sort_keys=True).encode()
    if _db.enabled():
        _db.save_artifact(os.path.basename(md_path), "text/markdown", markdown.encode())
        _db.save_artifact(os.path.basename(json_path), "application/json", encoded)
    else:
        os.makedirs(out_dir, exist_ok=True)
        with open(md_path, "w", encoding="utf-8") as fh:
            fh.write(markdown)
        with open(json_path, "w", encoding="utf-8") as fh:
            fh.write(encoded.decode())
    return {"md": md_path, "json": json_path}


# =================================================================================================
# Stage 8 — the run
# =================================================================================================


def evaluate_rung(
    rung: str,
    cfg: AblationConfig,
    *,
    store,
    now: str | None = None,
    calibration_source=None,
) -> tuple[dict, int]:
    """Read one rung's records back, measure them, and return `(result, exit_code)`.

    Separated from `run_rung` on purpose: writing predictions and measuring outcomes are different
    acts at different times — a 60-day horizon written today cannot be measured for three months —
    and Wave 5A measures far more than it writes.

    `calibration_source(rung, split) -> list[dict]` is injected so the calibration's FIT records
    come from a different split than the ones being scored. `None` (the default) means no
    calibration, and every Brier comes back `null` with the reason stated — never a probability
    invented from an ordinal bucket.
    """
    records = collect_records(store, experiment=rung, split=cfg.split)
    requested_cells = {
        (ticker.upper(), _iso(cutoff))
        for ticker in cfg.universe for cutoff in cfg.cutoffs
        if split_for(cutoff) == cfg.split
    }
    # The store is shared across ladder invocations. Score only the grid named by this invocation;
    # otherwise an older run on a different cutoff set silently changes today's result.
    records = [
        row for row in records
        if (str(row.get("ticker") or "").upper(), _iso(row.get("asOf"))) in requested_cells
    ]
    generated_at = now or datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

    # The calibration is fitted on a DIFFERENT split's records and applied here. Fitting it on the
    # split being scored would recover the training frequencies; `ablation_calibration.apply_to`
    # refuses that, and this is the call site that keeps the two splits apart.
    calibration = None
    calibration_report = None
    if calibration_source is not None and cfg.calibration_split != cfg.split:
        from .ablation_calibration import LeakageRefused, apply_to, fit

        fit_records = calibration_source(rung, cfg.calibration_split)
        if fit_records:
            calibration = fit(fit_records, split=cfg.calibration_split, experiment=rung,
                              now=generated_at)
            try:
                calibration_report = apply_to(calibration, records, split=cfg.split)
            except LeakageRefused as exc:  # pragma: no cover - guarded by the branch above
                calibration, calibration_report = None, {"refused": str(exc)}

    result = {
        "experiment": rung,
        "split": cfg.split,
        "splitVersion": SPLIT_VERSION,
        "generatedAt": generated_at,
        "cutoffs": [_iso(c) for c in cfg.cutoffs],
        "cutoffFingerprint": cutoff_fingerprint(cfg.cutoffs),
        "universe": list(cfg.universe),
        "rungInputs": list(RUNG_INPUTS.get(rung, ())),
        "costBps": cfg.cost_bps,
        "calibration": calibration_report,
        "plumbingOnly": not cfg.require_real_runtime,
        "metrics": compute_metrics(records, min_samples=cfg.min_samples,
                                   cost_bps=cfg.cost_bps, calibration=calibration),
    }

    # THE RUNTIME REFUSAL, checked at read-back as well as at write time. A store can hold records
    # from an earlier run, another operator, or a window before the runtime was configured, and a
    # verdict pooled across a real model and a fixture belongs to neither.
    stubbed = stub_records(records) if cfg.require_real_runtime else []
    if stubbed:
        result["stubRefusal"] = stubbed
        result["verdict"] = None
        result["reasons"] = []
        return result, EXIT_STUB_RUNTIME

    refusal = synthetic_outcomes(records)
    if refusal:
        # The refusal IS the output. No verdict, not even a partial one, and not a verdict
        # annotated as synthetic.
        result["refusal"] = refusal
        result["verdict"] = None
        result["reasons"] = []
        return result, EXIT_SYNTHETIC

    if not records:
        result["verdict"] = "INCONCLUSIVE"
        result["reasons"] = ["no prediction records for this rung and split"]
        return result, EXIT_NO_DATA

    verdict, reasons = verdict_for(result["metrics"])
    result["verdict"] = verdict
    result["reasons"] = reasons
    return result, EXIT_OK


def print_result(result: dict) -> None:
    bar = "=" * 64
    print(bar)
    if result.get("stubRefusal"):
        print("  STUB-SERVED RECORDS — cannot validate")
        print(bar)
        print(f"  {len(result['stubRefusal'])} record(s) in this set were produced by a stub, not")
        print("  by a model. `stub:offline` is a fixture: it has never disagreed with the schema,")
        print("  never emitted an unserved number and never triggered an override branch. A verdict")
        print("  measured against it would license the `direction` word on nothing, so none is")
        print("  produced. Set MODEL_RUNTIME_URL + MODEL_RUNTIME_MODEL on the llm service, re-run")
        print("  the ladder, and try again.")
        print(bar)
        return
    if result.get("refusal"):
        print("  SYNTHETIC DATA — cannot validate")
        print(bar)
        print(f"  {len(result['refusal'])} outcome(s) in this run were resolved against a synthetic")
        print("  bar. A verdict computed on synthetic data would be meaningless, so none is")
        print("  produced. Bring up the analysis service with a real price provider (yfinance needs")
        print("  no key, just network), re-run the backfill, and try again.")
        print(bar)
        return
    print(f"  Ablation rung {result['experiment']} · split {result['split']} · "
          f"cutoffs {result['cutoffFingerprint']}")
    print(bar)
    print(f"  VERDICT: {result['verdict']}")
    for reason in result["reasons"]:
        print(f"    - {reason}")
    print()
    print(f"  {'horizon':<9}{'calls':>7}{'abst':>7}{'cover':>8}{'scored':>8}{'acc':>8}"
          f"{'excess':>10}{'Sharpe':>9}")
    for horizon, h in result["metrics"]["perHorizon"].items():
        print(f"  {horizon:<9}{h['calls']:>7}{h['abstentions']:>7}{_fmt(h['abstentionCoverage']):>8}"
              f"{h['scored']:>8}{_fmt(h['directionalAccuracy']):>8}"
              f"{_fmt(h['meanExcessReturn']):>10}{_fmt(h['sharpe']):>9}")
    print()
    print(f"  {'horizon':<9}{'rankIC':>9}{'Sortino':>9}{'maxDD':>9}{'turnovr':>9}"
          f"{'meanNet':>10}{'Brier':>9}")
    for horizon, h in result["metrics"]["perHorizon"].items():
        ex = h.get("execution") or {}
        print(f"  {horizon:<9}{_fmt(h.get('rankIC')):>9}{_fmt(h.get('sortino')):>9}"
              f"{_fmt(h.get('maxDrawdown')):>9}{_fmt(ex.get('turnover')):>9}"
              f"{_fmt(ex.get('meanNetReturn')):>10}{_fmt(h.get('brier')):>9}")
    print()
    print("  EDGE is a NECESSARY condition, never a green light.")
    print(bar)


def main(argv: list[str] | None = None) -> int:
    cfg = AblationConfig.from_env()
    requested = [r.strip().upper() for r in (argv or cfg.rungs) if str(r).strip()]

    for rung in requested:
        status, reason = rung_status(rung)
        if status != RUNNABLE:
            print("=" * 64)
            print(f"  RUNG {rung} IS NOT AVAILABLE — {status}")
            print("=" * 64)
            print(f"  {reason}")
            print("=" * 64)
            return EXIT_RUNG_UNAVAILABLE

    if not cfg.cutoffs:
        print("=" * 64)
        print("  NO CUTOFF SET — cannot run")
        print("=" * 64)
        print("  Set ABLATE_CUTOFFS to a comma-separated list of RFC3339 cutoffs. Every rung runs")
        print("  against the IDENTICAL set (contract §5.1); there is no default, because a default")
        print("  cutoff set would silently differ between two runs.")
        return EXIT_NO_DATA

    # Every rung, the same cutoffs — asserted, not assumed.
    try:
        assert_identical_cutoff_set({rung: cfg.cutoffs for rung in requested})
    except CutoffSetMismatch as exc:
        print(f"  REFUSED: {exc}")
        return EXIT_CUTOFF_MISMATCH

    # SIZE THE LADDER FIRST, IN RUNS. Printed before anything is measured, so an operator sees the
    # bill before the box starts paying it — and so the wall-clock estimate is visibly a
    # multiplication waiting on 5C's `secondsPerAnalystRun` rather than a number nobody measured.
    plan = plan_runs(cfg, requested)
    print("=" * 64)
    print(f"  LADDER SIZE — {plan['analystRuns']} analyst runs "
          f"({plan['runsPerRung']} per rung x {len(plan['rungs'])} rungs)")
    print("=" * 64)
    print(f"  cutoffs in split {plan['split']}: {plan['cutoffsInSplit']} of "
          f"{plan['cutoffsRequested']} requested · universe {plan['universe']}")
    print(f"  model generations (upper bound, {plan['generationsPerRun']} per run): "
          f"{plan['modelGenerations']}")
    print(f"  wall clock: {plan['wallClock']}")
    print("=" * 64)

    # THE RUNTIME PRE-FLIGHT. Cheap, and it turns "the ladder ran for six hours and produced a
    # stub-served verdict" into one line printed before anything started.
    if cfg.require_real_runtime:
        ok, detail = runtime_precheck(cfg)
        if not ok:
            print("  REFUSED: no model runtime is configured on the llm service.")
            print(f"           {detail}")
            print("  A ladder measured against `stub:offline` produces a verdict that licenses the")
            print("  `direction` word (§9.20) on nothing. Set MODEL_RUNTIME_URL and")
            print("  MODEL_RUNTIME_MODEL (they have no defaults, deliberately) and re-run.")
            print("  ABLATE_REQUIRE_REAL_RUNTIME=false runs the PLUMBING only and can never")
            print("  produce a verdict a `direction` word may be gated on.")
            return EXIT_STUB_RUNTIME
        print(f"  runtime pre-flight ok — {detail}")

    store = PredictionStoreClient(cfg.events_url, timeout=cfg.timeout)
    inputs = AblationInputClient(cfg.analysis_url, cfg.events_url, timeout=cfg.timeout)
    analyst_client = AnalystHTTPClient(cfg.llm_url, timeout=cfg.analyst_timeout)

    # The calibration's FIT records come from a different split than the one being scored. The
    # source is a closure over the same store, so nothing here reads a second data path.
    def calibration_source(rung: str, split: str) -> list[dict]:
        try:
            return collect_records(store, experiment=rung, split=split)
        except Exception:  # noqa: BLE001 — no fit set means no calibration, not a failed run
            return []

    worst = EXIT_OK
    for rung in requested:
        try:
            run_report = run_rung(
                rung, cfg, analyst=analyst_client.analyse, store=store, fetch=inputs.fetch,
            )
        except StubRuntimeRefused as exc:
            print(f"  REFUSED: rung {rung} reached a stub runtime: {exc}")
            return EXIT_STUB_RUNTIME
        print(
            f"  Generated rung {rung}: {run_report.written} record(s) written, "
            f"{len(run_report.skipped)} cell(s) skipped"
        )
        try:
            backfill = store.backfill(experiment=rung)
        except Exception as exc:  # noqa: BLE001 — unresolved outcomes cannot support a verdict
            print(f"  REFUSED: rung {rung} outcomes could not be resolved: {type(exc).__name__}: {exc}")
            worst = max(worst, EXIT_NO_DATA)
            continue
        print(
            f"  Resolved rung {rung}: {backfill.get('updated', 0)} outcome(s), "
            f"{backfill.get('pending', 0)} pending, {backfill.get('refused', 0)} refused"
        )
        try:
            result, code = evaluate_rung(rung, cfg, store=store,
                                         calibration_source=calibration_source)
        except Exception as exc:  # noqa: BLE001 — an unreachable store is a refusal, not a crash
            print(f"  REFUSED: rung {rung} could not be measured: {type(exc).__name__}: {exc}")
            worst = max(worst, EXIT_NO_DATA)
            continue
        files = write_report(result, cfg.out_dir)
        print_result(result)
        print(f"  Report written: {files['md']}")
        print(f"                  {files['json']}")
        # §9.61 BINDING 5. Producing the number is not the exit criterion; the browser being able
        # to read it is. A refused run writes nothing — see `write_verdict`.
        verdict_path = write_verdict(result, cfg.verdict_dir)
        if verdict_path:
            print(f"  Verdict served: {verdict_path}")
            print("                  (read by the llm service and stamped onto each §7 object as")
            print("                   `ablationVerdict`; the browser gate opens from that field)")
        worst = max(worst, code)
    return worst


if __name__ == "__main__":
    sys.exit(main())
