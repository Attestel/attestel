"""Macro releases and the store-backed calendar (contract §3.9, §3.11, §9.27).

The whole point of this module is `surprise`, and the whole point of `surprise` is that it refuses
to lie. `actual − expected` is arithmetic anyone can do; knowing when NOT to do it is the part that
matters, because a surprise computed against a consensus captured after the release, or against an
expectation recorded in a different unit, is a plausible-looking number that corrupts Wave 5A's
rung F and nothing downstream would notice.

So every refusal below is its own named guard with its own test. `0001_init.sql` additionally
enforces the timing rule in SQL — `CHECK (surprise IS NULL OR ... expectation_captured_at <
release_at)` — which means a bug here raises IntegrityError rather than writing a lie.

No provider call, ever, on any endpoint in this module (§1.3, invariant #1). `GET /calendar` is
served from the store and is ADDITIVE — `gateway/calendar.go` keeps its current shape and is not
touched.
"""
from __future__ import annotations

import hashlib
from contextlib import contextmanager

from fastapi import APIRouter, HTTPException, Query

from .cluster import iso
from .db import connect
from .events import DEFAULT_LIMIT, MAX_LIMIT, resolve_as_of

router = APIRouter()

# §3.9's expectation_source enum. `none` means "we have no consensus", which is a different thing
# from "we have one and it is zero".
EXPECTATION_SOURCES = ("fmp", "none")

# The five conditions under which a surprise is NULL. Named so a test can address each one and so a
# future reader cannot mistake one for an accident.
SURPRISE_REFUSAL_MISSING_ACTUAL = "actual is NULL"
SURPRISE_REFUSAL_MISSING_EXPECTED = "expected is NULL"
SURPRISE_REFUSAL_NO_EXPECTATION_SOURCE = "expectation_source is 'none'"
SURPRISE_REFUSAL_NO_CAPTURE_TIME = "expectation_captured_at is NULL"
SURPRISE_REFUSAL_CAPTURED_AFTER_RELEASE = "expectation_captured_at >= release_at"
SURPRISE_REFUSAL_UNIT_MISMATCH = "the actual and the expectation are recorded in different units"


@contextmanager
def _db():
    conn = connect()
    try:
        yield conn
    finally:
        conn.close()


def _macro_id(series: str, release_at: str) -> str:
    """`mac_` + 16 hex, derived from content so a rebuild mints the same id (Phase 7).

    Keyed on `(series, release_at)` — §9.41.2's natural key and the table's UNIQUE — NOT on
    `event_id`. It cannot be `event_id`: 1C writes the row at ingest with `event_id = NULL` and 2A
    backfills it later, so an id derived from it would change identity the moment the row was
    linked, and two providers reporting the same release would mint two rows.
    """
    material = f"{series}|{release_at}".encode("utf-8")
    return "mac_" + hashlib.sha256(material).hexdigest()[:16]


def compute_surprise(
    *,
    actual: float | None,
    expected: float | None,
    unit: str,
    expectation_unit: str | None = None,
    expectation_source: str | None,
    expectation_captured_at: str | None,
    release_at: str,
) -> tuple[float | None, str | None]:
    """`actual − expected`, or (None, reason) when the subtraction would be a lie.

    Returns the surprise and, when it is None, the named guard that refused it — so a caller can log
    WHY a release carries no surprise instead of silently shipping a null.
    """
    if actual is None:
        return None, SURPRISE_REFUSAL_MISSING_ACTUAL
    if expected is None:
        return None, SURPRISE_REFUSAL_MISSING_EXPECTED
    if (expectation_source or "none") == "none":
        return None, SURPRISE_REFUSAL_NO_EXPECTATION_SOURCE
    if not expectation_captured_at:
        return None, SURPRISE_REFUSAL_NO_CAPTURE_TIME
    if iso(expectation_captured_at) >= iso(release_at):
        # §3.9: "A consensus captured after the release is worthless and is treated as absent."
        # Enforced, not advised — a post-release "consensus" is just the actual, wearing a hat.
        return None, SURPRISE_REFUSAL_CAPTURED_AFTER_RELEASE
    if expectation_unit is not None and expectation_unit != unit:
        # NEVER convert units to rescue a subtraction. A `pct` minus a `pp` is not a surprise, it is
        # a bug with a number in front of it.
        return None, SURPRISE_REFUSAL_UNIT_MISMATCH
    return actual - expected, None


