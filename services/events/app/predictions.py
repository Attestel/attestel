"""Point-in-time prediction and outcome store (contract §5, §5.1, §3.11, §9.25, §9.26, §9.46).

This is the component the whole programme exists to make possible: a record of what the analyst
said, *at a cutoff*, with everything needed to reproduce it, and an outcome resolved later from
stored bars only. Everything here is arranged so that a number in the ablation ladder can be
attributed to a cause — or refused.

Four rules run through the file. Each of them is code, not a comment:

1. **`as_of` is the cutoff; `created_at` is the wall clock.** They are stored separately and a write
   with `createdAt < asOf` is rejected. A record whose wall clock precedes its own cutoff is either
   a clock bug or a record built from the future, and neither may enter the store.

2. **`experiment` and `split` are on every row, and no aggregate query may omit either.**
   `build_prediction_query(..., aggregate=True)` raises without both. A rung silently mixed into
   another rung's statistics is invisible afterwards, which is exactly the failure the ladder
   exists to detect.

3. **Outcomes resolve over HTTP to `{ANALYSIS_URL}/candles` and nowhere else** (§9.25). `bars` lives
   in the analysis service's private PostgreSQL schema; this module never opens it,
   never attaches it, and never calls `/quote/{ticker}` — a live quote on a historical resolution
   silently invalidates every number downstream and nobody notices for months.

4. **A synthetic or absent bar never resolves an outcome** (§9.46). `/candles` carries `source` and
   `sourceIsSynthetic`; if either says synthetic anywhere in the data used to resolve a horizon,
   this module writes a REFUSAL row — every metric column NULL, `resolved_at` NULL, and
   `resolved_from_source` carrying the synthetic label — and counts it. The refusal is recorded, in
   the store, queryable locally. It is never substituted with a live fetch, a neighbouring bar, or a
   zero.

The only outbound call in this module is to `ANALYSIS_URL`. `requests` is already a dependency of
this service; nothing new is added (D-26, AD-2).
"""
from __future__ import annotations

import json
import math
import os
import secrets
from contextlib import contextmanager

import requests
from fastapi import APIRouter, Body, HTTPException, Query

from .db import Connection, Row

from .cluster import iso, parse_iso
from .db import connect
from .events import resolve_as_of

router = APIRouter()

# =================================================================================================
# Constants — every one of them versioned or enumerated by the contract
# =================================================================================================

ID_PREFIX = "prd_"          # §Conventions: reserved prefixes. `ev_`/`out_` belong to the journal.
ID_HEX_CHARS = 16

# §5.1. Rung A requires a VL model and is not built; the letters are still the closed enum, so a
# record can never claim a rung the ladder does not define.
EXPERIMENTS = ("A", "B", "C", "D", "E", "F")
SPLITS = ("dev", "validation", "test")

# §Conventions: map keys use the short form. The long form belongs to a single-horizon analyst
# response (§7) and is converted in services/llm/app/analyst.py, never here.
HORIZONS = ("1d", "5d", "20d", "60d")

# Horizons are TRADING DAYS. This many bars forward in the stored daily series — counted over rows
# that exist, never by calendar arithmetic, so a market holiday cannot silently eat an outcome.
HORIZON_BARS = {"1d": 1, "5d": 5, "20d": 20, "60d": 60}

DIRECTIONS = ("bullish", "bearish", "neutral", "unclear")
CONFIDENCE_BUCKETS = ("low", "medium", "high")
REASONING_MODES = ("thinking", "non_thinking")

# §9.40: identity has SEVEN keys and `temperature` lives inside generationSettings, once.
IDENTITY_KEYS = (
    "modelUsed",
    "quantization",
    "reasoningMode",
    "promptVersion",
    "schemaVersion",
    "toolSchemaVersion",
    "generationSettings",
)
# `quantization` may legitimately be null (an unquantised model); the KEY must still be present, so
# an omission is a rejected write rather than a silent null in the reproducibility block.
IDENTITY_REQUIRED_NONEMPTY = (
    "modelUsed",
    "reasoningMode",
    "promptVersion",
    "schemaVersion",
    "toolSchemaVersion",
)

# --- splits (locked decision 3) -------------------------------------------------------------------
# Assigned deterministically from `as_of` over DISJOINT time windows, never reassigned, never
# inferred at read time. Doc §11.1.5: the final test period is not a place to tune prompts.
#
# The store is the authority: a write whose `split` disagrees with `split_for(asOf)` is REJECTED
# rather than corrected, because a corrected split would be a silent reassignment of exactly the
# kind this rule forbids.
SPLIT_VERSION = "split@1"
SPLIT_DEV_END = "2024-01-01T00:00:00Z"          # dev:        as_of <  this
SPLIT_VALIDATION_END = "2025-07-01T00:00:00Z"   # validation: dev_end <= as_of < this
#                                               # test:       as_of >= validation_end

# --- outcome resolution (§9.25, §9.26) ------------------------------------------------------------
OUTCOME_VERSION = "outcome@1"
REGIME_VERSION = "regime@1"

BENCHMARK_TICKER = "SPY"

