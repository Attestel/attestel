"""Event assembly, deterministic scores and the read endpoints (contract §3.3, §3.6, §3.11, §3.12).

Everything here runs against a tmp_path database with no keys and no network. The first test in the
file is the one the whole lane exists to keep passing: a historical read must be served from the
store with every outbound path patched to raise.
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

import pytest
from fastapi.testclient import TestClient

from app import events as events_mod
from app.db import connect, migrate

SERVICE_ROOT = Path(__file__).resolve().parent.parent


# ---- fixtures ------------------------------------------------------------------------------------


@pytest.fixture()
def conn(tmp_path, monkeypatch):
    c = connect()
    migrate(c)
    yield c
    c.close()


@pytest.fixture()
def client(conn):
    """A TestClient over the real app, sharing the fixture's PostgreSQL schema."""
    from app.main import app

    with TestClient(app) as c:
        yield c


def insert_document(
    conn,
    doc_id,
    *,
    title,
    published_at,
    first_seen_at=None,
    provider="marketaux",
    tier="professional",
    raw_tickers=("NVDA",),
    excerpt="bounded excerpt",
    body=None,
    url=None,
):
    conn.execute(
        "INSERT INTO source_documents (id, content_hash, provider, source_tier, url, title, "
        "excerpt, body, published_at, first_seen_at, retrieved_at, raw_tickers, ingest_run_id) "
        "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
        (
            doc_id,
            f"hash-{doc_id}",
            provider,
            tier,
            url or f"https://example.test/{doc_id}",
            title,
            excerpt,
            body,
            published_at,
            first_seen_at or published_at,
            first_seen_at or published_at,
            json.dumps(list(raw_tickers)),
            "run_0123456789abcdef",
        ),
    )
    conn.commit()
    return doc_id


# ---- Phase 1: the leakage test (contract §1.3, §1.5) ---------------------------------------------


@pytest.fixture()
def no_network(monkeypatch):
    """Patch every outbound path this service could conceivably reach, to raise.

    services/events imports no HTTP client at all — that is the point. This fixture proves it
    rather than asserting it in prose: if any code path ever grows one, the historical read below
    stops passing.
    """
    import http.client
    import socket
    import urllib.request

    def boom(*_args, **_kwargs):  # pragma: no cover - the assertion is that it never runs
        raise AssertionError("a historical read attempted a network call (contract §1.3)")

    monkeypatch.setattr(socket.socket, "connect", boom, raising=False)
    monkeypatch.setattr(socket, "create_connection", boom, raising=False)
    monkeypatch.setattr(urllib.request, "urlopen", boom, raising=False)
    monkeypatch.setattr(http.client.HTTPConnection, "request", boom, raising=False)
    monkeypatch.setattr(http.client.HTTPSConnection, "request", boom, raising=False)
    try:
        import requests

        monkeypatch.setattr(requests.Session, "request", boom, raising=False)
        monkeypatch.setattr(requests, "request", boom, raising=False)
    except ModuleNotFoundError:  # pragma: no cover - requests is in requirements.txt
        pass


def test_historical_read_never_fetches(conn, client, no_network):
    """§1.3: when as_of is strictly in the past a provider fetch is forbidden, for any reason.

    The store is deliberately populated first, so the read has something to return and a passing
    result cannot be confused with "it returned empty because it could not reach anything".
    """
    insert_document(
        conn,
        "doc_leak000000000001",
        title="NVIDIA reports record data centre revenue for the quarter",
        published_at="2026-08-10T13:00:00Z",
    )
    events_mod.assemble(conn)

    body = client.get("/events", params={"as_of": "2026-08-12T00:00:00Z"}).json()

    assert body["asOf"] == "2026-08-12T00:00:00Z"
    assert body["coverage"] == "ok"
    assert len(body["events"]) == 1
    assert body["degraded"] == []


def test_empty_store_returns_coverage_not_a_500(client):
    """An empty store answers with coverage, never a 500 (invariant #1)."""
    body = client.get("/events").json()
    assert body["events"] == []
    assert body["coverage"] == "insufficient"
    assert body["coverageFrom"] is None
    assert body["coverageTo"] is None
    assert body["degraded"] == []


# ---- assembly ------------------------------------------------------------------------------------


def test_assembly_creates_one_event_per_document_cluster(conn):
    insert_document(
        conn,
        "doc_aaaaaaaaaaaaaaa1",
        title="NVIDIA reports record data centre revenue for the quarter",
        published_at="2026-08-10T13:00:00Z",
    )
    insert_document(
        conn,
        "doc_aaaaaaaaaaaaaaa2",
        title="Tesla opens a new assembly plant in Monterrey",
        published_at="2026-08-10T14:00:00Z",
        raw_tickers=("TSLA",),
    )
    created = events_mod.assemble(conn)
    assert len(created) == 2

    rows = conn.execute("SELECT id, document_count, source_tier FROM events ORDER BY id").fetchall()
    assert [r["document_count"] for r in rows] == [1, 1]
    assert {r["source_tier"] for r in rows} == {"professional"}


def test_assembly_is_idempotent(conn):
    insert_document(
        conn,
        "doc_bbbbbbbbbbbbbbb1",
        title="NVIDIA reports record data centre revenue for the quarter",
        published_at="2026-08-10T13:00:00Z",
    )
    events_mod.assemble(conn)
    events_mod.assemble(conn)
    events_mod.assemble(conn)
    n = conn.execute("SELECT COUNT(*) AS n FROM events").fetchone()["n"]
    assert n == 1