def record_macro_release(
    conn,
    *,
    event_id: str | None = None,
    series: str,
    release_at: str,
    actual: float | None = None,
    expected: float | None = None,
    previous: float | None = None,
    unit: str = "",
    expectation_unit: str | None = None,
    expectation_source: str | None = None,
    expectation_captured_at: str | None = None,
) -> str:
    """Write one `macro_events` row, with `surprise` computed by the guards above.

    `event_id` defaults to NULL (§9.41.1). Lane 1C calls this at ingest, from the FRED and FMP
    responses, knowing the numbers but not the event — events only exist after `assemble()` has run
    on the document that same pass just stored. Lane 2A backfills `event_id` from the document's
    `macro_key`; until it does, the row exists and carries its numbers but does not surface on
    `/events`, `/macro` or `/calendar`, all three of which filter through the parent event's
    visibility. That is the intended intermediate state, not a bug.

    PostgreSQL upsert on the `(series, release_at)` natural key means a re-ingest of the same
    release updates one row rather than forking it. Callers that need the earliest pre-release
    consensus preserved across passes must merge before calling — `ingest.store_macro_releases` does
    exactly that, and the reasoning is there.
    """
    if expectation_source is not None and expectation_source not in EXPECTATION_SOURCES:
        # Caught here rather than left to the SQL CHECK, so a caller gets the field name back
        # instead of a bare IntegrityError.
        raise ValueError(
            f"expectation_source must be one of {EXPECTATION_SOURCES}: {expectation_source!r}"
        )
    macro_id = _macro_id(series, iso(release_at))
    surprise, _reason = compute_surprise(
        actual=actual, expected=expected, unit=unit, expectation_unit=expectation_unit,
        expectation_source=expectation_source, expectation_captured_at=expectation_captured_at,
        release_at=release_at,
    )
    conn.execute(
        "INSERT INTO macro_events (id, event_id, series, release_at, actual, expected, "
        "previous, surprise, unit, expectation_source, expectation_captured_at) "
        "VALUES (?,?,?,?,?,?,?,?,?,?,?) "
        "ON CONFLICT (series, release_at) DO UPDATE SET "
        "id = EXCLUDED.id, event_id = EXCLUDED.event_id, actual = EXCLUDED.actual, "
        "expected = EXCLUDED.expected, previous = EXCLUDED.previous, surprise = EXCLUDED.surprise, "
        "unit = EXCLUDED.unit, expectation_source = EXCLUDED.expectation_source, "
        "expectation_captured_at = EXCLUDED.expectation_captured_at",
        (
            macro_id, event_id, series, iso(release_at), actual, expected, previous, surprise,
            unit, expectation_source,
            iso(expectation_captured_at) if expectation_captured_at else None,
        ),
    )
    conn.commit()
    return macro_id


def _macro_json(row) -> dict:
    return {
        "id": row["id"],
        "eventId": row["event_id"],
        "series": row["series"],
        "releaseAt": row["release_at"],
        "actual": row["actual"],
        "expected": row["expected"],
        "previous": row["previous"],
        "surprise": row["surprise"],
        "unit": row["unit"],
        "expectationSource": row["expectation_source"],
        "expectationCapturedAt": row["expectation_captured_at"],
    }


