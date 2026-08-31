from __future__ import annotations

from datetime import date, datetime, timezone

import pytest

from app import estimate_snapshots
from app.estimate_snapshots import SnapshotConfig, collect, select_quarter_estimate, snapshot_stage


def _cfg(**over):
    values = {
        "universe": ["NVDA"], "api_key": "test-key", "max_calls": 20,
        "actual_lookback_days": 7,
    }
    values.update(over)
    return SnapshotConfig(**values)


@pytest.fixture(autouse=True)
def provider_sleeps(monkeypatch):
    calls = []
    monkeypatch.setattr(estimate_snapshots.time, "sleep", calls.append)
    return calls


def test_select_quarter_estimate_requires_exact_fiscal_period():
    payload = {"estimates": [{
        "date": "2026-04-30",
        "horizon": "current quarter",
        "eps_estimate_average": "1.20",
        "eps_estimate_high": "1.30",
        "eps_estimate_low": "1.10",
        "eps_estimate_analyst_count": "42",
    }]}
    assert select_quarter_estimate(payload, "2026-04-30") == {
        "consensusEPS": 1.20, "estimateHigh": 1.30,
        "estimateLow": 1.10, "analystCount": 42,
    }
    assert select_quarter_estimate(payload, "2026-07-31") is None


def test_snapshot_stages_never_capture_on_or_after_report_date():
    today = date(2026, 8, 20)
    assert snapshot_stage(date(2026, 8, 21), today) == "t_minus_1"
    assert snapshot_stage(date(2026, 8, 27), today) == "t_minus_7"
    assert snapshot_stage(date(2026, 8, 20), today) is None
    assert snapshot_stage(date(2026, 8, 19), today) is None
    assert snapshot_stage(date(2026, 8, 28), today) is None


def test_collector_captures_one_due_snapshot_and_is_bounded(monkeypatch, provider_sleeps):
    saved = []
    monkeypatch.setattr(estimate_snapshots._db, "enabled", lambda: True)
    monkeypatch.setattr(estimate_snapshots._db, "estimate_snapshot_exists", lambda *_: False)
    monkeypatch.setattr(
        estimate_snapshots._db, "save_estimate_snapshot",
        lambda *args, **kwargs: saved.append((args, kwargs)) or True,
    )
    monkeypatch.setattr(estimate_snapshots._db, "list_estimate_snapshots", lambda _ticker: [])
    calendar = [{
        "symbol": "NVDA", "reportDate": "2026-08-26", "fiscalDateEnding": "2026-07-31",
    }]
    payload = {"estimates": [{
        "date": "2026-07-31", "eps_estimate_average": "0.95",
        "eps_estimate_analyst_count": "38",
    }]}
    result, code = collect(
        _cfg(), now=datetime(2026, 8, 20, 12, tzinfo=timezone.utc),
        calendar_fetch=lambda _key: calendar,
        estimates_fetch=lambda _ticker, _key: payload,
    )
    assert code == estimate_snapshots.EXIT_OK
    assert result["apiCalls"] == 2  # one calendar + one ticker estimate
    assert provider_sleeps == [estimate_snapshots.PROVIDER_MIN_INTERVAL_SECONDS]
    assert result["captured"] == [{
        "ticker": "NVDA", "fiscalDate": "2026-07-31", "stage": "t_minus_7",
    }]
    assert saved[0][0][:5] == (
        "NVDA", "2026-07-31", "2026-08-26", "t_minus_7", "alpha-vantage"
    )
    assert saved[0][1]["captured_at"] == "2026-08-20T12:00:00+00:00"


def test_existing_stage_spends_no_estimate_call(monkeypatch):
    monkeypatch.setattr(estimate_snapshots._db, "enabled", lambda: True)
    monkeypatch.setattr(estimate_snapshots._db, "estimate_snapshot_exists", lambda *_: True)
    monkeypatch.setattr(estimate_snapshots._db, "list_estimate_snapshots", lambda _ticker: [])
    calendar = [{
        "symbol": "NVDA", "reportDate": "2026-08-21", "fiscalDateEnding": "2026-07-31",
    }]
    result, code = collect(
        _cfg(), now=datetime(2026, 8, 20, 12, tzinfo=timezone.utc),
        calendar_fetch=lambda _key: calendar,
        estimates_fetch=lambda *_: (_ for _ in ()).throw(AssertionError("must not fetch")),
    )
    assert code == estimate_snapshots.EXIT_OK
    assert result["apiCalls"] == 1
    assert result["skippedExisting"][0]["stage"] == "t_minus_1"


def test_collector_refreshes_actual_after_a_forward_snapshot(monkeypatch):
    saved = []
    monkeypatch.setattr(estimate_snapshots._db, "enabled", lambda: True)
    monkeypatch.setattr(estimate_snapshots._db, "estimate_snapshot_exists", lambda *_: False)
    monkeypatch.setattr(estimate_snapshots._db, "list_estimate_snapshots", lambda _ticker: [{
        "expectedReportDate": "2026-08-20",
        "fiscalDate": "2026-07-31",
    }])
    monkeypatch.setattr(
        estimate_snapshots._db, "load_earnings_payload",
        lambda _ticker: {"payload": {"quarterlyEarnings": [{
            "fiscalDateEnding": "2026-04-30", "reportedEPS": "1.30",
        }]}},
    )
    monkeypatch.setattr(
        estimate_snapshots._db, "save_earnings_payload",
        lambda *args, **kwargs: saved.append((args, kwargs)),
    )
    result, code = collect(
        _cfg(), now=datetime(2026, 8, 20, 12, tzinfo=timezone.utc),
        calendar_fetch=lambda _key: [{
            "symbol": "NVDA", "reportDate": "2026-11-20", "fiscalDateEnding": "2026-10-31",
        }],
        earnings_fetch=lambda _ticker, _key: {
            "quarterlyEarnings": [{
                "fiscalDateEnding": "2026-07-31", "reportedDate": "2026-08-20",
                "reportedEPS": "1.30",
            }],
        },
    )
    assert code == estimate_snapshots.EXIT_OK
    assert result["apiCalls"] == 2
    assert result["actualsRefreshed"] == ["NVDA"]
    assert saved[0][0][0:2] == ("NVDA", "alpha-vantage")


def test_quota_exhaustion_reports_a_deferred_actual_refresh(monkeypatch):
    monkeypatch.setattr(estimate_snapshots._db, "enabled", lambda: True)
    monkeypatch.setattr(estimate_snapshots._db, "list_estimate_snapshots", lambda _ticker: [{
        "expectedReportDate": "2026-08-20", "fiscalDate": "2026-07-31",
    }])
    monkeypatch.setattr(
        estimate_snapshots._db, "load_earnings_payload",
        lambda _ticker: {"payload": {"quarterlyEarnings": []}},
    )
    result, code = collect(
        _cfg(max_calls=1), now=datetime(2026, 8, 20, 12, tzinfo=timezone.utc),
        calendar_fetch=lambda _key: [{
            "symbol": "NVDA", "reportDate": "2026-11-20",
            "fiscalDateEnding": "2026-10-31",
        }],
        earnings_fetch=lambda *_: (_ for _ in ()).throw(AssertionError("quota exhausted")),
    )
    assert code == estimate_snapshots.EXIT_OK
    assert result["apiCalls"] == 1
    assert result["quotaExhausted"] is True
    assert result["actualsRefreshed"] == []
