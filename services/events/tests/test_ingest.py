"""The scheduler, the run record, the leakage guard, and the end-to-end handshake with Wave 2A.

The two tests that matter most are at the ends of this file:

* `test_a_historical_read_is_served_from_the_store_with_the_client_patched_to_raise` — contract
  §1.5's leakage test, and the reason a past `as_of` can be trusted at all.
* `test_end_to_end_ingest_then_cluster` — this lane's producer feeding Wave 2 Lane 2A's consumer
  for real, rather than 2A's fixtures standing in for it. 2A landed out of order and has never seen
  a row this lane wrote; that test is the reconciliation.
"""
from __future__ import annotations

import json
import os
import re
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from app import events as events_module
from app import ingest as ingest_module
from app import providers as providers_module
from app.db import connect, migrate
from app.documents import router as documents_router
from app.documents import store_documents
from app.ingest import router as ingest_router
from app.ingest import (
    run_ingest,
    scheduled_id,
    store_macro_releases,
    store_scheduled_events,
    tickers,
)

SERVICE_ROOT = Path(__file__).resolve().parent.parent
APP_DIR = SERVICE_ROOT / "app"

KEY_ENV = (
    "MARKETAUX_API_KEY", "ALPHAVANTAGE_API_KEY", "FRED_API_KEY", "FMP_API_KEY", "EVENTS_CONTACT_UA",
)


@pytest.fixture()
def conn(tmp_path, monkeypatch):
    for name in KEY_ENV:
        monkeypatch.delenv(name, raising=False)
    monkeypatch.delenv("TICKERS", raising=False)
    monkeypatch.delenv("JOURNAL_URL", raising=False)
    c = connect()
    migrate(c)
    yield c
    c.close()


@pytest.fixture()
def client(conn):
    app = FastAPI()
    app.include_router(documents_router)
    app.include_router(ingest_router)
    with TestClient(app) as c:
        yield c


_DOCSTRING = re.compile(r'("""|\'\'\')(?:.|\n)*?\1')


def code_of(name: str) -> str:
    """A module's source with docstrings and comments removed.

    Every scan below is looking for what the code DOES. Scanning the prose too makes a module that
    documents the rule it obeys fail the test that checks the rule — which is exactly backwards.
    """
    source = (APP_DIR / name).read_text(encoding="utf-8")
    source = _DOCSTRING.sub("", source)
    return "\n".join(
        line for line in source.splitlines() if not line.strip().startswith("#")
    )


class Exploding:
    """Any outbound call is an assertion failure, not an exception the code can swallow."""

    def get(self, *args, **kwargs):
        raise AssertionError("a provider fetch happened where none is permitted")


# =================================================================================================
# The empty-.env pass (CLAUDE.md invariant #1)
# =================================================================================================


def test_a_full_pass_with_no_keys_fetches_nothing_and_raises_nothing(conn, monkeypatch):
    monkeypatch.setattr(providers_module, "requests", Exploding())
    result = run_ingest(conn)

    assert (result["fetched"], result["inserted"], result["deduped"]) == (0, 0, 0)
    assert result["runId"].startswith("run_")
    assert re.fullmatch(r"run_[0-9a-f]{16}", result["runId"])
    # one degraded entry per provider, in the single "<provider>:<reason>" vocabulary
    assert result["degraded"] == [
        "marketaux:no-key", "sec-edgar:no-key", "google-news-rss:no-key",
        "alphavantage:no-key",
        # Phase 2C — a per-ticker provider, so it reports here, before the window providers. It
        # says `disabled`, not `no-key`: the gate is checked before the contact address, so an
        # operator is shown the door they have to open first.
        "company-ir:disabled",
        "federal-reserve:disabled",
        # Phase 2A/2B — both keyless, both gated, both silent against an empty environment.
        "bls:disabled", "bea:disabled",
        "fred:no-key", "fmp:no-key",
    ]


def test_the_run_record_is_written_and_readable(conn, monkeypatch, client):
    monkeypatch.setattr(providers_module, "requests", Exploding())
    result = run_ingest(conn)
    runs = client.get("/ingest/runs").json()["runs"]
    assert [r["id"] for r in runs] == [result["runId"]]
    row = runs[0]
    assert row["finishedAt"] is not None
    assert row["tickers"] == ["NVDA", "GOOGL", "TSLA"]
    assert len(row["providers"]) == 10
    assert row["degraded"] == result["degraded"]


def test_post_ingest_is_the_manual_trigger(conn, monkeypatch, client):
    monkeypatch.setattr(providers_module, "requests", Exploding())
    # §9.31: `make ingest-once` posts {} when both filter variables are empty
    body = client.post("/ingest", json={}).json()
    assert set(body) == {"runId", "fetched", "inserted", "deduped", "degraded"}

    narrowed = client.post("/ingest", json={"providers": ["fred"], "tickers": ["NVDA"]}).json()
    assert narrowed["degraded"] == ["fred:no-key"]
    runs = client.get("/ingest/runs").json()["runs"]
    assert runs[0]["providers"] == ["fred"] and runs[0]["tickers"] == ["NVDA"]


def test_an_unknown_provider_name_is_ignored_rather_than_fatal(conn, monkeypatch):
    monkeypatch.setattr(providers_module, "requests", Exploding())
    result = run_ingest(conn, providers=["not-a-provider"])
    assert result["degraded"] == []
    assert result["fetched"] == 0


def test_the_ticker_universe_comes_from_configuration(monkeypatch):
    monkeypatch.delenv("TICKERS", raising=False)
    assert tickers() == ["NVDA", "GOOGL", "TSLA"]
    monkeypatch.setenv("TICKERS", " nvda , amd ,nvda ")
    assert tickers() == ["NVDA", "AMD"]


