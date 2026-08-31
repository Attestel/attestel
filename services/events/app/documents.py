"""The document store: the content hash, the D-29 caps, the retention sweep, `GET /documents`.

Contract §3.1 (the table), §3.2 + D-29 (excerpt caps, body policy, retention), §3.11 (the
endpoint), §1 (`as_of`), §9.10 (the `coverage` enum), §9.28 (the router is named `router`), §9.29
(the caps have one home), §9.30 (the sweep actually runs).

Three things in this module are the ones that will bite if they drift:

* **`content_hash` normalisation is pinned by a test**, because changing it silently re-duplicates
  the whole corpus: every document already stored keeps its old hash, so the next pass inserts a
  second copy of every article and every dedup counter goes to zero without anything failing.
* **`first_seen_at` is set on insert and NEVER updated.** It is the second half of §1.1's as-of
  predicate and the only thing that stops a document backfilled today from being read as if it had
  been visible at a past cutoff. A conflicting insert increments `deduped` and leaves the stored
  row completely untouched.
* **`body` is written only when `source_tier == 'official'`** (D-29). `0001_init.sql` enforces it
  in SQL — `CHECK (body IS NULL OR source_tier = 'official')` — and this module makes sure the SQL
  never has to, by dropping the body for every other tier before the INSERT is built.

**This endpoint never fetches.** Not on an empty result, not on a coverage gap, not when `as_of` is
now. Ingestion has exactly one entry point and it is `ingest.py`. Nothing in this module imports an
HTTP client.
"""
from __future__ import annotations

import hashlib
import json
import re
import secrets
import unicodedata
from contextlib import asynccontextmanager, contextmanager
from datetime import datetime, timedelta, timezone
from urllib.parse import parse_qsl, urlsplit, urlunsplit, urlencode

from fastapi import APIRouter, HTTPException, Query

# §9.29: config.py is the SOLE HOME of the five D-29 constants. They are IMPORTED, never restated —
# the five capped/retention literals must not appear anywhere in this lane's code, so that a
# counsel-driven narrowing stays the one-line change D-29 requires.
from .config import EXCERPT_CAPS, RETENTION_DAYS
from .db import Connection, DatabaseError, connect
from .events import (
    COVERAGE_INSUFFICIENT,
    COVERAGE_OK,
    DEFAULT_LIMIT,
    MAX_LIMIT,
    resolve_as_of,
)

# =================================================================================================
# Ids and clocks
# =================================================================================================

DOCUMENT_ID_PREFIX = "doc_"
ID_HEX_CHARS = 16


