"""Phase 3 — which companies a scheduled event bears on, and why.

THE RULE THAT SHAPES THIS FILE
-------------------------------
**Do not fabricate supply-chain relationships.** Every row this module produces comes from exactly
one of three places, and each row says which:

* `derived`   — computed from the event's OWN fields. NVIDIA's earnings is `direct` for NVDA
                because the event says `ticker = NVDA`. A CPI release is `macro` for the whole
                configured universe because it is a macro release. Nothing is inferred.
* `reference` — a STORED, CONFIGURED reference relationship (`REFERENCE_RELATIONSHIPS`, or the
                operator's `RELATIONSHIP_REGISTRY_PATH` file). Each entry carries its own
                one-sentence reason and a `sourceRef` naming where that came from.
* `evidence`  — an official document that states the relationship. Reserved and enforced by the
                schema; nothing writes it yet, and it is here so that when something does, it is a
                distinct provenance rather than a `reference` row with a nicer comment.

There is deliberately **no fourth source, and specifically no model**. A model-proposed supply
chain would be indistinguishable on the screen from a verified one, and a portfolio exposure
computed over it would be a fabricated number with a real-looking denominator.

THE ONE SUBTLETY: WHICH DIRECTION `relationship` POINTS
-------------------------------------------------------
Two different things are called a "relationship" here and they are inverses of each other. Getting
them the wrong way round would put NVIDIA's earnings on TSMC's page labelled "supplier", which
reads as "TSMC supplies this event" — nonsense that looks plausible. So, stated once:

* In `REFERENCE_RELATIONSHIPS`, `relationship` says **what the counterparty is to the subject**.
  `{subject: NVDA, counterparty: TSM, relationship: supplier}` reads "TSMC is NVIDIA's supplier".
* In a row this module EMITS, `relationship` says **what the event's subject is to the listed
  ticker** — because that is the question the ticker's page is asking. For an NVIDIA event, TSMC's
  row therefore reads `customer`: NVIDIA is a customer of TSMC, so NVIDIA's results bear on TSMC
  as customer news.

`INVERSE` performs exactly that flip, in one place, so the two directions cannot drift apart.

WHAT IS AND IS NOT A NUMBER HERE
--------------------------------
`relevance_band` is one of three words. It is not a score, not a probability and not a weight. The
only numbers Phase 3 produces are portfolio weights, and those are computed by `journal` from the
user's own holdings — never here, and never by a model.
"""
from __future__ import annotations

import json
import os
from datetime import datetime, timezone
from pathlib import Path

from .db import Connection

#: Bumped when the DERIVATION RULES change, so a stored row always says which rules produced it.
#: Not bumped when the reference table gains a company — that is data, and each row carries its own
#: `sourceRef`.
CALC_VERSION = "relationships@1"

REGISTRY_PATH_ENV = "RELATIONSHIP_REGISTRY_PATH"

# ---- the closed vocabulary (mirrored by the migration's CHECK constraint) -------------------------

DIRECT = "direct"
SECTOR = "sector"
SUPPLIER = "supplier"
CUSTOMER = "customer"
COMPETITOR = "competitor"
MACRO = "macro"
FACTOR = "factor"
RELATIONSHIPS = (DIRECT, SECTOR, SUPPLIER, CUSTOMER, COMPETITOR, MACRO, FACTOR)

SOURCE_DERIVED = "derived"
SOURCE_REFERENCE = "reference"
SOURCE_EVIDENCE = "evidence"
SOURCES = (SOURCE_DERIVED, SOURCE_REFERENCE, SOURCE_EVIDENCE)

BAND_PRIMARY = "primary"
BAND_SECONDARY = "secondary"
BAND_CONTEXTUAL = "contextual"
BANDS = (BAND_PRIMARY, BAND_SECONDARY, BAND_CONTEXTUAL)

#: Event kinds that are market-wide by nature. Everything else is company-scoped.
MARKET_WIDE_KINDS = ("macro_release", "central_bank")

#: Sector membership, for the `sector` relationship. This is the SAME static map
#: `predictions.TICKER_SECTOR_ETF` already owns under §9.26 — imported rather than re-declared, so
#: the two cannot drift into two different opinions about which sector a company is in.
from .predictions import TICKER_SECTOR_ETF  # noqa: E402  (placed here to keep the rationale next to it)