def test_marketaux_body_only_entities_do_not_reach_event_tickers(conn):
    insert_document(
        conn,
        "doc_subject00000000a1",
        title="Broadcom shares rise after a product announcement",
        published_at="2026-08-10T13:00:00Z",
        raw_tickers=("NVDA", "AVGO"),
    )

    events_mod.assemble(conn)

    rows = conn.execute(
        "SELECT ticker, is_primary FROM event_tickers ORDER BY ticker"
    ).fetchall()
    assert [(row["ticker"], row["is_primary"]) for row in rows] == [("AVGO", 1)]


def test_marketaux_negated_companies_do_not_reach_event_tickers(conn):
    insert_document(
        conn,
        "doc_subject00000000a2",
        title="Not Nvidia. Not AMD. This semiconductor company could lead the market.",
        published_at="2026-08-10T14:00:00Z",
        raw_tickers=("NVDA", "AMD"),
    )

    events_mod.assemble(conn)

    assert conn.execute("SELECT COUNT(*) AS n FROM event_tickers").fetchone()["n"] == 0


def test_official_document_raises_tier_and_confirms_the_event(conn):
    insert_document(
        conn,
        "doc_ccccccccccccccc1",
        title="US announces new export restrictions on NVIDIA advanced AI accelerators",
        published_at="2026-08-10T13:00:00Z",
        first_seen_at="2026-08-10T13:05:00Z",
    )
    events_mod.assemble(conn)
    insert_document(
        conn,
        "doc_ccccccccccccccc2",
        title="US announces new export restrictions on NVIDIA advanced AI accelerators",
        published_at="2026-08-10T13:30:00Z",
        first_seen_at="2026-08-10T13:35:00Z",
        provider="sec-edgar",
        tier="official",
        body="the full filing text",
    )
    events_mod.assemble(conn)

    row = conn.execute(
        "SELECT source_tier, official_confirmed, document_count, published_at FROM events"
    ).fetchone()
    assert row["source_tier"] == "official"
    assert row["official_confirmed"] == 1
    assert row["document_count"] == 2
    # published_at is the EARLIEST supporting document's, not the joining one's.
    assert row["published_at"] == "2026-08-10T13:00:00Z"


# ---- scores (§3.6) -------------------------------------------------------------------------------


def test_importance_rises_with_tier_and_document_count(conn):
    insert_document(
        conn,
        "doc_ddddddddddddddd1",
        title="Analysts trim Intel estimates for the semiconductor sector this quarter",
        published_at="2026-08-10T13:00:00Z",
        raw_tickers=("INTC",),
    )
    events_mod.assemble(conn)
    low = conn.execute("SELECT importance FROM events").fetchone()["importance"]

    insert_document(
        conn,
        "doc_ddddddddddddddd2",
        title="Analysts trim Intel estimates for the semiconductor sector this quarter",
        published_at="2026-08-10T13:10:00Z",
        provider="sec-edgar",
        tier="official",
        raw_tickers=("INTC",),
    )
    events_mod.assemble(conn)
    high = conn.execute("SELECT importance FROM events").fetchone()["importance"]
    assert high > low
    assert 0.0 <= low <= 1.0 and 0.0 <= high <= 1.0


def test_the_frozen_version_strings():
    """These name the corpus. Changing a weight, a prior or a rule table without bumping them makes
    two Wave 5A rungs incomparable while every test stays green."""
    from app.cluster import CLUSTER_VERSION

    assert events_mod.SCORE_VERSION == "score@1"
    assert CLUSTER_VERSION == "cluster@1"
    assert events_mod.IMPORTANCE_W_TIER == 0.35
    assert events_mod.IMPORTANCE_W_DOCUMENT_COUNT == 0.25
    assert events_mod.IMPORTANCE_W_ENTITY == 0.20
    assert events_mod.IMPORTANCE_W_EVENT_TYPE == 0.20
    assert sum((events_mod.IMPORTANCE_W_TIER, events_mod.IMPORTANCE_W_DOCUMENT_COUNT,
                events_mod.IMPORTANCE_W_ENTITY, events_mod.IMPORTANCE_W_EVENT_TYPE)) == 1.0


def test_importance_is_always_in_range(conn):
    """The events table CHECKs 0..1; a weight edit that breaks the bound must fail here first."""
    for tier in ("official", "professional", "discussion"):
        for count in (0, 1, 8, 500):
            for ticker in ("NVDA", "ZZZZ", None):
                for event_type in events_mod.EVENT_TYPE_PRIOR:
                    value = events_mod.compute_importance(
                        tier=tier, document_count=count, ticker=ticker, event_type=event_type
                    )
                    assert 0.0 <= value <= 1.0


def test_document_count_saturates(conn):
    """Five rewrites of one press release are not five times as important."""
    from app.events import document_count_saturation

    assert document_count_saturation(1) < document_count_saturation(4)
    assert document_count_saturation(8) == pytest.approx(1.0)
    assert document_count_saturation(80) == 1.0  # capped, never above 1


def test_novelty_is_one_for_a_tickers_first_event(conn):
    insert_document(
        conn,
        "doc_eeeeeeeeeeeeeee1",
        title="NVIDIA unveils a new accelerator architecture at its developer conference",
        published_at="2026-08-10T13:00:00Z",
    )
    events_mod.assemble(conn)
    assert conn.execute("SELECT novelty FROM events").fetchone()["novelty"] == 1.0