@router.get("/macro")
def list_macro(
    series: str | None = Query(default=None),
    since: str | None = Query(default=None),
    as_of: str | None = Query(default=None),
    limit: int = Query(default=DEFAULT_LIMIT, ge=1, le=MAX_LIMIT),
) -> dict:
    """Contract §3.11. The as-of filter runs against the PARENT EVENT's visibility, both halves —
    a macro row is only as visible as the event it hangs off."""
    resolved_as_of, _historical = resolve_as_of(as_of)
    where = [
        "EXISTS (SELECT 1 FROM events e WHERE e.id = m.event_id "
        "AND e.published_at <= :as_of AND e.first_seen_at <= :as_of)"
    ]
    params: dict[str, object] = {"as_of": resolved_as_of, "limit": limit}
    if series:
        marks = ",".join(f":s{i}" for i, _ in enumerate(series.split(",")))
        where.append(f"m.series IN ({marks})")
        params.update(
            {f"s{i}": s.strip().upper() for i, s in enumerate(series.split(",")) if s.strip()}
        )
    if since:
        params["since"] = iso(since)
        where.append("m.release_at >= :since")

    with _db() as conn:
        rows = conn.execute(
            f"SELECT m.* FROM macro_events m WHERE {' AND '.join(where)} "
            f"ORDER BY m.release_at DESC, m.id DESC LIMIT :limit",
            params,
        ).fetchall()
    return {"macro": [_macro_json(r) for r in rows], "asOf": resolved_as_of, "degraded": []}


