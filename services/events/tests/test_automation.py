"""Phase 1 — the automation dispatcher: default-off, locked, idempotent, restart-safe.

Every assertion here is about the OPERATIONAL layer, not about the jobs themselves. The jobs are
already tested by `test_ingest.py`; what was missing, and what this file covers, is:

* nothing runs unless BOTH the master flag and the lane's own flag are true;
* two dispatchers cannot enter the same lane;
* a completion from a stale lease cannot land;
* an abandoned run is reconciled rather than left `running` forever;
* a lane whose entrypoint raises is recorded as a failure and the lane is left retryable;
* neither the module nor the read routes contain a timer, and neither calls a provider or a model.
"""
from __future__ import annotations

import re
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from app import automation, budget
from app.automation import (
    LANE_ENRICH,
    LANE_INGEST,
    LANE_OPPORTUNITY_RADAR,
    LANE_REACTIONS,
    LANE_SCOUT,
    LANE_SCOUT_INTAKE,
    LANE_RESYNTH,
    LANE_THESIS_MONITOR,
    LANES,
    STATUS_FAILURE,
    STATUS_SUCCESS,
    acquire,
    complete,
    dispatch,
    lane_status,
    recent_runs,
    router as automation_router,
    run_lane,
)
from app.db import connect, migrate

SERVICE_ROOT = Path(__file__).resolve().parent.parent
SECRET = "test-internal-secret"
T0 = datetime(2026, 8, 23, 12, 0, 0, tzinfo=timezone.utc)


@pytest.fixture
def conn():
    connection = connect()
    migrate(connection)
    try:
        yield connection
    finally:
        connection.close()


@pytest.fixture
def client(monkeypatch):
    monkeypatch.setenv("AUTH_SECRET", SECRET)
    app = FastAPI()
    app.include_router(automation_router)
    connection = connect()
    migrate(connection)
    connection.close()
    return TestClient(app)


def _enable(monkeypatch, *lanes, master=True):
    monkeypatch.setenv("AUTOMATION_ENABLED", "true" if master else "false")
    for spec in LANES.values():
        monkeypatch.setenv(spec.flag_env, "true" if spec.name in lanes else "false")
    monkeypatch.setenv("AUTOMATION_LANES", ",".join(lanes) if lanes else "")


# ── the default posture ─────────────────────────────────────────────────────────────────────────


def test_automation_is_off_with_an_empty_environment(monkeypatch):
    for name in ("AUTOMATION_ENABLED", "AUTOMATION_LANES", "INGEST_ENABLED",
                 "EVENT_ENRICH_ENABLED", "THESIS_RESYNTH_ENABLED", "THESIS_MONITOR_ENABLED",
                 "REACTION_CAPTURE_ENABLED", "SCOUT_ENABLED", "SCOUT_INGEST_ENABLED",
                 "OPPORTUNITY_RADAR_ENABLED"):
        monkeypatch.delenv(name, raising=False)
    assert automation.automation_enabled() is False
    for lane in LANES:
        assert automation.lane_enabled(lane) is False


def test_dispatch_with_the_master_flag_off_does_nothing_and_opens_no_connection(monkeypatch):
    monkeypatch.setenv("AUTOMATION_ENABLED", "false")
    monkeypatch.setenv("INGEST_ENABLED", "true")

    def explode(*_a, **_kw):
        raise AssertionError("a disabled dispatcher must not open a database connection")

    monkeypatch.setattr(automation, "connect", explode)
    report = dispatch(now=T0)
    assert report == {"enabled": False, "dispatchedAt": "2026-08-23T12:00:00Z", "lanes": []}


def test_master_flag_alone_does_not_enable_a_lane(monkeypatch):
    monkeypatch.setenv("AUTOMATION_ENABLED", "true")
    monkeypatch.delenv("AUTOMATION_LANES", raising=False)
    monkeypatch.setenv("INGEST_ENABLED", "false")
    assert automation.lane_enabled(LANE_INGEST) is False


def test_lane_flag_alone_does_not_enable_a_lane(monkeypatch):
    monkeypatch.setenv("AUTOMATION_ENABLED", "false")
    monkeypatch.setenv("INGEST_ENABLED", "true")
    assert automation.lane_enabled(LANE_INGEST) is False


