"""Canonical events: assembly, deterministic scores, the read endpoints, the enrichment intake.

Contract §3.3, §3.6, §3.11, §3.12, plus binding resolutions §9.9, §9.12, §9.13, §9.14, §9.15.

Two rules run through everything here:

* **`as_of` is enforced in SQL** (§1.1) — `WHERE published_at <= :as_of AND first_seen_at <= :as_of`,
  both halves, every read. A Python-side filter over a loaded result set is one refactor away from
  leaking a future row.
* **`as_of` controls row visibility, never field values** (§9.13). An event visible at cutoff T must
  show the importance, documentCount, sourceTier and officialConfirmed it had at T, not the ones it
  has now after later documents joined its cluster. That is what `event_state_history` is for.

Nothing in this module imports an HTTP client or an LLM. `why_it_matters` and every
`event_tickers.potential_*` column are hypotheses that arrive only through
`POST /events/{id}/enrichment`; no code path here writes one.
"""
from __future__ import annotations

import base64
import binascii
import hashlib
import json
import re
import unicodedata
from contextlib import contextmanager
from datetime import datetime, timedelta, timezone
from math import log1p, sqrt

from fastapi import APIRouter, Body, HTTPException, Query

from .db import Row

from .cluster import (
    CLUSTER_JACCARD_MIN,
    CLUSTER_VERSION,
    bands_collide,
    cluster_key,
    date_bucket,
    estimated_jaccard,
    iso,
    minhash_signature,
    parse_iso,
    shingle_counts,
    shingles,
)
from .db import connect
from .entities import (
    EVENT_TYPES,
    RELEVANCE_PRIMARY,
    TIER_RANK,
    ResolvedTicker,
    guess_event_type,
    highest_tier,
    official_confirmed,
    primary_ticker,
    resolve_tickers,
    tier_for_provider,
)

router = APIRouter()

# =================================================================================================
# Deterministic scores (contract §3.6)
#
#   importance = 0.35·tierWeight + 0.25·documentCountSaturation + 0.20·entityMarketCapWeight
#              + 0.20·eventTypePrior
#   novelty    = 1 − maxCosineSimilarity(title_shingles, same_ticker_events_last_30d)
#
# Every constant below is covered by SCORE_VERSION. Changing any of them bumps it — the scores are
# an input to Wave 5A's ablation ladder, and a silently retuned weight makes two rungs
# incomparable.
# =================================================================================================

SCORE_VERSION = "score@1"

IMPORTANCE_W_TIER = 0.35
IMPORTANCE_W_DOCUMENT_COUNT = 0.25
IMPORTANCE_W_ENTITY = 0.20
IMPORTANCE_W_EVENT_TYPE = 0.20

TIER_WEIGHT = {"official": 1.0, "professional": 0.6, "discussion": 0.25}

# Volume must saturate: five rewrites of one press release are not five times as important, and
# without a cap a wire story syndicated forty times would outrank an 8-K.
DOC_COUNT_SATURATION_N = 8

# There is NO market-cap field anywhere in this repo and this lane may not fetch one (invariant #1
# — the stack runs with zero keys and zero network). This static table is a stand-in for the real
# term, not the real term: it is a rough size/liquidity prior over the tickers the app actually
# covers, frozen at SCORE_VERSION. A real market-cap source would replace this table wholesale and
# would be a SCORE_VERSION bump, not a value edit.
ENTITY_WEIGHTS = {
    "AAPL": 1.0, "MSFT": 1.0, "NVDA": 1.0, "GOOGL": 0.95, "AMZN": 0.95,
    "META": 0.90, "AVGO": 0.85, "TSLA": 0.85, "TSM": 0.80, "AMD": 0.70, "INTC": 0.60,
    "SPY": 0.50, "SMH": 0.50, "XLK": 0.50,
}
# Also used for a market-wide event with no subject company (a macro release touches everything and
# nothing in particular).
ENTITY_WEIGHT_DEFAULT = 0.40

# A prior, not a distribution: these do not sum to anything in particular. They express how much a
# type of occurrence tends to matter before any other evidence is read.
EVENT_TYPE_PRIOR = {
    "earnings_result": 0.95,
    "earnings_guidance": 0.90,
    "regulatory_action": 0.85,
    "ma_transaction": 0.85,
    "central_bank": 0.80,
    "macro_release": 0.75,
    "supply_chain": 0.70,
    "legal_action": 0.65,
    "management_change": 0.60,
    "product_launch": 0.60,
    "capital_return": 0.55,
    "analyst_revision": 0.50,
    "partnership": 0.50,
    "financing": 0.45,
    "sector_event": 0.40,
    "other": 0.25,
}
assert set(EVENT_TYPE_PRIOR) == set(EVENT_TYPES)

NOVELTY_WINDOW_DAYS = 30

# ---- extraction caps -----------------------------------------------------------------------------

# `summary` and the code-authored `key_facts` are VERBATIM extracts from a supporting document, so
# every one of them is traceable by construction and passes the same §9.14 verifier the model's
# proposals must pass.
SUMMARY_CHAR_CAP = 400
KEY_FACT_CHAR_CAP = 300
KEY_FACT_MAX = 3

# §9.14 verification. A fact must be long enough not to be trivially fabricated (transcript.py uses
# the same 20-character floor for evasive-moment quotes) and every n-gram of it must appear in the
# supporting documents' text.
KEY_FACT_NGRAM_N = 5
KEY_FACT_MIN_CHARS = 20

# ---- endpoint defaults ---------------------------------------------------------------------------

DEFAULT_LIMIT = 50
MAX_LIMIT = 200

COVERAGE_OK = "ok"
COVERAGE_INSUFFICIENT = "insufficient"

