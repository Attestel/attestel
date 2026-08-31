"""Offline edge-evaluation harness — the Stage-1 test that returns a verdict: EDGE / NO EDGE /
INCONCLUSIVE (or SUSPECT).

This is a BATCH CLI, not a request endpoint. It answers one question honestly: does the directional
model have a real, tradeable edge on REAL data, once you pool trades across a whole universe of
liquid names, subtract costs, hold out an untouched final slice, and compare against both
buy-and-hold and a permutation null? It produces NO buy/sell output and touches no money — it is a
research instrument that prints a conclusion and writes a report.

Design commitments (see docs/PREDICTION_MODELS.md, docs/VALIDATION_AND_GO_LIVE.md):
  * REAL DATA ONLY. If any ticker resolves to the synthetic seed, it REFUSES a verdict and exits
    non-zero — a number produced on synthetic data is a lie, so we never produce one.
  * NO LOOK-AHEAD. It reuses the EXISTING leakage-safe feature/label/walk-forward code verbatim
    (features.build_dataset, model.walk_forward, model._fit_predict) and reserves an untouched final
    holdout that no fold trains on. Costs are subtracted on every position change.
  * HONEST THRESHOLDS. A pooled Sharpe > 3 is flagged SUSPECT (probable leakage), never celebrated.
    EDGE is granted only when EVERY gate passes; otherwise the honest answer "NO EDGE" is stated
    plainly, because "there is no tradeable edge here" is a real and valuable finding.
  * HONEST POOLING (method `portfolio-v4`). Statistical metrics come from a DATE-ALIGNED equal-weight
    portfolio of the per-(ticker, horizon) streams, not from splicing those streams end to end. The
    old concatenation counted the same trading day across thirty correlated names as thirty
    independent observations, double-counted a ticker's returns whenever two horizons shared it, and
    drew an equity curve of a series no portfolio ever walked. Each label horizon is judged as its
    own portfolio and gets its own verdict; a combined-horizons number, if shown at all, is labelled
    OVERLAPPING and read by no check.

Run:
    cd services/prediction && python -m app.evaluate          # (or: make evaluate)
It is long-running: universe x horizons x walk-forward folds are LightGBM fits. Shrink EVAL_UNIVERSE
/ EVAL_PERMUTATIONS / EVAL_HISTORY_DAYS for a quick pass (see the report header for how long it took).
"""
from __future__ import annotations

import json
import math
import os
import sys
from dataclasses import dataclass
from datetime import datetime, timezone

import numpy as np

from .backtest import _downsample, annualization, derive_positions, max_drawdown, net_returns, run_backtest
from .config import ANALYSIS_URL, COMMISSION_BPS, SLIPPAGE_BPS
from .context import fetch_context, load_earnings
from .features import FEATURE_FRAME_POLICY, build_dataset, fetch_feature_frame
from .model import DEFAULT_LOWER, DEFAULT_UPPER, MIN_ROWS, _fit_predict, walk_forward
from .strategy import strategy_version
from .verdicts import EVALUATION_METHOD, EVIDENCE_FLOORS, write_verdict
from . import db

# ~30 liquid large caps — enough independent streams to pool for statistical power. A single ticker
# cannot answer "is there an edge"; the pooled universe can.
DEFAULT_UNIVERSE = (
    "NVDA,GOOGL,TSLA,AAPL,MSFT,AMZN,META,AMD,AVGO,NFLX,JPM,V,MA,COST,PEP,KO,"
    "XOM,CVX,UNH,JNJ,WMT,HD,PG,DIS,BA,CAT,CRM,ORCL,ADBE,QCOM"
)

# Exit codes (0 = a verdict was produced, even NO EDGE / INCONCLUSIVE / SUSPECT).
EXIT_OK = 0
EXIT_SYNTHETIC = 2   # refused: synthetic data
EXIT_NO_DATA = 3     # refused: nothing fetched (network down / analysis unreachable)

VERDICT_MEANING = {
    "EDGE": (
        "The pooled out-of-sample trades show a positive edge after costs that SURVIVES an untouched "
        "holdout, BEATS buy-and-hold risk-adjusted, and BEATS a permutation null (p<0.05). This is "
        "necessary, not sufficient. NEXT: paper-trade it (Stage 2) for several weeks and confirm live "
        "≈ backtest before ANY real money — see docs/VALIDATION_AND_GO_LIVE.md. Do not size up on this alone."
    ),
    "NO EDGE": (
        "The pooled out-of-sample results do NOT clear the bar (see the failed checks). The honest "
        "conclusion is there is no tradeable edge here on this data — a real, valuable finding, not a "
        "failure. NEXT: do not trade this; gather more data or shelve the model. Do NOT tune until it "
        "'works' — that is how you fool yourself."
    ),
    "INCONCLUSIVE": (
        "There are not enough pooled out-of-sample trades to say anything with statistical confidence. "
        "NEXT: widen EVAL_UNIVERSE and/or lengthen EVAL_HISTORY_DAYS and re-run. Do NOT read the "
        "numbers below as a result — they are noise at this sample size."
    ),
    "SUSPECT": (
        "The pooled Sharpe is implausibly high (>3). In this domain that is almost always look-ahead "
        "leakage or a bug, NOT a real edge. NEXT: stop and find the leak; do not trust or trade this. "
        "A great-looking backtest that is lying is the worst outcome of all."
    ),
}


# --------------------------------------------------------------------------- config

def _env(k: str, default: str) -> str:
    v = os.getenv(k)
    return v if v is not None and v.strip() != "" else default