def test_novelty_drops_for_a_near_repeat_of_a_prior_event(conn):
    insert_document(
        conn,
        "doc_fffffffffffffff1",
        title="NVIDIA unveils a new accelerator architecture at its developer conference",
        published_at="2026-08-01T13:00:00Z",
    )
    events_mod.assemble(conn)
    insert_document(
        conn,
        "doc_fffffffffffffff2",
        title="NVIDIA unveils a new accelerator architecture at its developer conference again",
        published_at="2026-08-09T13:00:00Z",
    )
    events_mod.assemble(conn)
    rows = conn.execute("SELECT novelty FROM events ORDER BY published_at").fetchall()
    assert rows[0]["novelty"] == 1.0
    assert rows[1]["novelty"] < 1.0


def test_novelty_never_looks_at_a_later_event(conn):
    """Comparing against the future would make novelty depend on when the batch ran."""
    insert_document(
        conn,
        "doc_ggggggggggggggg1",
        title="NVIDIA unveils a new accelerator architecture at its developer conference",
        published_at="2026-08-09T13:00:00Z",
    )
    insert_document(
        conn,
        "doc_ggggggggggggggg2",
        title="NVIDIA unveils a new accelerator architecture at its developer conference again",
        published_at="2026-08-10T13:00:00Z",
    )
    events_mod.assemble(conn)  # both in one batch
    rows = conn.execute("SELECT novelty FROM events ORDER BY published_at").fetchall()
    assert rows[0]["novelty"] == 1.0  # earlier event cannot see the later one


# ---- GET /events (§3.11) -------------------------------------------------------------------------


def _seed_feed(conn):
    insert_document(
        conn,
        "doc_feed0000000000a1",
        title="NVIDIA reports record data centre revenue for the quarter",
        published_at="2026-08-01T13:00:00Z",
    )
    insert_document(
        conn,
        "doc_feed0000000000a2",
        title="US announces new export restrictions on NVIDIA advanced AI accelerators",
        published_at="2026-08-02T13:00:00Z",
        provider="sec-edgar",
        tier="official",
    )
    insert_document(
        conn,
        "doc_feed0000000000a3",
        title="Tesla opens a new assembly plant in Monterrey",
        published_at="2026-08-03T13:00:00Z",
        raw_tickers=("TSLA",),
    )
    events_mod.assemble(conn)


def test_events_are_ordered_newest_first(conn, client):
    _seed_feed(conn)
    body = client.get("/events").json()
    published = [e["publishedAt"] for e in body["events"]]
    assert published == sorted(published, reverse=True)


def test_ticker_filter_returns_an_event_once(conn, client):
    """A multi-ticker event appears once per query, not once per matching ticker."""
    insert_document(
        conn,
        "doc_multi000000000a1",
        title="Alphabet and NVIDIA expand their cloud accelerator partnership",
        published_at="2026-08-04T13:00:00Z",
        raw_tickers=("NVDA", "GOOGL"),
    )
    events_mod.assemble(conn)
    body = client.get("/events", params={"tickers": "NVDA,GOOGL"}).json()
    assert len(body["events"]) == 1
    assert {t["ticker"] for t in body["events"][0]["tickers"]} == {"NVDA", "GOOGL"}


def test_min_importance_filters_server_side(conn, client):
    _seed_feed(conn)
    everything = client.get("/events").json()["events"]
    threshold = max(e["importance"] for e in everything)
    filtered = client.get("/events", params={"minImportance": threshold}).json()["events"]
    assert len(filtered) == 1
    assert filtered[0]["importance"] == threshold


def test_type_filter(conn, client):
    _seed_feed(conn)
    body = client.get("/events", params={"types": "regulatory_action"}).json()
    assert body["events"]
    assert {e["eventType"] for e in body["events"]} == {"regulatory_action"}


def test_enrichment_state_filter_is_the_workers_queue(conn, client):
    """§9.9: without this parameter the enrichment worker cannot express "give me the raw ones"."""
    _seed_feed(conn)
    raw = client.get("/events", params={"enrichmentState": "raw"}).json()["events"]
    assert len(raw) == 3
    enriched = client.get("/events", params={"enrichmentState": "enriched"}).json()["events"]
    assert enriched == []


def test_cursor_walks_every_event_exactly_once(conn, client):
    """The order is total — (publishedAt DESC, id DESC) — so the cursor cannot skip a row."""
    for i in range(7):
        insert_document(
            conn,
            f"doc_cursor000000{i:04d}",
            title=f"Distinct NVIDIA development number {i} reported by the trade press",
            published_at=f"2026-08-0{i + 1}T13:00:00Z",
        )
    events_mod.assemble(conn)

    seen, cursor, pages = [], None, 0
    while True:
        params = {"limit": 2}
        if cursor:
            params["cursor"] = cursor
        body = client.get("/events", params=params).json()
        seen.extend(e["id"] for e in body["events"])
        cursor = body["nextCursor"]
        pages += 1
        if not cursor or pages > 10:
            break
    assert len(seen) == 7
    assert len(set(seen)) == 7


def test_as_of_excludes_a_backfilled_document(conn, client):
    """The first_seen_at half of the §1.1 predicate, end to end through the endpoint."""
    insert_document(
        conn,
        "doc_visible0000000a1",
        title="NVIDIA reports record data centre revenue for the quarter",
        published_at="2026-08-01T13:00:00Z",
        first_seen_at="2026-08-01T13:00:00Z",
    )
    insert_document(
        conn,
        "doc_backfilled00000a",
        title="Older NVIDIA supply agreement surfaces in a trade publication report",
        published_at="2026-07-01T13:00:00Z",   # published BEFORE the cutoff
        first_seen_at="2026-08-16T09:00:00Z",  # but first seen AFTER it
    )
    events_mod.assemble(conn)

    ids = [e["id"] for e in client.get("/events", params={"as_of": "2026-08-10T00:00:00Z"}).json()["events"]]
    assert len(ids) == 1
    # published_at alone would wrongly include it; both halves must pass.
    later = client.get("/events", params={"as_of": "2026-08-20T00:00:00Z"}).json()["events"]
    assert len(later) == 2


