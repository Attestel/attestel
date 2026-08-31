"""Offline EVENT-STUDY harness — post-earnings drift (PEAD), with the same honesty as the price
harness (app/evaluate.py): real-data-only, no look-ahead, costs subtracted, an untouched-BY-DATE
holdout, a clustered timing null, a matched long abnormal-return baseline, and a report.

Run:  cd services/prediction && python -m app.evaluate_events     (or: make evaluate-events)

Two tests:
  * Test 1 — baseline PEAD (NO AI). SUE = (reportedEPS - estimatedEPS) / std(prior surprises). Trade
    only STRONG surprises (|SUE| in the top/bottom tercile), direction = sign(SUE), enter at the OPEN
    of the first session AFTER reportedDate, exit after H sessions. Pool events across the universe,
    split by DATE into a report era and an untouched holdout. It's a RULE (no fitting), so the holdout
    is a pure out-of-sample confirmation. Verdict per horizon.
  * Test 2 — AI marginal value (optional). Score event-time TEXT with the LLM earnings scorer and ask
    whether combining SUE with the AI tone IMPROVES Test 1 on the SAME events. HONEST about data
    limits: historical event-time text is scarce, so it runs only on the retrievable subset, reports
    the sample size, and is INCONCLUSIVE below EVENT_MIN. Designed to ACCUMULATE FORWARD.

Guardrails (shared with evaluate.py): REAL DATA ONLY (refuse on synthetic/no-network); NO LOOK-AHEAD
(entry strictly after reportedDate, asserted; SUE from prior surprises only); costs subtracted;
research-only — NO buy/sell output, no orders, no money. SURVIVORSHIP BIAS is reported prominently
(the universe is today's names, so dead/delisted names are missing — drift estimates are optimistic).
"""
from __future__ import annotations

import json
import math
import os
import sys
from dataclasses import dataclass
from datetime import datetime, timezone

import numpy as np
import requests

from .backtest import _downsample, max_drawdown, net_returns
from .config import ANALYSIS_URL, COMMISSION_BPS, SLIPPAGE_BPS
from .evaluate import (
    DEFAULT_UNIVERSE,
    EXIT_NO_DATA,
    EXIT_OK,
    EXIT_SYNTHETIC,
    _env,
    _fmt_pct,
    _print_refusal_no_data,
    _print_refusal_synthetic,
    _wrap,
    Stream,
    _Aligner,
    _exp_sharpe,
    _headline_verdict,
    _insufficiency_reasons,
    pool,
)
from .events import attach_forward_estimates, build_events, compute_sue, get_earnings
from .features import fetch_feature_frame
from . import db as _db
from .verdicts import EVALUATION_METHOD, EVIDENCE_FLOORS

EXIT_NO_KEY = 4  # refused: no ALPHAVANTAGE_API_KEY and no cached earnings
PEAD_STUDY_VERSION = "pead-abnormal-v2-forward-vintage"

SURVIVORSHIP_WARNING = (
    "SURVIVORSHIP BIAS: the universe is today's surviving names, so companies that were delisted, "
    "acquired, or went bankrupt are MISSING. Post-earnings drift measured only on survivors is "
    "OPTIMISTIC. Treat any positive result as an upper bound; a fairer test broadens the universe "
    "(and includes smaller caps, where drift is stronger than in heavily-covered mega-caps)."
)

EVENT_VERDICT_MEANING = {
    "EDGE": (
        "Pooled post-earnings-drift trades show a positive abnormal-return edge after costs that "
        "SURVIVES an untouched date-holdout, BEATS a matched long baseline, and BEATS a circular-shift "
        "null after correcting for the tested horizons. "
        "This is necessary, not sufficient — and it is measured on SURVIVORS only. NEXT: widen the "
        "universe, then paper-trade before any real money. Do not size up on this alone."
    ),
    "NO EDGE": (
        "Pooled event results do NOT clear the bar (see the failed checks). On this universe there is "
        "no tradeable post-earnings drift after costs — a real, valuable finding. Drift is weaker in "
        "mega-caps; a broader / smaller-cap universe is a fairer test. Do NOT tune until it 'works'."
    ),
    "INCONCLUSIVE": (
        "The sample or its provenance is not strong enough to judge. Read the checklist: gather more "
        "events/dates/tickers or prove the historical estimate vintage, then re-run. Do NOT treat the "
        "descriptive numbers as an edge."
    ),
    "SUSPECT": (
        "The pooled Sharpe is implausibly high (>3) — almost always look-ahead leakage or a bug in the "
        "event alignment, NOT a real edge. Stop and check the entry timing before believing it."
    ),
}


# --------------------------------------------------------------------------- config