@dataclass
class EvalConfig:
    universe: list[str]
    timeframe: str
    horizons: list[int]
    history_days: int
    holdout_frac: float
    cost_bps: float
    permutations: int
    min_trades: int
    seed: int
    upper: float
    lower: float
    allow_short: bool
    analysis_url: str
    out_dir: str
    # SAMPLE SUFFICIENCY (see decide_verdict). These do not change the strategy and are deliberately
    # not part of `strategy_version()` — they change what a verdict is judged SUFFICIENT ON, not what
    # was traded. They can only ever turn a verdict INTO `INCONCLUSIVE`; there is no path from any of
    # them to `EDGE`.
    min_dates: int = 252
    min_holdout_dates: int = 60
    min_tickers: int = 10
    min_coverage: float = 0.7
    earnings_cache_dir: str = ""

    @classmethod
    def from_env(cls) -> "EvalConfig":
        universe = [t.strip().upper() for t in _env("EVAL_UNIVERSE", DEFAULT_UNIVERSE).split(",") if t.strip()]
        horizons = [int(h) for h in _env("EVAL_HORIZONS", "5,10").split(",") if h.strip()]
        default_cost = COMMISSION_BPS + SLIPPAGE_BPS  # match the gated backtest's cost by default
        cfg = cls(
            universe=universe,
            timeframe=_env("EVAL_TIMEFRAME", "1D"),
            horizons=horizons,
            history_days=int(_env("EVAL_HISTORY_DAYS", "3650")),
            holdout_frac=float(_env("EVAL_HOLDOUT_FRAC", "0.2")),
            cost_bps=float(_env("EVAL_COST_BPS", str(default_cost))),
            permutations=int(_env("EVAL_PERMUTATIONS", "300")),
            min_trades=int(_env("EVAL_MIN_TRADES", "100")),
            seed=int(_env("EVAL_SEED", "42")),
            upper=float(_env("EVAL_UPPER", str(DEFAULT_UPPER))),
            lower=float(_env("EVAL_LOWER", str(DEFAULT_LOWER))),
            allow_short=_env("EVAL_ALLOW_SHORT", "false").lower() in ("1", "true", "yes", "on"),
            min_dates=int(_env("EVAL_MIN_DATES", "252")),
            min_holdout_dates=int(_env("EVAL_MIN_HOLDOUT_DATES", "60")),
            min_tickers=int(_env("EVAL_MIN_TICKERS", "10")),
            min_coverage=float(_env("EVAL_MIN_COVERAGE", "0.7")),
            analysis_url=_env("ANALYSIS_URL", ANALYSIS_URL),
            out_dir=(out_dir := _env("EVAL_OUT_DIR", os.path.join(os.getcwd(), "data", "eval"))),
            # same cache the event-study harness fills; cache-ONLY here (never burns AV quota)
            earnings_cache_dir=_env("EVENT_CACHE_DIR", os.path.join(out_dir, "earnings")),
        )
        configured = {
            "EVAL_MIN_DATES": (cfg.min_dates, EVIDENCE_FLOORS["minDates"]),
            "EVAL_MIN_HOLDOUT_DATES": (cfg.min_holdout_dates, EVIDENCE_FLOORS["minHoldoutDates"]),
            "EVAL_MIN_TICKERS": (cfg.min_tickers, EVIDENCE_FLOORS["minTickers"]),
            "EVAL_MIN_COVERAGE": (cfg.min_coverage, EVIDENCE_FLOORS["minCoverage"]),
        }
        weak = [f"{name}={value:g} (minimum {floor:g})"
                for name, (value, floor) in configured.items()
                if not math.isfinite(float(value)) or value < floor]
        if weak:
            raise ValueError(
                "evaluator sufficiency policy cannot be weakened below the live spendability floors: "
                + ", ".join(weak)
            )
        return cfg


# --------------------------------------------------------------------------- per-ticker OOS trades

@dataclass
class Stream:
    """One out-of-sample DECISION STREAM, carrying its bar dates.

    The dates are the whole point. Before them, `pool` concatenated every stream end to end and
    computed Sharpe, drawdown, equity and the permutation null on the splice — which treats the same
    trading day across thirty correlated names as thirty independent sequential observations, and
    draws an equity curve of a series that never existed. A stream that does not know WHEN its bars
    happened cannot be aligned with any other, so the dates travel with the numbers.
    """
    positions: np.ndarray
    ret_next: np.ndarray
    dates: np.ndarray
    label: str = ""

    def __post_init__(self):
        self.positions = np.asarray(self.positions, dtype=float)
        self.ret_next = np.asarray(self.ret_next, dtype=float)
        self.dates = np.asarray(self.dates)
        if not (len(self.positions) == len(self.ret_next) == len(self.dates)):
            raise ValueError(
                f"stream {self.label!r}: positions/ret_next/dates must be the same length "
                f"({len(self.positions)}/{len(self.ret_next)}/{len(self.dates)})"
            )


def make_stream(positions, ret_next, dates=None, label: str = "") -> Stream:
    """Build a Stream. `dates=None` falls back to BAR ORDINALS (0, 1, 2, ...), which aligns stream
    bar i with stream bar i — correct for crafted test series, and never used on the real path,
    where `evaluate_ticker` threads the actual index through."""
    positions = np.asarray(positions, dtype=float)
    if dates is None:
        dates = np.arange(len(positions))
    return Stream(positions=positions, ret_next=ret_next, dates=dates, label=label)


def concat_streams(a: Stream, b: Stream, label: str = "") -> Stream:
    """The full out-of-sample span of one (ticker, horizon): walk-forward folds then holdout. The
    two spans are strictly consecutive in time (evaluate_ticker asserts it), so this is a real
    series, not a splice of unrelated ones."""
    return Stream(
        positions=np.concatenate([a.positions, b.positions]),
        ret_next=np.concatenate([a.ret_next, b.ret_next]),
        dates=np.concatenate([a.dates, b.dates]),
        label=label or a.label or b.label,
    )


@dataclass
class TickerEval:
    """One (ticker, horizon)'s out-of-sample streams: the walk-forward out-of-fold span and the
    untouched final holdout span, kept separate so we can report them separately."""
    ticker: str
    horizon: int
    source: str
    folds: Stream
    holdout: Stream
    n_rows: int
    n_folds: int

    def combined(self) -> Stream:
        return concat_streams(self.folds, self.holdout, label=f"{self.ticker} h={self.horizon}")


def evaluate_ticker(df, ticker, source, horizon, cfg: EvalConfig, ctx=None, earnings=None) -> TickerEval | None:
    """Reuse the EXISTING leakage-safe pipeline to produce out-of-sample positions for one
    (ticker, horizon). Returns None (skip, not fail) if there isn't enough history to validate.
    ctx/earnings are the fail-soft context features (context.py) — None means neutral fills."""
    ds = build_dataset(df, horizon, ctx=ctx, earnings=earnings)
    n = len(ds.X)
    if n < MIN_ROWS or ds.y.nunique() < 2:
        return None

    holdout = max(int(n * cfg.holdout_frac), 30)
    wf_end = n - holdout
    x_wf, y_wf = ds.X.iloc[:wf_end], ds.y.iloc[:wf_end]
    rn_wf = ds.ret_next.iloc[:wf_end]
    x_ho, y_ho = ds.X.iloc[wf_end:], ds.y.iloc[wf_end:]
    rn_ho = ds.ret_next.iloc[wf_end:]

    # No-leakage guard: the split is strictly temporal — every training row precedes every holdout
    # row, so no fold can see the holdout. (Features are already lagged in build_dataset.)
    assert ds.index[:wf_end].max() < ds.index[wf_end:].min(), "holdout overlaps the walk-forward span"

    oof, folds = walk_forward(x_wf, y_wf)
    if len(oof) < 30:
        return None
    folds_pos = derive_positions(oof.to_numpy(), cfg.upper, cfg.lower, cfg.allow_short)
    folds_ret = rn_wf.loc[oof.index]

    # Frozen model: train once on ALL walk-forward data, score the untouched holdout exactly once.
    ho_prob = _fit_predict(x_wf, y_wf, x_ho)
    ho_pos = derive_positions(ho_prob, cfg.upper, cfg.lower, cfg.allow_short)

    label = f"{ticker} h={horizon}"
    return TickerEval(
        ticker=ticker, horizon=horizon, source=source,
        folds=Stream(folds_pos, folds_ret.to_numpy(), _dates_of(folds_ret), f"{label} folds"),
        holdout=Stream(ho_pos, rn_ho.to_numpy(), _dates_of(rn_ho), f"{label} holdout"),
        n_rows=int(n), n_folds=int(folds),
    )


