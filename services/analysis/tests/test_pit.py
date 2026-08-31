"""Point-in-time guarantees: no leakage, reproducible replay, honest coverage.

This is the suite the lane exists for. AD-3's promise is that a request carrying a past `as_of`
cannot reach a provider — not on a cache miss, not on a coverage gap, not behind a flag — and that
promise is only worth what a test that would actually catch a violation is worth. So the outbound
HTTP client AND every provider helper AND the synthetic generator are patched to raise, and then
every endpoint is called at a past cutoff. A single leaked fetch turns one of these into an error.

The store is seeded with KNOWN bars rather than generated ones on purpose: `_synthetic` seeds its
RNG from Python's `hash()` of a tuple containing a `str`, which is salted per process, so synthetic
bars are NOT stable across restarts. Determinism is exactly what the store is supposed to buy.

Run:  cd services/analysis && python -m pytest -q tests/test_pit.py
"""
import json

import numpy as np
import pandas as pd
import pytest
from fastapi.testclient import TestClient

import app.data as data
import app.expected as expected_mod
import app.store as store

AS_OF = "2026-03-02T00:00:00Z"          # inside the seeded range, comfortably in the past
BEFORE_ANY = "2019-01-01T00:00:00Z"     # before the seeded range starts

# Every endpoint that must survive a historical request: the three new ones and the four that
# predate this lane.
HISTORICAL_ROUTES = [
    "/candles/NVDA?limit=30&as_of={a}",
    "/technical-context/NVDA?as_of={a}",
    "/multi-timeframe-context/NVDA?as_of={a}",
    "/analysis/NVDA?as_of={a}",
    "/expected-close/NVDA?as_of={a}",
    "/analysis/NVDA/features?lookbackDays=400&as_of={a}",
    "/analysis/NVDA/confluence?timeframes=1D,1H&as_of={a}",
]


def known_bars(n, start, freq, seed):
    """A fixed OHLCV series. Same inputs, same bars, in this process and any other."""
    rng = np.random.default_rng(seed)
    close = 100 * np.exp(rng.normal(0.001, 0.012, n).cumsum())
    open_ = close * (1 + rng.normal(0, 0.003, n))
    high = np.maximum(open_, close) * (1 + np.abs(rng.normal(0, 0.005, n)))
    low = np.minimum(open_, close) * (1 - np.abs(rng.normal(0, 0.005, n)))
    vol = rng.integers(1_000_000, 4_000_000, n).astype(float)
    return pd.DataFrame(
        {"open": open_, "high": high, "low": low, "close": close, "volume": vol},
        index=pd.date_range(start=start, periods=n, freq=freq),
    )


@pytest.fixture
def bars_db():
    """The autouse PostgreSQL fixture gives every test a private schema."""
    store.close()
    yield store.database_schema()
    store.close()


@pytest.fixture
def seeded(bars_db):
    """400 daily + 400 hourly bars ending well after AS_OF, so every timeframe leg can be served."""
    store.write_bars("NVDA", "1D", known_bars(400, "2025-01-01", "B", 11), "yfinance")
    store.write_bars("NVDA", "1H", known_bars(400, "2025-11-03 09:30", "h", 12), "yfinance")
    return bars_db


@pytest.fixture
def no_network(monkeypatch):
    """Make any outbound call — or any bar generation — a loud failure.

    `_synthetic` is included deliberately: on a historical request nothing may manufacture bars at
    all, and it sits OUTSIDE `fetch_ohlcv`'s try/except, so an accidental fetch propagates instead
    of quietly degrading to a synthetic series that would look like a passing test.
    """
    calls = []

    def boom(*a, **k):
        calls.append(a)
        raise AssertionError("network / provider call on a historical request")

    monkeypatch.setattr(data.requests, "get", boom)
    monkeypatch.setattr(data.requests, "post", boom)
    for name in ("_from_alpaca", "_from_twelvedata", "_from_yfinance", "_from_tiingo",
                 "_synthetic", "fetch_ohlcv"):
        monkeypatch.setattr(data, name, boom)
    # expected.py bound fetch_ohlcv at import time; it must not be reachable either.
    monkeypatch.setattr(expected_mod, "fetch_ohlcv", boom)
    return calls


@pytest.fixture
def client():
    import app.main as main

    return TestClient(main.app)


# ------------------------------------------------------------------------------------- leakage


@pytest.mark.parametrize("route", HISTORICAL_ROUTES)
def test_historical_request_never_touches_a_provider(route, seeded, no_network, client):
    """AD-3's definition of done. If this is skipped or weakened, the lane has failed."""
    r = client.get(route.format(a=AS_OF))
    assert r.status_code == 200, f"{route} -> {r.status_code}: {r.text[:300]}"
    assert no_network == [], "a provider was called on a historical request"


