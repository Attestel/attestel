"""Phase 1 — the WORKER-side automation runner: one bounded dispatch pass, then exit.

    python -m app.automation                  # or: make automate-once  (this half of it)
    python -m app.automation --lane enrich --force

WHAT IT DOES, AND WHAT IT REFUSES TO DO
---------------------------------------
Three of the five automation lanes are executed outside `services/events`, so `services/events`
cannot dispatch them:

| lane             | entrypoint reused verbatim                     | model? |
|------------------|------------------------------------------------|--------|
| `enrich`         | `app.enrich_worker.run_once`                   | yes — under the background lease |
| `resynth`        | `app.thesis_resynth.run_once`                  | yes — under the background lease |
| `thesis-monitor` | `alerts` `POST /_internal/monitor/tick`        | **no** — deterministic sweep |

This module adds NO job logic. It leases the lane from the events service (the only durable,
cross-container lock in the platform), calls the existing entrypoint unchanged, and reports what
that entrypoint returned. If it were deleted, every one of those entrypoints would still work
exactly as it does today via its own `make` target — which is the test that it is an operational
layer and not a second implementation.

INVARIANT #4, RESTATED FOR THIS FILE
------------------------------------
There is no timer here: no unbounded loop, no sleep call, no scheduler thread. `dispatch()`
evaluates which lanes are due, runs those, and RETURNS. The repetition an operator wants lives in
the operator's cron, outside this repository — EVENT_CONTRACTS.md §9.22. `tests/test_automation.py`
asserts this against the source text of this file, so the forbidden spellings do not appear here
even as examples.

Two flags must BOTH be true before any lane runs: `AUTOMATION_ENABLED` (master, default false) and
the lane's own long-standing flag (`EVENT_ENRICH_ENABLED`, `THESIS_RESYNTH_ENABLED`,
`THESIS_MONITOR_ENABLED` — all default false). Automation is not a second way to switch a job on.

The lane vocabulary, the interval variables and the lease/complete protocol are all owned by
`services/events/app/automation.py`; this file is a client of that HTTP surface and deliberately
re-declares none of them beyond the three lane names it can execute.
"""
from __future__ import annotations

import json
import logging
import os
from datetime import datetime, timezone

import requests

log = logging.getLogger("automation")

LANE_ENRICH = "enrich"
LANE_RESYNTH = "resynth"
LANE_THESIS_MONITOR = "thesis-monitor"

#: The lanes this runner can execute, and the flag each one honours. The flags are named here only
#: so a disabled lane is never even LEASED — the authoritative gate is the events service's, which
#: refuses the lease as well.
WORKER_LANES: dict[str, str] = {
    LANE_ENRICH: "EVENT_ENRICH_ENABLED",
    LANE_RESYNTH: "THESIS_RESYNTH_ENABLED",
    LANE_THESIS_MONITOR: "THESIS_MONITOR_ENABLED",
}

DEFAULT_EVENTS_URL = "http://localhost:8004"
DEFAULT_ALERTS_URL = "http://localhost:8095"
HTTP_TIMEOUT = 30
TICK_TIMEOUT = 120

STATUS_SUCCESS = "success"
STATUS_DEGRADED = "degraded"
STATUS_FAILURE = "failure"


class _Unreachable(RuntimeError):
    """The events service (the lease holder) could not be reached. Nothing was run."""


# ---- environment (call time, never import time) -------------------------------------------------


def _flag(name: str, default: bool = False) -> bool:
    raw = (os.getenv(name) or "").strip().lower()
    if not raw:
        return default
    return raw in ("1", "true", "yes", "on")


def automation_enabled() -> bool:
    return _flag("AUTOMATION_ENABLED", False)


def selected_lanes() -> list[str]:
    raw = (os.getenv("AUTOMATION_LANES") or "").strip()
    if not raw:
        return list(WORKER_LANES)
    chosen = [part.strip() for part in raw.split(",") if part.strip()]
    return [name for name in chosen if name in WORKER_LANES]


def lane_enabled(lane: str) -> bool:
    flag = WORKER_LANES.get(lane)
    return bool(flag) and automation_enabled() and lane in selected_lanes() and _flag(flag, False)