def _dates_of(series) -> np.ndarray:
    """The bar dates of a pandas series, as a sortable numpy array."""
    return np.asarray(series.index.to_numpy())


# --------------------------------------------------------------------------- pooling + metrics
#
# TWO KINDS OF NUMBER, AND THE DIFFERENCE MATTERS.
#
#   * COUNTING stats — trade count, active-bar count, hit rate, mean return per active bar — are
#     per-DECISION. Concatenating streams to count them is honest: "how often was a position right"
#     does not care which day it was.
#   * STATISTICAL stats — expectancy, Sharpe, drawdown, equity, total return, and the permutation
#     null built on them — are properties of a TIME SERIES. Concatenating streams to compute them is
#     not honest: it multiplies the apparent sample by the number of tickers, double-counts the same
#     market move whenever two streams share a day, and produces a drawdown curve of a path no
#     portfolio ever walked.
#
# So the statistical half is computed on a DATE-ALIGNED, EQUAL-WEIGHT (1/N) PORTFOLIO: on each date,
# the mean net return over ALL N streams, with a stream that has no position (or no bar) that date
# contributing 0 — idle capital drags, exactly as it would in a real 1/N allocation.

POOLING_METHOD = EVALUATION_METHOD  # "portfolio-v4" — stamped on reports and verdict records


class _Aligner:
    """Maps each stream's bars onto one shared, sorted date axis, once, so a 300-permutation null
    does not re-derive the alignment 300 times."""

    def __init__(self, streams: list[Stream]):
        self.n = len(streams)
        if self.n == 0:
            self.dates = np.array([])
            self.idx = []
            return
        self.dates = np.unique(np.concatenate([s.dates for s in streams]))
        self.idx = [np.searchsorted(self.dates, s.dates) for s in streams]

    def portfolio(self, nets: list[np.ndarray]) -> np.ndarray:
        """Equal-weight mean over ALL N streams, per date. Absent streams contribute 0."""
        d = len(self.dates)
        if d == 0 or self.n == 0:
            return np.array([], dtype=float)
        acc = np.zeros(d, dtype=float)
        for i, net in enumerate(nets):
            acc += np.bincount(self.idx[i], weights=np.asarray(net, dtype=float), minlength=d)
        return acc / float(self.n)


def stream_nets(streams: list[Stream], cost_bps: float) -> list[np.ndarray]:
    """Per-stream net returns via backtest.net_returns, so the accounting matches the gated
    backtest exactly."""
    return [net_returns(s.ret_next, s.positions, cost_bps)[0] for s in streams]


def portfolio_returns(streams: list[Stream], cost_bps: float) -> tuple[np.ndarray, np.ndarray]:
    """(dates, portfolio net return per date) for the equal-weight 1/N portfolio."""
    aligner = _Aligner(streams)
    return aligner.dates, aligner.portfolio(stream_nets(streams, cost_bps))


def _exp_sharpe(port: np.ndarray, timeframe: str) -> tuple[float, float]:
    """(expectancy, annualized Sharpe) of a PORTFOLIO return series — one observation per date, not
    one per (stream, bar). The two metrics the permutation null is built on."""
    port = np.asarray(port, dtype=float)
    if len(port) == 0:
        return 0.0, 0.0
    mean, std = float(port.mean()), float(port.std(ddof=0))
    ann = annualization(timeframe)
    sharpe = float(mean / std * math.sqrt(ann)) if std > 1e-12 else 0.0
    return mean, sharpe


def pool(streams: list[Stream], cost_bps: float, timeframe: str) -> dict:
    """Pool a list of Streams into one metrics block: per-decision counting stats, and portfolio
    statistics computed on the date-aligned 1/N series (see the note above)."""
    streams = list(streams)
    if not streams:
        return _empty_pool()

    nets, act_nets, corrects = [], [], []
    n_trades = 0
    for s in streams:
        net, entries, active = net_returns(s.ret_next, s.positions, cost_bps)
        nets.append(net)
        n_trades += int(entries.sum())
        if active.any():
            act_nets.append(net[active])
            corrects.append(np.sign(s.positions[active]) == np.sign(s.ret_next[active]))

    flat = np.concatenate(nets) if nets else np.array([], dtype=float)
    act = np.concatenate(act_nets) if act_nets else np.array([], dtype=float)
    corr = np.concatenate(corrects) if corrects else np.array([], dtype=bool)
    if len(flat) == 0:
        return _empty_pool()

    aligner = _Aligner(streams)
    port = aligner.portfolio(nets)
    equity = np.cumprod(1.0 + port) if len(port) else np.array([], dtype=float)
    mean, sharpe = _exp_sharpe(port, timeframe)
    pos_sum = float(port[port > 0].sum())
    neg_sum = float(-port[port < 0].sum())
    profit_factor = round(pos_sum / neg_sum, 3) if neg_sum > 1e-12 else None
    return {
        "method": POOLING_METHOD,
        "nStreams": len(streams),
        "nTrades": int(n_trades),
        # nBars counts DECISIONS across streams; nDates counts the portfolio's actual observations.
        # They are not the same number and the report must never let them be read as one.
        "nBars": int(len(flat)),
        "nDates": int(len(port)),
        "nActiveBars": int(len(act)),
        "hitRate": round(float(corr.mean()), 4) if len(corr) else 0.0,
        "avgReturnPct": round(float(act.mean()) * 100.0, 4) if len(act) else 0.0,
        "expectancy": round(mean, 6),
        "sharpe": round(sharpe, 3),
        "maxDrawdown": round(max_drawdown(equity), 4),
        "profitFactor": profit_factor,
        "totalReturn": round(float(equity[-1] - 1.0), 4) if len(equity) else 0.0,
        "equityCurve": _downsample(equity, 120),
    }


def _empty_pool() -> dict:
    return {
        "method": POOLING_METHOD, "nStreams": 0, "nTrades": 0, "nBars": 0, "nDates": 0,
        "nActiveBars": 0, "hitRate": 0.0, "avgReturnPct": 0.0, "expectancy": 0.0, "sharpe": 0.0,
        "maxDrawdown": 0.0, "profitFactor": None, "totalReturn": 0.0, "equityCurve": [],
    }


