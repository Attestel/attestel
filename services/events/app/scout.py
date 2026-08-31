"""Deterministic company-level discovery, persisted as auditable research leads.

Scout answers one bounded question: which covered companies deserve a research file opened now,
and which stored facts caused them to surface?  It never answers what to buy.  Ranking uses
canonical events, scheduled catalysts, and point-in-time market context from the analysis service;
there is no prediction response, model call, target, expected return, or order seam here.

The production scheduler owns two one-shot entrypoints:

* ``run_intake`` rotates a small batch through a closed, keyless provider allowlist.  It reuses the
  normal ingestion path, including its PostgreSQL budget reservations and provenance rules.
* ``run_scout`` reads the resulting store, asks analysis only for a historical cutoff (which makes
  the analysis service store-only), writes one immutable ranked snapshot, and returns.

Both functions are bounded and return.  Repetition, leases, and due-state belong to
``app.automation``; Qwen remains behind explicit user actions in the gateway.
"""
from __future__ import annotations

import json
import math
import os
import secrets
from datetime import datetime, timedelta, timezone
from urllib.parse import quote

import requests
from fastapi import APIRouter, Query

from .db import Connection, connect
from .providers import COMPANY_IR, GOOGLE_NEWS_RSS, SEC_EDGAR

router = APIRouter()

SCORE_VERSION = "scout@2"
UNIVERSE_VERSION = "scout-universe@1"

DEFAULT_UNIVERSE = (
    "NVDA", "GOOGL", "TSLA", "AAPL", "MSFT", "AMZN", "META", "AMD", "AVGO", "NFLX",
    "JPM", "V", "MA", "COST", "PEP", "KO", "XOM", "CVX", "UNH", "JNJ", "WMT", "HD",
    "PG", "DIS", "BA", "CAT", "CRM", "ORCL", "ADBE", "QCOM",
)

SCOUT_UNIVERSE_ENV = "SCOUT_UNIVERSE"
SCOUT_INTAKE_PROVIDERS_ENV = "SCOUT_INTAKE_PROVIDERS"
SCOUT_INTAKE_BATCH_SIZE_ENV = "SCOUT_INTAKE_BATCH_SIZE"
SCOUT_INTAKE_SLOT_SECONDS_ENV = "SCOUT_INTAKE_SLOT_SECONDS"
SCOUT_MAX_BAR_AGE_DAYS_ENV = "SCOUT_MAX_BAR_AGE_DAYS"
SCOUT_MAX_RUN_AGE_SECONDS_ENV = "SCOUT_MAX_RUN_AGE_SECONDS"
ANALYSIS_URL_ENV = "ANALYSIS_URL"

DEFAULT_INTAKE_BATCH_SIZE = 5
DEFAULT_INTAKE_SLOT_SECONDS = 4 * 60 * 60
DEFAULT_MAX_BAR_AGE_DAYS = 7
DEFAULT_MAX_RUN_AGE_SECONDS = 12 * 60 * 60
DEFAULT_ANALYSIS_URL = "http://localhost:8001"
TECHNICAL_TIMEOUT_SECONDS = 5

# Paid/quota-constrained Marketaux and Alpha Vantage are intentionally absent.  An operator may
# narrow this set, never widen it through an environment typo.
SCOUT_INTAKE_ALLOWED = (GOOGLE_NEWS_RSS, SEC_EDGAR, COMPANY_IR)
SCOUT_INTAKE_DEFAULT = (GOOGLE_NEWS_RSS,)

EVENT_WINDOW_DAYS = 14
CATALYST_WINDOW_DAYS = 30
FRESHNESS_HALF_LIFE_HOURS = 36.0
TECHNICAL_ELIGIBILITY_MIN = 0.65

# A score is an ordering device, not a probability.  These sum to 1.0 and are frozen by
# SCORE_VERSION; changing one requires a version bump.
W_EVENT = 0.35
W_CATALYST = 0.25
W_TECHNICAL = 0.20
W_BREADTH = 0.10
W_SOURCE = 0.10
assert math.isclose(W_EVENT + W_CATALYST + W_TECHNICAL + W_BREADTH + W_SOURCE, 1.0)

