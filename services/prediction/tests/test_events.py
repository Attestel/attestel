"""Tests for the offline event-study harness (app/events.py + app/evaluate_events.py).

All tests run WITHOUT network, Alpha Vantage, or the LLM — the honesty guarantees (no look-ahead,
real-data refusal, permutation behavior, Test-2 small-sample) must be verifiable offline.

Run:  cd services/prediction && python -m pytest -q tests/test_events.py
"""
import os

import numpy as np
import pandas as pd
import pytest

from app import evaluate_events, events as events_app
from app.evaluate_events import (
    EventConfig,
    _cyclic_net,
    apply_tercile_filter,
    apply_sue_thresholds,
    build_event_stream,
    decide_event_verdict,
    event_net,
    run_test2,
    split_by_date,
)
from app.events import EarningsEvent, attach_forward_estimates, build_events, compute_sue
from app.evaluate import pool


# --------------------------------------------------------------------------- fixtures

def _cfg(**over) -> EventConfig:
    base = dict(
        universe=["TEST"], horizons=[10], sue_quantile=0.33, min_events=100, cost_bps=6.0,
        permutations=100, holdout_frac=0.2, seed=42, history_days=3650, timeframe="1D",
        analysis_url="http://analysis", out_dir="/tmp/ev-test", cache_dir="/tmp/ev-test/earnings",
        text_dir="/tmp/ev-test/text", av_key="", llm_url="http://llm", enable_ai=True,
        benchmark="SPY", min_dates=1, min_holdout_dates=1, min_tickers=1,
        min_coverage=1.0,
    )
    base.update(over)
    return EventConfig(**base)


def _price_df(start="2020-01-01", n=400):
    idx = [str(d.date()) for d in pd.bdate_range(start=start, periods=n)]
    # open ramps deterministically; enough bars for any horizon window
    opens = 100.0 + np.arange(n) * 0.1
    return pd.DataFrame({"open": opens, "close": opens + 0.5}, index=idx)


def _events(directions, returns_h, horizon=10, start_day=0):
    """Synthetic tradeable-event dicts for the pooling/permutation functions."""
    out = []
    for i, (d, r) in enumerate(zip(directions, returns_h)):
        out.append({
            "ticker": "T", "reportedDate": f"2020-{(i % 12) + 1:02d}-01",
            "entryDate": f"2020-{(i % 12) + 1:02d}-02", "sue": float(d) * 2.0,
            "sueSource": "std", "direction": float(d), "surprisePct": 5.0,
            "returns": {horizon: float(r)},
        })
    return out


def test_event_sufficiency_floors_cannot_be_weakened_from_environment(monkeypatch):
    monkeypatch.setenv("EVENT_MIN_DATES", "1")
    with pytest.raises(ValueError, match="cannot be weakened"):
        EventConfig.from_env()


# --------------------------------------------------------------------------- 1. SUE math

def test_sue_uses_one_standardized_scale_and_skips_early_events():
    surprises = [0.1, -0.1, 0.1, -0.1, 0.2]  # events 0..4 (oldest first)
    evs = [
        EarningsEvent("T", f"2020-0{i+1}-30", f"2020-0{i+1}-28", 1.0, 0.9, s, 11.0)
        for i, s in enumerate(surprises)
    ]
    compute_sue(evs, window=8, min_prior=4)

    # events 0..3 have < 4 priors. They are skipped rather than mixing raw percentages with z-scores.
    for e in evs[:4]:
        assert e.sue_source == ""
        assert e.sue is None
    # event 4 has exactly 4 priors [0.1,-0.1,0.1,-0.1]; std(ddof=1) = 0.11547
    prior = [0.1, -0.1, 0.1, -0.1]
    expected = 0.2 / np.std(prior, ddof=1)
    assert evs[4].sue_source == "std"
    assert evs[4].sue == pytest.approx(expected, rel=1e-9)