def permutation_test(streams: list[Stream], cost_bps: float, timeframe: str,
                     n_perms: int, seed: int) -> dict:
    """Null control on the SAME portfolio statistic the verdict is judged on.

    The null must destroy the model's TIMING while preserving everything else that could produce the
    number by accident. An i.i.d. shuffle of each position series destroys too much: it breaks the
    run-lengths (so it under-charges turnover costs relative to the real series) and it scrambles
    each stream's own date alignment. A CIRCULAR SHIFT by an independent random offset per stream
    keeps every position run intact, keeps each return on its own date, and keeps the
    cross-sectional structure of the portfolio — while making the alignment between a stream's
    positions and its returns arbitrary. That is the hypothesis being tested.

    p_value = fraction of permutations whose metric is >= the real metric (with a +1/+1 correction so
    a real edge never reports an impossibly clean p=0). Seeded, so it reproduces exactly.
    """
    streams = list(streams)
    rng = np.random.default_rng(seed)
    aligner = _Aligner(streams)
    real_exp, real_sharpe = _exp_sharpe(aligner.portfolio(stream_nets(streams, cost_bps)), timeframe)

    null_exp = np.empty(n_perms, dtype=float)
    null_sharpe = np.empty(n_perms, dtype=float)
    for i in range(n_perms):
        nets = []
        for s in streams:
            m = len(s.positions)
            offset = int(rng.integers(1, m)) if m > 1 else 0
            nets.append(net_returns(s.ret_next, np.roll(s.positions, offset), cost_bps)[0])
        e, sh = _exp_sharpe(aligner.portfolio(nets), timeframe)
        null_exp[i] = e
        null_sharpe[i] = sh

    def summ(real: float, null: np.ndarray) -> dict:
        p = (float(np.sum(null >= real)) + 1.0) / (n_perms + 1.0)
        return {
            "real": round(real, 6),
            "nullMean": round(float(null.mean()), 6),
            "null95": round(float(np.percentile(null, 95)), 6),
            "pValue": round(p, 4),
        }

    return {
        "permutations": n_perms,
        "method": "circular-shift per stream (preserves position run-lengths and date alignment)",
        "expectancy": summ(real_exp, null_exp),
        "sharpe": summ(real_sharpe, null_sharpe),
    }


# --------------------------------------------------------------------------- verdict

def sufficiency(all_pool: dict, holdout_pool: dict, cohort_tickers: list[str], cfg: "EvalConfig",
                failed: list[str], skipped: list[str]) -> dict:
    """The SAMPLE this verdict would be made on, as numbers, so the report and the stored record can
    both carry them.

    `evaluated / configured` is COVERAGE, and tickers that failed to fetch or were skipped for too
    little history count AGAINST it. They have to: a run configured for 30 names that could only
    evaluate 4 has answered a question about 4 names, and a verdict that does not say so reads as a
    universe-wide result.
    """
    configured = len(cfg.universe)
    evaluated = len(cohort_tickers)
    coverage = (evaluated / configured) if configured else 0.0
    return {
        "nDates": int(all_pool.get("nDates", 0)),
        "minDates": cfg.min_dates,
        "holdoutDates": int(holdout_pool.get("nDates", 0)),
        "minHoldoutDates": cfg.min_holdout_dates,
        "nStreams": evaluated,
        "minTickers": cfg.min_tickers,
        "configuredTickers": configured,
        "evaluatedTickers": sorted(cohort_tickers),
        "coverage": round(coverage, 4),
        "minCoverage": cfg.min_coverage,
        "failedTickers": sorted(failed),
        "skippedTickers": sorted(skipped),
        "note": (
            "Sample sufficiency, not strategy. These thresholds decide whether the evidence is large "
            "enough to judge; they are NOT part of strategy_version() and can only ever produce "
            "INCONCLUSIVE."
        ),
    }


def _insufficiency_reasons(suff: dict) -> list[str]:
    """Every sample-size reason this cohort cannot support a verdict, worst stated first.

    Each of these can only turn a verdict INTO `INCONCLUSIVE`. None of them is a route to `EDGE`:
    a run that satisfies all of them still has to pass every check below, unchanged.
    """
    out: list[str] = []
    if suff["nDates"] < suff["minDates"]:
        out.append(
            f"only {suff['nDates']} portfolio observation dates; need >= {suff['minDates']}. "
            "A Sharpe annualized from fewer dates than a trading year is not an annual number."
        )
    if suff["holdoutDates"] < suff["minHoldoutDates"]:
        out.append(
            f"the untouched holdout is only {suff['holdoutDates']} dates; need >= "
            f"{suff['minHoldoutDates']}. The holdout is the only span no fold trained on, and a "
            "short one cannot carry the verdict the rest of the run leans on."
        )
    if suff["nStreams"] < suff["minTickers"]:
        out.append(
            f"only {suff['nStreams']} evaluated stream(s) at this horizon; need >= "
            f"{suff['minTickers']}. Pooling a handful of correlated names is not a universe."
        )
    if suff["coverage"] < suff["minCoverage"]:
        named = []
        if suff["failedTickers"]:
            named.append("failed: " + ", ".join(suff["failedTickers"]))
        if suff["skippedTickers"]:
            named.append("skipped: " + ", ".join(suff["skippedTickers"]))
        detail = ("  " + "; ".join(named)) if named else ""
        out.append(
            f"coverage {suff['coverage']:.0%} ({suff['nStreams']}/{suff['configuredTickers']} "
            f"configured tickers); need >= {suff['minCoverage']:.0%}." + detail
        )
    return out


