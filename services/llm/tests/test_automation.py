"""Phase 1 — the worker-side automation runner.

The point of these tests is that this file is an OPERATIONAL layer and nothing else:

* with the flags at their shipped defaults it makes no call at all — not to the events service,
  not to alerts, and above all not to the model;
* it never runs a lane it did not successfully lease, so two runners cannot collide;
* it reuses the existing one-shot entrypoints rather than reimplementing them;
* a lost lease discards OUR result instead of overwriting the newer run's;
* the deterministic thesis-monitor lane reaches alerts and nothing else;
* the source contains no timer.
"""
from __future__ import annotations

import re
from pathlib import Path

import pytest

from app import automation

SERVICE_ROOT = Path(__file__).resolve().parent.parent


@pytest.fixture(autouse=True)
def clean_env(monkeypatch):
    for name in ("AUTOMATION_ENABLED", "AUTOMATION_LANES", "EVENT_ENRICH_ENABLED",
                 "THESIS_RESYNTH_ENABLED", "THESIS_MONITOR_ENABLED"):
        monkeypatch.delenv(name, raising=False)
    monkeypatch.setenv("EVENTS_URL", "http://events.test")
    monkeypatch.setenv("ALERTS_URL", "http://alerts.test")


def _no_http(monkeypatch):
    calls = []

    def explode(method, url, **kwargs):
        calls.append((method, url))
        raise AssertionError(f"no HTTP call is permitted here, saw {method} {url}")

    monkeypatch.setattr(automation.requests, "request", explode)
    return calls


def _record_http(monkeypatch, responses):
    """Route each request to a canned `(status, body)` by URL substring."""
    seen = []

    class _Resp:
        def __init__(self, status, body):
            self.status_code = status
            self._body = body

        def json(self):
            return self._body

    def fake(method, url, **kwargs):
        seen.append({"method": method, "url": url, "json": kwargs.get("json")})
        for needle, (status, body) in responses.items():
            if needle in url:
                return _Resp(status, body)
        raise AssertionError(f"unexpected URL {url}")

    monkeypatch.setattr(automation.requests, "request", fake)
    return seen


# ── the default posture ─────────────────────────────────────────────────────────────────────────


def test_every_lane_is_off_with_an_empty_environment():
    assert automation.automation_enabled() is False
    for lane in automation.WORKER_LANES:
        assert automation.lane_enabled(lane) is False


def test_dispatch_makes_no_call_at_all_when_disabled(monkeypatch):
    _no_http(monkeypatch)
    report = automation.dispatch()
    assert report["enabled"] is False
    assert report["lanes"] == []


def test_the_master_flag_alone_does_not_enable_the_model_lanes(monkeypatch):
    monkeypatch.setenv("AUTOMATION_ENABLED", "true")
    assert automation.lane_enabled("enrich") is False
    assert automation.lane_enabled("resynth") is False


def test_a_disabled_lane_is_never_even_leased(monkeypatch):
    monkeypatch.setenv("AUTOMATION_ENABLED", "true")
    monkeypatch.setenv("EVENT_ENRICH_ENABLED", "false")
    _no_http(monkeypatch)
    assert automation.run_lane("enrich") == {"lane": "enrich", "ran": False, "reason": "disabled"}


def test_the_allowlist_narrows_the_worker_lanes(monkeypatch):
    monkeypatch.setenv("AUTOMATION_ENABLED", "true")
    monkeypatch.setenv("EVENT_ENRICH_ENABLED", "true")
    monkeypatch.setenv("THESIS_RESYNTH_ENABLED", "true")
    monkeypatch.setenv("AUTOMATION_LANES", "enrich")
    assert automation.lane_enabled("enrich") is True
    assert automation.lane_enabled("resynth") is False


# ── leasing ──────────────────────────────────────────────────────────────────────────────────────


def test_a_refused_lease_runs_nothing(monkeypatch):
    monkeypatch.setenv("AUTOMATION_ENABLED", "true")
    monkeypatch.setenv("EVENT_ENRICH_ENABLED", "true")
    seen = _record_http(monkeypatch, {
        "/automation/lease": (200, {"leased": False, "reason": "locked"}),
    })

    def explode():
        raise AssertionError("the entrypoint must not run without a lease")

    monkeypatch.setitem(automation.LANE_RUNNERS, "enrich", explode)
    result = automation.run_lane("enrich")
    assert result == {"lane": "enrich", "ran": False, "reason": "locked", "nextEligibleAt": None}
    assert [call["url"] for call in seen] == ["http://events.test/automation/lease"]