def test_sue_is_prior_only_no_future_leak():
    """An event's SUE must not change when LATER events' surprises change (prior-only std)."""
    surprises = [0.1, -0.2, 0.15, -0.1, 0.3, -0.25, 0.2, 0.1]
    mk = lambda ss: [EarningsEvent("T", f"d{i}", f"r{i}", 1.0, 0.9, s, 10.0) for i, s in enumerate(ss)]
    a = mk(surprises)
    compute_sue(a)
    perturbed = list(surprises)
    perturbed[6] = 5.0  # change a LATE event's surprise
    b = mk(perturbed)
    compute_sue(b)
    # events before index 6 must be identical (their SUE used only prior surprises)
    for i in range(6):
        assert a[i].sue == pytest.approx(b[i].sue, rel=1e-12) if a[i].sue is not None else b[i].sue is None


def test_forward_snapshot_overrides_unverified_history_before_sue():
    event = EarningsEvent(
        "NVDA", "2026-04-30", "2026-05-27", 1.30, 9.99, -8.69, -87.0
    )
    attached = attach_forward_estimates([event], [{
        "ticker": "NVDA",
        "fiscalDate": "2026-04-30",
        "stage": "t_minus_1",
        "consensusEPS": 1.20,
        "capturedAt": "2026-05-26T20:00:00+00:00",
        "provider": "alpha-vantage",
        "payloadSha256": "a" * 64,
    }])
    assert attached == 1
    assert event.estimate_verified is True
    assert event.estimated_eps == pytest.approx(1.20)
    assert event.surprise == pytest.approx(0.10)
    assert event.surprise_pct == pytest.approx(100 * 0.10 / 1.20)
    assert event.estimate_payload_sha256 == "a" * 64


def test_report_day_snapshot_is_not_accepted_as_pre_release():
    event = EarningsEvent(
        "NVDA", "2026-04-30", "2026-05-27", 1.30, 1.20, 0.10, 8.3
    )
    attached = attach_forward_estimates([event], [{
        "ticker": "NVDA",
        "fiscalDate": "2026-04-30",
        "stage": "t_minus_1",
        "consensusEPS": 1.25,
        "capturedAt": "2026-05-27T01:00:00+00:00",
        "provider": "alpha-vantage",
        "payloadSha256": "b" * 64,
    }])
    assert attached == 0
    assert event.estimate_verified is False


def test_snapshot_for_another_ticker_cannot_verify_an_event():
    event = EarningsEvent(
        "NVDA", "2026-04-30", "2026-05-27", 1.30, 1.20, 0.10, 8.3
    )
    attached = attach_forward_estimates([event], [{
        "ticker": "AMD",
        "fiscalDate": "2026-04-30",
        "stage": "t_minus_1",
        "consensusEPS": 1.25,
        "capturedAt": "2026-05-26T20:00:00+00:00",
        "provider": "alpha-vantage",
        "payloadSha256": "c" * 64,
    }])
    assert attached == 0
    assert event.estimate_verified is False


# --------------------------------------------------------------------------- 2. no look-ahead

def test_entry_is_strictly_after_report_date():
    df = _price_df(n=200)
    dates = list(df.index)
    # place a report exactly on a trading day; entry must be the NEXT trading day's open
    report_i = 50
    e = EarningsEvent("T", "2020-03-31", dates[report_i], 1.2, 1.0, 0.2, 20.0)
    e.sue = 1.5
    built = build_events([e], df, [10])
    assert len(built) == 1
    ev = built[0]
    assert ev["entryDate"] > ev["reportedDate"]          # strictly after -> earnings already public
    assert ev["entryDate"] == dates[report_i + 1]         # the very next session
    # return is open-to-open over exactly H sessions
    expected = df["open"].iloc[report_i + 1 + 10] / df["open"].iloc[report_i + 1] - 1.0
    assert ev["returns"][10] == pytest.approx(expected, rel=1e-12)