def decide_verdict(all_pool: dict, holdout_pool: dict, bh_pool: dict,
                   perm: dict, min_trades: int, *,
                   suff: dict) -> tuple[str, float, list[str]]:
    """The honest gate. Returns (verdict, p_value, human-readable checklist).

    `suff` is REQUIRED, and deliberately has no default. A verdict is a claim about evidence, and a
    claim that does not state how much evidence it rests on is the exact failure this argument
    exists to prevent: before it, a run over four tickers and thirty portfolio dates could mint
    `EDGE` and the record it wrote said nothing about either number.
    """
    n_trades = all_pool["nTrades"]
    sharpe = all_pool["sharpe"]
    ho_exp = holdout_pool["expectancy"]
    beats_bh = sharpe > bh_pool["sharpe"]
    p_value = max(perm["expectancy"]["pValue"], perm["sharpe"]["pValue"])  # stricter of the two
    suspect = all_pool["sharpe"] > 3.0 or holdout_pool["sharpe"] > 3.0

    if n_trades < min_trades:
        need = min_trades - n_trades
        return "INCONCLUSIVE", p_value, [
            f"only {n_trades} pooled out-of-sample trades; need >= {min_trades} ({need} more).",
            "widen EVAL_UNIVERSE or lengthen EVAL_HISTORY_DAYS and re-run.",
        ]
    if suspect:
        # SUSPECT is checked before the sufficiency reasons on purpose. It is the WORST verdict in
        # VERDICT_SEVERITY, and a run that looks like leakage must not be softened to INCONCLUSIVE
        # just because it is also small — that would make the headline read less alarming than it did
        # before these checks existed.
        return "SUSPECT", p_value, [
            f"pooled Sharpe {all_pool['sharpe']} (holdout {holdout_pool['sharpe']}) > 3 — "
            "almost always leakage/bias, not a real edge. Investigate before believing it.",
        ]
    if reasons := _insufficiency_reasons(suff):
        return "INCONCLUSIVE", p_value, [
            "the sample is too small to judge this strategy on:", *[f"  - {r}" for r in reasons],
            "widen EVAL_UNIVERSE, lengthen EVAL_HISTORY_DAYS, or fix the failing tickers and re-run.",
        ]

    checks = {
        "positive holdout expectancy after costs": ho_exp > 0.0,
        "0 < Sharpe <= 3": 0.0 < sharpe <= 3.0,
        "beats buy-and-hold risk-adjusted": beats_bh,
        f"permutation p < 0.05 (p={p_value})": p_value < 0.05,
        f"trades >= {min_trades}": n_trades >= min_trades,
        # Recorded as PASSes rather than assumed: a verdict has to say what sample it stands on, and
        # `_insufficiency_reasons` has already returned INCONCLUSIVE if any of these were false.
        f"portfolio dates >= {suff['minDates']} (n={suff['nDates']})": True,
        f"holdout dates >= {suff['minHoldoutDates']} (n={suff['holdoutDates']})": True,
        f"evaluated streams >= {suff['minTickers']} (n={suff['nStreams']})": True,
        (f"ticker coverage >= {suff['minCoverage']:.0%} "
         f"({suff['nStreams']}/{suff['configuredTickers']} = {suff['coverage']:.0%})"): True,
    }
    checklist = [f"{'PASS' if ok else 'FAIL'}  {name}" for name, ok in checks.items()]
    verdict = "EDGE" if all(checks.values()) else "NO EDGE"
    return verdict, p_value, checklist


# Worst-first. A headline may never read more permissive than the horizons it summarises.
VERDICT_SEVERITY = ("SUSPECT", "NO EDGE", "INCONCLUSIVE", "EDGE")


def _headline_verdict(verdicts: list[str]) -> str:
    for v in VERDICT_SEVERITY:
        if v in verdicts:
            return v
    return "INCONCLUSIVE"


def run_strategy_version(cfg: EvalConfig) -> str:
    """The strategy identity of THIS run, from its own cost/threshold/shorting parameters — never
    the module defaults. See strategy.strategy_version."""
    return strategy_version(cfg.cost_bps, cfg.upper, cfg.lower, cfg.allow_short)


def _judge_horizon(cohort: list[TickerEval], cfg: EvalConfig,
                   failed: list[str] | None = None, skipped: list[str] | None = None) -> dict:
    """One horizon's portfolio, baselines, null and verdict. `cohort` is every (ticker) stream pair
    evaluated at that horizon — each ticker appears once, so the portfolio's dates are shared but
    its underlying returns are not double-counted.

    `failed` / `skipped` are the run-level tickers that never reached this cohort. They are carried
    in because COVERAGE is a property of the run, not of the streams that survived it: a verdict from
    a shrunken cohort has to say which names are missing from it."""
    folds = [te.folds for te in cohort]
    holdout = [te.holdout for te in cohort]
    combined = [te.combined() for te in cohort]

    folds_pool = pool(folds, cfg.cost_bps, cfg.timeframe)
    holdout_pool = pool(holdout, cfg.cost_bps, cfg.timeframe)
    all_pool = pool(combined, cfg.cost_bps, cfg.timeframe)

    # Buy-and-hold over the SAME out-of-sample dates (always long), through the same 1/N portfolio
    # math, so the comparison is like-for-like.
    bh_streams = [Stream(np.ones_like(s.ret_next), s.ret_next, s.dates, s.label + " B&H")
                  for s in combined]
    bh_pool = pool(bh_streams, cfg.cost_bps, cfg.timeframe)
    perm = permutation_test(combined, cfg.cost_bps, cfg.timeframe, cfg.permutations, cfg.seed)

    suff = sufficiency(all_pool, holdout_pool, sorted({te.ticker for te in cohort}), cfg,
                       failed or [], skipped or [])
    verdict, p_value, checklist = decide_verdict(all_pool, holdout_pool, bh_pool, perm,
                                                 cfg.min_trades, suff=suff)
    return {
        "horizon": cohort[0].horizon,
        "verdict": verdict,
        "meaning": VERDICT_MEANING[verdict],
        "tickers": sorted({te.ticker for te in cohort}),
        "checklist": checklist,
        "sufficiency": suff,
        "pValue": p_value,
        "pooled": {"all": all_pool, "folds": folds_pool, "holdout": holdout_pool},
        "buyHold": {"sharpe": bh_pool["sharpe"], "totalReturn": bh_pool["totalReturn"],
                    "expectancy": bh_pool["expectancy"], "nDates": bh_pool["nDates"]},
        "permutation": perm,
    }


# --------------------------------------------------------------------------- orchestration

def _utc_now() -> datetime:
    return datetime.now(timezone.utc)


