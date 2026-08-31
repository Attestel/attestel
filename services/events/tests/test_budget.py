"""Per-provider daily budgets and backoff (AD-10, contract §3.10).

The three properties these tests exist to hold: a call is counted BEFORE it is made, an over-budget
provider is skipped with a visible `"<provider>:budget"`, and a 429 or 5xx blocks the provider so
the next pass skips it with `"<provider>:backoff"`.
"""
from __future__ import annotations

import re
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest

from app import budget
from app.budget import (
    BACKOFF_BASE_SECONDS,
    BACKOFF_MAX_SECONDS,
    DAILY_LIMITS,
    daily_limit,
    degraded,
    next_backoff_seconds,
    record_failure,
    record_success,
    reserve,
    quota_summary,
    state,
)
from app.db import connect, migrate

SERVICE_ROOT = Path(__file__).resolve().parent.parent
NOW = datetime(2026, 8, 17, 12, 0, 0, tzinfo=timezone.utc)


@pytest.fixture()
def conn(tmp_path, monkeypatch):
    c = connect()
    migrate(c)
    yield c
    c.close()


def calls(conn, provider, date="2026-08-17"):
    row = conn.execute(
        "SELECT calls FROM provider_budget WHERE provider = ? AND date = ?", (provider, date)
    ).fetchone()
    return row["calls"] if row else None


# =================================================================================================
# Counting
# =================================================================================================


def test_a_reservation_increments_calls_before_the_request(conn):
    allowed, reason = reserve(conn, "marketaux", now=NOW)
    assert (allowed, reason) == (True, None)
    # The increment is committed by reserve() itself, so a crash mid-request cannot buy a free call.
    assert calls(conn, "marketaux") == 1
    reserve(conn, "marketaux", now=NOW)
    assert calls(conn, "marketaux") == 2


def test_over_budget_skips_with_the_budget_reason(conn, monkeypatch):
    monkeypatch.setenv("EVENTS_BUDGET_MARKETAUX", "2")
    assert reserve(conn, "marketaux", now=NOW)[0] is True
    assert reserve(conn, "marketaux", now=NOW)[0] is True
    allowed, reason = reserve(conn, "marketaux", now=NOW)
    assert allowed is False
    assert reason == "marketaux:budget"
    assert calls(conn, "marketaux") == 2  # a refused reservation does not spend a call


def test_the_budget_resets_on_the_utc_day(conn, monkeypatch):
    monkeypatch.setenv("EVENTS_BUDGET_MARKETAUX", "1")
    assert reserve(conn, "marketaux", now=NOW)[0] is True
    assert reserve(conn, "marketaux", now=NOW)[0] is False
    tomorrow = NOW + timedelta(days=1)
    assert reserve(conn, "marketaux", now=tomorrow)[0] is True


def test_the_env_override_beats_the_constant_and_a_malformed_one_does_not(monkeypatch):
    monkeypatch.setenv("EVENTS_BUDGET_GOOGLE_NEWS_RSS", "7")
    assert daily_limit("google-news-rss") == 7
    monkeypatch.setenv("EVENTS_BUDGET_GOOGLE_NEWS_RSS", "not-a-number")
    # A malformed budget must not stop the service booting offline (invariant #1).
    assert daily_limit("google-news-rss") == DAILY_LIMITS["google-news-rss"]


def test_the_env_override_name_is_the_documented_pattern(monkeypatch):
    """`EVENTS_BUDGET_<PROVIDER>`, '-' becoming '_'. Recorded in the handoff for .env.example."""
    monkeypatch.setenv("EVENTS_BUDGET_SEC_EDGAR", "3")
    assert daily_limit("sec-edgar") == 3


# =================================================================================================
# Backoff
# =================================================================================================


@pytest.mark.parametrize("status", [429, 500, 502, 503])
def test_a_rate_limit_or_server_error_blocks_the_provider(conn, status):
    reserve(conn, "alphavantage", now=NOW)
    blocked_until = record_failure(conn, "alphavantage", status=status, error=f"http {status}",
                                   now=NOW)
    assert blocked_until == (NOW + timedelta(seconds=BACKOFF_BASE_SECONDS)).strftime(
        "%Y-%m-%dT%H:%M:%SZ"
    )
    allowed, reason = reserve(conn, "alphavantage", now=NOW + timedelta(seconds=1))
    assert allowed is False
    assert reason == "alphavantage:backoff"


@pytest.mark.parametrize("status", [400, 401, 403, 404])
def test_a_client_error_does_not_block(conn, status):
    """A broken request is not a busy server. Blocking on a 404 would take a provider offline for
    hours because one ticker has no coverage."""
    reserve(conn, "fmp", now=NOW)
    assert record_failure(conn, "fmp", status=status, error="nope", now=NOW) is None
    assert reserve(conn, "fmp", now=NOW)[0] is True
    row = conn.execute(
        "SELECT last_error, blocked_until FROM provider_budget WHERE provider = 'fmp'"
    ).fetchone()
    assert row["blocked_until"] is None
    assert row["last_error"] == "nope"  # the breadcrumb is still recorded


def test_the_block_expires_and_the_provider_becomes_callable_again(conn):
    reserve(conn, "fred", now=NOW)
    record_failure(conn, "fred", status=429, error="rate limited", now=NOW)
    later = NOW + timedelta(seconds=BACKOFF_BASE_SECONDS + 1)
    assert reserve(conn, "fred", now=later)[0] is True


