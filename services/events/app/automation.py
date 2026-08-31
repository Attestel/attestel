"""Phase 1 — the bounded, opt-in automation dispatcher and its durable run ledger.

WHAT THIS IS
------------
The repository already owns every job it needs, and every one of them is a **one-shot entrypoint**
that drains a bounded batch and exits:

| lane            | entrypoint                                        | owner service |
|-----------------|---------------------------------------------------|---------------|
| `ingest`        | `app.ingest.run_ingest`                           | events        |
| `reactions`     | `app.reactions.capture_once`                      | events        |
| `scout-intake`  | `app.scout.run_intake`                            | events        |
| `scout`         | `app.scout.run_scout`                             | events        |
| `opportunity-radar` | `app.opportunities.run_opportunity_radar`      | events        |
| `enrich`        | `services/llm` `app.enrich_worker.run_once`       | llm           |
| `resynth`       | `services/llm` `app.thesis_resynth.run_once`      | llm           |
| `thesis-monitor`| `alerts` `ThesisMonitor.Tick`                     | alerts        |

What was missing was not a job. It was the operational layer around them: a way to say "run only
the lanes that are actually due", a lock that stops two dispatchers from running the same lane at
once, and a durable record of what each run did so an operator can see freshness and failure
without reading logs. That is this module, plus its lane rows in PostgreSQL.

WHAT THIS DELIBERATELY IS NOT — AND WHY (CLAUDE.md invariant #4, EVENT_CONTRACTS.md §9.22)
------------------------------------------------------------------------------------------
**There is no repeating clock here.** No unbounded loop, no delay call, no scheduler thread, no
startup interval — and `tests/test_automation.py` asserts that on this file's SOURCE TEXT, prose
included. `dispatch()` evaluates which lanes are due, runs those, and RETURNS.

That is not a technicality, it is the whole design constraint. Invariant #4 forbids a model call
caused by a scheduler tick "without qualification", and two of the eight lanes call the model. A
process that woke itself up and drained the enrichment queue would violate it no matter how bounded
the batch was. So the *cadence* is expressed as data — a per-lane minimum interval, recorded as
`next_eligible_at`. The separate `app.production_scheduler` may consult those records only for this
module's model-free, events-owned lanes. Worker-owned lanes remain explicit operator commands.

`next_eligible_at` is therefore a RECORD, never an alarm. It is consulted only during an explicit
dispatch pass or the model-free production clock's events-owned pass.

THE THREE GATES, ALL DEFAULT-OFF
--------------------------------
1. `AUTOMATION_ENABLED` — the master door. Default **false**. With it false, `dispatch()` runs
   nothing, leases nothing and makes no outbound call of any kind.
2. The lane's OWN pre-existing flag — `INGEST_ENABLED`, `EVENT_ENRICH_ENABLED`,
   `THESIS_RESYNTH_ENABLED`, `THESIS_MONITOR_ENABLED`, `REACTION_CAPTURE_ENABLED`,
   `SCOUT_INGEST_ENABLED`, `SCOUT_ENABLED`. Automation does
   not become a second way to switch a job on: a lane whose own flag is false stays off even with
   the master flag true. This is what keeps `make enrich-once`'s contract and this dispatcher's
   contract identical.
3. `AUTOMATION_LANES` — an optional allowlist for narrowing a run to specific lanes.

LOCKING, AND WHY IT IS A DATABASE ROW
-------------------------------------
`services/llm` and `services/events` are separate images. A `flock` cannot span them and an
in-memory flag cannot survive a restart, so the lane lease is a row with an opaque token and an
expiry (the same shape `alerts` already uses for the re-synthesis queue). Acquisition is a single
conditional `UPDATE ... WHERE lease is free`, which PostgreSQL serialises on the row: two
dispatchers racing for one lane produce exactly one winner. Completion requires the exact token, so
a worker whose lease expired cannot overwrite the result of the dispatcher that took over.

A lease that is abandoned — killed process, container restart — simply expires, and the next
acquisition reconciles the orphaned `running` row to `failure` with a stated reason rather than
leaving it running forever.
"""
from __future__ import annotations

import json
import os
import secrets
from datetime import datetime, timedelta, timezone

from fastapi import APIRouter, Body, Header, HTTPException, Query

from .db import Connection, connect

RUN_ID_PREFIX = "aut_"
LEASE_TOKEN_BYTES = 16

# ---- lanes ---------------------------------------------------------------------------------------

