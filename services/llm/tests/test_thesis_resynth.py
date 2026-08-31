from __future__ import annotations

import inspect
import json


JOB = {
    "id": "rsy_1234",
    "thesisId": "th_1",
    "userId": "u1",
    "ticker": "NVDA",
    "reason": "close moved 7.0% higher",
    "severity": "material",
    "asOf": "2026-08-01T00:00:00Z",
    "enqueuedAt": 1_780_000_000,
    "state": "processing",
    "attempts": 1,
    "leaseToken": "lease-123",
}

GOOD_REPLY = json.dumps({
    "verdict": "challenged",
    "confidence": 0.72,
    "evidence": [{
        "fact": "The stored close moved materially from the review baseline.",
        "bearing": "contradicts",
        "note": "The price context changed, so the original reasoning needs another look.",
    }],
    "watchFor": ["The next reported quarter"],
    "summary": "The collected facts challenge one premise in the user's thesis.",
})


def _configure(monkeypatch, tmp_path):
    monkeypatch.setenv("THESIS_RESYNTH_ENABLED", "true")
    monkeypatch.setenv("THESIS_RESYNTH_BATCH_SIZE", "5")
    monkeypatch.setenv("ALERTS_URL", "http://alerts.test")
    monkeypatch.setenv("JOURNAL_URL", "http://journal.test")
    monkeypatch.setenv("AUTH_SECRET", "test-secret")
    monkeypatch.setenv("READS_DIR", str(tmp_path))
    monkeypatch.setenv("ENRICH_IDLE_SECONDS", "0")


def _facts(_name, _args, _ctx):
    return {"ok": True, "result": {"events": [], "macro": [], "timeframes": {}},
            "error": None, "degraded": []}


def test_disabled_worker_opens_no_queue_or_model(monkeypatch):
    from app import thesis_resynth

    monkeypatch.delenv("THESIS_RESYNTH_ENABLED", raising=False)

    def explode(*_args, **_kwargs):
        raise AssertionError("disabled worker performed work")

    monkeypatch.setattr(thesis_resynth, "_http", explode)
    monkeypatch.setattr(thesis_resynth.llm, "call_model", explode)
    report = thesis_resynth.run_once()
    assert report["enabled"] is False
    assert report["generated"] == 0
    assert report["stopped"] == thesis_resynth.DISABLED


def test_one_pass_generates_under_lease_persists_then_completes(monkeypatch, tmp_path):
    from app import lease, thesis_resynth

    _configure(monkeypatch, tmp_path)
    requests = []
    fact_calls = []
    lease_reads = 0

    def fake_http(method, url, **kwargs):
        nonlocal lease_reads
        requests.append((method, url, kwargs))
        if url.endswith("/_internal/resynth/lease"):
            lease_reads += 1
            return (200, {"job": dict(JOB)}) if lease_reads == 1 else (204, {})
        if method == "GET":
            return 200, {"thesis": {
                "id": "th_1", "ticker": "NVDA", "status": "active",
                "claim": "Demand growth remains durable.",
            }}
        if method == "PATCH":
            return 200, {"thesis": {"id": "th_1"}}
        if url.endswith("/complete"):
            return 200, {"ok": True}
        raise AssertionError((method, url))

    held = []

    def model(messages):
        held.append(lease.background_lease_is_held())
        return GOOD_REPLY, "qwen3-14b"

    def facts(name, args, ctx):
        fact_calls.append((name, dict(args), dict(ctx)))
        return _facts(name, args, ctx)

    monkeypatch.setattr(thesis_resynth, "_http", fake_http)
    monkeypatch.setattr(thesis_resynth.tools, "execute", facts)
    monkeypatch.setattr(thesis_resynth.llm, "call_model", model)

    report = thesis_resynth.run_once()

    assert report["leased"] == 1
    assert report["generated"] == 1
    assert report["completed"] == 1
    assert held == [True]
    assert len(fact_calls) == 3
    assert all(ctx["asOf"] == JOB["asOf"] for _name, _args, ctx in fact_calls)
    event_args = next(args for name, args, _ctx in fact_calls if name == "get_ticker_events")
    assert event_args["since"] < JOB["asOf"]
    patch = next(kwargs["json_body"] for method, _url, kwargs in requests if method == "PATCH")
    assert patch["lastCheck"]["verdict"] == "challenged"
    assert patch["lastCheck"]["confidence"] == 72
    completion = [kwargs["json_body"] for method, url, kwargs in requests
                  if method == "POST" and url.endswith("/complete")]
    assert completion == [{"leaseToken": "lease-123", "outcome": "completed", "error": ""}]