# ---- the reference table -------------------------------------------------------------------------
#
# CONFIGURATION, CARRYING ITS OWN CITATION. Every entry names where the relationship came from in
# `sourceRef`, and every entry here is drawn from this repository's own verified reference data
# (`gateway/seed/nvda.json`'s supplier list, itself compiled in `docs/NVIDIA_RESEARCH.md`).
#
# It is deliberately SHORT. Only relationships whose counterparty resolves to a ticker we actually
# cover are listed: the seed also names Foxconn, Quanta and Wistron, which are real relationships to
# companies this system does not track, and a row pointing at a ticker nobody holds is noise. A
# relationship that cannot be stated precisely is left out rather than approximated.
REFERENCE_RELATIONSHIPS: tuple[dict, ...] = (
    {
        "subject": "NVDA",
        "counterparty": "TSM",
        "relationship": SUPPLIER,
        "reason": "TSMC is NVIDIA's foundry and CoWoS advanced-packaging supplier; packaging "
                  "capacity has been the binding supply constraint.",
        "sourceRef": "gateway/seed/nvda.json:suppliers (docs/NVIDIA_RESEARCH.md)",
        "band": BAND_PRIMARY,
    },
    {
        "subject": "NVDA",
        "counterparty": "MU",
        "relationship": SUPPLIER,
        "reason": "Micron supplies HBM memory used in NVIDIA data-centre accelerators.",
        "sourceRef": "gateway/seed/nvda.json:suppliers (docs/NVIDIA_RESEARCH.md)",
        "band": BAND_SECONDARY,
    },
    {
        "subject": "NVDA",
        "counterparty": "AMD",
        "relationship": COMPETITOR,
        "reason": "AMD competes with NVIDIA in data-centre accelerators.",
        "sourceRef": "gateway/seed/nvda.json (competitive set)",
        "band": BAND_SECONDARY,
    },
    {
        "subject": "NVDA",
        "counterparty": "MSFT",
        "relationship": CUSTOMER,
        "reason": "Hyperscaler capital spending on AI infrastructure is a demand driver for "
                  "NVIDIA data-centre revenue.",
        "sourceRef": "gateway/seed/nvda.json:catalysts (hyperscaler capex)",
        "band": BAND_SECONDARY,
    },
    {
        "subject": "NVDA",
        "counterparty": "GOOGL",
        "relationship": CUSTOMER,
        "reason": "Hyperscaler capital spending on AI infrastructure is a demand driver for "
                  "NVIDIA data-centre revenue.",
        "sourceRef": "gateway/seed/nvda.json:catalysts (hyperscaler capex)",
        "band": BAND_SECONDARY,
    },
)

#: The mirror of a relationship, when one exists. Used so a configured `NVDA supplier TSM` also
#: answers "an event about TSM is a `supplier` event for NVDA" without the operator writing it
#: twice — and writing it twice is how the two directions drift apart.
INVERSE = {
    SUPPLIER: CUSTOMER,
    CUSTOMER: SUPPLIER,
    COMPETITOR: COMPETITOR,
    SECTOR: SECTOR,
}

REQUIRED_REFERENCE_FIELDS = ("subject", "counterparty", "relationship", "reason", "sourceRef")


def _valid_reference(entry) -> bool:
    if not isinstance(entry, dict):
        return False
    if any(not str(entry.get(f) or "").strip() for f in REQUIRED_REFERENCE_FIELDS):
        return False
    if entry["relationship"] not in RELATIONSHIPS:
        return False
    return entry.get("band", BAND_SECONDARY) in BANDS


def _configured_references() -> list[dict]:
    """Reference relationships from `RELATIONSHIP_REGISTRY_PATH`, appended to the built-ins.

    Appended rather than replacing: the built-ins are cited facts about companies this system
    covers, and an operator adding their own coverage should not have to restate them. A malformed
    entry is dropped whole — a half-valid relationship with no reason is exactly the fabricated
    edge this module exists to prevent.
    """
    path = os.getenv(REGISTRY_PATH_ENV, "").strip()
    if not path:
        return []
    try:
        payload = json.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return []
    if isinstance(payload, dict):
        payload = payload.get("relationships") or []
    if not isinstance(payload, list):
        return []
    return [e for e in payload if _valid_reference(e)]