LANE_INGEST = "ingest"
LANE_REACTIONS = "reactions"
LANE_SCOUT_INTAKE = "scout-intake"
LANE_SCOUT = "scout"
LANE_OPPORTUNITY_RADAR = "opportunity-radar"
LANE_ENRICH = "enrich"
LANE_RESYNTH = "resynth"
LANE_THESIS_MONITOR = "thesis-monitor"


class Lane:
    """One automation lane: who owns it, which flag switches it on, and how often it may run."""

    __slots__ = ("name", "owner", "runner", "flag_env", "interval_env", "default_interval",
                 "summary")

    def __init__(self, name, owner, runner, flag_env, interval_env, default_interval, summary):
        self.name = name
        #: the service whose code actually performs the work
        self.owner = owner
        #: which dispatcher drives it — "events" (in-process) or "worker" (services/llm's runner)
        self.runner = runner
        self.flag_env = flag_env
        self.interval_env = interval_env
        self.default_interval = default_interval
        self.summary = summary


# The closed lane vocabulary.
#
# `owner` names the service whose code performs the work. `runner` names the dispatcher that drives
# it: "events" lanes run IN THIS PROCESS against the local database; "worker" lanes are driven by
# `services/llm`'s runner, which leases them over the HTTP surface at the bottom of this file. A
# dispatcher only ever runs its own lanes, so nothing here can turn into a general cross-service
# remote-execution surface.
LANES: dict[str, Lane] = {
    LANE_INGEST: Lane(
        LANE_INGEST, "events", "events", "INGEST_ENABLED",
        "AUTOMATION_INTERVAL_INGEST", 3600,
        "One bounded provider ingestion pass (documents, scheduled events, macro releases).",
    ),
    LANE_REACTIONS: Lane(
        LANE_REACTIONS, "events", "events", "REACTION_CAPTURE_ENABLED",
        "AUTOMATION_INTERVAL_REACTIONS", 21600,
        "One bounded post-event reaction capture pass over stored bars.",
    ),
    LANE_SCOUT_INTAKE: Lane(
        LANE_SCOUT_INTAKE, "events", "events", "SCOUT_INGEST_ENABLED",
        "AUTOMATION_INTERVAL_SCOUT_INTAKE", 14400,
        "One bounded, rotating discovery-universe ingestion batch from keyless sources.",
    ),
    LANE_SCOUT: Lane(
        LANE_SCOUT, "events", "events", "SCOUT_ENABLED",
        "AUTOMATION_INTERVAL_SCOUT", 14400,
        "One deterministic company-level research-lead materialization pass.",
    ),
    LANE_OPPORTUNITY_RADAR: Lane(
        LANE_OPPORTUNITY_RADAR, "events", "events", "OPPORTUNITY_RADAR_ENABLED",
        "AUTOMATION_INTERVAL_OPPORTUNITY_RADAR", 14400,
        "One completed-daily-bar early-setup scan; deterministic and model-free.",
    ),
    LANE_ENRICH: Lane(
        LANE_ENRICH, "llm", "worker", "EVENT_ENRICH_ENABLED",
        "AUTOMATION_INTERVAL_ENRICH", 1800,
        "One bounded enrichment pass over the raw-event queue, under the background model lease.",
    ),
    LANE_RESYNTH: Lane(
        LANE_RESYNTH, "llm", "worker", "THESIS_RESYNTH_ENABLED",
        "AUTOMATION_INTERVAL_RESYNTH", 3600,
        "One bounded stale-thesis re-synthesis pass, under the background model lease.",
    ),
    LANE_THESIS_MONITOR: Lane(
        LANE_THESIS_MONITOR, "alerts", "worker", "THESIS_MONITOR_ENABLED",
        "AUTOMATION_INTERVAL_THESIS_MONITOR", 900,
        "One deterministic, model-free thesis-monitoring sweep against the stored bar tip. The "
        "worker runner drives it over HTTP; no model is involved at any point in this lane.",
    ),
}

STATUS_RUNNING = "running"
STATUS_SUCCESS = "success"
STATUS_DEGRADED = "degraded"
STATUS_FAILURE = "failure"
STATUS_SKIPPED = "skipped"
TERMINAL_STATUSES = (STATUS_SUCCESS, STATUS_DEGRADED, STATUS_FAILURE, STATUS_SKIPPED)

REASON_DISABLED = "disabled"
REASON_NOT_DUE = "not-due"
REASON_LOCKED = "locked"
REASON_UNKNOWN_LANE = "unknown-lane"

DEFAULT_LEASE_SECONDS = 900
MAX_LEASE_SECONDS = 6 * 3600
MAX_RUN_LIMIT = 200
LAST_ERROR_CAP = 500

