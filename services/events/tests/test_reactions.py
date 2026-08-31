"""Phase 4 — post-event reactions and empirical sensitivity.

The tests that matter here are the ones that prove what the system REFUSES to do:

* it never reads a bar from beyond a window's maturity;
* it never resolves a window that has not matured — that state is `pending`, not `unavailable`,
  and not a zero;
* it never lets a synthetic bar reach an aggregate;
* it never reports a statistic below the sample floor — not a rounded one, not a hidden one;
* it never turns a missing benchmark into a zero.

Session handling (before-market, after-market, weekend, holiday) is tested against constructed bar
series rather than a live market, so the rules are pinned rather than observed.
"""
from __future__ import annotations

from datetime import datetime, timedelta, timezone

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from app import reactions as reactions_module
from app.db import connect, migrate
from app.ingest import store_scheduled_events
from app.reactions import (
    CALC_VERSION,
    HORIZONS,
    MIN_SAMPLE,
    SESSION_AFTER,
    SESSION_BEFORE,
    SESSION_NON_TRADING,
    SESSION_REGULAR,
    STATE_PENDING,
    STATE_RESOLVED,
    STATE_UNAVAILABLE,
    REASON_IMMATURE,
    REASON_NO_REFERENCE,
    capture_once,
    classify_session,
    compute_windows,
    reference_index,
    router as reactions_router,
    sensitivity,
)
from app.relationships import rebuild_relationships

NOW = datetime(2026, 8, 23, 12, 0, 0, tzinfo=timezone.utc)


@pytest.fixture()
def conn(monkeypatch):
    monkeypatch.setenv("REACTION_CAPTURE_ENABLED", "true")
    monkeypatch.delenv("REACTION_MIN_SAMPLE", raising=False)
    c = connect()
    migrate(c)
    yield c
    c.close()


@pytest.fixture()
def client(conn):
    app = FastAPI()
    app.include_router(reactions_router)
    return TestClient(app)


def bars(start: str, closes, *, volume=1_000_000.0, spread=1.0):
    """A daily series starting at `start` and skipping weekends — the shape `/candles` returns."""
    out = []
    day = datetime.strptime(start, "%Y-%m-%d").date()
    for close in closes:
        while day.weekday() >= 5:
            day += timedelta(days=1)
        out.append({
            "time": day.isoformat(), "open": close, "high": close + spread,
            "low": close - spread, "close": close, "volume": volume,
        })
        day += timedelta(days=1)
    return out


# ── session classification ───────────────────────────────────────────────────────────────────────


@pytest.mark.parametrize("moment,expected", [
    # 08:30 ET is 12:30 UTC in August (EDT, UTC-4).
    ("2026-08-20T12:30:00Z", SESSION_BEFORE),
    # 11:00 ET / 15:00 UTC — inside the session.
    ("2026-08-20T15:00:00Z", SESSION_REGULAR),
    # 17:00 ET / 21:00 UTC — after the close. NVIDIA reports here.
    ("2026-08-20T21:00:00Z", SESSION_AFTER),
    # Exactly at the open and exactly at the close.
    ("2026-08-20T13:30:00Z", SESSION_REGULAR),
    ("2026-08-20T20:00:00Z", SESSION_AFTER),
    # Saturday.
    ("2026-08-22T15:00:00Z", SESSION_NON_TRADING),
    # Sunday.
    ("2026-08-23T15:00:00Z", SESSION_NON_TRADING),
])
def test_sessions_are_classified_deterministically(moment, expected):
    assert classify_session(moment) == expected


def test_a_holiday_is_recognised_from_the_stored_bars_not_from_a_calendar():
    """A weekday with no stored bar is a non-trading day — observed, not assumed."""
    trading = {"2026-11-25", "2026-11-27"}  # Thanksgiving 2026-11-26 has no bar
    assert classify_session("2026-11-26T15:00:00Z", trading_days=trading) == SESSION_NON_TRADING
    assert classify_session("2026-11-25T15:00:00Z", trading_days=trading) == SESSION_REGULAR


def test_without_stored_bars_only_weekends_are_known():
    """The honest degradation: with no bar set we genuinely do not know about holidays."""
    assert classify_session("2026-11-26T15:00:00Z") == SESSION_REGULAR