DIRECTIONS = ("positive", "negative", "neutral", "unclear")
HORIZONS = ("intraday", "short_term", "medium_term", "long_term")
AFFECTED_DIMENSIONS = (
    "revenue_exposure", "margin", "growth", "regulation", "management", "litigation",
    "valuation", "demand", "supply", "rates", "currency",
)

# Deterministic, versioned and code's — a model may not author any of them (§9.15). `relevance` is
# in this set deliberately: Wave 5B thresholds notifications on it.
DETERMINISTIC_FIELDS = (
    "importance", "novelty", "relevance", "clusterKey", "cluster_key",
    "occurredAt", "occurred_at", "publishedAt", "published_at",
    "firstSeenAt", "first_seen_at", "sourceTier", "source_tier",
    "officialConfirmed", "official_confirmed", "documentCount", "document_count",
)

# The per-ticker hypothesis columns. Absent from a historical read whose cutoff predates the
# enrichment (§9.12).
POTENTIAL_FIELDS = (
    "potentialDirection", "potentialStrength", "expectedHorizon",
    "affectedDimensions", "transmission",
)


# =================================================================================================
# Scores
# =================================================================================================


def tier_weight(tier: str) -> float:
    return TIER_WEIGHT.get(tier, TIER_WEIGHT["professional"])


def document_count_saturation(count: int) -> float:
    """A saturating function of document count, capped at 1.0 by `DOC_COUNT_SATURATION_N`."""
    if count <= 0:
        return 0.0
    return min(1.0, log1p(count) / log1p(DOC_COUNT_SATURATION_N))


def entity_weight(ticker: str | None) -> float:
    return ENTITY_WEIGHTS.get(ticker or "", ENTITY_WEIGHT_DEFAULT)


def event_type_prior(event_type: str) -> float:
    return EVENT_TYPE_PRIOR.get(event_type, EVENT_TYPE_PRIOR["other"])


def compute_importance(*, tier: str, document_count: int, ticker: str | None,
                       event_type: str) -> float:
    """Contract §3.6. Deterministic, versioned by SCORE_VERSION, never model-authored."""
    return (
        IMPORTANCE_W_TIER * tier_weight(tier)
        + IMPORTANCE_W_DOCUMENT_COUNT * document_count_saturation(document_count)
        + IMPORTANCE_W_ENTITY * entity_weight(ticker)
        + IMPORTANCE_W_EVENT_TYPE * event_type_prior(event_type)
    )


def cosine_similarity(a, b) -> float:
    """Cosine over shingle count vectors."""
    if not a or not b:
        return 0.0
    shared = set(a) & set(b)
    if not shared:
        return 0.0
    dot = sum(a[s] * b[s] for s in shared)
    norm = sqrt(sum(v * v for v in a.values())) * sqrt(sum(v * v for v in b.values()))
    return dot / norm if norm else 0.0


def compute_novelty(conn, *, ticker: str | None, published_at: str, title: str) -> float:
    """`1 − maxCosineSimilarity(title_shingles, same_ticker_events_last_30d)`.

    Compared ONLY against events whose `published_at` is at or before this one's. Comparing against
    the future would make novelty depend on when the batch happened to run: a backlog ingested
    today would score its own oldest event against events that had not happened yet.
    """
    if not ticker:
        return 1.0
    window_start = iso(parse_iso(published_at) - timedelta(days=NOVELTY_WINDOW_DAYS))
    rows = conn.execute(
        "SELECT e.title FROM events e "
        "JOIN event_tickers t ON t.event_id = e.id AND t.is_primary = 1 "
        "WHERE t.ticker = ? AND e.published_at <= ? AND e.published_at >= ? "
        "ORDER BY e.id",
        (ticker, published_at, window_start),
    ).fetchall()
    if not rows:
        return 1.0  # a ticker's first event in the window is wholly new
    mine = shingle_counts(title)
    best = max(cosine_similarity(mine, shingle_counts(r["title"])) for r in rows)
    return 1.0 - best


# =================================================================================================
# Extraction (facts, never interpretation)
# =================================================================================================

_SENTENCE_SPLIT = re.compile(r"(?<=[.!?])\s+")
_VERIFY_PUNCT = re.compile(r"[^a-z0-9 ]+")
_VERIFY_WS = re.compile(r"\s+")


def normalise_for_verification(text: str) -> str:
    """NFKC + casefold + punctuation-stripped + whitespace-collapsed, for n-gram containment."""
    folded = unicodedata.normalize("NFKC", text or "").casefold()
    return _VERIFY_WS.sub(" ", _VERIFY_PUNCT.sub(" ", folded)).strip()


def _document_text(document) -> str:
    """Everything a supporting document can vouch for: title, excerpt and (official only) body."""
    parts = [document["title"] or "", document["excerpt"] or ""]
    try:
        parts.append(document["body"] or "")
    except (IndexError, KeyError):
        pass
    return " ".join(p for p in parts if p)


def verify_key_fact(fact: str, corpus: str) -> bool:
    """§9.14: a `key_facts` string must be traceable to a supporting document.

    Normalised n-gram containment. This is the pattern `services/llm/app/transcript.py` already
    uses to ground evasive-moment quotes, restated for a fact column: without it invariant #3 ("the
    LLM never invents a number") has a hole the size of the event store, because `key_facts` is
    rendered without interpretive tint and is fed to the ablation ladder's rung D.
    """
    normalised = normalise_for_verification(fact)
    if len(normalised) < KEY_FACT_MIN_CHARS:
        return False
    haystack = f" {corpus} "
    words = normalised.split()
    if len(words) <= KEY_FACT_NGRAM_N:
        return f" {normalised} " in haystack
    return all(
        f" {' '.join(words[i:i + KEY_FACT_NGRAM_N])} " in haystack
        for i in range(len(words) - KEY_FACT_NGRAM_N + 1)
    )