class _JournalResponse:
    def __init__(self, payload, status=200):
        self.payload = payload
        self.status_code = status

    def raise_for_status(self):
        if self.status_code >= 400:
            raise ingest_module.requests.HTTPError(str(self.status_code))

    def json(self):
        return self.payload


def test_default_universe_merges_configured_and_followed_tickers(monkeypatch):
    monkeypatch.setenv("TICKERS", "NVDA,GOOGL")
    monkeypatch.setenv("JOURNAL_URL", "http://journal:8096/")
    called = {}

    def fake_get(url, *, timeout):
        called.update(url=url, timeout=timeout)
        return _JournalResponse({"tickers": ["amd", "NVDA", "BRK.B", "bad ticker", "AMD"]})

    monkeypatch.setattr(ingest_module.requests, "get", fake_get)
    universe, degraded = ingest_module.ingestion_universe()

    assert universe == ["NVDA", "GOOGL", "AMD", "BRK.B"]
    assert degraded == []
    assert called == {
        "url": "http://journal:8096/_internal/tickers",
        "timeout": ingest_module.JOURNAL_TIMEOUT_SECONDS,
    }


def test_default_universe_falls_back_and_reports_unreachable_journal(monkeypatch):
    monkeypatch.setenv("TICKERS", "NVDA,GOOGL")
    monkeypatch.setenv("JOURNAL_URL", "http://journal:8096")

    def unavailable(*_args, **_kwargs):
        raise ingest_module.requests.ConnectionError("journal unavailable")

    monkeypatch.setattr(ingest_module.requests, "get", unavailable)
    universe, degraded = ingest_module.ingestion_universe()

    assert universe == ["NVDA", "GOOGL"]
    assert degraded == ["subscriptions:unreachable"]


def test_explicit_ingest_universe_never_calls_journal(monkeypatch):
    monkeypatch.setenv("JOURNAL_URL", "http://journal:8096")

    def must_not_call(*_args, **_kwargs):
        raise AssertionError("an explicitly narrowed pass consulted Journal")

    monkeypatch.setattr(ingest_module.requests, "get", must_not_call)
    universe, degraded = ingest_module.ingestion_universe([" tsla ", "TSLA", "brk.b"])

    assert universe == ["TSLA", "BRK.B"]
    assert degraded == []


def test_followed_tickers_are_persisted_on_the_ingest_run(conn, monkeypatch):
    monkeypatch.setenv("TICKERS", "NVDA")
    monkeypatch.setenv("JOURNAL_URL", "http://journal:8096")
    monkeypatch.setattr(
        ingest_module.requests,
        "get",
        lambda *_args, **_kwargs: _JournalResponse({"tickers": ["AMD", "NVDA"]}),
    )

    run_ingest(conn, providers=["not-a-provider"])
    row = conn.execute("SELECT tickers, degraded FROM ingest_runs").fetchone()

    assert json.loads(row["tickers"]) == ["NVDA", "AMD"]
    assert json.loads(row["degraded"]) == []


def test_the_scheduled_pass_is_off_by_default(conn, monkeypatch):
    """§9.31: `INGEST_ENABLED` defaults false, so `docker compose up` performs no provider call."""
    monkeypatch.setattr(providers_module, "requests", Exploding())
    monkeypatch.setattr(ingest_module, "INGEST_ENABLED", False)
    assert ingest_module.run_scheduled_pass() is None


def test_there_is_no_loop_and_no_sleep_in_the_scheduler():
    """§9.22's posture, applied to this lane: one pass, then return. Repetition is the operator's
    cron, outside the codebase."""
    code = code_of("ingest.py")
    assert "while True" not in code
    assert "time.sleep" not in code
    assert not re.search(r"\bimport threading\b", code)


# =================================================================================================
# scheduled_events (§9.27)
# =================================================================================================


def test_scheduled_events_are_idempotent_and_keep_their_first_seen_at(conn):
    row = {"kind": "earnings", "ticker": "NVDA", "series": None,
           "scheduled_at": "2026-11-19T00:00:00Z", "confirmed": 0, "source": "alphavantage"}
    assert store_scheduled_events(conn, [row], now="2026-08-10T00:00:00Z") == 1
    assert store_scheduled_events(conn, [row], now="2026-08-16T00:00:00Z") == 0

    stored = conn.execute("SELECT * FROM scheduled_events").fetchall()
    assert len(stored) == 1
    assert stored[0]["id"] == scheduled_id(row)
    assert re.fullmatch(r"sch_[0-9a-f]{16}", stored[0]["id"])
    # first_seen_at is what macro.get_calendar filters on; re-stamping it would make the entry
    # invisible to every historical read
    assert stored[0]["first_seen_at"] == "2026-08-10T00:00:00Z"