# ── the reference close ──────────────────────────────────────────────────────────────────────────


def test_a_before_market_event_measures_from_the_previous_close():
    series = bars("2026-08-17", [100, 101, 102, 103, 104])  # Mon..Fri
    # Wednesday 08:30 ET, before the open: Wednesday's own close already reflects it.
    index = reference_index(series, "2026-08-19T12:30:00Z", SESSION_BEFORE)
    assert series[index]["time"] == "2026-08-18"


def test_a_regular_session_event_measures_from_the_previous_close():
    series = bars("2026-08-17", [100, 101, 102, 103, 104])
    index = reference_index(series, "2026-08-19T15:00:00Z", SESSION_REGULAR)
    assert series[index]["time"] == "2026-08-18"


def test_an_after_market_event_measures_from_that_days_close():
    series = bars("2026-08-17", [100, 101, 102, 103, 104])
    # Wednesday 17:00 ET: the session already closed, so Wednesday's close is pre-event.
    index = reference_index(series, "2026-08-19T21:00:00Z", SESSION_AFTER)
    assert series[index]["time"] == "2026-08-19"


def test_a_weekend_event_measures_from_the_last_trading_close():
    series = bars("2026-08-17", [100, 101, 102, 103, 104])
    index = reference_index(series, "2026-08-22T15:00:00Z", SESSION_NON_TRADING)
    assert series[index]["time"] == "2026-08-21"


def test_a_holiday_event_measures_from_the_last_trading_close():
    series = [
        {"time": "2026-11-24", "open": 100, "high": 101, "low": 99, "close": 100, "volume": 1e6},
        {"time": "2026-11-25", "open": 101, "high": 102, "low": 100, "close": 101, "volume": 1e6},
        {"time": "2026-11-27", "open": 103, "high": 104, "low": 102, "close": 103, "volume": 1e6},
    ]
    index = reference_index(series, "2026-11-26T15:00:00Z", SESSION_NON_TRADING)
    assert series[index]["time"] == "2026-11-25"


def test_no_bar_before_the_event_is_no_reference_not_zero():
    series = bars("2026-08-24", [100, 101])
    assert reference_index(series, "2026-08-20T21:00:00Z", SESSION_AFTER) is None


# ── the look-ahead guard ─────────────────────────────────────────────────────────────────────────


def test_an_immature_window_is_pending_not_resolved_and_not_zero():
    series = bars("2026-08-17", [100, 101, 102])   # only two bars after the reference
    computed = compute_windows(series, 0, now=NOW)
    assert computed["windows"]["1d"]["state"] == STATE_RESOLVED
    for horizon in ("5d", "20d"):
        window = computed["windows"][horizon]
        assert window["state"] == STATE_PENDING
        assert window["missingReason"] == REASON_IMMATURE
        assert "rawReturn" not in window, "an unresolved window must carry no number at all"


def test_a_bar_dated_after_now_can_never_resolve_a_window():
    """Belt and braces on top of the store: the guard is STATED, not merely implied."""
    series = bars("2026-08-17", [100, 110, 120, 130])
    early = datetime(2026, 8, 18, 12, 0, 0, tzinfo=timezone.utc)
    computed = compute_windows(series, 0, now=early)
    assert computed["windows"]["1d"]["state"] == STATE_RESOLVED  # 2026-08-18 has passed
    # The 5d bar is 2026-08-21, which is after `early` — so it cannot be used yet.
    assert computed["windows"]["5d"]["state"] == STATE_PENDING


def test_a_resolved_window_reports_the_bars_it_used():
    # Starts far enough back that every bar of the 5-day window is already in the past at NOW.
    series = bars("2026-08-10", [100, 105, 110, 115, 120, 125, 130])
    window = compute_windows(series, 0, now=NOW)["windows"]["5d"]
    assert window["state"] == STATE_RESOLVED
    assert window["endTs"] == series[5]["time"]
    assert window["barsUsed"] == 5
    assert window["rawReturn"] == pytest.approx(125 / 100 - 1)