def test_event_before_price_window_is_dropped():
    """A report that predates our price coverage must be DROPPED, not silently mapped to the first
    bar (which would misalign the entry and leak the window-start return)."""
    df = _price_df(start="2024-07-08", n=200)  # coverage starts 2024-07-08 (mirrors Yahoo's ~2y 1D)
    old = EarningsEvent("T", "2020-02-20", "2020-02-20", 1.2, 1.0, 0.2, 20.0)
    old.sue = 1.5
    inwin = EarningsEvent("T", "2024-08-01", "2024-08-01", 1.2, 1.0, 0.2, 20.0)
    inwin.sue = 1.5
    built = build_events([old, inwin], df, [10])
    assert len(built) == 1                     # only the in-window event survives
    assert built[0]["reportedDate"] == "2024-08-01"
    assert built[0]["entryDate"] > "2024-08-01"


def test_report_on_nontrading_date_still_enters_after():
    df = _price_df(n=200)
    dates = list(df.index)
    e = EarningsEvent("T", "2020-02-15", "2020-02-15", 1.2, 1.0, 0.2, 20.0)  # a weekend
    e.sue = 1.0
    built = build_events([e], df, [10])
    assert built and built[0]["entryDate"] > "2020-02-15"
    assert built[0]["entryDate"] in dates


def test_event_net_applies_round_trip_cost():
    # one event, direction +1, +2% move, 10 bps cost per leg -> net = 0.02 - 2*0.001 = 0.018
    net = event_net([1.0], [0.02], cost_bps=10.0)
    assert net[0] == pytest.approx(0.018, abs=1e-9)


# --------------------------------------------------------------------------- 4. tercile + split

def test_tercile_keeps_only_strong_surprises():
    events = _events([1, 1, 1, -1, -1, -1], [0.01] * 6, horizon=10)
    for i, s in enumerate([0.1, 0.5, 3.0, -0.1, -0.5, -3.0]):
        events[i]["sue"] = s
        events[i]["direction"] = float(np.sign(s))
    strong, lo, hi = apply_tercile_filter(events, 0.33)
    sues = sorted(e["sue"] for e in strong)
    assert 3.0 in sues and -3.0 in sues          # the extremes are kept
    assert all(abs(e["sue"]) >= 0.4 for e in strong)  # weak middle dropped


def test_split_by_date_holdout_is_last_and_disjoint():
    events = _events([1] * 10, [0.01] * 10)
    for i, e in enumerate(events):
        e["entryDate"] = f"2021-01-{i+1:02d}"
    report, holdout, cutoff = split_by_date(events, 0.2)
    assert len(holdout) == 2 and len(report) == 8
    assert max(e["entryDate"] for e in report) < min(e["entryDate"] for e in holdout)
    assert cutoff == holdout[0]["entryDate"]


def test_split_by_date_never_splits_events_from_the_same_session():
    events = _events([1] * 6, [0.01] * 6)
    for i, e in enumerate(events):
        e["ticker"] = f"T{i}"
        e["entryDate"] = "2021-01-04" if i < 3 else "2021-04-05"
    report, holdout, cutoff = split_by_date(events, 0.5)
    assert len(report) == 3 and len(holdout) == 3
    assert {e["entryDate"] for e in report}.isdisjoint(
        {e["entryDate"] for e in holdout}
    )
    assert cutoff == "2021-04-05"


def test_holdout_outlier_cannot_move_the_report_era_sue_thresholds():
    report = _events([1, -1, 1, -1], [0.01] * 4)
    for e, sue in zip(report, [-3.0, -0.2, 0.2, 3.0]):
        e["sue"], e["direction"] = sue, float(np.sign(sue))
    _selected, low, high = apply_tercile_filter(report, 0.25)
    holdout = _events([1], [0.01])
    holdout[0]["sue"] = 1_000_000.0
    holdout[0]["direction"] = 1.0
    assert apply_sue_thresholds(holdout, low, high) == holdout
    assert (low, high) == pytest.approx((-0.9, 0.9))