# §9.26. Owned here, static, no lookup service. Both proxies must join the analysis service's warm
# set so their daily bars persist — that is a change in a file this lane does not own and is
# recorded in the handoff.
TICKER_SECTOR_ETF = {
    "NVDA": "SMH",
    "AMD": "SMH",
    "AVGO": "SMH",
    "MSFT": "XLK",
    "GOOGL": "XLK",
    "META": "XLK",
}
SECTOR_ETF_DEFAULT = BENCHMARK_TICKER

# `regime_label` thresholds. Deterministic, computed in code from the candles that resolved the
# horizon, versioned by REGIME_VERSION. No model call anywhere in this module.
#   high_vol : annualised realised vol over the horizon path exceeds this
#   bull/bear: path return outside ±(REGIME_BAND_PER_SQRT_DAY * sqrt(bars)), which keeps the band
#              comparable across a 1-day and a 60-day horizon instead of calling every 60d path
#              "bull" merely because it is longer.
REGIME_HIGH_VOL_ANNUALISED = 0.60
REGIME_BAND_PER_SQRT_DAY = 0.02
TRADING_DAYS_PER_YEAR = 252

# Synthetic detection. `/candles` labels a window with EVERY distinct source in it joined by '+'
# (`services/analysis/app/data.py::_label_sources`), so the value can be 'yfinance+synthetic', not
# just 'synthetic'. A substring test is therefore the correct check and an equality test is a hole.
SYNTHETIC_SOURCE = "synthetic"

ANALYSIS_TIMEOUT_SECONDS = 10.0

# How far past a prediction's cutoff the backfill reads when counting trading days. 60 trading days
# is the longest horizon; the slack covers holidays and a partial leading week.
FORWARD_READ_SLACK_BARS = 20


def analysis_url() -> str:
    """Read at call time, never at import, so a container that configures late and a test that
    monkeypatches are both honoured. The events service has no ANALYSIS_URL in its compose
    environment yet — that is a reserved-file change recorded in the handoff; the default below is
    the compose service name every other consumer uses (§9.31)."""
    return (os.getenv("ANALYSIS_URL", "").strip() or "http://analysis:8001").rstrip("/")


# =================================================================================================
# Small helpers
# =================================================================================================


@contextmanager
def _db():
    conn = connect()
    try:
        yield conn
    finally:
        conn.close()


def _reject(field: str, detail: str):
    """422 naming the offending field.

    A record that cannot be attributed to a cause is worse than no record — it pollutes the ladder —
    so this path rejects rather than defaulting. 422 rather than this service's usual 400 because
    the brief for this store specifies it and it matches FastAPI's own body-validation status.
    """
    raise HTTPException(status_code=422, detail=f"{field}: {detail}")