def _extract_summary(document) -> str:
    """A bounded, VERBATIM factual extract. Never a paraphrase — this is a fact column."""
    source = (document["excerpt"] or "").strip() or (document["title"] or "").strip()
    try:
        source = (document["body"] or "").strip() or source
    except (IndexError, KeyError):
        pass
    if len(source) <= SUMMARY_CHAR_CAP:
        return source
    clipped = source[:SUMMARY_CHAR_CAP]
    cut = clipped.rfind(" ")
    return (clipped[:cut] if cut > 0 else clipped).rstrip()


def _extract_key_facts(document) -> list[str]:
    """Verbatim sentences from the supporting document, so every one is traceable by construction."""
    source = (document["excerpt"] or "").strip()
    try:
        source = (document["body"] or "").strip() or source
    except (IndexError, KeyError):
        pass
    facts: list[str] = []
    for sentence in _SENTENCE_SPLIT.split(source):
        candidate = sentence.strip()[:KEY_FACT_CHAR_CAP].strip()
        if len(normalise_for_verification(candidate)) >= KEY_FACT_MIN_CHARS \
                and candidate not in facts:
            facts.append(candidate)
        if len(facts) >= KEY_FACT_MAX:
            break
    return facts


# =================================================================================================
# Assembly
# =================================================================================================


def _event_id(key: str, document_id: str) -> str:
    """Content-derived, so two runs over the same documents mint the same id (Phase 7)."""
    material = f"{CLUSTER_VERSION}|{key}|{document_id}".encode("utf-8")
    return "evt_" + hashlib.sha256(material).hexdigest()[:16]


def _document_tier(document) -> str:
    stored = (document["source_tier"] or "").strip()
    return stored if stored in TIER_WEIGHT else tier_for_provider(document["provider"])


def _record_state(conn, event_id: str, *, as_of: str, importance: float, novelty: float,
                  document_count: int, source_tier: str, confirmed: int) -> None:
    """Append the state that became true at `as_of` (§9.13).

    `as_of` is the triggering document's `first_seen_at` — when the store actually learned this —
    and never the wall clock, so rebuilding the database from the same documents rebuilds the same
    history. The upsert is required because two documents from one ingest run can share a
    `first_seen_at`: the state at that instant is the one after both were applied.
    """
    conn.execute(
        "INSERT INTO event_state_history "
        "(event_id, as_of, importance, novelty, document_count, source_tier, official_confirmed) "
        "VALUES (?,?,?,?,?,?,?) ON CONFLICT (event_id, as_of) DO UPDATE SET "
        "importance = EXCLUDED.importance, novelty = EXCLUDED.novelty, "
        "document_count = EXCLUDED.document_count, source_tier = EXCLUDED.source_tier, "
        "official_confirmed = EXCLUDED.official_confirmed",
        (event_id, as_of, importance, novelty, document_count, source_tier, confirmed),
    )


def _link_document(conn, event_id: str, document, tier: str) -> None:
    """§3.7's deliberate denormalisation: url/provider/source_tier/published_at are COPIED here.

    §3.2's retention sweep deletes the `source_documents` row; the event must keep its attribution.
    Every read of `sources[]` comes from this table, never from a join back to the document.
    """
    conn.execute(
        "INSERT INTO event_documents "
        "(event_id, document_id, url, provider, source_tier, published_at) VALUES (?,?,?,?,?,?) "
        "ON CONFLICT (event_id, document_id) DO NOTHING",
        (event_id, document["id"], document["url"], document["provider"], tier,
         iso(document["published_at"])),
    )


def _link_tickers(conn, event_id: str, resolved: list[ResolvedTicker], primary: str | None) -> None:
    """Write-once (§9.15): conflicts never rewrite a relevance a later document disagrees
    with, and never demotes or promotes an existing primary."""
    for entry in resolved:
        is_primary = 1 if entry.ticker == primary else 0
        relevance = RELEVANCE_PRIMARY if is_primary else entry.relevance
        conn.execute(
            "INSERT INTO event_tickers (event_id, ticker, relevance, is_primary, "
            "affected_dimensions) VALUES (?,?,?,?,'[]') "
            "ON CONFLICT (event_id, ticker) DO NOTHING",
            (event_id, entry.ticker, relevance, is_primary),
        )


def _candidate_events(conn, *, event_type: str, day: str, primary: str) -> list[Row]:
    """Events sharing (primary ticker set, event type, UTC day) — §3.5's bucket, ordered by id."""
    return conn.execute(
        "SELECT * FROM events e WHERE e.event_type = ? AND e.occurred_at::date = ?::date "
        "AND COALESCE((SELECT t.ticker FROM event_tickers t "
        "              WHERE t.event_id = e.id AND t.is_primary = 1), '') = ? "
        "ORDER BY e.id",
        (event_type, day, primary),
    ).fetchall()


