"""The document store: the content hash, D-29's caps, retention, and `GET /documents`.

Everything runs against a tmp_path database built by the REAL migration runner — never a
hand-written schema, because a test schema that drifts from `0001_init.sql` is worse than no test.
Nothing here has a key and nothing here has a network.
"""
from __future__ import annotations

import json
import re
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from app.config import (
    EXCERPT_CAP_OFFICIAL,
    EXCERPT_CAP_PROFESSIONAL,
    RETENTION_DISCUSSION_DAYS,
    RETENTION_PROFESSIONAL_DAYS,
)
from app.db import connect, migrate
from app.documents import (
    cap_excerpt,
    content_hash,
    normalise_text,
    normalise_url,
    run_retention_sweep,
    storable_body,
    store_documents,
)
from app.documents import router as documents_router

SERVICE_ROOT = Path(__file__).resolve().parent.parent
RUN_ID = "run_aaaaaaaaaaaaaaaa"


@pytest.fixture()
def conn(tmp_path, monkeypatch):
    c = connect()
    migrate(c)
    yield c
    c.close()


@pytest.fixture()
def client(conn):
    """A FastAPI app built in the test, per the lane's seam: `main.py` is not this lane's file."""
    app = FastAPI()
    app.include_router(documents_router)
    with TestClient(app) as c:
        yield c


def document(**overrides) -> dict:
    row = {
        "provider": "marketaux",
        "source_tier": "professional",
        "url": "https://example.test/a",
        "title": "NVIDIA reports record data-centre revenue",
        "excerpt": "bounded excerpt",
        "body": None,
        "published_at": "2026-08-10T12:00:00Z",
        "raw_tickers": ["NVDA"],
    }
    row.update(overrides)
    return row


# =================================================================================================
# The content hash (contract §3.1)
# =================================================================================================


def test_content_hash_recipe_is_pinned():
    """Every clause of the documented normalisation, asserted separately.

    This test is the reason the recipe can be trusted: changing it silently re-duplicates the whole
    corpus, because every stored row keeps its old hash and the next pass inserts a second copy of
    every article while `deduped` quietly drops to zero.
    """
    # title/body: NFKC + casefold + collapsed whitespace + strip
    assert normalise_text("  NVIDIA   Beats\tEstimates \n") == "nvidia beats estimates"
    assert normalise_text("ﬁle") == "file"  # NFKC decomposes the ligature
    assert normalise_text(None) == ""

    # url: lowercase scheme and host, drop the fragment, drop utm_*/fbclid, sort the rest
    assert normalise_url("HTTPS://Example.TEST/Path?b=2&a=1#frag") == \
        "https://example.test/Path?a=1&b=2"
    assert normalise_url("https://example.test/x?utm_source=news&fbclid=z&id=7") == \
        "https://example.test/x?id=7"
    # the PATH keeps its case: two case-different paths are two documents
    assert normalise_url("https://example.test/A") != normalise_url("https://example.test/a")

    # a missing body is the empty string, and the separator is a literal '|'
    import hashlib
    expected = hashlib.sha256("t|https://example.test/x|".encode("utf-8")).hexdigest()
    assert content_hash("T", "https://example.test/x", None) == expected
    assert content_hash("T", "https://example.test/x", None) == \
        content_hash("T", "https://example.test/x", "")


@pytest.mark.parametrize(
    "title,url",
    [
        ("  NVIDIA  beats \n estimates ", "https://example.test/a?x=1&y=2"),
        ("nvidia beats estimates", "HTTPS://EXAMPLE.TEST/a?y=2&x=1"),
        ("NVIDIA BEATS ESTIMATES", "https://example.test/a?y=2&utm_campaign=q3&x=1#top"),
    ],
)
def test_hash_is_stable_across_whitespace_case_tracking_and_query_order(title, url):
    baseline = content_hash("NVIDIA beats estimates", "https://example.test/a?x=1&y=2", None)
    assert content_hash(title, url, None) == baseline


def test_two_genuinely_different_documents_do_not_collide():
    a = content_hash("NVIDIA beats estimates", "https://example.test/a", None)
    b = content_hash("AMD beats estimates", "https://example.test/a", None)
    c = content_hash("NVIDIA beats estimates", "https://example.test/b", None)
    d = content_hash("NVIDIA beats estimates", "https://example.test/a", "full filing text")
    assert len({a, b, c, d}) == 4


