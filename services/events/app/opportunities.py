"""Deterministic early-opportunity research radar over real, completed daily bars.

The detector is intentionally not a signal generator.  It materializes four auditable research
states — emerging, confirmed, extended/no-chase, and invalidated — and stores immutable snapshots
in PostgreSQL.  A score orders evidence under ``DETECTOR_VERSION``; it is not a probability.

The scheduled entrypoint is bounded and model-free.  It may refresh market bars through the
analysis service's existing live provider cascade, but it cannot reach Qwen, prediction, paper,
promotion, a broker, or an order route.  The GET endpoint reads only the latest stored snapshot.
"""
from __future__ import annotations

import hashlib
import json
import math
import os
import secrets
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import date, datetime, timedelta, timezone
from statistics import mean
from urllib.parse import quote

import requests
from fastapi import APIRouter, Query

from .db import Connection, connect
from .scout import UNIVERSE_VERSION, latest_scout, scout_universe

router = APIRouter()

DETECTOR_VERSION = "early-opportunity@2"
RUN_PREFIX = "opp_"
ANALYSIS_URL_ENV = "ANALYSIS_URL"
BENCHMARK_ENV = "OPPORTUNITY_BENCHMARK"
MAX_BAR_AGE_DAYS_ENV = "OPPORTUNITY_MAX_BAR_AGE_DAYS"
MAX_RUN_AGE_SECONDS_ENV = "OPPORTUNITY_MAX_RUN_AGE_SECONDS"

DEFAULT_ANALYSIS_URL = "http://localhost:8001"
DEFAULT_BENCHMARK = "SPY"
DEFAULT_MAX_BAR_AGE_DAYS = 7
DEFAULT_MAX_RUN_AGE_SECONDS = 7 * 24 * 60 * 60
FETCH_TIMEOUT_SECONDS = 12
FETCH_WORKERS = 6
LOOKBACK_DAYS = 500
MIN_ROWS = 60
MAX_LIMIT = 100
ERROR_CAP = 500

# Frozen detector thresholds. Changing any value requires a DETECTOR_VERSION bump.
EMERGING_MIN = 0.62
EVENT_LED_PRICE_MIN = 0.45
EVENT_LED_CONTEXT_MIN = 0.55
INVALIDATION_SCORE = 0.45
CONFIRM_VOLUME_MIN = 1.20
EXTENDED_RETURN_1D = 0.06
EXTENDED_RETURN_2D = 0.08
EXTENDED_ATR = 2.50

PRICE_WEIGHTS = {
    "breakoutProximity": 0.30,
    "relativeStrength": 0.25,
    "trendQuality": 0.20,
    "volumeParticipation": 0.15,
    "rangeCompression": 0.10,
}
assert math.isclose(sum(PRICE_WEIGHTS.values()), 1.0)

STATE_ORDER = {"confirmed": 0, "emerging": 1, "extended": 2, "invalidated": 3}
ACTIVE_STATES = {"emerging", "confirmed", "extended"}

DISCLAIMER = (
    "Research lead, not an investment recommendation. Setup evidence is not a probability, and "
    "paper eligibility has not been assessed."
)
PAPER_ELIGIBILITY = {
    "state": "not-assessed",
    "reason": (
        "Only the independent prediction walk-forward, evaluator verdict, freshness, and "
        "no-synthetic-data gates can make a signal eligible for paper execution."
    ),
}


def _now() -> datetime:
    return datetime.now(timezone.utc)


def _iso(value: datetime) -> str:
    return value.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _parse(value: str | None) -> datetime | None:
    if not value:
        return None
    text = str(value).strip()
    if len(text) == 10:
        try:
            return datetime.combine(date.fromisoformat(text), datetime.min.time(), tzinfo=timezone.utc)
        except ValueError:
            return None
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    try:
        parsed = datetime.fromisoformat(text)
    except ValueError:
        return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc)


def _json(value, default):
    if isinstance(value, type(default)):
        return value
    try:
        decoded = json.loads(value or "")
    except (TypeError, ValueError):
        return default
    return decoded if isinstance(decoded, type(default)) else default


