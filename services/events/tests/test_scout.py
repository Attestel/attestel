"""Discovery Scout: deterministic ranking, durable snapshots, and store-only reads."""
from __future__ import annotations

import inspect
from datetime import datetime, timedelta, timezone

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from app import scout
from app.db import connect, migrate

T0 = datetime(2026, 8, 26, 20, 0, 0, tzinfo=timezone.utc)


@pytest.fixture
def conn():
    connection = connect()
    migrate(connection)
    try:
        yield connection
    finally:
        connection.close()


def event_row(ticker="AMD", *, event_id="evt_1", published=None, importance=0.9):
    return {
        "id": event_id,
        "event_type": "product_launch",
        "title": f"{ticker} announces a new accelerator platform",
        "published_at": scout._iso(published or (T0 - timedelta(hours=2))),
        "source_tier": "official",
        "importance": importance,
        "novelty": 0.9,
        "document_count": 2,
        "ticker": ticker,
        "relevance": 1.0,
        "is_primary": 1,
        "providers": '["sec-edgar","company-ir"]',
        "related": f'["{ticker}","NVDA"]',
    }


def unavailable_technical(_ticker, *, as_of):
    assert as_of == T0
    return {"available": False, "reason": "insufficient"}


def insert_event(conn, *, event_id="evt_db", ticker="AMD", published=None):
    stamp = scout._iso(published or (T0 - timedelta(hours=1)))
    conn.execute(
        "INSERT INTO events (id,event_type,title,summary,occurred_at,published_at,first_seen_at,"
        "source_tier,official_confirmed,importance,novelty,document_count,cluster_key) "
        "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
        (event_id, "product_launch", f"{ticker} launches a platform", "", stamp, stamp, stamp,
         "official", 1, 0.9, 0.85, 2, event_id),
    )
    conn.execute(
        "INSERT INTO event_tickers (event_id,ticker,relevance,is_primary) VALUES (?,?,?,1)",
        (event_id, ticker, 1.0),
    )
    conn.commit()


def test_default_universe_is_the_existing_thirty_name_evaluation_set():
    from app.entities import TICKER_ALIASES

    universe = scout.scout_universe()
    assert len(universe) == 30
    assert universe[:3] == ["NVDA", "GOOGL", "TSLA"]
    assert len(set(universe)) == len(universe)
    assert set(universe) <= set(TICKER_ALIASES), "RSS discovery needs an auditable alias per ticker"