@dataclass
class EventConfig:
    universe: list[str]
    horizons: list[int]
    sue_quantile: float
    min_events: int
    cost_bps: float
    permutations: int
    holdout_frac: float
    seed: int
    history_days: int
    timeframe: str
    analysis_url: str
    out_dir: str
    cache_dir: str
    text_dir: str
    av_key: str
    llm_url: str
    enable_ai: bool
    benchmark: str
    min_dates: int
    min_holdout_dates: int
    min_tickers: int
    min_coverage: float

    @classmethod
    def from_env(cls) -> "EventConfig":
        universe = [t.strip().upper() for t in _env("EVENT_UNIVERSE", _env("EVAL_UNIVERSE", DEFAULT_UNIVERSE)).split(",") if t.strip()]
        horizons = [int(h) for h in _env("EVENT_HORIZONS", "10,20").split(",") if h.strip()]
        default_cost = COMMISSION_BPS + SLIPPAGE_BPS
        out_dir = _env("EVENT_OUT_DIR", _env("EVAL_OUT_DIR", os.path.join(os.getcwd(), "data", "eval")))
        cfg = cls(
            universe=universe,
            horizons=horizons,
            sue_quantile=float(_env("EVENT_SUE_QUANTILE", "0.33")),
            min_events=int(_env("EVENT_MIN", "100")),
            cost_bps=float(_env("EVENT_COST_BPS", _env("EVAL_COST_BPS", str(default_cost)))),
            permutations=int(_env("EVENT_PERMUTATIONS", _env("EVAL_PERMUTATIONS", "300"))),
            holdout_frac=float(_env("EVENT_HOLDOUT_FRAC", _env("EVAL_HOLDOUT_FRAC", "0.2"))),
            seed=int(_env("EVENT_SEED", _env("EVAL_SEED", "42"))),
            history_days=int(_env("EVENT_HISTORY_DAYS", "3650")),
            timeframe=_env("EVENT_TIMEFRAME", "1D"),
            analysis_url=_env("ANALYSIS_URL", ANALYSIS_URL),
            out_dir=out_dir,
            cache_dir=_env("EVENT_CACHE_DIR", os.path.join(out_dir, "earnings")),
            text_dir=_env("EVENT_TEXT_DIR", os.path.join(out_dir, "earnings_text")),
            av_key=_env("ALPHAVANTAGE_API_KEY", ""),
            llm_url=_env("LLM_URL", "http://localhost:8002").rstrip("/"),
            enable_ai=_env("EVENT_ENABLE_AI", "false").lower() in ("1", "true", "yes", "on"),
            benchmark=_env("EVENT_BENCHMARK", "SPY").upper(),
            min_dates=int(_env("EVENT_MIN_DATES", str(EVIDENCE_FLOORS["minDates"]))),
            min_holdout_dates=int(_env(
                "EVENT_MIN_HOLDOUT_DATES", str(EVIDENCE_FLOORS["minHoldoutDates"])
            )),
            min_tickers=int(_env("EVENT_MIN_TICKERS", str(EVIDENCE_FLOORS["minTickers"]))),
            min_coverage=float(_env("EVENT_MIN_COVERAGE", str(EVIDENCE_FLOORS["minCoverage"]))),
        )
        configured = {
            "EVENT_MIN_DATES": (cfg.min_dates, EVIDENCE_FLOORS["minDates"]),
            "EVENT_MIN_HOLDOUT_DATES": (
                cfg.min_holdout_dates, EVIDENCE_FLOORS["minHoldoutDates"]
            ),
            "EVENT_MIN_TICKERS": (cfg.min_tickers, EVIDENCE_FLOORS["minTickers"]),
            "EVENT_MIN_COVERAGE": (cfg.min_coverage, EVIDENCE_FLOORS["minCoverage"]),
        }
        weak = [
            f"{name}={value:g} (minimum {floor:g})"
            for name, (value, floor) in configured.items()
            if not math.isfinite(float(value)) or value < floor
        ]
        if weak:
            raise ValueError(
                "PEAD sufficiency policy cannot be weakened below the live evidence floors: "
                + ", ".join(weak)
            )
        if not 0.0 < cfg.sue_quantile < 0.5:
            raise ValueError("EVENT_SUE_QUANTILE must be between 0 and 0.5")
        if not cfg.universe:
            raise ValueError("EVENT_UNIVERSE must contain at least one ticker")
        if not cfg.horizons or any(h <= 0 for h in cfg.horizons):
            raise ValueError("EVENT_HORIZONS must contain positive session counts")
        if not 0.0 < cfg.holdout_frac < 1.0:
            raise ValueError("EVENT_HOLDOUT_FRAC must be between 0 and 1")
        if cfg.min_events < 1 or cfg.permutations < 1:
            raise ValueError("EVENT_MIN and EVENT_PERMUTATIONS must be positive")
        if not cfg.benchmark:
            raise ValueError("EVENT_BENCHMARK must name a real market benchmark ticker")
        return cfg


# --------------------------------------------------------------------------- event accounting

def event_net(directions, raw_returns, cost_bps: float) -> np.ndarray:
    """Per-event NET return, ROUND-TRIP costs (entry + exit) applied via backtest.net_returns — the
    same cost primitive the price harness uses. Each event is an isolated open-then-close: we lay it
    out as [dir, 0] positions over [raw_return, 0] and let net_returns charge turnover on both legs."""
    d = np.asarray(directions, float)
    r = np.asarray(raw_returns, float)
    n = len(d)
    if n == 0:
        return np.array([], dtype=float)
    pos = np.zeros(2 * n, dtype=float)
    ret = np.zeros(2 * n, dtype=float)
    pos[0::2] = d
    ret[0::2] = r
    net, _, _ = net_returns(ret, pos, cost_bps)
    return net.reshape(n, 2).sum(axis=1)  # entry-bar net + exit-bar cost = dir*raw - 2*cost


def _event_exp_sharpe(net: np.ndarray, horizon: int) -> tuple[float, float]:
    """(expectancy, annualized Sharpe) for a per-event net-return series. An H-session holding period
    repeats ~252/H times per year, so we annualize by sqrt(252/H)."""
    if len(net) == 0:
        return 0.0, 0.0
    mean, std = float(net.mean()), float(net.std(ddof=0))
    ann = 252.0 / max(horizon, 1)
    sharpe = float(mean / std * math.sqrt(ann)) if std > 1e-12 else 0.0
    return mean, sharpe


def event_metrics(net, directions, raw_returns, horizon: int) -> dict:
    net = np.asarray(net, float)
    if len(net) == 0:
        return _empty_event_pool()
    d = np.asarray(directions, float)
    r = np.asarray(raw_returns, float)
    mean, sharpe = _event_exp_sharpe(net, horizon)
    equity = np.cumprod(1.0 + net)
    hit = float((np.sign(d) == np.sign(r)).mean())
    pos_sum = float(net[net > 0].sum())
    neg_sum = float(-net[net < 0].sum())
    pf = round(pos_sum / neg_sum, 3) if neg_sum > 1e-12 else None
    return {
        "nEvents": int(len(net)), "nTrades": int(len(net)),  # nTrades alias -> reuse decide_verdict
        "hitRate": round(hit, 4),
        "avgReturnPct": round(mean * 100.0, 4),
        "expectancy": round(mean, 6),
        "sharpe": round(sharpe, 3),
        "maxDrawdown": round(max_drawdown(equity), 4),
        "profitFactor": pf,
        "totalReturn": round(float(equity[-1] - 1.0), 4),
        "equityCurve": _downsample(equity, 120),
    }


def _empty_event_pool() -> dict:
    return {
        "nEvents": 0, "nTrades": 0, "hitRate": 0.0, "avgReturnPct": 0.0, "expectancy": 0.0,
        "sharpe": 0.0, "maxDrawdown": 0.0, "profitFactor": None, "totalReturn": 0.0, "equityCurve": [],
    }


def _arrays(events: list[dict], horizon: int) -> tuple[np.ndarray, np.ndarray, list[dict]]:
    """(directions, forward-returns, events) for the events that have a return at this horizon,
    ordered by entry date so the pooled equity curve is chronological."""
    ev = [e for e in events if horizon in e["returns"]]
    ev.sort(key=lambda e: e["entryDate"])
    d = np.array([e["direction"] for e in ev], dtype=float)
    r = np.array([e["returns"][horizon] for e in ev], dtype=float)
    return d, r, ev


# --------------------------------------------------------------------------- SUE tercile filter