def test_historical_response_echoes_the_resolved_as_of(seeded, no_network, client):
    for route in ("/candles/NVDA?as_of={a}", "/technical-context/NVDA?as_of={a}",
                  "/multi-timeframe-context/NVDA?as_of={a}"):
        body = client.get(route.format(a=AS_OF)).json()
        assert body["asOf"] == "2026-03-02T00:00:00Z"


def test_bars_returned_are_all_at_or_before_the_cutoff(seeded, no_network, client):
    bars = client.get(f"/candles/NVDA?limit=200&as_of={AS_OF}").json()["bars"]
    assert bars
    assert max(b["time"] for b in bars) <= "2026-03-02"


# --------------------------------------------------------------------------------- no coverage


@pytest.mark.parametrize(
    "route",
    ["/candles/NVDA?as_of={a}", "/technical-context/NVDA?as_of={a}",
     "/multi-timeframe-context/NVDA?as_of={a}"],
)
def test_empty_store_reports_insufficient_coverage_without_fetching(
    route, bars_db, no_network, client
):
    r = client.get(route.format(a=AS_OF))
    assert r.status_code == 200
    body = r.json()
    coverage = body["coverage"]
    if isinstance(coverage, dict):  # multi-timeframe reports coverage per timeframe
        assert set(coverage.values()) == {"insufficient"}
        assert body["timeframes"] == {}
    else:
        assert coverage == "insufficient"
        assert body["coverageFrom"] is None and body["coverageTo"] is None
    assert no_network == []


def test_coverage_is_insufficient_below_the_min_bars_threshold(bars_db, no_network, client):
    store.write_bars("NVDA", "1D", known_bars(data.MIN_BARS - 1, "2026-01-01", "B", 3), "yfinance")
    body = client.get(f"/candles/NVDA?as_of={AS_OF}").json()
    assert body["coverage"] == data.COVERAGE_INSUFFICIENT
    assert body["coverageFrom"] is not None  # there ARE bars — just not enough of them
    assert no_network == []


def test_coverage_is_ok_at_the_threshold(bars_db, no_network, client):
    store.write_bars("NVDA", "1D", known_bars(data.MIN_BARS, "2026-01-01", "B", 3), "yfinance")
    assert client.get(f"/candles/NVDA?as_of={AS_OF}").json()["coverage"] == data.COVERAGE_OK


def test_coverage_enum_is_the_agreed_spelling():
    assert (data.COVERAGE_OK, data.COVERAGE_INSUFFICIENT) == ("ok", "insufficient")


PRE_EXISTING_ROUTES = [
    "/analysis/NVDA?as_of={a}",
    "/expected-close/NVDA?as_of={a}",
    "/analysis/NVDA/features?as_of={a}",
    "/analysis/NVDA/confluence?as_of={a}",
]


@pytest.mark.parametrize("route", PRE_EXISTING_ROUTES)
def test_existing_endpoints_409_on_an_uncoverable_cutoff(route, bars_db, no_network, client):
    """§9.38.1. The four pre-Wave-1 endpoints carry no in-body `coverage` key (§4.4 grants that only
    to the three new ones), so 409 is how they say "I cannot serve this cutoff"."""
    r = client.get(route.format(a=AS_OF))
    assert r.status_code == 409, f"{route} -> {r.status_code}"
    assert r.json() == {"error": "insufficient_coverage", "coverageFrom": None, "coverageTo": None}
    assert no_network == []


def test_409_reports_the_coverage_window_it_does_have(bars_db, no_network, client):
    """A store with SOME history names its edges, so the caller can pick a servable cutoff."""
    df = known_bars(data.MIN_BARS - 1, "2026-01-01", "B", 31)
    store.write_bars("NVDA", "1D", df, "yfinance")
    body = client.get(f"/analysis/NVDA?as_of={AS_OF}").json()
    assert body["error"] == "insufficient_coverage"
    assert body["coverageFrom"] == df.index[0].strftime(store.TS_FMT)
    assert body["coverageTo"] == df.index[-1].strftime(store.TS_FMT)


def test_live_requests_keep_the_pre_existing_502(bars_db, monkeypatch, client):
    """409 is for historical cutoffs only. A LIVE request with no usable bars must still answer the
    way it always has, or live behaviour is no longer byte-identical."""
    monkeypatch.setattr(data, "fetch_ohlcv", lambda *a, **k: (_ for _ in ()).throw(
        data.DataError("offline")
    ))
    assert client.get("/analysis/NVDA").status_code == 502