def run(cfg: EvalConfig, *, now: datetime | None = None, write: bool = True) -> tuple[dict, int]:
    """Fetch -> refuse-if-synthetic -> per-ticker OOS -> pool -> permutation -> verdict -> report.
    Returns (report, exit_code). Prints to stdout; writes report files when write=True (never on a
    refusal). Never calls sys.exit — that is main()'s job, so this stays testable."""
    now = now or _utc_now()
    started = _utc_now()

    # 1. Fetch real history for each ticker (once); refuse on the FIRST synthetic resolution.
    fetched: dict[str, tuple[object, str]] = {}
    failed: list[tuple[str, str]] = []
    for t in cfg.universe:
        try:
            df, source, is_syn = fetch_feature_frame(t, cfg.timeframe, cfg.history_days, cfg.analysis_url)
        except Exception as e:  # noqa: BLE001 — analysis down / bad response: skip this ticker
            failed.append((t, str(e)))
            continue
        if is_syn:
            report = {
                "refused": "synthetic",
                "verdict": None,
                "message": f"{t} resolved to source '{source}' (synthetic seed).",
            }
            _print_refusal_synthetic(t, source)
            return report, EXIT_SYNTHETIC
        fetched[t] = (df, source)

    if not fetched:
        _print_refusal_no_data(failed)
        return {"refused": "no_data", "verdict": None, "failed": failed}, EXIT_NO_DATA

    # 2. Per (ticker, horizon) out-of-sample streams via the EXISTING walk-forward + holdout.
    # Market/sector context is fetched ONCE per run; earnings come from the shared cache only
    # (fail-soft: missing -> neutral features, exactly like the live service).
    ctx = fetch_context(cfg.timeframe, cfg.history_days, cfg.analysis_url)
    evals: list[TickerEval] = []
    skipped: list[str] = []
    for t, (df, source) in fetched.items():
        earnings = load_earnings(t, cfg.earnings_cache_dir)  # cache-only: api_key=""
        for h in cfg.horizons:
            te = evaluate_ticker(df, t, source, h, cfg, ctx=ctx, earnings=earnings)
            if te is None:
                skipped.append(f"{t} h={h}")
            else:
                evals.append(te)

    if not evals:
        _print_refusal_no_data(failed, note="every ticker had too little usable history to validate.")
        return {"refused": "no_data", "verdict": None, "failed": failed, "skipped": skipped}, EXIT_NO_DATA

    # 3. Pool PER HORIZON. Two horizons of one ticker are two views of the SAME underlying returns —
    #    pooling them together would count each market move twice and inflate the apparent sample —
    #    so each label horizon is evaluated as its own portfolio and gets its own verdict. The
    #    verdict FILES have always been per (ticker, timeframe, horizon); now their contents are too.
    #    Coverage is judged per horizon: a ticker that was skipped for one horizon and evaluated for
    #    another is missing from exactly the cohort it is missing from, and named there.
    failed_tickers = [t for t, _ in failed]
    by_horizon: dict[str, dict] = {}
    for h in sorted({te.horizon for te in evals}):
        cohort = [te for te in evals if te.horizon == h]
        skipped_here = [s.split(" h=")[0] for s in skipped if s.endswith(f" h={h}")]
        by_horizon[str(h)] = _judge_horizon(cohort, cfg, failed_tickers, skipped_here)

    # 3b. The combined-horizons number, when there is more than one horizon. It is reported for
    #     context ONLY and is explicitly labelled OVERLAPPING: its streams share underlying returns,
    #     so its Sharpe and p-value are not honest evidence. No verdict check reads it.
    overlapping = None
    if len(by_horizon) > 1:
        combined = [te.combined() for te in evals]
        overlapping = {
            "warning": (
                "OVERLAPPING — the horizons pooled here share the same underlying returns for the "
                "same tickers, so these observations are not independent. Reported for context; "
                "EXCLUDED from every verdict check, which are made per horizon."
            ),
            "pooled": pool(combined, cfg.cost_bps, cfg.timeframe),
        }

    # 4. Headline verdict = the WORST across horizons, so the summary can never read more permissive
    #    than any horizon it summarises. The binding verdict for a config is its own horizon's.
    verdict = _headline_verdict([v["verdict"] for v in by_horizon.values()])

    # 5. Per-ticker breakdown (each ticker/horizon's own OOS, plus its buy-and-hold). These are
    #    single-stream reports — one real series each — so run_backtest is the right tool and no
    #    alignment question arises.
    per_ticker = []
    for te in evals:
        oos = te.combined()
        rep = run_backtest(oos.ret_next, oos.positions, cfg.cost_bps, cfg.timeframe)
        bh = run_backtest(oos.ret_next, np.ones_like(oos.ret_next), cfg.cost_bps, cfg.timeframe)
        per_ticker.append({
            "ticker": te.ticker, "horizon": te.horizon, "source": te.source,
            "rows": te.n_rows, "folds": te.n_folds,
            "nTrades": rep["numTrades"], "hitRate": rep["directionalHitRate"],
            "expectancy": rep["expectancy"], "sharpe": rep["strategySharpe"],
            "maxDrawdown": rep["maxDrawdown"], "suspect": rep["suspect"],
            "buyHoldReturn": bh["totalReturn"], "buyHoldSharpe": bh["strategySharpe"],
        })
    per_ticker.sort(key=lambda r: (r["ticker"], r["horizon"]))

    elapsed = (_utc_now() - started).total_seconds()
    report = {
        "verdict": verdict,
        "meaning": VERDICT_MEANING[verdict],
        "verdictNote": (
            "Headline = the most conservative verdict across horizons. The BINDING verdict for a "
            "(ticker, timeframe, horizon) is that horizon's own, under `byHorizon`."
        ),
        # The pooling methodology these numbers were produced by. A stored verdict tagged with a
        # different one is not `current` (verdicts.EVALUATION_METHOD) — its numbers are not these.
        "method": POOLING_METHOD,
        "methodNote": (
            "Statistical metrics (expectancy, Sharpe, drawdown, equity, total return, permutation "
            "null) are computed on a DATE-ALIGNED equal-weight 1/N portfolio of the streams; a "
            "stream with no position on a date contributes 0. Counting stats (trades, hit rate, "
            "active bars) remain per-decision. `nBars` counts decisions across streams and is NOT a "
            "count of independent observations — `nDates` is."
        ),
        "generatedAt": now.isoformat(),
        "elapsedSec": round(elapsed, 1),
        "config": {
            "universe": cfg.universe, "tickersEvaluated": sorted(fetched.keys()),
            "timeframe": cfg.timeframe, "horizons": cfg.horizons,
            "historyDays": cfg.history_days, "holdoutFrac": cfg.holdout_frac,
            "costBps": cfg.cost_bps, "permutations": cfg.permutations,
            "minTrades": cfg.min_trades, "seed": cfg.seed,
            # Sample-sufficiency thresholds this run judged against (see decide_verdict). Recorded
            # so a report can be read years later without guessing which floors were in force.
            "minDates": cfg.min_dates, "minHoldoutDates": cfg.min_holdout_dates,
            "minTickers": cfg.min_tickers, "minCoverage": cfg.min_coverage,
            "upper": cfg.upper, "lower": cfg.lower, "allowShort": cfg.allow_short,
            # The identity of the strategy THIS RUN evaluated, from this run's own parameters. It is
            # what gets stamped on every verdict record: a run at non-default cost/thresholds/shorting
            # must never be spendable as the default strategy (contract §4.3).
            "strategyVersion": run_strategy_version(cfg),
        },
        "byHorizon": by_horizon,
        "overlapping": overlapping,
        "perTicker": per_ticker,
        "skipped": skipped,
        "failed": [{"ticker": t, "error": e} for t, e in failed],
    }

    report["files"] = _write_report(report, cfg, now) if write else {}
    # 7. Persist the verdict where the LIVE services can look it up (verdicts.py). The report has
    #    always been written; nothing read it, so the paper engine kept trading a config this
    #    harness had already judged NO EDGE. This writes down what decide_verdict returned — no
    #    gate here is loosened and no verdict is recomputed.
    report["verdictFiles"] = _write_verdicts(report, cfg, evals, now) if write else []
    _print_report(report)
    return report, EXIT_OK