TIER_WEIGHT = {"official": 1.0, "professional": 0.6, "discussion": 0.25}
CATALYST_IMPORTANCE = {"high": 1.0, "medium": 0.65, "low": 0.35}
RELATIONSHIP_WEIGHT = {
    "direct": 1.0, "supplier": 0.75, "customer": 0.75, "competitor": 0.65, "sector": 0.45,
}

RUN_PREFIX = "sct_"
MAX_LIMIT = 100
ERROR_CAP = 500


def _now() -> datetime:
    return datetime.now(timezone.utc)


def _iso(value: datetime) -> str:
    return value.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _parse(value: str | None) -> datetime | None:
    if not value:
        return None
    text = str(value).strip()
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


def _clamp(value) -> float:
    try:
        number = float(value)
    except (TypeError, ValueError):
        return 0.0
    if not math.isfinite(number):
        return 0.0
    return max(0.0, min(1.0, number))


def _positive_int(name: str, default: int) -> int:
    try:
        value = int(os.getenv(name, "").strip() or default)
    except ValueError:
        return default
    return value if value > 0 else default


def scout_universe(explicit=None) -> list[str]:
    raw = explicit
    if raw is None:
        raw = os.getenv(SCOUT_UNIVERSE_ENV, "").split(",") if os.getenv(
            SCOUT_UNIVERSE_ENV, ""
        ).strip() else DEFAULT_UNIVERSE
    out: list[str] = []
    seen: set[str] = set()
    for item in raw:
        ticker = str(item or "").strip().upper()
        if ticker and ticker not in seen:
            seen.add(ticker)
            out.append(ticker)
    return out or list(DEFAULT_UNIVERSE)


def intake_providers() -> list[str]:
    raw = os.getenv(SCOUT_INTAKE_PROVIDERS_ENV, "").strip()
    requested = [p.strip() for p in raw.split(",") if p.strip()] if raw else list(
        SCOUT_INTAKE_DEFAULT
    )
    selected = [p for p in requested if p in SCOUT_INTAKE_ALLOWED]
    return selected or list(SCOUT_INTAKE_DEFAULT)


def intake_batch(*, now: datetime | None = None, universe_=None) -> list[str]:
    """Stable rotating slice; six default slots cover the thirty-name universe once per day."""
    universe = scout_universe(universe_)
    size = min(len(universe), _positive_int(
        SCOUT_INTAKE_BATCH_SIZE_ENV, DEFAULT_INTAKE_BATCH_SIZE
    ))
    seconds = _positive_int(SCOUT_INTAKE_SLOT_SECONDS_ENV, DEFAULT_INTAKE_SLOT_SECONDS)
    slot = int((now or _now()).timestamp()) // seconds
    start = (slot * size) % len(universe)
    return [universe[(start + offset) % len(universe)] for offset in range(size)]


def run_intake(conn: Connection, *, now: datetime | None = None) -> dict:
    """One budgeted discovery intake batch through the existing ingestion contract."""
    from . import ingest

    moment = now or _now()
    batch = intake_batch(now=moment)
    report = ingest.run_ingest(
        conn, providers=intake_providers(), tickers_=batch, now=moment,
    )
    report["scoutUniverseVersion"] = UNIVERSE_VERSION
    report["scoutBatch"] = batch
    return report


def _freshness(published: datetime | None, now: datetime) -> float:
    if published is None:
        return 0.0
    age_hours = max(0.0, (now - published).total_seconds() / 3600.0)
    return _clamp(math.exp(-math.log(2) * age_hours / FRESHNESS_HALF_LIFE_HOURS))