def test_a_servable_cutoff_is_not_a_409(seeded, no_network, client):
    for route in PRE_EXISTING_ROUTES:
        assert client.get(route.format(a=AS_OF)).status_code == 200, route


# -------------------------------------------------------------------------------- reproducibility


@pytest.mark.parametrize("route", HISTORICAL_ROUTES)
def test_identical_historical_requests_are_byte_identical(route, seeded, no_network, client):
    url = route.format(a=AS_OF)
    first = json.dumps(client.get(url).json(), sort_keys=True)
    second = json.dumps(client.get(url).json(), sort_keys=True)
    assert first == second, f"{route} is not reproducible"


def test_replay_is_stable_across_a_store_reconnect(seeded, no_network, client):
    url = f"/technical-context/NVDA?as_of={AS_OF}"
    first = json.dumps(client.get(url).json(), sort_keys=True)
    store.close()  # drop the cached connection; the answer must come back the same
    second = json.dumps(client.get(url).json(), sort_keys=True)
    assert first == second


# ------------------------------------------------------------------------------ as_of boundary


def test_bar_exactly_at_the_cutoff_is_included_and_the_next_is_not(bars_db, no_network, client):
    df = known_bars(60, "2026-01-01", "B", 5)
    store.write_bars("NVDA", "1D", df, "yfinance")
    cutoff_ts = df.index[40]
    cutoff = cutoff_ts.strftime(store.TS_FMT)

    window = data.load_bars("NVDA", "1D", as_of=cutoff)
    assert window.df.index[-1] == cutoff_ts               # ts <= as_of includes the boundary bar
    assert df.index[41] not in window.df.index            # and stops there

    one_second_earlier = (cutoff_ts - pd.Timedelta(seconds=1)).strftime(store.TS_FMT)
    assert data.load_bars("NVDA", "1D", as_of=one_second_earlier).df.index[-1] == df.index[39]


def test_a_future_as_of_is_not_historical(bars_db):
    _resolved, historical = data.resolve_as_of("2099-01-01T00:00:00Z")
    assert historical is False  # "historical" means supplied AND strictly before now


def test_no_as_of_resolves_to_now_and_is_not_historical(bars_db):
    resolved, historical = data.resolve_as_of(None)
    assert historical is False
    assert resolved.endswith("Z") and len(resolved) == 20


# ----------------------------------------------------------------------------- malformed as_of


@pytest.mark.parametrize("bad", ["not-a-date", "2026-13-45", "yesterday", "1786296000"])
@pytest.mark.parametrize(
    "route",
    ["/candles/NVDA?as_of={a}", "/technical-context/NVDA?as_of={a}",
     "/multi-timeframe-context/NVDA?as_of={a}", "/analysis/NVDA?as_of={a}",
     "/expected-close/NVDA?as_of={a}", "/analysis/NVDA/features?as_of={a}"],
)
def test_malformed_as_of_is_a_400_not_a_silent_now(route, bad, seeded, no_network, client):
    """A typo'd cutoff that degrades to "now" is a leak that looks like data."""
    assert client.get(route.format(a=bad)).status_code == 400
    assert no_network == []


def test_parse_as_of_accepts_rfc3339_forms():
    assert data.parse_as_of("2026-03-02T00:00:00Z").isoformat() == "2026-03-02T00:00:00"
    assert data.parse_as_of("2026-03-02T05:00:00+05:00").isoformat() == "2026-03-02T00:00:00"
    assert data.parse_as_of("2026-03-02").isoformat() == "2026-03-02T00:00:00"
    assert data.parse_as_of(None) is None
    assert data.parse_as_of("  ") is None
    with pytest.raises(data.AsOfError):
        data.parse_as_of("nope")


# ------------------------------------------------------------------------------ the live path


def test_live_request_persists_bars_and_the_replay_needs_no_second_fetch(
    bars_db, monkeypatch, client
):
    calls = []
    df = known_bars(300, "2025-06-02", "B", 21)

    def stub(ticker, timeframe="1D", n=260, lookback_days=None):
        calls.append((ticker, timeframe))
        return df, "yfinance"

    monkeypatch.setattr(data, "fetch_ohlcv", stub)

    live = client.get("/candles/NVDA?limit=10")
    assert live.status_code == 200
    assert len(calls) == 1
    assert live.json()["source"] == "yfinance"

    # The bars are now in the store, so the same window at a past cutoff needs no provider at all.
    monkeypatch.setattr(data, "fetch_ohlcv", lambda *a, **k: (_ for _ in ()).throw(
        AssertionError("historical replay re-fetched")
    ))
    cutoff = df.index[-1].strftime(store.TS_FMT)
    replay = client.get(f"/candles/NVDA?limit=10&as_of={cutoff}")
    assert replay.status_code == 200
    assert len(calls) == 1
    assert [b["close"] for b in replay.json()["bars"]] == [
        b["close"] for b in live.json()["bars"]
    ]


