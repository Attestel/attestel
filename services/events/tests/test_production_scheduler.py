"""The production clock is model-free, opt-in, and bounded by the existing dispatcher."""
from __future__ import annotations

import inspect

from app import production_scheduler as scheduler
from app.automation import EVENTS_OWNED


class StopAfterOnePass:
    def __init__(self):
        self.stopped = False
        self.waited = []

    def is_set(self):
        return self.stopped

    def wait(self, seconds):
        self.waited.append(seconds)
        self.stopped = True


def test_disabled_scheduler_returns_without_dispatch(monkeypatch):
    monkeypatch.setenv("PRODUCTION_SCHEDULER_ENABLED", "false")
    monkeypatch.setattr(scheduler, "dispatch", lambda **_kwargs: (_ for _ in ()).throw(
        AssertionError("disabled scheduler dispatched work")
    ))
    output = []
    assert scheduler.run_forever(emit=output.append) == 0
    assert '"reason": "disabled"' in output[0]


def test_enabled_scheduler_dispatches_only_events_owned_lanes(monkeypatch):
    monkeypatch.setenv("PRODUCTION_SCHEDULER_ENABLED", "true")
    monkeypatch.setenv("PRODUCTION_SCHEDULER_POLL_SECONDS", "17")
    calls = []

    def fake_dispatch(**kwargs):
        calls.append(kwargs)
        return {"enabled": True, "lanes": []}

    monkeypatch.setattr(scheduler, "dispatch", fake_dispatch)
    stop = StopAfterOnePass()
    assert scheduler.run_forever(stop_event=stop, emit=lambda _line: None) == 0
    assert calls == [{"lanes": EVENTS_OWNED, "trigger": "production-scheduler"}]
    assert stop.waited == [17]


def test_scheduler_survives_a_transient_dispatch_failure(monkeypatch):
    monkeypatch.setenv("PRODUCTION_SCHEDULER_ENABLED", "true")
    monkeypatch.setattr(scheduler, "dispatch", lambda **_kwargs: (_ for _ in ()).throw(
        RuntimeError("database restarting")
    ))
    output = []
    assert scheduler.run_forever(stop_event=StopAfterOnePass(), emit=output.append) == 0
    assert "schedulerError" in output[0]
    assert "database restarting" in output[0]


def test_scheduler_has_no_model_worker_seam():
    source = inspect.getsource(scheduler)
    assert "services.llm" not in source
    assert "LANE_ENRICH" not in source
    assert "LANE_RESYNTH" not in source
    assert "LLM_URL" not in source
    assert "EVENTS_OWNED" in source