def events_url() -> str:
    return ((os.getenv("EVENTS_URL") or DEFAULT_EVENTS_URL).strip().rstrip("/")) or DEFAULT_EVENTS_URL


def alerts_url() -> str:
    return ((os.getenv("ALERTS_URL") or DEFAULT_ALERTS_URL).strip().rstrip("/")) or DEFAULT_ALERTS_URL


def internal_headers() -> dict[str, str]:
    """The same internal seam `thesis_resynth.py` already uses to reach the alerts queue."""
    return {
        "X-Internal-Secret": os.getenv("AUTH_SECRET", "dev-insecure-change-me"),
        "Content-Type": "application/json",
    }


def _iso(moment: datetime | None = None) -> str:
    return (moment or datetime.now(timezone.utc)).astimezone(timezone.utc).strftime(
        "%Y-%m-%dT%H:%M:%SZ"
    )


def _http(method: str, url: str, *, json_body=None, timeout=HTTP_TIMEOUT) -> tuple[int, dict]:
    try:
        response = requests.request(
            method, url, json=json_body, headers=internal_headers(), timeout=timeout,
        )
    except (requests.RequestException, OSError) as exc:
        raise _Unreachable(f"{method} {url}: {type(exc).__name__}: {exc}") from exc
    try:
        body = response.json()
    except ValueError:
        body = {}
    return response.status_code, body if isinstance(body, dict) else {}


# ---- lease / complete against the events service -------------------------------------------------


def _lease(lane: str, *, force: bool) -> dict:
    status, body = _http(
        "POST", f"{events_url()}/automation/lease",
        json_body={"lane": lane, "trigger": "worker", "force": bool(force)},
    )
    if status != 200:
        raise _Unreachable(f"lease for {lane} returned HTTP {status}")
    return body


def _complete(run_id: str, token: str, outcome: dict) -> None:
    status, _ = _http(
        "POST", f"{events_url()}/automation/runs/{run_id}/complete",
        json_body={
            "leaseToken": token,
            "status": outcome["status"],
            "recordsRead": outcome.get("recordsRead", 0),
            "recordsWritten": outcome.get("recordsWritten", 0),
            "recordsSkipped": outcome.get("recordsSkipped", 0),
            "queueDepth": outcome.get("queueDepth"),
            "error": outcome.get("error", ""),
            "detail": outcome.get("detail") or {},
        },
    )
    if status == 409:
        # Our lease expired and someone else took the lane. The other run's result stands; ours is
        # discarded rather than overwriting a newer one. This is the takeover rule, not an error.
        log.warning("run %s could not be completed: the lease is no longer ours", run_id)
        return
    if status != 200:
        raise _Unreachable(f"completing {run_id} returned HTTP {status}")


# ---- the three lanes -----------------------------------------------------------------------------


def _enrich_lane() -> dict:
    from . import enrich_worker

    report = enrich_worker.run_once()
    failures = report.get("failures") or []
    stopped = report.get("stopped")
    degraded = bool(failures) or stopped not in (None, "drained")
    return {
        "status": STATUS_DEGRADED if degraded else STATUS_SUCCESS,
        "recordsRead": int(report.get("candidates") or 0),
        "recordsWritten": int(report.get("posted") or 0),
        "recordsSkipped": int(report.get("skipped") or 0) + int(report.get("rejected") or 0),
        "error": f"stopped={stopped}" if degraded else "",
        "detail": report,
    }


def _resynth_lane() -> dict:
    from . import thesis_resynth

    report = thesis_resynth.run_once()
    failures = report.get("failures") or []
    stopped = report.get("stopped")
    degraded = bool(failures) or stopped not in (None, "drained")
    return {
        "status": STATUS_DEGRADED if degraded else STATUS_SUCCESS,
        "recordsRead": int(report.get("leased") or 0),
        "recordsWritten": int(report.get("completed") or 0),
        "recordsSkipped": int(report.get("skippedInactive") or 0)
        + int(report.get("alreadyApplied") or 0),
        "error": f"stopped={stopped}" if degraded else "",
        "detail": report,
    }