def test_an_unreachable_lease_service_runs_nothing(monkeypatch):
    monkeypatch.setenv("AUTOMATION_ENABLED", "true")
    monkeypatch.setenv("EVENT_ENRICH_ENABLED", "true")

    def boom(method, url, **kwargs):
        raise automation.requests.RequestException("connection refused")

    monkeypatch.setattr(automation.requests, "request", boom)
    monkeypatch.setitem(
        automation.LANE_RUNNERS, "enrich",
        lambda: (_ for _ in ()).throw(AssertionError("must not run")),
    )
    result = automation.run_lane("enrich")
    assert result["ran"] is False and result["reason"] == "events-unreachable"


def test_the_lease_carries_the_internal_secret(monkeypatch):
    monkeypatch.setenv("AUTOMATION_ENABLED", "true")
    monkeypatch.setenv("EVENT_ENRICH_ENABLED", "true")
    monkeypatch.setenv("AUTH_SECRET", "unit-secret")
    captured = {}

    class _Resp:
        status_code = 200

        @staticmethod
        def json():
            return {"leased": False, "reason": "not-due"}

    def fake(method, url, **kwargs):
        captured.update(kwargs.get("headers") or {})
        return _Resp()

    monkeypatch.setattr(automation.requests, "request", fake)
    automation.run_lane("enrich")
    assert captured["X-Internal-Secret"] == "unit-secret"


# ── reuse of the existing entrypoints ────────────────────────────────────────────────────────────


def test_the_enrich_lane_calls_run_once_and_reports_its_counters(monkeypatch):
    monkeypatch.setenv("AUTOMATION_ENABLED", "true")
    monkeypatch.setenv("EVENT_ENRICH_ENABLED", "true")
    seen = _record_http(monkeypatch, {
        "/automation/lease": (200, {"leased": True, "runId": "aut_1", "leaseToken": "tok"}),
        "/complete": (200, {"ok": True}),
    })

    from app import enrich_worker
    monkeypatch.setattr(enrich_worker, "run_once", lambda: {
        "enabled": True, "candidates": 4, "posted": 3, "skipped": 1, "rejected": 0,
        "failed": 0, "stopped": "drained", "failures": [],
    })

    result = automation.run_lane("enrich")
    assert result["status"] == "success"
    assert (result["recordsRead"], result["recordsWritten"], result["recordsSkipped"]) == (4, 3, 1)
    completion = [c for c in seen if "/complete" in c["url"]][0]["json"]
    assert completion["leaseToken"] == "tok"
    assert completion["detail"]["stopped"] == "drained"


def test_a_stopped_enrich_pass_is_degraded_not_successful(monkeypatch):
    monkeypatch.setenv("AUTOMATION_ENABLED", "true")
    monkeypatch.setenv("EVENT_ENRICH_ENABLED", "true")
    _record_http(monkeypatch, {
        "/automation/lease": (200, {"leased": True, "runId": "aut_2", "leaseToken": "tok"}),
        "/complete": (200, {"ok": True}),
    })
    from app import enrich_worker
    monkeypatch.setattr(enrich_worker, "run_once", lambda: {
        "enabled": True, "candidates": 2, "posted": 0, "skipped": 0, "rejected": 0,
        "stopped": "events_unreachable", "failures": [{"eventId": None}],
    })
    assert automation.run_lane("enrich")["status"] == "degraded"


def test_the_resynth_lane_calls_its_own_run_once(monkeypatch):
    monkeypatch.setenv("AUTOMATION_ENABLED", "true")
    monkeypatch.setenv("THESIS_RESYNTH_ENABLED", "true")
    _record_http(monkeypatch, {
        "/automation/lease": (200, {"leased": True, "runId": "aut_3", "leaseToken": "tok"}),
        "/complete": (200, {"ok": True}),
    })
    from app import thesis_resynth
    monkeypatch.setattr(thesis_resynth, "run_once", lambda: {
        "enabled": True, "leased": 2, "completed": 2, "retried": 0, "alreadyApplied": 0,
        "skippedInactive": 0, "stopped": "drained", "failures": [],
    })
    result = automation.run_lane("resynth")
    assert result["status"] == "success"
    assert (result["recordsRead"], result["recordsWritten"]) == (2, 2)