MASTER_FLAG_ENV = "AUTOMATION_ENABLED"
LANE_ALLOWLIST_ENV = "AUTOMATION_LANES"
LEASE_SECONDS_ENV = "AUTOMATION_LEASE_SECONDS"
INTERNAL_SECRET_ENV = "AUTH_SECRET"
INTERNAL_SECRET_DEFAULT = "dev-insecure-change-me"

# The all-in-one production image can run a dedicated clock for the events-owned, model-free
# lanes. It is a separate process (app.production_scheduler), off by default, and structurally
# unable to dispatch the worker-owned model lanes.
PRODUCTION_SCHEDULER_FLAG_ENV = "PRODUCTION_SCHEDULER_ENABLED"
PRODUCTION_SCHEDULER_POLL_ENV = "PRODUCTION_SCHEDULER_POLL_SECONDS"
DEFAULT_PRODUCTION_SCHEDULER_POLL_SECONDS = 60
MIN_PRODUCTION_SCHEDULER_POLL_SECONDS = 10
MAX_PRODUCTION_SCHEDULER_POLL_SECONDS = 3600


# ---- environment (read at call time, never at import) --------------------------------------------


def _flag(name: str, default: bool = False) -> bool:
    raw = os.getenv(name, "").strip().lower()
    if not raw:
        return default
    return raw in ("1", "true", "yes", "on")


def _positive_int(name: str, default: int) -> int:
    try:
        value = int(os.getenv(name, "").strip() or default)
    except ValueError:
        return default
    return value if value > 0 else default


def automation_enabled() -> bool:
    """The master door. Default false — an unconfigured deployment automates nothing."""
    return _flag(MASTER_FLAG_ENV, False)


def production_scheduler_enabled() -> bool:
    """Whether the separate production clock process should stay running. Default false."""
    return _flag(PRODUCTION_SCHEDULER_FLAG_ENV, False)


def production_scheduler_poll_seconds() -> int:
    """How often the clock checks due state; lane intervals still govern actual executions."""
    value = _positive_int(
        PRODUCTION_SCHEDULER_POLL_ENV, DEFAULT_PRODUCTION_SCHEDULER_POLL_SECONDS
    )
    return max(MIN_PRODUCTION_SCHEDULER_POLL_SECONDS,
               min(value, MAX_PRODUCTION_SCHEDULER_POLL_SECONDS))


def selected_lanes() -> list[str]:
    """`AUTOMATION_LANES`, or every known lane when it is unset. Unknown names are dropped."""
    raw = os.getenv(LANE_ALLOWLIST_ENV, "").strip()
    if not raw:
        return list(LANES)
    chosen = [part.strip() for part in raw.split(",") if part.strip()]
    return [name for name in chosen if name in LANES]


def lane_flag_enabled(lane: str) -> bool:
    """The lane's own pre-existing flag. Automation is never a second way to switch a job on."""
    spec = LANES.get(lane)
    return bool(spec) and _flag(spec.flag_env, False)


def lane_enabled(lane: str) -> bool:
    return automation_enabled() and lane in selected_lanes() and lane_flag_enabled(lane)


def interval_seconds(lane: str) -> int:
    spec = LANES.get(lane)
    if spec is None:
        return 0
    return _positive_int(spec.interval_env, spec.default_interval)


def lease_seconds() -> int:
    return min(_positive_int(LEASE_SECONDS_ENV, DEFAULT_LEASE_SECONDS), MAX_LEASE_SECONDS)


def internal_secret() -> str:
    return os.getenv(INTERNAL_SECRET_ENV, "").strip() or INTERNAL_SECRET_DEFAULT


# ---- helpers --------------------------------------------------------------------------------------


def _iso(moment: datetime) -> str:
    return moment.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _now(now: datetime | None = None) -> datetime:
    return (now or datetime.now(timezone.utc)).astimezone(timezone.utc)


def new_run_id() -> str:
    return RUN_ID_PREFIX + secrets.token_hex(8)


def new_lease_token() -> str:
    return secrets.token_hex(LEASE_TOKEN_BYTES)


def _ensure_lane_row(conn: Connection, lane: str, stamp: str) -> None:
    conn.execute(
        "INSERT INTO automation_lanes (lane, updated_at) VALUES (?,?) "
        "ON CONFLICT (lane) DO NOTHING",
        (lane, stamp),
    )