def _thesis_monitor_lane() -> dict:
    """One deterministic sweep in `alerts`. No model is reached on this path, by construction:
    this function calls exactly one URL and it is the alerts service's."""
    status, body = _http("POST", f"{alerts_url()}/_internal/monitor/tick", timeout=TICK_TIMEOUT)
    if status != 200:
        raise _Unreachable(f"monitor tick returned HTTP {status}")
    if not body.get("ran"):
        return {
            "status": "skipped",
            "error": f"monitor disabled ({body.get('flag') or 'THESIS_MONITOR_ENABLED'})",
            "detail": body,
        }
    tick = body.get("tick") or {}
    queue = body.get("queue") or {}
    degraded = bool(tick.get("degraded"))
    return {
        "status": STATUS_DEGRADED if degraded else STATUS_SUCCESS,
        "recordsRead": int(tick.get("examined") or 0),
        "recordsWritten": int(tick.get("marked") or 0),
        "recordsSkipped": max(0, int(tick.get("theses") or 0) - int(tick.get("examined") or 0)),
        "queueDepth": int(queue.get("depth") or 0),
        "error": "; ".join(tick.get("degraded") or []),
        "detail": body,
    }


LANE_RUNNERS = {
    LANE_ENRICH: _enrich_lane,
    LANE_RESYNTH: _resynth_lane,
    LANE_THESIS_MONITOR: _thesis_monitor_lane,
}


# ---- the pass ------------------------------------------------------------------------------------


def run_lane(lane: str, *, force: bool = False) -> dict:
    """Lease → run the existing entrypoint → complete. Never raises for a lane reason."""
    if lane not in WORKER_LANES:
        return {"lane": lane, "ran": False, "reason": "unknown-lane"}
    if not lane_enabled(lane):
        return {"lane": lane, "ran": False, "reason": "disabled"}

    try:
        lease = _lease(lane, force=force)
    except _Unreachable as exc:
        return {"lane": lane, "ran": False, "reason": "events-unreachable", "error": str(exc)}
    if not lease.get("leased"):
        return {"lane": lane, "ran": False, "reason": lease.get("reason"),
                "nextEligibleAt": lease.get("nextEligibleAt")}

    run_id, token = lease["runId"], lease["leaseToken"]
    try:
        outcome = LANE_RUNNERS[lane]()
    except Exception as exc:  # noqa: BLE001 — recorded on the run, never propagated
        outcome = {"status": STATUS_FAILURE, "error": f"{type(exc).__name__}: {exc}"}

    try:
        _complete(run_id, token, outcome)
    except _Unreachable as exc:
        # The work happened; only the bookkeeping failed. Say so rather than reporting success —
        # the lane's lease will expire and the run will be reconciled to `failure`, which is the
        # honest state: we cannot prove it finished.
        return {"lane": lane, "ran": True, "runId": run_id, "status": outcome["status"],
                "reason": "completion-unreachable", "error": str(exc)}

    return {"lane": lane, "ran": True, "runId": run_id, "status": outcome["status"],
            "recordsRead": outcome.get("recordsRead", 0),
            "recordsWritten": outcome.get("recordsWritten", 0),
            "recordsSkipped": outcome.get("recordsSkipped", 0),
            "error": outcome.get("error", "")}


def dispatch(*, lanes=None, force: bool = False) -> dict:
    """ONE pass over the worker lanes that are due. Runs them, then RETURNS."""
    if not automation_enabled():
        return {"enabled": False, "dispatchedAt": _iso(), "lanes": []}
    requested = [name for name in (lanes or WORKER_LANES) if name in WORKER_LANES]
    return {
        "enabled": True,
        "dispatchedAt": _iso(),
        "lanes": [run_lane(name, force=force) for name in requested],
    }


def main(argv=None) -> int:
    import argparse

    logging.basicConfig(level=os.getenv("LOG_LEVEL", "INFO").upper())
    parser = argparse.ArgumentParser(description="One bounded worker-side automation pass.")
    parser.add_argument("--lane", action="append", default=None,
                        help="restrict to this lane (repeatable)")
    parser.add_argument("--force", action="store_true",
                        help="ignore next-eligible pacing (still respects the lease and the flags)")
    args = parser.parse_args(argv)

    print(json.dumps(dispatch(lanes=args.lane, force=args.force), indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