@router.get("/calendar")
def get_calendar(
    from_: str = Query(alias="from"),
    to: str = Query(...),
    as_of: str | None = Query(default=None),
) -> dict:
    """Canonical Catalyst Calendar, served from the STORE ONLY.

    No read invokes a provider.  `first_seen_at` preserves what was knowable at the requested
    cutoff, and the macro table only fills structured values that passed its timing guards.
    """
    resolved_as_of, _historical = resolve_as_of(as_of)
    try:
        window_from, window_to = iso(from_), iso(to)
    except ValueError:
        raise HTTPException(status_code=400, detail="from and to must be ISO 8601 timestamps")

    with _db() as conn:
        macro_rows = conn.execute(
            "SELECT m.* FROM macro_events m WHERE m.release_at >= :from AND m.release_at <= :to "
            "AND EXISTS (SELECT 1 FROM events e WHERE e.id = m.event_id "
            "            AND e.published_at <= :as_of AND e.first_seen_at <= :as_of) "
            "ORDER BY m.release_at, m.id",
            {"from": window_from, "to": window_to, "as_of": resolved_as_of},
        ).fetchall()
        # scheduled_events (§9.27) is populated by Wave 1 Lane 1C from Alpha Vantage
        # EARNINGS_CALENDAR, FRED release dates and FMP. first_seen_at carries the as-of filter:
        # a calendar entry captured after the cutoff was not knowable at the cutoff.
        scheduled_rows = conn.execute(
            "SELECT DISTINCT s.* FROM scheduled_events s "
            "LEFT JOIN scheduled_event_history h ON h.event_id = s.id AND h.observed_at <= :as_of "
            "WHERE s.first_seen_at <= :as_of AND ("
            "  (s.scheduled_at >= :from AND s.scheduled_at <= :to) OR "
            "  (h.scheduled_at >= :from AND h.scheduled_at <= :to)"
            ") ORDER BY s.scheduled_at, s.id",
            {"from": window_from, "to": window_to, "as_of": resolved_as_of},
        ).fetchall()
        history_rows = conn.execute(
            "SELECT * FROM scheduled_event_history WHERE observed_at <= :as_of "
            "ORDER BY observed_at, id",
            {"as_of": resolved_as_of},
        ).fetchall()

    history_by_event: dict[str, list] = {}
    for row in history_rows:
        history_by_event.setdefault(row["event_id"], []).append(row)

    def revision_json(row) -> dict:
        return {
            "id": row["id"],
            "observedAt": row["observed_at"],
            "changeType": row["change_type"],
            "priorScheduledAt": row["prior_scheduled_at"],
            "scheduledAt": row["scheduled_at"],
            "priorStatus": row["prior_status"],
            "status": row["status"],
            "source": row["source"],
            "sourceTier": row["source_tier"],
        }

    def lifecycle(item: dict) -> str:
        if item.get("status") == "cancelled":
            return "cancelled"
        if item.get("actual") is not None or item.get("status") == "released":
            return "released"
        if item["scheduledAt"] < resolved_as_of:
            return "occurred"
        if item.get("confirmed"):
            return "confirmed"
        return "tentative"

    def natural_key(item: dict) -> tuple:
        return (
            item["kind"], item.get("ticker") or "", item.get("series") or "",
            item["scheduledAt"],
        )

    by_key: dict[tuple, dict] = {}
    for r in scheduled_rows:
        event_history = history_by_event.get(r["id"], [])
        state = dict(r)
        if event_history:
            latest = event_history[-1]
            for target, source in (
                ("scheduled_at", "scheduled_at"), ("status", "status"),
                ("confirmed", "confirmed"), ("source", "source"),
                ("source_tier", "source_tier"), ("source_url", "source_url"),
                ("title", "title"), ("description", "description"),
                ("importance", "importance"), ("timezone", "timezone"),
                ("local_time", "local_time"), ("previous", "previous"),
                ("expected", "expected"), ("actual", "actual"),
                ("surprise", "surprise"), ("unit", "unit"),
            ):
                state[target] = latest[source]
        if not (window_from <= state["scheduled_at"] <= window_to):
            continue
        title = state["title"] or (
            f"{r['ticker']} earnings" if r["ticker"] else (r["series"] or "Scheduled event")
        )
        item = {
            "id": r["id"],
            "occurrenceKey": r["occurrence_key"],
            "kind": r["kind"],
            "ticker": r["ticker"],
            "series": r["series"],
            "title": title,
            "description": state["description"],
            "scheduledAt": state["scheduled_at"],
            "timezone": state["timezone"],
            "localTime": state["local_time"],
            "status": state["status"],
            "confirmed": bool(state["confirmed"]),
            "importance": state["importance"],
            "source": state["source"],
            "sourceTier": state["source_tier"],
            "sourceUrl": state["source_url"],
            "firstSeenAt": r["first_seen_at"],
            "updatedAt": event_history[-1]["observed_at"] if event_history else (
                r["updated_at"] or r["first_seen_at"]
            ),
            "previous": state["previous"],
            "expected": state["expected"],
            "actual": state["actual"],
            "surprise": state["surprise"],
            "unit": state["unit"],
            "revisions": [revision_json(h) for h in event_history if h["change_type"] != "created"],
        }
        item["status"] = lifecycle(item)
        key = natural_key(item)
        prior = by_key.get(key)
        # A pre-0005 database may contain one row per provider.  Prefer official metadata while
        # retaining any structured values the other row uniquely supplied.
        if prior is None:
            by_key[key] = item
        else:
            primary, secondary = (
                (item, prior) if item["sourceTier"] == "official" else (prior, item)
            )
            for field in ("previous", "expected", "actual", "surprise", "unit"):
                if primary.get(field) in (None, "") and secondary.get(field) not in (None, ""):
                    primary[field] = secondary[field]
            by_key[key] = primary

    for r in macro_rows:
        item = {
            "id": r["id"],
            "occurrenceKey": None,
            "kind": "macro_release",
            "ticker": None,
            "series": r["series"],
            "title": r["series"],
            "description": "A released macro data point in the stored economic context.",
            "scheduledAt": r["release_at"],
            "timezone": "UTC",
            "localTime": "",
            "status": "released" if r["actual"] is not None else "occurred",
            "confirmed": True,
            "importance": "medium",
            "source": r["expectation_source"] or "store",
            "sourceTier": "professional" if r["expectation_source"] else "official",
            "sourceUrl": None,
            "firstSeenAt": None,
            "updatedAt": None,
            "previous": r["previous"],
            "expected": r["expected"],
            "actual": r["actual"],
            "surprise": r["surprise"],
            "unit": r["unit"],
            "revisions": [],
        }
        key = natural_key(item)
        if key not in by_key:
            by_key[key] = item
        else:
            stored = by_key[key]
            for field in ("previous", "expected", "actual", "surprise", "unit"):
                if item.get(field) not in (None, ""):
                    stored[field] = item[field]
            stored["status"] = lifecycle(stored)

    scheduled = sorted(by_key.values(), key=lambda item: (item["scheduledAt"], item["id"]))
    return {"scheduled": scheduled, "asOf": resolved_as_of, "degraded": []}