# --------------------------------------------------------------------------- reporting

def _write_report(report: dict, cfg: EvalConfig, now: datetime) -> dict:
    stamp = now.strftime("%Y%m%dT%H%M%SZ")
    files = {
        "json": os.path.join(cfg.out_dir, f"report-{stamp}.json"),
        "md": os.path.join(cfg.out_dir, f"report-{stamp}.md"),
    }
    report["files"] = files  # so the persisted report self-references its own paths
    rendered_md = _render_md(report).encode("utf-8")
    rendered_json = json.dumps(report, indent=2, default=str).encode("utf-8")
    if db.enabled():
        db.save_artifact(os.path.basename(files["md"]), "text/markdown", rendered_md)
        db.save_artifact(os.path.basename(files["json"]), "application/json", rendered_json)
    else:
        os.makedirs(cfg.out_dir, exist_ok=True)
        with open(files["md"], "wb") as f:
            f.write(rendered_md)
        with open(files["json"], "wb") as f:
            f.write(rendered_json)
    return files


def _write_verdicts(report: dict, cfg: EvalConfig, evals: list, now: datetime) -> list[str]:
    """One verdict record per evaluated (ticker, timeframe, horizon).

    The verdict is POOLED across the universe AT THAT HORIZON — every record for a given horizon
    carries that horizon's verdict, and what varies between records is WHICH configs the run covered.
    `scope` says so on each record so nobody reads a per-ticker claim into a pooled result.

    `strategyVersion` is computed from THIS RUN'S OWN cfg (cost, thresholds, allowShort), not from
    the service defaults. A run at EVAL_COST_BPS=0, EVAL_UPPER=0.9, EVAL_ALLOW_SHORT=true used to be
    stamped as the default 6 bps long-only strategy and served `current: true` — a verdict about one
    strategy spent by another, which defeats gate 4 entirely.
    """
    source = (report.get("files") or {}).get("json", "")
    version = run_strategy_version(cfg)
    by_horizon = report.get("byHorizon") or {}
    written: list[str] = []
    for te in evals:
        horizon_block = by_horizon.get(str(te.horizon))
        if not horizon_block:
            # A config with no judged horizon block has no verdict to record. Writing the headline
            # here would attribute a number to a horizon that was never judged.
            print(f"  ! no horizon verdict for {te.ticker} h={te.horizon}: nothing written")
            continue
        try:
            written.append(write_verdict(
                te.ticker, cfg.timeframe, te.horizon,
                verdict=horizon_block["verdict"],
                evaluated_at=report["generatedAt"],
                report_file=source,
                strategy_version=version,
                out_dir=cfg.out_dir,
                scope=f"pooled-universe-h{te.horizon}",
                sufficiency=horizon_block.get("sufficiency"),
                data_policy=FEATURE_FRAME_POLICY,
            ))
        except OSError as e:  # noqa: PERF203 — a write failure must not lose the printed verdict
            print(f"  ! could not write verdict for {te.ticker} h={te.horizon}: {e}")
    return written


def _fmt_pct(x) -> str:
    return f"{x * 100:+.2f}%" if isinstance(x, (int, float)) else "—"


def _pool_table(a: dict, fo: dict, h: dict) -> list[str]:
    return [
        "| metric | all OOS | walk-forward folds | untouched holdout |",
        "|---|---:|---:|---:|",
        f"| trades | {a['nTrades']} | {fo['nTrades']} | {h['nTrades']} |",
        f"| decisions (bars x streams) | {a['nBars']} | {fo['nBars']} | {h['nBars']} |",
        f"| portfolio observations (dates) | {a['nDates']} | {fo['nDates']} | {h['nDates']} |",
        f"| hit rate | {a['hitRate']} | {fo['hitRate']} | {h['hitRate']} |",
        f"| avg return / active bar | {a['avgReturnPct']}% | {fo['avgReturnPct']}% | {h['avgReturnPct']}% |",
        f"| expectancy (net/portfolio day) | {a['expectancy']} | {fo['expectancy']} | {h['expectancy']} |",
        f"| Sharpe (annualized) | {a['sharpe']} | {fo['sharpe']} | {h['sharpe']} |",
        f"| max drawdown | {_fmt_pct(a['maxDrawdown'])} | {_fmt_pct(fo['maxDrawdown'])} | {_fmt_pct(h['maxDrawdown'])} |",
        f"| profit factor | {a['profitFactor']} | {fo['profitFactor']} | {h['profitFactor']} |",
        f"| total return | {_fmt_pct(a['totalReturn'])} | {_fmt_pct(fo['totalReturn'])} | {_fmt_pct(h['totalReturn'])} |",
    ]


def _render_md(r: dict) -> str:
    v = r["verdict"]
    L: list[str] = []
    L.append(f"# Edge evaluation — **{v}**")
    L.append("")
    L.append(f"_{r['generatedAt']} · {r['elapsedSec']}s · seed {r['config']['seed']} · "
             f"method `{r.get('method')}` · strategy `{r['config'].get('strategyVersion')}`_")
    L.append("")
    L.append("> " + r["meaning"].replace("\n", " "))
    L.append("")
    L.append("_" + r.get("verdictNote", "") + "_")
    L.append("")
    L.append("**How these numbers are computed.** " + r.get("methodNote", ""))
    L.append("")

    for key in sorted(r.get("byHorizon", {}), key=int):
        b = r["byHorizon"][key]
        a, h, fo = b["pooled"]["all"], b["pooled"]["holdout"], b["pooled"]["folds"]
        bh = b["buyHold"]
        L.append(f"## Horizon {key} — **{b['verdict']}**")
        L.append("")
        L.append(f"_{a['nStreams']} streams over {len(b['tickers'])} tickers._")
        L.append("")
        L.append("### Checklist")
        for c in b["checklist"]:
            L.append(f"- {c}")
        L.append("")
        L.append("### Pooled out-of-sample metrics (date-aligned 1/N portfolio)")
        L.append("")
        L.extend(_pool_table(a, fo, h))
        L.append("")
        L.append(f"**Buy-and-hold** over the same dates: Sharpe {bh['sharpe']}, total return "
                 f"{_fmt_pct(bh['totalReturn'])} (strategy beats it risk-adjusted: "
                 f"{'yes' if a['sharpe'] > bh['sharpe'] else 'no'}).")
        L.append("")
        p = b["permutation"]
        L.append("### Permutation null (random timing)")
        L.append(f"{p['permutations']} circular shifts of the per-stream positions — "
                 f"{p.get('method', '')}.")
        L.append("")
        L.append("| metric | real | null mean | null 95th pct | p-value |")
        L.append("|---|---:|---:|---:|---:|")
        for metric in ("expectancy", "sharpe"):
            sm = p[metric]
            L.append(f"| {metric} | {sm['real']} | {sm['nullMean']} | {sm['null95']} | {sm['pValue']} |")
        L.append("")

    if r.get("overlapping"):
        o = r["overlapping"]["pooled"]
        L.append("## Combined horizons — CONTEXT ONLY")
        L.append("")
        L.append("> " + r["overlapping"]["warning"])
        L.append("")
        L.append(f"Sharpe {o['sharpe']}, expectancy {o['expectancy']}, trades {o['nTrades']}, "
                 f"portfolio observations {o['nDates']}.")
        L.append("")

    L.append("## Per-ticker / horizon")
    L.append("")
    L.append("| ticker | h | src | rows | trades | hit | expectancy | Sharpe | maxDD | B&H ret | B&H Sharpe | suspect |")
    L.append("|---|--:|---|--:|--:|--:|--:|--:|--:|--:|--:|:-:|")
    for t in r["perTicker"]:
        L.append(
            f"| {t['ticker']} | {t['horizon']} | {t['source']} | {t['rows']} | {t['nTrades']} | "
            f"{t['hitRate']} | {t['expectancy']} | {t['sharpe']} | {_fmt_pct(t['maxDrawdown'])} | "
            f"{_fmt_pct(t['buyHoldReturn'])} | {t['buyHoldSharpe']} | {'!' if t['suspect'] else ''} |"
        )
    if r["skipped"]:
        L.append("")
        L.append(f"_Skipped (too little history): {', '.join(r['skipped'])}._")
    if r["failed"]:
        L.append("")
        L.append(f"_Fetch failed: {', '.join(f['ticker'] for f in r['failed'])}._")
    L.append("")
    L.append("---")
    L.append("_Research tool. No buy/sell output, no orders, no money. A verdict of EDGE is a "
             "necessary condition for paper trading (Stage 2), never a green light to trade._")
    L.append("")
    return "\n".join(L)


