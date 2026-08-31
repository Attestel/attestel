"""Per-provider daily call budgets and failure backoff (AD-10, contract §3.10).

Every provider call in this service passes through `reserve()` first. There is no other way to
reach `requests` — `providers.py` asserts it in its own docstring and `tests/test_budget.py`
asserts it against the source.

Three rules run through the whole module:

* **The reservation increments `calls` BEFORE the request goes out**, not after. A crash, a timeout
  or a `SIGKILL` mid-request must not buy a free call: an over-count is a wasted call, an
  under-count is a rate-limit ban.
* **A skip is a visible state, never a silent one.** Every refusal returns a `degraded` string in
  the single vocabulary `"<provider>:<reason>"`, because Wave 2's feeds and the frontend render
  these strings verbatim.
* **Nothing here reaches a network or a model.** It is `provider_budget` arithmetic and a clock.

The daily limits below are named constants in ONE block, keyed by provider, exactly so that
narrowing one is a one-line change. They are deliberately CONSERVATIVE: the free tiers they track
are documented in `CLAUDE.md`'s provider table (Alpha Vantage 25/day, Marketaux 100/day) and the
keyless ones are self-imposed politeness limits, not published quotas.
"""
from __future__ import annotations

import os
from datetime import datetime, timedelta, timezone

from .db import Connection, Row

# =================================================================================================
# Daily limits — one named constant per provider, in one block (AD-10).
#
# Override at runtime with EVENTS_BUDGET_<PROVIDER>, where <PROVIDER> is the provider name
# uppercased with '-' replaced by '_': EVENTS_BUDGET_MARKETAUX, EVENTS_BUDGET_SEC_EDGAR,
# EVENTS_BUDGET_GOOGLE_NEWS_RSS, EVENTS_BUDGET_ALPHAVANTAGE, EVENTS_BUDGET_FRED,
# EVENTS_BUDGET_FMP, EVENTS_BUDGET_FEDERAL_RESERVE. An unparseable value falls back to the
# constant rather than stopping the
# service: a malformed budget must not break invariant #1's offline boot.
# =================================================================================================

BUDGET_MARKETAUX = 80  # free tier is 100/day; the remainder is the gateway's live path
BUDGET_SEC_EDGAR = 500  # keyless; SEC's fair-access guidance is 10 req/s, this is a daily politeness cap
BUDGET_GOOGLE_NEWS_RSS = 250  # keyless and undocumented; conservative by choice
BUDGET_ALPHAVANTAGE = 20  # free tier is 25/day and the gateway already spends some of it
BUDGET_FRED = 500  # generous free tier; capped anyway so a loop cannot run away
BUDGET_FMP = 200  # paid plan; the cap protects the bill, not the quota
BUDGET_FEDERAL_RESERVE = 50  # keyless official page; one calendar fetch per ingestion pass
# Phase 2. All three are keyless official sources, so the cap is politeness, not a quota: BLS is at
# most two pages per pass (a window can straddle a year boundary), BEA is one, and company-IR is one
# per covered ticker.
BUDGET_BLS = 40
BUDGET_BEA = 40
BUDGET_COMPANY_IR = 100

DAILY_LIMITS: dict[str, int] = {
    "marketaux": BUDGET_MARKETAUX,
    "sec-edgar": BUDGET_SEC_EDGAR,
    "google-news-rss": BUDGET_GOOGLE_NEWS_RSS,
    "alphavantage": BUDGET_ALPHAVANTAGE,
    "fred": BUDGET_FRED,
    "fmp": BUDGET_FMP,
    "federal-reserve": BUDGET_FEDERAL_RESERVE,
    "bls": BUDGET_BLS,
    "bea": BUDGET_BEA,
    "company-ir": BUDGET_COMPANY_IR,
}

# A provider with no entry above gets this, so an added provider is throttled by default rather
# than unlimited by default.
DEFAULT_DAILY_LIMIT = 50

BUDGET_ENV_PREFIX = "EVENTS_BUDGET_"

# Monitoring thresholds. This ledger covers provider calls reserved by services/events only; the
# gateway and prediction service have independent provider paths and must not be presented as if
# they were counted here.
QUOTA_WARNING_RATIO = 0.80
QUOTA_SCOPE = "events-ingestion"
QUOTA_SCOPE_NOTE = (
    "Counts calls reserved by the events ingestion service only; gateway and prediction provider "
    "calls are outside this ledger."
)

# ---- backoff --------------------------------------------------------------------------------------

BACKOFF_BASE_SECONDS = 60
BACKOFF_MAX_SECONDS = 6 * 60 * 60  # six hours: long enough to outlast a daily rate-limit window
BACKOFF_FACTOR = 2