def apply_tercile_filter(events: list[dict], quantile: float) -> tuple[list[dict], float, float]:
    """Keep only STRONG surprises: SUE in the bottom (<= q) or top (>= 1-q) tercile. Direction is
    already sign(SUE) (long strong-positive, short strong-negative). The threshold is a cross-sectional
    magnitude filter on SUE — it uses NO forward returns, so it cannot leak the outcome."""
    if not events:
        return [], 0.0, 0.0
    sues = np.array([e["sue"] for e in events], dtype=float)
    lo = float(np.quantile(sues, quantile))
    hi = float(np.quantile(sues, 1.0 - quantile))
    strong = [e for e in events if (e["sue"] <= lo or e["sue"] >= hi) and e["direction"] != 0.0]
    return strong, lo, hi


def apply_sue_thresholds(events: list[dict], low: float, high: float) -> list[dict]:
    """Apply thresholds frozen on the report era to later events without re-estimating them."""
    return [
        e for e in events
        if (e["sue"] <= low or e["sue"] >= high) and e["direction"] != 0.0
    ]


def split_by_date(events: list[dict], holdout_frac: float) -> tuple[list[dict], list[dict], str]:
    """Split pooled events by ENTRY DATE: the last holdout_frac (chronologically) is the untouched
    holdout; the rest is the report era. Returns (report, holdout, cutoff_date)."""
    ev = sorted(events, key=lambda e: (e["entryDate"], e.get("ticker", "")))
    dates = sorted({e["entryDate"] for e in ev})
    if not dates:
        return [], [], ""
    if len(dates) == 1:
        return ev, [], dates[0]
    cut = max(1, min(len(dates) - 1, int(len(dates) * (1.0 - holdout_frac))))
    cutoff = dates[cut]
    report = [e for e in ev if e["entryDate"] < cutoff]
    holdout = [e for e in ev if e["entryDate"] >= cutoff]
    return report, holdout, cutoff


# --------------------------------------------------------------------------- Test 1

def _dates_and_open_returns(frame) -> tuple[list[str], np.ndarray]:
    dates = [str(x)[:10] for x in frame.index]
    opens = frame["open"].to_numpy(dtype=float)
    if len(opens) < 2:
        return [], np.array([], dtype=float)
    return dates, opens[1:] / opens[:-1] - 1.0


def build_event_stream(
    ticker: str,
    price_frame,
    benchmark_frame,
    events: list[dict],
    horizon: int,
    *,
    direction_override: float | None = None,
) -> tuple[Stream | None, list[dict]]:
    """Build one ticker's daily event-position stream on benchmark-adjusted returns.

    Each ticker owns one fixed portfolio slot. Positions are active from the first post-report open
    through the H-session exit, so `evaluate.pool` charges entry/exit costs and aligns every ticker
    on one date axis. Same-ticker overlaps are skipped rather than levering the slot above 1x.
    """
    dates, stock_ret = _dates_and_open_returns(price_frame)
    b_dates, benchmark_ret = _dates_and_open_returns(benchmark_frame)
    if not dates or not b_dates:
        return None, []
    date_idx = {d: i for i, d in enumerate(dates)}
    benchmark_by_date = {d: float(r) for d, r in zip(b_dates[:-1], benchmark_ret)}
    positions = np.zeros(len(stock_ret), dtype=float)
    occupied = np.zeros(len(stock_ret), dtype=bool)
    used: list[dict] = []
    for event in sorted(events, key=lambda e: e["entryDate"]):
        if horizon not in event["returns"]:
            continue
        start = date_idx.get(event["entryDate"])
        stop = None if start is None else start + horizon
        if start is None or stop > len(stock_ret):
            continue
        span = slice(start, stop)
        span_dates = dates[start:stop]
        if occupied[span].any() or any(
            d not in benchmark_by_date or not np.isfinite(benchmark_by_date[d])
            for d in span_dates
        ):
            continue
        if not np.isfinite(stock_ret[span]).all():
            continue
        direction = (
            float(direction_override) if direction_override is not None
            else float(event["direction"])
        )
        positions[span] = direction
        occupied[span] = True
        used.append(event)
    active = np.flatnonzero(positions != 0.0)
    if len(active) == 0:
        return None, []
    lo, hi = int(active[0]), int(active[-1]) + 1
    abnormal = np.array([
        float(stock_ret[i]) - benchmark_by_date[dates[i]] for i in range(lo, hi)
    ])
    # net_returns charges turnover when the POSITION CHANGES. Carry an explicit flat observation at
    # the exit open so the final event pays its closing cost; trimming at the last active bar would
    # silently omit that leg and make every stream optimistic by one cost.
    stream_positions = np.concatenate([positions[lo:hi], [0.0]])
    abnormal = np.concatenate([abnormal, [0.0]])
    stream_dates = np.asarray(dates[lo:hi] + [dates[hi]])
    return Stream(
        stream_positions, abnormal, stream_dates,
        f"{ticker} PEAD h={horizon} vs benchmark",
    ), used


def _event_streams(
    events: list[dict], price_frames: dict[str, object], benchmark_frame, horizon: int,
    *, direction_override: float | None = None,
) -> tuple[list[Stream], list[dict]]:
    grouped: dict[str, list[dict]] = {}
    for event in events:
        grouped.setdefault(event["ticker"], []).append(event)
    streams: list[Stream] = []
    used: list[dict] = []
    for ticker in sorted(grouped):
        frame = price_frames.get(ticker)
        if frame is None:
            continue
        stream, accepted = build_event_stream(
            ticker, frame, benchmark_frame, grouped[ticker], horizon,
            direction_override=direction_override,
        )
        if stream is not None:
            streams.append(stream)
            used.extend(accepted)
    return streams, used


def _portfolio_pool(streams: list[Stream], events: list[dict], cfg: EventConfig) -> dict:
    result = pool(streams, cfg.cost_bps, cfg.timeframe)
    result["nEvents"] = len(events)
    return result


def _cyclic_net(ret_next: np.ndarray, positions: np.ndarray, cost_bps: float) -> np.ndarray:
    """Net returns for a circular-shift null with boundary-invariant turnover.

    evaluate.permutation_test preserves run lengths but its normal accounting treats the array
    boundary as flat. A shifted episode can cross that boundary, changing its charged turnover. PEAD
    uses the cyclic predecessor instead: every episode has exactly one entry and exit at every shift.
    The real PEAD streams end flat, so their cyclic and normal accounting are identical.
    """
    positions = np.asarray(positions, dtype=float)
    ret_next = np.asarray(ret_next, dtype=float)
    if len(positions) == 0:
        return np.array([], dtype=float)
    turnover = np.abs(positions - np.roll(positions, 1))
    return positions * ret_next - (cost_bps / 10000.0) * turnover