# ---- §9.13: historical reads project event_state_history -----------------------------------------


def test_historical_read_projects_the_state_history_row(conn, client):
    """as_of controls row visibility, never field values."""
    insert_document(
        conn,
        "doc_state000000000a1",
        title="US announces new export restrictions on NVIDIA advanced AI accelerators",
        published_at="2026-08-01T13:00:00Z",
        first_seen_at="2026-08-01T13:00:00Z",
    )
    events_mod.assemble(conn)
    at_create = conn.execute("SELECT importance, document_count FROM events").fetchone()

    # A second, higher-tier document about the same occurrence — same UTC day, so it joins the
    # cluster — but FIRST SEEN a week later, so a cutoff between the two separates them.
    insert_document(
        conn,
        "doc_state000000000a2",
        title="US announces new export restrictions on NVIDIA advanced AI accelerators",
        published_at="2026-08-01T18:00:00Z",
        first_seen_at="2026-08-08T13:00:00Z",
        provider="sec-edgar",
        tier="official",
    )
    events_mod.assemble(conn)
    now_row = conn.execute("SELECT importance, document_count FROM events").fetchone()
    assert now_row["document_count"] == 2
    assert now_row["importance"] > at_create["importance"]

    historical = client.get("/events", params={"as_of": "2026-08-05T00:00:00Z"}).json()["events"][0]
    assert historical["documentCount"] == at_create["document_count"]
    assert historical["importance"] == at_create["importance"]
    assert historical["sourceTier"] == "professional"
    assert historical["officialConfirmed"] is False

    live = client.get("/events").json()["events"][0]
    assert live["documentCount"] == 2
    assert live["importance"] == now_row["importance"]
    assert live["sourceTier"] == "official"


# ---- §9.12: point-in-time enrichment visibility --------------------------------------------------


def _enrich(conn, event_id, *, enrichment_as_of):
    conn.execute(
        "UPDATE events SET enrichment_state='enriched', why_it_matters=?, key_facts=?, "
        "enrichment_as_of=?, enriched_at=?, model_used=?, prompt_version=?, schema_version=?, "
        "reasoning_mode=? WHERE id=?",
        (
            "May materially affect high-end accelerator demand.",
            json.dumps(["a hypothesis-adjacent fact"]),
            enrichment_as_of,
            enrichment_as_of,
            "qwen3-14b",
            "event-agent@1",
            "event@1",
            "non_thinking",
            event_id,
        ),
    )
    conn.execute(
        "UPDATE event_tickers SET potential_direction='negative', potential_strength=0.8, "
        "expected_horizon='medium_term', affected_dimensions=?, transmission=? WHERE event_id=?",
        (json.dumps(["revenue_exposure"]), "Exposure runs through data-centre revenue.", event_id),
    )
    conn.commit()


def test_past_as_of_hides_enrichment_computed_later(conn, client):
    insert_document(
        conn,
        "doc_pit00000000000a1",
        title="US announces new export restrictions on NVIDIA advanced AI accelerators",
        published_at="2026-08-01T13:00:00Z",
    )
    events_mod.assemble(conn)
    event_id = conn.execute("SELECT id FROM events").fetchone()["id"]
    _enrich(conn, event_id, enrichment_as_of="2026-08-09T13:00:00Z")

    hidden = client.get("/events", params={"as_of": "2026-08-05T00:00:00Z"}).json()["events"][0]
    assert hidden["enrichmentState"] == "raw"
    assert "whyItMatters" not in hidden
    assert "keyFacts" not in hidden
    assert hidden["audit"] is None
    assert [ticker["ticker"] for ticker in hidden["tickers"]] == ["NVDA"]
    for t in hidden["tickers"]:
        for field in events_mod.POTENTIAL_FIELDS:
            assert field not in t
        # relevance and isPrimary are code's and are NOT gated (§9.15).
        assert t["relevance"] == 1.0
        assert t["isPrimary"] is True


def test_past_as_of_shows_enrichment_computed_before_the_cutoff(conn, client):
    insert_document(
        conn,
        "doc_pit00000000000a2",
        title="US announces new export restrictions on NVIDIA advanced AI accelerators",
        published_at="2026-08-01T13:00:00Z",
    )
    events_mod.assemble(conn)
    event_id = conn.execute("SELECT id FROM events").fetchone()["id"]
    _enrich(conn, event_id, enrichment_as_of="2026-08-02T13:00:00Z")

    shown = client.get("/events", params={"as_of": "2026-08-05T00:00:00Z"}).json()["events"][0]
    assert shown["enrichmentState"] == "enriched"
    assert shown["whyItMatters"].startswith("May materially affect")
    assert shown["keyFacts"] == ["a hypothesis-adjacent fact"]
    assert shown["audit"]["promptVersion"] == "event-agent@1"
    assert shown["audit"]["reasoningMode"] == "non_thinking"
    assert shown["tickers"][0]["potentialDirection"] == "negative"


def test_live_read_is_unaffected_by_the_enrichment_cutoff(conn, client):
    insert_document(
        conn,
        "doc_pit00000000000a3",
        title="US announces new export restrictions on NVIDIA advanced AI accelerators",
        published_at="2026-08-01T13:00:00Z",
    )
    events_mod.assemble(conn)
    event_id = conn.execute("SELECT id FROM events").fetchone()["id"]
    _enrich(conn, event_id, enrichment_as_of="2026-08-09T13:00:00Z")

    live = client.get("/events").json()["events"][0]
    assert live["enrichmentState"] == "enriched"
    assert "whyItMatters" in live