def _technical_salience(payload: dict) -> tuple[float, dict]:
    price = payload.get("price") if isinstance(payload.get("price"), dict) else {}
    indicators = payload.get("indicators") if isinstance(payload.get("indicators"), dict) else {}
    latest = indicators.get("latest") if isinstance(indicators.get("latest"), dict) else {}

    rsi = latest.get("rsi")
    change = price.get("changePct")
    close = price.get("lastClose")
    ema20 = latest.get("ema20")
    adx = latest.get("adx")

    rsi_term = _clamp(abs(float(rsi) - 50.0) / 30.0) if rsi is not None else 0.0
    move_term = _clamp(abs(float(change)) / 5.0) if change is not None else 0.0
    ema_term = 0.0
    if close is not None and ema20 not in (None, 0):
        ema_term = _clamp(abs(float(close) / float(ema20) - 1.0) / 0.08)
    adx_term = _clamp((float(adx) - 20.0) / 30.0) if adx is not None else 0.0

    dislocation = max(rsi_term, move_term, ema_term)
    score = _clamp(0.75 * dislocation + 0.25 * adx_term)
    facts = {
        "rsi": rsi,
        "changePct": change,
        "lastClose": close,
        "ema20": ema20,
        "adx": adx,
    }
    return score, facts


def fetch_technical(ticker: str, *, as_of: datetime) -> dict:
    """Stored-bar technical context. Supplying a past cutoff prevents an upstream provider fetch."""
    cutoff = as_of - timedelta(seconds=1)
    base = os.getenv(ANALYSIS_URL_ENV, DEFAULT_ANALYSIS_URL).rstrip("/")
    url = f"{base}/analysis/{quote(ticker)}"
    try:
        response = requests.get(
            url,
            params={"timeframe": "1D", "n": 260, "as_of": _iso(cutoff)},
            timeout=TECHNICAL_TIMEOUT_SECONDS,
        )
    except requests.RequestException:
        return {"available": False, "reason": "unreachable", "degraded": "technical:unreachable"}
    if response.status_code in (404, 409, 502):
        return {"available": False, "reason": "insufficient"}
    if not response.ok:
        return {"available": False, "reason": "error", "degraded": "technical:error"}
    try:
        payload = response.json()
    except (TypeError, ValueError):
        return {"available": False, "reason": "error", "degraded": "technical:error"}
    if not isinstance(payload, dict) or payload.get("sourceIsSynthetic"):
        return {"available": False, "reason": "synthetic"}

    price = payload.get("price") if isinstance(payload.get("price"), dict) else {}
    data_at = _parse(price.get("asOf"))
    max_age = _positive_int(SCOUT_MAX_BAR_AGE_DAYS_ENV, DEFAULT_MAX_BAR_AGE_DAYS)
    if data_at is None or as_of - data_at > timedelta(days=max_age):
        return {"available": False, "reason": "stale"}

    score, facts = _technical_salience(payload)
    pieces = []
    if facts.get("rsi") is not None:
        pieces.append(f"RSI {float(facts['rsi']):.1f}")
    if facts.get("changePct") is not None:
        pieces.append(f"an absolute daily move of {abs(float(facts['changePct'])):.1f}%")
    summary = "Current completed daily bars show " + " and ".join(pieces[:2]) + "."
    if not pieces:
        summary = "Current completed daily bars show an unusual technical state."
    return {
        "available": True,
        "score": score,
        "at": _iso(data_at),
        "source": str(payload.get("source") or "unknown"),
        "facts": facts,
        "summary": summary,
    }


def _event_rows(conn: Connection, *, universe: list[str], now: datetime) -> list:
    return conn.execute(
        "SELECT e.id, e.event_type, e.title, e.published_at, e.source_tier, e.importance, "
        "e.novelty, e.document_count, t.ticker, t.relevance, t.is_primary, "
        "COALESCE((SELECT jsonb_agg(p.provider ORDER BY p.provider) FROM ("
        "  SELECT DISTINCT ed.provider FROM event_documents ed WHERE ed.event_id = e.id"
        ") p), '[]'::jsonb) AS providers, "
        "COALESCE((SELECT jsonb_agg(t2.ticker ORDER BY t2.ticker) FROM event_tickers t2 "
        "  WHERE t2.event_id = e.id), '[]'::jsonb) AS related "
        "FROM events e JOIN event_tickers t ON t.event_id = e.id "
        "WHERE t.ticker = ANY(?) AND e.published_at >= ? AND e.published_at <= ? "
        "AND e.first_seen_at <= ? ORDER BY e.published_at DESC, e.id DESC, t.ticker",
        (universe, _iso(now - timedelta(days=EVENT_WINDOW_DAYS)), _iso(now), _iso(now)),
    ).fetchall()