def test_allowlist_narrows_the_enabled_set(monkeypatch):
    monkeypatch.setenv("AUTOMATION_ENABLED", "true")
    monkeypatch.setenv("INGEST_ENABLED", "true")
    monkeypatch.setenv("EVENT_ENRICH_ENABLED", "true")
    monkeypatch.setenv("AUTOMATION_LANES", "ingest")
    assert automation.lane_enabled(LANE_INGEST) is True
    assert automation.lane_enabled(LANE_ENRICH) is False


def test_scout_intake_lane_reuses_the_bounded_intake_entrypoint(monkeypatch):
    from app import scout as scout_module

    monkeypatch.setattr(
        scout_module, "run_intake",
        lambda connection, *, now=None: {
            "runId": "ingest_1", "fetched": 4, "inserted": 3, "deduped": 1,
            "degraded": [], "scoutBatch": ["AMD", "AVGO"],
        },
    )
    result = automation._run_scout_intake_lane(object(), now=T0)
    assert result["status"] == STATUS_SUCCESS
    assert (result["recordsRead"], result["recordsWritten"], result["recordsSkipped"]) == (
        4, 3, 1,
    )


def test_scout_lane_reuses_the_model_free_materializer(monkeypatch):
    from app import scout as scout_module

    monkeypatch.setattr(
        scout_module, "run_scout",
        lambda connection, *, now=None: {
            "runId": "scout_1", "candidateCount": 2, "degraded": [],
            "coverage": {"canonicalEventRows": 5, "scheduledCatalystRows": 1},
        },
    )
    result = automation._run_scout_lane(object(), now=T0)
    assert result["status"] == STATUS_SUCCESS
    assert (result["recordsRead"], result["recordsWritten"], result["recordsSkipped"]) == (
        6, 2, 0,
    )


def test_opportunity_lane_reuses_the_completed_bar_materializer(monkeypatch):
    from app import opportunities as opportunities_module

    monkeypatch.setattr(
        opportunities_module, "run_opportunity_radar",
        lambda connection, *, now=None: {
            "runId": "opp_1", "candidateCount": 2, "persistedCandidateCount": 2,
            "degraded": [], "skipped": False,
            "coverage": {"state": "ok", "marketDataCovered": 30, "universeSize": 30},
        },
    )
    result = automation._run_opportunity_radar_lane(object(), now=T0)
    assert result["status"] == STATUS_SUCCESS
    assert (result["recordsRead"], result["recordsWritten"], result["recordsSkipped"]) == (
        30, 2, 0,
    )
    assert LANE_OPPORTUNITY_RADAR in automation.EVENTS_OWNED


def test_a_disabled_lane_is_never_run_by_run_lane(monkeypatch, conn):
    _enable(monkeypatch, master=False)
    result = run_lane(conn, LANE_INGEST, now=T0)
    assert result == {"lane": LANE_INGEST, "ran": False, "reason": "disabled"}
    assert recent_runs(conn) == []


# ── leasing and locking ──────────────────────────────────────────────────────────────────────────


def test_a_second_dispatcher_cannot_enter_a_leased_lane(conn):
    first = acquire(conn, LANE_INGEST, now=T0)
    assert first["leased"] is True
    second = acquire(conn, LANE_INGEST, now=T0)
    assert second["leased"] is False
    assert second["reason"] == "locked"


def test_a_lane_is_not_due_until_its_interval_elapses(monkeypatch, conn):
    monkeypatch.setenv("AUTOMATION_INTERVAL_INGEST", "3600")
    lease = acquire(conn, LANE_INGEST, now=T0)
    assert complete(conn, lease["runId"], lease_token=lease["leaseToken"],
                    status=STATUS_SUCCESS, now=T0) is True

    too_soon = acquire(conn, LANE_INGEST, now=T0 + timedelta(minutes=30))
    assert too_soon["leased"] is False
    assert too_soon["reason"] == "not-due"

    due = acquire(conn, LANE_INGEST, now=T0 + timedelta(minutes=61))
    assert due["leased"] is True


def test_force_overrides_pacing_but_not_the_lease(monkeypatch, conn):
    monkeypatch.setenv("AUTOMATION_INTERVAL_INGEST", "3600")
    lease = acquire(conn, LANE_INGEST, now=T0)
    complete(conn, lease["runId"], lease_token=lease["leaseToken"], status=STATUS_SUCCESS, now=T0)

    forced = acquire(conn, LANE_INGEST, now=T0 + timedelta(minutes=1), force=True)
    assert forced["leased"] is True
    blocked = acquire(conn, LANE_INGEST, now=T0 + timedelta(minutes=2), force=True)
    assert blocked["leased"] is False and blocked["reason"] == "locked"


