"""Forward-only paired scoring for one frozen challenger and one frozen active model.

Shadow evidence is deliberately separate from the official paper book. Both versions see the same
completed real feature frame and the same next-bar close; each pays its own validated turnover cost.
The result can inform a later human promotion review, but it cannot move the serving pointer.
"""
from __future__ import annotations

import math
from dataclasses import dataclass

import numpy as np
import pandas as pd

from . import db
from .committee_features import load_committee_history
from .config import ALPHAVANTAGE_API_KEY, EARNINGS_CACHE_DIR
from .context import fetch_context, load_earnings
from .features import FEATURE_FRAME_POLICY, MODEL_FEATURES, fetch_feature_frame, latest_feature_row
from .model import derive_direction, predict_prob
from .store import load_version_model


class ShadowDeferred(Exception):
    """No new trustworthy completed bar is available yet; retry on a later controller poll."""


class ShadowInvalid(Exception):
    """The frozen comparison cannot be scored and should be made visibly terminal."""


@dataclass(frozen=True)
class VersionScore:
    model_version: str
    target: int
    probability: float
    cost_bps: float


def _score_version(ticker: str, timeframe: str, horizon: int, version: str, row) -> VersionScore:
    model, _calibrator, record = load_version_model(ticker, timeframe, horizon, version)
    if model is None or not isinstance(record, dict):
        raise ShadowInvalid(f"model version {version} is unavailable")
    if record.get("trainedOnSynthetic"):
        raise ShadowInvalid(f"model version {version} was trained on synthetic data")
    if record.get("dataPolicy") != FEATURE_FRAME_POLICY:
        raise ShadowInvalid(f"model version {version} does not use {FEATURE_FRAME_POLICY}")
    report = record.get("report") if isinstance(record.get("report"), dict) else {}
    thresholds = report.get("thresholds") if isinstance(report.get("thresholds"), dict) else {}
    try:
        upper = float(thresholds["upper"])
        lower = float(thresholds["lower"])
        cost_bps = float(report["costBps"])
    except (KeyError, TypeError, ValueError) as exc:
        raise ShadowInvalid(f"model version {version} has incomplete strategy identity") from exc
    allow_short = report.get("allowShort")
    if not isinstance(allow_short, bool) or not math.isfinite(cost_bps) or cost_bps < 0:
        raise ShadowInvalid(f"model version {version} has invalid strategy identity")
    columns = record.get("features") or list(getattr(model, "feature_name_", []) or []) or MODEL_FEATURES
    try:
        probability = float(predict_prob(model, row, columns))
    except Exception as exc:  # noqa: BLE001 — corrupt/incompatible model is terminal evidence
        raise ShadowInvalid(f"model version {version} could not score the current frame") from exc
    if not math.isfinite(probability) or not 0 <= probability <= 1:
        raise ShadowInvalid(f"model version {version} returned an invalid probability")
    direction = derive_direction(probability, upper, lower, allow_short)
    target = {"Buy": 1, "Sell": -1, "Hold": 0}[direction]
    return VersionScore(version, target, probability, cost_bps)