# =================================================================================================
# Dedup and first_seen_at
# =================================================================================================


def test_the_same_story_from_two_providers_is_one_row(conn):
    """§3.1's exact-dedup key does its job across providers, not just within one."""
    marketaux = document(provider="marketaux")
    rss = document(
        provider="google-news-rss",
        # same story, different casing and a tracking parameter
        title="NVIDIA Reports Record Data-Centre Revenue",
        url="https://example.test/a?utm_source=googlenews",
        excerpt="a different excerpt entirely",
    )
    inserted, deduped = store_documents(conn, [marketaux, rss], run_id=RUN_ID)
    assert (inserted, deduped) == (1, 1)
    assert conn.execute("SELECT COUNT(*) AS n FROM source_documents").fetchone()["n"] == 1


def test_first_seen_at_is_never_updated(conn):
    store_documents(conn, [document()], run_id=RUN_ID, now="2026-08-10T00:00:00Z")
    before = conn.execute("SELECT * FROM source_documents").fetchone()

    inserted, deduped = store_documents(
        conn, [document()], run_id="run_bbbbbbbbbbbbbbbb", now="2026-08-16T09:00:00Z"
    )
    after = conn.execute("SELECT * FROM source_documents").fetchone()

    assert (inserted, deduped) == (0, 1)
    assert after["first_seen_at"] == before["first_seen_at"] == "2026-08-10T00:00:00Z"
    # the whole row is untouched, not just the one column
    assert tuple(after) == tuple(before)


# =================================================================================================
# D-29 caps (contract §3.2, §9.29)
# =================================================================================================


def test_official_keeps_its_body_and_the_official_excerpt_cap(conn):
    long_text = "x" * (EXCERPT_CAP_OFFICIAL * 2)
    store_documents(conn, [document(
        provider="sec-edgar", source_tier="official", excerpt=long_text, body=long_text,
    )], run_id=RUN_ID)
    row = conn.execute("SELECT * FROM source_documents").fetchone()
    assert len(row["excerpt"]) == EXCERPT_CAP_OFFICIAL
    assert row["body"] == long_text  # the body is not capped, only the excerpt is


def test_professional_has_a_null_body_and_the_professional_cap(conn):
    long_text = "y" * (EXCERPT_CAP_OFFICIAL * 2)
    store_documents(conn, [document(excerpt=long_text, body="a body we may not keep")],
                    run_id=RUN_ID)
    row = conn.execute("SELECT * FROM source_documents").fetchone()
    assert row["body"] is None
    assert len(row["excerpt"]) == EXCERPT_CAP_PROFESSIONAL


def test_body_is_null_for_every_non_official_row(conn):
    store_documents(conn, [
        document(provider="sec-edgar", source_tier="official", body="filing text"),
        document(url="https://example.test/b", body="news text"),
        document(url="https://example.test/c", source_tier="discussion", body="forum text"),
    ], run_id=RUN_ID)
    rows = conn.execute(
        "SELECT source_tier, body FROM source_documents WHERE source_tier != 'official'"
    ).fetchall()
    assert rows and all(r["body"] is None for r in rows)
    assert storable_body("anything", "professional") is None
    assert storable_body("anything", "discussion") is None


def test_truncation_mirrors_journal_evidence_go_exactly():
    """`journal/evidence.go:329 truncateRunes`: trim, hard code-point cut, NO marker."""
    text = "  " + "z" * (EXCERPT_CAP_PROFESSIONAL + 10) + "  "
    capped, truncated = cap_excerpt(text, "professional")
    assert truncated is True
    assert len(capped) == EXCERPT_CAP_PROFESSIONAL
    assert capped.endswith("z")  # no ellipsis, no "…", no marker of any kind
    short, truncated = cap_excerpt("  short  ", "professional")
    assert (short, truncated) == ("short", False)