def test_an_expired_lease_can_be_retaken(monkeypatch, conn):
    monkeypatch.setenv("AUTOMATION_LEASE_SECONDS", "60")
    first = acquire(conn, LANE_INGEST, now=T0)
    assert first["leased"] is True
    retaken = acquire(conn, LANE_INGEST, now=T0 + timedelta(minutes=5))
    assert retaken["leased"] is True
    assert retaken["leaseToken"] != first["leaseToken"]


def test_a_stale_lease_token_cannot_complete_a_run(monkeypatch, conn):
    monkeypatch.setenv("AUTOMATION_LEASE_SECONDS", "60")
    first = acquire(conn, LANE_INGEST, now=T0)
    second = acquire(conn, LANE_INGEST, now=T0 + timedelta(minutes=5))
    assert second["leased"] is True

    assert complete(conn, first["runId"], lease_token=first["leaseToken"],
                    status=STATUS_SUCCESS, now=T0 + timedelta(minutes=6)) is False
    assert complete(conn, second["runId"], lease_token=second["leaseToken"],
                    status=STATUS_SUCCESS, now=T0 + timedelta(minutes=6)) is True


def test_completing_twice_is_rejected_the_second_time(conn):
    lease = acquire(conn, LANE_INGEST, now=T0)
    assert complete(conn, lease["runId"], lease_token=lease["leaseToken"],
                    status=STATUS_SUCCESS, now=T0) is True
    assert complete(conn, lease["runId"], lease_token=lease["leaseToken"],
                    status=STATUS_SUCCESS, now=T0) is False


def test_a_wrong_token_cannot_complete_a_live_run(conn):
    lease = acquire(conn, LANE_INGEST, now=T0)
    assert complete(conn, lease["runId"], lease_token="not-the-token",
                    status=STATUS_SUCCESS, now=T0) is False


def test_an_abandoned_run_is_reconciled_to_failure_not_left_running(monkeypatch, conn):
    monkeypatch.setenv("AUTOMATION_LEASE_SECONDS", "60")
    first = acquire(conn, LANE_INGEST, now=T0)
    acquire(conn, LANE_INGEST, now=T0 + timedelta(minutes=5))

    runs = {r["id"]: r for r in recent_runs(conn, lane=LANE_INGEST)}
    assert runs[first["runId"]]["status"] == STATUS_FAILURE
    assert "abandoned" in runs[first["runId"]]["lastError"]


# ── run metadata ────────────────────────────────────────────────────────────────────────────────


def test_a_completed_run_records_the_required_metadata(conn):
    lease = acquire(conn, LANE_INGEST, now=T0, trigger="dispatcher")
    complete(conn, lease["runId"], lease_token=lease["leaseToken"], status=STATUS_SUCCESS,
             records_read=7, records_written=4, records_skipped=3, queue_depth=2,
             detail={"runId": "run_x"}, now=T0 + timedelta(seconds=30))

    run = recent_runs(conn, lane=LANE_INGEST)[0]
    assert run["lane"] == LANE_INGEST
    assert run["trigger"] == "dispatcher"
    assert run["startedAt"] == "2026-08-23T12:00:00Z"
    assert run["completedAt"] == "2026-08-23T12:00:30Z"
    assert run["status"] == STATUS_SUCCESS
    assert (run["recordsRead"], run["recordsWritten"], run["recordsSkipped"]) == (7, 4, 3)
    assert run["queueDepth"] == 2
    assert run["detail"] == {"runId": "run_x"}


def test_lane_status_reports_freshness_and_next_eligible(monkeypatch, conn):
    monkeypatch.setenv("AUTOMATION_INTERVAL_INGEST", "600")
    lease = acquire(conn, LANE_INGEST, now=T0)
    complete(conn, lease["runId"], lease_token=lease["leaseToken"], status=STATUS_SUCCESS, now=T0)

    row = {r["lane"]: r for r in lane_status(conn, now=T0 + timedelta(minutes=2))}[LANE_INGEST]
    assert row["lastStatus"] == STATUS_SUCCESS
    assert row["lastSuccessAt"] == "2026-08-23T12:00:00Z"
    assert row["nextEligibleAt"] == "2026-08-23T12:10:00Z"
    assert row["secondsSinceLastSuccess"] == 120
    assert row["running"] is False
    assert row["consecutiveFailures"] == 0