def test_event_stream_uses_daily_abnormal_returns_and_one_ticker_slot():
    dates = [str(d.date()) for d in pd.bdate_range("2024-01-02", periods=8)]
    benchmark_open = np.array([100, 101, 102.01, 103.0301, 104.060401, 105.101005, 106.152015, 107.213535])
    stock_open = benchmark_open.copy()  # exactly the market: abnormal return must be zero
    stock = pd.DataFrame({"open": stock_open}, index=dates)
    benchmark = pd.DataFrame({"open": benchmark_open}, index=dates)
    event = {
        "ticker": "T", "entryDate": dates[1], "reportedDate": dates[0], "sue": 2.0,
        "direction": 1.0, "returns": {3: stock_open[4] / stock_open[1] - 1.0},
    }
    stream, used = build_event_stream("T", stock, benchmark, [event], 3)
    assert used == [event]
    assert stream is not None
    assert stream.positions.tolist() == [1.0, 1.0, 1.0, 0.0]
    assert stream.ret_next == pytest.approx([0.0, 0.0, 0.0, 0.0], abs=1e-12)


def test_same_day_events_are_parallel_capital_not_sequential_compounding():
    dates = [str(d.date()) for d in pd.bdate_range("2024-01-02", periods=3)]
    benchmark = pd.DataFrame({"open": [100.0, 100.0, 100.0]}, index=dates)
    event = {
        "entryDate": dates[0], "reportedDate": "2024-01-01", "sue": 2.0,
        "direction": 1.0, "returns": {1: 0.10},
    }
    streams = []
    for ticker in ("A", "B"):
        stock = pd.DataFrame({"open": [100.0, 110.0, 110.0]}, index=dates)
        stream, _ = build_event_stream(ticker, stock, benchmark, [{**event, "ticker": ticker}], 1)
        streams.append(stream)
    metrics = pool(streams, cost_bps=0.0, timeframe="1D")
    assert metrics["totalReturn"] == pytest.approx(0.10)
    assert metrics["nDates"] == 2


def test_daily_event_stream_pays_both_entry_and_exit_costs():
    dates = [str(d.date()) for d in pd.bdate_range("2024-01-02", periods=3)]
    flat = pd.DataFrame({"open": [100.0, 100.0, 100.0]}, index=dates)
    event = {
        "ticker": "T", "entryDate": dates[0], "reportedDate": "2024-01-01", "sue": 2.0,
        "direction": 1.0, "returns": {1: 0.0},
    }
    stream, _ = build_event_stream("T", flat, flat, [event], 1)
    metrics = pool([stream], cost_bps=10.0, timeframe="1D")
    assert metrics["totalReturn"] == pytest.approx((1 - 0.001) ** 2 - 1, abs=1e-4)


def test_permutation_turnover_is_invariant_when_an_episode_crosses_the_boundary():
    positions = np.array([1.0, 1.0, 0.0, 0.0])
    returns = np.zeros_like(positions)
    costs = [float(_cyclic_net(returns, np.roll(positions, k), 10.0).sum()) for k in range(4)]
    assert costs == pytest.approx([-0.002] * 4)


def test_unverified_estimate_vintage_can_never_mint_edge():
    cfg = _cfg(min_events=1)
    good = {"nEvents": 100, "nTrades": 100, "nDates": 300, "expectancy": 0.01, "sharpe": 1.0}
    holdout = {**good, "nDates": 80, "sharpe": 1.2}
    baseline = {**holdout, "sharpe": 0.1}
    perm = {"expectancy": {"pValue": 0.001}, "sharpe": {"pValue": 0.001}}
    suff = {
        "nDates": 300, "minDates": 1, "holdoutDates": 80, "minHoldoutDates": 1,
        "nStreams": 1, "minTickers": 1, "configuredTickers": 1, "coverage": 1.0,
        "minCoverage": 1.0, "failedTickers": [], "skippedTickers": [],
        "estimateVintageMode": "unverified_descriptive",
    }
    verdict, _p, checklist = decide_event_verdict(good, holdout, baseline, perm, cfg, suff)
    assert verdict == "INCONCLUSIVE"
    assert "unverified" in " ".join(checklist)