def reference_table() -> list[dict]:
    out = []
    for entry in tuple(REFERENCE_RELATIONSHIPS) + tuple(_configured_references()):
        out.append({
            "subject": str(entry["subject"]).strip().upper(),
            "counterparty": str(entry["counterparty"]).strip().upper(),
            "relationship": entry["relationship"],
            "reason": str(entry["reason"]).strip(),
            "sourceRef": str(entry["sourceRef"]).strip(),
            "band": entry.get("band", BAND_SECONDARY),
            "effectiveFrom": str(entry.get("effectiveFrom") or "").strip(),
        })
    return out


def counterparties_of(ticker: str) -> list[dict]:
    """Every stored relationship in which `ticker` is the SUBJECT of the event.

    The returned `relationship` is in EMITTED direction: what the event's subject is to the listed
    ticker. See the module docstring — a configured "TSMC is NVIDIA's supplier" means an NVIDIA
    event is `customer` news for TSMC, and a TSMC event is `supplier` news for NVIDIA.
    """
    symbol = (ticker or "").strip().upper()
    if not symbol:
        return []
    out: list[dict] = []
    for entry in reference_table():
        if entry["subject"] == symbol:
            # The event is about the subject; the counterparty relates to it as the INVERSE.
            inverse = INVERSE.get(entry["relationship"])
            if inverse:
                out.append({**entry, "ticker": entry["counterparty"], "relationship": inverse})
        elif entry["counterparty"] == symbol:
            out.append({**entry, "ticker": entry["subject"],
                        "relationship": entry["relationship"]})
    return out


def sector_peers(ticker: str) -> list[str]:
    symbol = (ticker or "").strip().upper()
    sector = TICKER_SECTOR_ETF.get(symbol)
    if not sector:
        return []
    return sorted(
        peer for peer, group in TICKER_SECTOR_ETF.items()
        if group == sector and peer != symbol
    )


# ---- derivation ----------------------------------------------------------------------------------


def _iso(moment: datetime) -> str:
    return moment.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def derive_relationships(event: dict, *, universe=()) -> list[dict]:
    """Every ticker this event bears on, with the reason and the provenance for each. Pure.

    `universe` is the set of companies the deployment actually covers. It bounds the macro fan-out:
    a CPI print bears on every company in the world, and writing a row per company in the world
    would be true and useless. Rows are written for the covered universe and the reason says so.
    """
    kind = str(event.get("kind") or "")
    subject = str(event.get("ticker") or "").strip().upper()
    series = str(event.get("series") or "").strip()
    covered = [t.strip().upper() for t in universe if str(t).strip()]

    rows: dict[tuple[str, str], dict] = {}

    def add(ticker, relationship, reason, source, source_ref, band, effective_from=""):
        if not ticker or relationship not in RELATIONSHIPS:
            return
        key = (ticker, relationship)
        if key in rows:
            return
        rows[key] = {
            "ticker": ticker,
            "relationship": relationship,
            "reason": reason,
            "source": source,
            "sourceRef": source_ref,
            "band": band,
            "effectiveFrom": effective_from,
        }

    if kind in MARKET_WIDE_KINDS:
        label = series or kind.replace("_", " ")
        for ticker in covered:
            add(
                ticker, MACRO,
                f"{label} is a market-wide release; it bears on every covered company through the "
                "macro backdrop rather than through company-specific news.",
                SOURCE_DERIVED, f"event.kind={kind}", BAND_CONTEXTUAL,
            )
        return sorted(rows.values(), key=lambda r: (r["ticker"], r["relationship"]))

    if not subject:
        # A company-scoped event with no company. Nothing can be said about who it bears on, so
        # nothing is said — an unattributed event is not a market-wide one.
        return []

    add(
        subject, DIRECT,
        "The event names this company as its subject.",
        SOURCE_DERIVED, "event.ticker", BAND_PRIMARY,
    )

    for entry in counterparties_of(subject):
        add(
            entry["ticker"], entry["relationship"], entry["reason"],
            SOURCE_REFERENCE, entry["sourceRef"], entry["band"], entry["effectiveFrom"],
        )

    sector = TICKER_SECTOR_ETF.get(subject)
    for peer in sector_peers(subject):
        add(
            peer, SECTOR,
            f"{peer} and {subject} are both mapped to the {sector} sector group, so a "
            f"{subject} event is sector context for {peer}.",
            SOURCE_DERIVED, f"TICKER_SECTOR_ETF[{subject}]={sector}", BAND_CONTEXTUAL,
        )

    return sorted(rows.values(), key=lambda r: (r["ticker"], r["relationship"]))