def event_permutation_test(
    streams: list[Stream], cost_bps: float, timeframe: str, n_perms: int, seed: int,
) -> dict:
    """Shift each ticker's complete event-position history while preserving clusters and costs."""
    streams = list(streams)
    rng = np.random.default_rng(seed)
    aligner = _Aligner(streams)

    def metrics(position_sets: list[np.ndarray]) -> tuple[float, float]:
        nets = [
            _cyclic_net(stream.ret_next, positions, cost_bps)
            for stream, positions in zip(streams, position_sets)
        ]
        return _exp_sharpe(aligner.portfolio(nets), timeframe)

    real_exp, real_sharpe = metrics([s.positions for s in streams])
    null_exp = np.empty(n_perms, dtype=float)
    null_sharpe = np.empty(n_perms, dtype=float)
    for i in range(n_perms):
        shifted = []
        for stream in streams:
            n = len(stream.positions)
            offset = int(rng.integers(1, n)) if n > 1 else 0
            shifted.append(np.roll(stream.positions, offset))
        null_exp[i], null_sharpe[i] = metrics(shifted)

    def summary(real: float, null: np.ndarray) -> dict:
        p_value = (float(np.sum(null >= real)) + 1.0) / (n_perms + 1.0)
        return {
            "real": round(real, 6), "nullMean": round(float(null.mean()), 6),
            "null95": round(float(np.percentile(null, 95)), 6),
            "pValue": round(p_value, 4),
        }

    return {
        "permutations": n_perms,
        "method": (
            "independent circular shift per ticker; preserves event clusters, run lengths, "
            "date alignment and cyclic turnover"
        ),
        "expectancy": summary(real_exp, null_exp),
        "sharpe": summary(real_sharpe, null_sharpe),
    }


def event_sufficiency(
    all_pool: dict, holdout_pool: dict, tickers: list[str], cfg: EventConfig,
    failed: list[str], skipped: list[str], estimate_vintage_mode: str,
) -> dict:
    configured = len(cfg.universe)
    evaluated = len(set(tickers))
    coverage = evaluated / configured if configured else 0.0
    return {
        "nDates": int(all_pool.get("nDates", 0)), "minDates": cfg.min_dates,
        "holdoutDates": int(holdout_pool.get("nDates", 0)),
        "minHoldoutDates": cfg.min_holdout_dates,
        "nStreams": evaluated, "minTickers": cfg.min_tickers,
        "configuredTickers": configured, "evaluatedTickers": sorted(set(tickers)),
        "coverage": round(coverage, 4), "minCoverage": cfg.min_coverage,
        "failedTickers": sorted(set(failed)), "skippedTickers": sorted(set(skipped)),
        "estimateVintageMode": estimate_vintage_mode,
        "note": "Evidence sufficiency only; these thresholds can only produce INCONCLUSIVE.",
    }


def decide_event_verdict(
    all_pool: dict, holdout_pool: dict, holdout_benchmark: dict, holdout_perm: dict,
    cfg: EventConfig, suff: dict,
) -> tuple[str, float, list[str]]:
    """PEAD-specific gate: the untouched holdout owns every edge-confirmation check."""
    p_value = max(
        holdout_perm["expectancy"]["pValue"], holdout_perm["sharpe"]["pValue"]
    )
    alpha = 0.05 / max(len(cfg.horizons), 1)
    if all_pool["sharpe"] > 3.0 or holdout_pool["sharpe"] > 3.0:
        return "SUSPECT", p_value, [
            f"pooled Sharpe {all_pool['sharpe']} (holdout {holdout_pool['sharpe']}) > 3 — "
            "investigate leakage or event alignment before using this result."
        ]
    if all_pool["nEvents"] < cfg.min_events:
        return "INCONCLUSIVE", p_value, [
            f"only {all_pool['nEvents']} usable events; need >= {cfg.min_events}.",
            "widen EVENT_UNIVERSE or EVENT_HISTORY_DAYS; do not tune the rule.",
        ]
    if reasons := _insufficiency_reasons(suff):
        return "INCONCLUSIVE", p_value, [
            "the PEAD sample is too small to judge:", *[f"  - {r}" for r in reasons],
        ]
    if suff.get("estimateVintageMode") != "forward_verified":
        return "INCONCLUSIVE", p_value, [
            "this is a descriptive run over unverified historical estimatedEPS values; it cannot "
            "mint EDGE.",
            "the primary PEAD sample switches automatically to individually verified pre-release "
            "PostgreSQL snapshots once enough forward events have accumulated.",
        ]

    checks = {
        "positive untouched-holdout abnormal expectancy after costs": holdout_pool["expectancy"] > 0,
        "0 < untouched-holdout Sharpe <= 3": 0 < holdout_pool["sharpe"] <= 3,
        "untouched holdout beats matched long abnormal-return baseline": (
            holdout_pool["sharpe"] > holdout_benchmark["sharpe"]
        ),
        f"holdout circular-shift p < {alpha:.4f} after {len(cfg.horizons)} horizon tests "
        f"(p={p_value})": p_value < alpha,
        f"events >= {cfg.min_events}": all_pool["nEvents"] >= cfg.min_events,
        f"portfolio dates >= {suff['minDates']} (n={suff['nDates']})": True,
        f"holdout dates >= {suff['minHoldoutDates']} (n={suff['holdoutDates']})": True,
        f"evaluated tickers >= {suff['minTickers']} (n={suff['nStreams']})": True,
        (f"ticker coverage >= {suff['minCoverage']:.0%} "
         f"({suff['nStreams']}/{suff['configuredTickers']} = {suff['coverage']:.0%})"): True,
    }
    checklist = [f"{'PASS' if ok else 'FAIL'}  {name}" for name, ok in checks.items()]
    return ("EDGE" if all(checks.values()) else "NO EDGE"), p_value, checklist