def test_scheduled_events_converge_across_providers_and_prefer_official_metadata(conn):
    base = {
        "kind": "macro_release", "ticker": None, "series": "CPI",
        "scheduled_at": "2026-11-12T13:30:00Z", "confirmed": 1,
    }
    professional = {
        **base, "source": "fmp", "source_tier": "professional", "title": "CPI YoY",
        "description": "professional description", "importance": "high", "status": "confirmed",
        "source_url": "https://professional.test/cpi", "timezone": "UTC", "local_time": "",
        "previous": 2.8, "expected": 2.9, "actual": None, "unit": "%",
    }
    official = {
        **base, "source": "fred", "source_tier": "official",
        "title": "Consumer Price Index (CPI)", "description": "official description",
        "importance": "high", "status": "confirmed", "source_url": "https://official.test/cpi",
        "timezone": "America/New_York", "local_time": "08:30",
    }

    assert scheduled_id(professional) == scheduled_id(official), "provider leaked into identity"
    assert store_scheduled_events(conn, [professional], now="2026-08-01T00:00:00Z") == 1
    assert store_scheduled_events(conn, [official], now="2026-08-02T00:00:00Z") == 0

    row = conn.execute("SELECT * FROM scheduled_events").fetchone()
    assert row["source"] == "fred" and row["source_tier"] == "official"
    assert row["title"] == "Consumer Price Index (CPI)"
    assert row["expected"] == 2.9 and row["previous"] == 2.8
    assert row["first_seen_at"] == "2026-08-01T00:00:00Z"
    assert row["updated_at"] == "2026-08-02T00:00:00Z"
    assert [h["change_type"] for h in conn.execute(
        "SELECT * FROM scheduled_event_history ORDER BY observed_at"
    )] == ["created", "source_upgraded"]


def test_a_reschedule_keeps_one_event_and_appends_point_in_time_history(conn):
    first = {
        "kind": "earnings", "ticker": "NVDA", "series": None,
        "occurrence_key": "earnings|NVDA|2026-10-31",
        "scheduled_at": "2026-11-18T00:00:00Z", "confirmed": 0,
        "source": "alphavantage", "status": "tentative", "title": "NVIDIA earnings",
    }
    moved = {**first, "scheduled_at": "2026-11-19T00:00:00Z"}
    assert scheduled_id(first) == scheduled_id(moved), "date leaked into stable occurrence identity"
    assert store_scheduled_events(conn, [first], now="2026-08-01T00:00:00Z") == 1
    assert store_scheduled_events(conn, [moved], now="2026-08-10T00:00:00Z") == 0

    rows = conn.execute("SELECT * FROM scheduled_events").fetchall()
    assert len(rows) == 1 and rows[0]["scheduled_at"] == "2026-11-19T00:00:00Z"
    history = conn.execute(
        "SELECT * FROM scheduled_event_history ORDER BY observed_at"
    ).fetchall()
    assert [h["change_type"] for h in history] == ["created", "rescheduled"]
    assert history[1]["prior_scheduled_at"] == "2026-11-18T00:00:00Z"
    assert history[1]["scheduled_at"] == "2026-11-19T00:00:00Z"
    assert rows[0]["first_seen_at"] == "2026-08-01T00:00:00Z"

    from app.macro import router as macro_router

    app = FastAPI()
    app.include_router(macro_router)
    with TestClient(app) as client:
        old = client.get("/calendar", params={
            "from": "2026-11-18T00:00:00Z", "to": "2026-11-18T23:59:59Z",
            "as_of": "2026-08-05T00:00:00Z",
        }).json()["scheduled"]
        current = client.get("/calendar", params={
            "from": "2026-11-19T00:00:00Z", "to": "2026-11-19T23:59:59Z",
            "as_of": "2026-08-15T00:00:00Z",
        }).json()["scheduled"]
    assert len(old) == 1 and old[0]["scheduledAt"] == "2026-11-18T00:00:00Z"
    assert len(current) == 1 and current[0]["scheduledAt"] == "2026-11-19T00:00:00Z"
    assert current[0]["revisions"][-1]["changeType"] == "rescheduled"


def test_cancellation_is_a_lifecycle_change_not_a_deleted_event(conn):
    row = {
        "kind": "earnings", "ticker": "NVDA", "series": None,
        "occurrence_key": "earnings|NVDA|2026-10-31",
        "scheduled_at": "2026-11-19T00:00:00Z", "confirmed": 1,
        "source": "company-ir", "source_tier": "official", "status": "confirmed",
        "title": "NVIDIA earnings",
    }
    assert store_scheduled_events(conn, [row], now="2026-08-01T00:00:00Z") == 1
    assert store_scheduled_events(
        conn, [{**row, "status": "cancelled"}], now="2026-08-12T00:00:00Z"
    ) == 0
    stored = conn.execute("SELECT * FROM scheduled_events").fetchone()
    assert stored["status"] == "cancelled"
    assert conn.execute(
        "SELECT change_type FROM scheduled_event_history ORDER BY observed_at DESC LIMIT 1"
    ).fetchone()["change_type"] == "cancelled"


def test_an_official_source_can_reinstate_a_cancelled_occurrence(conn):
    row = {
        "kind": "company_event", "ticker": "NVDA", "series": None,
        "occurrence_key": "company-event|NVDA|investor-day-2026",
        "scheduled_at": "2026-11-19T14:00:00Z", "confirmed": 1,
        "source": "company-ir", "source_tier": "official", "status": "confirmed",
        "title": "NVIDIA investor day",
    }
    store_scheduled_events(conn, [row], now="2026-08-01T00:00:00Z")
    store_scheduled_events(
        conn, [{**row, "status": "cancelled"}], now="2026-08-12T00:00:00Z"
    )
    store_scheduled_events(conn, [row], now="2026-08-20T00:00:00Z")

    assert conn.execute("SELECT status FROM scheduled_events").fetchone()["status"] == "confirmed"
    assert [history["change_type"] for history in conn.execute(
        "SELECT change_type FROM scheduled_event_history ORDER BY observed_at"
    )] == ["created", "cancelled", "status_changed"]


