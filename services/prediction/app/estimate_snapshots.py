"""Bounded forward collector for point-in-time earnings-consensus evidence.

This is an operator job, not a timer. Production starts it from the authenticated Settings runner;
local operators can run the module directly. It spends one Alpha Vantage calendar request, then
only requests estimates for configured tickers that are seven or one calendar day from a reported
earnings date and do not already have that stage persisted.
Snapshots are immutable PostgreSQL evidence; the PEAD evaluator never promotes historical provider
rows into verified vintages.
"""
from __future__ import annotations

import csv
import io
import json
import os
import time
from dataclasses import dataclass
from datetime import date, datetime, timedelta, timezone

import requests

from . import db as _db
from .evaluate import DEFAULT_UNIVERSE
from .events import AV_URL, fetch_earnings_av

EXIT_OK = 0
EXIT_REFUSED = 3
PROVIDER = "alpha-vantage"
# Alpha Vantage's free tier permits one request per second. Sleeping after each response is a
# deliberately conservative protocol rule: it avoids burst refusals without adding a tunable gate.
PROVIDER_MIN_INTERVAL_SECONDS = 1.1


def _env(name: str, default: str = "") -> str:
    return os.getenv(name, default).strip()


@dataclass(frozen=True)
class SnapshotConfig:
    universe: list[str]
    api_key: str
    max_calls: int
    actual_lookback_days: int

    @classmethod
    def from_env(cls) -> "SnapshotConfig":
        universe = [
            ticker.strip().upper()
            for ticker in _env("EVENT_UNIVERSE", _env("EVAL_UNIVERSE", DEFAULT_UNIVERSE)).split(",")
            if ticker.strip()
        ]
        cfg = cls(
            universe=universe,
            api_key=_env("ALPHAVANTAGE_API_KEY"),
            max_calls=int(_env("ESTIMATE_SNAPSHOT_MAX_CALLS", "20")),
            actual_lookback_days=int(_env("ESTIMATE_ACTUAL_LOOKBACK_DAYS", "7")),
        )
        if not cfg.universe:
            raise ValueError("EVENT_UNIVERSE must contain at least one ticker")
        if cfg.max_calls < 1:
            raise ValueError("ESTIMATE_SNAPSHOT_MAX_CALLS must be positive")
        if cfg.actual_lookback_days < 1:
            raise ValueError("ESTIMATE_ACTUAL_LOOKBACK_DAYS must be positive")
        return cfg


def _provider_error(data: dict) -> str | None:
    for key in ("Note", "Information", "Error Message"):
        if data.get(key):
            return str(data[key])
    return None


def fetch_calendar(api_key: str, *, timeout: int = 30) -> list[dict[str, str]]:
    if not api_key:
        raise RuntimeError("no ALPHAVANTAGE_API_KEY")
    response = requests.get(
        AV_URL,
        params={"function": "EARNINGS_CALENDAR", "horizon": "3month", "apikey": api_key},
        timeout=timeout,
    )
    if response.status_code >= 400:
        raise RuntimeError(f"alpha vantage calendar HTTP {response.status_code}")
    body = response.text.strip()
    if body.startswith("{"):
        data = response.json()
        raise RuntimeError(f"alpha vantage calendar: {_provider_error(data) or 'unexpected JSON'}")
    rows = [dict(row) for row in csv.DictReader(io.StringIO(body))]
    if not rows:
        raise RuntimeError("alpha vantage calendar returned no rows")
    return rows


def fetch_estimates(ticker: str, api_key: str, *, timeout: int = 30) -> dict:
    if not api_key:
        raise RuntimeError("no ALPHAVANTAGE_API_KEY")
    response = requests.get(
        AV_URL,
        params={"function": "EARNINGS_ESTIMATES", "symbol": ticker.upper(), "apikey": api_key},
        timeout=timeout,
    )
    if response.status_code >= 400:
        raise RuntimeError(f"alpha vantage estimates HTTP {response.status_code}")
    data = response.json()
    if error := _provider_error(data):
        raise RuntimeError(f"alpha vantage estimates: {error}")
    return data


def _value(row: dict, *names: str):
    for name in names:
        if row.get(name) not in (None, "", "None"):
            return row[name]
    return None