def test_intake_rotates_the_whole_universe_in_six_bounded_slots(monkeypatch):
    monkeypatch.delenv("SCOUT_INTAKE_BATCH_SIZE", raising=False)
    monkeypatch.delenv("SCOUT_INTAKE_SLOT_SECONDS", raising=False)
    universe = list(scout.DEFAULT_UNIVERSE)
    epoch = datetime(2026, 8, 27, 0, 0, 0, tzinfo=timezone.utc)
    # Anchor on a slot boundary so each subsequent four-hour step advances exactly one batch.
    epoch = datetime.fromtimestamp(
        (int(epoch.timestamp()) // scout.DEFAULT_INTAKE_SLOT_SECONDS)
        * scout.DEFAULT_INTAKE_SLOT_SECONDS,
        tz=timezone.utc,
    )
    batches = [scout.intake_batch(
        now=epoch + timedelta(hours=4 * offset), universe_=universe,
    ) for offset in range(6)]
    assert all(len(batch) == 5 for batch in batches)
    assert set(ticker for batch in batches for ticker in batch) == set(universe)


def test_default_intake_cannot_spend_marketaux_or_alpha_vantage(monkeypatch):
    monkeypatch.setenv(
        "SCOUT_INTAKE_PROVIDERS",
        "marketaux,alphavantage,google-news-rss,not-a-provider",
    )
    assert scout.intake_providers() == ["google-news-rss"]
    assert "marketaux" not in scout.SCOUT_INTAKE_ALLOWED
    assert "alphavantage" not in scout.SCOUT_INTAKE_ALLOWED


def test_company_ranking_is_deterministic_and_explains_every_candidate():
    rows = [
        event_row("AMD", event_id="evt_a", importance=0.92),
        event_row("ORCL", event_id="evt_b", importance=0.55),
    ]
    first = scout.build_candidates(rows, [], {}, now=T0)
    second = scout.build_candidates(rows, [], {}, now=T0)
    assert first == second
    assert [item["ticker"] for item in first] == ["AMD", "ORCL"]
    for item in first:
        assert item["whyNow"]
        assert item["evidence"]
        assert item["components"].keys() == {
            "eventAttention", "catalystProximity", "technicalSalience",
            "evidenceBreadth", "sourceQuality",
        }
        assert "buy" not in item["whyNow"].lower()
        assert "sell" not in item["whyNow"].lower()


def test_unusual_real_technical_state_can_surface_without_an_event():
    technical = {
        "AMD": {
            "available": True,
            "score": 0.9,
            "at": scout._iso(T0 - timedelta(days=1)),
            "source": "yfinance",
            "facts": {"rsi": 20.0, "changePct": -6.0},
            "summary": "Current completed daily bars show RSI 20.0 and an absolute daily move of 6.0%.",
        }
    }
    candidates = scout.build_candidates([], [], technical, now=T0)
    assert len(candidates) == 1
    assert candidates[0]["ticker"] == "AMD"
    assert candidates[0]["components"]["technicalSalience"] == 0.9
    assert candidates[0]["attentionBand"] == "emerging"


def test_technical_read_supplies_a_past_cutoff_and_rejects_synthetic(monkeypatch):
    seen = {}

    class Response:
        status_code = 200
        ok = True

        @staticmethod
        def json():
            return {
                "source": "yfinance",
                "sourceIsSynthetic": False,
                "price": {"asOf": "2026-08-26T00:00:00Z", "lastClose": 100, "changePct": -6},
                "indicators": {"latest": {"rsi": 22, "ema20": 108, "adx": 35}},
            }

    def fake_get(url, *, params, timeout):
        seen.update({"url": url, "params": params, "timeout": timeout})
        return Response()

    monkeypatch.setattr(scout.requests, "get", fake_get)
    result = scout.fetch_technical("AMD", as_of=T0)
    assert result["available"] is True
    assert result["score"] >= scout.TECHNICAL_ELIGIBILITY_MIN
    assert seen["params"]["as_of"] == "2026-08-26T19:59:59Z"
    assert seen["params"]["n"] == 260

    Response.json = staticmethod(lambda: {"source": "synthetic", "sourceIsSynthetic": True})
    assert scout.fetch_technical("AMD", as_of=T0) == {
        "available": False, "reason": "synthetic",
    }


def test_run_persists_an_immutable_snapshot_and_latest_read(conn):
    insert_event(conn)
    report = scout.run_scout(
        conn, now=T0, universe_=["AMD"], technical_fetcher=unavailable_technical,
    )
    assert report["candidateCount"] == 1
    assert report["coverage"]["predictionSignal"] == "excluded"
    assert report["coverage"]["modelRanking"] == "excluded"

    body = scout.latest_scout(conn, now=T0)
    assert body["runId"] == report["runId"]
    assert body["scoreVersion"] == scout.SCORE_VERSION
    assert [item["ticker"] for item in body["candidates"]] == ["AMD"]
    assert body["candidates"][0]["evidence"][0]["id"] == "evt_db"


def test_latest_read_withholds_pre_relevance_fix_snapshots(conn):
    conn.execute(
        "INSERT INTO scout_runs (id,score_version,universe_version,universe,as_of,started_at,"
        "completed_at,status,coverage,degraded) VALUES (?,?,?,?,?,?,?,?,?,?)",
        (
            "sct_old_relevance", "scout@1", scout.UNIVERSE_VERSION, '["NVDA"]',
            scout._iso(T0), scout._iso(T0), scout._iso(T0), "success",
            '{"state":"ok"}', "[]",
        ),
    )
    conn.commit()

    body = scout.latest_scout(conn, now=T0)

    assert scout.SCORE_VERSION == "scout@2"
    assert body["runId"] is None
    assert body["coverage"] == {"state": "insufficient", "reason": "no-runs"}


def test_stale_events_do_not_become_candidates(conn):
    insert_event(conn, published=T0 - timedelta(days=scout.EVENT_WINDOW_DAYS + 1))
    report = scout.run_scout(
        conn, now=T0, universe_=["AMD"], technical_fetcher=unavailable_technical,
    )
    assert report["candidateCount"] == 0
    assert scout.latest_scout(conn, now=T0)["candidates"] == []


def test_scheduled_catalyst_can_surface_with_no_model_enrichment(conn):
    scheduled = scout._iso(T0 + timedelta(days=5))
    conn.execute(
        "INSERT INTO scheduled_events (id,occurrence_key,kind,ticker,scheduled_at,confirmed,source,"
        "first_seen_at,title,importance,source_tier) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
        ("sched_1", "earnings|AMD|2026Q3", "earnings", "AMD", scheduled, 1,
         "company-ir", scout._iso(T0 - timedelta(days=1)), "AMD quarterly results", "high",
         "official"),
    )
    conn.commit()
    report = scout.run_scout(
        conn, now=T0, universe_=["AMD"], technical_fetcher=unavailable_technical,
    )
    assert report["candidateCount"] == 1
    candidate = scout.latest_scout(conn, now=T0)["candidates"][0]
    assert candidate["components"]["catalystProximity"] == 1.0
    assert "scheduled in 5 days" in candidate["whyNow"]


def test_scout_get_is_store_only_even_when_every_provider_is_configured(monkeypatch, conn):
    insert_event(conn)
    scout.run_scout(conn, now=T0, universe_=["AMD"], technical_fetcher=unavailable_technical)
    monkeypatch.setattr(scout, "_now", lambda: T0)

    def explode(*_args, **_kwargs):
        raise AssertionError("GET /scout reached a network client")

    monkeypatch.setattr(scout.requests, "get", explode)
    app = FastAPI()
    app.include_router(scout.router)
    response = TestClient(app).get("/scout")
    assert response.status_code == 200
    assert response.json()["candidates"][0]["ticker"] == "AMD"


def test_an_old_snapshot_is_explicitly_withheld(conn):
    insert_event(conn)
    report = scout.run_scout(
        conn, now=T0, universe_=["AMD"], technical_fetcher=unavailable_technical,
    )
    body = scout.latest_scout(
        conn, now=T0 + timedelta(seconds=scout.DEFAULT_MAX_RUN_AGE_SECONDS + 1),
    )
    assert body["runId"] == report["runId"]
    assert body["coverage"]["state"] == "stale"
    assert body["candidates"] == []
    assert "scout:stale" in body["degraded"]


def test_scout_source_has_no_model_or_prediction_seam():
    source = inspect.getsource(scout)
    assert "LLM_URL" not in source
    assert "PREDICTION_URL" not in source
    assert "/predict" not in source
    assert "services.llm" not in source