def test_the_calendar_endpoint_sees_what_this_lane_wrote(conn, monkeypatch):
    """§9.27's whole point: `GET /calendar` had no producer until this lane."""
    from app.macro import router as macro_router

    store_scheduled_events(conn, [{
        "kind": "earnings", "ticker": "NVDA", "series": None,
        "scheduled_at": "2026-11-19T00:00:00Z", "confirmed": 0, "source": "alphavantage",
    }], now="2026-08-10T00:00:00Z")

    app = FastAPI()
    app.include_router(macro_router)
    with TestClient(app) as client:
        body = client.get("/calendar", params={
            "from": "2026-11-01T00:00:00Z", "to": "2026-12-01T00:00:00Z",
        }).json()
    assert [item["ticker"] for item in body["scheduled"]] == ["NVDA"]

    # …and a cutoff before we captured it hides it, because it was not knowable then
    with TestClient(app) as client:
        body = client.get("/calendar", params={
            "from": "2026-11-01T00:00:00Z", "to": "2026-12-01T00:00:00Z",
            "as_of": "2026-08-01T00:00:00Z",
        }).json()
    assert body["scheduled"] == []


# =================================================================================================
# The leakage test (contract §1.3, §1.5) — part of this lane's definition of done
# =================================================================================================


def test_a_historical_read_is_served_from_the_store_with_the_client_patched_to_raise(
    conn, monkeypatch, client
):
    """A past `as_of` forbids a provider fetch. Not on a cache miss, not on a coverage gap.

    `providers.requests` is replaced by an object that raises on any call, so the read either
    succeeds from the store or the test fails loudly. It may never raise.
    """
    monkeypatch.setattr(providers_module, "requests", Exploding())
    store_documents(conn, [{
        "provider": "marketaux", "source_tier": "professional",
        "url": "https://news.test/historic", "title": "NVIDIA beats estimates",
        "excerpt": "e", "body": None, "published_at": "2026-08-10T00:00:00Z",
        "raw_tickers": ["NVDA"],
    }], run_id="run_cccccccccccccccc", now="2026-08-10T00:00:00Z")

    response = client.get("/documents", params={"as_of": "2026-08-12T00:00:00Z"})
    assert response.status_code == 200
    assert [d["url"] for d in response.json()["documents"]] == ["https://news.test/historic"]


def test_a_historical_read_with_no_coverage_still_never_fetches(conn, monkeypatch, client):
    """The coverage gap is the tempting case, and §1.3 forbids the fetch there too."""
    monkeypatch.setattr(providers_module, "requests", Exploding())
    response = client.get("/documents", params={"as_of": "2019-01-01T00:00:00Z"})
    assert response.status_code == 200
    body = response.json()
    assert body["documents"] == []
    assert body["coverage"] == "insufficient"


# =================================================================================================
# No LLM in this lane (locked decision 6)
# =================================================================================================


def test_no_module_in_this_service_reaches_the_llm():
    """A crude source scan on purpose — it is the assertion that matters, not its elegance."""
    offenders = []
    for path in sorted(APP_DIR.glob("*.py")):
        for number, line in enumerate(code_of(path.name).splitlines(), 1):
            if re.search(r"services[._]llm|from\s+\.\.llm|import\s+ollama", line):
                offenders.append(f"{path.name}:{number}: {line.strip()}")
            # no provider URL may point at the llm service
            if re.search(r"(localhost|127\.0\.0\.1|//llm)[:/]?8002|LLM_URL", line):
                offenders.append(f"{path.name}:{number}: {line.strip()}")
    assert offenders == []


def test_the_provider_urls_are_the_documented_hosts():
    hosts = set(re.findall(r"https://([a-z0-9.\-]+)/", code_of("providers.py")))
    assert hosts == {
        "api.marketaux.com", "data.sec.gov", "news.google.com", "www.alphavantage.co",
        "api.stlouisfed.org", "financialmodelingprep.com", "www.sec.gov",
        "fred.stlouisfed.org", "www.federalreserve.gov",
        # Phase 2A/2B — the two agencies' own schedule pages.
        "www.bls.gov", "www.bea.gov",
    }
    # Phase 2C moves ONE host out of this module and into configuration, so the assertion follows
    # it: `ir_registry.py` is the only other place in this service allowed to name an outbound host,
    # and every host it names must belong to the company itself (IR or its official newsroom).
    registry_hosts = set(re.findall(r"https://([a-z0-9.\-]+)/", code_of("ir_registry.py")))
    assert registry_hosts == {"investor.nvidia.com", "nvidianews.nvidia.com"}


# =================================================================================================
# End to end: this lane's producer into Wave 2 Lane 2A's consumer
# =================================================================================================


MARKETAUX_BODY = json.dumps({"data": [
    {"title": "Reuters: NVIDIA reports record revenue in its data-centre segment",
     "description": "NVIDIA said data-centre revenue set a record in the quarter.",
     "url": "https://news.test/story-a?utm_source=newsletter",
     "published_at": "2026-08-14T18:00:00Z",
     "entities": [{"symbol": "NVDA"}]},
]})

RSS_BODY = """<?xml version="1.0"?>
<rss version="2.0"><channel>
  <item><title>CNBC: NVIDIA reports record revenue in its data-centre segment</title>
        <link>https://news.test/story-b</link>
        <description>NVIDIA said data-centre revenue set a record in the quarter.</description>
        <pubDate>Fri, 14 Aug 2026 18:30:00 GMT</pubDate></item>
</channel></rss>"""

SEC_BODY = json.dumps({"filings": {"recent": {
    "form": ["8-K"],
    "accessionNumber": ["0001045810-26-000123"],
    "primaryDocument": ["nvda-8k.htm"],
    "items": ["2.02"],
    "filingDate": ["2026-08-14"],
    "acceptanceDateTime": ["2026-08-14T17:00:00.000Z"],
}}})