# ---- storage -------------------------------------------------------------------------------------


def store_relationships(
    conn: Connection, event_id: str, rows, *, now: datetime | None = None
) -> int:
    """Upsert relationships for one event. Idempotent; `first_seen_at` is write-once.

    Returns the number of rows INSERTED (not updated), so a re-run reports zero rather than
    reporting its own no-op as work.
    """
    moment = (now or datetime.now(timezone.utc)).astimezone(timezone.utc)
    stamp = _iso(moment)
    inserted = 0
    for row in rows or []:
        effective = str(row.get("effectiveFrom") or "").strip() or stamp
        cursor = conn.execute(
            "INSERT INTO event_relationships "
            "(event_id, ticker, relationship, reason, source, source_ref, first_seen_at, "
            " effective_from, relevance_band, calc_version, updated_at) "
            "VALUES (?,?,?,?,?,?,?,?,?,?,?) "
            "ON CONFLICT (event_id, ticker, relationship) DO UPDATE SET "
            "  reason = EXCLUDED.reason, source = EXCLUDED.source, "
            "  source_ref = EXCLUDED.source_ref, relevance_band = EXCLUDED.relevance_band, "
            "  calc_version = EXCLUDED.calc_version, updated_at = EXCLUDED.updated_at",
            (event_id, row["ticker"], row["relationship"], row.get("reason") or "",
             row.get("source") or SOURCE_DERIVED, row.get("sourceRef") or "", stamp, effective,
             row.get("band") or BAND_SECONDARY, CALC_VERSION, stamp),
        )
        if cursor.rowcount:
            existing = conn.execute(
                "SELECT first_seen_at FROM event_relationships "
                "WHERE event_id = ? AND ticker = ? AND relationship = ?",
                (event_id, row["ticker"], row["relationship"]),
            ).fetchone()
            if existing and existing["first_seen_at"] == stamp:
                inserted += 1
    conn.commit()
    return inserted


def relationships_for_event(conn: Connection, event_id: str, *, as_of: str | None = None):
    if as_of:
        return conn.execute(
            "SELECT * FROM event_relationships WHERE event_id = ? AND first_seen_at <= ? "
            "ORDER BY ticker, relationship",
            (event_id, as_of),
        ).fetchall()
    return conn.execute(
        "SELECT * FROM event_relationships WHERE event_id = ? ORDER BY ticker, relationship",
        (event_id,),
    ).fetchall()


def rebuild_relationships(
    conn: Connection, *, universe=(), now: datetime | None = None, limit: int = 500
) -> dict:
    """Derive and store relationships for the most recent scheduled events. Bounded, idempotent.

    Called from the ingestion pass. It reads only stored rows and configuration: no provider, no
    model, no clock-dependent behaviour beyond stamping `first_seen_at` once.
    """
    moment = (now or datetime.now(timezone.utc)).astimezone(timezone.utc)
    events = conn.execute(
        "SELECT id, kind, ticker, series FROM scheduled_events "
        "ORDER BY scheduled_at DESC, id DESC LIMIT ?",
        (max(1, int(limit)),),
    ).fetchall()

    report = {"events": len(events), "written": 0, "inserted": 0}
    for event in events:
        rows = derive_relationships(dict(event), universe=universe)
        if not rows:
            continue
        report["written"] += len(rows)
        report["inserted"] += store_relationships(conn, event["id"], rows, now=moment)
    return report


# ---- HTTP surface (§9.28: the router is exported as `router`) -------------------------------------
#
# READS ONLY, and store-only. Nothing here fetches, and nothing here generates: the answer is
# assembled from `scheduled_events` and `event_relationships`, both of which were written by an
# ingestion pass. Opening Calendar, Following or Portfolio therefore cannot cause a provider call
# or a model call, which is the property Phase 3's acceptance list turns on.