def _reconcile_abandoned(conn: Connection, lane: str, stamp: str) -> None:
    """Close out a run whose lease expired without a completion.

    Restart safety lives here. A killed dispatcher leaves an `automation_runs` row at `running`
    forever, which would read as "this lane is still working" on the status surface. The next
    acquisition converts it to a `failure` with a stated reason — never to a success, and never
    silently deleted.
    """
    conn.execute(
        "UPDATE automation_runs SET status = ?, completed_at = ?, "
        "last_error = COALESCE(last_error, ?) "
        "WHERE lane = ? AND status = ?",
        (STATUS_FAILURE, stamp, "abandoned: lease expired without completion", lane,
         STATUS_RUNNING),
    )


# ---- lease / complete -----------------------------------------------------------------------------


def acquire(
    conn: Connection,
    lane: str,
    *,
    now: datetime | None = None,
    trigger: str = "operator",
    force: bool = False,
    lease_for: int | None = None,
) -> dict:
    """Take the lane lease if it is free AND the lane is due. One conditional UPDATE, so it races
    correctly: PostgreSQL serialises the two writers on the row and exactly one sees a free lease.

    Returns `{"leased": True, "runId", "leaseToken", "lane", "leaseExpiresAt"}` or
    `{"leased": False, "reason": ...}`. It never raises for a "not now" answer — a dispatcher asks
    every lane and expects most of them to decline.

    `force=True` ignores `next_eligible_at` (the manual-run path: `make automate-lane LANE=ingest`).
    It does NOT ignore the lease — a forced run still may not collide with a running one.
    """
    if lane not in LANES:
        return {"leased": False, "reason": REASON_UNKNOWN_LANE, "lane": lane}

    moment = _now(now)
    stamp = _iso(moment)
    ttl = min(max(1, int(lease_for or lease_seconds())), MAX_LEASE_SECONDS)
    expires = _iso(moment + timedelta(seconds=ttl))
    run_id = new_run_id()
    token = new_lease_token()

    _ensure_lane_row(conn, lane, stamp)
    conn.commit()

    updated = conn.execute(
        "UPDATE automation_lanes SET lease_token = ?, lease_run_id = ?, lease_expires_at = ?, "
        "last_started_at = ?, last_status = ?, updated_at = ? "
        "WHERE lane = ? "
        "  AND (lease_token IS NULL OR lease_expires_at IS NULL OR lease_expires_at <= ?) "
        "  AND (? OR next_eligible_at IS NULL OR next_eligible_at <= ?) "
        "RETURNING lane",
        (token, run_id, expires, stamp, STATUS_RUNNING, stamp, lane, stamp,
         bool(force), stamp),
    ).fetchone()

    if updated is None:
        conn.rollback()
        row = conn.execute(
            "SELECT lease_expires_at, next_eligible_at FROM automation_lanes WHERE lane = ?",
            (lane,),
        ).fetchone()
        conn.commit()
        held = bool(row and row["lease_expires_at"] and row["lease_expires_at"] > stamp)
        return {
            "leased": False,
            "lane": lane,
            "reason": REASON_LOCKED if held else REASON_NOT_DUE,
            "nextEligibleAt": row["next_eligible_at"] if row else None,
            "leaseExpiresAt": row["lease_expires_at"] if row else None,
        }

    _reconcile_abandoned(conn, lane, stamp)
    conn.execute(
        "INSERT INTO automation_runs (id, lane, trigger, started_at, status) VALUES (?,?,?,?,?)",
        (run_id, lane, str(trigger or "operator")[:40], stamp, STATUS_RUNNING),
    )
    conn.commit()
    return {
        "leased": True,
        "lane": lane,
        "runId": run_id,
        "leaseToken": token,
        "leaseExpiresAt": expires,
        "startedAt": stamp,
    }