def test_a_zero_reference_close_is_unavailable_not_an_infinity():
    series = bars("2026-08-17", [0, 105, 110])
    assert compute_windows(series, 0, now=NOW)["windows"]["1d"]["state"] == STATE_UNAVAILABLE


def test_volume_and_range_changes_are_ratios_against_a_pre_event_baseline():
    pre = bars("2026-07-01", [100] * 20, volume=1_000_000.0, spread=1.0)
    post = bars("2026-07-29", [100, 100], volume=3_000_000.0, spread=3.0)
    series = pre + post
    window = compute_windows(series, len(pre) - 1, now=NOW)["windows"]["1d"]
    assert window["volumeChange"] == pytest.approx(2.0)   # 3x the baseline
    assert window["rangeChange"] == pytest.approx(2.0)


def test_a_zero_baseline_yields_none_not_an_infinity():
    series = bars("2026-08-03", [100] * 5, volume=0.0, spread=0.0)
    window = compute_windows(series, 3, now=NOW)["windows"]["1d"]
    assert window["volumeChange"] is None
    assert window["rangeChange"] is None


# ── capture ──────────────────────────────────────────────────────────────────────────────────────


def _event(conn, *, event_id_key="earnings|NVDA|2026-07-31", scheduled_at="2026-08-19T21:00:00Z",
           ticker="NVDA", kind="earnings"):
    store_scheduled_events(conn, [{
        "kind": kind, "ticker": ticker, "series": None, "scheduled_at": scheduled_at,
        "confirmed": 1, "status": "confirmed", "source": "company-ir",
        "source_tier": "official", "title": f"{ticker} earnings",
        "occurrence_key": event_id_key,
    }], now="2026-08-01T00:00:00Z")
    rebuild_relationships(conn, universe=(ticker,), now=NOW)
    return conn.execute(
        "SELECT id FROM scheduled_events WHERE occurrence_key = ?", (event_id_key,)
    ).fetchone()["id"]


def _stub_candles(monkeypatch, series_by_ticker, *, source="yfinance", synthetic=False):
    calls = []

    def fake(ticker, *, as_of, limit):
        calls.append({"ticker": ticker, "as_of": as_of, "limit": limit})
        if ticker not in series_by_ticker:
            raise reactions_module.CandleUnavailable(f"no bars for {ticker}")
        return {
            "ticker": ticker, "timeframe": "1D", "source": source,
            "sourceIsSynthetic": synthetic, "bars": series_by_ticker[ticker],
        }

    monkeypatch.setattr(reactions_module, "fetch_candles", fake)
    return calls


def test_capture_is_off_by_default(conn, monkeypatch):
    monkeypatch.setenv("REACTION_CAPTURE_ENABLED", "false")
    _event(conn)
    calls = _stub_candles(monkeypatch, {})
    report = capture_once(conn, now=NOW, tickers=["NVDA"])
    assert report["degraded"] == ["reactions:disabled"]
    assert report["examined"] == 0
    assert calls == [], "a disabled capture must not read a bar"


def test_capture_records_a_resolved_reaction_with_its_provenance(conn, monkeypatch):
    event_id = _event(conn)
    series = bars("2026-07-20", [100] * 20 + [100, 110, 111, 112, 113, 114])
    _stub_candles(monkeypatch, {"NVDA": series, "SPY": series})

    report = capture_once(conn, now=NOW, tickers=["NVDA"])
    assert report["examined"] == 1
    assert report["resolved"] >= 1

    row = conn.execute(
        "SELECT * FROM event_reactions WHERE event_id = ? AND ticker = 'NVDA'", (event_id,)
    ).fetchone()
    assert row["session"] == SESSION_AFTER
    assert row["calc_version"] == CALC_VERSION
    assert row["captured_at"]
    assert row["reference_ts"]
    assert row["reference_source"] == "yfinance"
    assert row["synthetic"] == 0

    window = conn.execute(
        "SELECT * FROM event_reaction_windows WHERE event_id = ? AND horizon = '1d'", (event_id,)
    ).fetchone()
    assert window["state"] == STATE_RESOLVED
    assert window["end_ts"]
    assert window["bars_used"] == 1
    assert window["bar_source"] == "yfinance"
    assert window["resolved_at"]