def run_test1(
    report_ev: list[dict], holdout_ev: list[dict], cfg: EventConfig,
    price_frames: dict[str, object], benchmark_frame, *, failed: list[str] | None = None,
) -> dict:
    tradeable = report_ev + holdout_ev
    estimate_vintage_mode = (
        "forward_verified"
        if tradeable and all(event.get("estimateVerified") is True for event in tradeable)
        else "unverified_descriptive"
    )
    cutoff = min((e["entryDate"] for e in holdout_ev), default="")
    per_horizon = {}
    for h in cfg.horizons:
        all_streams, all_used = _event_streams(tradeable, price_frames, benchmark_frame, h)
        report_streams, report_used = _event_streams(report_ev, price_frames, benchmark_frame, h)
        holdout_streams, holdout_used = _event_streams(holdout_ev, price_frames, benchmark_frame, h)
        bh_streams, bh_used = _event_streams(
            holdout_used, price_frames, benchmark_frame, h, direction_override=1.0
        )
        all_pool = _portfolio_pool(all_streams, all_used, cfg)
        report_pool = _portfolio_pool(report_streams, report_used, cfg)
        holdout_pool = _portfolio_pool(holdout_streams, holdout_used, cfg)
        bh = _portfolio_pool(bh_streams, bh_used, cfg)
        perm = event_permutation_test(
            holdout_streams, cfg.cost_bps, cfg.timeframe, cfg.permutations, cfg.seed + h
        )
        evaluated = sorted({e["ticker"] for e in all_used})
        skipped = [t for t in cfg.universe if t not in evaluated]
        suff = event_sufficiency(
            all_pool, holdout_pool, evaluated, cfg, failed or [], skipped,
            estimate_vintage_mode,
        )
        verdict, p_value, checklist = decide_event_verdict(
            all_pool, holdout_pool, bh, perm, cfg, suff
        )
        per_horizon[h] = {
            "verdict": verdict, "meaning": EVENT_VERDICT_MEANING[verdict], "pValue": p_value,
            "checklist": checklist,
            "all": all_pool, "reportEra": report_pool, "holdout": holdout_pool,
            "buyHold": {"sharpe": bh["sharpe"], "expectancy": bh["expectancy"],
                        "totalReturn": bh["totalReturn"], "hitRate": bh["hitRate"],
                        "nDates": bh["nDates"]},
            "permutation": perm,
            "sufficiency": suff,
            "beatsBuyHold": holdout_pool["sharpe"] > bh["sharpe"],
        }
    return {"holdoutCutoff": cutoff, "reportEvents": len(report_ev),
            "holdoutEvents": len(holdout_ev), "perHorizon": per_horizon}


# --------------------------------------------------------------------------- Test 2 (AI marginal value)

def load_event_text(text_dir: str, ticker: str, reported_date: str) -> str:
    """Read event-time text from PostgreSQL, with legacy files as an import/fallback path."""
    if _db.enabled():
        stored = _db.load_earnings_text(ticker, reported_date)
        if stored is not None:
            return str(stored.get("text") or "").strip()
    base = os.path.join(text_dir, ticker.upper())
    for ext, is_json in ((".txt", False), (".json", True)):
        path = os.path.join(base, reported_date + ext)
        if os.path.exists(path):
            try:
                with open(path) as fh:
                    if is_json:
                        text = str(json.load(fh).get("text") or "").strip()
                    else:
                        text = fh.read().strip()
                if text and _db.enabled():
                    _db.save_earnings_text(ticker, reported_date, "legacy-file", text)
                return text
            except (OSError, json.JSONDecodeError):
                return ""
    return ""


def _llm_scorer(llm_url: str):
    """Default scorer: POST event-time text to the LLM /score-earnings endpoint. Returns the
    structured dict, or None if the service is unreachable (the event is then dropped from Test 2)."""
    def score(ticker: str, as_of: str, text: str):
        try:
            resp = requests.post(
                f"{llm_url}/score-earnings",
                json={"ticker": ticker, "asOf": as_of, "text": text},
                timeout=60,
            )
            resp.raise_for_status()
            return resp.json().get("structured")
        except Exception:  # noqa: BLE001 — llm down: skip this event (Test 2 stays honest about coverage)
            return None
    return score


def run_test2(tradeable: list[dict], cfg: EventConfig, scorer=None) -> dict:
    """Does combining SUE with the AI tone improve Test 1 on the SAME events? Runs ONLY on events with
    retrievable event-time text; reports the increment (combined - base) per horizon and the sample
    size. INCONCLUSIVE below EVENT_MIN — no pretending. `scorer(ticker, asOf, text)->dict|None` is
    injectable for tests."""
    if not cfg.enable_ai:
        return {"verdict": "DISABLED", "nWithText": 0, "perHorizon": {}}
    scorer = scorer or _llm_scorer(cfg.llm_url)

    scored: list[dict] = []
    for e in tradeable:
        text = load_event_text(cfg.text_dir, e["ticker"], e["reportedDate"])
        if not text:
            continue
        ai = scorer(e["ticker"], e["reportedDate"], text)
        if not ai:
            continue
        aug = dict(e)
        aug["aiTone"] = float(ai.get("tone", 0.0) or 0.0)
        aug["aiGuidance"] = str(ai.get("guidanceDirection", "flat"))
        scored.append(aug)

    n = len(scored)
    base = {"nWithText": n, "minEvents": cfg.min_events, "textDir": cfg.text_dir}
    if n < cfg.min_events:
        base["verdict"] = "INCONCLUSIVE"
        base["reason"] = (
            f"only {n} tradeable events had retrievable event-time text (need >= {cfg.min_events}). "
            "Historical earnings text is not freely available at scale, so this test ACCUMULATES "
            "FORWARD in prediction.earnings_event_texts. Legacy files under "
            "data/eval/earnings_text/{TICKER}/{reportedDate}.txt are imported when present. Do not "
            "read the increment below as a result at this sample size."
        )
        base["perHorizon"] = _test2_per_horizon(scored, cfg) if n else {}
        return base

    base["verdict"] = "REPORTED"
    base["perHorizon"] = _test2_per_horizon(scored, cfg)
    return base


def _test2_per_horizon(scored: list[dict], cfg: EventConfig) -> dict:
    """Per horizon: Test-1 metrics on the text subset (base) vs Test-1+AI (trade only when the SUE
    direction and the AI tone AGREE), and the increment between them."""
    per_h = {}
    for h in cfg.horizons:
        subset = [e for e in scored if h in e["returns"]]
        d1, r1, _ = _arrays(subset, h)
        base_m = event_metrics(event_net(d1, r1, cfg.cost_bps), d1, r1, h)

        agree = [e for e in subset if e.get("aiTone", 0.0) != 0.0 and np.sign(e["aiTone"]) == e["direction"]]
        d2, r2, _ = _arrays(agree, h)
        comb_m = event_metrics(event_net(d2, r2, cfg.cost_bps), d2, r2, h) if len(d2) else _empty_event_pool()

        per_h[h] = {
            "base": base_m, "combined": comb_m, "nAgree": int(len(d2)),
            "increment": {
                "expectancy": round(comb_m["expectancy"] - base_m["expectancy"], 6),
                "sharpe": round(comb_m["sharpe"] - base_m["sharpe"], 3),
                "hitRate": round(comb_m["hitRate"] - base_m["hitRate"], 4),
            },
        }
    return per_h


# --------------------------------------------------------------------------- orchestration

def _utc_now() -> datetime:
    return datetime.now(timezone.utc)