def _catalyst_rows(conn: Connection, *, universe: list[str], now: datetime) -> list:
    return conn.execute(
        "SELECT s.id, s.kind, s.ticker AS subject, s.title, s.scheduled_at, s.confirmed, "
        "s.importance, s.source, s.source_tier, COALESCE(r.ticker, s.ticker) AS ticker, "
        "COALESCE(r.relationship, 'direct') AS relationship, "
        "COALESCE(r.reason, '') AS relationship_reason "
        "FROM scheduled_events s LEFT JOIN event_relationships r ON r.event_id = s.id "
        "WHERE COALESCE(r.ticker, s.ticker) = ANY(?) AND s.first_seen_at <= ? "
        "AND s.scheduled_at >= ? AND s.scheduled_at <= ? "
        "AND s.status NOT IN ('cancelled','released') "
        "AND COALESCE(r.relationship, 'direct') NOT IN ('macro','factor') "
        "ORDER BY s.scheduled_at, s.id, COALESCE(r.ticker, s.ticker)",
        (universe, _iso(now), _iso(now), _iso(now + timedelta(days=CATALYST_WINDOW_DAYS))),
    ).fetchall()


def _event_attention(row, now: datetime) -> float:
    return _clamp(
        0.40 * _clamp(row["importance"])
        + 0.25 * _clamp(row["novelty"])
        + 0.20 * TIER_WEIGHT.get(row["source_tier"], 0.0)
        + 0.15 * _freshness(_parse(row["published_at"]), now)
    )


def _catalyst_attention(row, now: datetime) -> float:
    scheduled = _parse(row["scheduled_at"])
    if scheduled is None:
        return 0.0
    days = max(0.0, (scheduled - now).total_seconds() / 86400.0)
    proximity = 1.0 if days <= 7 else 0.70 if days <= 14 else 0.40
    confirmation = 1.0 if bool(row["confirmed"]) else 0.70
    return _clamp(
        CATALYST_IMPORTANCE.get(str(row["importance"] or "").lower(), 0.35)
        * proximity
        * confirmation
        * RELATIONSHIP_WEIGHT.get(row["relationship"], 0.0)
    )


def _empty_candidate(ticker: str) -> dict:
    return {
        "ticker": ticker,
        "events": [],
        "catalysts": [],
        "technical": None,
        "providers": set(),
        "tiers": set(),
        "related": set(),
    }


def _why_now(candidate: dict, now: datetime) -> str:
    choices: list[tuple[float, str]] = []
    if candidate["events"]:
        event = max(candidate["events"], key=lambda item: (item["score"], item["at"], item["id"]))
        label = event["eventType"].replace("_", " ")
        choices.append((W_EVENT * event["score"], f"Fresh {label} evidence: {event['title']}."))
    if candidate["catalysts"]:
        catalyst = max(
            candidate["catalysts"], key=lambda item: (item["score"], item["at"], item["id"]),
        )
        when = _parse(catalyst["at"])
        days = max(0, math.ceil((when - now).total_seconds() / 86400.0)) if when else 0
        choices.append((
            W_CATALYST * catalyst["score"],
            f"A {catalyst['importance']} {catalyst['kindName'].replace('_', ' ')} catalyst is "
            f"scheduled in {days} days: {catalyst['title']}.",
        ))
    technical = candidate.get("technical")
    if technical:
        choices.append((W_TECHNICAL * technical["score"], technical["summary"]))
    choices.sort(key=lambda item: (item[0], item[1]), reverse=True)
    return choices[0][1] if choices else ""