def test_an_unmatured_window_stays_pending_and_is_retried(conn, monkeypatch):
    event_id = _event(conn)
    # 22 weekday bars ending 2026-08-20 — exactly one bar past the 2026-08-19 after-market event,
    # so 1d has matured and 5d/20d have not.
    series = bars("2026-07-22", [100] * 20 + [100, 110])
    _stub_candles(monkeypatch, {"NVDA": series, "SPY": series})

    capture_once(conn, now=NOW, tickers=["NVDA"])
    states = {
        r["horizon"]: r["state"] for r in conn.execute(
            "SELECT horizon, state FROM event_reaction_windows WHERE event_id = ?", (event_id,)
        ).fetchall()
    }
    assert states["1d"] == STATE_RESOLVED
    assert states["5d"] == STATE_PENDING and states["20d"] == STATE_PENDING

    # The bars arrive later; the pending windows resolve. The already-resolved one is untouched.
    longer = bars("2026-07-20", [100] * 20 + [100] + [110 + i for i in range(25)])
    _stub_candles(monkeypatch, {"NVDA": longer, "SPY": longer})
    capture_once(conn, now=NOW + timedelta(days=40), tickers=["NVDA"])
    states = {
        r["horizon"]: r["state"] for r in conn.execute(
            "SELECT horizon, state FROM event_reaction_windows WHERE event_id = ?", (event_id,)
        ).fetchall()
    }
    assert states == {"1d": STATE_RESOLVED, "5d": STATE_RESOLVED, "20d": STATE_RESOLVED}


def test_failed_retry_preserves_metadata_for_an_already_resolved_window(conn, monkeypatch):
    event_id = _event(conn)
    series = bars("2026-07-22", [100] * 20 + [100, 110])
    _stub_candles(monkeypatch, {"NVDA": series, "SPY": series})
    capture_once(conn, now=NOW, tickers=["NVDA"])

    before = dict(conn.execute(
        "SELECT * FROM event_reactions WHERE event_id = ? AND ticker = 'NVDA'", (event_id,)
    ).fetchone())
    resolved_before = dict(conn.execute(
        "SELECT * FROM event_reaction_windows WHERE event_id = ? AND horizon = '1d'", (event_id,)
    ).fetchone())

    # The still-pending longer windows cause another pass, but the bars service is temporarily down.
    _stub_candles(monkeypatch, {})
    capture_once(conn, now=NOW + timedelta(hours=6), tickers=["NVDA"])

    after = dict(conn.execute(
        "SELECT * FROM event_reactions WHERE event_id = ? AND ticker = 'NVDA'", (event_id,)
    ).fetchone())
    resolved_after = dict(conn.execute(
        "SELECT * FROM event_reaction_windows WHERE event_id = ? AND horizon = '1d'", (event_id,)
    ).fetchone())
    for field in ("session", "reference_ts", "reference_close", "reference_source", "synthetic"):
        assert after[field] == before[field]
    assert resolved_after["state"] == STATE_RESOLVED
    assert resolved_after["raw_return"] == resolved_before["raw_return"]


def test_capture_is_idempotent_and_never_rewrites_a_resolved_window(conn, monkeypatch):
    event_id = _event(conn)
    series = bars("2026-07-20", [100] * 20 + [100, 110, 111, 112, 113, 114])
    _stub_candles(monkeypatch, {"NVDA": series, "SPY": series})

    capture_once(conn, now=NOW, tickers=["NVDA"])
    first = [dict(r) for r in conn.execute(
        "SELECT * FROM event_reaction_windows WHERE event_id = ? ORDER BY horizon", (event_id,)
    ).fetchall()]

    # The bars are REVISED. A resolved reaction is a point-in-time record and must not be rewritten.
    revised = bars("2026-07-20", [100] * 20 + [100, 999, 999, 999, 999, 999])
    _stub_candles(monkeypatch, {"NVDA": revised, "SPY": revised})
    capture_once(conn, now=NOW, tickers=["NVDA"])
    second = [dict(r) for r in conn.execute(
        "SELECT * FROM event_reaction_windows WHERE event_id = ? ORDER BY horizon", (event_id,)
    ).fetchall()]

    assert len(first) == len(second) == len(HORIZONS)
    resolved_before = {r["horizon"]: r["raw_return"] for r in first if r["state"] == STATE_RESOLVED}
    resolved_after = {r["horizon"]: r["raw_return"] for r in second if r["state"] == STATE_RESOLVED}
    assert resolved_before == resolved_after