def run(cfg: EventConfig, *, now: datetime | None = None, write: bool = True, scorer=None) -> tuple[dict, int]:
    """Gather earnings (cache-first) -> refuse if no key/cache -> fetch prices (refuse on synthetic) ->
    build events -> tercile filter -> Test 1 (+ Test 2) -> verdict/report. Returns (report, exit_code);
    never calls sys.exit. `scorer` is injectable for Test 2 in tests."""
    now = now or _utc_now()
    started = _utc_now()

    # 1. Earnings actuals are cache-first. Historical estimatedEPS rows remain descriptive. The
    # primary sample switches to forward mode only when a ticker has enough individually matched,
    # pre-release PostgreSQL snapshots to compute prior-only SUE without borrowing unverified rows.
    historical_earnings: dict[str, list] = {}
    forward_earnings: dict[str, list] = {}
    sources = {"cache": 0, "live": 0, "missing": 0}
    estimate_provenance = {
        "historicalEvents": 0, "snapshotsAttached": 0, "verifiedReadyEvents": 0,
    }
    for t in cfg.universe:
        evs, src = get_earnings(t, cfg.cache_dir, cfg.av_key)
        if evs:
            historical_earnings[t] = evs
            estimate_provenance["historicalEvents"] += len(evs)
            snapshots = _db.list_estimate_snapshots(t) if _db.enabled() else []
            estimate_provenance["snapshotsAttached"] += attach_forward_estimates(evs, snapshots)
            verified = [event for event in evs if event.estimate_verified]
            compute_sue(verified)
            if any(event.sue is not None for event in verified):
                forward_earnings[t] = verified
                estimate_provenance["verifiedReadyEvents"] += sum(
                    event.sue is not None for event in verified
                )
            sources[src] = sources.get(src, 0) + 1
        else:
            sources["missing"] += 1

    if not historical_earnings:
        if not cfg.av_key:
            _print_refusal_no_key(cfg)
            return {"refused": "no_key", "verdict": None}, EXIT_NO_KEY
        _print_refusal_no_data([], note="no earnings history returned (Alpha Vantage rate-limited? 25/day cap).")
        return {"refused": "no_data", "verdict": None}, EXIT_NO_DATA

    if forward_earnings:
        earnings = forward_earnings
        estimate_vintage_mode = "forward_verified"
    else:
        # Keep the existing historical study visible while the forward corpus is young, but derive
        # its permanent INCONCLUSIVE status from the data mode — never from an operator trust flag.
        for evs in historical_earnings.values():
            compute_sue(evs)
        earnings = historical_earnings
        estimate_vintage_mode = "unverified_descriptive"

    # 2. The market benchmark is part of the outcome definition, not optional context. PEAD is
    # judged on stock-minus-benchmark open-to-open returns so a broad market rally cannot masquerade
    # as earnings drift.
    try:
        benchmark_frame, benchmark_source, benchmark_is_syn = fetch_feature_frame(
            cfg.benchmark, cfg.timeframe, cfg.history_days, cfg.analysis_url
        )
    except Exception as e:  # noqa: BLE001
        _print_refusal_no_data([(cfg.benchmark, str(e))], note="PEAD benchmark fetch failed.")
        return {"refused": "no_benchmark", "verdict": None, "benchmark": cfg.benchmark}, EXIT_NO_DATA
    if benchmark_is_syn:
        _print_refusal_synthetic(cfg.benchmark, benchmark_source)
        return {
            "refused": "synthetic", "verdict": None,
            "message": f"benchmark {cfg.benchmark} resolved to source '{benchmark_source}'.",
        }, EXIT_SYNTHETIC

    # 3. Price bars per ticker (REAL prices); refuse on the FIRST synthetic resolution.
    all_events: list[dict] = []
    price_frames: dict[str, object] = {}
    price_failed: list[tuple[str, str]] = []
    for t, evs in earnings.items():
        try:
            df, source, is_syn = fetch_feature_frame(t, cfg.timeframe, cfg.history_days, cfg.analysis_url)
        except Exception as e:  # noqa: BLE001
            price_failed.append((t, str(e)))
            continue
        if is_syn:
            _print_refusal_synthetic(t, source)
            return {"refused": "synthetic", "verdict": None,
                    "message": f"{t} resolved to source '{source}' (synthetic seed)."}, EXIT_SYNTHETIC
        price_frames[t] = df
        built = build_events(evs, df, cfg.horizons)
        # explicit no-look-ahead guard (build_events already asserts; re-assert here for the DoD)
        for ev in built:
            assert ev["entryDate"] > ev["reportedDate"], "entry must be strictly after reportedDate"
        all_events.extend(built)

    if not all_events:
        _print_refusal_no_data(price_failed, note="no earnings events had a full forward price window.")
        return {"refused": "no_data", "verdict": None, "priceFailed": price_failed}, EXIT_NO_DATA

    # 4. Split BEFORE selecting strong surprises. The report era owns the SUE thresholds; the
    # untouched holdout only receives those frozen values and cannot influence its own membership.
    report_candidates, holdout_candidates, cutoff = split_by_date(
        all_events, cfg.holdout_frac
    )
    report_tradeable, sue_lo, sue_hi = apply_tercile_filter(
        report_candidates, cfg.sue_quantile
    )
    holdout_tradeable = apply_sue_thresholds(holdout_candidates, sue_lo, sue_hi)
    tradeable = report_tradeable + holdout_tradeable
    if not tradeable:
        _print_refusal_no_data([], note="no events fell in the strong-surprise terciles.")
        return {"refused": "no_data", "verdict": None}, EXIT_NO_DATA

    # 5. Test 1 (PEAD rule) and Test 2 (AI marginal value). Test 2 is descriptive and disabled by
    # default until a forward, durable event-time text corpus exists.
    test1 = run_test1(
        report_tradeable, holdout_tradeable, cfg, price_frames, benchmark_frame,
        failed=[t for t, _ in price_failed] + [t for t in cfg.universe if t not in earnings],
    )
    test2 = run_test2(tradeable, cfg, scorer=scorer)

    # 6. Per-ticker breakdown (first horizon), for the descriptive report table.
    h0 = cfg.horizons[0]
    per_ticker = _per_ticker_breakdown(tradeable, h0, cfg)

    elapsed = (_utc_now() - started).total_seconds()
    report = {
        "study": "post-earnings drift (PEAD)",
        "survivorshipWarning": SURVIVORSHIP_WARNING,
        "generatedAt": now.isoformat(),
        "elapsedSec": round(elapsed, 1),
        "config": {
            "universe": cfg.universe, "tickersWithEarnings": sorted(earnings.keys()),
            "horizons": cfg.horizons, "sueQuantile": cfg.sue_quantile, "minEvents": cfg.min_events,
            "costBps": cfg.cost_bps, "permutations": cfg.permutations, "holdoutFrac": cfg.holdout_frac,
            "seed": cfg.seed, "historyDays": cfg.history_days, "enableAI": cfg.enable_ai,
            "benchmark": cfg.benchmark,
            "minDates": cfg.min_dates, "minHoldoutDates": cfg.min_holdout_dates,
            "minTickers": cfg.min_tickers, "minCoverage": cfg.min_coverage,
            "estimateVintagePolicy": "per-event-forward-snapshot",
            "studyVersion": PEAD_STUDY_VERSION,
        },
        "method": EVALUATION_METHOD + "+" + PEAD_STUDY_VERSION,
        "benchmark": {
            "ticker": cfg.benchmark, "source": benchmark_source,
            "return": "stock open-to-open minus benchmark open-to-open",
        },
        "estimateVintage": {
            "verified": estimate_vintage_mode == "forward_verified",
            "status": estimate_vintage_mode,
            "warning": (
                "Primary metrics use only estimates captured before each report date."
                if estimate_vintage_mode == "forward_verified"
                else "Historical estimatedEPS remains descriptive. EDGE is disabled until enough "
                     "individually verified forward snapshots have accumulated."
            ),
            **estimate_provenance,
        },
        "earningsSources": sources,
        "totals": {
            "eventsBuilt": len(all_events), "tradeable": len(tradeable),
            "sueTercileLow": round(sue_lo, 4), "sueTercileHigh": round(sue_hi, 4),
            "reportEvents": test1["reportEvents"], "holdoutEvents": test1["holdoutEvents"],
            "holdoutCutoff": cutoff,
        },
        "test1": test1,
        "test2": test2,
        "perTicker": per_ticker,
        "priceFailed": [{"ticker": t, "error": e} for t, e in price_failed],
    }
    # headline verdict = the per-horizon verdicts (Test 1), summarized
    report["verdictByHorizon"] = {h: test1["perHorizon"][h]["verdict"] for h in cfg.horizons}
    report["byHorizon"] = {str(h): test1["perHorizon"][h] for h in cfg.horizons}
    report["verdict"] = _headline_verdict(list(report["verdictByHorizon"].values()))
    report["meaning"] = EVENT_VERDICT_MEANING[report["verdict"]]

    report["files"] = _write_report(report, cfg, now) if write else {}
    _print_report(report)
    return report, EXIT_OK