class ScriptedRequests:
    def __init__(self):
        self.calls = 0

    def get(self, url, params=None, headers=None, timeout=None):
        self.calls += 1
        symbol = (params or {}).get("symbols") or (params or {}).get("q") or ""

        class R:
            status_code = 200
            text = ""

        response = R()
        if "NVDA" not in str(symbol) and "marketaux" in url:
            response.text = json.dumps({"data": []})
        elif "marketaux" in url:
            response.text = MARKETAUX_BODY
        elif "news.google.com" in url:
            response.text = RSS_BODY if "NVDA" in str(symbol) else "<rss><channel/></rss>"
        elif "data.sec.gov" in url:
            response.text = SEC_BODY if "1045810" in url else json.dumps({"filings": {"recent": {}}})
        else:
            raise AssertionError(f"unscripted call: {url}")
        return response


def test_end_to_end_ingest_then_cluster(conn, monkeypatch):
    """Ingest through `documents.py`, then let Wave 2 Lane 2A cluster what this lane actually wrote.

    This is the reconciliation the Lane 1C amendment asks for: 2A built and passed 152 tests against
    fixture rows it inserted itself, and had never seen a row a real ingester produced. Everything
    below reads 2A's tables and 2A's code — `events`, `event_tickers`, `event_documents`,
    `event_state_history` — over documents this lane created.
    """
    monkeypatch.setenv("EVENTS_CONTACT_UA", "attestel ops@example.test")
    monkeypatch.setenv("MARKETAUX_API_KEY", "k")
    monkeypatch.setattr(providers_module, "requests", ScriptedRequests())

    result = run_ingest(
        conn,
        providers=["marketaux", "sec-edgar", "google-news-rss"],
        tickers_=["NVDA"],
    )
    assert result["inserted"] == 3
    assert result["degraded"] == []

    # 1. Every stored row is indistinguishable in shape from a 2A fixture row.
    rows = conn.execute("SELECT * FROM source_documents ORDER BY provider").fetchall()
    assert len(rows) == 3
    for row in rows:
        assert re.fullmatch(r"doc_[0-9a-f]{16}", row["id"])
        assert re.fullmatch(r"[0-9a-f]{64}", row["content_hash"])
        assert re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z", row["published_at"])
        assert re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z", row["first_seen_at"])
        assert row["source_tier"] in ("official", "professional", "discussion")
        assert isinstance(json.loads(row["raw_tickers"]), list)
        assert row["ingest_run_id"] == result["runId"]
        assert row["body"] is None or row["source_tier"] == "official"

    # 2. `run_ingest` already called 2A's assemble(). The two wire copies of one story collapsed
    #    into ONE event; the 8-K is a separate occurrence.
    events = conn.execute("SELECT * FROM events ORDER BY document_count DESC").fetchall()
    assert len(events) == 2
    clustered = events[0]
    assert clustered["document_count"] == 2
    # Both wire copies normalise to the same title once `cluster.normalise_title` strips the
    # known publisher prefix, which is why two rows from two providers became one event.
    assert clustered["event_type"] == "earnings_result"

    # 3. 2A's resolvers read the columns this lane wrote, and got the right answers.
    primary = conn.execute(
        "SELECT ticker, relevance FROM event_tickers WHERE event_id = ? AND is_primary = 1",
        (clustered["id"],),
    ).fetchone()
    assert (primary["ticker"], primary["relevance"]) == ("NVDA", 1.0)

    filing = conn.execute(
        "SELECT * FROM events WHERE source_tier = 'official'"
    ).fetchone()
    assert filing is not None and filing["official_confirmed"] == 1
    assert filing["event_type"] == "earnings_result"   # from the 8-K item label, not the prose
    assert filing["document_count"] == 1               # banding proposed, the threshold disposed

    # 4. Attribution is denormalised onto event_documents and survives the sweep (§3.7).
    links = conn.execute("SELECT * FROM event_documents").fetchall()
    assert len(links) == 3
    assert {r["provider"] for r in links} == {"marketaux", "google-news-rss", "sec-edgar"}

    # 5. event_state_history was written from this lane's first_seen_at, not the wall clock (§9.13).
    history = conn.execute(
        "SELECT DISTINCT as_of FROM event_state_history"
    ).fetchall()
    stored_first_seen = {r["first_seen_at"] for r in rows}
    assert {r["as_of"] for r in history} <= stored_first_seen

    # 6. A second identical pass inserts nothing and creates no new event.
    again = run_ingest(
        conn, providers=["marketaux", "sec-edgar", "google-news-rss"], tickers_=["NVDA"]
    )
    assert (again["inserted"], again["deduped"]) == (0, 3)
    assert conn.execute("SELECT COUNT(*) AS n FROM events").fetchone()["n"] == 2


def test_the_events_endpoint_serves_what_this_lane_ingested(conn, monkeypatch):
    monkeypatch.setenv("EVENTS_CONTACT_UA", "attestel ops@example.test")
    monkeypatch.setenv("MARKETAUX_API_KEY", "k")
    monkeypatch.setattr(providers_module, "requests", ScriptedRequests())
    run_ingest(conn, providers=["marketaux", "sec-edgar", "google-news-rss"], tickers_=["NVDA"])

    app = FastAPI()
    app.include_router(events_module.router)
    with TestClient(app) as client:
        body = client.get("/events", params={"tickers": "NVDA"}).json()
    assert body["coverage"] == "ok"
    assert len(body["events"]) == 2
    assert all(event["enrichmentState"] == "raw" for event in body["events"])
    assert all(event["sources"] for event in body["events"])

    # …and a cutoff before we ever saw them hides them, from the same store, with no fetch
    monkeypatch.setattr(providers_module, "requests", Exploding())
    with TestClient(app) as client:
        body = client.get("/events", params={"as_of": "2026-08-01T00:00:00Z"}).json()
    assert body["events"] == []
    assert body["coverage"] == "insufficient"


