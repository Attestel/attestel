"""Early Opportunity Radar: fixed detector states, persistence, and store-only reads."""
from __future__ import annotations

import inspect
from datetime import datetime, timedelta, timezone

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from app import opportunities
from app.db import connect, migrate

T0 = datetime(2026, 8, 28, 12, 0, 0, tzinfo=timezone.utc)


@pytest.fixture
def conn():
    connection = connect()
    migrate(connection)
    try:
        yield connection
    finally:
        connection.close()


def feature_payload(
    ticker="NVDA",
    *,
    last_jump=0.0,
    current_volume=110.0,
    breakout=False,
    close_below_ema=False,
    bar_time="2026-08-27",
):
    rows = []
    for idx in range(65):
        close = 100.0 + idx * 0.20
        if idx == 64 and breakout:
            close += 1.8
        if idx == 64 and last_jump:
            close *= 1.0 + last_jump
        ema20 = close - 1.0
        if idx == 64 and close_below_ema:
            ema20 = close + 2.0
        rows.append({
            "time": f"2026-06-{idx + 1:02d}" if idx < 29 else bar_time,
            "open": close - 0.2,
            "high": close + 0.6,
            "low": close - 0.6,
            "close": close,
            "volume": current_volume if idx == 64 else 100.0,
            "ema20": ema20,
            "sma50": close - 2.0,
            "atr": 1.5,
        })
    return {
        "ticker": ticker,
        "timeframe": "1D",
        "source": "alpaca",
        "sourceIsSynthetic": False,
        "completedOnly": True,
        "dataThrough": bar_time,
        "rows": rows,
    }


def result_for(ticker, payload=None):
    body = payload or feature_payload(ticker)
    return {
        "available": True,
        "ticker": ticker,
        "barTime": body["dataThrough"],
        "source": body["source"],
        "payload": body,
    }


def test_price_setup_is_deterministic_and_score_is_not_a_probability():
    payload = feature_payload()
    first = opportunities.compute_price_setup(payload, benchmark_return_5d=0.0)
    second = opportunities.compute_price_setup(payload, benchmark_return_5d=0.0)
    assert first == second
    assert 0 <= first["priceScore"] <= 1
    assert set(first["components"]) == set(opportunities.PRICE_WEIGHTS)
    assert "probability" not in str(first).lower()


def test_developing_breakout_and_late_move_are_different_states():
    emerging = opportunities.classify_setup(
        opportunities.compute_price_setup(feature_payload(), benchmark_return_5d=0.0),
    )
    assert emerging is not None and emerging["state"] == "emerging"

    confirmed = opportunities.classify_setup(
        opportunities.compute_price_setup(
            feature_payload(breakout=True, current_volume=150.0), benchmark_return_5d=0.0,
        ),
    )
    assert confirmed is not None and confirmed["state"] == "confirmed"

    extended = opportunities.classify_setup(
        opportunities.compute_price_setup(
            feature_payload(last_jump=0.09, current_volume=180.0), benchmark_return_5d=0.0,
        ),
    )
    assert extended is not None and extended["state"] == "extended"
    assert "No-chase" in extended["reason"]
    assert "late-move-no-chase" in extended["riskFlags"]


def test_a_prior_setup_can_be_invalidated_but_an_unqualified_new_ticker_is_omitted():
    weak = opportunities.compute_price_setup(
        feature_payload(close_below_ema=True, current_volume=20.0), benchmark_return_5d=0.10,
    )
    assert opportunities.classify_setup(weak) is None
    invalidated = opportunities.classify_setup(
        weak,
        previous={"state": "emerging"},
    )
    assert invalidated is not None and invalidated["state"] == "invalidated"


def test_event_context_can_surface_only_a_partially_formed_price_setup():
    price = opportunities.compute_price_setup(feature_payload(current_volume=70.0),
                                              benchmark_return_5d=0.04)
    candidate = opportunities.classify_setup(
        price,
        scout_candidate={
            "components": {"eventAttention": 0.9, "catalystProximity": 0.0},
            "whyNow": "Fresh official product evidence.",
        },
    )
    assert price["priceScore"] >= opportunities.EVENT_LED_PRICE_MIN
    assert candidate is not None and candidate["state"] == "emerging"
    assert "Fresh official product evidence" in candidate["reason"]