def test_the_d29_literals_live_only_in_config_py():
    """§9.29's gate: this lane's modules must not restate a cap or a retention window.

    `config.py` is the SOLE HOME of the five constants and is on this lane's may-not-touch list.
    Two homes would make a counsel-driven narrowing a two-line change, which is exactly what D-29
    exists to prevent — so the gate is that the literals appear only there.

    Every module this lane owns is scanned, not a hand-picked four: a constant restated in a file
    nobody thought to list is the case the gate is for. `events.py`, `cluster.py` and `macro.py`
    belong to Wave 2 Lane 2A and are excluded — `events.KEY_FACT_CHAR_CAP` is a legitimate 300 that
    has nothing to do with a D-29 excerpt cap, and widening the gate over another lane's file would
    make this test fail for a reason it cannot describe.

    Lines mentioning an HTTP status are skipped — `400` is also a status code, and a bad-request
    response is not a retention window.
    """
    literals = re.compile(r"(?<![\d.])(4000|1200|300|400|90)(?![\d.])")
    offenders = []
    for name in ("documents.py", "providers.py", "ingest.py", "budget.py", "entities.py",
                 "db.py", "main.py"):
        path = SERVICE_ROOT / "app" / name
        for number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
            if re.search(r"status|http", line, re.IGNORECASE):
                continue
            if literals.search(line):
                offenders.append(f"{name}:{number}: {line.strip()}")
    assert offenders == []

    # …and they really are in config.py, so the test above is not vacuous.
    config = (SERVICE_ROOT / "app" / "config.py").read_text(encoding="utf-8")
    for value in (EXCERPT_CAP_OFFICIAL, EXCERPT_CAP_PROFESSIONAL,
                  RETENTION_PROFESSIONAL_DAYS, RETENTION_DISCUSSION_DAYS):
        assert str(value) in config


# =================================================================================================
# Retention sweep (contract §3.2, §9.30)
# =================================================================================================


def test_sweep_deletes_expired_professional_and_keeps_official_of_the_same_age(conn):
    now = datetime(2026, 8, 17, tzinfo=timezone.utc)
    old = (now - timedelta(days=RETENTION_PROFESSIONAL_DAYS + 5)).strftime("%Y-%m-%dT%H:%M:%SZ")

    store_documents(conn, [
        document(url="https://example.test/pro", source_tier="professional"),
        document(url="https://example.test/sec", provider="sec-edgar", source_tier="official"),
        document(url="https://example.test/disc", source_tier="discussion"),
    ], run_id=RUN_ID, now=old)
    store_documents(conn, [document(url="https://example.test/fresh")], run_id=RUN_ID,
                    now=now.strftime("%Y-%m-%dT%H:%M:%SZ"))

    deleted = run_retention_sweep(conn, now=now)
    assert deleted["professional"] == 1
    assert deleted["discussion"] == 1
    assert "official" not in deleted  # official retention is indefinite, by policy

    kept = {r["url"] for r in conn.execute("SELECT url FROM source_documents").fetchall()}
    assert kept == {"https://example.test/sec", "https://example.test/fresh"}


def test_sweep_touches_only_the_document_table(conn):
    """§3.2: the sweep deletes the document ROW; derived structure is ours and survives."""
    now = datetime(2026, 8, 17, tzinfo=timezone.utc)
    old = (now - timedelta(days=RETENTION_PROFESSIONAL_DAYS + 5)).strftime("%Y-%m-%dT%H:%M:%SZ")
    store_documents(conn, [document()], run_id=RUN_ID, now=old)
    doc_id = conn.execute("SELECT id FROM source_documents").fetchone()["id"]

    conn.execute(
        "INSERT INTO events (id, event_type, title, occurred_at, published_at, first_seen_at, "
        "source_tier, cluster_key) VALUES (?,?,?,?,?,?,?,?)",
        ("evt_1111111111111111", "other", "t", old, old, old, "professional", "k"),
    )
    conn.execute(
        "INSERT INTO event_documents (event_id, document_id, url, provider, source_tier, "
        "published_at) VALUES (?,?,?,?,?,?)",
        ("evt_1111111111111111", doc_id, "https://example.test/a", "marketaux", "professional", old),
    )
    conn.commit()

    run_retention_sweep(conn, now=now)
    assert conn.execute("SELECT COUNT(*) AS n FROM source_documents").fetchone()["n"] == 0
    assert conn.execute("SELECT COUNT(*) AS n FROM event_documents").fetchone()["n"] == 1
    assert conn.execute("SELECT COUNT(*) AS n FROM events").fetchone()["n"] == 1