def _positive_int(name: str, default: int) -> int:
    try:
        value = int(os.getenv(name, "").strip() or default)
    except ValueError:
        return default
    return value if value > 0 else default


def _clamp(value: float) -> float:
    return max(0.0, min(1.0, float(value)))


def _scale(value: float, low: float, high: float) -> float:
    if high <= low:
        raise ValueError("scale requires high > low")
    return _clamp((float(value) - low) / (high - low))


def _scale_desc(value: float, good: float, bad: float) -> float:
    return 1.0 - _scale(value, good, bad)


def benchmark_ticker() -> str:
    return os.getenv(BENCHMARK_ENV, "").strip().upper() or DEFAULT_BENCHMARK


def _rows(payload: dict) -> list[dict]:
    rows = payload.get("rows") if isinstance(payload, dict) else None
    return rows if isinstance(rows, list) else []


def _return_for(payload: dict, sessions: int) -> float | None:
    rows = _rows(payload)
    if len(rows) <= sessions:
        return None
    try:
        current = float(rows[-1]["close"])
        prior = float(rows[-1 - sessions]["close"])
    except (KeyError, TypeError, ValueError, ZeroDivisionError):
        return None
    return current / prior - 1.0 if prior else None


def fetch_completed_features(ticker: str, *, as_of: datetime) -> dict:
    """Refresh and return the analysis service's real completed daily feature history."""
    base = os.getenv(ANALYSIS_URL_ENV, DEFAULT_ANALYSIS_URL).rstrip("/")
    url = f"{base}/analysis/{quote(ticker)}/features"
    try:
        response = requests.get(
            url,
            params={
                "timeframe": "1D",
                "lookbackDays": LOOKBACK_DAYS,
                "completedOnly": "true",
            },
            timeout=FETCH_TIMEOUT_SECONDS,
        )
    except requests.RequestException:
        return {"available": False, "reason": "unreachable", "degraded": "analysis:unreachable"}
    if response.status_code in (404, 409, 502):
        return {"available": False, "reason": "insufficient"}
    if not response.ok:
        return {"available": False, "reason": "error", "degraded": "analysis:error"}
    try:
        payload = response.json()
    except (TypeError, ValueError):
        return {"available": False, "reason": "error", "degraded": "analysis:error"}
    if not isinstance(payload, dict) or payload.get("sourceIsSynthetic"):
        return {"available": False, "reason": "synthetic"}
    rows = _rows(payload)
    if len(rows) < MIN_ROWS:
        return {"available": False, "reason": "insufficient"}
    data_through = str(payload.get("dataThrough") or rows[-1].get("time") or "")
    data_at = _parse(data_through)
    max_age = _positive_int(MAX_BAR_AGE_DAYS_ENV, DEFAULT_MAX_BAR_AGE_DAYS)
    if data_at is None or as_of - data_at > timedelta(days=max_age):
        return {"available": False, "reason": "stale", "barTime": data_through or None}
    return {
        "available": True,
        "ticker": ticker,
        "barTime": data_through,
        "source": str(payload.get("source") or "unknown"),
        "payload": payload,
    }


def _true_ranges(rows: list[dict]) -> list[float]:
    out: list[float] = []
    for idx in range(1, len(rows)):
        try:
            high = float(rows[idx]["high"])
            low = float(rows[idx]["low"])
            previous_close = float(rows[idx - 1]["close"])
        except (KeyError, TypeError, ValueError):
            continue
        out.append(max(high - low, abs(high - previous_close), abs(low - previous_close)))
    return out