# =================================================================================================
# macro_events: the numbers land at ingest, with a NULL event_id (§3.9, §9.41)
# =================================================================================================


FMP_BODY = json.dumps([
    # A FORWARD release: the consensus is captured before it prints, so a surprise becomes
    # computable the moment the actual lands.
    {"country": "US", "event": "CPI", "date": "2026-09-10 12:30:00",
     "previous": 3.1, "estimate": 3.2, "actual": None, "unit": "percent"},
    # A PAST release, first seen after the fact. §3.9: a consensus we could only have captured at or
    # after the release is treated as absent, so this one carries no surprise however tempting the
    # arithmetic looks.
    {"country": "US", "event": "Nonfarm Payrolls", "date": "2026-08-12 12:30:00",
     "previous": 150000, "estimate": 175000, "actual": 210000, "unit": ""},
    # Not US — dropped before it reaches macro_events at all.
    {"country": "DE", "event": "IFO", "date": "2026-09-01 08:00:00",
     "previous": 1, "estimate": 2, "actual": 3, "unit": ""},
])

FRED_BODY = json.dumps({"release_dates": [
    {"release_id": 10, "date": "2026-09-10"},
]})


class MacroRequests:
    """FRED and FMP only. Any other host is an unscripted call and fails loudly."""

    def get(self, url, params=None, headers=None, timeout=None):
        class R:
            status_code = 200
            text = ""

        response = R()
        if "stlouisfed.org" in url:
            response.text = FRED_BODY
        elif "financialmodelingprep.com" in url:
            response.text = FMP_BODY
        else:
            raise AssertionError(f"unscripted call: {url}")
        return response


#: The pass clock every macro test runs at. Pinned, so the provider window and the captured
#: `expectation_captured_at` are the same in any month — a wall-clock default would silently drop
#: the fixture releases out of the lookback window a week after this was written.
MACRO_NOW = datetime(2026, 8, 17, 9, 0, 0, tzinfo=timezone.utc)


@pytest.fixture()
def macro_conn(conn, monkeypatch):
    monkeypatch.setenv("FRED_API_KEY", "k")
    monkeypatch.setenv("FMP_API_KEY", "k")
    monkeypatch.setattr(providers_module, "requests", MacroRequests())
    return conn


def macro_pass(conn, providers=("fred", "fmp")):
    return run_ingest(conn, providers=list(providers), tickers_=["NVDA"], now=MACRO_NOW)


def _macro_rows(conn):
    return {r["series"]: r for r in conn.execute("SELECT * FROM macro_events").fetchall()}


def test_ingest_writes_macro_events_and_assembly_links_them(macro_conn):
    """§9.41.1, §9.41.4 and §9.41.5. Before this, `macro_events` had no possible writer — `event_id`
    was NOT NULL and events only exist after 2A's `assemble()` has run on the very document this
    pass just stored. The numbers come from the provider response, never re-parsed from article text.

    Updated by Wave 2 Lane 2A. `run_ingest` ends with `assemble()`, so a full pass now leaves
    `event_id` populated rather than NULL. What 1C owns is unchanged and is what this still pins:
    the numbers. 2A supplies only the link, and it must be a link to a real event — "not invented"
    is now tested as a foreign key that resolves, which is a stronger claim than "is NULL".
    """
    macro_pass(macro_conn)

    rows = _macro_rows(macro_conn)
    assert set(rows) == {"CPI", "NFP"}, "a non-US release reached macro_events"

    for row in rows.values():
        assert row["event_id"] is not None, "2A must backfill event_id during assemble (§9.41.5)"
        assert macro_conn.execute(
            "SELECT COUNT(*) AS n FROM events WHERE id = ?", (row["event_id"],)
        ).fetchone()["n"] == 1, "event_id must resolve to a real assembled event, not an invented id"
        assert re.fullmatch(r"mac_[0-9a-f]{16}", row["id"])

    cpi = rows["CPI"]
    assert (cpi["previous"], cpi["expected"], cpi["actual"]) == (3.1, 3.2, None)
    assert cpi["unit"] == "percent"


def test_the_macro_key_links_the_document_to_its_release(macro_conn):
    """§9.41.3 — the one link 2A's backfill needs, and the only one."""
    macro_pass(macro_conn)

    keys = {
        r["macro_key"]
        for r in macro_conn.execute(
            "SELECT macro_key FROM source_documents WHERE macro_key IS NOT NULL").fetchall()
    }
    assert "CPI|2026-09-10T12:30:00Z" in keys

    # Every key resolves to exactly one row through the §9.41.2 natural key.
    for key in keys:
        series, release_at = key.split("|", 1)
        found = macro_conn.execute(
            "SELECT COUNT(*) AS n FROM macro_events WHERE series = ? AND release_at = ?",
            (series, release_at),
        ).fetchone()["n"]
        assert found == 1, f"macro_key {key} resolves to {found} rows"