def test_fetch_uses_completed_only_and_rejects_synthetic(monkeypatch):
    seen = {}

    class Response:
        status_code = 200
        ok = True

        @staticmethod
        def json():
            return feature_payload()

    def fake_get(url, *, params, timeout):
        seen.update({"url": url, "params": params, "timeout": timeout})
        return Response()

    monkeypatch.setattr(opportunities.requests, "get", fake_get)
    assert opportunities.fetch_completed_features("NVDA", as_of=T0)["available"] is True
    assert seen["params"]["completedOnly"] == "true"
    assert seen["params"]["timeframe"] == "1D"

    Response.json = staticmethod(lambda: {**feature_payload(), "sourceIsSynthetic": True})
    assert opportunities.fetch_completed_features("NVDA", as_of=T0) == {
        "available": False, "reason": "synthetic",
    }


def test_run_is_idempotent_for_the_same_completed_bar_fingerprint(conn):
    payloads = {
        "SPY": result_for("SPY", feature_payload("SPY")),
        "NVDA": result_for("NVDA", feature_payload("NVDA")),
    }

    def fetcher(ticker, *, as_of):
        assert as_of == T0
        return payloads[ticker]

    first = opportunities.run_opportunity_radar(
        conn, now=T0, universe_=["NVDA"], feature_fetcher=fetcher,
    )
    second = opportunities.run_opportunity_radar(
        conn, now=T0, universe_=["NVDA"], feature_fetcher=fetcher,
    )
    assert first["skipped"] is False
    assert first["candidateCount"] == 1
    assert second["skipped"] is True
    assert second["runId"] == first["runId"]
    assert second["persistedCandidateCount"] == 0
    assert conn.execute("SELECT count(*) AS n FROM opportunity_runs").fetchone()["n"] == 1

    body = opportunities.latest_opportunities(conn, now=T0)
    assert body["candidates"][0]["ticker"] == "NVDA"
    assert body["candidates"][0]["paperEligibility"]["state"] == "not-assessed"


def test_latest_read_withholds_pre_relevance_fix_snapshots(conn):
    conn.execute(
        "INSERT INTO opportunity_runs (id,detector_version,universe_version,universe,benchmark,"
        "data_fingerprint,as_of,started_at,completed_at,status,coverage,degraded) "
        "VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
        (
            "opp_old_relevance", "early-opportunity@1", opportunities.UNIVERSE_VERSION,
            '["NVDA"]', "SPY", "old-relevance-fingerprint", opportunities._iso(T0),
            opportunities._iso(T0), opportunities._iso(T0), "success", '{"state":"ok"}', "[]",
        ),
    )
    conn.commit()

    body = opportunities.latest_opportunities(conn, now=T0)

    assert opportunities.DETECTOR_VERSION == "early-opportunity@2"
    assert body["runId"] is None
    assert body["coverage"] == {"state": "insufficient", "reason": "no-runs"}


def test_get_is_store_only_and_stale_snapshot_is_withheld(monkeypatch, conn):
    def fetcher(ticker, *, as_of):
        return result_for(ticker)

    opportunities.run_opportunity_radar(
        conn, now=T0, universe_=["NVDA"], feature_fetcher=fetcher,
    )
    monkeypatch.setattr(opportunities, "_now", lambda: T0)

    def explode(*_args, **_kwargs):
        raise AssertionError("GET /opportunities reached the analysis client")

    monkeypatch.setattr(opportunities.requests, "get", explode)
    app = FastAPI()
    app.include_router(opportunities.router)
    response = TestClient(app).get("/opportunities")
    assert response.status_code == 200
    assert response.json()["candidates"][0]["ticker"] == "NVDA"

    stale = opportunities.latest_opportunities(
        conn,
        now=T0 + timedelta(seconds=opportunities.DEFAULT_MAX_RUN_AGE_SECONDS + 1),
    )
    assert stale["candidates"] == []
    assert stale["coverage"]["state"] == "stale"


def test_module_has_no_model_prediction_paper_or_order_seam():
    source = inspect.getsource(opportunities)
    for forbidden in ("LLM_URL", "PREDICTION_URL", "PAPER_URL", "/predict", "requests.post"):
        assert forbidden not in source