def test_a_degraded_run_is_neither_a_success_nor_a_failure(conn):
    """The three outcomes stay distinct.

    A pass in which every provider skipped for a missing key really did run — but it is not
    evidence that the lane works, so it must not stamp `lastSuccessAt` and make an entirely idle
    lane read as healthy. Nor is it a failure, so it must not accumulate toward a false alarm.
    """
    lease = acquire(conn, LANE_INGEST, now=T0)
    complete(conn, lease["runId"], lease_token=lease["leaseToken"], status="degraded",
             error="fred:no-key", now=T0)
    row = {r["lane"]: r for r in lane_status(conn, now=T0)}[LANE_INGEST]
    assert row["lastStatus"] == "degraded"
    assert row["lastFinishedAt"] == "2026-08-23T12:00:00Z"   # it ran
    assert row["lastSuccessAt"] is None                       # but it did not fully work
    assert row["lastFailureAt"] is None                       # and it did not fail
    assert row["consecutiveFailures"] == 0


def test_failures_accumulate_and_a_success_resets_them(conn):
    for offset in (0, 1):
        lease = acquire(conn, LANE_INGEST, now=T0 + timedelta(hours=offset), force=True)
        complete(conn, lease["runId"], lease_token=lease["leaseToken"], status=STATUS_FAILURE,
                 error="boom", now=T0 + timedelta(hours=offset))
    row = {r["lane"]: r for r in lane_status(conn, now=T0)}[LANE_INGEST]
    assert row["consecutiveFailures"] == 2
    assert row["lastFailureAt"] is not None
    assert row["lastSuccessAt"] is None
    assert row["lastError"] == "boom"

    lease = acquire(conn, LANE_INGEST, now=T0 + timedelta(hours=2), force=True)
    complete(conn, lease["runId"], lease_token=lease["leaseToken"], status=STATUS_SUCCESS,
             now=T0 + timedelta(hours=2))
    row = {r["lane"]: r for r in lane_status(conn, now=T0)}[LANE_INGEST]
    assert row["consecutiveFailures"] == 0


def test_lane_status_never_leaks_a_flag_value_or_a_secret(monkeypatch, conn):
    monkeypatch.setenv("AUTH_SECRET", "super-secret-value")
    monkeypatch.setenv("INGEST_ENABLED", "true")
    rendered = repr(lane_status(conn, now=T0))
    assert "super-secret-value" not in rendered
    # The variable NAME is reported so an operator knows which knob to turn; its value is not.
    assert "INGEST_ENABLED" in rendered


# ── failure containment ──────────────────────────────────────────────────────────────────────────


def test_a_raising_entrypoint_is_recorded_and_the_lane_is_left_retryable(monkeypatch, conn):
    _enable(monkeypatch, LANE_INGEST)

    def boom(_conn, *, now):
        raise RuntimeError("provider exploded")

    monkeypatch.setitem(automation.LANE_RUNNERS, LANE_INGEST, boom)
    result = run_lane(conn, LANE_INGEST, now=T0)
    assert result["ran"] is True and result["status"] == STATUS_FAILURE
    assert "provider exploded" in result["error"]

    run = recent_runs(conn, lane=LANE_INGEST)[0]
    assert run["status"] == STATUS_FAILURE
    assert run["completedAt"] is not None

    row = {r["lane"]: r for r in lane_status(conn, now=T0)}[LANE_INGEST]
    assert row["running"] is False  # the lease was released, so the next pass can retry


def test_run_lane_reuses_the_existing_ingest_entrypoint(monkeypatch, conn):
    _enable(monkeypatch, LANE_INGEST)
    seen = {}

    def fake_run_ingest(connection, *, now=None):
        seen["called"] = True
        return {"runId": "run_1", "fetched": 5, "inserted": 3, "deduped": 2, "degraded": []}

    from app import ingest as ingest_module
    monkeypatch.setattr(ingest_module, "run_ingest", fake_run_ingest)

    result = run_lane(conn, LANE_INGEST, now=T0)
    assert seen == {"called": True}
    assert result["status"] == STATUS_SUCCESS
    assert (result["recordsRead"], result["recordsWritten"], result["recordsSkipped"]) == (5, 3, 2)


