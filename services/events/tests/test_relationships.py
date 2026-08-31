"""Phase 3 — event↔ticker relationships: derived or cited, never fabricated.

The rule under test throughout: every relationship row can name where it came from. There are
exactly three provenances (`derived`, `reference`, `evidence`) and no path in the codebase writes a
fourth — in particular, nothing writes one from a model.
"""
from __future__ import annotations

import json
from datetime import datetime, timedelta, timezone

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from app import relationships as rel_module
from app.db import connect, migrate
from app.ingest import store_scheduled_events
from app.macro import router as macro_router
from app.relationships import (
    BAND_CONTEXTUAL,
    BAND_PRIMARY,
    CALC_VERSION,
    RELATIONSHIPS,
    SOURCE_DERIVED,
    SOURCE_REFERENCE,
    counterparties_of,
    derive_relationships,
    rebuild_relationships,
    relationships_for_event,
    router as relationships_router,
    store_relationships,
)

UNIVERSE = ("NVDA", "GOOGL", "TSLA")
T0 = datetime(2026, 8, 23, 12, 0, 0, tzinfo=timezone.utc)


@pytest.fixture()
def conn(monkeypatch):
    monkeypatch.delenv("RELATIONSHIP_REGISTRY_PATH", raising=False)
    c = connect()
    migrate(c)
    yield c
    c.close()


@pytest.fixture()
def client(conn):
    app = FastAPI()
    app.include_router(relationships_router)
    app.include_router(macro_router)
    return TestClient(app)


def _event(conn, **overrides):
    row = {
        "kind": "earnings", "ticker": "NVDA", "series": None,
        "scheduled_at": "2026-11-18T22:00:00Z", "confirmed": 1, "status": "confirmed",
        "source": "company-ir", "source_tier": "official", "title": "NVIDIA earnings",
        "occurrence_key": overrides.pop("occurrence_key", "earnings|NVDA|2026-10-31"),
    }
    row.update(overrides)
    store_scheduled_events(conn, [row], now="2026-08-01T00:00:00Z")
    return conn.execute(
        "SELECT * FROM scheduled_events WHERE occurrence_key = ?", (row["occurrence_key"],)
    ).fetchone()


# ── derivation ───────────────────────────────────────────────────────────────────────────────────


def test_a_company_event_is_direct_for_its_own_subject():
    rows = derive_relationships({"kind": "earnings", "ticker": "NVDA"}, universe=UNIVERSE)
    direct = [r for r in rows if r["relationship"] == "direct"]
    assert [r["ticker"] for r in direct] == ["NVDA"]
    assert direct[0]["source"] == SOURCE_DERIVED
    assert direct[0]["sourceRef"] == "event.ticker"
    assert direct[0]["band"] == BAND_PRIMARY


def test_one_event_reaches_several_tickers_with_different_relationship_types():
    rows = derive_relationships({"kind": "earnings", "ticker": "NVDA"}, universe=UNIVERSE)
    by_ticker = {}
    for row in rows:
        by_ticker.setdefault(row["ticker"], set()).add(row["relationship"])

    assert by_ticker["NVDA"] == {"direct"}
    # TSMC supplies NVIDIA, so an NVIDIA event is CUSTOMER news for TSMC.
    assert "customer" in by_ticker["TSM"]
    assert "competitor" in by_ticker["AMD"]
    # AMD is also a sector peer; both types are recorded, not collapsed.
    assert "sector" in by_ticker["AMD"]
    # Hyperscalers are NVIDIA's customers, so an NVIDIA event is SUPPLIER news for them.
    assert "supplier" in by_ticker["MSFT"]


def test_every_relationship_type_is_in_the_closed_vocabulary():
    rows = derive_relationships({"kind": "earnings", "ticker": "NVDA"}, universe=UNIVERSE)
    assert rows, "the fixture must produce rows for this assertion to mean anything"
    for row in rows:
        assert row["relationship"] in RELATIONSHIPS


def test_every_row_carries_a_reason_and_a_provenance():
    rows = derive_relationships({"kind": "earnings", "ticker": "NVDA"}, universe=UNIVERSE)
    for row in rows:
        assert row["reason"].strip(), row
        assert row["source"] in (SOURCE_DERIVED, SOURCE_REFERENCE)
        assert row["sourceRef"].strip(), row


def test_a_reference_relationship_cites_where_it_came_from():
    rows = {r["ticker"]: r for r in derive_relationships(
        {"kind": "earnings", "ticker": "NVDA"}, universe=UNIVERSE)}
    tsm = rows["TSM"]
    assert tsm["source"] == SOURCE_REFERENCE
    assert "seed/nvda.json" in tsm["sourceRef"]
    assert "CoWoS" in tsm["reason"]


