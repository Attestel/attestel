"""The ingestion scheduler and its run records (contract §3.11, §9.27, §9.31, AD-10).

**One scheduler for every provider.** Not one loop per provider: a single pass walks the
provider list, takes a budget reservation, fetches, normalises, inserts, sweeps and accumulates
counters. Every document stores the `ingest_run_id` of the pass that created it, so any row in the
corpus can be traced back to the fetch that produced it.

The rules this module exists to hold:

* **Ingestion has exactly one entry point.** `GET /documents`, `GET /events`, `GET /macro` and
  `GET /calendar` are reads and they read; only this module writes. No page load, quote poll, alert
  poll or read endpoint may cause a provider call, and a past `as_of` forbids a fetch outright
  (§1.3).
* **`POST /ingest` is a manual trigger and the UI never calls it.** Its callers are the Wave 0
  `Makefile` targets `ingest` / `ingest-once` and an operator.
* **The scheduled pass is OFF unless `INGEST_ENABLED` is true** (§9.31), and the flag is imported
  from `config.py` — this lane reads the name, it does not choose it. Default false, so the stack
  still comes up clean with no keys and no network (CLAUDE.md invariant #1).
* **No `while True`, no `time.sleep`.** The enabled pass drains one batch and returns, exactly as
  §9.22 requires of the enrichment worker. Repetition is the operator's cron, outside the codebase.
* **No model call and no import from `services.llm`.** Enrichment is Wave 2 Lane 2B, under the
  D-25 lease. `tests/test_no_llm.py` asserts this against the source of every module in this
  package.
"""
from __future__ import annotations

import hashlib
import json
import os
import re
import secrets
from contextlib import asynccontextmanager, contextmanager
from datetime import datetime, timedelta, timezone

import requests
from fastapi import APIRouter, Body, Query

from .db import Connection, DatabaseError, connect

from . import events as events_module
from . import ir_registry
from . import planned_events
from . import relationships
from .budget import degraded as degraded_string
from .config import INGEST_ENABLED
from .documents import run_retention_sweep, store_documents, utc_now
from .entities import scheduled_precedence, tier_for_provider
from .events import MAX_LIMIT
from .macro import record_macro_release
from .providers import ALL_PROVIDERS, PER_TICKER_PROVIDERS, WINDOW_PROVIDERS

RUN_ID_PREFIX = "run_"
SCHEDULED_ID_PREFIX = "sch_"
SCHEDULED_HISTORY_ID_PREFIX = "seh_"
ID_HEX_CHARS = 16

# The baseline ticker universe for per-ticker providers. Default and scheduled passes merge this
# with journal's privacy-safe cross-user subscription set. Explicitly narrowed manual passes do not
# consult journal: an operator who asks for one ticker gets one ticker.
DEFAULT_TICKERS = ("NVDA", "GOOGL", "TSLA")
TICKER_PATTERN = re.compile(r"^[A-Z0-9][A-Z0-9.\-]{0,11}$")
JOURNAL_TICKERS_PATH = "/_internal/tickers"
JOURNAL_TIMEOUT_SECONDS = 5
SUBSCRIPTIONS_STAGE = "subscriptions"
SUBSCRIPTIONS_UNREACHABLE = "unreachable"

# The window the two calendar providers are asked for. Backwards far enough to catch a release that
# printed since the last pass, forwards far enough to populate a forward calendar.
CALENDAR_LOOKBACK_DAYS = 7
CALENDAR_LOOKAHEAD_DAYS = 120

# `events.assemble` can only fail on a malformed corpus, and a clustering failure must not lose the
# documents the pass already stored. It is reported like any other degradation instead.
ASSEMBLY_STAGE = "events"
ASSEMBLY_REASON = "assemble"

# Phase 2D's linkage step, degraded under its own name so an operator can tell a clustering failure
# from a calendar-linkage failure without reading a stack trace.
PLANNED_STAGE = "scheduled"
PLANNED_REASON = "link"

# Phase 3's relationship derivation, degraded under its own name for the same reason.
RELATIONSHIP_STAGE = "relationships"
RELATIONSHIP_REASON = "derive"


def _normalise_tickers(values) -> list[str]:
    """Upper-case, validate and de-duplicate ticker-like values, preserving first-seen order."""
    out: list[str] = []
    for value in values:
        ticker = str(value or "").strip().upper()
        if ticker and TICKER_PATTERN.fullmatch(ticker) and ticker not in out:
            out.append(ticker)
    return out