def compute_price_setup(payload: dict, *, benchmark_return_5d: float | None) -> dict:
    """Pure feature calculation. Identical rows and benchmark return produce identical facts."""
    rows = _rows(payload)
    if len(rows) < MIN_ROWS:
        raise ValueError(f"need at least {MIN_ROWS} completed rows")
    last = rows[-1]
    previous = rows[-2]
    closes = [float(row["close"]) for row in rows]
    highs = [float(row["high"]) for row in rows]
    volumes = [float(row["volume"]) for row in rows]

    close = closes[-1]
    return_1d = close / closes[-2] - 1.0
    return_2d = close / closes[-3] - 1.0
    return_5d = close / closes[-6] - 1.0
    prior_high_20 = max(highs[-21:-1])
    breakout_distance = close / prior_high_20 - 1.0
    prior_volume_20 = mean(volumes[-21:-1])
    relative_volume = volumes[-1] / prior_volume_20 if prior_volume_20 else 0.0

    ema20 = float(last["ema20"])
    previous_ema20 = float(previous["ema20"])
    sma50 = float(last["sma50"])
    atr14 = float(last["atr"])
    extension_atr = (close - ema20) / atr14 if atr14 else 0.0

    ranges = _true_ranges(rows)
    recent_range = mean(ranges[-5:])
    baseline_range = mean(ranges[-25:-5])
    compression_ratio = recent_range / baseline_range if baseline_range else 1.0

    excess_return_5d = (
        return_5d - benchmark_return_5d if benchmark_return_5d is not None else None
    )
    relative_strength_component = (
        _scale(excess_return_5d, -0.02, 0.05) if excess_return_5d is not None else 0.50
    )
    trend_component = mean([
        1.0 if close > ema20 else 0.0,
        1.0 if ema20 > previous_ema20 else 0.0,
        1.0 if close > sma50 else 0.0,
    ])
    components = {
        "breakoutProximity": _scale(breakout_distance, -0.08, 0.0),
        "relativeStrength": relative_strength_component,
        "trendQuality": trend_component,
        "volumeParticipation": _scale(relative_volume, 0.70, 1.50),
        "rangeCompression": _scale_desc(compression_ratio, 0.75, 1.30),
    }
    price_score = sum(PRICE_WEIGHTS[key] * value for key, value in components.items())
    return {
        "priceScore": round(_clamp(price_score), 6),
        "components": {key: round(value, 6) for key, value in components.items()},
        "facts": {
            "close": round(close, 4),
            "return1dPct": round(return_1d * 100.0, 4),
            "return2dPct": round(return_2d * 100.0, 4),
            "return5dPct": round(return_5d * 100.0, 4),
            "benchmarkReturn5dPct": (
                round(benchmark_return_5d * 100.0, 4)
                if benchmark_return_5d is not None else None
            ),
            "excessReturn5dPct": (
                round(excess_return_5d * 100.0, 4)
                if excess_return_5d is not None else None
            ),
            "priorHigh20": round(prior_high_20, 4),
            "breakoutDistancePct": round(breakout_distance * 100.0, 4),
            "relativeVolume20": round(relative_volume, 4),
            "ema20": round(ema20, 4),
            "sma50": round(sma50, 4),
            "atr14": round(atr14, 4),
            "extensionAtr": round(extension_atr, 4),
            "rangeCompression5v20": round(compression_ratio, 4),
        },
    }


def _scout_trigger(candidate: dict | None) -> tuple[float, dict]:
    if not candidate:
        return 0.0, {}
    components = candidate.get("components") if isinstance(candidate.get("components"), dict) else {}
    trigger = max(
        float(components.get("eventAttention") or 0.0),
        float(components.get("catalystProximity") or 0.0),
    )
    return _clamp(trigger), {
        "runId": candidate.get("runId"),
        "attentionScore": candidate.get("attentionScore"),
        "attentionBand": candidate.get("attentionBand"),
        "whyNow": candidate.get("whyNow"),
        "eventAttention": components.get("eventAttention"),
        "catalystProximity": components.get("catalystProximity"),
    }