def _float(row: dict, *names: str) -> float | None:
    value = _value(row, *names)
    if value is None:
        return None
    try:
        return float(value)
    except (TypeError, ValueError):
        return None


def _int(row: dict, *names: str) -> int | None:
    value = _value(row, *names)
    if value is None:
        return None
    try:
        return int(float(value))
    except (TypeError, ValueError):
        return None


def select_quarter_estimate(payload: dict, fiscal_date: str) -> dict | None:
    """Select only an exact fiscal-period match; horizon guesses would create false provenance."""
    rows: list[dict] = []
    for key in ("estimates", "quarterlyEstimates", "quarterly_estimates"):
        value = payload.get(key)
        if isinstance(value, list):
            rows.extend(row for row in value if isinstance(row, dict))
    for row in rows:
        row_date = str(_value(row, "date", "fiscalDateEnding", "fiscal_date") or "")[:10]
        if row_date != fiscal_date:
            continue
        consensus = _float(
            row, "eps_estimate_average", "epsEstimateAverage", "eps_estimate_avg"
        )
        if consensus is None:
            return None
        return {
            "consensusEPS": consensus,
            "estimateHigh": _float(row, "eps_estimate_high", "epsEstimateHigh"),
            "estimateLow": _float(row, "eps_estimate_low", "epsEstimateLow"),
            "analystCount": _int(
                row, "eps_estimate_analyst_count", "epsEstimateAnalystCount"
            ),
        }
    return None


def snapshot_stage(report_date: date, today: date) -> str | None:
    days = (report_date - today).days
    if days == 1:
        return "t_minus_1"
    if 2 <= days <= 7:
        return "t_minus_7"
    return None


def _calendar_event(row: dict[str, str]) -> tuple[str, str, str] | None:
    ticker = str(_value(row, "symbol", "ticker") or "").strip().upper()
    fiscal = str(_value(row, "fiscalDateEnding", "fiscal_date") or "")[:10]
    report = str(_value(row, "reportDate", "report_date") or "")[:10]
    try:
        date.fromisoformat(fiscal)
        date.fromisoformat(report)
    except ValueError:
        return None
    return ticker, fiscal, report


def _redact(message: str, api_key: str) -> str:
    clean = message.replace(api_key, "<redacted>") if api_key else message
    return clean[:500]


def _has_actual(stored: dict | None, fiscal_date: str) -> bool:
    payload = (stored or {}).get("payload") or {}
    for row in payload.get("quarterlyEarnings") or []:
        if not isinstance(row, dict):
            continue
        if str(row.get("fiscalDateEnding") or "")[:10] != fiscal_date:
            continue
        return row.get("reportedEPS") not in (None, "", "None")
    return False