def complete(
    conn: Connection,
    run_id: str,
    *,
    lease_token: str,
    status: str,
    records_read: int = 0,
    records_written: int = 0,
    records_skipped: int = 0,
    queue_depth: int | None = None,
    error: str = "",
    detail: dict | None = None,
    now: datetime | None = None,
) -> bool:
    """Close a leased run. Returns False when the token is not the lane's CURRENT lease token.

    Idempotence and takeover safety are the same rule: the lane row holds exactly one live token,
    so a completion from a worker whose lease already expired and was re-taken cannot land, and a
    second completion of the same run cannot land either (the first cleared the token).
    """
    if status not in TERMINAL_STATUSES:
        raise ValueError(f"status must be one of {TERMINAL_STATUSES}")

    moment = _now(now)
    stamp = _iso(moment)
    row = conn.execute(
        "SELECT lane, lease_token, lease_run_id FROM automation_lanes WHERE lease_run_id = ?",
        (run_id,),
    ).fetchone()
    if row is None or not row["lease_token"] or row["lease_token"] != lease_token:
        conn.rollback()
        return False

    lane = row["lane"]
    trimmed = (error or "")[:LAST_ERROR_CAP] or None
    conn.execute(
        "UPDATE automation_runs SET completed_at = ?, status = ?, records_read = ?, "
        "records_written = ?, records_skipped = ?, queue_depth = ?, last_error = ?, detail = ? "
        "WHERE id = ?",
        (stamp, status, max(0, int(records_read)), max(0, int(records_written)),
         max(0, int(records_skipped)),
         None if queue_depth is None else int(queue_depth), trimmed,
         json.dumps(detail or {}, sort_keys=True, separators=(",", ":")), run_id),
    )

    # THREE OUTCOMES, THREE MEANINGS — and they are deliberately not collapsed into two.
    #
    # `last_success_at` moves only on a CLEAN success, because that field is the freshness signal an
    # operator reads as "this lane is fully working". A pass in which every provider skipped for a
    # missing key genuinely ran, but it is not evidence that ingestion works; calling it a success
    # would make an entirely idle lane look healthy forever.
    #
    # `last_failure_at` and the failure counter move only on a real `failure`, so a degraded pass
    # does not accumulate toward a false alarm either. `last_finished_at` moves on all of them and
    # is the "it ran at all" signal.
    clean = status == STATUS_SUCCESS
    failed = status == STATUS_FAILURE
    next_eligible = _iso(moment + timedelta(seconds=interval_seconds(lane)))
    conn.execute(
        "UPDATE automation_lanes SET lease_token = NULL, lease_run_id = NULL, "
        "lease_expires_at = NULL, last_finished_at = ?, last_status = ?, last_error = ?, "
        "last_success_at = CASE WHEN ? THEN ? ELSE last_success_at END, "
        "last_failure_at = CASE WHEN ? THEN ? ELSE last_failure_at END, "
        "consecutive_failures = CASE WHEN ? THEN consecutive_failures + 1 ELSE 0 END, "
        "next_eligible_at = ?, updated_at = ? WHERE lane = ?",
        (stamp, status, trimmed, clean, stamp, failed, stamp, failed,
         next_eligible, stamp, lane),
    )
    conn.commit()
    return True


# ---- status ----------------------------------------------------------------------------------------


def lane_status(conn: Connection, *, now: datetime | None = None) -> list[dict]:
    """Operator-safe lane state. Carries NO secret, NO key, NO URL and no environment VALUE — only
    the NAME of the variable that switches each lane, and booleans derived from it."""
    moment = _now(now)
    stamp = _iso(moment)
    rows = {
        r["lane"]: r
        for r in conn.execute("SELECT * FROM automation_lanes").fetchall()
    }
    allow = set(selected_lanes())
    master = automation_enabled()

    out: list[dict] = []
    for name in sorted(LANES):
        spec = LANES[name]
        row = rows.get(name)
        held = bool(row and row["lease_expires_at"] and row["lease_expires_at"] > stamp)
        last_success = row["last_success_at"] if row else None
        age = None
        if last_success:
            try:
                age = int(
                    (moment - datetime.strptime(last_success, "%Y-%m-%dT%H:%M:%SZ").replace(
                        tzinfo=timezone.utc)).total_seconds()
                )
            except ValueError:
                age = None
        out.append({
            "lane": name,
            "owner": spec.owner,
            "runner": spec.runner,
            "summary": spec.summary,
            "enabled": master and name in allow and lane_flag_enabled(name),
            "automationEnabled": master,
            "laneFlagEnv": spec.flag_env,
            "laneFlagEnabled": lane_flag_enabled(name),
            "inAllowlist": name in allow,
            "intervalSeconds": interval_seconds(name),
            "running": held,
            "leaseExpiresAt": row["lease_expires_at"] if held else None,
            "nextEligibleAt": row["next_eligible_at"] if row else None,
            "lastStartedAt": row["last_started_at"] if row else None,
            "lastFinishedAt": row["last_finished_at"] if row else None,
            "lastStatus": row["last_status"] if row else None,
            "lastSuccessAt": last_success,
            "lastFailureAt": row["last_failure_at"] if row else None,
            "lastError": row["last_error"] if row else None,
            "consecutiveFailures": int(row["consecutive_failures"]) if row else 0,
            "secondsSinceLastSuccess": age,
        })
    return out


def _run_json(row) -> dict:
    detail = row["detail"]
    if isinstance(detail, str):
        try:
            detail = json.loads(detail or "{}")
        except ValueError:
            detail = {}
    return {
        "id": row["id"],
        "lane": row["lane"],
        "trigger": row["trigger"],
        "startedAt": row["started_at"],
        "completedAt": row["completed_at"],
        "status": row["status"],
        "recordsRead": row["records_read"],
        "recordsWritten": row["records_written"],
        "recordsSkipped": row["records_skipped"],
        "queueDepth": row["queue_depth"],
        "lastError": row["last_error"],
        "detail": detail if isinstance(detail, dict) else {},
    }