def assemble(conn) -> list[str]:
    """Cluster every unlinked document into events. Returns the ids created, in creation order.

    Documents are processed in `(published_at, first_seen_at, id)` order regardless of the order
    they come back in, so shuffling the input cannot change the result. That total sort is what
    makes the whole pipeline order-independent — see `tests/test_cluster.py`.
    """
    documents = conn.execute(
        "SELECT * FROM source_documents d "
        "WHERE NOT EXISTS (SELECT 1 FROM event_documents ed WHERE ed.document_id = d.id) "
        "ORDER BY published_at, first_seen_at, id"
    ).fetchall()

    created: list[str] = []
    for document in documents:
        resolved = resolve_tickers(document)
        primary = primary_ticker(resolved)
        event_type = guess_event_type(document)
        tier = _document_tier(document)
        # source_documents carries no separate filing/release timestamp, so the document's
        # published_at IS its occurrence time. When 1C adds one, read it here.
        occurred_at = iso(document["published_at"])
        published_at = occurred_at
        first_seen_at = iso(document["first_seen_at"])
        title_shingles = shingles(document["title"])
        signature = minhash_signature(title_shingles)
        key = cluster_key(primary or "", event_type, occurred_at, title_shingles)

        joined = None
        for candidate in _candidate_events(
            conn, event_type=event_type, day=date_bucket(occurred_at), primary=primary or ""
        ):
            candidate_signature = minhash_signature(shingles(candidate["title"]))
            # Banding proposes; the threshold disposes.
            if not bands_collide(signature, candidate_signature):
                continue
            if estimated_jaccard(signature, candidate_signature) >= CLUSTER_JACCARD_MIN:
                joined = candidate
                break

        if joined is None:
            created.append(_create_event(
                conn, document, resolved=resolved, primary=primary, event_type=event_type,
                tier=tier, key=key, occurred_at=occurred_at, published_at=published_at,
                first_seen_at=first_seen_at,
            ))
        else:
            _join_event(
                conn, joined, document, resolved=resolved, primary=primary, tier=tier,
                published_at=published_at, first_seen_at=first_seen_at,
            )

    _reconcile_macro_events(conn)
    conn.commit()
    return created


def _reconcile_macro_events(conn) -> int:
    """§9.41.5 — link each release row to the event its document landed in. Returns rows linked.

    1C writes `macro_events` at ingest from the FRED and FMP responses with `event_id` NULL: it has
    the numbers but not the event, because events only exist once `assemble()` has run on the
    document that same pass stored. The document carries `macro_key` — `"{series}|{release_at ISO}"`,
    built in the one place, `providers.macro_key` — and §9.41.2 makes `(series, release_at)` the
    natural key of the row, so the link needs no second convention.

    Only `event_id` is written. `actual`, `expected`, `previous` and `surprise` stay 1C's, sourced
    from the structured provider responses; re-deriving them from assembled article text would put
    a model-touchable number into a fact column (§9.41.4).

    THIS RUNS OVER THE WHOLE STORE, NOT JUST THIS PASS'S DOCUMENTS, AND THAT IS THE POINT.
    `macro.record_macro_release` upserts the natural key with `event_id=None`, so
    the routine re-ingest its own docstring describes — "a later pass sees the same row again" —
    resets the link to NULL. A backfill that only saw newly-clustered documents could not repair
    that, because `assemble()` never revisits a document already in `event_documents`: the link
    would survive exactly one ingest pass and then be lost permanently, silently taking rung F with
    it. Reconciling the whole store makes the link self-healing instead, and `ingest.py` runs
    `store_macro_releases` before `assemble()` in the same pass, so the wipe and the repair happen
    together. Idempotent: re-running changes nothing once every linkable row is linked.
    """
    linked = 0
    # `macro_key` arrives with migration 0002; tolerate a row shape that predates it.
    if not _has_column(conn, "source_documents", "macro_key"):
        return linked
    # Ordered by the same total sort `assemble()` uses, so where two documents share one key the
    # earliest wins and the outcome does not depend on row order — see `tests/test_cluster.py`.
    rows = conn.execute(
        "SELECT d.macro_key AS macro_key, ed.event_id AS event_id "
        "FROM source_documents d JOIN event_documents ed ON ed.document_id = d.id "
        "WHERE d.macro_key IS NOT NULL AND d.macro_key != '' "
        "ORDER BY d.published_at, d.first_seen_at, d.id"
    ).fetchall()

    seen: set[str] = set()
    for row in rows:
        raw = str(row["macro_key"])
        if raw in seen:
            continue
        seen.add(raw)
        series, separator, release_at = raw.partition("|")
        if not separator or not series or not release_at:
            continue
        try:
            # `record_macro_release` stored `iso(release_at)`; match what is actually in the column
            # rather than the raw half of the key, so provider-side formatting cannot break the join.
            normalised = iso(release_at)
        except (ValueError, TypeError):
            # An unparseable key links nothing. It must not take the rest of the pass down with it.
            continue
        cursor = conn.execute(
            "UPDATE macro_events SET event_id = :event_id "
            "WHERE series = :series AND release_at = :release_at "
            "AND (event_id IS NULL OR event_id != :event_id)",
            {"event_id": row["event_id"], "series": series, "release_at": normalised},
        )
        linked += cursor.rowcount if cursor.rowcount > 0 else 0
    return linked


def _has_column(conn, table: str, column: str) -> bool:
    return conn.execute(
        "SELECT 1 AS present FROM information_schema.columns "
        "WHERE table_schema = current_schema() AND table_name = ? AND column_name = ?",
        (table, column),
    ).fetchone() is not None


def _create_event(conn, document, *, resolved, primary, event_type, tier, key, occurred_at,
                  published_at, first_seen_at) -> str:
    event_id = _event_id(key, document["id"])
    importance = compute_importance(
        tier=tier, document_count=1, ticker=primary, event_type=event_type
    )
    novelty = compute_novelty(
        conn, ticker=primary, published_at=published_at, title=document["title"]
    )
    confirmed = official_confirmed([tier])
    conn.execute(
        "INSERT INTO events (id, event_type, title, summary, key_facts, occurred_at, published_at, "
        "first_seen_at, source_tier, official_confirmed, importance, novelty, document_count, "
        "cluster_key, enrichment_state) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,'raw')",
        (event_id, event_type, document["title"], _extract_summary(document),
         json.dumps(_extract_key_facts(document)), occurred_at, published_at, first_seen_at,
         tier, confirmed, importance, novelty, 1, key),
    )
    _link_document(conn, event_id, document, tier)
    _link_tickers(conn, event_id, resolved, primary)
    _record_state(conn, event_id, as_of=first_seen_at, importance=importance, novelty=novelty,
                  document_count=1, source_tier=tier, confirmed=confirmed)
    return event_id


