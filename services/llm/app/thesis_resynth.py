"""One bounded, operator-invoked pass over stale-thesis re-synthesis jobs.

The alerts service detects staleness deterministically and owns the durable queue. This process is
the model-owning half: it leases one job at a time, resolves the current thesis through journal,
builds a point-in-time fact bundle from the stored analysis/events services, generates under the
cross-process background lease, persists only ``lastCheck``, and acknowledges the job.

It is intentionally not imported by ``main.py`` and has no timer or HTTP route. The feature is off
unless ``THESIS_RESYNTH_ENABLED=true`` and every invocation processes at most
``THESIS_RESYNTH_BATCH_SIZE`` jobs before exiting.
"""
from __future__ import annotations

import json
import logging
import os
import time
from datetime import datetime, timedelta, timezone
from urllib.parse import quote

import requests

from . import lease, llm, tools
from .thesis import run_thesis_check

log = logging.getLogger("thesis_resynth")

DEFAULT_ALERTS_URL = "http://localhost:8095"
DEFAULT_JOURNAL_URL = "http://localhost:8096"
DEFAULT_BATCH_SIZE = 5
HTTP_TIMEOUT = 20

DISABLED = "disabled"
DRAINED = "drained"
PREEMPTED = "preempted"
ALERTS_UNREACHABLE = "alerts_unreachable"
LEASE_LOST = "lease_lost"


class _TransportError(RuntimeError):
    pass


class _LeaseLost(_TransportError):
    pass


def enabled() -> bool:
    return (os.getenv("THESIS_RESYNTH_ENABLED") or "").strip().lower() in (
        "true", "1", "yes", "on",
    )


def batch_size() -> int:
    raw = (os.getenv("THESIS_RESYNTH_BATCH_SIZE") or "").strip()
    try:
        value = int(raw) if raw else DEFAULT_BATCH_SIZE
    except ValueError:
        return DEFAULT_BATCH_SIZE
    return value if value > 0 else DEFAULT_BATCH_SIZE


def alerts_url() -> str:
    return (os.getenv("ALERTS_URL") or DEFAULT_ALERTS_URL).strip().rstrip("/")


def journal_url() -> str:
    return (os.getenv("JOURNAL_URL") or DEFAULT_JOURNAL_URL).strip().rstrip("/")


def internal_headers() -> dict[str, str]:
    return {
        "X-Internal-Secret": os.getenv("AUTH_SECRET", "dev-insecure-change-me"),
        "Content-Type": "application/json",
    }


def _http(method: str, url: str, *, params=None, json_body=None, headers=None,
          timeout=HTTP_TIMEOUT) -> tuple[int, dict]:
    try:
        response = requests.request(
            method, url, params=params, json=json_body, headers=headers, timeout=timeout,
        )
    except (requests.RequestException, OSError) as exc:
        raise _TransportError(f"{method} {url}: {type(exc).__name__}: {exc}") from exc
    try:
        body = response.json()
    except ValueError:
        body = {}
    return response.status_code, body if isinstance(body, dict) else {}


def _lease_job() -> dict | None:
    status, body = _http(
        "POST", f"{alerts_url()}/_internal/resynth/lease", headers=internal_headers(),
    )
    if status == 204:
        return None
    if status != 200 or not isinstance(body.get("job"), dict):
        raise _TransportError(f"lease queue returned HTTP {status}")
    return body["job"]


def _complete(job: dict, outcome: str, error: str = "") -> None:
    job_id = str(job.get("id") or "")
    status, _body = _http(
        "POST", f"{alerts_url()}/_internal/resynth/{quote(job_id, safe='')}/complete",
        json_body={
            "leaseToken": str(job.get("leaseToken") or ""),
            "outcome": outcome,
            "error": error,
        },
        headers=internal_headers(),
    )
    if status == 409:
        raise _LeaseLost("queue lease expired or was replaced by a newer worker")
    if status != 200:
        raise _TransportError(f"queue completion returned HTTP {status}")


def _get_thesis(job: dict) -> dict:
    thesis_id = quote(str(job.get("thesisId") or ""), safe="")
    status, body = _http(
        "GET", f"{journal_url()}/_internal/thesis-resynth/{thesis_id}",
        params={"userId": str(job.get("userId") or "")}, headers=internal_headers(),
    )
    if status != 200 or not isinstance(body.get("thesis"), dict):
        raise _TransportError(f"journal thesis read returned HTTP {status}")
    return body["thesis"]


def _put_check(job: dict, check: dict) -> None:
    thesis_id = quote(str(job.get("thesisId") or ""), safe="")
    status, _body = _http(
        "PATCH", f"{journal_url()}/_internal/thesis-resynth/{thesis_id}",
        params={"userId": str(job.get("userId") or "")},
        json_body={"lastCheck": check}, headers=internal_headers(),
    )
    if status != 200:
        raise _TransportError(f"journal thesis write returned HTTP {status}")


def _unix(value) -> int:
    if isinstance(value, bool):
        return 0
    if isinstance(value, (int, float)):
        return int(value)
    return 0


def _already_applied(thesis: dict, job: dict) -> bool:
    last = thesis.get("lastCheck")
    return isinstance(last, dict) and _unix(last.get("at")) >= _unix(job.get("enqueuedAt")) > 0


def _cutoff(value: str) -> datetime:
    text = str(value or "").strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    parsed = datetime.fromisoformat(text)
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc)