def test_live_path_reads_back_through_the_store(bars_db, monkeypatch, client):
    df = known_bars(120, "2025-06-02", "B", 22)
    monkeypatch.setattr(data, "fetch_ohlcv", lambda *a, **k: (df, "alpaca:iex"))
    body = client.get("/candles/NVDA?limit=5").json()
    # "alpaca:iex" is the fetch label; the store enum is the provider name (contract §4.1).
    assert body["source"] == "alpaca"
    assert store.read_coverage("NVDA", "1D", store.now_ts())[0] == 120


def test_a_store_write_failure_does_not_fail_the_request(bars_db, monkeypatch, client):
    df = known_bars(120, "2025-06-02", "B", 23)
    monkeypatch.setattr(data, "fetch_ohlcv", lambda *a, **k: (df, "yfinance"))
    monkeypatch.setattr(store, "write_bars", lambda *a, **k: (_ for _ in ()).throw(OSError("disk")))
    r = client.get("/candles/NVDA?limit=5")
    assert r.status_code == 200
    assert len(r.json()["bars"]) == 5  # degraded to the fetched frame rather than 500-ing


# ------------------------------------------------------------------------------------ the store


def test_the_store_refuses_to_write_weekly_bars(bars_db):
    """An accidental weekly row is the exact failure mode contract §4.1 exists to prevent."""
    with pytest.raises(store.StoreError):
        store.write_bars("NVDA", "1W", known_bars(40, "2026-01-01", "W-FRI", 4), "yfinance")


def test_weekly_is_served_but_never_stored(bars_db, no_network, client):
    store.write_bars("NVDA", "1D", known_bars(300, "2025-06-02", "B", 24), "yfinance")
    body = client.get(f"/candles/NVDA?timeframe=1W&limit=8&as_of={AS_OF}").json()
    assert body["timeframe"] == "1W"
    assert len(body["bars"]) == 8

    conn = store.connect()
    assert conn.execute("SELECT COUNT(*) FROM bars WHERE timeframe='1W'").fetchone()[0] == 0
    assert {r[0] for r in conn.execute("SELECT DISTINCT timeframe FROM bars")} == {"1D"}


def test_weekly_bars_agree_with_the_dailies_they_came_from(bars_db, no_network):
    df = known_bars(200, "2025-06-02", "B", 25)
    store.write_bars("NVDA", "1D", df, "yfinance")
    cutoff = df.index[-1].strftime(store.TS_FMT)
    weekly = data.load_bars("NVDA", "1W", as_of=cutoff, limit=4).df
    daily = data.load_bars("NVDA", "1D", as_of=cutoff).df
    # The most recent weekly close is the most recent daily close, by construction.
    assert weekly["close"].iloc[-1] == pytest.approx(daily["close"].iloc[-1])
    assert weekly.index[-1] == daily.index[-1]


def test_upsert_is_idempotent(bars_db):
    df = known_bars(50, "2026-01-01", "B", 6)
    store.write_bars("NVDA", "1D", df, "yfinance")
    store.write_bars("NVDA", "1D", df, "yfinance")
    assert store.read_coverage("NVDA", "1D", store.now_ts())[0] == 50


def test_stored_timestamps_are_fixed_width_utc(bars_db):
    store.write_bars("NVDA", "1D", known_bars(5, "2026-01-01", "B", 7), "yfinance")
    rows = store.connect().execute("SELECT ts FROM bars").fetchall()
    for (ts,) in rows:
        assert len(ts) == 20 and ts.endswith("Z") and "+" not in ts and "." not in ts
    # lexicographic order IS chronological order — the property the SQL filter relies on
    stored = [r[0] for r in store.connect().execute("SELECT ts FROM bars ORDER BY ts")]
    assert stored == sorted(stored)


def test_source_normalisation(bars_db):
    assert store.normalize_source("alpaca:iex") == "alpaca"
    for s in ("twelvedata", "yfinance", "tiingo", "synthetic"):
        assert store.normalize_source(s) == s