def _join_event(conn, event, document, *, resolved, primary, tier, published_at,
                first_seen_at) -> None:
    """`cluster_key` is NEVER mutated after creation — §9.7 allows two events to share one."""
    event_id = event["id"]
    _link_document(conn, event_id, document, tier)
    _link_tickers(conn, event_id, resolved, None)  # the primary is settled at creation

    links = conn.execute(
        "SELECT source_tier FROM event_documents WHERE event_id = ?", (event_id,)
    ).fetchall()
    document_count = len(links)
    tiers = [r["source_tier"] for r in links]
    new_tier = highest_tier(tiers)
    confirmed = official_confirmed(tiers)

    title, summary, key_facts = event["title"], event["summary"], event["key_facts"]
    if TIER_RANK.get(tier, 0) > TIER_RANK.get(event["source_tier"], 0):
        # "title from the highest-tier, earliest document": a higher tier outranks an earlier one.
        title = document["title"]
        summary = _extract_summary(document)
        key_facts = json.dumps(_merge_key_facts(
            json.loads(event["key_facts"] or "[]"), _extract_key_facts(document)
        ))

    earliest_published = min(event["published_at"], published_at)
    earliest_first_seen = min(event["first_seen_at"], first_seen_at)
    importance = compute_importance(
        tier=new_tier, document_count=document_count, ticker=primary,
        event_type=event["event_type"],
    )
    # novelty is a property of the moment the event first appeared and is not recomputed on join.
    novelty = event["novelty"]

    conn.execute(
        "UPDATE events SET title = ?, summary = ?, key_facts = ?, published_at = ?, "
        "first_seen_at = ?, source_tier = ?, official_confirmed = ?, importance = ?, "
        "document_count = ? WHERE id = ?",
        (title, summary, key_facts, earliest_published, earliest_first_seen, new_tier,
         confirmed, importance, document_count, event_id),
    )
    _record_state(conn, event_id, as_of=first_seen_at, importance=importance, novelty=novelty,
                  document_count=document_count, source_tier=new_tier, confirmed=confirmed)


def _merge_key_facts(existing: list[str], incoming: list[str]) -> list[str]:
    """Append and deduplicate, preserving order. Deduplication is on the NORMALISED form so two
    spellings of one sentence do not both survive."""
    merged = list(existing)
    seen = {normalise_for_verification(f) for f in merged}
    for fact in incoming:
        norm = normalise_for_verification(fact)
        if norm and norm not in seen:
            merged.append(fact)
            seen.add(norm)
    return merged


# =================================================================================================
# Reads
# =================================================================================================


@contextmanager
def _db():
    conn = connect()
    try:
        yield conn
    finally:
        conn.close()


def resolve_as_of(as_of: str | None) -> tuple[str, bool]:
    """Returns (resolved asOf, historical). Absent ⇒ now, which is never historical."""
    now = datetime.now(timezone.utc)
    if not as_of:
        return iso(now), False
    try:
        parsed = parse_iso(as_of)
    except ValueError:
        raise HTTPException(status_code=400, detail=f"as_of is not an ISO 8601 timestamp: {as_of}")
    return iso(parsed), parsed < now


def _csv(value: str | None) -> list[str]:
    return [part.strip() for part in (value or "").split(",") if part.strip()]


def _encode_cursor(published_at: str, event_id: str) -> str:
    raw = f"{published_at}|{event_id}".encode("utf-8")
    return base64.urlsafe_b64encode(raw).decode("ascii").rstrip("=")


def _decode_cursor(cursor: str) -> tuple[str, str]:
    padded = cursor + "=" * (-len(cursor) % 4)
    try:
        published_at, _, event_id = base64.urlsafe_b64decode(padded).decode("utf-8").partition("|")
    except (binascii.Error, UnicodeDecodeError, ValueError):
        raise HTTPException(status_code=400, detail="cursor is not a cursor this endpoint issued")
    if not published_at or not event_id:
        raise HTTPException(status_code=400, detail="cursor is not a cursor this endpoint issued")
    return published_at, event_id


# The projected-state SELECT. On a historical read every mutable field comes from the latest
# event_state_history row at or before the cutoff (§9.13); on a live read it comes from the events
# row. COALESCE covers an event whose history row is somehow missing rather than returning NULL.
_HISTORICAL_STATE = """
  COALESCE((SELECT h.importance FROM event_state_history h
            WHERE h.event_id = e.id AND h.as_of <= :as_of
            ORDER BY h.as_of DESC LIMIT 1), e.importance)         AS eff_importance,
  COALESCE((SELECT h.novelty FROM event_state_history h
            WHERE h.event_id = e.id AND h.as_of <= :as_of
            ORDER BY h.as_of DESC LIMIT 1), e.novelty)            AS eff_novelty,
  COALESCE((SELECT h.document_count FROM event_state_history h
            WHERE h.event_id = e.id AND h.as_of <= :as_of
            ORDER BY h.as_of DESC LIMIT 1), e.document_count)     AS eff_document_count,
  COALESCE((SELECT h.source_tier FROM event_state_history h
            WHERE h.event_id = e.id AND h.as_of <= :as_of
            ORDER BY h.as_of DESC LIMIT 1), e.source_tier)        AS eff_source_tier,
  COALESCE((SELECT h.official_confirmed FROM event_state_history h
            WHERE h.event_id = e.id AND h.as_of <= :as_of
            ORDER BY h.as_of DESC LIMIT 1), e.official_confirmed) AS eff_official_confirmed
"""