def collect(
    cfg: SnapshotConfig,
    *,
    now: datetime | None = None,
    calendar_fetch=fetch_calendar,
    estimates_fetch=fetch_estimates,
    earnings_fetch=fetch_earnings_av,
) -> tuple[dict, int]:
    now = now or datetime.now(timezone.utc)
    if now.tzinfo is None:
        raise ValueError("collector time must be timezone-aware")
    now = now.astimezone(timezone.utc)
    if not cfg.api_key:
        return {"state": "refused", "reason": "no ALPHAVANTAGE_API_KEY"}, EXIT_REFUSED
    if not _db.enabled():
        return {"state": "refused", "reason": "PostgreSQL is required"}, EXIT_REFUSED

    calls = 0
    try:
        calls += 1
        calendar = calendar_fetch(cfg.api_key)
    except Exception as exc:  # noqa: BLE001 - bounded job reports provider refusal as data
        return {
            "state": "refused", "reason": _redact(str(exc), cfg.api_key), "apiCalls": calls,
        }, EXIT_REFUSED

    today = now.date()
    universe = set(cfg.universe)
    due: list[tuple[str, str, str, str]] = []
    for row in calendar:
        parsed = _calendar_event(row)
        if parsed is None:
            continue
        ticker, fiscal, report = parsed
        stage = snapshot_stage(date.fromisoformat(report), today)
        if ticker in universe and stage:
            due.append((report, ticker, fiscal, stage))
    due.sort()

    captured: list[dict] = []
    skipped_existing: list[dict] = []
    provider_errors: list[dict] = []
    processed_due = 0
    for report, ticker, fiscal, stage in due:
        if calls >= cfg.max_calls:
            break
        if _db.estimate_snapshot_exists(ticker, fiscal, stage):
            skipped_existing.append({"ticker": ticker, "fiscalDate": fiscal, "stage": stage})
            processed_due += 1
            continue
        try:
            time.sleep(PROVIDER_MIN_INTERVAL_SECONDS)
            calls += 1
            payload = estimates_fetch(ticker, cfg.api_key)
            selected = select_quarter_estimate(payload, fiscal)
            if selected is None:
                provider_errors.append({
                    "ticker": ticker, "stage": stage,
                    "error": f"no exact estimate row for fiscal period {fiscal}",
                })
                processed_due += 1
                continue
            inserted = _db.save_estimate_snapshot(
                ticker, fiscal, report, stage, PROVIDER, payload,
                consensus_eps=selected["consensusEPS"],
                estimate_high=selected["estimateHigh"],
                estimate_low=selected["estimateLow"],
                analyst_count=selected["analystCount"],
                captured_at=now.isoformat(),
            )
            if inserted:
                captured.append({"ticker": ticker, "fiscalDate": fiscal, "stage": stage})
            processed_due += 1
        except Exception as exc:  # noqa: BLE001 - one provider failure must not erase prior writes
            processed_due += 1
            provider_errors.append({
                "ticker": ticker, "stage": stage, "error": _redact(str(exc), cfg.api_key),
            })

    # Actuals are refreshed only for events for which we already hold a pre-release snapshot. A
    # historical EARNINGS row fetched today is not vintage proof; it merely completes the observed
    # side of a forward event whose estimate was captured before the report.
    actuals_refreshed: list[str] = []
    actual_candidates: dict[str, dict] = {}
    oldest = today - timedelta(days=cfg.actual_lookback_days)
    for ticker in cfg.universe:
        for snapshot in _db.list_estimate_snapshots(ticker):
            report_date = date.fromisoformat(snapshot["expectedReportDate"])
            if oldest <= report_date <= today:
                current = actual_candidates.get(ticker)
                if current is None or snapshot["expectedReportDate"] > current["expectedReportDate"]:
                    actual_candidates[ticker] = snapshot
    actuals_due: list[tuple[str, dict]] = []
    for ticker, snapshot in sorted(actual_candidates.items()):
        stored = _db.load_earnings_payload(ticker)
        if not _has_actual(stored, snapshot["fiscalDate"]):
            actuals_due.append((ticker, snapshot))

    processed_actuals = 0
    for ticker, snapshot in actuals_due:
        if calls >= cfg.max_calls:
            break
        try:
            time.sleep(PROVIDER_MIN_INTERVAL_SECONDS)
            calls += 1
            payload = earnings_fetch(ticker, cfg.api_key)
            _db.save_earnings_payload(ticker, PROVIDER, payload, vintage_status="unverified")
            actuals_refreshed.append(ticker)
        except Exception as exc:  # noqa: BLE001
            provider_errors.append({
                "ticker": ticker, "stage": "actual", "error": _redact(str(exc), cfg.api_key),
            })
        finally:
            processed_actuals += 1

    quota_exhausted = calls >= cfg.max_calls and (
        processed_due < len(due) or processed_actuals < len(actuals_due)
    )
    return {
        "state": "done",
        "capturedAt": now.isoformat(),
        "apiCalls": calls,
        "maxCalls": cfg.max_calls,
        "due": len(due),
        "captured": captured,
        "skippedExisting": skipped_existing,
        "actualsRefreshed": actuals_refreshed,
        "providerErrors": provider_errors,
        "quotaExhausted": quota_exhausted,
    }, EXIT_OK


def main() -> int:
    try:
        cfg = SnapshotConfig.from_env()
        result, code = collect(cfg)
    except (TypeError, ValueError) as exc:
        result, code = {"state": "refused", "reason": str(exc)}, EXIT_REFUSED
    print(json.dumps(result, indent=2, sort_keys=True))
    return code


if __name__ == "__main__":
    raise SystemExit(main())