def test_degraded_providers_produce_a_degraded_run_not_a_success(monkeypatch, conn):
    _enable(monkeypatch, LANE_INGEST)

    from app import ingest as ingest_module
    monkeypatch.setattr(
        ingest_module, "run_ingest",
        lambda connection, *, now=None: {
            "runId": "run_2", "fetched": 0, "inserted": 0, "deduped": 0,
            "degraded": ["fred:no-key"],
        },
    )
    result = run_lane(conn, LANE_INGEST, now=T0)
    assert result["status"] == "degraded"
    assert recent_runs(conn, lane=LANE_INGEST)[0]["lastError"] == "fred:no-key"


def test_dispatch_only_runs_events_owned_lanes(monkeypatch, conn):
    _enable(monkeypatch, LANE_INGEST, LANE_ENRICH, LANE_RESYNTH, LANE_THESIS_MONITOR)
    from app import ingest as ingest_module
    monkeypatch.setattr(
        ingest_module, "run_ingest",
        lambda connection, *, now=None: {"runId": "r", "fetched": 0, "inserted": 0,
                                         "deduped": 0, "degraded": []},
    )
    report = dispatch(conn, now=T0)
    assert report["enabled"] is True
    # Every events-owned lane is CONSIDERED, so the report explains each decision; only the
    # enabled one actually runs, and the llm/alerts lanes are not considered here at all.
    assert {entry["lane"] for entry in report["lanes"]} == {
        LANE_INGEST, LANE_REACTIONS, LANE_SCOUT_INTAKE, LANE_SCOUT,
        LANE_OPPORTUNITY_RADAR,
    }
    assert [e["lane"] for e in report["lanes"] if e["ran"]] == [LANE_INGEST]
    assert [e["reason"] for e in report["lanes"] if not e["ran"]] == [
        "disabled", "disabled", "disabled", "disabled",
    ]


def test_the_llm_owned_lanes_are_declared_but_not_executed_here(conn):
    assert LANES[LANE_ENRICH].owner == "llm"
    assert LANES[LANE_RESYNTH].owner == "llm"
    assert LANES[LANE_THESIS_MONITOR].owner == "alerts"
    assert run_lane(conn, LANE_ENRICH, now=T0)["reason"] == "not-owned-here"


# ── the HTTP surface ────────────────────────────────────────────────────────────────────────────


def test_lease_requires_the_internal_secret(client):
    unauthorised = client.post("/automation/lease", json={"lane": LANE_ENRICH})
    assert unauthorised.status_code == 401
    wrong = client.post("/automation/lease", json={"lane": LANE_ENRICH},
                        headers={"X-Internal-Secret": "nope"})
    assert wrong.status_code == 401


def test_complete_requires_the_internal_secret(client):
    assert client.post("/automation/runs/aut_x/complete",
                       json={"status": "success"}).status_code == 401


def test_lease_refuses_a_disabled_lane_even_with_the_secret(client, monkeypatch):
    monkeypatch.setenv("AUTOMATION_ENABLED", "false")
    response = client.post("/automation/lease", json={"lane": LANE_ENRICH},
                           headers={"X-Internal-Secret": SECRET})
    assert response.status_code == 200
    assert response.json() == {"leased": False, "lane": LANE_ENRICH, "reason": "disabled"}


def test_lease_then_complete_over_http(client, monkeypatch):
    _enable(monkeypatch, LANE_ENRICH)
    monkeypatch.setenv("AUTH_SECRET", SECRET)
    leased = client.post("/automation/lease", json={"lane": LANE_ENRICH, "trigger": "worker"},
                         headers={"X-Internal-Secret": SECRET}).json()
    assert leased["leased"] is True

    done = client.post(
        f"/automation/runs/{leased['runId']}/complete",
        json={"leaseToken": leased["leaseToken"], "status": "success", "recordsRead": 3,
              "recordsWritten": 1, "queueDepth": 9, "detail": {"stopped": "drained"}},
        headers={"X-Internal-Secret": SECRET},
    )
    assert done.status_code == 200

    replay = client.post(
        f"/automation/runs/{leased['runId']}/complete",
        json={"leaseToken": leased["leaseToken"], "status": "success"},
        headers={"X-Internal-Secret": SECRET},
    )
    assert replay.status_code == 409


def test_status_is_readable_without_the_secret_and_carries_no_secret(client, monkeypatch):
    monkeypatch.setenv("AUTH_SECRET", "super-secret-value")
    response = client.get("/automation/status")
    assert response.status_code == 200
    body = response.json()
    assert {row["lane"] for row in body["lanes"]} == set(LANES)
    assert "super-secret-value" not in response.text
    assert body["automationEnabled"] in (True, False)
    assert body["productionScheduler"]["ownedLanes"] == [
        "ingest", "reactions", "scout-intake", "scout", "opportunity-radar",
    ]
    assert body["productionScheduler"]["modelLanesScheduled"] is False
    assert body["quotaSummary"]["scope"] == "events-ingestion"
    assert {row["provider"] for row in body["providerQuotas"]} == set(budget.DAILY_LIMITS)