def _fact_context(job: dict) -> tuple[dict, bool]:
    as_of = str(job.get("asOf") or "")
    ticker = str(job.get("ticker") or "").upper()
    since = (_cutoff(as_of) - timedelta(days=90)).isoformat().replace("+00:00", "Z")
    runtime_ctx = {"asOf": as_of}
    calls = (
        ("get_multi_timeframe_context", {"ticker": ticker}),
        ("get_ticker_events", {"ticker": ticker, "since": since}),
        ("get_macro_context", {}),
    )
    results = {name: tools.execute(name, args, runtime_ctx) for name, args in calls}
    technical = (results["get_multi_timeframe_context"].get("result")
                 if results["get_multi_timeframe_context"].get("ok") else None)
    event_result = (results["get_ticker_events"].get("result")
                    if results["get_ticker_events"].get("ok") else None) or {}
    macro_result = (results["get_macro_context"].get("result")
                    if results["get_macro_context"].get("ok") else None) or {}
    context = {
        "asOf": as_of,
        "trigger": {"reason": job.get("reason"), "severity": job.get("severity")},
        "technical": technical,
        "news": event_result.get("events") or [],
        "macro": macro_result.get("macro") or [],
        "coverage": {
            name: {
                "ok": bool(envelope.get("ok")),
                "error": envelope.get("error"),
                "degraded": envelope.get("degraded") or [],
            }
            for name, envelope in results.items()
        },
    }
    return context, any(envelope.get("ok") for envelope in results.values())


def _journal_check(result: dict) -> dict:
    structured = result.get("structured") or {}
    confidence = structured.get("confidence")
    if isinstance(confidence, bool) or not isinstance(confidence, (int, float)):
        confidence_pct = None
    else:
        confidence_pct = max(0, min(100, int(float(confidence) * 100 + 0.5)))
    check = {
        "at": int(time.time()),
        "verdict": structured.get("verdict"),
        "summary": structured.get("summary"),
        "evidence": structured.get("evidence") or [],
        "watchFor": structured.get("watchFor") or [],
        "modelUsed": result.get("modelUsed"),
    }
    if confidence_pct is not None:
        check["confidence"] = confidence_pct
    return check


def _report(**updates) -> dict:
    report = {
        "enabled": False, "leased": 0, "generated": 0, "completed": 0,
        "retried": 0, "alreadyApplied": 0, "skippedInactive": 0,
        "stopped": None, "failures": [],
    }
    report.update(updates)
    return report


def run_once() -> dict:
    if not enabled():
        return _report(stopped=DISABLED)

    report = _report(enabled=True)
    for _ in range(batch_size()):
        try:
            job = _lease_job()
        except _TransportError as exc:
            report["stopped"] = ALERTS_UNREACHABLE
            report["failures"].append({"jobId": None, "detail": str(exc)})
            break
        if job is None:
            report["stopped"] = DRAINED
            break

        report["leased"] += 1
        job_id = str(job.get("id") or "")
        try:
            thesis = _get_thesis(job)
            if str(thesis.get("status") or "") not in ("active", "open"):
                _complete(job, "completed")
                report["skippedInactive"] += 1
                report["completed"] += 1
                continue
            if _already_applied(thesis, job):
                _complete(job, "completed")
                report["alreadyApplied"] += 1
                report["completed"] += 1
                continue
            claim = str(thesis.get("claim") or thesis.get("text") or "").strip()
            ticker = str(job.get("ticker") or "").upper()
            if not claim or str(thesis.get("ticker") or "").upper() != ticker:
                raise ValueError("thesis claim is empty or ticker does not match the queued job")
            context, has_facts = _fact_context(job)
            if not has_facts:
                raise ValueError("no point-in-time fact source could serve this cutoff")

            with lease.acquire_background() as held:
                held.check()
                result = run_thesis_check(ticker, claim, context, llm.call_model, llm.safe_json)
            report["generated"] += 1

            structured = result.get("structured") or {}
            model_used = str(result.get("modelUsed") or "")
            if structured.get("_stub") or model_used.startswith("stub:") or "fell back to stub" in model_used:
                raise ValueError(f"model did not produce a valid live check ({model_used or 'unknown'})")
            _put_check(job, _journal_check(result))
            _complete(job, "completed")
            report["completed"] += 1
        except _LeaseLost as exc:
            report["stopped"] = LEASE_LOST
            report["failures"].append({"jobId": job_id, "detail": str(exc)})
            break
        except lease.LeaseUnavailable as exc:
            try:
                _complete(job, "queued", str(exc))
            except _TransportError:
                pass
            report["retried"] += 1
            report["stopped"] = PREEMPTED
            break
        except Exception as exc:  # noqa: BLE001 — return the durable job to its queue on any item failure
            try:
                _complete(job, "queued", str(exc))
            except _TransportError as ack_exc:
                report["stopped"] = ALERTS_UNREACHABLE
                report["failures"].append({"jobId": job_id, "detail": str(ack_exc)})
                break
            report["retried"] += 1
            report["failures"].append({"jobId": job_id, "detail": str(exc)})

    if report["stopped"] is None:
        report["stopped"] = DRAINED
    return report


def main() -> int:
    logging.basicConfig(level=os.getenv("LOG_LEVEL", "INFO").upper())
    print(json.dumps(run_once(), indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