def recent_runs(conn: Connection, *, lane: str | None = None, limit: int = 20) -> list[dict]:
    limit = max(1, min(int(limit), MAX_RUN_LIMIT))
    if lane:
        rows = conn.execute(
            "SELECT * FROM automation_runs WHERE lane = ? "
            "ORDER BY started_at DESC, sequence DESC LIMIT ?", (lane, limit),
        ).fetchall()
    else:
        rows = conn.execute(
            "SELECT * FROM automation_runs ORDER BY started_at DESC, sequence DESC LIMIT ?",
            (limit,),
        ).fetchall()
    return [_run_json(r) for r in rows]


# ---- the dispatcher (events-owned lanes only) --------------------------------------------------


EVENTS_OWNED = tuple(name for name, spec in LANES.items() if spec.runner == "events")


def _run_ingest_lane(conn: Connection, *, now: datetime) -> dict:
    """Reuse `ingest.run_ingest` verbatim. This lane adds no fetching logic of its own."""
    from . import ingest as ingest_module

    report = ingest_module.run_ingest(conn, now=now)
    degraded = report.get("degraded") or []
    return {
        "status": STATUS_DEGRADED if degraded else STATUS_SUCCESS,
        "recordsRead": int(report.get("fetched") or 0),
        "recordsWritten": int(report.get("inserted") or 0),
        "recordsSkipped": int(report.get("deduped") or 0),
        "error": "; ".join(degraded)[:LAST_ERROR_CAP] if degraded else "",
        "detail": report,
    }


def _run_reactions_lane(conn: Connection, *, now: datetime) -> dict:
    """Reuse `reactions.capture_once` verbatim (Phase 4)."""
    from . import reactions as reactions_module

    report = reactions_module.capture_once(conn, now=now)
    degraded = report.get("degraded") or []
    return {
        "status": STATUS_DEGRADED if degraded else STATUS_SUCCESS,
        "recordsRead": int(report.get("examined") or 0),
        "recordsWritten": int(report.get("resolved") or 0),
        "recordsSkipped": int(report.get("pending") or 0),
        "queueDepth": int(report.get("outstanding") or 0),
        "error": "; ".join(degraded)[:LAST_ERROR_CAP] if degraded else "",
        "detail": report,
    }


def _run_scout_intake_lane(conn: Connection, *, now: datetime) -> dict:
    """Rotate a bounded universe batch through Scout's closed provider allowlist."""
    from . import scout as scout_module

    report = scout_module.run_intake(conn, now=now)
    degraded = report.get("degraded") or []
    return {
        "status": STATUS_DEGRADED if degraded else STATUS_SUCCESS,
        "recordsRead": int(report.get("fetched") or 0),
        "recordsWritten": int(report.get("inserted") or 0),
        "recordsSkipped": int(report.get("deduped") or 0),
        "error": "; ".join(degraded)[:LAST_ERROR_CAP] if degraded else "",
        "detail": report,
    }


def _run_scout_lane(conn: Connection, *, now: datetime) -> dict:
    """Materialize one stored Scout snapshot; no model or provider route is reachable."""
    from . import scout as scout_module

    report = scout_module.run_scout(conn, now=now)
    degraded = report.get("degraded") or []
    return {
        "status": STATUS_DEGRADED if degraded else STATUS_SUCCESS,
        "recordsRead": int(report.get("coverage", {}).get("canonicalEventRows") or 0)
                       + int(report.get("coverage", {}).get("scheduledCatalystRows") or 0),
        "recordsWritten": int(report.get("candidateCount") or 0),
        "recordsSkipped": 0,
        "error": "; ".join(degraded)[:LAST_ERROR_CAP] if degraded else "",
        "detail": report,
    }


def _run_opportunity_radar_lane(conn: Connection, *, now: datetime) -> dict:
    """Materialize one completed-bar research snapshot; no model or prediction route exists."""
    from . import opportunities as opportunities_module

    report = opportunities_module.run_opportunity_radar(conn, now=now)
    degraded = report.get("degraded") or []
    coverage = report.get("coverage") or {}
    coverage_state = str(coverage.get("state") or "insufficient")
    coverage_marker = f"coverage:{coverage_state}" if coverage_state != "ok" else ""
    return {
        "status": STATUS_DEGRADED if degraded or coverage_marker else STATUS_SUCCESS,
        "recordsRead": int(coverage.get("marketDataCovered") or 0),
        "recordsWritten": int(report.get("persistedCandidateCount") or 0),
        "recordsSkipped": int(coverage.get("universeSize") or 0) if report.get("skipped") else 0,
        "error": "; ".join([*degraded, *([coverage_marker] if coverage_marker else [])])[
            :LAST_ERROR_CAP
        ],
        "detail": report,
    }


