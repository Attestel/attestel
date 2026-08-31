"""Environment configuration for the events service.

This module is the SOLE HOME (EVENT_CONTRACTS.md §9.29) of the five D-29 tier constants below.
Every later lane IMPORTS them from here and may not redeclare them anywhere else; the gate is
"these literals appear only in config.py". Two homes would make a counsel-driven narrowing a
two-line change, which is precisely what D-29 forbids.

Nothing here opens a connection. PostgreSQL connection validation happens in ``app.db.connect``.
"""
from __future__ import annotations

import os


def _int(name: str, default: int) -> int:
    """Read an int from the environment, falling back to `default` on absent or unparseable input.

    A malformed retention value must not stop the service from booting offline (invariant #1).
    """
    try:
        return int(os.getenv(name, "").strip() or default)
    except ValueError:
        return default


def _bool(name: str, default: bool) -> bool:
    raw = os.getenv(name, "").strip().lower()
    if not raw:
        return default
    return raw in ("1", "true", "yes", "on")


# ---- storage ------------------------------------------------------------------------------------

# D-26 amendment (2026-08-22): PostgreSQL is the only event-store engine. DATABASE_URL is accepted
# as a hosting-platform alias, but EVENTS_DATABASE_URL remains the explicit service contract.
EVENTS_DATABASE_URL = (
    os.getenv("EVENTS_DATABASE_URL", "").strip()
    or os.getenv("DATABASE_URL", "").strip()
)
EVENTS_DB_CONNECT_TIMEOUT = _int("EVENTS_DB_CONNECT_TIMEOUT", 10)
EVENTS_DB_STATEMENT_TIMEOUT_MS = _int("EVENTS_DB_STATEMENT_TIMEOUT_MS", 15000)

# ---- D-29 source tiers --------------------------------------------------------------------------

SOURCE_TIERS = ("official", "professional", "discussion")

# D-29 / EVENT_CONTRACTS.md §3.2 — ONE NAMED CONSTANT PER TIER, so a counsel-driven narrowing is a
# one-line change. This is the same pattern journal/evidence.go:46-51 already uses for its per-kind
# excerpt caps. Do not inline these numbers, and do not collapse the five into a loop over a table:
# the point is that each tier can be narrowed independently, on one line, without touching the
# others. `official` retention is indefinite by policy and deliberately has no knob — SEC filings
# and central-bank releases are the sources whose full body we are entitled to keep.
EXCERPT_CAP_OFFICIAL = 4000  # D-29 §3.2
EXCERPT_CAP_PROFESSIONAL = 1200  # D-29 §3.2
EXCERPT_CAP_DISCUSSION = 300  # D-29 §3.2
RETENTION_PROFESSIONAL_DAYS = _int("RETENTION_PROFESSIONAL_DAYS", 400)  # D-29 §3.2
RETENTION_DISCUSSION_DAYS = _int("RETENTION_DISCUSSION_DAYS", 90)  # D-29 §3.2

EXCERPT_CAPS = {
    "official": EXCERPT_CAP_OFFICIAL,
    "professional": EXCERPT_CAP_PROFESSIONAL,
    "discussion": EXCERPT_CAP_DISCUSSION,
}

RETENTION_DAYS = {
    "official": None,  # indefinite, by policy
    "professional": RETENTION_PROFESSIONAL_DAYS,
    "discussion": RETENTION_DISCUSSION_DAYS,
}

# ---- feature flags ------------------------------------------------------------------------------

# D-25: background LLM enrichment is OFF by default and is never triggered by a UI event
# (CLAUDE.md invariant #4). Wave 2 Lane B reads this; nothing in Wave 0 does.
EVENT_ENRICH_ENABLED = _bool("EVENT_ENRICH_ENABLED", False)

# AD-10 / §9.31: ingestion is a batch job, not a page load. OFF by default alongside enrichment.
# Wave 1 Lane C reads this; nothing in Wave 0 does.
INGEST_ENABLED = _bool("INGEST_ENABLED", False)