# HTTP statuses that mean "stop asking for a while" rather than "this request was wrong".
BACKOFF_STATUSES_MIN_SERVER_ERROR = 500
BACKOFF_STATUS_RATE_LIMITED = 429

LAST_ERROR_CAP = 200  # `last_error` is a breadcrumb, not a log

# ---- the degraded vocabulary (one spelling, service-wide) -----------------------------------------

REASON_BUDGET = "budget"
REASON_BACKOFF = "backoff"
REASON_NO_KEY = "no-key"
REASON_DISABLED = "disabled"
REASON_ERROR = "error"
REASON_NO_CIK = "no-cik"
REASON_UNPARSEABLE_TIME = "unparseable-time"
# Phase 2C. The company has no configured investor-relations source. Distinct from `no-key` (which
# means WE are unconfigured) and from `error` (which means the source failed): this one says the
# coverage gap is real and known, which is what the Calendar surfaces instead of a guessed date.
REASON_NO_COVERAGE = "no-coverage"


def degraded(provider: str, reason: str) -> str:
    """The single `"<provider>:<reason>"` spelling. Never build one of these by hand."""
    return f"{provider}:{reason}"


# =================================================================================================
# Clock + limits
# =================================================================================================


def _now() -> datetime:
    return datetime.now(timezone.utc)


def _iso(value: datetime) -> str:
    """Fixed-width ISO 8601 UTC. Fixed width matters: `blocked_until` is compared as a string."""
    return value.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _parse(value: str | None) -> datetime | None:
    if not value:
        return None
    text = value.strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    try:
        parsed = datetime.fromisoformat(text)
    except ValueError:
        return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc)


def utc_date(now: datetime | None = None) -> str:
    """The budget day, `YYYY-MM-DD` UTC. Budgets reset on the UTC day, not the local one."""
    return (now or _now()).astimezone(timezone.utc).strftime("%Y-%m-%d")


def daily_limit(provider: str) -> int:
    """This provider's limit for today: the env override if set and parseable, else the constant."""
    key = BUDGET_ENV_PREFIX + provider.strip().upper().replace("-", "_")
    raw = os.getenv(key, "").strip()
    if raw:
        try:
            return max(0, int(raw))
        except ValueError:
            pass  # a malformed override must not stop the service (invariant #1)
    return DAILY_LIMITS.get(provider, DEFAULT_DAILY_LIMIT)


# =================================================================================================
# provider_budget rows
# =================================================================================================
#
# "limit" is quoted because it is a SQL keyword.


def _row(conn: Connection, provider: str, date: str) -> Row:
    conn.execute(
        'INSERT INTO provider_budget (provider, date, calls, "limit") VALUES (?,?,0,?) '
        'ON CONFLICT (provider, date) DO NOTHING',
        (provider, date, daily_limit(provider)),
    )
    # The limit is refreshed on every read so an env override takes effect without a restart and
    # without a second source of truth.
    conn.execute(
        'UPDATE provider_budget SET "limit" = ? WHERE provider = ? AND date = ?',
        (daily_limit(provider), provider, date),
    )
    return conn.execute(
        "SELECT * FROM provider_budget WHERE provider = ? AND date = ? FOR UPDATE", (provider, date)
    ).fetchone()


def state(conn: Connection, *, now: datetime | None = None) -> list[dict]:
    """Every provider's state today, for `GET /health` (contract §3.11's `providers`).

    Reports providers that have no row yet as well, so /health lists the full cascade rather than
    only the ones that happen to have been called.
    """
    moment = now or _now()
    date = utc_date(moment)
    out: list[dict] = []
    for provider in sorted(DAILY_LIMITS):
        row = _row(conn, provider, date)
        blocked = _parse(row["blocked_until"])
        calls = int(row["calls"])
        limit = int(row["limit"])
        remaining = max(0, limit - calls)
        utilization = min(100.0, round((calls / limit) * 100, 1)) if limit > 0 else 100.0
        is_blocked = bool(blocked and blocked > moment)
        exhausted = remaining == 0
        warning = not exhausted and utilization >= QUOTA_WARNING_RATIO * 100
        status = (
            "blocked" if is_blocked else
            "exhausted" if exhausted else
            "warning" if warning else
            "ok"
        )
        out.append({
            "provider": provider,
            "date": date,
            "calls": calls,
            "limit": limit,
            "remaining": remaining,
            "utilizationPct": utilization,
            "exhausted": exhausted,
            "warning": warning,
            "status": status,
            "blockedUntil": row["blocked_until"],
            "blocked": is_blocked,
            "lastError": row["last_error"],
        })
    conn.commit()
    return out


