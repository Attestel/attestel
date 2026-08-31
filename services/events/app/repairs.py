"""Versioned, deterministic repairs over already-ingested event data.

Forward resolver fixes do not touch canonical events that were assembled by an older deployment.
This runner applies each repair once under a PostgreSQL advisory lock and records the result in
``event_data_repairs``. It never fetches, calls a model, or rewrites source-document provenance.
"""
from __future__ import annotations

import json

from .db import Connection
from .entities import primary_ticker, resolve_tickers

MARKETAUX_HEADLINE_SUBJECT_REPAIR = "marketaux-headline-subject@1"
_LOCK_NAME = "attestel.events.data_repairs"


def _repair_marketaux_subject_links(conn: Connection) -> dict:
    events = conn.execute(
        "SELECT DISTINCT e.id, e.title FROM events e "
        "JOIN event_documents ed ON ed.event_id = e.id "
        "WHERE ed.provider = 'marketaux' ORDER BY e.id"
    ).fetchall()
    result = {
        "eventsScanned": len(events),
        "eventsRepaired": 0,
        "linksRemoved": 0,
        "primariesReassigned": 0,
        "eventsSkippedMissingSources": 0,
    }

    for event in events:
        sources = conn.execute(
            "SELECT ed.document_id, d.id AS source_id, d.provider, d.title, d.raw_tickers "
            "FROM event_documents ed "
            "LEFT JOIN source_documents d ON d.id = ed.document_id "
            "WHERE ed.event_id = ? ORDER BY ed.published_at, ed.document_id",
            (event["id"],),
        ).fetchall()
        # Retention deliberately allows source_documents to disappear while attribution survives.
        # Without the original title/entity row the stricter rule cannot be replayed, so leave that
        # event untouched rather than guessing from the denormalised event title alone.
        if not sources or any(row["source_id"] is None for row in sources):
            result["eventsSkippedMissingSources"] += 1
            continue

        allowed: set[str] = set()
        title_primary: str | None = None
        for source in sources:
            resolved = resolve_tickers(source)
            allowed.update(row.ticker for row in resolved)
            if title_primary is None and source["title"] == event["title"]:
                title_primary = primary_ticker(resolved)

        existing = conn.execute(
            "SELECT ticker, is_primary FROM event_tickers WHERE event_id = ? ORDER BY ticker",
            (event["id"],),
        ).fetchall()
        unsupported = [row for row in existing if row["ticker"] not in allowed]
        if not unsupported:
            continue

        removed_primary = any(bool(row["is_primary"]) for row in unsupported)
        for row in unsupported:
            conn.execute(
                "DELETE FROM event_tickers WHERE event_id = ? AND ticker = ?",
                (event["id"], row["ticker"]),
            )
        result["eventsRepaired"] += 1
        result["linksRemoved"] += len(unsupported)

        # Usually a noisy body mention is only a secondary link. If an older resolver made it the
        # primary, promote only the company resolved from the document that authored the event's
        # title; otherwise leave the event companyless rather than inventing a replacement.
        if removed_primary and title_primary in allowed:
            conn.execute(
                "UPDATE event_tickers SET is_primary = 0 WHERE event_id = ?",
                (event["id"],),
            )
            updated = conn.execute(
                "UPDATE event_tickers SET is_primary = 1, relevance = 1.0 "
                "WHERE event_id = ? AND ticker = ?",
                (event["id"], title_primary),
            )
            if updated.rowcount > 0:
                result["primariesReassigned"] += 1

    return result


def apply_data_repairs(conn: Connection) -> dict:
    """Apply every pending repair once; return the Marketaux repair's durable result."""
    with conn.transaction():
        conn.execute("SELECT pg_advisory_xact_lock(hashtext(?))", (_LOCK_NAME,))
        stored = conn.execute(
            "SELECT details FROM event_data_repairs WHERE version = ?",
            (MARKETAUX_HEADLINE_SUBJECT_REPAIR,),
        ).fetchone()
        if stored is not None:
            details = json.loads(stored["details"] or "{}")
            return {"applied": False, **details}

        details = _repair_marketaux_subject_links(conn)
        conn.execute(
            "INSERT INTO event_data_repairs (version, applied_at, details) "
            "VALUES (?, NOW(), ?)",
            (MARKETAUX_HEADLINE_SUBJECT_REPAIR, json.dumps(details, sort_keys=True)),
        )
        return {"applied": True, **details}


def data_repair_status(conn: Connection) -> list[dict]:
    """Credential-free durable repair provenance for the service health response."""
    rows = conn.execute(
        "SELECT version, applied_at, details FROM event_data_repairs ORDER BY version"
    ).fetchall()
    return [{
        "version": row["version"],
        "appliedAt": row["applied_at"],
        "details": json.loads(row["details"] or "{}"),
    } for row in rows]