def test_forward_verified_mode_removes_only_the_provenance_refusal():
    cfg = _cfg(min_events=1)
    good = {"nEvents": 100, "nTrades": 100, "nDates": 300, "expectancy": 0.01, "sharpe": 1.0}
    holdout = {**good, "nDates": 80, "sharpe": 1.2}
    baseline = {**holdout, "sharpe": 0.1}
    perm = {"expectancy": {"pValue": 0.001}, "sharpe": {"pValue": 0.001}}
    suff = {
        "nDates": 300, "minDates": 1, "holdoutDates": 80, "minHoldoutDates": 1,
        "nStreams": 1, "minTickers": 1, "configuredTickers": 1, "coverage": 1.0,
        "minCoverage": 1.0, "failedTickers": [], "skippedTickers": [],
        "estimateVintageMode": "forward_verified",
    }
    verdict, _p, checklist = decide_event_verdict(good, holdout, baseline, perm, cfg, suff)
    assert verdict == "EDGE"
    assert all("vintage" not in item for item in checklist)


def test_successful_pead_run_reaches_a_report_without_crashing(monkeypatch, tmp_path):
    dates = [str(d.date()) for d in pd.bdate_range("2023-01-02", periods=180)]
    benchmark = pd.DataFrame({"open": 100.0 + np.arange(180) * 0.05}, index=dates)
    stock = pd.DataFrame({"open": 100.0 + np.arange(180) * 0.08}, index=dates)
    earnings = []
    for i in range(12):
        idx = 10 + i * 12
        e = EarningsEvent("TEST", dates[idx], dates[idx], 1.1, 1.0, 0.1, 10.0)
        e.sue = -2.0 if i % 2 else 2.0
        e.sue_source = "std"
        earnings.append(e)
    monkeypatch.setattr(evaluate_events, "get_earnings", lambda *_: (earnings, "database"))
    monkeypatch.setattr(evaluate_events, "compute_sue", lambda _events: None)
    monkeypatch.setattr(
        evaluate_events, "fetch_feature_frame",
        lambda ticker, *_args, **_kwargs: (
            (benchmark if ticker == "SPY" else stock), "real-test", False
        ),
    )
    cfg = _cfg(
        horizons=[2], min_events=1, permutations=10, out_dir=str(tmp_path), enable_ai=False
    )
    report, code = evaluate_events.run(cfg, write=False)
    assert code == evaluate_events.EXIT_OK
    assert report["verdict"] in {"INCONCLUSIVE", "NO EDGE", "SUSPECT"}
    assert report["test1"]["perHorizon"][2]["sufficiency"]["nStreams"] == 1
    assert report["benchmark"]["ticker"] == "SPY"
    assert report["config"]["studyVersion"] == "pead-abnormal-v2-forward-vintage"


# --------------------------------------------------------------------------- 5. refusals

def test_refuses_without_key_or_cache(tmp_path, capsys):
    cfg = _cfg(av_key="", cache_dir=str(tmp_path / "earnings"), out_dir=str(tmp_path))
    report, code = evaluate_events.run(cfg, write=False)
    assert code == evaluate_events.EXIT_NO_KEY and code != 0
    assert report["refused"] == "no_key"
    assert "NO EARNINGS DATA" in capsys.readouterr().out


def test_refuses_on_synthetic(monkeypatch, tmp_path, capsys):
    ev = EarningsEvent("NVDA", "2020-01-30", "2020-01-28", 1.0, 0.9, 0.1, 11.0)
    monkeypatch.setattr(evaluate_events, "get_earnings", lambda t, c, k: ([ev], "cache"))
    monkeypatch.setattr(evaluate_events, "fetch_feature_frame",
                        lambda *a, **k: (pd.DataFrame(), "synthetic", True))
    cfg = _cfg(universe=["NVDA"], av_key="x", out_dir=str(tmp_path))
    report, code = evaluate_events.run(cfg, write=False)
    assert code == evaluate_events.EXIT_SYNTHETIC and code != 0
    assert report["refused"] == "synthetic"
    assert "SYNTHETIC DATA" in capsys.readouterr().out