def classify_setup(
    price_setup: dict,
    *,
    scout_candidate: dict | None = None,
    previous: dict | None = None,
    benchmark_available: bool = True,
) -> dict | None:
    """Apply the versioned state machine to one completed-bar setup."""
    price_score = float(price_setup["priceScore"])
    facts = dict(price_setup["facts"])
    trigger_score, evidence_context = _scout_trigger(scout_candidate)
    # No stored event/catalyst context is neutral rather than bearish. Context can influence at
    # most 20% of the score, so price structure remains the dominant and independently visible leg.
    context_term = trigger_score if evidence_context else 0.50
    setup_score = _clamp(0.80 * price_score + 0.20 * context_term)
    facts["eventCatalystScore"] = round(trigger_score, 6) if evidence_context else None

    return_1d = facts["return1dPct"] / 100.0
    return_2d = facts["return2dPct"] / 100.0
    extended = (
        return_1d >= EXTENDED_RETURN_1D
        or return_2d >= EXTENDED_RETURN_2D
        or facts["extensionAtr"] >= EXTENDED_ATR
    )
    confirmed = (
        facts["breakoutDistancePct"] >= 0.0
        and facts["relativeVolume20"] >= CONFIRM_VOLUME_MIN
        and (facts["excessReturn5dPct"] or 0.0) > 0.0
        and setup_score >= EMERGING_MIN
    )
    event_led = trigger_score >= EVENT_LED_CONTEXT_MIN and price_score >= EVENT_LED_PRICE_MIN
    emerging = setup_score >= EMERGING_MIN or event_led

    previous_state = str(previous.get("state") or "") if previous else ""
    prior_active = previous_state in ACTIVE_STATES
    close_below_ema = facts["close"] < facts["ema20"]
    invalidated = prior_active and (setup_score < INVALIDATION_SCORE or close_below_ema)

    if extended:
        state = "extended"
    elif confirmed:
        state = "confirmed"
    elif invalidated:
        state = "invalidated"
    elif emerging or prior_active:
        state = "emerging"
    else:
        return None

    risk_flags: list[str] = []
    if not benchmark_available:
        risk_flags.append("benchmark-unavailable")
    if state == "extended":
        risk_flags.append("late-move-no-chase")
        if return_1d >= EXTENDED_RETURN_1D:
            risk_flags.append("one-session-move")
        if return_2d >= EXTENDED_RETURN_2D:
            risk_flags.append("two-session-move")
        if facts["extensionAtr"] >= EXTENDED_ATR:
            risk_flags.append("atr-extension")
    elif state == "invalidated":
        risk_flags.append("setup-invalidated")
    elif facts["relativeVolume20"] < 1.0:
        risk_flags.append("below-average-volume")

    distance = facts["breakoutDistancePct"]
    excess = facts["excessReturn5dPct"]
    if state == "extended":
        late_evidence: list[str] = []
        if return_1d >= EXTENDED_RETURN_1D:
            late_evidence.append(f"a {facts['return1dPct']:.1f}% one-session move")
        if return_2d >= EXTENDED_RETURN_2D:
            late_evidence.append(f"a {facts['return2dPct']:.1f}% two-session move")
        if facts["extensionAtr"] >= EXTENDED_ATR:
            late_evidence.append(f"{facts['extensionAtr']:.1f} ATR above EMA20")
        reason = (
            f"No-chase: the completed bar shows {', '.join(late_evidence)}. This detector found "
            "the move after it became extended, not early."
        )
    elif state == "confirmed":
        reason = (
            f"Price closed {abs(distance):.1f}% beyond the prior 20-session high on "
            f"{facts['relativeVolume20']:.1f}x normal volume, with "
            f"{excess:.1f} percentage points of five-session relative strength."
        )
    elif state == "invalidated":
        reason = (
            f"The prior setup lost evidence: close {facts['close']:.2f} versus EMA20 "
            f"{facts['ema20']:.2f}, with setup evidence {setup_score * 100:.0f}/100."
        )
    else:
        location = (
            f"{abs(distance):.1f}% below" if distance < 0 else f"{distance:.1f}% above"
        )
        relative = (
            f" and five-session relative strength is {excess:.1f} percentage points"
            if excess is not None else ""
        )
        context = str(evidence_context.get("whyNow") or "").strip()
        suffix = f" Stored context: {context}" if context else ""
        reason = (
            f"Evidence is accumulating before extension: price is {location} the prior "
            f"20-session high{relative}, on {facts['relativeVolume20']:.1f}x normal volume."
            f"{suffix}"
        )

    return {
        "state": state,
        "setupScore": round(setup_score, 6),
        "facts": facts,
        "components": {
            **price_setup["components"],
            "priceSetup": round(price_score, 6),
            "eventCatalystContext": round(trigger_score, 6) if evidence_context else None,
        },
        "evidenceContext": evidence_context,
        "reason": reason,
        "riskFlags": risk_flags,
    }