def test_a_missing_benchmark_is_null_never_zero(conn, monkeypatch):
    event_id = _event(conn)
    series = bars("2026-07-22", [100] * 20 + [100, 110, 111, 112])
    _stub_candles(monkeypatch, {"NVDA": series})  # SPY deliberately absent

    capture_once(conn, now=NOW, tickers=["NVDA"])
    window = conn.execute(
        "SELECT * FROM event_reaction_windows WHERE event_id = ? AND horizon = '1d'", (event_id,)
    ).fetchone()
    assert window["state"] == STATE_RESOLVED
    assert window["raw_return"] is not None
    assert window["benchmark_return"] is None
    assert window["excess_return"] is None


def test_the_excess_return_is_the_difference_when_the_benchmark_exists(conn, monkeypatch):
    event_id = _event(conn)
    ticker_series = bars("2026-07-22", [100] * 20 + [100, 110])
    bench_series = bars("2026-07-22", [100] * 20 + [100, 105])
    _stub_candles(monkeypatch, {"NVDA": ticker_series, "SPY": bench_series})

    capture_once(conn, now=NOW, tickers=["NVDA"])
    window = conn.execute(
        "SELECT * FROM event_reaction_windows WHERE event_id = ? AND horizon = '1d'", (event_id,)
    ).fetchone()
    assert window["raw_return"] == pytest.approx(0.10)
    assert window["benchmark_return"] == pytest.approx(0.05)
    assert window["excess_return"] == pytest.approx(0.05)


def test_unreachable_bars_produce_unavailable_not_a_number(conn, monkeypatch):
    event_id = _event(conn)
    _stub_candles(monkeypatch, {})
    report = capture_once(conn, now=NOW, tickers=["NVDA"])
    assert report["unavailable"] == len(HORIZONS)
    rows = conn.execute(
        "SELECT * FROM event_reaction_windows WHERE event_id = ?", (event_id,)
    ).fetchall()
    assert {r["state"] for r in rows} == {STATE_UNAVAILABLE}
    assert all(r["raw_return"] is None for r in rows)


def test_unavailable_bars_are_retried_after_the_store_recovers(conn, monkeypatch):
    event_id = _event(conn)
    _stub_candles(monkeypatch, {})
    capture_once(conn, now=NOW, tickers=["NVDA"])

    series = bars("2026-07-20", [100] * 20 + [100, 110, 111, 112, 113, 114])
    calls = _stub_candles(monkeypatch, {"NVDA": series, "SPY": series})
    report = capture_once(conn, now=NOW + timedelta(hours=6), tickers=["NVDA"])

    assert report["examined"] == 1
    assert calls, "the unavailable pair must be read again after a transient outage"
    rows = conn.execute(
        "SELECT state FROM event_reaction_windows WHERE event_id = ?", (event_id,)
    ).fetchall()
    assert STATE_RESOLVED in {row["state"] for row in rows}


def test_no_reference_bar_is_recorded_as_such(conn, monkeypatch):
    event_id = _event(conn, scheduled_at="2026-06-01T21:00:00Z")
    series = bars("2026-07-20", [100] * 25)
    _stub_candles(monkeypatch, {"NVDA": series, "SPY": series})
    capture_once(conn, now=NOW, tickers=["NVDA"])
    row = conn.execute(
        "SELECT * FROM event_reaction_windows WHERE event_id = ? LIMIT 1", (event_id,)
    ).fetchone()
    assert row["state"] == STATE_UNAVAILABLE
    assert REASON_NO_REFERENCE in row["missing_reason"]


# ── empirical sensitivity ────────────────────────────────────────────────────────────────────────