def test_backoff_escalates_while_blocked_and_is_capped():
    assert next_backoff_seconds(0) == BACKOFF_BASE_SECONDS
    assert next_backoff_seconds(BACKOFF_BASE_SECONDS) == BACKOFF_BASE_SECONDS * 2
    assert next_backoff_seconds(BACKOFF_BASE_SECONDS * 2) == BACKOFF_BASE_SECONDS * 4
    assert next_backoff_seconds(BACKOFF_MAX_SECONDS) == BACKOFF_MAX_SECONDS


def test_successive_failures_within_one_pass_escalate(conn):
    """A pass calls a per-ticker provider once per ticker, so an outage escalates inside the pass."""
    record_failure(conn, "marketaux", status=503, error="down", now=NOW)
    first = conn.execute(
        "SELECT blocked_until FROM provider_budget WHERE provider = 'marketaux'"
    ).fetchone()["blocked_until"]
    record_failure(conn, "marketaux", status=503, error="still down", now=NOW)
    second = conn.execute(
        "SELECT blocked_until FROM provider_budget WHERE provider = 'marketaux'"
    ).fetchone()["blocked_until"]
    assert second > first


def test_a_success_clears_the_block_and_the_breadcrumb(conn):
    record_failure(conn, "fred", status=500, error="boom", now=NOW)
    record_success(conn, "fred", now=NOW)
    row = conn.execute(
        "SELECT blocked_until, last_error FROM provider_budget WHERE provider = 'fred'"
    ).fetchone()
    assert row["blocked_until"] is None and row["last_error"] is None


def test_last_error_is_truncated(conn):
    record_failure(conn, "fmp", status=500, error="e" * 10_000, now=NOW)
    stored = conn.execute(
        "SELECT last_error FROM provider_budget WHERE provider = 'fmp'"
    ).fetchone()["last_error"]
    assert len(stored) == budget.LAST_ERROR_CAP


# =================================================================================================
# Vocabulary and /health state
# =================================================================================================


def test_degraded_strings_use_the_single_vocabulary():
    assert degraded("marketaux", "budget") == "marketaux:budget"
    for reason in (budget.REASON_BUDGET, budget.REASON_BACKOFF, budget.REASON_NO_KEY,
                   budget.REASON_ERROR, budget.REASON_NO_CIK, budget.REASON_UNPARSEABLE_TIME):
        assert re.fullmatch(r"[a-z-]+", reason), reason


def test_state_reports_every_provider_for_health(conn):
    reserve(conn, "marketaux", now=NOW)
    record_failure(conn, "fred", status=429, error="slow down", now=NOW)
    rows = {r["provider"]: r for r in state(conn, now=NOW)}
    assert set(rows) == set(DAILY_LIMITS)
    assert rows["marketaux"]["calls"] == 1
    assert rows["marketaux"]["remaining"] == rows["marketaux"]["limit"] - 1
    assert rows["marketaux"]["utilizationPct"] > 0
    assert rows["marketaux"]["status"] == "ok"
    assert rows["fred"]["blocked"] is True
    assert rows["fred"]["status"] == "blocked"
    assert rows["fmp"]["blocked"] is False


def test_quota_monitor_distinguishes_warning_exhaustion_and_scope(conn, monkeypatch):
    monkeypatch.setenv("EVENTS_BUDGET_MARKETAUX", "5")
    monkeypatch.setenv("EVENTS_BUDGET_ALPHAVANTAGE", "1")
    for _ in range(4):
        assert reserve(conn, "marketaux", now=NOW)[0] is True
    assert reserve(conn, "alphavantage", now=NOW)[0] is True

    rows = {row["provider"]: row for row in state(conn, now=NOW)}
    assert rows["marketaux"]["status"] == "warning"
    assert rows["marketaux"]["utilizationPct"] == 80.0
    assert rows["alphavantage"]["status"] == "exhausted"
    assert rows["alphavantage"]["remaining"] == 0

    summary = quota_summary(list(rows.values()), now=NOW)
    assert summary["scope"] == "events-ingestion"
    assert "gateway and prediction" in summary["scopeNote"]
    assert summary["warningProviders"] == ["marketaux"]
    assert summary["exhaustedProviders"] == ["alphavantage"]
    assert summary["attentionRequired"] is True
    assert summary["resetAt"] == "2026-08-18T00:00:00Z"


# =================================================================================================
# The structural rule: no provider call without a reservation
# =================================================================================================


def test_providers_reach_requests_through_exactly_one_budgeted_helper():
    """`providers.py` may not call `requests` outside `_get`, which reserves first.

    A source-level assertion on purpose: it is the only kind that catches a NEW provider function
    added later that forgets the reservation.
    """
    source = (SERVICE_ROOT / "app" / "providers.py").read_text(encoding="utf-8")
    call_sites = [
        line.strip()
        for line in source.splitlines()
        if re.search(r"\brequests\.", line) and not line.strip().startswith("#")
    ]
    assert call_sites == ["response = requests.get("], call_sites
    # and that one call site is inside _get, immediately after a reserve()
    body = source.split("def _get(", 1)[1].split("\ndef ", 1)[0]
    assert "reserve(conn, provider)" in body
    assert body.index("reserve(conn, provider)") < body.index("requests.get(")