def test_earnings_reads_postgres_before_legacy_files_or_network(monkeypatch, tmp_path):
    raw = {"quarterlyEarnings": [{
        "reportedDate": "2026-05-27", "fiscalDateEnding": "2026-04-30",
        "reportedEPS": "1.30", "estimatedEPS": "1.20", "surprise": "0.10",
    }]}
    monkeypatch.setattr(events_app._db, "enabled", lambda: True)
    monkeypatch.setattr(
        events_app._db, "load_earnings_payload",
        lambda ticker: {"payload": raw, "provider": "alpha-vantage"},
    )
    monkeypatch.setattr(
        events_app, "fetch_earnings_av",
        lambda *_args, **_kwargs: pytest.fail("network must not run when PostgreSQL has the payload"),
    )
    rows, source = events_app.get_earnings("NVDA", str(tmp_path), "unused")
    assert source == "database"
    assert len(rows) == 1 and rows[0].reported_date == "2026-05-27"


def test_live_earnings_write_postgres_not_the_container_filesystem(monkeypatch, tmp_path):
    raw = {"quarterlyEarnings": [{"reportedDate": "2026-05-27"}]}
    saved = {}
    monkeypatch.setattr(events_app._db, "enabled", lambda: True)
    monkeypatch.setattr(events_app._db, "load_earnings_payload", lambda _ticker: None)
    monkeypatch.setattr(events_app, "fetch_earnings_av", lambda *_args, **_kwargs: raw)
    monkeypatch.setattr(
        events_app._db, "save_earnings_payload",
        lambda ticker, provider, payload, **meta: saved.update(
            ticker=ticker, provider=provider, payload=payload, meta=meta
        ),
    )
    rows, source = events_app.get_earnings("NVDA", str(tmp_path), "key")
    assert source == "live" and len(rows) == 1
    assert saved["provider"] == "alpha-vantage"
    assert saved["meta"]["vintage_status"] == "unverified"
    assert not (tmp_path / "NVDA.json").exists()


# --------------------------------------------------------------------------- 6. Test 2 (AI)

def _write_text(text_dir, ticker, date, text):
    d = os.path.join(text_dir, ticker.upper())
    os.makedirs(d, exist_ok=True)
    with open(os.path.join(d, date + ".txt"), "w") as fh:
        fh.write(text)


def test_test2_small_sample_is_inconclusive(tmp_path):
    cfg = _cfg(text_dir=str(tmp_path / "text"), min_events=100)
    events = _events([1, -1, 1], [0.02, -0.02, 0.03])
    for e in events:  # give all three retrievable text
        _write_text(cfg.text_dir, e["ticker"], e["reportedDate"], "strong beat, raised guidance")
    scorer = lambda tk, ao, tx: {"tone": 0.8, "guidanceDirection": "up", "confidence": 0.7, "riskFlags": []}
    res = run_test2(events, cfg, scorer=scorer)
    assert res["verdict"] == "INCONCLUSIVE"      # 3 < 100
    assert res["nWithText"] == 3
    assert "ACCUMULATE" in res["reason"].upper()


def test_test2_reports_increment_when_enough_text(tmp_path):
    cfg = _cfg(text_dir=str(tmp_path / "text"), min_events=3)
    events = _events([1, 1, -1, -1], [0.05, 0.04, -0.05, 0.06])  # last one: SUE down but rose
    for e in events:
        _write_text(cfg.text_dir, e["ticker"], e["reportedDate"], "beat")
    # AI agrees with the up names, flat/negative on the down names
    def scorer(tk, ao, tx):
        return {"tone": 0.6, "guidanceDirection": "up", "confidence": 0.6, "riskFlags": []}
    res = run_test2(events, cfg, scorer=scorer)
    assert res["verdict"] == "REPORTED"
    assert res["nWithText"] == 4
    ph = res["perHorizon"][10]
    assert "increment" in ph and ph["nAgree"] >= 1     # AI (tone +) agrees with the +1 direction events
    assert set(ph["increment"]) == {"expectancy", "sharpe", "hitRate"}


def test_test2_no_text_yields_zero_sample(tmp_path):
    cfg = _cfg(text_dir=str(tmp_path / "empty"), min_events=100)
    events = _events([1, -1], [0.02, -0.02])
    res = run_test2(events, cfg, scorer=lambda *a: {"tone": 0.5, "guidanceDirection": "up"})
    assert res["verdict"] == "INCONCLUSIVE" and res["nWithText"] == 0