def build_candidates(event_rows, catalyst_rows, technical: dict[str, dict], *, now: datetime) -> list[dict]:
    """Pure ranking half: fixed inputs and clock produce a byte-stable candidate order."""
    grouped: dict[str, dict] = {}

    for row in event_rows:
        ticker = str(row["ticker"] or "").upper()
        if not ticker:
            continue
        candidate = grouped.setdefault(ticker, _empty_candidate(ticker))
        providers = _json(row["providers"], [])
        candidate["providers"].update(str(p) for p in providers if p)
        candidate["tiers"].add(str(row["source_tier"] or ""))
        candidate["related"].update(
            str(t).upper() for t in _json(row["related"], []) if str(t).upper() != ticker
        )
        candidate["events"].append({
            "kind": "canonical_event",
            "id": row["id"],
            "title": row["title"],
            "eventType": row["event_type"],
            "at": row["published_at"],
            "sourceTier": row["source_tier"],
            "importance": round(float(row["importance"]), 6),
            "novelty": round(float(row["novelty"]), 6),
            "score": _event_attention(row, now),
        })

    for row in catalyst_rows:
        ticker = str(row["ticker"] or "").upper()
        if not ticker:
            continue
        candidate = grouped.setdefault(ticker, _empty_candidate(ticker))
        candidate["providers"].add(str(row["source"] or ""))
        candidate["tiers"].add(str(row["source_tier"] or ""))
        subject = str(row["subject"] or "").upper()
        if subject and subject != ticker:
            candidate["related"].add(subject)
        candidate["catalysts"].append({
            "kind": "scheduled_catalyst",
            "id": row["id"],
            "title": row["title"],
            "at": row["scheduled_at"],
            "source": row["source"],
            "sourceTier": row["source_tier"],
            "importance": str(row["importance"] or "medium").lower(),
            "relationship": row["relationship"],
            "relationshipReason": row["relationship_reason"],
            "score": _catalyst_attention(row, now),
            "kindName": row["kind"],
        })

    for ticker, context in technical.items():
        if not context.get("available"):
            continue
        if ticker not in grouped and float(context.get("score") or 0) < TECHNICAL_ELIGIBILITY_MIN:
            continue
        candidate = grouped.setdefault(ticker, _empty_candidate(ticker))
        candidate["providers"].add(str(context.get("source") or ""))
        candidate["technical"] = {
            "kind": "technical_state",
            "id": f"{ticker}:1D:{context['at']}",
            "title": context["summary"],
            "at": context["at"],
            "source": context["source"],
            "sourceTier": "market_data",
            "facts": context.get("facts") or {},
            "score": _clamp(context.get("score")),
            "summary": context["summary"],
        }

    ranked: list[dict] = []
    for ticker, candidate in grouped.items():
        events = candidate["events"]
        catalysts = candidate["catalysts"]
        tech = candidate["technical"]
        event_term = max((item["score"] for item in events), default=0.0)
        catalyst_term = max((item["score"] for item in catalysts), default=0.0)
        technical_term = float(tech["score"]) if tech else 0.0
        independent_items = len(events) + len(catalysts)
        breadth_term = _clamp(
            0.5 * min(1.0, independent_items / 3.0)
            + 0.5 * min(1.0, len(candidate["providers"]) / 3.0)
        )
        source_term = max((TIER_WEIGHT.get(tier, 0.0) for tier in candidate["tiers"]), default=0.0)
        components = {
            "eventAttention": round(event_term, 6),
            "catalystProximity": round(catalyst_term, 6),
            "technicalSalience": round(technical_term, 6),
            "evidenceBreadth": round(breadth_term, 6),
            "sourceQuality": round(source_term, 6),
        }
        score = _clamp(
            W_EVENT * event_term + W_CATALYST * catalyst_term + W_TECHNICAL * technical_term
            + W_BREADTH * breadth_term + W_SOURCE * source_term
        )
        evidence = [*events, *catalysts]
        if tech:
            evidence.append(tech)
        evidence.sort(key=lambda item: (item["at"], item["id"]), reverse=True)
        latest = max((stamp for stamp in [_parse(item["at"]) for item in evidence] if stamp), default=now)
        why = _why_now(candidate, now)
        if not why:
            continue
        band = "high_attention" if score >= 0.65 else "monitor" if score >= 0.40 else "emerging"
        ranked.append({
            "ticker": ticker,
            "attentionScore": round(score, 6),
            "attentionBand": band,
            "components": components,
            "whyNow": why,
            "evidence": evidence[:8],
            "relatedTickers": sorted(candidate["related"]),
            "latestEvidenceAt": _iso(latest),
            "sourceTiers": sorted(t for t in candidate["tiers"] if t),
            "dataState": "live",
        })

    ranked.sort(key=lambda item: (
        -item["attentionScore"], -(_parse(item["latestEvidenceAt"]) or now).timestamp(), item["ticker"],
    ))
    for index, item in enumerate(ranked, 1):
        item["rank"] = index
    return ranked