LANE_RUNNERS = {
    LANE_INGEST: _run_ingest_lane,
    LANE_REACTIONS: _run_reactions_lane,
    LANE_SCOUT_INTAKE: _run_scout_intake_lane,
    LANE_SCOUT: _run_scout_lane,
    LANE_OPPORTUNITY_RADAR: _run_opportunity_radar_lane,
}


def run_lane(
    conn: Connection,
    lane: str,
    *,
    now: datetime | None = None,
    force: bool = False,
    trigger: str = "dispatcher",
) -> dict:
    """Lease → run the existing one-shot entrypoint → complete. Never raises for a lane reason.

    A lane that is not due, is locked, or is disabled produces a `ran: False` report and no work.
    A lane whose entrypoint raises is recorded as a `failure` WITH the exception text, and the
    lease is released so the next dispatcher invocation can retry it — a crashed run must not wedge
    the lane until its lease expires.
    """
    moment = _now(now)
    if lane not in LANES:
        return {"lane": lane, "ran": False, "reason": REASON_UNKNOWN_LANE}
    if LANES[lane].runner != "events":
        return {"lane": lane, "ran": False, "reason": "not-owned-here"}
    if not lane_enabled(lane):
        return {"lane": lane, "ran": False, "reason": REASON_DISABLED}

    lease = acquire(conn, lane, now=moment, trigger=trigger, force=force)
    if not lease.get("leased"):
        return {"lane": lane, "ran": False, "reason": lease.get("reason"),
                "nextEligibleAt": lease.get("nextEligibleAt")}

    run_id, token = lease["runId"], lease["leaseToken"]
    runner = LANE_RUNNERS.get(lane)
    try:
        outcome = runner(conn, now=moment)
    except Exception as exc:  # noqa: BLE001 — a lane failure is recorded, never propagated
        try:
            conn.rollback()
        except Exception:  # noqa: BLE001 — the rollback is best-effort bookkeeping
            pass
        complete(conn, run_id, lease_token=token, status=STATUS_FAILURE,
                 error=f"{type(exc).__name__}: {exc}", now=moment)
        return {"lane": lane, "ran": True, "runId": run_id, "status": STATUS_FAILURE,
                "error": f"{type(exc).__name__}: {exc}"[:LAST_ERROR_CAP]}

    complete(
        conn, run_id, lease_token=token, status=outcome["status"],
        records_read=outcome.get("recordsRead", 0),
        records_written=outcome.get("recordsWritten", 0),
        records_skipped=outcome.get("recordsSkipped", 0),
        queue_depth=outcome.get("queueDepth"),
        error=outcome.get("error", ""), detail=outcome.get("detail"), now=moment,
    )
    return {
        "lane": lane, "ran": True, "runId": run_id, "status": outcome["status"],
        "recordsRead": outcome.get("recordsRead", 0),
        "recordsWritten": outcome.get("recordsWritten", 0),
        "recordsSkipped": outcome.get("recordsSkipped", 0),
        "error": outcome.get("error", ""),
    }


def dispatch(
    conn: Connection | None = None,
    *,
    lanes=None,
    now: datetime | None = None,
    force: bool = False,
    trigger: str = "dispatcher",
) -> dict:
    """ONE pass over the events-owned lanes that are due. Runs them, then RETURNS.

    There is no loop and no delay here, by design (see the module docstring). This never calls
    itself; a caller decides whether it is an explicit pass or a model-free production check.
    """
    moment = _now(now)
    if not automation_enabled():
        return {"enabled": False, "dispatchedAt": _iso(moment), "lanes": []}

    requested = [name for name in (lanes or EVENTS_OWNED) if LANES.get(name)
                 and LANES[name].runner == "events"]
    owns = conn is None
    conn = conn or connect()
    try:
        results = [
            run_lane(conn, name, now=moment, force=force, trigger=trigger)
            for name in requested
        ]
    finally:
        if owns:
            conn.close()
    return {"enabled": True, "dispatchedAt": _iso(moment), "lanes": results}


# ---- HTTP surface (§9.28: the router is exported as `router`) ------------------------------------

router = APIRouter()