def _resolve_many(conn, count, *, returns=None, synthetic=False, ticker="NVDA"):
    """Write `count` matured reactions directly, so the aggregation can be tested in isolation."""
    for i in range(count):
        key = f"earnings|{ticker}|sample-{i}"
        # A DISTINCT instant per sample. Two events with the same kind, ticker and instant are
        # the same occurrence by definition, and `store_scheduled_events` correctly merges them —
        # which would quietly shrink the sample this test is about.
        day = datetime(2026, 1, 5, tzinfo=timezone.utc) + timedelta(days=7 * i)
        event_id = _event(conn, event_id_key=key, ticker=ticker,
                          scheduled_at=day.strftime("%Y-%m-%dT21:00:00Z"))
        conn.execute(
            "INSERT INTO event_reactions (event_id, ticker, event_at, session, reference_ts, "
            " reference_close, reference_source, synthetic, calc_version, captured_at, updated_at) "
            "VALUES (?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT (event_id, ticker) DO NOTHING",
            (event_id, ticker, "2026-01-01T00:00:00Z", SESSION_AFTER, "2026-01-01",
             100.0, "yfinance", 1 if synthetic else 0, CALC_VERSION,
             "2026-01-02T00:00:00Z", "2026-01-02T00:00:00Z"),
        )
        value = (returns or [0.01 * (1 if i % 2 == 0 else -1)] * count)[i]
        conn.execute(
            "INSERT INTO event_reaction_windows (event_id, ticker, horizon, state, raw_return, "
            " benchmark_return, excess_return, synthetic, bar_source, resolved_at, updated_at) "
            "VALUES (?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT (event_id, ticker, horizon) DO NOTHING",
            (event_id, ticker, "1d", STATE_RESOLVED, value, 0.0, value,
             1 if synthetic else 0, "yfinance", "2026-01-02T00:00:00Z", "2026-01-02T00:00:00Z"),
        )
    conn.commit()


def test_below_the_sample_floor_no_statistic_is_reported_at_all(conn):
    _resolve_many(conn, MIN_SAMPLE - 1)
    result = sensitivity(conn, ticker="NVDA", horizon="1d")
    assert result["sufficient"] is False
    assert result["reason"] == "insufficient history"
    assert result["sampleCount"] == MIN_SAMPLE - 1
    assert result["shortBy"] == 1
    # NOT a rounded number, not a hidden one: nothing.
    for field in ("raw", "excess", "volumeChange", "rangeChange", "benchmarkCoverage"):
        assert result[field] is None, field