from fastapi import APIRouter, HTTPException, Query  # noqa: E402

from .events import MAX_LIMIT  # noqa: E402
from .macro import _db, iso, resolve_as_of  # noqa: E402

router = APIRouter()


def _relationship_json(row) -> dict:
    return {
        "eventId": row["event_id"],
        "ticker": row["ticker"],
        "relationship": row["relationship"],
        "reason": row["reason"],
        "source": row["source"],
        "sourceRef": row["source_ref"],
        "firstSeenAt": row["first_seen_at"],
        "effectiveFrom": row["effective_from"],
        "relevanceBand": row["relevance_band"],
        "calcVersion": row["calc_version"],
    }


@router.get("/relationships")
def get_relationships(
    tickers: str | None = Query(default=None),
    from_: str | None = Query(default=None, alias="from"),
    to: str | None = Query(default=None),
    as_of: str | None = Query(default=None),
    limit: int = Query(default=200, ge=1, le=MAX_LIMIT),
) -> dict:
    """Scheduled events that bear on the requested companies, with the relationship and the reason.

    Point-in-time on both halves, like every other read in this service: an event that was not
    knowable at the cutoff is excluded by its `first_seen_at`, and so is a RELATIONSHIP that was
    not — a supply-chain relationship added to the reference table last week must not appear to
    have been known a year ago.
    """
    resolved_as_of, _historical = resolve_as_of(as_of)
    wanted = [t.strip().upper() for t in (tickers or "").split(",") if t.strip()]

    where = [
        "r.first_seen_at <= :as_of",
        "s.first_seen_at <= :as_of",
    ]
    params: dict = {"as_of": resolved_as_of, "limit": limit}
    if wanted:
        where.append("r.ticker = ANY(:tickers)")
        params["tickers"] = wanted
    if from_:
        try:
            params["from"] = iso(from_)
        except ValueError:
            raise HTTPException(status_code=400, detail="from must be an ISO 8601 timestamp")
        where.append("s.scheduled_at >= :from")
    if to:
        try:
            params["to"] = iso(to)
        except ValueError:
            raise HTTPException(status_code=400, detail="to must be an ISO 8601 timestamp")
        where.append("s.scheduled_at <= :to")

    with _db() as conn:
        rows = conn.execute(
            "SELECT r.*, s.kind, s.ticker AS subject, s.series, s.scheduled_at, s.title, "
            "       s.description, s.status, s.confirmed, s.importance, s.source AS event_source, "
            "       s.source_tier, s.source_url "
            "FROM event_relationships r JOIN scheduled_events s ON s.id = r.event_id "
            f"WHERE {' AND '.join(where)} "
            "ORDER BY s.scheduled_at, r.event_id, r.ticker, r.relationship LIMIT :limit",
            params,
        ).fetchall()

    items = []
    for row in rows:
        item = _relationship_json(row)
        item["event"] = {
            "id": row["event_id"],
            "kind": row["kind"],
            "ticker": row["subject"],
            "series": row["series"],
            "scheduledAt": row["scheduled_at"],
            "title": row["title"],
            "description": row["description"],
            "status": row["status"],
            "confirmed": bool(row["confirmed"]),
            "importance": row["importance"],
            "source": row["event_source"],
            "sourceTier": row["source_tier"],
            "sourceUrl": row["source_url"],
        }
        items.append(item)

    return {
        "relationships": items,
        "asOf": resolved_as_of,
        "calcVersion": CALC_VERSION,
        "vocabulary": list(RELATIONSHIPS),
        "degraded": [],
    }


@router.get("/relationships/reference")
def get_reference_table() -> dict:
    """The stored reference relationships, with the citation each one carries.

    Exposed so the relationship set is AUDITABLE from outside the container: "why does this event
    appear on TSMC's page" has an answer a person can read, and a fabricated edge would be visible
    here rather than only in a table.
    """
    return {
        "relationships": reference_table(),
        "vocabulary": list(RELATIONSHIPS),
        "sources": list(SOURCES),
        "bands": list(BANDS),
        "calcVersion": CALC_VERSION,
        "registrySource": "override" if _configured_references() else "builtin",
    }