def _require_internal(secret: str | None) -> None:
    """Constant-length comparison against `AUTH_SECRET`, the same internal seam `alerts` uses.

    These routes mutate lane state and are for `services/llm`'s runner and an operator. They are
    never reachable from the browser: the gateway does not proxy them.
    """
    import hmac

    expected = internal_secret()
    if not hmac.compare_digest(str(secret or ""), expected):
        raise HTTPException(status_code=401, detail="internal authentication required")


@router.post("/automation/lease")
def http_lease(
    payload: dict = Body(default=None),
    x_internal_secret: str | None = Header(default=None, alias="X-Internal-Secret"),
) -> dict:
    """Lease one lane for a worker in another service (`services/llm` owns two of the eight)."""
    _require_internal(x_internal_secret)
    body = payload if isinstance(payload, dict) else {}
    lane = str(body.get("lane") or "").strip()
    if lane not in LANES:
        raise HTTPException(status_code=400, detail=f"unknown lane: {lane or '(missing)'}")
    if not lane_enabled(lane):
        return {"leased": False, "lane": lane, "reason": REASON_DISABLED}

    conn = connect()
    try:
        return acquire(
            conn, lane,
            trigger=str(body.get("trigger") or "worker"),
            force=bool(body.get("force")),
            lease_for=body.get("leaseSeconds"),
        )
    finally:
        conn.close()


@router.post("/automation/runs/{run_id}/complete")
def http_complete(
    run_id: str,
    payload: dict = Body(default=None),
    x_internal_secret: str | None = Header(default=None, alias="X-Internal-Secret"),
) -> dict:
    _require_internal(x_internal_secret)
    body = payload if isinstance(payload, dict) else {}
    status = str(body.get("status") or "").strip()
    if status not in TERMINAL_STATUSES:
        raise HTTPException(status_code=400,
                            detail=f"status must be one of {list(TERMINAL_STATUSES)}")
    detail = body.get("detail")
    conn = connect()
    try:
        ok = complete(
            conn, run_id,
            lease_token=str(body.get("leaseToken") or ""),
            status=status,
            records_read=int(body.get("recordsRead") or 0),
            records_written=int(body.get("recordsWritten") or 0),
            records_skipped=int(body.get("recordsSkipped") or 0),
            queue_depth=body.get("queueDepth"),
            error=str(body.get("error") or ""),
            detail=detail if isinstance(detail, dict) else {},
        )
    finally:
        conn.close()
    if not ok:
        raise HTTPException(status_code=409,
                            detail="lease is missing, expired, or already completed")
    return {"ok": True, "runId": run_id}


@router.get("/automation/status")
def http_status(limit: int = Query(default=20, ge=1, le=MAX_RUN_LIMIT)) -> dict:
    """Operator-safe automation health. Reports flag NAMES and booleans, never a flag VALUE."""
    from . import budget

    conn = connect()
    try:
        provider_quotas = budget.state(conn)
        return {
            "automationEnabled": automation_enabled(),
            "selectedLanes": selected_lanes(),
            "productionScheduler": {
                "enabled": production_scheduler_enabled(),
                "pollSeconds": production_scheduler_poll_seconds(),
                "ownedLanes": list(EVENTS_OWNED),
                "modelLanesScheduled": False,
            },
            "lanes": lane_status(conn),
            "recentRuns": recent_runs(conn, limit=limit),
            "providerQuotas": provider_quotas,
            "quotaSummary": budget.quota_summary(provider_quotas),
        }
    finally:
        conn.close()


@router.get("/automation/runs")
def http_runs(
    lane: str | None = Query(default=None),
    limit: int = Query(default=20, ge=1, le=MAX_RUN_LIMIT),
) -> dict:
    if lane and lane not in LANES:
        raise HTTPException(status_code=400, detail=f"unknown lane: {lane}")
    conn = connect()
    try:
        return {"runs": recent_runs(conn, lane=lane, limit=limit)}
    finally:
        conn.close()


# ---- CLI ------------------------------------------------------------------------------------------


def main(argv=None) -> int:
    """`python -m app.automation [--lane NAME ...] [--force]` — ONE dispatch pass, then exit."""
    import argparse

    parser = argparse.ArgumentParser(description="One bounded automation dispatch pass.")
    parser.add_argument("--lane", action="append", default=None,
                        help="restrict to this lane (repeatable)")
    parser.add_argument("--force", action="store_true",
                        help="ignore next-eligible pacing (still respects the lease and the flags)")
    args = parser.parse_args(argv)

    report = dispatch(lanes=args.lane, force=args.force)
    print(json.dumps(report, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