_LIVE_STATE = """
  e.importance AS eff_importance, e.novelty AS eff_novelty,
  e.document_count AS eff_document_count, e.source_tier AS eff_source_tier,
  e.official_confirmed AS eff_official_confirmed
"""


def _enrichment_visible(row, *, as_of: str, historical: bool) -> bool:
    """§9.12. A backlog event enriched today must not leak hypotheses built against future
    information into a historical read."""
    if row["enrichment_state"] != "enriched":
        return False
    if not historical:
        return True
    enrichment_as_of = row["enrichment_as_of"]
    return bool(enrichment_as_of) and enrichment_as_of <= as_of


def _ticker_json(conn, event_id: str, *, enriched: bool) -> list[dict]:
    rows = conn.execute(
        "SELECT * FROM event_tickers WHERE event_id = ? ORDER BY is_primary DESC, ticker",
        (event_id,),
    ).fetchall()
    out = []
    for row in rows:
        # relevance and isPrimary are code's, deterministic and write-once — never gated (§9.15).
        item = {
            "ticker": row["ticker"],
            "relevance": row["relevance"],
            "isPrimary": bool(row["is_primary"]),
        }
        if enriched:
            if row["potential_direction"] is not None:
                item["potentialDirection"] = row["potential_direction"]
            if row["potential_strength"] is not None:
                item["potentialStrength"] = row["potential_strength"]
            if row["expected_horizon"] is not None:
                item["expectedHorizon"] = row["expected_horizon"]
            dimensions = json.loads(row["affected_dimensions"] or "[]")
            if dimensions:
                item["affectedDimensions"] = dimensions
            if row["transmission"] is not None:
                item["transmission"] = row["transmission"]
        out.append(item)
    return out


def _sources_json(conn, event_id: str) -> list[dict]:
    """Built from `event_documents`, never from a join back to `source_documents` (§3.2 / D-29)."""
    rows = conn.execute(
        "SELECT document_id, provider, source_tier, url, published_at FROM event_documents "
        "WHERE event_id = ? ORDER BY published_at, document_id",
        (event_id,),
    ).fetchall()
    return [
        {
            "documentId": r["document_id"],
            "provider": r["provider"],
            "sourceTier": r["source_tier"],
            "url": r["url"],
            "publishedAt": r["published_at"],
        }
        for r in rows
    ]


def _event_json(conn, row, *, as_of: str, historical: bool) -> dict:
    """Contract §3.12, camelCase, verbatim."""
    enriched = _enrichment_visible(row, as_of=as_of, historical=historical)
    event = {
        "id": row["id"],
        "eventType": row["event_type"],
        "title": row["title"],
        "summary": row["summary"],
        "occurredAt": row["occurred_at"],
        "publishedAt": row["published_at"],
        "firstSeenAt": row["first_seen_at"],
        "sourceTier": row["eff_source_tier"],
        "officialConfirmed": bool(row["eff_official_confirmed"]),
        "importance": row["eff_importance"],
        "novelty": row["eff_novelty"],
        "documentCount": row["eff_document_count"],
        "enrichmentState": row["enrichment_state"] if enriched else "raw",
        "tickers": _ticker_json(conn, row["id"], enriched=enriched),
        "sources": _sources_json(conn, row["id"]),
        "audit": None,
    }
    if enriched:
        if row["why_it_matters"] is not None:
            event["whyItMatters"] = row["why_it_matters"]
        event["keyFacts"] = json.loads(row["key_facts"] or "[]")
        event["audit"] = {
            "modelUsed": row["model_used"],
            "promptVersion": row["prompt_version"],
            "schemaVersion": row["schema_version"],
            "reasoningMode": row["reasoning_mode"],
            "enrichedAt": row["enriched_at"],
        }
    return event


def _coverage(conn, as_of: str, visible: int) -> dict:
    """§9.10's enum, plus §1.3's bounds when the store cannot serve the cutoff."""
    if visible:
        return {"coverage": COVERAGE_OK}
    bounds = conn.execute(
        "SELECT MIN(published_at) AS lo, MAX(published_at) AS hi FROM events"
    ).fetchone()
    return {
        "coverage": COVERAGE_INSUFFICIENT,
        "coverageFrom": bounds["lo"],
        "coverageTo": bounds["hi"],
    }