def run_scout(
    conn: Connection | None = None,
    *,
    now: datetime | None = None,
    universe_=None,
    technical_fetcher=fetch_technical,
) -> dict:
    """Materialize one immutable Scout snapshot. A failed run remains visible in ``scout_runs``."""
    moment = now or _now()
    universe = scout_universe(universe_)
    run_id = RUN_PREFIX + secrets.token_hex(8)
    owns = conn is None
    conn = conn or connect()
    started = _iso(moment)
    conn.execute(
        "INSERT INTO scout_runs (id, score_version, universe_version, universe, as_of, started_at) "
        "VALUES (?,?,?,?,?,?)",
        (run_id, SCORE_VERSION, UNIVERSE_VERSION, json.dumps(universe), started, started),
    )
    conn.commit()

    try:
        event_rows = _event_rows(conn, universe=universe, now=moment)
        catalyst_rows = _catalyst_rows(conn, universe=universe, now=moment)

        technical: dict[str, dict] = {}
        degraded: list[str] = []
        technical_missing: list[str] = []
        for ticker in universe:
            context = technical_fetcher(ticker, as_of=moment)
            technical[ticker] = context
            if not context.get("available"):
                technical_missing.append(ticker)
            marker = str(context.get("degraded") or "")
            if marker and marker not in degraded:
                degraded.append(marker)

        candidates = build_candidates(event_rows, catalyst_rows, technical, now=moment)
        coverage = {
            "state": "ok" if candidates else "insufficient",
            "universeSize": len(universe),
            "eligibleTickers": len(candidates),
            "canonicalEventRows": len(event_rows),
            "scheduledCatalystRows": len(catalyst_rows),
            "technicalCovered": len(universe) - len(technical_missing),
            "technicalMissing": technical_missing,
            "predictionSignal": "excluded",
            "modelRanking": "excluded",
        }
        for candidate in candidates:
            conn.execute(
                "INSERT INTO scout_candidates (run_id, ticker, rank, attention_score, "
                "attention_band, components, why_now, evidence, related_tickers, "
                "latest_evidence_at, source_tiers, data_state) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
                (
                    run_id, candidate["ticker"], candidate["rank"], candidate["attentionScore"],
                    candidate["attentionBand"], json.dumps(candidate["components"]),
                    candidate["whyNow"], json.dumps(candidate["evidence"]),
                    json.dumps(candidate["relatedTickers"]), candidate["latestEvidenceAt"],
                    json.dumps(candidate["sourceTiers"]), candidate["dataState"],
                ),
            )
        conn.execute(
            "UPDATE scout_runs SET completed_at = ?, status = 'success', candidate_count = ?, "
            "coverage = ?, degraded = ? WHERE id = ?",
            (_iso(moment), len(candidates), json.dumps(coverage), json.dumps(degraded), run_id),
        )
        conn.commit()
        return {
            "runId": run_id,
            "scoreVersion": SCORE_VERSION,
            "universeVersion": UNIVERSE_VERSION,
            "asOf": started,
            "candidateCount": len(candidates),
            "coverage": coverage,
            "degraded": degraded,
        }
    except Exception as exc:  # noqa: BLE001 — record the domain run before automation records its run
        conn.rollback()
        conn.execute(
            "UPDATE scout_runs SET completed_at = ?, status = 'failure', error = ? WHERE id = ?",
            (_iso(moment), f"{type(exc).__name__}: {exc}"[:ERROR_CAP], run_id),
        )
        conn.commit()
        raise
    finally:
        if owns:
            conn.close()