def test_a_macro_release_is_macro_for_the_covered_universe_and_nothing_else():
    rows = derive_relationships(
        {"kind": "macro_release", "ticker": None, "series": "CPI"}, universe=UNIVERSE)
    assert {r["ticker"] for r in rows} == set(UNIVERSE)
    assert {r["relationship"] for r in rows} == {"macro"}
    assert all(r["band"] == BAND_CONTEXTUAL for r in rows)
    # A macro release has no supply chain and no sector: it is not a company event.
    assert not any(r["relationship"] in ("supplier", "customer", "competitor") for r in rows)


def test_a_central_bank_event_is_market_wide_too():
    rows = derive_relationships(
        {"kind": "central_bank", "ticker": None, "series": "FOMC"}, universe=("NVDA",))
    assert [(r["ticker"], r["relationship"]) for r in rows] == [("NVDA", "macro")]


def test_a_company_event_with_no_company_relates_to_nothing():
    """An unattributed event is not a market-wide one. Silence beats a guess."""
    assert derive_relationships({"kind": "earnings", "ticker": None}, universe=UNIVERSE) == []


def test_the_macro_fan_out_is_bounded_by_the_covered_universe():
    assert derive_relationships(
        {"kind": "macro_release", "series": "CPI"}, universe=()) == []


def test_relationships_are_deterministic():
    event = {"kind": "earnings", "ticker": "NVDA"}
    assert derive_relationships(event, universe=UNIVERSE) == derive_relationships(
        event, universe=UNIVERSE)


# ── the reference table is data ───────────────────────────────────────────────────────────────────


def test_the_reference_table_can_be_extended_by_configuration(monkeypatch, tmp_path):
    path = tmp_path / "relationships.json"
    path.write_text(json.dumps([{
        "subject": "ACME", "counterparty": "WIDGET", "relationship": "supplier",
        "reason": "Widget Co supplies Acme's primary component.",
        "sourceRef": "operator configuration, 2026 supplier disclosure",
        "band": "primary",
    }]), encoding="utf-8")
    monkeypatch.setenv("RELATIONSHIP_REGISTRY_PATH", str(path))

    rows = derive_relationships({"kind": "earnings", "ticker": "ACME"}, universe=("ACME",))
    widget = [r for r in rows if r["ticker"] == "WIDGET"]
    assert len(widget) == 1
    assert widget[0]["relationship"] == "customer"  # emitted direction: ACME is WIDGET's customer
    assert widget[0]["source"] == SOURCE_REFERENCE
    assert "operator configuration" in widget[0]["sourceRef"]
    # The built-ins are still there.
    assert any(r["ticker"] == "TSM" for r in derive_relationships(
        {"kind": "earnings", "ticker": "NVDA"}, universe=("NVDA",)))


def test_a_configured_relationship_without_a_reason_is_dropped_whole(monkeypatch, tmp_path):
    path = tmp_path / "relationships.json"
    path.write_text(json.dumps([
        {"subject": "ACME", "counterparty": "WIDGET", "relationship": "supplier",
         "sourceRef": "somewhere"},                       # no reason
        {"subject": "ACME", "counterparty": "GADGET", "relationship": "supplier",
         "reason": "stated", "sourceRef": ""},            # no citation
        {"subject": "ACME", "counterparty": "GIZMO", "relationship": "friend-of",
         "reason": "stated", "sourceRef": "somewhere"},   # outside the vocabulary
    ]), encoding="utf-8")
    monkeypatch.setenv("RELATIONSHIP_REGISTRY_PATH", str(path))
    assert derive_relationships({"kind": "earnings", "ticker": "ACME"}, universe=("ACME",)) == [
        {"ticker": "ACME", "relationship": "direct",
         "reason": "The event names this company as its subject.",
         "source": "derived", "sourceRef": "event.ticker", "band": "primary",
         "effectiveFrom": ""},
    ]


def test_an_unreadable_registry_falls_back_to_the_builtins(monkeypatch, tmp_path):
    monkeypatch.setenv("RELATIONSHIP_REGISTRY_PATH", str(tmp_path / "missing.json"))
    assert any(e["counterparty"] == "TSM" for e in rel_module.reference_table())


def test_the_inverse_of_a_relationship_is_declared_once():
    """Both directions come from one entry, so they cannot drift apart."""
    forward = {e["ticker"]: e["relationship"] for e in counterparties_of("NVDA")}
    backward = {e["ticker"]: e["relationship"] for e in counterparties_of("TSM")}
    assert forward["TSM"] == "customer"   # an NVDA event is customer news for TSM
    assert backward["NVDA"] == "supplier"  # a TSM event is supplier news for NVDA


# ── storage ───────────────────────────────────────────────────────────────────────────────────────


def test_storing_is_idempotent_and_first_seen_at_is_write_once(conn):
    event = _event(conn)
    rows = derive_relationships(dict(event), universe=UNIVERSE)

    assert store_relationships(conn, event["id"], rows, now=T0) == len(rows)
    first = [dict(r) for r in relationships_for_event(conn, event["id"])]

    assert store_relationships(conn, event["id"], rows, now=T0 + timedelta(days=1)) == 0
    second = [dict(r) for r in relationships_for_event(conn, event["id"])]

    assert len(first) == len(second)
    assert [r["first_seen_at"] for r in first] == [r["first_seen_at"] for r in second]
    assert all(r["calc_version"] == CALC_VERSION for r in second)


