"""Versioned repairs for canonical rows created before stricter resolver deployments."""
from __future__ import annotations

import json

import pytest

from app.db import connect, migrate
from app.repairs import MARKETAUX_HEADLINE_SUBJECT_REPAIR, apply_data_repairs

STAMP = "2026-08-15T13:21:00Z"


@pytest.fixture()
def conn():
    connection = connect()
    migrate(connection)
    try:
        yield connection
    finally:
        connection.close()


def insert_source(conn, document_id: str, title: str, tickers) -> None:
    conn.execute(
        "INSERT INTO source_documents (id,content_hash,provider,source_tier,url,title,excerpt,"
        "published_at,first_seen_at,retrieved_at,raw_tickers,ingest_run_id) "
        "VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
        (
            document_id, f"hash-{document_id}", "marketaux", "professional",
            f"https://example.test/{document_id}", title, "bounded excerpt", STAMP, STAMP,
            STAMP, json.dumps(tickers), "run_old_relevance",
        ),
    )


def insert_old_event(conn, event_id: str, document_id: str, title: str, links) -> None:
    conn.execute(
        "INSERT INTO events (id,event_type,title,summary,occurred_at,published_at,first_seen_at,"
        "source_tier,official_confirmed,importance,novelty,document_count,cluster_key) "
        "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
        (
            event_id, "other", title, "", STAMP, STAMP, STAMP, "professional", 0, 0.6, 1.0,
            1, f"cluster-{event_id}",
        ),
    )
    conn.execute(
        "INSERT INTO event_documents (event_id,document_id,url,provider,source_tier,published_at) "
        "VALUES (?,?,?,?,?,?)",
        (event_id, document_id, f"https://example.test/{document_id}", "marketaux",
         "professional", STAMP),
    )
    for ticker, primary in links:
        conn.execute(
            "INSERT INTO event_tickers (event_id,ticker,relevance,is_primary) VALUES (?,?,?,?)",
            (event_id, ticker, 1.0 if primary else 0.6, primary),
        )


def test_repair_removes_body_only_links_reassigns_primary_and_is_idempotent(conn):
    broadcom_title = "Broadcom shares rise after a product announcement"
    insert_source(conn, "doc_broadcom", broadcom_title, ["NVDA", "AVGO"])
    insert_old_event(
        conn, "evt_broadcom", "doc_broadcom", broadcom_title,
        [("NVDA", 1), ("AVGO", 0)],
    )
    spacex_title = "What's Going on with SpaceX Stock?"
    insert_source(conn, "doc_spacex", spacex_title, ["NVDA"])
    insert_old_event(conn, "evt_spacex", "doc_spacex", spacex_title, [("NVDA", 1)])
    conn.commit()

    first = apply_data_repairs(conn)

    assert first == {
        "applied": True,
        "eventsScanned": 2,
        "eventsRepaired": 2,
        "linksRemoved": 2,
        "primariesReassigned": 1,
        "eventsSkippedMissingSources": 0,
    }
    links = conn.execute(
        "SELECT event_id,ticker,is_primary FROM event_tickers ORDER BY event_id,ticker"
    ).fetchall()
    assert [tuple(row.values()) for row in links] == [("evt_broadcom", "AVGO", 1)]
    assert json.loads(conn.execute(
        "SELECT raw_tickers FROM source_documents WHERE id = 'doc_broadcom'"
    ).fetchone()["raw_tickers"]) == ["NVDA", "AVGO"]

    second = apply_data_repairs(conn)

    assert second == {**first, "applied": False}
    assert conn.execute(
        "SELECT count(*) AS n FROM event_data_repairs WHERE version = ?",
        (MARKETAUX_HEADLINE_SUBJECT_REPAIR,),
    ).fetchone()["n"] == 1


def test_repair_skips_an_event_when_retention_removed_its_source(conn):
    title = "SpaceX expands Starlink service"
    insert_old_event(conn, "evt_retained", "doc_expired", title, [("NVDA", 1)])
    conn.commit()

    result = apply_data_repairs(conn)

    assert result["eventsSkippedMissingSources"] == 1
    assert result["linksRemoved"] == 0
    assert conn.execute(
        "SELECT ticker FROM event_tickers WHERE event_id = 'evt_retained'"
    ).fetchone()["ticker"] == "NVDA"