def tickers() -> list[str]:
    """The configured baseline universe, uppercased and de-duplicated, order preserved."""
    raw = os.getenv("TICKERS", "").strip()
    values = raw.split(",") if raw else DEFAULT_TICKERS
    out = _normalise_tickers(values)
    return out or list(DEFAULT_TICKERS)


def subscription_tickers() -> tuple[list[str], str | None]:
    """Journal's aggregate follow set, or a typed degradation when configured but unavailable.

    The endpoint returns symbols only: no user ids, counts or per-user groupings. An absent
    ``JOURNAL_URL`` keeps standalone/offline runs on the configured baseline without opening a
    socket. A configured but unhealthy Journal never blocks ingestion; it is recorded and the
    baseline continues.
    """
    journal_url = os.getenv("JOURNAL_URL", "").strip()
    if not journal_url:
        return [], None
    try:
        response = requests.get(
            journal_url.rstrip("/") + JOURNAL_TICKERS_PATH,
            timeout=JOURNAL_TIMEOUT_SECONDS,
        )
        response.raise_for_status()
        payload = response.json()
    except (requests.RequestException, ValueError, TypeError):
        return [], degraded_string(SUBSCRIPTIONS_STAGE, SUBSCRIPTIONS_UNREACHABLE)
    if not isinstance(payload, dict) or not isinstance(payload.get("tickers"), list):
        return [], degraded_string(SUBSCRIPTIONS_STAGE, SUBSCRIPTIONS_UNREACHABLE)
    return _normalise_tickers(payload["tickers"]), None


def ingestion_universe(explicit=None) -> tuple[list[str], list[str]]:
    """Resolve one pass's universe and any non-fatal subscription-source degradation."""
    if explicit is not None:
        return _normalise_tickers(explicit), []
    followed, marker = subscription_tickers()
    universe = _normalise_tickers([*tickers(), *followed])
    return universe, [marker] if marker else []