def test_a_raising_entrypoint_is_recorded_as_a_failure(monkeypatch):
    monkeypatch.setenv("AUTOMATION_ENABLED", "true")
    monkeypatch.setenv("EVENT_ENRICH_ENABLED", "true")
    seen = _record_http(monkeypatch, {
        "/automation/lease": (200, {"leased": True, "runId": "aut_4", "leaseToken": "tok"}),
        "/complete": (200, {"ok": True}),
    })

    def boom():
        raise RuntimeError("model unreachable")

    monkeypatch.setitem(automation.LANE_RUNNERS, "enrich", boom)
    result = automation.run_lane("enrich")
    assert result["status"] == "failure"
    completion = [c for c in seen if "/complete" in c["url"]][0]["json"]
    assert completion["status"] == "failure"
    assert "model unreachable" in completion["error"]


def test_a_lost_lease_discards_our_result_instead_of_overwriting(monkeypatch):
    monkeypatch.setenv("AUTOMATION_ENABLED", "true")
    monkeypatch.setenv("EVENT_ENRICH_ENABLED", "true")
    _record_http(monkeypatch, {
        "/automation/lease": (200, {"leased": True, "runId": "aut_5", "leaseToken": "tok"}),
        "/complete": (409, {"detail": "lease is missing, expired, or already completed"}),
    })
    monkeypatch.setitem(automation.LANE_RUNNERS, "enrich", lambda: {
        "status": "success", "recordsRead": 1, "recordsWritten": 1,
    })
    # A 409 is the takeover rule, not a transport error: it is logged and swallowed.
    assert automation.run_lane("enrich")["status"] == "success"


# ── the deterministic lane ───────────────────────────────────────────────────────────────────────


def test_the_thesis_monitor_lane_reaches_alerts_and_nothing_else(monkeypatch):
    monkeypatch.setenv("AUTOMATION_ENABLED", "true")
    monkeypatch.setenv("THESIS_MONITOR_ENABLED", "true")
    seen = _record_http(monkeypatch, {
        "/automation/lease": (200, {"leased": True, "runId": "aut_6", "leaseToken": "tok"}),
        "/complete": (200, {"ok": True}),
        "/_internal/monitor/tick": (200, {
            "ran": True,
            "tick": {"theses": 5, "examined": 4, "marked": 2, "degraded": []},
            "queue": {"depth": 3},
        }),
    })
    result = automation.run_lane("thesis-monitor")
    assert result["status"] == "success"
    assert (result["recordsRead"], result["recordsWritten"], result["recordsSkipped"]) == (4, 2, 1)

    hosts = {call["url"].split("/")[2] for call in seen}
    assert hosts == {"events.test", "alerts.test"}
    completion = [c for c in seen if "/complete" in c["url"]][0]["json"]
    assert completion["queueDepth"] == 3


def test_a_disabled_alerts_monitor_produces_a_skipped_run(monkeypatch):
    monkeypatch.setenv("AUTOMATION_ENABLED", "true")
    monkeypatch.setenv("THESIS_MONITOR_ENABLED", "true")
    seen = _record_http(monkeypatch, {
        "/automation/lease": (200, {"leased": True, "runId": "aut_7", "leaseToken": "tok"}),
        "/complete": (200, {"ok": True}),
        "/_internal/monitor/tick": (200, {"ran": False, "flag": "THESIS_MONITOR_ENABLED"}),
    })
    assert automation.run_lane("thesis-monitor")["status"] == "skipped"
    completion = [c for c in seen if "/complete" in c["url"]][0]["json"]
    assert "THESIS_MONITOR_ENABLED" in completion["error"]


# ── the invariants, asserted against the source ─────────────────────────────────────────────────


def test_the_runner_contains_no_timer():
    source = (SERVICE_ROOT / "app" / "automation.py").read_text(encoding="utf-8")
    assert "while" + " True" not in source
    assert "time." + "sleep" not in source
    assert "sleep(" not in source
    assert not re.search(r"\bimport\s+(sched|threading|asyncio)\b", source)


def test_the_runner_reimplements_no_job_logic():
    """It may import the entrypoints; it may not contain the model or the queue itself."""
    source = (SERVICE_ROOT / "app" / "automation.py").read_text(encoding="utf-8")
    assert "call_model" not in source
    assert "acquire_background" not in source
    assert "from . import llm" not in source