def test_the_sweep_runs_at_startup(tmp_path, monkeypatch):
    """§9.30: written and never executed is the failure mode this clause exists to prevent."""
    c = connect()
    migrate(c)
    ancient = (datetime.now(timezone.utc)
               - timedelta(days=RETENTION_PROFESSIONAL_DAYS + 30)).strftime("%Y-%m-%dT%H:%M:%SZ")
    store_documents(c, [document()], run_id=RUN_ID, now=ancient)
    assert c.execute("SELECT COUNT(*) AS n FROM source_documents").fetchone()["n"] == 1
    c.close()

    app = FastAPI()
    app.include_router(documents_router)
    with TestClient(app):
        pass

    c = connect()
    assert c.execute("SELECT COUNT(*) AS n FROM source_documents").fetchone()["n"] == 0
    c.close()


# =================================================================================================
# GET /documents (contract §3.11, §1, §9.10)
# =================================================================================================


def test_empty_store_answers_with_the_contract_shape(client):
    body = client.get("/documents").json()
    assert body["documents"] == []
    assert body["coverage"] == "insufficient"
    assert body["degraded"] == []
    assert body["asOf"].endswith("Z")


def test_as_of_excludes_a_backfilled_document(client, conn):
    """The whole point of storing both timestamps (§1.2)."""
    store_documents(conn, [document(url="https://example.test/visible",
                                    published_at="2026-08-10T00:00:00Z")],
                    run_id=RUN_ID, now="2026-08-10T00:00:00Z")
    store_documents(conn, [document(url="https://example.test/backfilled",
                                    published_at="2026-07-01T00:00:00Z")],
                    run_id=RUN_ID, now="2026-08-16T09:00:00Z")

    body = client.get("/documents", params={"as_of": "2026-08-15T00:00:00Z"}).json()
    urls = [d["url"] for d in body["documents"]]
    assert urls == ["https://example.test/visible"]
    assert body["asOf"] == "2026-08-15T00:00:00Z"  # the RESOLVED cutoff is echoed
    assert body["coverage"] == "ok"


def test_as_of_excludes_a_document_published_after_the_cutoff(client, conn):
    store_documents(conn, [
        document(url="https://example.test/old", published_at="2026-08-10T00:00:00Z"),
        document(url="https://example.test/new", published_at="2026-08-20T00:00:00Z"),
    ], run_id=RUN_ID, now="2026-08-01T00:00:00Z")
    body = client.get("/documents", params={"as_of": "2026-08-15T00:00:00Z"}).json()
    assert [d["url"] for d in body["documents"]] == ["https://example.test/old"]


def test_ticker_and_provider_filters(client, conn):
    store_documents(conn, [
        document(url="https://example.test/nvda", raw_tickers=["NVDA"]),
        document(url="https://example.test/amd", raw_tickers=["AMD"], provider="google-news-rss"),
        document(url="https://example.test/both", raw_tickers=["AMD", "NVDA"]),
    ], run_id=RUN_ID, now="2026-08-10T00:00:00Z")

    got = client.get("/documents", params={"ticker": "nvda"}).json()["documents"]
    assert {d["url"] for d in got} == {"https://example.test/nvda", "https://example.test/both"}

    got = client.get("/documents", params={"provider": "google-news-rss"}).json()["documents"]
    assert [d["url"] for d in got] == ["https://example.test/amd"]


def test_since_rejects_a_non_timestamp(client):
    assert client.get("/documents", params={"since": "yesterday"}).status_code == 400
    assert client.get("/documents", params={"as_of": "yesterday"}).status_code == 400


def test_raw_tickers_round_trip_exactly_as_the_provider_reported_them(conn, client):
    store_documents(conn, [document(raw_tickers=["nvda", "AMD", "BRK.B"])],
                    run_id=RUN_ID, now="2026-08-10T00:00:00Z")
    got = client.get("/documents").json()["documents"][0]
    # No normalisation, no inference: mapping to companies is entities.resolve_tickers' job.
    assert got["rawTickers"] == ["nvda", "AMD", "BRK.B"]
    stored = conn.execute("SELECT raw_tickers FROM source_documents").fetchone()["raw_tickers"]
    assert json.loads(stored) == ["nvda", "AMD", "BRK.B"]