def test_a_relationship_not_yet_known_is_invisible_at_an_earlier_cutoff(conn):
    event = _event(conn)
    store_relationships(conn, event["id"], derive_relationships(
        dict(event), universe=UNIVERSE), now=T0)

    before = relationships_for_event(
        conn, event["id"], as_of=(T0 - timedelta(days=1)).strftime("%Y-%m-%dT%H:%M:%SZ"))
    after = relationships_for_event(
        conn, event["id"], as_of=(T0 + timedelta(days=1)).strftime("%Y-%m-%dT%H:%M:%SZ"))
    assert before == []
    assert len(after) > 0


def test_the_database_refuses_a_relationship_outside_the_vocabulary(conn):
    event = _event(conn)
    with pytest.raises(Exception):
        store_relationships(conn, event["id"], [{
            "ticker": "NVDA", "relationship": "friend-of", "reason": "x",
            "source": "derived", "sourceRef": "y", "band": "primary",
        }], now=T0)
    conn.rollback()


def test_rebuild_is_bounded_and_idempotent(conn):
    _event(conn)
    _event(conn, ticker="GOOGL", occurrence_key="earnings|GOOGL|2026-09-30",
           scheduled_at="2026-10-28T20:00:00Z", title="Alphabet earnings")

    first = rebuild_relationships(conn, universe=UNIVERSE, now=T0)
    assert first["events"] == 2 and first["inserted"] > 0

    second = rebuild_relationships(conn, universe=UNIVERSE, now=T0 + timedelta(days=1))
    assert second["inserted"] == 0, "a second pass must write nothing new"


# ── the read surface ──────────────────────────────────────────────────────────────────────────────


def test_the_read_returns_the_event_with_the_relationship_and_the_reason(conn, client):
    event = _event(conn)
    rebuild_relationships(conn, universe=UNIVERSE, now=T0)

    response = client.get("/relationships", params={"tickers": "TSM"})
    assert response.status_code == 200
    body = response.json()
    assert body["calcVersion"] == CALC_VERSION
    assert set(body["vocabulary"]) == set(RELATIONSHIPS)

    rows = body["relationships"]
    assert len(rows) == 1
    row = rows[0]
    assert row["ticker"] == "TSM"
    assert row["relationship"] == "customer"
    assert row["source"] == "reference"
    assert row["reason"]
    assert row["event"]["id"] == event["id"]
    assert row["event"]["scheduledAt"] == "2026-11-18T22:00:00Z"
    assert row["event"]["sourceTier"] == "official"


def test_the_read_is_point_in_time_on_both_the_event_and_the_relationship(conn, client):
    _event(conn)
    rebuild_relationships(conn, universe=UNIVERSE, now=T0)

    before = client.get("/relationships", params={
        "tickers": "NVDA", "as_of": "2026-08-22T00:00:00Z"}).json()
    assert before["relationships"] == []

    after = client.get("/relationships", params={
        "tickers": "NVDA", "as_of": "2026-08-24T00:00:00Z"}).json()
    assert len(after["relationships"]) == 1


def test_the_read_filters_by_window(conn, client):
    _event(conn)
    rebuild_relationships(conn, universe=UNIVERSE, now=T0)
    outside = client.get("/relationships", params={
        "tickers": "NVDA", "from": "2027-01-01T00:00:00Z", "to": "2027-02-01T00:00:00Z"}).json()
    assert outside["relationships"] == []


def test_the_reference_table_is_auditable_over_http(client):
    body = client.get("/relationships/reference").json()
    assert body["registrySource"] == "builtin"
    assert any(e["counterparty"] == "TSM" for e in body["relationships"])
    for entry in body["relationships"]:
        assert entry["reason"] and entry["sourceRef"], entry


def test_no_read_route_touches_a_provider_or_a_model(conn, client, monkeypatch):
    from app import providers as providers_module

    _event(conn)
    rebuild_relationships(conn, universe=UNIVERSE, now=T0)

    def explode(*_a, **_kw):
        raise AssertionError("a relationship read reached a provider")

    monkeypatch.setattr(providers_module.requests, "get", explode)
    assert client.get("/relationships", params={"tickers": "NVDA"}).status_code == 200
    assert client.get("/relationships/reference").status_code == 200
    assert client.get("/calendar", params={
        "from": "2026-01-01T00:00:00Z", "to": "2027-01-01T00:00:00Z"}).status_code == 200


def test_the_module_names_no_model():
    from pathlib import Path
    source = (Path(__file__).resolve().parent.parent / "app" / "relationships.py").read_text()
    assert "requests" not in source
    assert "LLM" not in source
    assert "8002" not in source