def _per_ticker_breakdown(tradeable: list[dict], horizon: int, cfg: EventConfig) -> list[dict]:
    by_ticker: dict[str, list[dict]] = {}
    for e in tradeable:
        by_ticker.setdefault(e["ticker"], []).append(e)
    rows = []
    for t, evs in by_ticker.items():
        d, r, _ = _arrays(evs, horizon)
        if len(d) == 0:
            continue
        m = event_metrics(event_net(d, r, cfg.cost_bps), d, r, horizon)
        rows.append({
            "ticker": t, "horizon": horizon, "nEvents": m["nEvents"], "hitRate": m["hitRate"],
            "expectancy": m["expectancy"], "sharpe": m["sharpe"], "totalReturn": m["totalReturn"],
        })
    rows.sort(key=lambda x: x["ticker"])
    return rows


# --------------------------------------------------------------------------- reporting

def _write_report(report: dict, cfg: EventConfig, now: datetime) -> dict:
    stamp = now.strftime("%Y%m%dT%H%M%SZ")
    files = {
        "json": os.path.join(cfg.out_dir, f"event-report-{stamp}.json"),
        "md": os.path.join(cfg.out_dir, f"event-report-{stamp}.md"),
    }
    report["files"] = files
    markdown = _render_md(report)
    payload = json.dumps(report, indent=2, default=str).encode()
    if _db.enabled():
        _db.save_artifact(os.path.basename(files["md"]), "text/markdown", markdown.encode())
        _db.save_artifact(os.path.basename(files["json"]), "application/json", payload)
    else:
        os.makedirs(cfg.out_dir, exist_ok=True)
        with open(files["md"], "w") as f:
            f.write(markdown)
        with open(files["json"], "wb") as f:
            f.write(payload)
    return files