def test_non_macro_documents_carry_no_macro_key(conn, monkeypatch):
    monkeypatch.setenv("EVENTS_CONTACT_UA", "attestel ops@example.test")
    monkeypatch.setenv("MARKETAUX_API_KEY", "k")
    monkeypatch.setattr(providers_module, "requests", ScriptedRequests())
    run_ingest(conn, providers=["marketaux", "sec-edgar", "google-news-rss"], tickers_=["NVDA"])

    keys = [r["macro_key"] for r in conn.execute("SELECT macro_key FROM source_documents")]
    assert keys and all(k is None for k in keys)


def test_a_consensus_captured_at_or_after_the_release_yields_no_surprise(macro_conn):
    """§3.9, restated by §9.41.6. The payrolls print is in the past and its 'consensus' is only
    reaching us now, so `actual − expected` would be a plausible-looking lie."""
    macro_pass(macro_conn, ["fmp"])

    nfp = _macro_rows(macro_conn)["NFP"]
    assert nfp["actual"] == 210000 and nfp["expected"] == 175000
    assert nfp["surprise"] is None, "a post-release consensus is treated as absent"


def test_a_pre_release_consensus_survives_later_passes_and_yields_a_surprise(macro_conn):
    """The merge rule in `store_macro_releases`, and the reason it exists.

    Pass 1 captures the CPI consensus before the print. Pass 2, after the print, brings the actual.
    If the second pass restamped `expectation_captured_at` to its own clock, the timing guard would
    fire and a legitimately computable surprise would silently become NULL.
    """
    macro_pass(macro_conn)
    first = _macro_rows(macro_conn)["CPI"]
    assert first["expectation_source"] == "fmp"
    assert first["expectation_captured_at"] < first["release_at"]
    assert first["surprise"] is None  # no actual yet

    # The release prints. A later pass sees the same row with the actual filled in.
    store_macro_releases(macro_conn, [{
        "series": "CPI", "release_at": "2026-09-10T12:30:00Z",
        "actual": 3.4, "expected": 3.2, "previous": 3.1,
        "unit": "percent", "expectation_source": "fmp",
    }], now="2026-09-10T13:00:00Z")

    after = _macro_rows(macro_conn)["CPI"]
    assert after["expectation_captured_at"] == first["expectation_captured_at"], \
        "a later pass overwrote the pre-release capture time"
    assert after["actual"] == 3.4
    assert round(after["surprise"], 6) == round(3.4 - 3.2, 6)


def test_re_ingesting_the_same_release_updates_one_row(macro_conn):
    """§9.41.2's UNIQUE(series, release_at). Two providers reporting the same release, and the same
    provider on a second pass, converge on one row rather than forking the corpus."""
    macro_pass(macro_conn)
    before = len(_macro_rows(macro_conn))
    macro_pass(macro_conn)
    assert len(_macro_rows(macro_conn)) == before


def test_fred_alone_never_blanks_out_numbers_fmp_already_reported(macro_conn):
    """FRED's `releases/dates` carries dates and nothing else. It announces the CPI release for the
    same date FMP priced, and must not overwrite the consensus with NULLs."""
    macro_pass(macro_conn, ["fmp"])
    priced = _macro_rows(macro_conn)["CPI"]
    assert priced["expected"] == 3.2

    store_macro_releases(macro_conn, [
        {"series": "CPI", "release_at": "2026-09-10T12:30:00Z"},  # a FRED-shaped row: no numbers
    ])
    after = _macro_rows(macro_conn)["CPI"]
    assert (after["expected"], after["previous"], after["unit"]) == (3.2, 3.1, "percent")


def test_an_unlinked_macro_row_is_invisible_to_every_read(macro_conn, monkeypatch):
    """The intended intermediate state of §9.41.5, stated as a test so it is not mistaken for a bug.

    `/macro` and `/calendar` both filter through the parent event's visibility, and a row with a
    NULL `event_id` has no parent — so the numbers are stored and are not yet served. They cannot
    be: `macro_events` has no `first_seen_at`, so with no parent event there is no honest answer to
    "was this knowable at the cutoff?", and inventing one would let a row appear at a cutoff before
    anyone could have seen it. It becomes visible when 2A backfills `event_id`.

    Updated by Wave 2 Lane 2A. The invariant is unchanged and still worth pinning — a row 2A has
    not reached stays invisible — but a full pass no longer produces one, so the unlinked state is
    now ARRANGED rather than assumed. This is §9.47's discipline applied to a table instead of a
    bar store: assert the empty case only against a store deliberately put in it. A release whose
    document never clustered is the real-world shape of this row.
    """
    from app.macro import router as macro_router

    macro_pass(macro_conn)
    assert _macro_rows(macro_conn), "nothing was stored, so this test proves nothing"
    # Undo 2A's backfill to recreate the pre-link state this test is about.
    macro_conn.execute("UPDATE macro_events SET event_id = NULL")
    macro_conn.commit()
    assert all(r["event_id"] is None for r in _macro_rows(macro_conn).values())

    app = FastAPI()
    app.include_router(macro_router)
    with TestClient(app) as c:
        assert c.get("/macro").json()["macro"] == []
        assert c.get("/calendar?from=2026-01-01&to=2027-01-01").json()["scheduled"], \
            "scheduled_events is a separate table and must still answer"
        kinds = c.get("/calendar?from=2026-01-01&to=2027-01-01").json()["scheduled"]
        assert all(item["source"] != "store" for item in kinds), \
            "an unlinked macro row leaked into the calendar"


# =================================================================================================
# The provider gate has no default (§9.42)
# =================================================================================================


#: Every table an ingestion pass can write a row into. "Zero rows" has to mean all of them: a gate
#: that stopped documents but still wrote a calendar entry would have leaked a provider call.
WRITABLE_TABLES = ("source_documents", "scheduled_events", "macro_events")