# ---- GET /events/{id} ----------------------------------------------------------------------------


def test_event_detail_shape(conn, client):
    insert_document(
        conn,
        "doc_detail00000000a1",
        title="US announces new export restrictions on NVIDIA advanced AI accelerators",
        published_at="2026-08-01T13:00:00Z",
        provider="sec-edgar",
        tier="official",
    )
    events_mod.assemble(conn)
    event_id = conn.execute("SELECT id FROM events").fetchone()["id"]

    body = client.get(f"/events/{event_id}").json()
    assert set(body) == {"event", "documents", "tickers", "asOf", "degraded"}
    assert body["event"]["id"] == event_id
    assert body["documents"][0]["provider"] == "sec-edgar"
    assert body["tickers"][0]["ticker"] == "NVDA"


def test_unknown_event_is_a_404(client):
    assert client.get("/events/evt_0000000000000000").status_code == 404


@pytest.mark.parametrize("params", [
    {"as_of": "not-a-timestamp"},
    {"since": "not-a-timestamp"},
    {"enrichmentState": "pending"},
    {"cursor": "not-a-cursor-this-endpoint-issued"},
])
def test_malformed_query_parameters_are_400s_not_500s(client, params):
    assert client.get("/events", params=params).status_code == 400


# ---- Phase 6: D-29 survival ----------------------------------------------------------------------


def test_event_attribution_survives_the_retention_sweep(conn, client):
    """§3.2 / §3.7: the document row goes, the event keeps documentCount and the URL list."""
    insert_document(
        conn,
        "doc_sweep000000000a1",
        title="US announces new export restrictions on NVIDIA advanced AI accelerators",
        published_at="2026-08-01T13:00:00Z",
        url="https://example.test/first",
    )
    insert_document(
        conn,
        "doc_sweep000000000a2",
        title="US announces new export restrictions on NVIDIA advanced AI accelerators",
        published_at="2026-08-01T14:00:00Z",
        provider="google-news-rss",
        url="https://example.test/second",
    )
    events_mod.assemble(conn)
    event_id = conn.execute("SELECT id FROM events").fetchone()["id"]

    conn.execute("DELETE FROM source_documents")
    conn.commit()
    assert conn.execute("SELECT COUNT(*) AS n FROM source_documents").fetchone()["n"] == 0

    body = client.get(f"/events/{event_id}").json()
    assert body["event"]["documentCount"] == 2
    assert [s["url"] for s in body["event"]["sources"]] == [
        "https://example.test/first",
        "https://example.test/second",
    ]
    assert {s["sourceTier"] for s in body["event"]["sources"]} == {"professional"}
    assert len(body["documents"]) == 2


# ---- Phase 6: the enrichment intake --------------------------------------------------------------


def _raw_event(conn, client, *, title="US announces new export restrictions on advanced AI accelerators",
               excerpt="The administration said the measures cover advanced AI accelerators."):
    insert_document(
        conn,
        "doc_enrich00000000a1",
        title=title,
        excerpt=excerpt,
        published_at="2026-08-01T13:00:00Z",
        provider="sec-edgar",
        tier="official",
        raw_tickers=("NVDA", "AMD"),
    )
    events_mod.assemble(conn)
    return conn.execute("SELECT id FROM events").fetchone()["id"]


VALID_PAYLOAD = {
    "whyItMatters": "May materially affect high-end accelerator demand.",
    "keyFacts": ["The administration said the measures cover advanced AI accelerators."],
    "tickers": [
        {
            "ticker": "NVDA",
            "potentialDirection": "negative",
            "potentialStrength": 0.8,
            "expectedHorizon": "medium_term",
            "affectedDimensions": ["revenue_exposure", "growth"],
            "transmission": "A material share of data-centre revenue is exposed.",
        }
    ],
    "audit": {
        "modelUsed": "qwen3-14b",
        "promptVersion": "event-agent@1",
        "schemaVersion": "event@1",
        "reasoningMode": "non_thinking",
        "enrichedAt": "2026-08-02T13:00:00Z",
    },
    "enrichmentAsOf": "2026-08-02T13:00:00Z",
}


def test_enrichment_happy_path(conn, client):
    event_id = _raw_event(conn, client)
    resp = client.post(f"/events/{event_id}/enrichment", json=VALID_PAYLOAD)
    assert resp.status_code == 200
    event = resp.json()["event"]
    assert event["enrichmentState"] == "enriched"
    assert event["whyItMatters"] == VALID_PAYLOAD["whyItMatters"]
    assert event["audit"]["reasoningMode"] == "non_thinking"
    nvda = [t for t in event["tickers"] if t["ticker"] == "NVDA"][0]
    assert nvda["potentialDirection"] == "negative"
    assert nvda["relevance"] == 1.0  # untouched by the model


@pytest.mark.parametrize(
    "field, value",
    [
        ("importance", 0.99),
        ("novelty", 0.99),
        ("relevance", 0.99),
        ("clusterKey", "deadbeefdeadbeef"),
        ("occurredAt", "2026-08-01T00:00:00Z"),
        ("publishedAt", "2026-08-01T00:00:00Z"),
        ("firstSeenAt", "2026-08-01T00:00:00Z"),
        ("sourceTier", "discussion"),
        ("officialConfirmed", False),
    ],
)
def test_enrichment_rejects_every_deterministic_field(conn, client, field, value):
    """§9.15: these are code's, deterministic and versioned. A model may not overwrite them."""
    event_id = _raw_event(conn, client)
    payload = json.loads(json.dumps(VALID_PAYLOAD))
    payload[field] = value
    resp = client.post(f"/events/{event_id}/enrichment", json=payload)
    assert resp.status_code == 400
    assert field in resp.json()["detail"]