@router.get("/events")
def list_events(
    tickers: str | None = Query(default=None),
    types: str | None = Query(default=None),
    minImportance: float | None = Query(default=None),
    since: str | None = Query(default=None),
    as_of: str | None = Query(default=None),
    enrichmentState: str | None = Query(default=None),
    limit: int = Query(default=DEFAULT_LIMIT, ge=1, le=MAX_LIMIT),
    cursor: str | None = Query(default=None),
) -> dict:
    """Contract §3.11 / §9.9. Ordered `(published_at DESC, id DESC)` — a TOTAL order, so the cursor
    can never skip a row when two events share a published time."""
    resolved_as_of, historical = resolve_as_of(as_of)
    # §9.54: `enrichment_state` has two values. `failed` is rejected rather than accepted-and-empty:
    # nothing has ever written it (`put_enrichment` hardcodes 'enriched' and is the only writer), so
    # filtering on it returned zero rows always -- a false negative that reads as a working queue
    # selector. Un-enriched is `raw`, for every reason.
    if enrichmentState is not None and enrichmentState not in ("raw", "enriched"):
        raise HTTPException(
            status_code=400,
            detail=f"enrichmentState must be raw or enriched: {enrichmentState}",
        )

    where = ["e.published_at <= :as_of", "e.first_seen_at <= :as_of"]
    params: dict[str, object] = {"as_of": resolved_as_of}

    ticker_list = _csv(tickers)
    if ticker_list:
        marks = ",".join(f":tk{i}" for i in range(len(ticker_list)))
        # EXISTS, not a join: a multi-ticker event must appear ONCE per query, not once per match.
        where.append(
            f"EXISTS (SELECT 1 FROM event_tickers t WHERE t.event_id = e.id "
            f"AND t.ticker IN ({marks}))"
        )
        params.update({f"tk{i}": t.strip().upper() for i, t in enumerate(ticker_list)})

    type_list = _csv(types)
    if type_list:
        marks = ",".join(f":ty{i}" for i in range(len(type_list)))
        where.append(f"e.event_type IN ({marks})")
        params.update({f"ty{i}": t for i, t in enumerate(type_list)})

    if since:
        try:
            params["since"] = iso(since)
        except ValueError:
            raise HTTPException(status_code=400, detail=f"since is not an ISO 8601 timestamp: {since}")
        where.append("e.published_at >= :since")

    if enrichmentState is not None:
        # On a historical read an enrichment computed after the cutoff is invisible (§9.12), so it
        # must not satisfy an `enriched` filter either.
        if historical and enrichmentState == "enriched":
            where.append("e.enrichment_state = 'enriched'")
            where.append("e.enrichment_as_of IS NOT NULL AND e.enrichment_as_of <= :as_of")
        elif historical and enrichmentState == "raw":
            where.append(
                "(e.enrichment_state = 'raw' OR e.enrichment_as_of IS NULL "
                "OR e.enrichment_as_of > :as_of)"
            )
        else:
            params["estate"] = enrichmentState
            where.append("e.enrichment_state = :estate")

    if cursor:
        cursor_published, cursor_id = _decode_cursor(cursor)
        params["cur_p"], params["cur_i"] = cursor_published, cursor_id
        where.append("(e.published_at < :cur_p OR (e.published_at = :cur_p AND e.id < :cur_i))")

    state = _HISTORICAL_STATE if historical else _LIVE_STATE
    sql = (
        f"SELECT * FROM (SELECT e.*, {state} FROM events e WHERE {' AND '.join(where)}) "
        f"WHERE 1=1"
    )
    if minImportance is not None:
        params["min_importance"] = minImportance
        sql += " AND eff_importance >= :min_importance"
    sql += " ORDER BY published_at DESC, id DESC LIMIT :limit"
    params["limit"] = limit + 1  # one extra row tells us whether another page exists

    with _db() as conn:
        rows = conn.execute(sql, params).fetchall()
        page, has_more = rows[:limit], len(rows) > limit
        events = [_event_json(conn, r, as_of=resolved_as_of, historical=historical) for r in page]
        body = {
            "events": events,
            "asOf": resolved_as_of,
            "nextCursor": _encode_cursor(page[-1]["published_at"], page[-1]["id"]) if has_more else None,
            "degraded": [],
        }
        body.update(_coverage(conn, resolved_as_of, len(events)))
    return body


@router.get("/events/{event_id}")
def get_event(event_id: str, as_of: str | None = Query(default=None)) -> dict:
    """Contract §3.11 → `{event, documents, tickers, asOf}`."""
    resolved_as_of, historical = resolve_as_of(as_of)
    state = _HISTORICAL_STATE if historical else _LIVE_STATE
    with _db() as conn:
        row = conn.execute(
            f"SELECT e.*, {state} FROM events e WHERE e.id = :id "
            f"AND e.published_at <= :as_of AND e.first_seen_at <= :as_of",
            {"id": event_id, "as_of": resolved_as_of},
        ).fetchone()
        if row is None:
            raise HTTPException(status_code=404, detail=f"no such event at this cutoff: {event_id}")
        event = _event_json(conn, row, as_of=resolved_as_of, historical=historical)
        return {
            "event": event,
            "documents": _sources_json(conn, event_id),
            "tickers": event["tickers"],
            "asOf": resolved_as_of,
            "degraded": [],
        }


# =================================================================================================
# Enrichment intake (contract §3.11, §9.12, §9.14, §9.15, §9.16)
#
# Called ONLY by services/llm's enrichment worker. Never by the UI, never by the gateway. This lane
# makes no model call of its own: it validates what the worker proposes and refuses everything that
# is code's to decide.
# =================================================================================================


def _reject(detail: str):
    raise HTTPException(status_code=400, detail=detail)