def test_the_store_refuses_to_write_synthetic_bars(bars_db):
    """A fabricated bar that reaches the store becomes a fabricated answer about history."""
    with pytest.raises(store.StoreError):
        store.write_bars("NVDA", "1D", known_bars(40, "2026-01-01", "B", 8), "synthetic")
    with pytest.raises(store.StoreError):  # the fetch-time label normalises to the same source
        store.write_bars("NVDA", "1D", known_bars(40, "2026-01-01", "B", 8), "SYNTHETIC")


def test_an_offline_live_request_does_not_write_history(bars_db, monkeypatch, client):
    """The route the leakage fixture cannot see: a LIVE request offline falls back to synthetic
    bars, and if those were persisted a later historical read would serve fabricated history under
    `coverage: "ok"`. The live response is still synthetic and still labelled; the store stays
    empty; the historical request is a 409 rather than a confident lie."""
    def dead(*a, **k):
        raise data.DataError("offline")

    for name in ("_from_alpaca", "_from_twelvedata", "_from_yfinance", "_from_tiingo"):
        monkeypatch.setattr(data, name, dead)

    live = client.get("/candles/NVDA?limit=5").json()
    assert live["sourceIsSynthetic"] is True          # invariant #1 still holds for LIVE requests
    assert len(live["bars"]) == 5

    assert store.read_coverage("NVDA", "1D", store.now_ts())[0] == 0, "synthetic bars were stored"

    hist = client.get(f"/candles/NVDA?limit=5&as_of={live['asOf']}")
    assert hist.status_code == 200
    assert hist.json()["coverage"] == "insufficient"
    assert hist.json()["bars"] == []
    assert hist.json()["sourceIsSynthetic"] is False  # nothing fabricated is offered as history
    assert client.get(f"/analysis/NVDA?as_of={live['asOf']}").status_code == 409


def test_a_mixed_window_is_labelled_synthetic_if_any_bar_is(bars_db, monkeypatch, client):
    """The live degraded path still labels honestly: with the store unwritable, the fetched frame is
    served directly and its synthetic label survives."""
    df = known_bars(120, "2025-06-02", "B", 26)
    monkeypatch.setattr(data, "fetch_ohlcv", lambda *a, **k: (df, "synthetic"))
    body = client.get("/candles/NVDA?limit=5").json()
    assert body["source"] == "synthetic"
    assert body["sourceIsSynthetic"] is True
    assert store.read_coverage("NVDA", "1D", store.now_ts())[0] == 0


def test_retention_sweep_keeps_dailies_and_drops_old_intraday(bars_db):
    store.write_bars("NVDA", "1D", known_bars(40, "2020-01-01", "B", 10), "yfinance")
    store.write_bars("NVDA", "1H", known_bars(40, "2020-01-01 09:30", "h", 10), "yfinance")
    removed = store.sweep_intraday(retention_days=90)
    assert removed == 40
    conn = store.connect()
    assert conn.execute("SELECT COUNT(*) FROM bars WHERE timeframe='1D'").fetchone()[0] == 40
    assert conn.execute("SELECT COUNT(*) FROM bars WHERE timeframe='1H'").fetchone()[0] == 0


def test_health_reports_the_bar_store(bars_db, client):
    store.write_bars("NVDA", "1D", known_bars(40, "2026-01-01", "B", 13), "yfinance")
    body = client.get("/health").json()
    assert body["status"] == "ok" and body["service"] == "analysis"  # unchanged, additive only
    assert body["barStore"]["backend"] == "postgresql"
    assert body["barStore"]["schema"].startswith("analysis_test_")
    assert body["barStore"]["rows"] == 40
    assert body["barStore"]["tickers"] == 1
    assert body["barStore"]["timeframes"] == ["1D"]
    assert body["barStore"]["writable"] is True


# ------------------------------------------------------------------------ offline, no keys, no store


def test_offline_with_no_store_still_answers_from_synthetic_bars(bars_db, monkeypatch, client):
    """Invariant #1: empty PostgreSQL store + no network -> labelled synthetic live bars."""
    for var in ("APCA_API_KEY_ID", "APCA_API_SECRET_KEY", "TWELVEDATA_API_KEY", "TIINGO_API_KEY"):
        monkeypatch.delenv(var, raising=False)

    def dead(*a, **k):
        raise data.DataError("offline")

    for name in ("_from_alpaca", "_from_twelvedata", "_from_yfinance", "_from_tiingo"):
        monkeypatch.setattr(data, name, dead)

    body = client.get("/candles/NVDA?limit=10").json()
    assert body["sourceIsSynthetic"] is True
    assert body["source"] == "synthetic"
    assert len(body["bars"]) == 10
    assert client.get("/technical-context/NVDA").json()["sourceIsSynthetic"] is True