def _row_counts(conn) -> dict:
    return {
        table: conn.execute(f"SELECT COUNT(*) AS n FROM {table}").fetchone()["n"]
        for table in WRITABLE_TABLES
    }


def test_a_pass_with_the_gate_variable_genuinely_unset_writes_zero_rows(conn, monkeypatch):
    """§9.42's gate, asserted on ROWS rather than on counters.

    `EVENTS_CONTACT_UA` is deleted from the environment, not set to `""` — a gate that reads the
    variable with a default would still fill itself in here, and a counter-only assertion could
    pass while a provider quietly stored something. The transport is replaced with one that raises,
    so an attempted fetch is a failure rather than a silent success.

    `SEC_USER_AGENT` is deliberately left POPULATED, which is the whole point of §9.45: the gateway
    needs it, compose hands one environment to every service, and the events service must still
    write nothing. Before the rename this test could not have been written — the two were the same
    variable, so "the gateway is configured" and "the corpus may ingest" were the same statement.
    """
    for name in KEY_ENV:
        monkeypatch.delenv(name, raising=False)
    assert "EVENTS_CONTACT_UA" not in os.environ
    monkeypatch.setenv("SEC_USER_AGENT", "NVDA-Platform/0.1 (ops@example.test)")
    monkeypatch.setattr(providers_module, "requests", Exploding())

    result = run_ingest(conn)

    assert _row_counts(conn) == {t: 0 for t in WRITABLE_TABLES}
    assert (result["fetched"], result["inserted"], result["deduped"]) == (0, 0, 0)
    # The skip is LABELLED, exactly like a missing key — a silent skip is indistinguishable from a
    # provider that ran and found nothing.
    assert "sec-edgar:no-key" in result["degraded"]
    assert "google-news-rss:no-key" in result["degraded"]


def test_the_gate_variable_is_read_with_no_default(conn):
    """The failure mode §9.42 names in so many words: a gate written as
    `os.getenv("EVENTS_CONTACT_UA", "<anything>")` closes nothing, and a unit test that sets its own
    environment would still pass. So this reads the SOURCE.

    `gateway/config.go:81` really does default `SEC_USER_AGENT` to a populated contact string, and
    the root `.env.example` really does ship that value — this service must not copy either pattern,
    and since §9.45 it does not even share the variable.
    """
    source = code_of("providers.py")
    for offender in re.findall(r"os\.getenv\([^)]*\)", source):
        assert re.fullmatch(r'os\.getenv\(\s*name\s*,\s*""\s*\)', offender.strip()), (
            f"providers.py reads the environment with a default: {offender}. §9.42: absent or empty "
            "must mean the provider is skipped."
        )
    assert 'CONTACT_UA_ENV = "EVENTS_CONTACT_UA"' in source, \
        "§9.45: the events gate is EVENTS_CONTACT_UA, named once as a constant"
    assert re.search(r'def contact_user_agent\(\)[^\n]*\n\s*return _env\(CONTACT_UA_ENV\)', source), \
        "contact_user_agent() no longer reads the bare constant"
    # §9.45: the gateway's variable must not gate anything in this service any more.
    assert "SEC_USER_AGENT" not in source, \
        "services/events still reads the gateway's SEC_USER_AGENT"


def test_one_variable_gates_two_providers(conn, monkeypatch):
    """Recorded as a test, not only in the handoff, because it is surprising: `EVENTS_CONTACT_UA`
    switches on SEC EDGAR *and* Google News RSS together.

    That coupling is deliberate and, since §9.45, honest. Both providers write to the same
    persistent cross-user corpus, so both are gated on the same D-29 permission — "we are permitted
    to build this corpus from keyless sources" — rather than on a per-request contact address. It is
    still exactly the kind of thing a reader assumes is per-provider.
    """
    for name in KEY_ENV:
        monkeypatch.delenv(name, raising=False)
    monkeypatch.setattr(providers_module, "requests", ScriptedRequests())

    # Unset: BOTH providers skip and nothing is stored.
    blocked = run_ingest(conn, providers=["sec-edgar", "google-news-rss"], tickers_=["NVDA"])
    assert blocked["inserted"] == 0
    assert blocked["degraded"] == ["sec-edgar:no-key", "google-news-rss:no-key"]

    # Set: BOTH providers run. One variable, two sockets.
    monkeypatch.setenv("EVENTS_CONTACT_UA", "attestel ops@example.test")
    opened = run_ingest(conn, providers=["sec-edgar", "google-news-rss"], tickers_=["NVDA"])
    assert opened["inserted"] == 2
    assert opened["degraded"] == []


def test_the_events_block_of_env_example_ships_the_gate_unset(conn):
    """§9.42 + §9.45: `.env.example` may document the variable but must ship it commented out or
    empty. Both copies are checked — the service's own, and the ROOT one compose actually reads.

    The root file is the one that mattered and the one nobody was checking: `cp .env.example .env`
    is how this stack is configured, so a populated value there is a live invariant #1 violation
    however empty the per-service copy is.
    """
    for label, path in (
        ("services/events/.env.example", SERVICE_ROOT / ".env.example"),
        ("the root .env.example", SERVICE_ROOT.parent.parent / ".env.example"),
    ):
        assignments = [
            line.strip() for line in path.read_text(encoding="utf-8").splitlines()
            if line.strip().startswith("EVENTS_CONTACT_UA=")
        ]
        assert assignments in ([], ["EVENTS_CONTACT_UA="]), (
            f"{label} must ship the events gate empty or commented, got {assignments}")