def test_enrichment_rejects_relevance_nested_under_a_ticker(conn, client):
    event_id = _raw_event(conn, client)
    payload = json.loads(json.dumps(VALID_PAYLOAD))
    payload["tickers"][0]["relevance"] = 0.5
    resp = client.post(f"/events/{event_id}/enrichment", json=payload)
    assert resp.status_code == 400
    assert "relevance" in resp.json()["detail"]


@pytest.mark.parametrize(
    "field, value",
    [
        ("potentialDirection", "up"),
        ("expectedHorizon", "20_trading_days"),
        ("affectedDimensions", ["vibes"]),
        ("potentialStrength", 1.4),
    ],
)
def test_enrichment_rejects_an_invalid_enum_rather_than_coercing(conn, client, field, value):
    event_id = _raw_event(conn, client)
    payload = json.loads(json.dumps(VALID_PAYLOAD))
    payload["tickers"][0][field] = value
    assert client.post(f"/events/{event_id}/enrichment", json=payload).status_code == 400


def test_enrichment_accepts_unclear_and_neutral(conn, client):
    event_id = _raw_event(conn, client)
    payload = json.loads(json.dumps(VALID_PAYLOAD))
    payload["tickers"][0]["potentialDirection"] = "unclear"
    resp = client.post(f"/events/{event_id}/enrichment", json=payload)
    assert resp.status_code == 200
    assert resp.json()["event"]["tickers"][0]["potentialDirection"] == "unclear"


def test_enrichment_rejects_a_ticker_the_event_does_not_carry(conn, client):
    event_id = _raw_event(conn, client)
    payload = json.loads(json.dumps(VALID_PAYLOAD))
    payload["tickers"][0]["ticker"] = "MSFT"
    resp = client.post(f"/events/{event_id}/enrichment", json=payload)
    assert resp.status_code == 400
    assert "MSFT" in resp.json()["detail"]


def test_enrichment_rejects_a_fabricated_key_fact(conn, client):
    """§9.14: key_facts is a FACT column, so code verifies it against the supporting documents."""
    event_id = _raw_event(conn, client)
    payload = json.loads(json.dumps(VALID_PAYLOAD))
    payload["keyFacts"] = ["The company confirmed a fourteen billion dollar buyback programme."]
    resp = client.post(f"/events/{event_id}/enrichment", json=payload)
    assert resp.status_code == 400
    assert "keyFacts" in resp.json()["detail"]
    # The whole payload is rejected — nothing partial is written.
    row = conn.execute("SELECT enrichment_state, why_it_matters FROM events").fetchone()
    assert row["enrichment_state"] == "raw"
    assert row["why_it_matters"] is None


def test_enrichment_rejects_an_event_type_that_contradicts_the_deterministic_guess(conn, client):
    event_id = _raw_event(conn, client)
    payload = json.loads(json.dumps(VALID_PAYLOAD))
    payload["eventType"] = "partnership"
    resp = client.post(f"/events/{event_id}/enrichment", json=payload)
    assert resp.status_code == 400
    assert "eventType" in resp.json()["detail"]


def test_enrichment_accepts_an_echoed_event_type(conn, client):
    event_id = _raw_event(conn, client)
    stored = conn.execute("SELECT event_type FROM events").fetchone()["event_type"]
    payload = json.loads(json.dumps(VALID_PAYLOAD))
    payload["eventType"] = stored
    assert client.post(f"/events/{event_id}/enrichment", json=payload).status_code == 200


def test_enrichment_appends_and_deduplicates_key_facts(conn, client):
    event_id = _raw_event(conn, client)
    client.post(f"/events/{event_id}/enrichment", json=VALID_PAYLOAD)
    body = client.post(f"/events/{event_id}/enrichment", json=VALID_PAYLOAD).json()
    facts = body["event"]["keyFacts"]
    assert len(facts) == len(set(facts))


def test_enrichment_without_a_cutoff_stays_invisible_to_historical_reads(conn, client):
    """§9.12: `enrichmentAsOf` has NO fallback to `enrichedAt`.

    One is a cutoff, the other a wall clock; substituting one for the other is exactly how
    future-informed hypotheses leak into a historical read. A worker that omits it gets NULL, and
    NULL means "invisible at every past cutoff" — the fail-safe direction.
    """
    event_id = _raw_event(conn, client)
    payload = json.loads(json.dumps(VALID_PAYLOAD))
    payload.pop("enrichmentAsOf")
    assert client.post(f"/events/{event_id}/enrichment", json=payload).status_code == 200
    assert conn.execute("SELECT enrichment_as_of FROM events").fetchone()["enrichment_as_of"] is None

    hidden = client.get("/events", params={"as_of": "2026-08-05T00:00:00Z"}).json()["events"][0]
    assert hidden["enrichmentState"] == "raw"
    assert "whyItMatters" not in hidden
    # ...while the live read still shows it.
    assert "whyItMatters" in client.get("/events").json()["events"][0]


def test_enrichment_on_an_unknown_event_is_a_404(client):
    assert client.post("/events/evt_0000000000000000/enrichment", json=VALID_PAYLOAD).status_code == 404


# ---- Phase 7: reproducibility --------------------------------------------------------------------