def quota_summary(rows: list[dict], *, now: datetime | None = None) -> dict:
    """Small operator summary for the automation/health surfaces; no provider or DB call."""
    moment = (now or _now()).astimezone(timezone.utc)
    reset_at = (moment.replace(hour=0, minute=0, second=0, microsecond=0) + timedelta(days=1))
    providers = {
        "warning": [str(row["provider"]) for row in rows if row.get("warning")],
        "exhausted": [str(row["provider"]) for row in rows if row.get("exhausted")],
        "blocked": [str(row["provider"]) for row in rows if row.get("blocked")],
    }
    return {
        "scope": QUOTA_SCOPE,
        "scopeNote": QUOTA_SCOPE_NOTE,
        "date": utc_date(moment),
        "resetAt": _iso(reset_at),
        "providerCount": len(rows),
        "warningProviders": providers["warning"],
        "exhaustedProviders": providers["exhausted"],
        "blockedProviders": providers["blocked"],
        "attentionRequired": any(providers.values()),
    }


def reserve(
    conn: Connection, provider: str, *, now: datetime | None = None
) -> tuple[bool, str | None]:
    """Check-and-reserve one call. Returns `(allowed, degraded_string_or_None)`.

    The `calls` increment and the commit happen HERE, before the caller opens a socket. A provider
    function that calls `requests` without a `True` from this function is a defect.
    """
    moment = now or _now()
    date = utc_date(moment)
    row = _row(conn, provider, date)

    blocked_until = _parse(row["blocked_until"])
    if blocked_until and blocked_until > moment:
        conn.commit()
        return False, degraded(provider, REASON_BACKOFF)

    if row["calls"] >= row["limit"]:
        conn.commit()
        return False, degraded(provider, REASON_BUDGET)

    conn.execute(
        "UPDATE provider_budget SET calls = calls + 1 WHERE provider = ? AND date = ?",
        (provider, date),
    )
    conn.commit()
    return True, None


def record_success(
    conn: Connection, provider: str, *, now: datetime | None = None
) -> None:
    """A good response clears the block and the breadcrumb. The `calls` increment stands."""
    date = utc_date(now or _now())
    _row(conn, provider, date)
    conn.execute(
        "UPDATE provider_budget SET blocked_until = NULL, last_error = NULL "
        "WHERE provider = ? AND date = ?",
        (provider, date),
    )
    conn.commit()


def next_backoff_seconds(remaining_seconds: float) -> int:
    """The next block length, given how much of the CURRENT block is left.

    Interpretation, recorded in the handoff: `provider_budget` has four columns and none of them is
    a consecutive-failure counter, so the escalation is carried by `blocked_until` itself — a
    failure that arrives while the provider is still blocked doubles what is left, capped at
    `BACKOFF_MAX_SECONDS`. A failure that arrives after a block has fully expired starts again at
    `BACKOFF_BASE_SECONDS`: the provider has had a complete quiet period and is entitled to one
    cheap probe. In practice a scheduler pass calls a per-ticker provider once per ticker, so a
    sustained outage escalates 60 → 120 → 240 within the pass, which is the behaviour AD-10 wants.
    """
    if remaining_seconds <= 0:
        return BACKOFF_BASE_SECONDS
    return int(min(BACKOFF_MAX_SECONDS, max(BACKOFF_BASE_SECONDS, remaining_seconds) * BACKOFF_FACTOR))


def record_failure(
    conn: Connection,
    provider: str,
    *,
    status: int | None = None,
    error: str = "",
    now: datetime | None = None,
) -> str | None:
    """Record a failed call. Returns the `blocked_until` it set, or None when it did not block.

    Only HTTP 429 and 5xx set a block. A 401, a 404 or a decode failure is a broken request, not a
    busy server: blocking on those would take a provider offline for six hours because one ticker
    has no coverage.
    """
    moment = now or _now()
    date = utc_date(moment)
    row = _row(conn, provider, date)
    message = (error or "")[:LAST_ERROR_CAP]

    should_block = status is not None and (
        status == BACKOFF_STATUS_RATE_LIMITED or status >= BACKOFF_STATUSES_MIN_SERVER_ERROR
    )
    if not should_block:
        conn.execute(
            "UPDATE provider_budget SET last_error = ? WHERE provider = ? AND date = ?",
            (message, provider, date),
        )
        conn.commit()
        return None

    current = _parse(row["blocked_until"])
    remaining = (current - moment).total_seconds() if current and current > moment else 0.0
    blocked_until = _iso(moment + timedelta(seconds=next_backoff_seconds(remaining)))
    conn.execute(
        "UPDATE provider_budget SET blocked_until = ?, last_error = ? "
        "WHERE provider = ? AND date = ?",
        (blocked_until, message, provider, date),
    )
    conn.commit()
    return blocked_until