def _latest_prior(conn: Connection, universe: list[str]) -> dict[str, dict]:
    rows = conn.execute(
        "SELECT DISTINCT ON (c.ticker) c.ticker, c.state, c.first_seen_at, "
        "c.state_changed_at, c.bar_time FROM opportunity_candidates c "
        "JOIN opportunity_runs r ON r.id = c.run_id "
        "WHERE r.status = 'success' AND r.detector_version = ? AND c.ticker = ANY(?) "
        "ORDER BY c.ticker, r.as_of DESC, r.sequence DESC",
        (DETECTOR_VERSION, universe),
    ).fetchall()
    return {str(row["ticker"]): dict(row) for row in rows}


def _scout_context(conn: Connection, *, now: datetime) -> tuple[str | None, dict[str, dict]]:
    body = latest_scout(conn, limit=100, now=now)
    run_id = body.get("runId")
    candidates: dict[str, dict] = {}
    for item in body.get("candidates") or []:
        ticker = str(item.get("ticker") or "").upper()
        if ticker:
            candidates[ticker] = {**item, "runId": run_id}
    return run_id, candidates


def _scan(
    universe: list[str],
    *,
    benchmark: str,
    now: datetime,
    feature_fetcher,
) -> tuple[dict, dict[str, dict]]:
    tickers = list(dict.fromkeys([benchmark, *universe]))
    results: dict[str, dict] = {}
    if feature_fetcher is fetch_completed_features:
        with ThreadPoolExecutor(max_workers=min(FETCH_WORKERS, len(tickers))) as pool:
            futures = {
                pool.submit(feature_fetcher, ticker, as_of=now): ticker for ticker in tickers
            }
            for future in as_completed(futures):
                ticker = futures[future]
                try:
                    results[ticker] = future.result()
                except Exception as exc:  # noqa: BLE001 — one ticker cannot abort the bounded pass
                    results[ticker] = {
                        "available": False,
                        "reason": "error",
                        "degraded": f"analysis:{type(exc).__name__}",
                    }
    else:
        for ticker in tickers:
            results[ticker] = feature_fetcher(ticker, as_of=now)

    benchmark_result = results.get(benchmark) or {"available": False, "reason": "missing"}
    benchmark_return = (
        _return_for(benchmark_result["payload"], 5)
        if benchmark_result.get("available") else None
    )
    return {
        "ticker": benchmark,
        "available": benchmark_return is not None,
        "return5d": benchmark_return,
        "barTime": benchmark_result.get("barTime"),
        "source": benchmark_result.get("source"),
        "reason": benchmark_result.get("reason"),
    }, {ticker: results[ticker] for ticker in universe}