def score_pair(trial: dict, *, lookback_days: int) -> dict:
    ticker, timeframe, horizon = trial["ticker"], trial["timeframe"], trial["horizon"]
    candidate = trial.get("candidateModelVersion")
    champion = trial.get("championModelVersion")
    if not candidate or not champion:
        raise ShadowInvalid("paired shadow requires frozen candidate and champion versions")
    if candidate == champion:
        raise ShadowInvalid("candidate and champion versions are identical")
    try:
        frame, source, synthetic = fetch_feature_frame(ticker, timeframe, lookback_days)
    except Exception as exc:  # noqa: BLE001 — transient analysis/provider outage; retry later
        raise ShadowDeferred(f"completed feature frame unavailable: {exc}") from exc
    if synthetic:
        raise ShadowDeferred(f"completed feature frame is synthetic ({source})")
    if frame.empty or "close" not in frame.columns:
        raise ShadowDeferred("completed feature frame is empty or has no close")

    context = fetch_context(timeframe, lookback_days)
    earnings = load_earnings(ticker, EARNINGS_CACHE_DIR, ALPHAVANTAGE_API_KEY)
    committee = load_committee_history(ticker)
    row, as_of = latest_feature_row(
        frame, ctx=context, earnings=earnings, committee=committee
    )
    if row is None or as_of is None:
        raise ShadowDeferred("latest completed frame has no usable lagged feature row")
    try:
        close = float(frame["close"].iloc[-1])
    except (TypeError, ValueError, IndexError) as exc:
        raise ShadowDeferred("latest completed frame has no usable close") from exc
    if not math.isfinite(close) or close <= 0:
        raise ShadowDeferred("latest completed frame has an invalid close")

    candidate_score = _score_version(ticker, timeframe, horizon, candidate, row)
    champion_score = _score_version(ticker, timeframe, horizon, champion, row)
    parsed_bar = pd.to_datetime(as_of, utc=True, errors="coerce")
    if pd.isna(parsed_bar):
        raise ShadowDeferred("latest completed frame has an unparseable timestamp")
    # UTC ISO strings preserve chronological order in PostgreSQL's idempotency comparison.
    bar_time = parsed_bar.isoformat()
    return {
        "barTime": bar_time, "close": close, "source": str(source or "unknown"),
        "candidate": candidate_score, "champion": champion_score,
    }


def metrics(rows: list[dict], min_paired_bars: int) -> dict:
    settled = [
        row for row in rows
        if row.get("candidateNetReturn") is not None and row.get("championNetReturn") is not None
    ]
    candidate = np.asarray([float(row["candidateNetReturn"]) for row in settled], dtype=float)
    champion = np.asarray([float(row["championNetReturn"]) for row in settled], dtype=float)
    candidate_total = float(np.prod(1.0 + candidate) - 1.0) if len(candidate) else 0.0
    champion_total = float(np.prod(1.0 + champion) - 1.0) if len(champion) else 0.0
    delta = candidate_total - champion_total
    measured = len(settled) >= min_paired_bars
    result = "unmeasured"
    if measured:
        if abs(delta) < 1e-12:
            result = "tied"
        else:
            result = "candidate-ahead" if delta > 0 else "champion-ahead"
    return {
        "pairedBars": len(settled), "requiredPairedBars": min_paired_bars,
        "measured": measured, "result": result,
        "candidateTotalReturn": candidate_total,
        "championTotalReturn": champion_total,
        "returnDelta": delta,
        "candidateAheadBars": int(np.sum(candidate > champion)) if len(candidate) else 0,
        "championAheadBars": int(np.sum(champion > candidate)) if len(candidate) else 0,
        "ties": int(np.sum(candidate == champion)) if len(candidate) else 0,
        "firstBar": rows[0]["barTime"] if rows else None,
        "lastSettledBar": settled[-1]["nextBarTime"] if settled else None,
        "note": (
            "Paired prospective net returns on identical future completed bars. This is research "
            "evidence, not permission to promote or use real money."
        ),
    }


def advance_trial(trial: dict, *, lookback_days: int, min_paired_bars: int) -> dict:
    scored = score_pair(trial, lookback_days=lookback_days)
    candidate: VersionScore = scored["candidate"]
    champion: VersionScore = scored["champion"]
    write = db.record_shadow_bar(
        trial["id"], bar_time=scored["barTime"], close=scored["close"],
        source=scored["source"], candidate_target=candidate.target,
        champion_target=champion.target, candidate_probability=candidate.probability,
        champion_probability=champion.probability, candidate_cost_bps=candidate.cost_bps,
        champion_cost_bps=champion.cost_bps, min_paired_bars=min_paired_bars,
    )
    rows = db.load_shadow_observations(trial["id"])
    summary = metrics(rows, min_paired_bars)
    if write.get("complete") or summary["measured"]:
        db.set_automation_trial_status(trial["id"], "shadow-complete")
    elif write.get("appended") and trial.get("status") != "shadowing":
        db.set_automation_trial_status(trial["id"], "shadowing")
    return {"write": write, "summary": summary}