def new_run_id() -> str:
    """`run_` + 16 lowercase hex (§Conventions)."""
    return RUN_ID_PREFIX + secrets.token_hex(ID_HEX_CHARS // 2)


@contextmanager
def _db():
    conn = connect()
    try:
        yield conn
    finally:
        conn.close()


def _unique(values) -> list[str]:
    """Degraded strings, de-duplicated with first-seen order preserved.

    Order preserved rather than sorted: the sequence tells an operator which provider failed first,
    which is usually the one that caused the rest.
    """
    out: list[str] = []
    for value in values:
        if value and value not in out:
            out.append(value)
    return out


# =================================================================================================
# scheduled_events (§9.27)
# =================================================================================================


def scheduled_occurrence_key(row: dict) -> str:
    """Provider occurrence key, or the conservative legacy natural key.

    A provider key must identify the release period independently of its current date (for example
    an earnings fiscal period).  Providers that cannot honestly supply one keep date in the
    fallback, so two real weekly releases are never merged by proximity or guesswork.
    """
    supplied = str(row.get("occurrence_key") or "").strip()
    if supplied:
        return supplied
    return "|".join((
        str(row.get("kind") or ""),
        str(row.get("ticker") or ""),
        str(row.get("series") or ""),
        str(row.get("scheduled_at") or ""),
    ))


def scheduled_id(row: dict) -> str:
    """`sch_` + 16 hex derived from stable occurrence identity."""
    material = scheduled_occurrence_key(row)
    return SCHEDULED_ID_PREFIX + hashlib.sha256(material.encode("utf-8")).hexdigest()[:16]


def _history_id(snapshot: dict) -> str:
    material = json.dumps(snapshot, sort_keys=True, separators=(",", ":"))
    return SCHEDULED_HISTORY_ID_PREFIX + hashlib.sha256(material.encode("utf-8")).hexdigest()[:16]


def _store_scheduled_history(
    conn: Connection,
    *,
    event_id: str,
    observed_at: str,
    change_type: str,
    current: dict,
    prior: dict | None = None,
) -> None:
    snapshot = {
        "event_id": event_id,
        "observed_at": observed_at,
        "change_type": change_type,
        "prior_scheduled_at": prior.get("scheduled_at") if prior else None,
        "scheduled_at": current["scheduled_at"],
        "prior_status": prior.get("status") if prior else None,
        "status": current["status"],
        "confirmed": int(current.get("confirmed") or 0),
        "source": current["source"],
        "source_tier": current["source_tier"],
        "source_url": current.get("source_url"),
        "title": current.get("title") or "",
        "description": current.get("description") or "",
        "importance": current.get("importance") or "medium",
        "timezone": current.get("timezone") or "UTC",
        "local_time": current.get("local_time") or "",
        "previous": current.get("previous"),
        "expected": current.get("expected"),
        "actual": current.get("actual"),
        "surprise": current.get("surprise"),
        "unit": current.get("unit") or "",
    }
    conn.execute(
        "INSERT INTO scheduled_event_history "
        "(id, event_id, observed_at, change_type, prior_scheduled_at, scheduled_at, prior_status, "
        "status, confirmed, source, source_tier, source_url, title, description, importance, "
        "timezone, local_time, previous, expected, actual, surprise, unit) "
        "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT (id) DO NOTHING",
        (_history_id(snapshot),) + tuple(snapshot[name] for name in (
            "event_id", "observed_at", "change_type", "prior_scheduled_at", "scheduled_at",
            "prior_status", "status", "confirmed", "source", "source_tier", "source_url",
            "title", "description", "importance", "timezone", "local_time", "previous",
            "expected", "actual", "surprise", "unit",
        )),
    )



# -- convergence: when two sources describe the same occurrence under different keys --------------
#
# Occurrence keys converge two observations only when both sources can produce the SAME key, and
# for the two most important cases they cannot:
#
#   * a macro release — BLS keys it by reference period (`macro|CPI|2026-07`, which it publishes),
#     FRED has no period to key on and falls back to the date;
#   * an earnings date — Alpha Vantage keys it by fiscal period end, and a company's IR feed states
#     the EVENT, not the fiscal period.
#
# The second case is the one the product turns on: "use official IR information to upgrade
# tentative aggregator dates to confirmed dates". A confirmation that arrives on a DIFFERENT date
# from the estimate is exactly when this matters, so matching on `scheduled_at` is useless there.
#
# So there is one adoption rule, and it is deliberately narrow.

#: How far a company's confirmed earnings date may sit from the aggregator's estimate and still be
#: understood as the same quarter's report. A quarter is ~13 weeks; three weeks of drift is far more
#: than aggregators actually produce and far less than the gap between two consecutive quarters, so
#: it cannot merge Q3 into Q2.
EARNINGS_ADOPTION_WINDOW_DAYS = 21


def _adoptable_row(conn: Connection, incoming: dict):
    """The existing row this observation should merge onto, or None. Never merges on a guess.

    Two ways in, and both refuse when anything is ambiguous:

    1. **An exact-fact legacy row.** Same kind, ticker, series AND instant, and its occurrence key
       is still its own id (a pre-0006 row that never had a real key). Adopting it only attaches a
       stronger key; no fact changes.

    2. **A weaker source's UNCONFIRMED row for the same company and kind, nearby in time**, when
       the incoming source ranks strictly higher. This is the IR upgrade path. Every clause is
       load-bearing:
       - *unconfirmed only* — a date the company already confirmed is never re-dated by adoption;
       - *strictly higher precedence* — an aggregator can never adopt an agency's row;
       - *bounded window* — outside it, the two are different quarters and stay separate rows;
       - *exactly one candidate* — two plausible matches means we do not know which, so we adopt
         neither and insert a new row. A wrong merge silently destroys a real calendar entry;
         a duplicate is visible and fixable.
    """
    # An exact-fact match IS the same occurrence, by definition: same kind, same company, same
    # series, same instant. This is how a period-keyed BLS row and a date-keyed FRED row for one
    # CPI print converge on one calendar entry instead of appearing twice.
    exact = conn.execute(
        "SELECT * FROM scheduled_events WHERE kind = ? "
        "AND ticker IS NOT DISTINCT FROM ? AND series IS NOT DISTINCT FROM ? "
        "AND scheduled_at = ? ORDER BY id LIMIT 1",
        (incoming["kind"], incoming["ticker"], incoming["series"], incoming["scheduled_at"]),
    ).fetchone()
    if exact is not None:
        return exact

    if not incoming.get("ticker"):
        return None

    try:
        target = datetime.strptime(incoming["scheduled_at"], "%Y-%m-%dT%H:%M:%SZ").replace(
            tzinfo=timezone.utc
        )
    except (TypeError, ValueError):
        return None
    window = timedelta(days=EARNINGS_ADOPTION_WINDOW_DAYS)
    lower = (target - window).strftime("%Y-%m-%dT%H:%M:%SZ")
    upper = (target + window).strftime("%Y-%m-%dT%H:%M:%SZ")

    candidates = conn.execute(
        "SELECT * FROM scheduled_events WHERE kind = ? AND ticker = ? "
        "AND confirmed = 0 AND status NOT IN ('released','cancelled','occurred') "
        "AND scheduled_at >= ? AND scheduled_at <= ? ORDER BY id",
        (incoming["kind"], incoming["ticker"], lower, upper),
    ).fetchall()
    stronger = [
        row for row in candidates
        if scheduled_precedence(incoming["source"]) > scheduled_precedence(row["source"] or "")
    ]
    if len(stronger) != 1:
        return None
    return stronger[0]


def store_scheduled_events(conn: Connection, rows, *, now: str | None = None) -> int:
    """Upsert canonical calendar rows and return the number newly inserted.

    Identity deliberately excludes provider, so FRED and FMP observations of the same release
    converge.  The highest source tier owns descriptive metadata; a lower-tier source may fill a
    fact that the official row did not supply, but it cannot replace a known official fact.
    `first_seen_at` remains write-once for point-in-time reads.
    """
    stamp = now or utc_now()
    inserted = 0
    columns = (
        "id", "occurrence_key", "kind", "ticker", "series", "scheduled_at", "confirmed",
        "source", "first_seen_at", "title", "description", "importance", "status",
        "source_tier", "source_url", "timezone", "local_time", "previous", "expected", "actual",
        "surprise", "unit", "updated_at",
    )
    status_rank = {
        "tentative": 0, "scheduled": 1, "confirmed": 2, "occurred": 3, "released": 4,
        "cancelled": 5,
    }

    for raw in rows or []:
        row = dict(raw)
        supplied_key = bool(str(row.get("occurrence_key") or "").strip())
        occurrence_key = scheduled_occurrence_key(row)
        event_id = scheduled_id(row)
        source = str(row.get("source") or "store")
        incoming = {
            "id": event_id,
            "occurrence_key": occurrence_key,
            "kind": row.get("kind"),
            "ticker": row.get("ticker"),
            "series": row.get("series"),
            "scheduled_at": row.get("scheduled_at"),
            "confirmed": int(row.get("confirmed") or 0),
            "source": source,
            "first_seen_at": stamp,
            "title": row.get("title") or "",
            "description": row.get("description") or "",
            "importance": row.get("importance") or "medium",
            "status": row.get("status") or ("confirmed" if row.get("confirmed") else "tentative"),
            "source_tier": row.get("source_tier") or tier_for_provider(source),
            "source_url": row.get("source_url"),
            "timezone": row.get("timezone") or "UTC",
            "local_time": row.get("local_time") or "",
            "previous": row.get("previous"),
            "expected": row.get("expected"),
            "actual": row.get("actual"),
            "surprise": row.get("surprise"),
            "unit": row.get("unit") or "",
            "updated_at": stamp,
        }
        existing_row = conn.execute(
            "SELECT * FROM scheduled_events WHERE occurrence_key = ?", (occurrence_key,)
        ).fetchone()
        if existing_row is None:
            existing_row = _adoptable_row(conn, incoming)
            if existing_row is not None:
                # The adopted row keeps its PUBLIC ID — every stored reference to it stays valid.
                #
                # It takes the incoming occurrence key ONLY when that key was supplied by the
                # provider. A provider key names the release itself (a reference period, a fiscal
                # quarter) and survives a reschedule; the fallback key is built from the DATE and
                # does not. Overwriting a real key with a fallback would throw away the identity
                # that makes the next reschedule land on this row.
                if supplied_key and existing_row["occurrence_key"] != occurrence_key:
                    conn.execute(
                        "UPDATE scheduled_events SET occurrence_key = ? WHERE id = ?",
                        (occurrence_key, existing_row["id"]),
                    )
                else:
                    occurrence_key = existing_row["occurrence_key"]
                existing_row = conn.execute(
                    "SELECT * FROM scheduled_events WHERE id = ?", (existing_row["id"],)
                ).fetchone()
        if existing_row is None:
            marks = ",".join("?" for _ in columns)
            conn.execute(
                f"INSERT INTO scheduled_events ({','.join(columns)}) VALUES ({marks})",
                tuple(incoming[name] for name in columns),
            )
            _store_scheduled_history(
                conn, event_id=event_id, observed_at=stamp, change_type="created", current=incoming,
            )
            inserted += 1
            continue

        existing = dict(existing_row)
        event_id = existing["id"]
        incoming["id"] = event_id
        # PRECEDENCE, NOT TIER (Phase 2). `source_tier` has three values and cannot separate the
        # company's own investor-relations schedule from an 8-K that merely mentions the same
        # event — both are `official`, and ranking them equal let whichever arrived second
        # overwrite the other's date. `entities.scheduled_precedence` is the four-rank order the
        # Calendar actually needs: agency/company > filing > professional > secondary.
        #
        # Ties still resolve to "preferred", so a second observation from the SAME source refreshes
        # its own row — which is how a reschedule announced by the owning agency lands.
        preferred = scheduled_precedence(incoming["source"]) >= scheduled_precedence(
            existing.get("source") or ""
        )
        merged = dict(existing)
        merged["confirmed"] = max(int(existing.get("confirmed") or 0), incoming["confirmed"])
        merged["first_seen_at"] = existing["first_seen_at"]
        merged["updated_at"] = stamp
        merged["occurrence_key"] = occurrence_key

        if preferred:
            for name in (
                "source", "source_tier", "source_url", "title", "description", "importance",
                "timezone", "local_time",
            ):
                if incoming.get(name) not in (None, ""):
                    merged[name] = incoming[name]
            merged["scheduled_at"] = incoming["scheduled_at"]
        else:
            for name in ("source_url", "title", "description", "timezone", "local_time"):
                if merged.get(name) in (None, "") and incoming.get(name) not in (None, ""):
                    merged[name] = incoming[name]

        current_status = existing.get("status") or "scheduled"
        next_status = incoming.get("status") or "scheduled"
        if existing.get("actual") is not None or incoming.get("actual") is not None:
            merged["status"] = "released"
        elif current_status == "cancelled" and preferred:
            # Cancellation is not deletion. A later observation from an equal-or-stronger source
            # may reinstate the same occurrence; the cancellation remains in append-only history.
            merged["status"] = next_status
        else:
            merged["status"] = max(
                (current_status, next_status), key=lambda value: status_rank.get(value, 0)
            )
        for name in ("previous", "expected", "actual", "surprise", "unit"):
            if incoming.get(name) not in (None, "") and (preferred or merged.get(name) in (None, "")):
                merged[name] = incoming[name]

        change_type = ""
        if merged["scheduled_at"] != existing["scheduled_at"]:
            change_type = "rescheduled"
        elif merged["status"] == "cancelled" and existing.get("status") != "cancelled":
            change_type = "cancelled"
        elif merged.get("actual") is not None and existing.get("actual") is None:
            change_type = "released"
        elif (
            merged["status"] != existing.get("status")
            or merged["confirmed"] != int(existing.get("confirmed") or 0)
        ):
            change_type = "status_changed"
        elif scheduled_precedence(merged["source"]) > scheduled_precedence(
            existing.get("source") or ""
        ):
            change_type = "source_upgraded"
        elif any(
            merged.get(name) != existing.get(name)
            for name in (
                "source", "source_url", "title", "description", "importance", "timezone",
                "local_time", "previous", "expected", "actual", "surprise", "unit",
            )
        ):
            change_type = "updated"

        assignments = ",".join(f"{name} = ?" for name in columns if name != "id")
        conn.execute(
            f"UPDATE scheduled_events SET {assignments} WHERE id = ?",
            tuple(merged[name] for name in columns if name != "id") + (event_id,),
        )
        if change_type:
            _store_scheduled_history(
                conn, event_id=event_id, observed_at=stamp, change_type=change_type,
                current=merged, prior=existing,
            )
    conn.commit()
    return inserted


# =================================================================================================
# One pass
# =================================================================================================


def store_macro_releases(conn: Connection, rows, *, now: str | None = None) -> int:
    """Write `macro_events` rows from what FRED and FMP actually returned (§3.9, §9.41.4).

    `event_id` is left NULL. 1C knows the numbers; it does not know the event, because events only
    exist after Lane 2A's `assemble()` has run on the document this same pass just stored. §9.41.5
    gives 2A the backfill, keyed on the document's `macro_key`.

    THE MERGE RULE IS THE POINT OF THIS FUNCTION. A release is seen more than once: FRED announces
    the date with no numbers, FMP carries a consensus before the print and the actual after it, and
    a later pass sees the same row again. Two things must survive that:

    * **An expectation captured BEFORE the release is never overwritten by a later capture.** If it
      were, the second pass would restamp `expectation_captured_at` to a moment after `release_at`,
      §3.9's timing guard would fire, and a `surprise` that was legitimately computable would
      silently become NULL. The earliest pre-release capture wins, permanently.
    * **A known number is never replaced by a NULL.** FRED's `releases/dates` carries no values at
      all; it must not blank out what FMP already reported.

    `expectation_captured_at` is stamped HERE, from our own clock, never from the provider — it
    records when *we* could have known the consensus, which is the only reading of §3.9 that a
    replay can reproduce. A consensus first seen at or after `release_at` therefore yields no
    surprise, exactly as §9.41.6 restates.

    Returns the number of rows written or updated.
    """
    stamp = now or utc_now()
    written = 0
    for row in rows or []:
        series = str(row.get("series") or "").strip()
        release_at = str(row.get("release_at") or "").strip()
        if not series or not release_at:
            continue

        existing = conn.execute(
            "SELECT expected, previous, actual, unit, expectation_source, expectation_captured_at "
            "FROM macro_events WHERE series = ? AND release_at = ?",
            (series, release_at),
        ).fetchone()

        expected = row.get("expected")
        source = row.get("expectation_source")
        captured_at = stamp if expected is not None else None

        if existing is not None:
            prior_captured = existing["expectation_captured_at"]
            if prior_captured and prior_captured < release_at:
                # A usable pre-release capture already exists. Keep all three fields together —
                # taking the new `expected` with the old timestamp would attribute a number to a
                # moment it was not known at, which is the same lie from the other direction.
                expected = existing["expected"]
                source = existing["expectation_source"]
                captured_at = prior_captured
            elif expected is None:
                expected, source, captured_at = (
                    existing["expected"], existing["expectation_source"], prior_captured,
                )

        actual = row.get("actual") if row.get("actual") is not None else (
            existing["actual"] if existing is not None else None)
        previous = row.get("previous") if row.get("previous") is not None else (
            existing["previous"] if existing is not None else None)
        unit = row.get("unit") or (existing["unit"] if existing is not None else "") or ""

        record_macro_release(
            conn,
            event_id=None,
            series=series,
            release_at=release_at,
            actual=actual,
            expected=expected,
            previous=previous,
            unit=unit,
            expectation_source=source,
            expectation_captured_at=captured_at,
        )
        written += 1
    return written


def _stamp(moment: datetime) -> str:
    """This pass's clock as a §Conventions timestamp.

    `store_macro_releases` reads it as "when we captured the consensus", so it must come from the
    pass's own `moment` rather than from `utc_now()` — one clock per pass, and a test that pins
    `now` gets a reproducible `expectation_captured_at` instead of a wall-clock one.
    """
    return moment.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _calendar_window(now: datetime) -> tuple[str, str]:
    return (
        (now - timedelta(days=CALENDAR_LOOKBACK_DAYS)).strftime("%Y-%m-%d"),
        (now + timedelta(days=CALENDAR_LOOKAHEAD_DAYS)).strftime("%Y-%m-%d"),
    )


def run_ingest(
    conn: Connection | None = None,
    *,
    providers=None,
    tickers_=None,
    now: datetime | None = None,
) -> dict:
    """One synchronous ingestion pass. Returns `{runId, fetched, inserted, deduped, degraded}`.

    Exactly the contract §3.11 shape for `POST /ingest`, and the same dict the enabled startup pass
    produces — one code path, so a manual trigger and a scheduled pass cannot diverge.

    Fails soft at every level. A provider with no key, over budget, in backoff, or returning
    nonsense contributes a `degraded` entry and zero documents; the pass continues to the next
    provider and still writes its run record. Nothing here raises for a provider reason.
    """
    moment = now or datetime.now(timezone.utc)
    owns_connection = conn is None
    conn = conn or connect()

    requested = [p for p in (providers or ALL_PROVIDERS) if p in ALL_PROVIDERS]
    universe, universe_degraded = ingestion_universe(tickers_)
    run_id = new_run_id()
    started_at = utc_now()

    fetched = inserted = deduped = 0
    degraded: list[str] = list(universe_degraded)
    error: str | None = None

    try:
        conn.execute(
            "INSERT INTO ingest_runs (id, started_at, providers, tickers) VALUES (?,?,?,?)",
            (run_id, started_at, json.dumps(requested), json.dumps(universe)),
        )
        conn.commit()

        window_from, window_to = _calendar_window(moment)

        # ONE pass over ONE provider list. A per-ticker provider is asked once per ticker; a
        # calendar provider is asked once per pass over a date window.
        for provider in requested:
            if provider in PER_TICKER_PROVIDERS:
                fetcher = PER_TICKER_PROVIDERS[provider]
                calls = [{"ticker": ticker} for ticker in universe]
            else:
                fetcher = WINDOW_PROVIDERS[provider]
                calls = [{"from_date": window_from, "to_date": window_to, "now": moment}]

            for kwargs in calls:
                result = fetcher(conn, **kwargs)
                fetched += len(result.documents)
                degraded.extend(result.degraded)
                added, duplicated = store_documents(conn, result.documents, run_id=run_id)
                inserted += added
                deduped += duplicated
                store_scheduled_events(conn, result.scheduled)
                # §9.41.4: the numbers land at ingest, from the provider response, with a NULL
                # event_id. Before this, `macro_events` had no possible writer and `surprise` — the
                # whole point of §3.9 — was never populated at all.
                store_macro_releases(conn, result.macro, now=_stamp(moment))

        # Phase 2D. Link the future events the stored filings EXPLICITLY scheduled into the
        # canonical calendar. It reads documents this pass (and earlier passes) already stored and
        # makes NO provider call — which is why it sits here rather than inside a fetcher. Wrapped
        # for the same reason clustering is: a linkage failure must not discard the pass's work.
        try:
            planned = planned_events.link_planned_events(conn, now=moment)
            inserted += planned["linked"]
        except (DatabaseError, ValueError, KeyError) as exc:
            conn.rollback()
            degraded.append(degraded_string(PLANNED_STAGE, PLANNED_REASON))
            error = error or f"{type(exc).__name__}: {exc}"[:500]

        # Phase 3. Derive which companies each scheduled event bears on, from the event's own
        # fields and the STORED reference table — never from a model, and never from a guess. Local
        # reads and writes only.
        try:
            relationships.rebuild_relationships(conn, universe=universe, now=moment)
        except (DatabaseError, ValueError, KeyError) as exc:
            conn.rollback()
            degraded.append(degraded_string(RELATIONSHIP_STAGE, RELATIONSHIP_REASON))
            error = error or f"{type(exc).__name__}: {exc}"[:500]

        # Cluster whatever landed into canonical events (Wave 2 Lane 2A's `assemble`). Recorded in
        # the handoff as an interpretation: 2A shipped `assemble()` with no production caller and
        # this is the only place documents arrive, so without this line the corpus grows and
        # `GET /events` stays permanently empty. It is wrapped because a clustering failure must
        # not discard the documents this pass already stored.
        try:
            events_module.assemble(conn)
        except (DatabaseError, ValueError, KeyError) as exc:
            # PostgreSQL aborts the transaction after a statement error. Roll it back before the
            # pass records its degraded result and continues with retention/run bookkeeping.
            conn.rollback()
            degraded.append(degraded_string(ASSEMBLY_STAGE, ASSEMBLY_REASON))
            error = f"{type(exc).__name__}: {exc}"[:500]

        # §3.2's sweep as an explicit step in the scheduler — never on an independent timer.
        run_retention_sweep(conn, now=moment)

        degraded = _unique(degraded)
        conn.execute(
            "UPDATE ingest_runs SET finished_at = ?, fetched = ?, inserted = ?, deduped = ?, "
            "degraded = ?, error = ? WHERE id = ?",
            (utc_now(), fetched, inserted, deduped, json.dumps(degraded), error, run_id),
        )
        conn.commit()
    finally:
        if owns_connection:
            conn.close()

    return {
        "runId": run_id,
        "fetched": fetched,
        "inserted": inserted,
        "deduped": deduped,
        "degraded": degraded,
    }


# =================================================================================================
# The scheduled pass (§9.31): off by default, one pass, no loop
# =================================================================================================


def run_scheduled_pass() -> dict | None:
    """One pass at startup when `INGEST_ENABLED` is true, otherwise nothing at all.

    Deliberately a SINGLE pass that returns — there is no `while True` and no `time.sleep` in this
    module, exactly as §9.22 requires of the enrichment worker's drainer. Repetition is the
    operator's cron calling `make ingest-once`, outside the codebase. With the flag at its default
    `false`, `docker compose up` with an empty `.env` performs no provider call at all, which is
    what invariant #1 requires.
    """
    if not INGEST_ENABLED:
        return None
    return run_ingest()


@asynccontextmanager
async def _lifespan(_app):
    run_scheduled_pass()
    yield


# §9.28: every module in this service exports its router under the literal name `router`, and hands
# the integrator NO manual include_router line — `app/main.py` auto-discovers it.
router = APIRouter(lifespan=_lifespan)


# =================================================================================================
# Endpoints (contract §3.11)
# =================================================================================================


@router.post("/ingest")
def trigger_ingest(payload: dict = Body(default=None)) -> dict:
    """A MANUAL trigger. The UI never calls it; `make ingest-once` and an operator do.

    `{providers?, tickers?}` narrows the pass. Both are optional and an empty body is a full pass —
    §9.31 requires `make ingest-once` to post `{}` when both filter variables are empty rather than
    `{"providers":[""],"tickers":[""]}`, and this handler treats both spellings as "no filter"
    anyway.
    """
    body = payload if isinstance(payload, dict) else {}
    requested = [p for p in _as_list(body.get("providers")) if p]
    requested_tickers = [t for t in _as_list(body.get("tickers")) if t]
    return run_ingest(
        providers=requested or None,
        tickers_=requested_tickers or None,
    )


def _as_list(value) -> list[str]:
    if value is None:
        return []
    if isinstance(value, str):
        return [part.strip() for part in value.split(",") if part.strip()]
    if isinstance(value, (list, tuple)):
        return [str(item).strip() for item in value if str(item).strip()]
    return []


@router.get("/ir/coverage")
def ir_coverage(tickers: str | None = Query(default=None)) -> dict:
    """Which companies have an official investor-relations source configured, and which do not.

    A READ over configuration — no database, no provider, no model. It exists because "we have no
    official confirmation for this company" is a fact the product must be able to state. Without
    it, a tentative aggregator date and a company-confirmed date look identical on the screen, and
    the honest gap becomes an invisible one.

    `tickers` is an optional comma-separated list; without it the answer is the whole registry.
    """
    requested = _as_list(tickers) if tickers else []
    body = ir_registry.coverage([t.upper() for t in requested] or None)
    if not requested:
        # Without a requested set, "missing" is not answerable — there is nothing to be missing
        # FROM. Reported as the configured universe instead of as an empty list that reads as
        # "nothing is missing".
        body["missing"] = []
        body["scope"] = "registry"
    else:
        body["scope"] = "requested"
    return body


@router.get("/ingest/runs")
def list_runs(limit: int = Query(default=20, ge=1, le=MAX_LIMIT)) -> dict:
    """Recent passes, newest first. Every `source_documents` row carries one of these ids."""
    with _db() as conn:
        # PostgreSQL identity sequence is the insertion-order tiebreak when second-level clocks tie.
        rows = conn.execute(
            "SELECT * FROM ingest_runs ORDER BY started_at DESC, sequence DESC LIMIT ?", (limit,)
        ).fetchall()
    return {
        "runs": [
            {
                "id": r["id"],
                "startedAt": r["started_at"],
                "finishedAt": r["finished_at"],
                "providers": json.loads(r["providers"] or "[]"),
                "tickers": json.loads(r["tickers"] or "[]"),
                "fetched": r["fetched"],
                "inserted": r["inserted"],
                "deduped": r["deduped"],
                "degraded": json.loads(r["degraded"] or "[]"),
                "error": r["error"],
            }
            for r in rows
        ],
        "degraded": [],
    }


# Keep the combined registry mechanically tied to the two scheduler lanes.
assert set(ALL_PROVIDERS) == set(PER_TICKER_PROVIDERS) | set(WINDOW_PROVIDERS)