def test_at_the_floor_a_distribution_is_reported_not_a_single_number(conn):
    _resolve_many(conn, MIN_SAMPLE, returns=[0.02] * (MIN_SAMPLE // 2) + [-0.01] * (MIN_SAMPLE // 2))
    result = sensitivity(conn, ticker="NVDA", horizon="1d")
    assert result["sufficient"] is True
    raw = result["raw"]
    assert raw["count"] == MIN_SAMPLE
    for field in ("median", "mean", "stdev", "p25", "p75", "min", "max",
                  "positiveCount", "negativeCount", "positiveFrequency"):
        assert field in raw, field
    assert raw["positiveCount"] == MIN_SAMPLE // 2
    assert result["taxonomy"]["horizon"] == "1d"
    assert result["calcVersion"] == CALC_VERSION
    assert "association, not causation" in result["interpretation"]


def test_synthetic_outcomes_can_never_enter_an_aggregate(conn):
    _resolve_many(conn, MIN_SAMPLE + 5, synthetic=True)
    result = sensitivity(conn, ticker="NVDA", horizon="1d")
    assert result["sampleCount"] == 0
    assert result["sufficient"] is False
    assert result["syntheticExcluded"] is True


def test_a_synthetic_row_does_not_top_up_a_real_sample(conn):
    _resolve_many(conn, MIN_SAMPLE - 2)
    _resolve_many(conn, 5, synthetic=True, ticker="AMD")
    result = sensitivity(conn, horizon="1d")
    assert result["sampleCount"] == MIN_SAMPLE - 2
    assert result["sufficient"] is False


def test_unresolved_windows_never_count_toward_the_sample(conn, monkeypatch):
    _resolve_many(conn, MIN_SAMPLE - 1)
    event_id = _event(conn, event_id_key="earnings|NVDA|pending-one")
    conn.execute(
        "INSERT INTO event_reactions (event_id, ticker, event_at, session, calc_version, "
        " captured_at, updated_at) VALUES (?,?,?,?,?,?,?)",
        (event_id, "NVDA", "2026-01-01T00:00:00Z", SESSION_AFTER, CALC_VERSION,
         "2026-01-02T00:00:00Z", "2026-01-02T00:00:00Z"),
    )
    conn.execute(
        "INSERT INTO event_reaction_windows (event_id, ticker, horizon, state, updated_at) "
        "VALUES (?,?,?,?,?)",
        (event_id, "NVDA", "1d", STATE_PENDING, "2026-01-02T00:00:00Z"),
    )
    conn.commit()
    assert sensitivity(conn, ticker="NVDA", horizon="1d")["sufficient"] is False


def test_the_sample_is_point_in_time(conn):
    _resolve_many(conn, MIN_SAMPLE)
    # Every row above resolved on 2026-01-02; a cutoff before that sees none of them.
    early = sensitivity(conn, ticker="NVDA", horizon="1d", as_of="2026-01-01T00:00:00Z")
    assert early["sampleCount"] == 0 and early["sufficient"] is False
    later = sensitivity(conn, ticker="NVDA", horizon="1d", as_of="2026-06-01T00:00:00Z")
    assert later["sufficient"] is True


def test_the_floor_is_configurable_and_still_enforced(conn, monkeypatch):
    monkeypatch.setenv("REACTION_MIN_SAMPLE", "3")
    _resolve_many(conn, 3)
    assert sensitivity(conn, ticker="NVDA", horizon="1d")["sufficient"] is True
    monkeypatch.setenv("REACTION_MIN_SAMPLE", "50")
    assert sensitivity(conn, ticker="NVDA", horizon="1d")["sufficient"] is False


def test_an_unknown_horizon_is_rejected(conn):
    with pytest.raises(ValueError):
        sensitivity(conn, horizon="7d")


# ── the read surface ─────────────────────────────────────────────────────────────────────────────


def test_the_reaction_read_carries_the_bars_the_source_and_the_version(conn, client, monkeypatch):
    event_id = _event(conn)
    series = bars("2026-07-20", [100] * 20 + [100, 110, 111, 112, 113, 114])
    _stub_candles(monkeypatch, {"NVDA": series, "SPY": series})
    capture_once(conn, now=NOW, tickers=["NVDA"])

    body = client.get("/reactions", params={"eventId": event_id}).json()
    assert len(body["reactions"]) == 1
    reaction = body["reactions"][0]
    assert reaction["session"] == SESSION_AFTER
    assert reaction["calcVersion"] == CALC_VERSION
    assert reaction["referenceTs"] and reaction["referenceClose"]
    assert reaction["synthetic"] is False
    windows = {w["horizon"]: w for w in reaction["windows"]}
    assert set(windows) == set(HORIZONS)
    assert windows["1d"]["barSource"] == "yfinance"
    assert windows["1d"]["barsUsed"] == 1
    assert windows["20d"]["state"] == STATE_PENDING


def test_the_reaction_read_requires_a_subject(client):
    assert client.get("/reactions").status_code == 400


def test_the_sensitivity_read_refuses_below_the_floor(conn, client):
    _resolve_many(conn, 3)
    body = client.get("/sensitivity", params={"ticker": "NVDA", "horizon": "1d"}).json()
    assert body["sufficient"] is False
    assert body["raw"] is None
    assert body["minimumSample"] == MIN_SAMPLE


def test_the_sensitivity_read_rejects_an_unknown_horizon(client):
    assert client.get("/sensitivity", params={"horizon": "7d"}).status_code == 400


def test_no_read_touches_a_provider_or_a_bar(conn, client, monkeypatch):
    def explode(*_a, **_kw):
        raise AssertionError("a reaction read fetched a bar")

    monkeypatch.setattr(reactions_module, "fetch_candles", explode)
    assert client.get("/reactions", params={"ticker": "NVDA"}).status_code == 200
    assert client.get("/sensitivity", params={"ticker": "NVDA"}).status_code == 200


def test_the_module_contains_no_model_seam():
    from pathlib import Path
    source = (Path(__file__).resolve().parent.parent / "app" / "reactions.py").read_text()
    assert "LLM" not in source
    assert "8002" not in source
    assert "call_model" not in source