def _validate_payload(payload: dict, *, stored_event_type: str, linked: set[str]) -> None:
    for field in DETERMINISTIC_FIELDS:
        if field in payload:
            _reject(
                f"{field} is deterministic and versioned in services/events (contract §9.15); "
                f"the enrichment payload may not carry it"
            )

    if "eventType" in payload or "event_type" in payload:
        proposed = payload.get("eventType", payload.get("event_type"))
        if proposed not in EVENT_TYPES:
            _reject(f"eventType is not a member of the §3.4 enum: {proposed}")
        if proposed != stored_event_type:
            _reject(
                f"eventType is deterministic: this event is {stored_event_type!r} and it feeds "
                f"cluster_key, so it may not be changed to {proposed!r}"
            )

    why = payload.get("whyItMatters")
    if why is not None and not isinstance(why, str):
        _reject("whyItMatters must be a string")

    facts = payload.get("keyFacts", [])
    if not isinstance(facts, list) or any(not isinstance(f, str) for f in facts):
        _reject("keyFacts must be an array of strings")

    tickers = payload.get("tickers", [])
    if not isinstance(tickers, list):
        _reject("tickers must be an array")
    for entry in tickers:
        if not isinstance(entry, dict):
            _reject("each tickers[] entry must be an object")
        if "relevance" in entry:
            _reject(
                "relevance is code's and write-once (contract §9.15); Wave 5B thresholds "
                "notifications on it, so it may not be a model-authored float"
            )
        ticker = str(entry.get("ticker", "")).strip().upper()
        if ticker not in linked:
            _reject(
                f"{ticker or '(missing)'} is not linked to this event; the model may weaken or "
                f"annotate a link but may not invent a company"
            )
        direction = entry.get("potentialDirection")
        if direction is not None and direction not in DIRECTIONS:
            _reject(f"potentialDirection must be one of {DIRECTIONS}: {direction!r}")
        horizon = entry.get("expectedHorizon")
        if horizon is not None and horizon not in HORIZONS:
            _reject(f"expectedHorizon must be one of {HORIZONS}: {horizon!r}")
        strength = entry.get("potentialStrength")
        if strength is not None:
            if not isinstance(strength, (int, float)) or isinstance(strength, bool) \
                    or not 0.0 <= float(strength) <= 1.0:
                _reject(f"potentialStrength must be a number in [0,1]: {strength!r}")
        dimensions = entry.get("affectedDimensions")
        if dimensions is not None:
            if not isinstance(dimensions, list):
                _reject("affectedDimensions must be an array")
            for dimension in dimensions:
                if dimension not in AFFECTED_DIMENSIONS:
                    _reject(f"affectedDimensions member is not in the §3.8 list: {dimension!r}")


@router.post("/events/{event_id}/enrichment")
def put_enrichment(event_id: str, payload: dict = Body(...)) -> dict:
    with _db() as conn:
        event = conn.execute("SELECT * FROM events WHERE id = ?", (event_id,)).fetchone()
        if event is None:
            raise HTTPException(status_code=404, detail=f"no such event: {event_id}")

        linked = {
            r["ticker"]
            for r in conn.execute(
                "SELECT ticker FROM event_tickers WHERE event_id = ?", (event_id,)
            ).fetchall()
        }
        _validate_payload(payload, stored_event_type=event["event_type"], linked=linked)

        # §9.14: verify EVERY proposed fact against the supporting documents before writing any of
        # it. The whole payload is rejected on one unverified string — a partial write would leave
        # a fact column holding something no document says.
        proposed_facts = [f for f in payload.get("keyFacts", []) if f.strip()]
        if proposed_facts:
            corpus = _supporting_corpus(conn, event_id)
            unverified = [f for f in proposed_facts if not verify_key_fact(f, corpus)]
            if unverified:
                _reject(
                    "keyFacts must be traceable to a supporting document (contract §9.14); "
                    f"unverified: {unverified!r}"
                )

        audit = payload.get("audit") or {}
        merged = _merge_key_facts(json.loads(event["key_facts"] or "[]"), proposed_facts)
        conn.execute(
            "UPDATE events SET why_it_matters = ?, key_facts = ?, enrichment_state = 'enriched', "
            "model_used = ?, prompt_version = ?, schema_version = ?, reasoning_mode = ?, "
            "enriched_at = ?, enrichment_as_of = ? WHERE id = ?",
            (
                payload.get("whyItMatters", event["why_it_matters"]),
                json.dumps(merged),
                audit.get("modelUsed"),
                audit.get("promptVersion"),
                audit.get("schemaVersion"),
                audit.get("reasoningMode"),
                audit.get("enrichedAt"),
                # §9.12. Stored verbatim, with NO fallback to enrichedAt: enrichedAt is a wall
                # clock and enrichment_as_of is a CUTOFF, and quietly substituting one for the
                # other is how future-informed hypotheses leak into a historical read. A worker
                # that omits it gets NULL, which makes its hypotheses invisible to every
                # historical read — the fail-safe direction.
                payload.get("enrichmentAsOf"),
                event_id,
            ),
        )
        for entry in payload.get("tickers", []):
            conn.execute(
                "UPDATE event_tickers SET potential_direction = ?, potential_strength = ?, "
                "expected_horizon = ?, affected_dimensions = ?, transmission = ? "
                "WHERE event_id = ? AND ticker = ?",
                (
                    entry.get("potentialDirection"),
                    entry.get("potentialStrength"),
                    entry.get("expectedHorizon"),
                    json.dumps(entry.get("affectedDimensions") or []),
                    entry.get("transmission"),
                    event_id,
                    str(entry["ticker"]).strip().upper(),
                ),
            )
        conn.commit()

        row = conn.execute(
            f"SELECT e.*, {_LIVE_STATE} FROM events e WHERE e.id = ?", (event_id,)
        ).fetchone()
        return {"event": _event_json(conn, row, as_of=iso(datetime.now(timezone.utc)),
                                     historical=False)}


def _supporting_corpus(conn, event_id: str) -> str:
    """Everything the surviving supporting documents actually say.

    A retention sweep (§3.2) may already have removed some rows; verification runs against what
    remains, so a fact whose only source has been swept is unverifiable and is refused. That is the
    honest outcome — it is not a reason to relax the check.
    """
    rows = conn.execute(
        "SELECT d.title, d.excerpt, d.body FROM source_documents d "
        "JOIN event_documents ed ON ed.document_id = d.id WHERE ed.event_id = ? ORDER BY d.id",
        (event_id,),
    ).fetchall()
    return normalise_for_verification(" ".join(_document_text(r) for r in rows))