def _print_report(r: dict) -> None:
    bar = "=" * 64
    print(bar)
    print(f"  VERDICT:  {r['verdict']}   (headline — the most conservative horizon)")
    print(bar)
    print(_wrap(r["meaning"]))
    print()
    print(_wrap(f"Method: {r.get('method')}. {r.get('methodNote', '')}"))
    print()
    for key in sorted(r.get("byHorizon", {}), key=int):
        b = r["byHorizon"][key]
        a, h = b["pooled"]["all"], b["pooled"]["holdout"]
        bh, p = b["buyHold"], b["permutation"]
        print(f"  --- horizon {key}: {b['verdict']} ({a['nStreams']} streams) ---")
        for c in b["checklist"]:
            print(f"    {c}")
        print(f"  Pooled OOS: trades={a['nTrades']}  dates={a['nDates']}  decisions={a['nBars']}  "
              f"hitRate={a['hitRate']}  expectancy={a['expectancy']}  Sharpe={a['sharpe']}  "
              f"maxDD={_fmt_pct(a['maxDrawdown'])}  profitFactor={a['profitFactor']}")
        print(f"  Holdout   : trades={h['nTrades']}  dates={h['nDates']}  hitRate={h['hitRate']}  "
              f"expectancy={h['expectancy']}  Sharpe={h['sharpe']}")
        print(f"  Buy&hold  : Sharpe={bh['sharpe']}  totalReturn={_fmt_pct(bh['totalReturn'])}  "
              f"(strategy beats risk-adjusted: {'yes' if a['sharpe'] > bh['sharpe'] else 'no'})")
        print(f"  Null test : expectancy p={p['expectancy']['pValue']} (real {p['expectancy']['real']} "
              f"vs null mean {p['expectancy']['nullMean']}), "
              f"Sharpe p={p['sharpe']['pValue']} (real {p['sharpe']['real']} vs null mean {p['sharpe']['nullMean']})")
        print()
    if r.get("overlapping"):
        print(_wrap("Combined-horizons number: " + r["overlapping"]["warning"]))
        print()
    print(f"  {'ticker':<7}{'h':>3}  {'src':<11}{'trades':>7}{'hit':>7}{'expect':>10}{'Sharpe':>8}{'susp':>6}")
    for t in r["perTicker"]:
        print(f"  {t['ticker']:<7}{t['horizon']:>3}  {t['source']:<11}{t['nTrades']:>7}"
              f"{t['hitRate']:>7}{t['expectancy']:>10}{t['sharpe']:>8}{'  !' if t['suspect'] else '':>6}")
    if r["skipped"]:
        print(f"\n  Skipped (too little history): {', '.join(r['skipped'])}")
    if r["failed"]:
        print(f"  Fetch failed: {', '.join(f['ticker'] for f in r['failed'])}")
    files = r.get("files") or {}
    if files:
        print()
        print(f"  Report written: {files.get('md')}")
        print(f"                  {files.get('json')}")
    print(bar)


def _wrap(text: str, width: int = 78, indent: str = "  ") -> str:
    words = text.split()
    lines, cur = [], indent
    for w in words:
        if len(cur) + len(w) + 1 > width and cur.strip():
            lines.append(cur)
            cur = indent + w
        else:
            cur = (cur + " " + w) if cur.strip() else (indent + w)
    if cur.strip():
        lines.append(cur)
    return "\n".join(lines)


def _print_refusal_synthetic(ticker: str, source: str) -> None:
    bar = "=" * 64
    print(bar)
    print("  SYNTHETIC DATA — cannot validate")
    print(bar)
    print(_wrap(
        f"Ticker {ticker} resolved to source '{source}' (the synthetic ~$162 seed used offline). "
        "A verdict computed on synthetic data would be meaningless, so this harness refuses to "
        "produce one. Bring up the analysis service with a real price provider (yfinance needs no "
        "key, just network) and re-run."
    ))
    print(bar)


def _print_refusal_no_data(failed: list[tuple[str, str]], note: str = "") -> None:
    bar = "=" * 64
    print(bar)
    print("  NO DATA — cannot validate")
    print(bar)
    msg = "No ticker returned usable real data (is the analysis service reachable at ANALYSIS_URL?)."
    if note:
        msg += " " + note
    print(_wrap(msg))
    if failed:
        print(_wrap("Failed: " + ", ".join(f"{t} ({e[:60]})" for t, e in failed[:5])))
    print(bar)


def main(argv: list[str] | None = None) -> int:
    cfg = EvalConfig.from_env()
    print(_wrap(
        f"Evaluating {len(cfg.universe)} tickers x horizons {cfg.horizons} on {cfg.timeframe} "
        f"({cfg.history_days}d history, {cfg.permutations} permutations). This is long-running…",
        indent="",
    ))
    _report, code = run(cfg)
    return code


if __name__ == "__main__":
    sys.exit(main())