BUILD_SCRIPT = r"""
import json, sys
sys.path.insert(0, %(root)r)
from app.db import connect, migrate
from app import events as events_mod

FIXTURE = json.load(open(%(fixture)r))
conn = connect(sys.argv[1])
migrate(conn)
for d in FIXTURE:
    conn.execute(
        "INSERT INTO source_documents (id, content_hash, provider, source_tier, url, title, "
        "excerpt, published_at, first_seen_at, retrieved_at, raw_tickers, ingest_run_id) "
        "VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
        (d["id"], "hash-" + d["id"], d["provider"], d["tier"], "https://example.test/" + d["id"],
         d["title"], d.get("excerpt", ""), d["published_at"], d["published_at"], d["published_at"],
         json.dumps(d["raw_tickers"]), "run_0123456789abcdef"),
    )
conn.commit()
events_mod.assemble(conn)

dump = {}
for table, order in (("events", "id"), ("event_documents", "event_id, document_id"),
                     ("event_tickers", "event_id, ticker"), ("macro_events", "id"),
                     ("event_state_history", "event_id, as_of")):
    rows = conn.execute("SELECT * FROM %%s ORDER BY %%s" %% (table, order)).fetchall()
    dump[table] = [[repr(v) for v in r.values()] for r in rows]
conn.close()
print(json.dumps(dump, sort_keys=True))
"""


REPRO_FIXTURE = [
    {"id": "doc_repro0000000001", "provider": "marketaux", "tier": "professional",
     "title": "US announces new export restrictions on advanced AI accelerators",
     "published_at": "2026-08-01T13:00:00Z", "raw_tickers": ["NVDA", "AMD"]},
    {"id": "doc_repro0000000002", "provider": "google-news-rss", "tier": "professional",
     "title": "US announces new export restrictions on advanced AI accelerators sold to China",
     "published_at": "2026-08-01T13:40:00Z", "raw_tickers": ["NVDA"]},
    {"id": "doc_repro0000000003", "provider": "sec-edgar", "tier": "official",
     "title": "8-K filed 2026-08-02 - Results of Operations",
     "published_at": "2026-08-02T13:00:00Z", "raw_tickers": ["NVDA"]},
    {"id": "doc_repro0000000004", "provider": "marketaux", "tier": "professional",
     "title": "Tesla opens a new assembly plant in Monterrey this quarter",
     "published_at": "2026-08-03T13:00:00Z", "raw_tickers": ["TSLA"]},
    {"id": "doc_repro0000000005", "provider": "marketaux", "tier": "professional",
     "title": "Alphabet and NVIDIA expand their cloud accelerator partnership",
     "published_at": "2026-08-04T13:00:00Z", "raw_tickers": ["GOOGL", "NVDA"]},
]


def _build_in_subprocess(db_path, fixture_path, hashseed):
    script = BUILD_SCRIPT % {"root": str(SERVICE_ROOT), "fixture": str(fixture_path)}
    env = dict(os.environ, PYTHONHASHSEED=hashseed)
    out = subprocess.run(
        [sys.executable, "-c", script, str(db_path)],
        capture_output=True, text=True, env=env, cwd=str(SERVICE_ROOT), check=True,
    )
    return out.stdout


@pytest.mark.parametrize("hashseed", ["0", "1"])
def test_two_runs_identical(tmp_path, hashseed):
    """The guarantee Wave 5A's ablation ladder rests on: same inputs, byte-identical rows.

    Two SEPARATE processes, so a per-process hash salt would show up as a diff. Run under both
    PYTHONHASHSEED=0 and =1 — a stray builtin hash() would pass one and fail the other.
    """
    fixture = tmp_path / "fixture.json"
    fixture.write_text(json.dumps(REPRO_FIXTURE), encoding="utf-8")
    first = _build_in_subprocess(tmp_path / "one", fixture, hashseed)
    second = _build_in_subprocess(tmp_path / "two", fixture, hashseed)
    assert first == second
    assert json.loads(first)["events"], "the fixture must actually produce events"


# ---- §9.41.5: the macro_events backfill (Wave 2 Lane 2A) -----------------------------------------
#
# 1C writes the release row at ingest with `event_id` NULL — it has the numbers from FRED and FMP
# but no event, because events only exist once `assemble()` has run on the document that same pass
# stored. The document carries `macro_key`; `assemble()` turns it into the link. Only `event_id` is
# written here: `actual`, `expected`, `previous` and `surprise` stay 1C's, sourced from the
# structured provider responses, because re-deriving them from assembled article text would put a
# model-touchable number into a fact column (§9.41.4).

MACRO_SERIES = "CPI"
MACRO_RELEASE_AT = "2026-09-10T12:30:00Z"
#: Before `release_at`, so §3.9's timing guard is satisfied and `surprise` is actually computable.
#: A test whose `surprise` is NULL would pass while proving nothing about it surviving.
MACRO_CAPTURED_AT = "2026-09-01T00:00:00Z"


def ingest_macro_release(conn, *, doc_id="doc_macro_cpi", series=MACRO_SERIES,
                         release_at=MACRO_RELEASE_AT, actual=3.4, expected=3.2, previous=3.1):
    """Drive one release through 1C's writers, exactly as an ingest pass does.

    `store_macro_releases` and `providers.macro_key` are the real thing, not a re-implementation —
    the point of this test is that 2A's backfill meets 1C's output, so a hand-built row would prove
    nothing about the join actually holding.
    """
    from app.ingest import store_macro_releases
    from app.providers import macro_key

    store_macro_releases(
        conn,
        [{"series": series, "release_at": release_at, "actual": actual, "expected": expected,
          "previous": previous, "unit": "percent", "expectation_source": "fmp"}],
        now=MACRO_CAPTURED_AT,
    )
    insert_document(
        conn, doc_id,
        title=f"{series} release for {release_at[:10]}",
        published_at=release_at,
        provider="fred",
        tier="official",
        raw_tickers=(),  # a macro release is market-wide; naming a subject company is an inference
    )
    conn.execute(
        "UPDATE source_documents SET macro_key = ? WHERE id = ?",
        (macro_key(series, release_at), doc_id),
    )
    conn.commit()
    return doc_id