def test_stub_result_is_requeued_and_never_persisted(monkeypatch, tmp_path):
    from app import thesis_resynth

    _configure(monkeypatch, tmp_path)
    calls = []
    lease_reads = 0

    def fake_http(method, url, **kwargs):
        nonlocal lease_reads
        calls.append((method, url, kwargs))
        if url.endswith("/_internal/resynth/lease"):
            lease_reads += 1
            return (200, {"job": dict(JOB)}) if lease_reads == 1 else (204, {})
        if method == "GET":
            return 200, {"thesis": {
                "id": "th_1", "ticker": "NVDA", "status": "active",
                "claim": "Demand growth remains durable.",
            }}
        if url.endswith("/complete"):
            return 200, {"ok": True}
        raise AssertionError((method, url))

    monkeypatch.setattr(thesis_resynth, "_http", fake_http)
    monkeypatch.setattr(thesis_resynth.tools, "execute", _facts)
    monkeypatch.setattr(thesis_resynth.llm, "call_model", lambda _messages: ("", "stub:offline"))

    report = thesis_resynth.run_once()

    assert report["generated"] == 1
    assert report["completed"] == 0
    assert report["retried"] == 1
    assert not any(method == "PATCH" for method, _url, _kwargs in calls)
    completion = next(kwargs["json_body"] for method, url, kwargs in calls
                      if method == "POST" and url.endswith("/complete"))
    assert completion["outcome"] == "queued"
    assert completion["leaseToken"] == "lease-123"
    assert "stub:offline" in completion["error"]


def test_existing_newer_check_acknowledges_without_generation(monkeypatch, tmp_path):
    from app import thesis_resynth

    _configure(monkeypatch, tmp_path)
    lease_reads = 0

    def fake_http(method, url, **kwargs):
        nonlocal lease_reads
        if url.endswith("/_internal/resynth/lease"):
            lease_reads += 1
            return (200, {"job": dict(JOB)}) if lease_reads == 1 else (204, {})
        if method == "GET":
            return 200, {"thesis": {
                "id": "th_1", "ticker": "NVDA", "status": "active",
                "claim": "Demand growth remains durable.",
                "lastCheck": {"at": JOB["enqueuedAt"] + 1, "verdict": "neutral"},
            }}
        if url.endswith("/complete"):
            return 200, {"ok": True}
        raise AssertionError((method, url))

    monkeypatch.setattr(thesis_resynth, "_http", fake_http)
    monkeypatch.setattr(thesis_resynth.tools, "execute",
                        lambda *_args: (_ for _ in ()).throw(AssertionError("facts fetched")))
    monkeypatch.setattr(thesis_resynth.llm, "call_model",
                        lambda *_args: (_ for _ in ()).throw(AssertionError("model called")))

    report = thesis_resynth.run_once()
    assert report["alreadyApplied"] == 1
    assert report["generated"] == 0
    assert report["completed"] == 1


def test_archived_after_enqueue_is_acknowledged_without_generation(monkeypatch, tmp_path):
    from app import thesis_resynth

    _configure(monkeypatch, tmp_path)
    lease_reads = 0

    def fake_http(method, url, **kwargs):
        nonlocal lease_reads
        if url.endswith("/_internal/resynth/lease"):
            lease_reads += 1
            return (200, {"job": dict(JOB)}) if lease_reads == 1 else (204, {})
        if method == "GET":
            return 200, {"thesis": {
                "id": "th_1", "ticker": "NVDA", "status": "archived",
                "claim": "Demand growth remains durable.",
            }}
        if url.endswith("/complete"):
            return 200, {"ok": True}
        raise AssertionError((method, url))

    monkeypatch.setattr(thesis_resynth, "_http", fake_http)
    monkeypatch.setattr(thesis_resynth.tools, "execute",
                        lambda *_args: (_ for _ in ()).throw(AssertionError("facts fetched")))
    monkeypatch.setattr(thesis_resynth.llm, "call_model",
                        lambda *_args: (_ for _ in ()).throw(AssertionError("model called")))

    report = thesis_resynth.run_once()
    assert report["skippedInactive"] == 1
    assert report["generated"] == 0
    assert report["completed"] == 1


def test_worker_has_no_fastapi_route_or_startup_import():
    from app import thesis_resynth
    from app.main import app

    assert "thesis_resynth" not in inspect.getsource(__import__("app.main", fromlist=["app"]))
    source = inspect.getsource(thesis_resynth)
    assert "while True" not in source
    assert "time.sleep" not in source
    assert "@app." not in source
    for route in app.routes:
        endpoint = getattr(route, "endpoint", None)
        module = inspect.getmodule(inspect.unwrap(endpoint)) if endpoint else None
        assert not (module and module.__name__.endswith("thesis_resynth"))