def new_document_id() -> str:
    """`doc_` + 16 lowercase hex (§Conventions)."""
    return DOCUMENT_ID_PREFIX + secrets.token_hex(ID_HEX_CHARS // 2)


def utc_now() -> str:
    """Fixed-width ISO 8601 UTC ending `Z`. Fixed width is what makes the SQL as-of filter correct."""
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


@contextmanager
def _db():
    conn = connect()
    try:
        yield conn
    finally:
        conn.close()


# =================================================================================================
# The content hash (contract §3.1)
# =================================================================================================
#
# NOTE FOR A LATER READER, and please do not "fix" this:
#
#   `cluster.normalise_title` is a DIFFERENT normalisation for a DIFFERENT purpose and the two must
#   not be unified. This one exists to answer "are these two rows the same document?" — it is exact,
#   it includes the URL and the body, and it deliberately does NOT strip a source prefix, because
#   "Reuters: NVIDIA beats" and "CNBC: NVIDIA beats" from two URLs are two documents. `cluster.py`'s
#   exists to answer "do these two documents describe the same occurrence?" — it strips known
#   publisher prefixes and shingles what is left, precisely so those two DO collapse into one event.
#   Making either one serve the other's purpose breaks the other's tests and, worse, its meaning.

_WHITESPACE = re.compile(r"\s+")

# Tracking parameters carry no identity: the same article shared from two campaigns is one document.
TRACKING_PARAM_PREFIXES = ("utm_",)
TRACKING_PARAMS = frozenset({"fbclid"})

HASH_SEPARATOR = "|"


def normalise_text(value: str | None) -> str:
    """NFKC, casefold, internal whitespace collapsed to single spaces, stripped.

    A missing value is the empty string — never `None`, so the separator layout is stable.
    """
    text = unicodedata.normalize("NFKC", value or "").casefold()
    return _WHITESPACE.sub(" ", text).strip()


def normalise_url(value: str | None) -> str:
    """Lowercase scheme and host, drop the fragment, drop tracking parameters, sort the rest.

    The PATH keeps its case: many CDNs serve case-sensitive paths, and lowercasing one would make
    two genuinely different documents hash the same. Only the parts that are case-insensitive by
    RFC 3986 (scheme and host) are folded.
    """
    raw = unicodedata.normalize("NFKC", value or "").strip()
    if not raw:
        return ""
    try:
        split = urlsplit(raw)
    except ValueError:
        return _WHITESPACE.sub(" ", raw)
    query = [
        (key, val)
        for key, val in parse_qsl(split.query, keep_blank_values=True)
        if key.lower() not in TRACKING_PARAMS
        and not any(key.lower().startswith(p) for p in TRACKING_PARAM_PREFIXES)
    ]
    query.sort()
    return urlunsplit((
        split.scheme.lower(),
        split.netloc.lower(),
        split.path,
        urlencode(query),
        "",  # the fragment is never part of a document's identity
    ))


def content_hash(title: str | None, url: str | None, body: str | None) -> str:
    """`sha256(normalised_title | normalised_url | normalised_body)`, hex — the exact-dedup key.

    The exact recipe, because `tests/test_documents.py::test_content_hash_recipe_is_pinned` asserts
    every clause of it and a change here re-duplicates the entire stored corpus:

    * title and body: NFKC, casefold, collapse internal whitespace, strip
    * url: lowercase scheme and host, drop the fragment, drop `utm_*` and `fbclid`, sort the
      remaining query parameters
    * a missing body is the empty string
    * the separator is a literal `|`
    """
    material = HASH_SEPARATOR.join((
        normalise_text(title),
        normalise_url(url),
        normalise_text(body),
    ))
    return hashlib.sha256(material.encode("utf-8")).hexdigest()


# =================================================================================================
# The D-29 caps (contract §3.2, §9.29)
# =================================================================================================


def excerpt_cap(source_tier: str) -> int:
    """This tier's cap, from `config.py`. An unknown tier gets the tightest cap on the table."""
    if source_tier in EXCERPT_CAPS:
        return EXCERPT_CAPS[source_tier]
    return min(EXCERPT_CAPS.values())


def cap_excerpt(text: str | None, source_tier: str) -> tuple[str, bool]:
    """Truncate to the tier's cap. Returns `(text, truncated)`.

    Mirrors `journal/evidence.go:329 truncateRunes` EXACTLY, as D-29 requires: the input is
    trimmed first (`evidence.go:439` passes `strings.TrimSpace(e.Excerpt)`), the cut is a hard
    count of code points, and there is **no ellipsis and no marker** — the boolean is the marker.
    Two excerpt policies in one product is the drift D-07/D-08 were written to prevent, so this
    function does not get "improved" with a word-boundary rule that `evidence.go` does not have.
    """
    trimmed = (text or "").strip()
    cap = excerpt_cap(source_tier)
    if len(trimmed) <= cap:
        return trimmed, False
    return trimmed[:cap], True


def storable_body(body: str | None, source_tier: str) -> str | None:
    """D-29: a full body is stored ONLY for the `official` tier. Every other tier gets NULL.

    `0001_init.sql`'s `CHECK (body IS NULL OR source_tier = 'official')` enforces this in SQL. This
    function exists so the CHECK never has to fire: a provider that hands us a body for a
    professional-tier row is not an IntegrityError, it is a body we are not entitled to keep.
    """
    if source_tier != "official":
        return None
    text = (body or "").strip()
    return text or None


# =================================================================================================
# Insert
# =================================================================================================

_COLUMNS = (
    "id", "content_hash", "provider", "source_tier", "url", "title", "excerpt", "body",
    "published_at", "first_seen_at", "retrieved_at", "raw_tickers", "ingest_run_id",
    # §9.41.3. `"{series}|{release_at ISO}"` on a macro document, NULL on every other row. It is
    # deliberately NOT part of `content_hash`: the hash identifies the document, and two providers
    # reporting the same release still produce two documents.
    "macro_key",
)


def store_documents(
    conn: Connection,
    documents,
    *,
    run_id: str,
    now: str | None = None,
) -> tuple[int, int]:
    """Insert normalised provider documents. Returns `(inserted, deduped)`.

    `first_seen_at`, `retrieved_at` and `ingest_run_id` are assigned HERE, not by the provider — a
    provider must not be able to claim we saw a document earlier than we did. The insert is guarded
    by the UNIQUE index on `content_hash`: a conflict counts as a dedup and leaves the STORED row
    untouched, so `first_seen_at` survives re-ingestion unchanged.
    """
    stamp = now or utc_now()
    inserted = 0
    deduped = 0

    for document in documents:
        tier = str(document.get("source_tier") or "professional")
        title = str(document.get("title") or "")
        url = str(document.get("url") or "")
        published_at = str(document.get("published_at") or "")
        if not published_at:
            # providers.py drops these with a degraded entry; belt and braces, because a row with
            # no published_at would be invisible to every as-of read anyway.
            continue
        body = storable_body(document.get("body"), tier)
        excerpt, _truncated = cap_excerpt(document.get("excerpt"), tier)
        raw_tickers = document.get("raw_tickers") or []
        if isinstance(raw_tickers, str):
            raw_tickers = [raw_tickers]

        # The hash is taken over the FULL body the provider gave us, not the storable one: two
        # documents that differ only in text we are not entitled to keep are still two documents,
        # and hashing the stored (NULL) body would collapse every bodyless article with the same
        # title and URL into one — which is what we want — while collapsing two official filings
        # that differ only in body into one, which we do not.
        digest = content_hash(title, url, document.get("body"))

        cursor = conn.execute(
            f"INSERT INTO source_documents ({', '.join(_COLUMNS)}) "
            f"VALUES ({', '.join('?' * len(_COLUMNS))}) "
            "ON CONFLICT (content_hash) DO NOTHING",
            (
                new_document_id(), digest, str(document.get("provider") or ""), tier, url, title,
                excerpt, body, published_at, stamp, stamp, json.dumps(list(raw_tickers)), run_id,
                (str(document["macro_key"]).strip() or None) if document.get("macro_key") else None,
            ),
        )
        if cursor.rowcount:
            inserted += 1
        else:
            deduped += 1
    conn.commit()
    return inserted, deduped


# =================================================================================================
# Retention sweep (contract §3.2, §9.30)
# =================================================================================================


def run_retention_sweep(conn: Connection | None = None, *, now: datetime | None = None) -> dict:
    """Delete document rows past their tier's retention. Returns `{tier: deleted}`.

    Three properties, all of them contractual:

    * **`official` is never swept.** `RETENTION_DAYS['official']` is `None` by policy (D-29): SEC
      filings and central-bank releases are the sources we are entitled to keep indefinitely.
    * **The clock is `first_seen_at`, not `published_at`.** Retention is how long WE have held the
      row, which is the licensing-relevant duration; a 2019 article ingested yesterday is one day
      old for this purpose, not seven years old.
    * **It deletes the document row only** (§3.2). The canonical event, its `event_documents` link
      rows and every derived score survive, because derived structure is ours and provenance may
      never silently vanish. `event_documents.document_id` carries no foreign key and no cascade
      (§9.6) precisely so that no sweep implementation *can* destroy the attribution — this
      function must not add one, and Wave 2 Lane 2A's handoff says so explicitly.

    It is a local `DELETE` and never a provider call, so running it at startup leaves invariant #1
    untouched.
    """
    moment = now or datetime.now(timezone.utc)
    owns_connection = conn is None
    conn = conn or connect()
    deleted: dict[str, int] = {}
    try:
        for tier, days in RETENTION_DAYS.items():
            if days is None:
                continue
            cutoff = (moment - timedelta(days=days)).strftime("%Y-%m-%dT%H:%M:%SZ")
            cursor = conn.execute(
                "DELETE FROM source_documents WHERE source_tier = ? AND first_seen_at < ?",
                (tier, cutoff),
            )
            deleted[tier] = cursor.rowcount if cursor.rowcount and cursor.rowcount > 0 else 0
        conn.commit()
    finally:
        if owns_connection:
            conn.close()
    return deleted


def _sweep_on_startup() -> dict:
    """§9.30's unconditional startup sweep, wrapped so a sweep failure cannot stop the service.

    Ingestion is OFF by default (`INGEST_ENABLED=false`), so without this the sweep would be
    written and never executed. It is wrapped because a service that refuses to boot because a
    DELETE failed is strictly worse than one that boots with a stale row.
    """
    try:
        return run_retention_sweep()
    except DatabaseError:
        return {}


@asynccontextmanager
async def _lifespan(_app):
    """Runs the §9.30 sweep at events-service startup.

    This is how the sweep gets wired WITHOUT touching `app/main.py`, which this lane may not edit:
    FastAPI's `include_router` merges a sub-router's lifespan into the application's, so the
    §9.28 auto-discovery in `main.py` picks this up with no integrator change. `main.py`'s own
    lifespan runs `migrate()` and it runs FIRST, so the table is guaranteed to exist here.
    """
    _sweep_on_startup()
    yield


router = APIRouter(lifespan=_lifespan)


# =================================================================================================
# GET /documents (contract §3.11)
# =================================================================================================

# `ticker` filters the native PostgreSQL JSONB array. The precise event↔ticker join remains the
# canonical relationship for assembled events; raw_tickers is the source-document view.
_TICKER_MATCH = "raw_tickers @> jsonb_build_array(:ticker::text)"


def _document_json(row) -> dict:
    document = {
        "id": row["id"],
        "provider": row["provider"],
        "sourceTier": row["source_tier"],
        "url": row["url"],
        "title": row["title"],
        "excerpt": row["excerpt"],
        "publishedAt": row["published_at"],
        "firstSeenAt": row["first_seen_at"],
        "retrievedAt": row["retrieved_at"],
        "rawTickers": json.loads(row["raw_tickers"] or "[]"),
        "ingestRunId": row["ingest_run_id"],
        "contentHash": row["content_hash"],
    }
    if row["body"] is not None:
        # Only ever populated for `official` (D-29), so this key appears only where we are
        # entitled to serve full text.
        document["body"] = row["body"]
    return document


def _coverage(conn, visible: int) -> dict:
    """§9.10's enum, plus §1.3's bounds when the store cannot serve the cutoff.

    The same shape `events.py::_coverage` returns, over `source_documents` instead of `events`, so
    the two endpoints answer "I cannot serve this cutoff" in one vocabulary.
    """
    if visible:
        return {"coverage": COVERAGE_OK}
    bounds = conn.execute(
        "SELECT MIN(published_at) AS lo, MAX(published_at) AS hi FROM source_documents"
    ).fetchone()
    return {
        "coverage": COVERAGE_INSUFFICIENT,
        "coverageFrom": bounds["lo"],
        "coverageTo": bounds["hi"],
    }


@router.get("/documents")
def list_documents(
    ticker: str | None = Query(default=None),
    provider: str | None = Query(default=None),
    since: str | None = Query(default=None),
    as_of: str | None = Query(default=None),
    limit: int = Query(default=DEFAULT_LIMIT, ge=1, le=MAX_LIMIT),
) -> dict:
    """Contract §3.11. Served from the STORE ONLY — this endpoint never causes a provider call.

    The as-of filter is §1.1's, in SQL, both halves: `published_at <= :as_of AND
    first_seen_at <= :as_of`. A Python-side filter over a loaded result set is one refactor away
    from leaking a future row, and `first_seen_at` is the half that stops a document backfilled
    today from being read as if it had been visible at a past cutoff.
    """
    resolved_as_of, _historical = resolve_as_of(as_of)

    where = ["published_at <= :as_of", "first_seen_at <= :as_of"]
    params: dict[str, object] = {"as_of": resolved_as_of, "limit": limit}

    if ticker:
        params["ticker"] = ticker.strip().upper()
        where.append(_TICKER_MATCH)
    if provider:
        params["provider"] = provider.strip().lower()
        where.append("provider = :provider")
    if since:
        params["since"] = _iso_or_bad_request(since, "since")
        where.append("published_at >= :since")

    with _db() as conn:
        rows = conn.execute(
            f"SELECT * FROM source_documents WHERE {' AND '.join(where)} "
            f"ORDER BY published_at DESC, id DESC LIMIT :limit",
            params,
        ).fetchall()
        visible = conn.execute(
            "SELECT COUNT(*) AS n FROM source_documents "
            "WHERE published_at <= :as_of AND first_seen_at <= :as_of",
            {"as_of": resolved_as_of},
        ).fetchone()["n"]
        coverage = _coverage(conn, visible)

    return {
        "documents": [_document_json(r) for r in rows],
        "asOf": resolved_as_of,
        **coverage,
        "degraded": [],
    }


def _iso_or_bad_request(value: str, field: str) -> str:
    """Normalise a caller-supplied timestamp, or raise a bad-request naming the field."""
    from .cluster import iso  # local import: cluster is 2A's and has no dependency on this module

    try:
        return iso(value)
    except ValueError:
        raise HTTPException(
            status_code=400, detail=f"{field} is not an ISO 8601 timestamp: {value}"
        )