def latest_scout(
    conn: Connection,
    *,
    limit: int = 20,
    ticker: str = "",
    now: datetime | None = None,
) -> dict:
    limit = max(1, min(int(limit), MAX_LIMIT))
    run = conn.execute(
        "SELECT * FROM scout_runs WHERE status = 'success' "
        "AND score_version = ? AND universe_version = ? "
        "ORDER BY as_of DESC, sequence DESC LIMIT 1",
        (SCORE_VERSION, UNIVERSE_VERSION),
    ).fetchone()
    if run is None:
        return {
            "runId": None,
            "scoreVersion": SCORE_VERSION,
            "universeVersion": UNIVERSE_VERSION,
            "asOf": None,
            "coverage": {"state": "insufficient", "reason": "no-runs"},
            "candidates": [],
            "degraded": ["scout:no-runs"],
        }

    coverage = _json(run["coverage"], {})
    degraded = _json(run["degraded"], [])
    as_of = _parse(run["as_of"])
    max_age = _positive_int(SCOUT_MAX_RUN_AGE_SECONDS_ENV, DEFAULT_MAX_RUN_AGE_SECONDS)
    if as_of is None or (now or _now()) - as_of > timedelta(seconds=max_age):
        coverage = {**coverage, "state": "stale", "reason": "snapshot-too-old"}
        return {
            "runId": run["id"],
            "scoreVersion": run["score_version"],
            "universeVersion": run["universe_version"],
            "asOf": run["as_of"],
            "coverage": coverage,
            "candidates": [],
            "degraded": list(dict.fromkeys([*degraded, "scout:stale"])),
        }

    params: list = [run["id"]]
    where = "run_id = ?"
    symbol = str(ticker or "").strip().upper()
    if symbol:
        where += " AND ticker = ?"
        params.append(symbol)
    params.append(limit)
    rows = conn.execute(
        f"SELECT * FROM scout_candidates WHERE {where} ORDER BY rank LIMIT ?", params,
    ).fetchall()
    candidates = [{
        "ticker": row["ticker"],
        "rank": row["rank"],
        "attentionScore": row["attention_score"],
        "attentionBand": row["attention_band"],
        "components": _json(row["components"], {}),
        "whyNow": row["why_now"],
        "evidence": _json(row["evidence"], []),
        "relatedTickers": _json(row["related_tickers"], []),
        "latestEvidenceAt": row["latest_evidence_at"],
        "sourceTiers": _json(row["source_tiers"], []),
        "dataState": row["data_state"],
    } for row in rows]
    return {
        "runId": run["id"],
        "scoreVersion": run["score_version"],
        "universeVersion": run["universe_version"],
        "asOf": run["as_of"],
        "coverage": coverage,
        "candidates": candidates,
        "degraded": degraded,
    }


@router.get("/scout")
def get_scout(
    limit: int = Query(default=20, ge=1, le=MAX_LIMIT),
    ticker: str = Query(default=""),
) -> dict:
    """Latest stored snapshot only. This GET cannot fetch a provider or invoke a model."""
    conn = connect()
    try:
        return latest_scout(conn, limit=limit, ticker=ticker)
    finally:
        conn.close()


def main() -> int:
    report = run_scout()
    print(json.dumps(report, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