def _fingerprint(
    *,
    benchmark: dict,
    results: dict[str, dict],
    scout_run_id: str | None,
) -> str:
    def content_hash(result: dict) -> str | None:
        if not result.get("available"):
            return None
        # A provider correction to an already completed bar must produce a new snapshot even when
        # ticker, source and date are unchanged. Hash the feature rows that drive the detector,
        # not merely the bar label.
        raw_rows = json.dumps(
            _rows(result.get("payload") or {})[-MIN_ROWS:],
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
        return hashlib.sha256(raw_rows).hexdigest()

    material = {
        "benchmark": benchmark,
        "scoutRunId": scout_run_id,
        "tickers": [{
            "ticker": ticker,
            "available": bool(result.get("available")),
            "barTime": result.get("barTime"),
            "source": result.get("source"),
            "reason": result.get("reason"),
            "contentHash": content_hash(result),
        } for ticker, result in sorted(results.items())],
    }
    raw = json.dumps(material, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(raw).hexdigest()


def _existing_report(conn: Connection, row) -> dict:
    return {
        "runId": row["id"],
        "detectorVersion": row["detector_version"],
        "universeVersion": row["universe_version"],
        "asOf": row["as_of"],
        "candidateCount": int(row["candidate_count"]),
        "persistedCandidateCount": 0,
        "coverage": _json(row["coverage"], {}),
        "degraded": _json(row["degraded"], []),
        "skipped": True,
        "skipReason": "unchanged-completed-bar-fingerprint",
    }


def run_opportunity_radar(
    conn: Connection | None = None,
    *,
    now: datetime | None = None,
    universe_=None,
    feature_fetcher=fetch_completed_features,
) -> dict:
    """Run one bounded scan and persist a snapshot only when its source fingerprint changed."""
    owns = conn is None
    conn = conn or connect()
    moment = now or _now()
    started = _iso(moment)
    universe = scout_universe(universe_)
    benchmark = benchmark_ticker()
    run_id = RUN_PREFIX + secrets.token_hex(12)

    try:
        scout_run_id, scout_candidates = _scout_context(conn, now=moment)
        benchmark_result, results = _scan(
            universe, benchmark=benchmark, now=moment, feature_fetcher=feature_fetcher,
        )
        fingerprint = _fingerprint(
            benchmark=benchmark_result, results=results, scout_run_id=scout_run_id,
        )
        existing = conn.execute(
            "SELECT * FROM opportunity_runs WHERE detector_version = ? "
            "AND universe_version = ? AND data_fingerprint = ? AND status = 'success' "
            "ORDER BY sequence DESC LIMIT 1",
            (DETECTOR_VERSION, UNIVERSE_VERSION, fingerprint),
        ).fetchone()
        if existing is not None:
            conn.commit()
            return _existing_report(conn, existing)

        prior = _latest_prior(conn, universe)
        covered = [ticker for ticker, result in results.items() if result.get("available")]
        benchmark_unaligned: list[str] = []
        missing = [{
            "ticker": ticker,
            "reason": str(result.get("reason") or "unknown"),
        } for ticker, result in results.items() if not result.get("available")]
        degraded = sorted({
            str(result.get("degraded")) for result in results.values() if result.get("degraded")
        })
        if not benchmark_result["available"]:
            degraded.append("benchmark:unavailable")

        candidates: list[dict] = []
        for ticker in universe:
            result = results[ticker]
            if not result.get("available"):
                continue
            try:
                benchmark_aligned = bool(
                    benchmark_result["available"]
                    and benchmark_result.get("barTime") == result.get("barTime")
                )
                if benchmark_result["available"] and not benchmark_aligned:
                    benchmark_unaligned.append(ticker)
                price_setup = compute_price_setup(
                    result["payload"],
                    benchmark_return_5d=(benchmark_result["return5d"] if benchmark_aligned else None),
                )
                candidate = classify_setup(
                    price_setup,
                    scout_candidate=scout_candidates.get(ticker),
                    previous=prior.get(ticker),
                    benchmark_available=benchmark_aligned,
                )
            except (KeyError, TypeError, ValueError, ZeroDivisionError):
                missing.append({"ticker": ticker, "reason": "malformed-features"})
                continue
            if candidate is None:
                continue
            previous = prior.get(ticker)
            previous_state = str(previous.get("state") or "") if previous else ""
            reset_lifecycle = not previous or previous_state == "invalidated"
            first_seen_at = started if reset_lifecycle else previous["first_seen_at"]
            state_changed_at = (
                previous["state_changed_at"]
                if previous and previous_state == candidate["state"] else started
            )
            candidates.append({
                "ticker": ticker,
                "previousState": previous_state or None,
                "barTime": result["barTime"],
                "source": result["source"],
                "firstSeenAt": first_seen_at,
                "stateChangedAt": state_changed_at,
                **candidate,
            })

        candidates.sort(key=lambda item: (
            STATE_ORDER[item["state"]], -float(item["setupScore"]), item["ticker"],
        ))
        for rank, candidate in enumerate(candidates, start=1):
            candidate["rank"] = rank

        coverage = {
            "state": (
                "ok" if len(covered) == len(universe)
                else "partial" if covered else "insufficient"
            ),
            "universeSize": len(universe),
            "marketDataCovered": len(covered),
            "marketDataMissing": missing,
            "completedOnly": True,
            "timeframe": "1D",
            "benchmark": benchmark_result,
            "benchmarkUnaligned": benchmark_unaligned,
            "scoutRunId": scout_run_id,
            "predictionSignal": "excluded",
            "modelCall": "excluded",
            "paperEligibility": "not-assessed",
        }

        inserted = conn.execute(
            "INSERT INTO opportunity_runs (id,detector_version,universe_version,universe,"
            "benchmark,data_fingerprint,as_of,started_at,status,coverage,degraded) "
            "VALUES (?,?,?,?,?,?,?,?, 'running', ?, ?) ON CONFLICT "
            "(detector_version,universe_version,data_fingerprint) DO NOTHING RETURNING id",
            (run_id, DETECTOR_VERSION, UNIVERSE_VERSION, json.dumps(universe), benchmark,
             fingerprint, started, started, json.dumps(coverage), json.dumps(degraded)),
        ).fetchone()
        if inserted is None:
            conn.rollback()
            raced = conn.execute(
                "SELECT * FROM opportunity_runs WHERE detector_version = ? "
                "AND universe_version = ? AND data_fingerprint = ? ORDER BY sequence DESC LIMIT 1",
                (DETECTOR_VERSION, UNIVERSE_VERSION, fingerprint),
            ).fetchone()
            conn.commit()
            return _existing_report(conn, raced)

        for candidate in candidates:
            conn.execute(
                "INSERT INTO opportunity_candidates (run_id,ticker,rank,state,previous_state,"
                "setup_score,bar_time,first_seen_at,state_changed_at,source,facts,components,"
                "evidence_context,reason,risk_flags) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (run_id, candidate["ticker"], candidate["rank"], candidate["state"],
                 candidate["previousState"], candidate["setupScore"], candidate["barTime"],
                 candidate["firstSeenAt"], candidate["stateChangedAt"], candidate["source"],
                 json.dumps(candidate["facts"]), json.dumps(candidate["components"]),
                 json.dumps(candidate["evidenceContext"]), candidate["reason"],
                 json.dumps(candidate["riskFlags"])),
            )
        conn.execute(
            "UPDATE opportunity_runs SET completed_at = ?, status = 'success', "
            "candidate_count = ?, coverage = ?, degraded = ? WHERE id = ?",
            (started, len(candidates), json.dumps(coverage), json.dumps(degraded), run_id),
        )
        conn.commit()
        return {
            "runId": run_id,
            "detectorVersion": DETECTOR_VERSION,
            "universeVersion": UNIVERSE_VERSION,
            "asOf": started,
            "candidateCount": len(candidates),
            "persistedCandidateCount": len(candidates),
            "coverage": coverage,
            "degraded": degraded,
            "skipped": False,
        }
    except Exception as exc:  # noqa: BLE001 — persist the domain run when it exists, then fail
        conn.rollback()
        try:
            conn.execute(
                "UPDATE opportunity_runs SET completed_at = ?, status = 'failure', error = ? "
                "WHERE id = ?",
                (started, f"{type(exc).__name__}: {exc}"[:ERROR_CAP], run_id),
            )
            conn.commit()
        except Exception:  # noqa: BLE001 — preserve the original failure
            conn.rollback()
        raise
    finally:
        if owns:
            conn.close()


def latest_opportunities(
    conn: Connection,
    *,
    limit: int = 20,
    ticker: str = "",
    state: str = "",
    now: datetime | None = None,
) -> dict:
    limit = max(1, min(int(limit), MAX_LIMIT))
    run = conn.execute(
        "SELECT * FROM opportunity_runs WHERE status = 'success' "
        "AND detector_version = ? AND universe_version = ? "
        "ORDER BY as_of DESC, sequence DESC LIMIT 1",
        (DETECTOR_VERSION, UNIVERSE_VERSION),
    ).fetchone()
    if run is None:
        return {
            "runId": None,
            "detectorVersion": DETECTOR_VERSION,
            "universeVersion": UNIVERSE_VERSION,
            "asOf": None,
            "coverage": {"state": "insufficient", "reason": "no-runs"},
            "candidates": [],
            "degraded": ["opportunities:no-runs"],
            "paperEligibility": PAPER_ELIGIBILITY,
            "disclaimer": DISCLAIMER,
        }

    coverage = _json(run["coverage"], {})
    degraded = _json(run["degraded"], [])
    as_of = _parse(run["as_of"])
    max_age = _positive_int(MAX_RUN_AGE_SECONDS_ENV, DEFAULT_MAX_RUN_AGE_SECONDS)
    if as_of is None or (now or _now()) - as_of > timedelta(seconds=max_age):
        return {
            "runId": run["id"],
            "detectorVersion": run["detector_version"],
            "universeVersion": run["universe_version"],
            "asOf": run["as_of"],
            "coverage": {**coverage, "state": "stale", "reason": "snapshot-too-old"},
            "candidates": [],
            "degraded": list(dict.fromkeys([*degraded, "opportunities:stale"])),
            "paperEligibility": PAPER_ELIGIBILITY,
            "disclaimer": DISCLAIMER,
        }

    where = ["run_id = ?"]
    params: list = [run["id"]]
    symbol = str(ticker or "").strip().upper()
    state_filter = str(state or "").strip().lower()
    if symbol:
        where.append("ticker = ?")
        params.append(symbol)
    if state_filter in STATE_ORDER:
        where.append("state = ?")
        params.append(state_filter)
    params.append(limit)
    rows = conn.execute(
        f"SELECT * FROM opportunity_candidates WHERE {' AND '.join(where)} "
        "ORDER BY rank LIMIT ?", params,
    ).fetchall()
    candidates = [{
        "ticker": row["ticker"],
        "rank": int(row["rank"]),
        "state": row["state"],
        "previousState": row["previous_state"],
        "setupScore": float(row["setup_score"]),
        "barTime": row["bar_time"],
        "firstSeenAt": row["first_seen_at"],
        "stateChangedAt": row["state_changed_at"],
        "source": row["source"],
        "facts": _json(row["facts"], {}),
        "components": _json(row["components"], {}),
        "evidenceContext": _json(row["evidence_context"], {}),
        "reason": row["reason"],
        "riskFlags": _json(row["risk_flags"], []),
        "paperEligibility": PAPER_ELIGIBILITY,
        "disclaimer": DISCLAIMER,
    } for row in rows]
    return {
        "runId": run["id"],
        "detectorVersion": run["detector_version"],
        "universeVersion": run["universe_version"],
        "asOf": run["as_of"],
        "coverage": coverage,
        "candidates": candidates,
        "degraded": degraded,
        "paperEligibility": PAPER_ELIGIBILITY,
        "disclaimer": DISCLAIMER,
    }


@router.get("/opportunities")
def get_opportunities(
    limit: int = Query(default=20, ge=1, le=MAX_LIMIT),
    ticker: str = Query(default=""),
    state: str = Query(default=""),
) -> dict:
    """Latest stored snapshot only. This GET cannot fetch market data or invoke a model."""
    conn = connect()
    try:
        return latest_opportunities(conn, limit=limit, ticker=ticker, state=state)
    finally:
        conn.close()


def main() -> int:
    print(json.dumps(run_opportunity_radar(), indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