def macro_row(conn, series=MACRO_SERIES):
    return conn.execute("SELECT * FROM macro_events WHERE series = ?", (series,)).fetchone()


def test_assemble_backfills_event_id_and_leaves_the_numbers_alone(conn):
    """The lane's whole reason to exist: `surprise` reaches a consumer (§9.41.5, Wave 5A rung F)."""
    ingest_macro_release(conn)

    before = macro_row(conn)
    assert before["event_id"] is None, "1C must write the row unlinked; otherwise nothing is proven"
    assert before["surprise"] == pytest.approx(3.4 - 3.2), "surprise must be computable pre-assembly"

    created = events_mod.assemble(conn)
    assert len(created) == 1

    after = macro_row(conn)
    assert after["event_id"] == created[0], "assemble must link the row to the event it assembled"
    # The numbers are 1C's and 2A does not touch them — asserted field by field rather than by
    # comparing `surprise` alone, because a backfill that rewrote `actual` would still leave a
    # surprise that happened to match.
    assert (after["actual"], after["expected"], after["previous"]) == (3.4, 3.2, 3.1)
    assert after["surprise"] == before["surprise"]
    assert after["expectation_captured_at"] == before["expectation_captured_at"]
    assert after["expectation_source"] == before["expectation_source"]
    assert after["id"] == before["id"], "the natural key must not fork a second row"


def test_a_second_assemble_over_the_same_documents_is_a_no_op(conn):
    """Idempotence, twice over: `assemble()` selects only documents no `event_documents` row
    references, so the second pass never revisits this document — and the UPDATE would be a no-op
    if it did. Neither duplicates the release row nor orphans the link."""
    ingest_macro_release(conn)
    created = events_mod.assemble(conn)
    first = dict(macro_row(conn))
    event_count = conn.execute("SELECT COUNT(*) AS n FROM events").fetchone()["n"]

    again = events_mod.assemble(conn)

    assert again == [], "a second pass must assemble nothing"
    assert conn.execute("SELECT COUNT(*) AS n FROM events").fetchone()["n"] == event_count
    assert conn.execute("SELECT COUNT(*) AS n FROM macro_events").fetchone()["n"] == 1
    assert dict(macro_row(conn)) == first, "the release row must be byte-identical after re-assembly"
    assert macro_row(conn)["event_id"] == created[0], "the link must not be orphaned"


def test_a_document_with_no_macro_key_changes_nothing(conn):
    """Every non-macro document takes this path — it must not touch `macro_events` at all."""
    ingest_macro_release(conn)  # an unrelated release row, to be left strictly alone
    before = dict(macro_row(conn))

    insert_document(conn, "doc_plain", title="NVIDIA ships something",
                    published_at="2026-09-11T00:00:00Z")
    conn.execute("DELETE FROM source_documents WHERE id = 'doc_macro_cpi'")
    conn.commit()

    events_mod.assemble(conn)

    assert dict(macro_row(conn)) == before
    assert macro_row(conn)["event_id"] is None, "a keyless document must link nothing"


def test_a_malformed_macro_key_links_nothing_and_does_not_break_the_pass(conn):
    """An unparseable key is a provider defect, not a reason to lose every other event in the pass."""
    ingest_macro_release(conn)
    conn.execute("UPDATE source_documents SET macro_key = ? WHERE id = ?",
                 ("CPI|not-a-timestamp", "doc_macro_cpi"))
    insert_document(conn, "doc_other", title="An unrelated headline",
                    published_at="2026-09-12T00:00:00Z")
    conn.commit()

    created = events_mod.assemble(conn)

    assert len(created) == 2, "the malformed key must not abort the pass"
    assert macro_row(conn)["event_id"] is None, "a key that resolves to nothing links nothing"


def test_a_re_ingest_that_wipes_the_link_is_repaired_by_the_next_assemble(conn):
    """The reason the backfill reconciles the whole store instead of only this pass's documents.

    `record_macro_release` upserts with `event_id=None`, so a second ingest pass over
    the same release — routine, and described in `store_macro_releases`'s own docstring — resets the
    link to NULL. `assemble()` never revisits a document already in `event_documents`, so a
    per-document backfill would lose the link permanently after one pass and rung F would go quietly
    dark. `ingest.py` runs `store_macro_releases` before `assemble()`, so the repair lands in the
    same pass as the wipe.
    """
    from app.ingest import store_macro_releases

    ingest_macro_release(conn)
    created = events_mod.assemble(conn)
    assert macro_row(conn)["event_id"] == created[0]

    # A second pass sees the same release again — FMP re-reporting a print is normal.
    store_macro_releases(
        conn,
        [{"series": MACRO_SERIES, "release_at": MACRO_RELEASE_AT, "actual": 3.4, "expected": 3.2,
          "previous": 3.1, "unit": "percent", "expectation_source": "fmp"}],
        now="2026-09-02T00:00:00Z",
    )
    assert macro_row(conn)["event_id"] is None, "the premise: re-ingest really does wipe the link"

    assert events_mod.assemble(conn) == [], "no new document, so no new event"
    after = macro_row(conn)
    assert after["event_id"] == created[0], "the next assemble must restore the link"
    assert after["surprise"] == pytest.approx(3.4 - 3.2), "and the numbers must still be 1C's"