def test_unknown_lane_is_rejected(client, monkeypatch):
    monkeypatch.setenv("AUTH_SECRET", SECRET)
    assert client.post("/automation/lease", json={"lane": "nope"},
                       headers={"X-Internal-Secret": SECRET}).status_code == 400
    assert client.get("/automation/runs", params={"lane": "nope"}).status_code == 400


# ── the invariants, asserted against the source ─────────────────────────────────────────────────


def test_the_module_contains_no_timer():
    """§9.22: repetition is the operator's cron, outside the codebase.

    Asserted on the source text, prose included, exactly as `test_enrich_worker.py` does — so the
    two forbidden spellings do not appear here even as an example.
    """
    source = (SERVICE_ROOT / "app" / "automation.py").read_text(encoding="utf-8")
    assert "while" + " True" not in source
    assert "time." + "sleep" not in source
    assert "sleep(" not in source
    assert not re.search(r"\bimport\s+(sched|threading|asyncio)\b", source)
    assert "Timer" not in source


def test_the_module_calls_no_provider_and_no_model():
    source = (SERVICE_ROOT / "app" / "automation.py").read_text(encoding="utf-8")
    assert "requests" not in source
    assert "8002" not in source
    assert not re.search(r"\bfrom\s+\.providers\b", source)


# ── read paths never reach a provider or a model ────────────────────────────────────────────────


def test_the_read_routes_make_no_provider_call_even_with_every_key_set(monkeypatch):
    """The §1.5 leakage shape, applied to the Phase 1 surface.

    Providers are patched to RAISE. If any read route could reach one, the request fails loudly
    rather than quietly succeeding against a live socket — which is the only version of this test
    that proves anything (§9.44: an absent log line is not evidence).
    """
    from app import macro as macro_module
    from app import providers as providers_module

    for name in ("MARKETAUX_API_KEY", "ALPHAVANTAGE_API_KEY", "FRED_API_KEY", "FMP_API_KEY"):
        monkeypatch.setenv(name, "set-but-must-not-be-used")
    monkeypatch.setenv("EVENTS_CONTACT_UA", "ops@example.com")
    monkeypatch.setenv("FEDERAL_RESERVE_ENABLED", "true")
    monkeypatch.setenv("AUTOMATION_ENABLED", "true")
    monkeypatch.setenv("INGEST_ENABLED", "true")

    def explode(*_a, **_kw):
        raise AssertionError("a read route reached a provider")

    monkeypatch.setattr(providers_module.requests, "get", explode)
    monkeypatch.setattr(providers_module.requests, "request", explode, raising=False)

    app = FastAPI()
    app.include_router(automation_router)
    app.include_router(macro_module.router)
    connection = connect()
    migrate(connection)
    connection.close()
    reader = TestClient(app)

    assert reader.get("/automation/status").status_code == 200
    assert reader.get("/automation/runs").status_code == 200
    assert reader.get(
        "/calendar", params={"from": "2026-01-01T00:00:00Z", "to": "2026-12-31T00:00:00Z"}
    ).status_code == 200


def test_no_read_route_can_start_a_lane(monkeypatch):
    """A GET must never be able to lease a lane. Leasing is a POST behind the internal secret."""
    monkeypatch.setenv("AUTOMATION_ENABLED", "true")
    monkeypatch.setenv("INGEST_ENABLED", "true")
    monkeypatch.setenv("AUTH_SECRET", SECRET)
    app = FastAPI()
    app.include_router(automation_router)
    connection = connect()
    migrate(connection)
    connection.close()
    reader = TestClient(app)

    assert reader.get("/automation/lease").status_code in (404, 405)
    before = reader.get("/automation/status").json()
    after = reader.get("/automation/status").json()
    assert before["recentRuns"] == after["recentRuns"] == []


def test_the_events_service_automation_module_has_no_model_seam():
    """Phase 1 added no model seam to this service. The lanes that call the model live elsewhere."""
    source = (SERVICE_ROOT / "app" / "automation.py").read_text(encoding="utf-8")
    assert "LLM_URL" not in source
    assert "MODEL_RUNTIME" not in source