def _render_md(r: dict) -> str:
    L: list[str] = []
    vb = " · ".join(f"h={h}: **{v}**" for h, v in r["verdictByHorizon"].items())
    L.append("# Event study — post-earnings drift (PEAD)")
    L.append("")
    L.append(f"Test 1 verdict — {vb}")
    L.append("")
    L.append(f"> ⚠ {r['survivorshipWarning']}")
    L.append("")
    L.append(f"> Benchmark: {r['benchmark']['ticker']} — {r['benchmark']['return']}.")
    L.append(f"> Estimate vintage: **{r['estimateVintage']['status']}** — "
             f"{r['estimateVintage']['warning']}")
    L.append("")
    L.append(f"_{r['generatedAt']} · {r['elapsedSec']}s · seed {r['config']['seed']} · "
             f"{r['totals']['tradeable']} tradeable events of {r['totals']['eventsBuilt']} built · "
             f"holdout after {r['totals']['holdoutCutoff']}_")
    L.append("")

    L.append("## Test 1 — SUE-based PEAD (no AI)")
    for h in r["config"]["horizons"]:
        ph = r["test1"]["perHorizon"][h]
        a, ho = ph["all"], ph["holdout"]
        bh = ph["buyHold"]
        L.append("")
        L.append(f"### Horizon {h} sessions — **{ph['verdict']}**")
        L.append("> " + ph["meaning"])
        L.append("")
        for c in ph["checklist"]:
            L.append(f"- {c}")
        L.append("")
        L.append("| metric | all events | report era | untouched holdout |")
        L.append("|---|--:|--:|--:|")
        re_ = ph["reportEra"]
        L.append(f"| events | {a['nEvents']} | {re_['nEvents']} | {ho['nEvents']} |")
        L.append(f"| hit rate | {a['hitRate']} | {re_['hitRate']} | {ho['hitRate']} |")
        L.append(f"| avg return / event | {a['avgReturnPct']}% | {re_['avgReturnPct']}% | {ho['avgReturnPct']}% |")
        L.append(f"| expectancy | {a['expectancy']} | {re_['expectancy']} | {ho['expectancy']} |")
        L.append(f"| Sharpe (annualized) | {a['sharpe']} | {re_['sharpe']} | {ho['sharpe']} |")
        L.append(f"| max drawdown | {_fmt_pct(a['maxDrawdown'])} | {_fmt_pct(re_['maxDrawdown'])} | {_fmt_pct(ho['maxDrawdown'])} |")
        L.append(f"| profit factor | {a['profitFactor']} | {re_['profitFactor']} | {ho['profitFactor']} |")
        L.append("")
        L.append(f"**Matched long abnormal-return baseline**: Sharpe {bh['sharpe']}, expectancy {bh['expectancy']} "
                 f"(strategy beats it risk-adjusted: {'yes' if ph['beatsBuyHold'] else 'no'}).")
        p = ph["permutation"]
        L.append("")
        L.append("| permutation null | real | null mean | null 95th | p-value |")
        L.append("|---|--:|--:|--:|--:|")
        for key in ("expectancy", "sharpe"):
            s = p[key]
            L.append(f"| {key} | {s['real']} | {s['nullMean']} | {s['null95']} | {s['pValue']} |")

    L.append("")
    L.append("## Test 2 — AI marginal value")
    t2 = r["test2"]
    if t2.get("verdict") == "DISABLED":
        L.append("_Disabled (EVENT_ENABLE_AI=false)._")
    else:
        L.append(f"**{t2['verdict']}** — {t2['nWithText']} of the tradeable events had retrievable "
                 f"event-time text (need ≥ {t2.get('minEvents')}).")
        if t2.get("reason"):
            L.append("")
            L.append(f"> {t2['reason']}")
        if t2.get("perHorizon"):
            L.append("")
            L.append("| horizon | base expectancy | +AI expectancy | Δexpectancy | Δsharpe | ΔhitRate | n(agree) |")
            L.append("|--:|--:|--:|--:|--:|--:|--:|")
            for h, ph in t2["perHorizon"].items():
                inc = ph["increment"]
                L.append(f"| {h} | {ph['base']['expectancy']} | {ph['combined']['expectancy']} | "
                         f"{inc['expectancy']} | {inc['sharpe']} | {inc['hitRate']} | {ph['nAgree']} |")
            L.append("")
            L.append("_The Δ columns are the honest answer to \"does AI add value here\" — on this sample._")

    L.append("")
    L.append("## Per-ticker (horizon " + str(r["config"]["horizons"][0]) + ")")
    L.append("")
    L.append("| ticker | events | hit | expectancy | Sharpe | total ret |")
    L.append("|---|--:|--:|--:|--:|--:|")
    for t in r["perTicker"]:
        L.append(f"| {t['ticker']} | {t['nEvents']} | {t['hitRate']} | {t['expectancy']} | "
                 f"{t['sharpe']} | {_fmt_pct(t['totalReturn'])} |")
    if r["priceFailed"]:
        L.append("")
        L.append(f"_Price fetch failed: {', '.join(f['ticker'] for f in r['priceFailed'])}._")
    L.append("")
    L.append("---")
    L.append("_Research tool. No buy/sell output, no orders, no money. Entry is the open of the first "
             "session AFTER the report (no look-ahead); costs are subtracted; results are on SURVIVORS only._")
    L.append("")
    return "\n".join(L)


def _print_report(r: dict) -> None:
    bar = "=" * 68
    print(bar)
    vb = "   ".join(f"h={h}: {v}" for h, v in r["verdictByHorizon"].items())
    print(f"  EVENT STUDY (PEAD) — Test 1 verdict:  {vb}")
    print(bar)
    print(_wrap("⚠ " + r["survivorshipWarning"]))
    print()
    print(f"  {r['totals']['tradeable']} tradeable events of {r['totals']['eventsBuilt']} built · "
          f"holdout after {r['totals']['holdoutCutoff']} · "
          f"earnings sources: {r['earningsSources']}")
    for h in r["config"]["horizons"]:
        ph = r["test1"]["perHorizon"][h]
        a, ho, bh = ph["all"], ph["holdout"], ph["buyHold"]
        p = ph["permutation"]
        print()
        print(f"  ── Horizon {h} sessions: {ph['verdict']}")
        for c in ph["checklist"]:
            print(f"       {c}")
        print(f"     all    : n={a['nEvents']}  hit={a['hitRate']}  exp={a['expectancy']}  "
              f"Sharpe={a['sharpe']}  maxDD={_fmt_pct(a['maxDrawdown'])}  pf={a['profitFactor']}")
        print(f"     holdout: n={ho['nEvents']}  hit={ho['hitRate']}  exp={ho['expectancy']}  Sharpe={ho['sharpe']}")
        print(f"     long α : Sharpe={bh['sharpe']}  exp={bh['expectancy']}  "
              f"(strategy beats: {'yes' if ph['beatsBuyHold'] else 'no'})")
        print(f"     null   : expectancy p={p['expectancy']['pValue']}  Sharpe p={p['sharpe']['pValue']}")

    t2 = r["test2"]
    print()
    if t2.get("verdict") == "DISABLED":
        print("  Test 2 (AI marginal value): DISABLED")
    else:
        print(f"  Test 2 (AI marginal value): {t2['verdict']} — {t2['nWithText']} events with text "
              f"(need >= {t2.get('minEvents')})")
        for h, ph in (t2.get("perHorizon") or {}).items():
            inc = ph["increment"]
            print(f"     h={h}: Δexpectancy={inc['expectancy']}  Δsharpe={inc['sharpe']}  "
                  f"ΔhitRate={inc['hitRate']}  n(agree)={ph['nAgree']}")

    files = r.get("files") or {}
    if files:
        print()
        print(f"  Report written: {files.get('md')}")
        print(f"                  {files.get('json')}")
    print(bar)


def _print_refusal_no_key(cfg: EventConfig) -> None:
    bar = "=" * 68
    print(bar)
    print("  NO EARNINGS DATA — cannot run the event study")
    print(bar)
    print(_wrap(
        "The event study needs earnings history from Alpha Vantage (function=EARNINGS), but no "
        "ALPHAVANTAGE_API_KEY is set and no durable payload exists in PostgreSQL (or the local "
        f"fallback {cfg.cache_dir}). Set ALPHAVANTAGE_API_KEY (free, ~25 req/day) and re-run; the harness "
        "persists each ticker's full raw payload, so a large universe can be completed across a "
        "couple of days or with a smaller EVENT_UNIVERSE. (Alternative provider: FMP "
        "earnings-surprises — not wired here.)"
    ))
    print(bar)


def main(argv: list[str] | None = None) -> int:
    cfg = EventConfig.from_env()
    print(_wrap(
        f"Event study (PEAD) over {len(cfg.universe)} tickers, horizons {cfg.horizons}. Earnings from "
        "Alpha Vantage (cached), prices from analysis. This can be long-running…",
        indent="",
    ))
    _report, code = run(cfg)
    return code


if __name__ == "__main__":
    sys.exit(main())