def new_prediction_id() -> str:
    """`prd_` + 16 lowercase hex (§Conventions)."""
    return ID_PREFIX + secrets.token_hex(ID_HEX_CHARS // 2)


def split_for(as_of: str) -> str:
    """The split a cutoff belongs to. Deterministic, total, and disjoint by construction — the three
    windows are defined by two boundaries, so no cutoff can land in two of them."""
    stamp = iso(as_of)
    if stamp < SPLIT_DEV_END:
        return "dev"
    if stamp < SPLIT_VALIDATION_END:
        return "validation"
    return "test"


def _date_of(stamp: str) -> str:
    """The UTC calendar date of an ISO timestamp. Daily bars are keyed by date in `/candles`."""
    return iso(stamp)[:10]


def _end_of_day(date: str) -> str:
    """`YYYY-MM-DD` -> the last second of that UTC day.

    Measured, not assumed: `/candles` resolves `as_of` through `parse_as_of` and compares
    `ts <= as_of` as a STRING against stored bars whose ts is fixed-width
    (`services/analysis/app/store.py::TS_FMT`). A date-only cutoff would land at midnight and drop a
    bar stored later in the day. End-of-day includes that day's bar and excludes the next one.
    """
    return f"{date}T23:59:59Z"


def _is_synthetic(payload: dict) -> bool:
    """True if the candle response is synthetic in either of the two ways it can say so."""
    if bool(payload.get("sourceIsSynthetic")):
        return True
    return SYNTHETIC_SOURCE in str(payload.get("source") or "").lower()


def _json_loads(text, default):
    try:
        value = json.loads(text)
    except (TypeError, ValueError):
        return default
    return value if value is not None else default


# =================================================================================================
# The one query builder (locked decision 2)
# =================================================================================================


def build_prediction_query(
    *,
    aggregate: bool = False,
    experiment: str | None = None,
    split: str | None = None,
    ticker: str | None = None,
    as_of: str | None = None,
    date_from: str | None = None,
    date_to: str | None = None,
    limit: int | None = None,
    columns: str = "*",
) -> tuple[str, dict]:
    """Every read of `predictions` in this lane is built here.

    `aggregate=True` means "the rows are about to be pooled into a statistic". Such a query MUST
    name an `experiment` AND a `split`; without both it raises `ValueError` before any SQL exists.
    That is locked decision 2 as code: there is no aggregate path that can silently pool rung C with
    rung D, or the dev window with the untouched test window, because the query cannot be built.

    A listing (`GET /predictions`, §3.11) is not an aggregate — the contract's query set makes
    `experiment` optional there and has no `split` at all — so `aggregate=False` allows both to be
    absent. What it never allows is a *statistic* over an unfiltered set.
    """
    if aggregate:
        if not experiment:
            raise ValueError(
                "an aggregate over `predictions` must filter on `experiment` (contract §5.1): "
                "pooling two rungs produces a number that belongs to neither"
            )
        if not split:
            raise ValueError(
                "an aggregate over `predictions` must filter on `split` (locked decision 2): "
                "pooling dev with test destroys the only untouched evaluation window there is"
            )
    where: list[str] = []
    params: dict[str, object] = {}
    if experiment:
        where.append("p.experiment = :experiment")
        params["experiment"] = experiment
    if split:
        where.append("p.split = :split")
        params["split"] = split
    if ticker:
        where.append("p.ticker = :ticker")
        params["ticker"] = ticker.strip().upper()
    if as_of:
        # §1.1/§1.2 applied to this table: `as_of` is the cutoff (the analogue of published_at) and
        # `created_at` is when the store learned it (the analogue of first_seen_at). BOTH must pass,
        # so a prediction written today cannot be read as if it had existed at a past cutoff.
        where.append("p.as_of <= :cutoff AND p.created_at <= :cutoff")
        params["cutoff"] = iso(as_of)
    if date_from:
        where.append("p.as_of >= :date_from")
        params["date_from"] = iso(date_from)
    if date_to:
        where.append("p.as_of <= :date_to")
        params["date_to"] = iso(date_to)
    sql = f"SELECT {columns} FROM predictions p"
    if where:
        sql += " WHERE " + " AND ".join(where)
    sql += " ORDER BY p.as_of DESC, p.id DESC"
    if limit is not None:
        sql += " LIMIT :limit"
        params["limit"] = int(limit)
    return sql, params


# =================================================================================================
# Write path (§3.11 POST /predictions)
# =================================================================================================


def _require(payload: dict, field: str):
    value = payload.get(field)
    if value is None or (isinstance(value, str) and not value.strip()):
        _reject(field, "is required")
    return value


def _validate_identity(identity) -> dict:
    if not isinstance(identity, dict):
        _reject("identity", "must be an object with §5's seven keys")
    for key in IDENTITY_KEYS:
        if key not in identity:
            _reject(f"identity.{key}", "is required (contract §5, §9.40)")
    for key in IDENTITY_REQUIRED_NONEMPTY:
        value = identity.get(key)
        if not isinstance(value, str) or not value.strip():
            _reject(f"identity.{key}", "is required and must be a non-empty string")
    if identity.get("reasoningMode") not in REASONING_MODES:
        _reject("identity.reasoningMode", f"must be one of {list(REASONING_MODES)}")
    settings = identity.get("generationSettings")
    if not isinstance(settings, dict):
        _reject("identity.generationSettings", "must be an object (temperature, topP, seed)")
    if "temperature" in identity:
        # §9.40: one canonical location per value. A top-level temperature beside the nested one
        # drifts the first time someone writes to one and not the other.
        _reject(
            "identity.temperature",
            "lives in identity.generationSettings.temperature and nowhere else (contract §9.40)",
        )
    return identity


def _validate_evidence(evidence) -> dict:
    if not isinstance(evidence, dict):
        _reject("evidence", "must be an object (contract §5)")
    bars_ref = evidence.get("barsRef")
    if not isinstance(bars_ref, dict) or not bars_ref:
        _reject("evidence.barsRef", "is required: the OHLCV reference the record was built on")
    technical_hash = evidence.get("technicalStateHash")
    if not isinstance(technical_hash, str) or not technical_hash.strip():
        _reject(
            "evidence.technicalStateHash",
            "is required: sha256 of the exact technical payload sent to the model",
        )
    for key in ("eventIds", "macroEventIds", "toolCalls"):
        value = evidence.get(key, [])
        if value is not None and not isinstance(value, list):
            _reject(f"evidence.{key}", "must be an array")
    return evidence


def _validate_forecast(forecast) -> dict:
    if not isinstance(forecast, dict):
        _reject("forecast", "must be an object (contract §5)")
    horizons = forecast.get("horizons")
    if not isinstance(horizons, dict) or not horizons:
        _reject("forecast.horizons", f"is required and must cover {list(HORIZONS)}")
    unknown = [k for k in horizons if k not in HORIZONS]
    if unknown:
        _reject("forecast.horizons", f"unknown horizon keys {unknown}; §Conventions fixes {list(HORIZONS)}")
    for key, entry in horizons.items():
        if not isinstance(entry, dict):
            _reject(f"forecast.horizons.{key}", "must be an object {direction, score, abstain}")
        if entry.get("direction") not in DIRECTIONS:
            _reject(f"forecast.horizons.{key}.direction", f"must be one of {list(DIRECTIONS)}")
        score = entry.get("score")
        if score is not None and not isinstance(score, (int, float)):
            _reject(f"forecast.horizons.{key}.score", "must be a number or null")
        if not isinstance(entry.get("abstain"), bool):
            _reject(f"forecast.horizons.{key}.abstain", "must be a boolean")
    if forecast.get("confidenceBucket") not in CONFIDENCE_BUCKETS:
        _reject("forecast.confidenceBucket", f"must be one of {list(CONFIDENCE_BUCKETS)}")
    return forecast


@router.post("/predictions")
def post_prediction(payload: dict = Body(...)) -> dict:
    """Contract §3.11 / §5. Rejects, loudly and by field name, anything that cannot be attributed."""
    if not isinstance(payload, dict):
        _reject("body", "must be a JSON object")

    ticker = str(_require(payload, "ticker")).strip().upper()

    raw_as_of = _require(payload, "asOf")
    raw_created = _require(payload, "createdAt")
    try:
        as_of = iso(raw_as_of)
    except (ValueError, TypeError):
        _reject("asOf", f"is not an ISO 8601 timestamp: {raw_as_of!r}")
    try:
        created_at = iso(raw_created)
    except (ValueError, TypeError):
        _reject("createdAt", f"is not an ISO 8601 timestamp: {raw_created!r}")
    if created_at < as_of:
        # The wall clock cannot precede the cutoff. Either the clock is wrong or the record was
        # built with knowledge from after its own cutoff; both are disqualifying.
        _reject(
            "createdAt",
            f"is before asOf ({created_at} < {as_of}): asOf is the cutoff, createdAt is the wall clock",
        )

    experiment = str(_require(payload, "experiment")).strip().upper()
    if experiment not in EXPERIMENTS:
        _reject("experiment", f"must be one of {list(EXPERIMENTS)} (contract §5.1)")

    split = str(_require(payload, "split")).strip().lower()
    if split not in SPLITS:
        _reject("split", f"must be one of {list(SPLITS)}")
    expected_split = split_for(as_of)
    if split != expected_split:
        _reject(
            "split",
            f"{split!r} disagrees with the deterministic assignment {expected_split!r} for asOf "
            f"{as_of} ({SPLIT_VERSION}); split is derived from the cutoff and never chosen",
        )

    identity = _validate_identity(payload.get("identity"))
    evidence = _validate_evidence(payload.get("evidence"))
    forecast = _validate_forecast(payload.get("forecast"))

    prediction_id = new_prediction_id()
    with _db() as conn:
        conn.execute(
            "INSERT INTO predictions ("
            "  id, ticker, as_of, created_at, experiment, split,"
            "  model_used, quantization, reasoning_mode, prompt_version, schema_version,"
            "  tool_schema_version, generation_settings,"
            "  bars_ref, technical_state_hash, earnings_snapshot_id, tool_calls,"
            "  forecast, confidence_bucket, thesis, invalidation"
            ") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
            (
                prediction_id,
                ticker,
                as_of,
                created_at,
                experiment,
                split,
                identity["modelUsed"],
                identity.get("quantization"),
                identity["reasoningMode"],
                identity["promptVersion"],
                identity["schemaVersion"],
                identity["toolSchemaVersion"],
                json.dumps(identity["generationSettings"], sort_keys=True),
                json.dumps(evidence["barsRef"], sort_keys=True),
                evidence["technicalStateHash"],
                evidence.get("earningsSnapshotId"),
                json.dumps(evidence.get("toolCalls") or []),
                json.dumps(forecast["horizons"], sort_keys=True),
                forecast["confidenceBucket"],
                forecast.get("thesis") or "",
                forecast.get("invalidation") or "",
            ),
        )
        # One row per referenced artefact. The join stays inside the event store so it cannot leak across
        # a service boundary at read time (AD-2, §5).
        for kind, key in (("event", "eventIds"), ("macro_event", "macroEventIds")):
            for ref in evidence.get(key) or []:
                ref_id = str(ref).strip()
                if not ref_id:
                    continue
                conn.execute(
                    "INSERT INTO prediction_evidence (prediction_id, kind, ref_id) "
                    "VALUES (?,?,?) ON CONFLICT (prediction_id, kind, ref_id) DO NOTHING",
                    (prediction_id, kind, ref_id),
                )
        conn.commit()
        row = conn.execute("SELECT * FROM predictions WHERE id = ?", (prediction_id,)).fetchone()
        body = _prediction_json(conn, row)
    return {"prediction": body}


# =================================================================================================
# Read path (§3.11 GET /predictions)
# =================================================================================================


def _prediction_json(conn: Connection, row: Row) -> dict:
    """§5's shape, camelCase, with outcomes inlined.

    Outcomes ride along deliberately: §3.11 defines no separate outcomes read, and the ablation
    driver's synthetic refusal is specified as a LOCAL query against `outcomes` rather than a second
    cross-service call. Inlining keeps the prediction↔outcome join inside the events DB, which is
    where §5 put it.
    """
    evidence_rows = conn.execute(
        "SELECT kind, ref_id FROM prediction_evidence WHERE prediction_id = ? ORDER BY kind, ref_id",
        (row["id"],),
    ).fetchall()
    outcome_rows = conn.execute(
        "SELECT * FROM outcomes WHERE prediction_id = ? ORDER BY horizon", (row["id"],)
    ).fetchall()
    return {
        "id": row["id"],
        "ticker": row["ticker"],
        "asOf": row["as_of"],
        "createdAt": row["created_at"],
        "experiment": row["experiment"],
        "split": row["split"],
        "splitVersion": SPLIT_VERSION,
        "identity": {
            "modelUsed": row["model_used"],
            "quantization": row["quantization"],
            "reasoningMode": row["reasoning_mode"],
            "promptVersion": row["prompt_version"],
            "schemaVersion": row["schema_version"],
            "toolSchemaVersion": row["tool_schema_version"],
            "generationSettings": _json_loads(row["generation_settings"], {}),
        },
        "evidence": {
            "barsRef": _json_loads(row["bars_ref"], {}),
            "technicalStateHash": row["technical_state_hash"],
            "eventIds": [r["ref_id"] for r in evidence_rows if r["kind"] == "event"],
            "macroEventIds": [r["ref_id"] for r in evidence_rows if r["kind"] == "macro_event"],
            "earningsSnapshotId": row["earnings_snapshot_id"],
            "toolCalls": _json_loads(row["tool_calls"], []),
        },
        "forecast": {
            "horizons": _json_loads(row["forecast"], {}),
            "confidenceBucket": row["confidence_bucket"],
            "thesis": row["thesis"],
            "invalidation": row["invalidation"],
        },
        "outcomes": {r["horizon"]: _outcome_json(r) for r in outcome_rows},
    }


def _outcome_json(row: Row) -> dict:
    return {
        "horizon": row["horizon"],
        "realizedReturn": row["realized_return"],
        "benchmarkReturn": row["benchmark_return"],
        "sectorReturn": row["sector_return"],
        "excessReturn": row["excess_return"],
        "realizedVol": row["realized_vol"],
        "regimeLabel": row["regime_label"],
        "resolvedAt": row["resolved_at"],
        "resolvedFromBarsTs": row["resolved_from_bars_ts"],
        "resolvedFromSource": row["resolved_from_source"],
        # Derived, not stored: a row with no `resolved_at` is either not yet mature or a §9.46
        # refusal, and the caller must be able to tell those apart without re-deriving the rule.
        "resolved": row["resolved_at"] is not None,
        "refused": row["resolved_at"] is None
        and SYNTHETIC_SOURCE in str(row["resolved_from_source"] or "").lower(),
    }


@router.get("/predictions")
def get_predictions(
    ticker: str | None = Query(default=None),
    from_: str | None = Query(default=None, alias="from"),
    to: str | None = Query(default=None),
    experiment: str | None = Query(default=None),
    split: str | None = Query(default=None),
    as_of: str | None = Query(default=None),
    limit: int = Query(default=50, ge=1, le=500),
) -> dict:
    """Contract §3.11. Echoes the resolved `asOf` and carries `degraded`, like every read here.

    `split` is an additive filter beyond §3.11's listed query set: the ablation driver needs to read
    one split's records without pooling, and the alternative — filtering client-side after a listing
    — is exactly the Python-side filter §1.1 forbids for cutoffs.
    """
    resolved_as_of, _historical = resolve_as_of(as_of)
    if experiment and experiment.strip().upper() not in EXPERIMENTS:
        _reject("experiment", f"must be one of {list(EXPERIMENTS)}")
    if split and split.strip().lower() not in SPLITS:
        _reject("split", f"must be one of {list(SPLITS)}")
    try:
        sql, params = build_prediction_query(
            aggregate=False,
            experiment=experiment.strip().upper() if experiment else None,
            split=split.strip().lower() if split else None,
            ticker=ticker,
            as_of=resolved_as_of,
            date_from=from_,
            date_to=to,
            limit=limit,
        )
    except ValueError as exc:  # pragma: no cover — aggregate=False cannot raise
        _reject("query", str(exc))
    with _db() as conn:
        rows = conn.execute(sql, params).fetchall()
        predictions = [_prediction_json(conn, r) for r in rows]
    return {
        "predictions": predictions,
        "asOf": resolved_as_of,
        "splitVersion": SPLIT_VERSION,
        "degraded": [],
    }


# =================================================================================================
# Outcome resolution (§9.25, §9.26, §9.46) — the only outbound call in this service
# =================================================================================================


class CandleUnavailable(Exception):
    """The analysis service could not be reached, or answered with no usable bars.

    Carried as an exception so the backfill can leave the horizon `pending` and keep going. It never
    triggers a fallback: there is no second source, by design.
    """


def fetch_candles(ticker: str, *, as_of: str, limit: int) -> dict:
    """`GET {ANALYSIS_URL}/candles/{ticker}?timeframe=1D&as_of=…&limit=…` — contract §9.25.

    This is the ONLY network call this module makes, and the only permitted route to a bar. There is
    no direct read of the analysis PostgreSQL schema, no cross-schema query, and no
    `/quote/{ticker}` — a live quote on a historical resolution would
    silently invalidate every number the programme produces.
    """
    url = f"{analysis_url()}/candles/{ticker.strip().upper()}"
    try:
        response = requests.get(
            url,
            params={"timeframe": "1D", "as_of": as_of, "limit": int(limit)},
            timeout=ANALYSIS_TIMEOUT_SECONDS,
        )
    except Exception as exc:  # noqa: BLE001 — requests raises a family; all of them mean "no bar"
        raise CandleUnavailable(f"{url}: {type(exc).__name__}: {exc}") from exc
    if response.status_code != 200:
        raise CandleUnavailable(f"{url}: HTTP {response.status_code}")
    try:
        payload = response.json()
    except ValueError as exc:
        raise CandleUnavailable(f"{url}: response was not JSON") from exc
    if not isinstance(payload, dict):
        raise CandleUnavailable(f"{url}: response was not an object")
    return payload


def _one_bar(payload: dict) -> dict | None:
    bars = payload.get("bars") or []
    return bars[-1] if bars else None


def _returns(closes: list[float]) -> list[float]:
    return [
        (closes[i] - closes[i - 1]) / closes[i - 1]
        for i in range(1, len(closes))
        if closes[i - 1]
    ]


def realized_vol(closes: list[float]) -> float | None:
    """Annualised sample standard deviation of daily simple returns over the horizon path.

    `None` — never 0.0 — when there are fewer than two returns to work with. A 1-day horizon has one
    return and therefore no dispersion; reporting 0.0 would claim a zero-volatility path, which is a
    different and false statement.
    """
    rets = _returns(closes)
    if len(rets) < 2:
        return None
    mean = sum(rets) / len(rets)
    variance = sum((r - mean) ** 2 for r in rets) / (len(rets) - 1)
    return math.sqrt(variance) * math.sqrt(TRADING_DAYS_PER_YEAR)


def regime_label(closes: list[float]) -> str | None:
    """`bull | bear | sideways | high_vol` from the resolved candles. Deterministic, versioned by
    REGIME_VERSION, computed over the horizon path (baseline bar through target bar inclusive).

    No model is consulted, here or anywhere in this module.
    """
    if len(closes) < 2 or not closes[0]:
        return None
    vol = realized_vol(closes)
    if vol is not None and vol > REGIME_HIGH_VOL_ANNUALISED:
        return "high_vol"
    path_return = (closes[-1] - closes[0]) / closes[0]
    band = REGIME_BAND_PER_SQRT_DAY * math.sqrt(max(1, len(closes) - 1))
    if path_return > band:
        return "bull"
    if path_return < -band:
        return "bear"
    return "sideways"


def sector_etf_for(ticker: str) -> str:
    """§9.26's static map. `SPY` for anything unmapped — a named default, not a guess."""
    return TICKER_SECTOR_ETF.get(ticker.strip().upper(), SECTOR_ETF_DEFAULT)


class _CandleCache:
    """Per-backfill memo over `(ticker, as_of, limit)`.

    A backfill resolves many horizons for the same ticker and the same benchmark; without this, one
    run would ask the analysis service the same question hundreds of times. It is a cache of
    RESPONSES, not of bars — it never synthesises an answer it did not receive.
    """

    def __init__(self, fetch=fetch_candles):
        self._fetch = fetch
        self._store: dict[tuple[str, str, int], dict | CandleUnavailable] = {}

    def get(self, ticker: str, *, as_of: str, limit: int) -> dict:
        key = (ticker.strip().upper(), as_of, int(limit))
        cached = self._store.get(key)
        if isinstance(cached, CandleUnavailable):
            raise cached
        if cached is not None:
            return cached
        try:
            payload = self._fetch(ticker, as_of=as_of, limit=limit)
        except CandleUnavailable as exc:
            self._store[key] = exc
            raise
        self._store[key] = payload
        return payload


def _close_at(cache: _CandleCache, ticker: str, date: str) -> tuple[float | None, dict | None]:
    """The close of `ticker`'s daily bar ON `date`, via the §9.25 single-candle call.

    Returns `(close, payload)`. `close` is None when the day has no stored bar — a holiday, a
    coverage gap, or a ticker nobody ingested. The payload is returned even then so the caller can
    inspect `source` / `sourceIsSynthetic`.
    """
    payload = cache.get(ticker, as_of=_end_of_day(date), limit=1)
    bar = _one_bar(payload)
    if bar is None or str(bar.get("time")) != date:
        return None, payload
    try:
        return float(bar["close"]), payload
    except (KeyError, TypeError, ValueError):
        return None, payload


def _proxy_return(
    cache: _CandleCache, proxy: str, baseline_date: str, target_date: str, degraded: list[str]
) -> float | None:
    """A benchmark or sector return over the same two calendar anchors as the ticker's outcome.

    `None` — never 0.0 — on missing coverage, on an unreachable service, and on a synthetic bar. A
    missing benchmark is not a zero benchmark, and a synthetic benchmark is a fabricated one.
    """
    try:
        base_close, base_payload = _close_at(cache, proxy, baseline_date)
        target_close, target_payload = _close_at(cache, proxy, target_date)
    except CandleUnavailable as exc:
        degraded.append(f"{proxy}:unavailable")
        del exc
        return None
    for payload in (base_payload, target_payload):
        if payload is not None and _is_synthetic(payload):
            degraded.append(f"{proxy}:synthetic")
            return None
    if base_close is None or target_close is None or not base_close:
        degraded.append(f"{proxy}:no_coverage")
        return None
    return (target_close - base_close) / base_close


def _write_refusal(conn: Connection, prediction_id: str, horizon: str, source: str) -> None:
    """§9.46: record the refusal, never substitute.

    Every metric column stays NULL and `resolved_at` stays NULL, so this row can never be mistaken
    for a result — but `resolved_from_source` carries the synthetic label, so the refusal is
    DISCOVERABLE by a local query against `outcomes`, which is exactly how the ladder's refusal is
    specified to work (§9.25). Silence here would leave a synthetic resolution indistinguishable
    from a horizon that simply has not matured.
    """
    conn.execute(
        "INSERT INTO outcomes (prediction_id, horizon, resolved_from_source) VALUES (?,?,?) "
        "ON CONFLICT(prediction_id, horizon) DO UPDATE SET "
        "  realized_return = NULL, benchmark_return = NULL, sector_return = NULL,"
        "  excess_return = NULL, realized_vol = NULL, regime_label = NULL,"
        "  resolved_at = NULL, resolved_from_bars_ts = NULL, resolved_from_source = excluded.resolved_from_source",
        (prediction_id, horizon, source),
    )


def resolve_horizon(
    cache: _CandleCache,
    *,
    ticker: str,
    as_of: str,
    horizon: str,
    cutoff: str,
    degraded: list[str],
) -> dict:
    """Resolve one horizon, or explain why it cannot be resolved.

    Returns a dict with `status` ∈ `resolved | pending | refused` and, when resolved, the outcome
    columns. The three statuses are the whole vocabulary: there is no fourth branch that produces a
    number from something other than a stored, non-synthetic bar.
    """
    bars_forward = HORIZON_BARS[horizon]
    baseline_date = _date_of(as_of)

    # One wide read per (ticker, cutoff) to COUNT TRADING DAYS over rows that exist. Calendar
    # arithmetic would turn a market holiday into a missing or mis-dated outcome.
    try:
        window = cache.get(
            ticker,
            as_of=cutoff,
            limit=_forward_limit(baseline_date, cutoff, bars_forward),
        )
    except CandleUnavailable as exc:
        degraded.append(f"{ticker}:unavailable")
        del exc
        return {"status": "pending", "reason": "analysis_unreachable"}

    if _is_synthetic(window):
        return {
            "status": "refused",
            "reason": "synthetic_window",
            "source": str(window.get("source") or SYNTHETIC_SOURCE),
        }

    bars = window.get("bars") or []
    baseline_index = None
    for i, bar in enumerate(bars):
        if str(bar.get("time")) <= baseline_date:
            baseline_index = i
        else:
            break
    if baseline_index is None:
        return {"status": "pending", "reason": "no_baseline_bar"}
    target_index = baseline_index + bars_forward
    if target_index >= len(bars):
        # Not enough trading days have been stored yet. `pending` is the honest answer; the nearest
        # available bar is a different horizon wearing this horizon's name.
        return {"status": "pending", "reason": "horizon_not_matured"}

    baseline_date = str(bars[baseline_index].get("time"))
    target_date = str(bars[target_index].get("time"))

    # The §9.25 resolving reads: one candle each, by date, through the mandated endpoint. Done as
    # their own calls (rather than trusting the wide window) so `resolved_from_bars_ts` and
    # `resolved_from_source` describe the response that actually resolved the horizon.
    try:
        base_close, base_payload = _close_at(cache, ticker, baseline_date)
        target_close, target_payload = _close_at(cache, ticker, target_date)
    except CandleUnavailable as exc:
        degraded.append(f"{ticker}:unavailable")
        del exc
        return {"status": "pending", "reason": "analysis_unreachable"}

    for payload in (base_payload, target_payload):
        if payload is not None and _is_synthetic(payload):
            return {
                "status": "refused",
                "reason": "synthetic_bar",
                "source": str(payload.get("source") or SYNTHETIC_SOURCE),
            }
    if base_close is None or target_close is None or not base_close:
        return {"status": "pending", "reason": "no_resolving_bar"}

    path = []
    for bar in bars[baseline_index : target_index + 1]:
        try:
            path.append(float(bar["close"]))
        except (KeyError, TypeError, ValueError):
            path = []
            break

    sector = sector_etf_for(ticker)
    benchmark_return = _proxy_return(cache, BENCHMARK_TICKER, baseline_date, target_date, degraded)
    sector_return = (
        benchmark_return
        if sector == BENCHMARK_TICKER
        else _proxy_return(cache, sector, baseline_date, target_date, degraded)
    )
    realized = (target_close - base_close) / base_close
    return {
        "status": "resolved",
        "realized_return": realized,
        "benchmark_return": benchmark_return,
        "sector_return": sector_return,
        # NULL, never 0, when the benchmark is missing: a missing benchmark is not a zero benchmark.
        "excess_return": None if benchmark_return is None else realized - benchmark_return,
        "realized_vol": realized_vol(path) if path else None,
        "regime_label": regime_label(path) if path else None,
        "resolved_from_bars_ts": target_date,
        "resolved_from_source": str((target_payload or {}).get("source") or ""),
    }


def _forward_limit(baseline_date: str, cutoff: str, bars_forward: int) -> int:
    """How many daily bars to read so the baseline bar and `bars_forward` bars after it are both in
    the window. Calendar days times a trading-day density, plus the horizon and slack — deliberately
    generous, because a window that is one bar short turns a resolvable outcome into a false
    `pending`."""
    try:
        span_days = (parse_iso(cutoff) - parse_iso(baseline_date)).days
    except (ValueError, TypeError):
        span_days = 0
    trading_days = int(max(0, span_days) * 0.72) + bars_forward + FORWARD_READ_SLACK_BARS
    return max(bars_forward + FORWARD_READ_SLACK_BARS, min(trading_days, 5000))


@router.post("/outcomes/backfill")
def post_outcomes_backfill(payload: dict = Body(default=None)) -> dict:
    """Contract §3.11. `{asOf}` in, `{updated, pending}` out.

    `asOf` is the cutoff the backfill resolves UP TO — a horizon whose target bar lies after it stays
    `pending`. Nothing here falls back to a live fetch: if the candle cannot be resolved, the horizon
    is counted and left alone.

    Two counters beyond §3.11's pair, both additive and both subsets of `pending`: `refused` is
    §9.46's synthetic refusal (recorded in `outcomes`, never a number) and `degraded` names what was
    unreachable, as every other response in this service does.
    """
    body = payload if isinstance(payload, dict) else {}
    resolved_as_of, _historical = resolve_as_of(body.get("asOf"))
    ticker_filter = body.get("ticker")
    experiment_filter = body.get("experiment")

    cache = _CandleCache()
    degraded: list[str] = []
    updated = 0
    pending = 0
    refused = 0

    sql, params = build_prediction_query(
        aggregate=False,
        experiment=str(experiment_filter).strip().upper() if experiment_filter else None,
        ticker=ticker_filter,
        as_of=resolved_as_of,
        columns="p.id, p.ticker, p.as_of, p.forecast",
    )
    with _db() as conn:
        rows = conn.execute(sql, params).fetchall()
        for row in rows:
            horizons = _json_loads(row["forecast"], {})
            existing = {
                r["horizon"]: r
                for r in conn.execute(
                    "SELECT * FROM outcomes WHERE prediction_id = ?", (row["id"],)
                ).fetchall()
            }
            for horizon in HORIZONS:
                if horizon not in horizons:
                    continue
                current = existing.get(horizon)
                if current is not None and current["resolved_at"] is not None:
                    continue
                result = resolve_horizon(
                    cache,
                    ticker=row["ticker"],
                    as_of=row["as_of"],
                    horizon=horizon,
                    cutoff=resolved_as_of,
                    degraded=degraded,
                )
                if result["status"] == "refused":
                    _write_refusal(conn, row["id"], horizon, result["source"])
                    refused += 1
                    pending += 1
                    continue
                if result["status"] != "resolved":
                    pending += 1
                    continue
                conn.execute(
                    "INSERT INTO outcomes ("
                    "  prediction_id, horizon, realized_return, benchmark_return, sector_return,"
                    "  excess_return, realized_vol, regime_label, resolved_at,"
                    "  resolved_from_bars_ts, resolved_from_source"
                    ") VALUES (?,?,?,?,?,?,?,?,?,?,?) "
                    "ON CONFLICT(prediction_id, horizon) DO UPDATE SET "
                    "  realized_return = excluded.realized_return,"
                    "  benchmark_return = excluded.benchmark_return,"
                    "  sector_return = excluded.sector_return,"
                    "  excess_return = excluded.excess_return,"
                    "  realized_vol = excluded.realized_vol,"
                    "  regime_label = excluded.regime_label,"
                    "  resolved_at = excluded.resolved_at,"
                    "  resolved_from_bars_ts = excluded.resolved_from_bars_ts,"
                    "  resolved_from_source = excluded.resolved_from_source",
                    (
                        row["id"],
                        horizon,
                        result["realized_return"],
                        result["benchmark_return"],
                        result["sector_return"],
                        result["excess_return"],
                        result["realized_vol"],
                        result["regime_label"],
                        iso(resolved_as_of),
                        result["resolved_from_bars_ts"],
                        result["resolved_from_source"],
                    ),
                )
                updated += 1
        conn.commit()

    return {
        "updated": updated,
        "pending": pending,
        "refused": refused,
        "asOf": resolved_as_of,
        "outcomeVersion": OUTCOME_VERSION,
        "regimeVersion": REGIME_VERSION,
        "degraded": sorted(set(degraded)),
    }
