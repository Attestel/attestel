"""Tests for the in-app evaluation runner (app/evalrun.py + the /evaluate/* endpoints).

The subject is the RUNNER, never the verdict. Nothing here asserts what `evaluate.py` should
conclude — that is tested in test_evaluate.py and is deliberately untouched by this lane. What these
tests pin down is the machinery around it: single-flight, stale-lock reclaim, the no-parameters rule
(contract §4.3), and that a finished run's exit code is reported as the MEANING it has rather than as
a generic error.

`app.evaluate` itself is never executed here: `evalrun._spawn` is monkeypatched, so the suite stays
deterministic, offline, and fast.

Run:  cd services/prediction && ./.venv/bin/python -m pytest -q tests/test_evalrun.py
"""
import json
import os
import subprocess
import sys
import threading

import pytest
from fastapi.testclient import TestClient

from app import config as app_config
from app import evalrun, features, store, strategy, verdicts
from app.main import app


# --------------------------------------------------------------------------- fixtures

class FakeProc:
    """A subprocess stand-in whose exit is under the test's control. `pid` is this process's own,
    so the real `os.kill(pid, 0)` liveness check sees it as alive."""

    def __init__(self, code: int = 0, pid: int | None = None):
        self.pid = pid if pid is not None else os.getpid()
        self._code = code
        self._done = threading.Event()

    def wait(self) -> int:
        self._done.wait(timeout=10)
        return self._code

    def finish(self) -> None:
        self._done.set()


class FakeLog:
    def __init__(self, path: str):
        self.path = path
        self.closed = False

    def close(self) -> None:
        self.closed = True


@pytest.fixture
def out_dir(tmp_path, monkeypatch):
    """A private EVAL_OUT_DIR. `evalrun.out_dir()` reads config at call time, so this is all it takes."""
    d = tmp_path / "eval"
    d.mkdir()
    monkeypatch.setattr(app_config, "EVAL_OUT_DIR", str(d))
    return str(d)


@pytest.fixture
def spawned(monkeypatch):
    """Capture every _spawn call and hand back a controllable FakeProc. Ends the run cleanly so no
    reaper thread outlives the test."""
    procs: list[FakeProc] = []
    calls: list[tuple[str, str]] = []
    modules: list[str] = []
    exit_code = {"code": 0}

    def fake_spawn(log_file: str, cwd: str, module: str = "app.evaluate"):
        calls.append((log_file, cwd))
        modules.append(module)
        proc = FakeProc(code=exit_code["code"])
        procs.append(proc)
        return proc, FakeLog(log_file)

    monkeypatch.setattr(evalrun, "_spawn", fake_spawn)
    holder = type(
        "Spawned", (),
        {"procs": procs, "calls": calls, "modules": modules, "exit_code": exit_code},
    )()
    yield holder
    for p in procs:
        p.finish()


@pytest.fixture
def client(out_dir, spawned):
    with TestClient(app) as c:
        yield c


def _dead_pid() -> int:
    """A pid that is genuinely gone: a real process, started and reaped. Better than a made-up
    number, which a busy box could have handed to something real."""
    p = subprocess.Popen([sys.executable, "-c", "pass"])  # noqa: S603
    p.wait()
    return p.pid


def _wait_for(predicate, timeout: float = 5.0) -> bool:
    """Poll a predicate — the reaper runs on its own thread, so the outcome lands asynchronously."""
    deadline = threading.Event()
    for _ in range(int(timeout * 200)):
        if predicate():
            return True
        deadline.wait(0.005)
    return predicate()


# --------------------------------------------------------------------------- starting a run

def test_run_returns_202_and_creates_the_lock(client, out_dir, spawned):
    res = client.post("/evaluate/run")
    assert res.status_code == 202
    body = res.json()
    assert body["started"] is True
    assert body["startedAt"]
    assert body["logFile"].startswith("run-") and body["logFile"].endswith(".log")

    lock = json.load(open(os.path.join(out_dir, evalrun.LOCK_NAME)))
    assert lock["pid"] == spawned.procs[0].pid
    assert lock["startedAt"] == body["startedAt"]
    assert lock["logFile"] == body["logFile"]

    # The harness is started from the service directory so `-m app.evaluate` resolves.
    log_file, cwd = spawned.calls[0]
    assert cwd == evalrun.service_dir()
    assert os.path.dirname(log_file) == out_dir


def test_second_run_while_one_is_in_flight_is_409(client, spawned):
    assert client.post("/evaluate/run").status_code == 202

    res = client.post("/evaluate/run")
    assert res.status_code == 409
    body = res.json()
    assert body["started"] is False
    # The conflict carries the running job's status — "one is already going" is only actionable
    # next to what that one is doing.
    assert body["status"]["state"] == "running"
    assert len(spawned.procs) == 1, "a second harness was started"


def test_stale_lock_is_reclaimed_with_a_log_line(client, out_dir, spawned, capsys):
    stale = _dead_pid()
    with open(os.path.join(out_dir, evalrun.LOCK_NAME), "w") as f:
        json.dump({"pid": stale, "startedAt": "2026-08-01T00:00:00+00:00", "logFile": "run-old.log"}, f)

    res = client.post("/evaluate/run")
    assert res.status_code == 202, "a lock held by a dead process must not block the next run"
    assert "reclaiming stale lock" in capsys.readouterr().out, "a silent reclaim is invisible to an operator"

    lock = json.load(open(os.path.join(out_dir, evalrun.LOCK_NAME)))
    assert lock["pid"] == spawned.procs[0].pid


def test_an_empty_or_unreadable_lock_is_reclaimed(client, out_dir, spawned):
    # The window between creating the lock and writing its contents. It must not wedge future runs.
    open(os.path.join(out_dir, evalrun.LOCK_NAME), "w").close()
    assert client.post("/evaluate/run").status_code == 202


def test_pead_runner_has_its_own_module_and_status(client, out_dir, spawned):
    res = client.post("/evaluate-events/run")
    assert res.status_code == 202
    assert spawned.modules == ["app.evaluate_events"]
    assert os.path.exists(os.path.join(out_dir, evalrun.LOCK_NAME))
    status = client.get("/evaluate-events/status").json()
    assert status["state"] == "running"
    assert status["kind"] == "event"
    assert status["verdicts"] == [], "PEAD must never surface or write the price gate's verdicts"


def test_price_and_pead_runs_share_one_cpu_single_flight(client, spawned):
    assert client.post("/evaluate/run").status_code == 202
    blocked = client.post("/evaluate-events/run")
    assert blocked.status_code == 409
    assert blocked.json()["status"]["kind"] == "price"
    assert spawned.modules == ["app.evaluate"]
    assert len(spawned.procs) == 1


def test_estimate_collector_has_a_separate_bounded_runner(client, out_dir, spawned):
    res = client.post("/estimate-snapshots/run")
    assert res.status_code == 202
    assert spawned.modules == ["app.estimate_snapshots"]
    assert os.path.exists(os.path.join(out_dir, evalrun.ESTIMATE_LOCK_NAME))
    status = client.get("/estimate-snapshots/status").json()
    assert status["state"] == "running"
    assert status["kind"] == "estimate"
    assert status["verdicts"] == []

    blocked = client.post("/estimate-snapshots/run")
    assert blocked.status_code == 409
    assert blocked.json()["status"]["kind"] == "estimate"
    assert len(spawned.procs) == 1


# --------------------------------------------------------------------------- the no-parameters rule

@pytest.mark.parametrize("url", [
    "/evaluate/run?upper=0.9",
    "/evaluate/run?EVAL_COST_BPS=0",
    "/evaluate/run?anything=1",
])
def test_query_parameters_are_rejected_with_400(client, spawned, url):
    res = client.post(url)
    assert res.status_code == 400
    detail = res.json()["detail"]
    # The refusal names the rule, so the caller learns WHY rather than that it is unsupported.
    assert "no parameters" in detail
    assert "strategy version" in detail
    assert spawned.procs == [], "a rejected request must not start a run"


def test_body_fields_are_rejected_with_400(client, spawned):
    res = client.post("/evaluate/run", json={"upper": 0.9, "allowShort": True})
    assert res.status_code == 400
    assert "no parameters" in res.json()["detail"]
    assert spawned.procs == []


def test_pead_request_parameters_are_rejected_with_400(client, spawned):
    res = client.post("/evaluate-events/run?EVENT_MIN=1")
    assert res.status_code == 400
    assert "no parameters" in res.json()["detail"]
    assert spawned.procs == []


def test_estimate_request_parameters_are_rejected_with_400(client, spawned):
    res = client.post("/estimate-snapshots/run?maxCalls=999")
    assert res.status_code == 400
    assert "no parameters" in res.json()["detail"]
    assert spawned.procs == []


@pytest.mark.parametrize("payload", [None, {}])
def test_an_empty_body_is_not_a_parameter(client, spawned, payload):
    res = client.post("/evaluate/run") if payload is None else client.post("/evaluate/run", json=payload)
    assert res.status_code == 202


def test_unparseable_body_is_rejected(client, spawned):
    res = client.post("/evaluate/run", content=b"upper=0.9", headers={"Content-Type": "application/json"})
    assert res.status_code == 400
    assert spawned.procs == []


# --------------------------------------------------------------------------- status: exit meanings

@pytest.mark.parametrize("code,state,refusal,fragment", [
    (0, "done", False, "a verdict was produced"),
    (2, "failed", True, "synthetic"),
    (3, "failed", True, "nothing was fetched"),
])
def test_status_maps_exit_codes_to_their_meanings(client, out_dir, spawned, code, state, refusal, fragment):
    spawned.exit_code["code"] = code
    started = client.post("/evaluate/run").json()
    spawned.procs[0].finish()
    assert _wait_for(lambda: not os.path.exists(os.path.join(out_dir, evalrun.LOCK_NAME)))

    body = client.get("/evaluate/status").json()
    assert body["state"] == state
    assert body["exitCode"] == code
    assert fragment in body["exitMeaning"]
    # 2 and 3 are the harness REFUSING on purpose, not the machinery breaking. A UI must not have to
    # infer that from the number.
    assert body["refusal"] is refusal
    assert body["startedAt"] == started["startedAt"]
    assert body["finishedAt"]


def test_exit_zero_is_a_success_even_when_the_verdict_is_no_edge(client, out_dir, spawned):
    """NO EDGE / INCONCLUSIVE / SUSPECT are results, not failures — they arrive on exit code 0 and
    must read as `done`. This is the most likely outcome and the whole point of the harness."""
    with open(os.path.join(out_dir, "report-20260824T101500Z.json"), "w") as f:
        json.dump({
            "verdict": "NO EDGE", "meaning": "no tradeable edge was demonstrated",
            "generatedAt": "2026-08-24T10:15:00+00:00", "method": "portfolio-v3",
            "byHorizon": {"5": {"verdict": "NO EDGE"}, "10": {"verdict": "INCONCLUSIVE"}},
            "config": {"strategyVersion": "sv-abc"},
        }, f)
    spawned.exit_code["code"] = 0
    client.post("/evaluate/run")
    spawned.procs[0].finish()
    assert _wait_for(lambda: not os.path.exists(os.path.join(out_dir, evalrun.LOCK_NAME)))

    body = client.get("/evaluate/status").json()
    assert body["state"] == "done"
    assert body["refusal"] is False
    assert body["latestReport"]["verdict"] == "NO EDGE"


def test_status_is_idle_before_anything_has_run(client):
    body = client.get("/evaluate/status").json()
    assert body["state"] == "idle"
    assert body["exitCode"] is None
    assert body["verdicts"] == []
    assert body["latestReport"] is None


def test_status_reports_failed_when_the_process_vanished(client, out_dir):
    """A service restart mid-run leaves the lock behind. Status must say so rather than report
    `running` forever against a pid that is gone."""
    with open(os.path.join(out_dir, evalrun.LOCK_NAME), "w") as f:
        json.dump({"pid": _dead_pid(), "startedAt": "2026-08-24T09:00:00+00:00",
                   "logFile": "run-gone.log"}, f)

    body = client.get("/evaluate/status").json()
    assert body["state"] == "failed"
    assert "restarted mid-run" in body["note"]
    assert body["exitCode"] is None, "no exit was ever recorded; inventing one would be a lie"


# --------------------------------------------------------------------------- status: report + tail

def test_status_surfaces_the_newest_report(client, out_dir, spawned):
    for stamp, verdict in [("20260801T120000Z", "INCONCLUSIVE"), ("20260824T120000Z", "NO EDGE")]:
        with open(os.path.join(out_dir, f"report-{stamp}.json"), "w") as f:
            json.dump({
                "verdict": verdict, "generatedAt": f"{stamp}", "method": "portfolio-v3",
                "byHorizon": {
                    "5": {"verdict": verdict, "checklist": ["PASS  positive holdout", "FAIL  beats buy-and-hold"]},
                    "10": {"verdict": "NO EDGE", "checklist": ["FAIL  permutation p < 0.05"]},
                },
                "config": {"strategyVersion": "sv-1"},
            }, f)

    latest = client.get("/evaluate/status").json()["latestReport"]
    assert latest["file"] == "report-20260824T120000Z.json"
    assert latest["verdict"] == "NO EDGE"
    assert latest["byHorizon"]["5"]["verdict"] == "NO EDGE"
    assert latest["byHorizon"]["10"]["verdict"] == "NO EDGE"
    assert latest["byHorizon"]["5"]["checklist"][-1] == "FAIL  beats buy-and-hold"
    assert latest["byHorizon"]["10"]["checklist"] == ["FAIL  permutation p < 0.05"]
    assert latest["strategyVersion"] == "sv-1"


def test_pead_status_reads_only_event_reports_and_adapts_their_shape(client, out_dir):
    with open(os.path.join(out_dir, "report-20260826T120000Z.json"), "w") as f:
        json.dump({"verdict": "EDGE", "byHorizon": {}}, f)
    with open(os.path.join(out_dir, "event-report-20260826T120001Z.json"), "w") as f:
        json.dump({
            "verdict": "INCONCLUSIVE", "meaning": "estimate vintage unverified",
            "generatedAt": "2026-08-26T12:00:01Z", "method": "portfolio-v4+pead-abnormal-v2-forward-vintage",
            "byHorizon": {"10": {
                "verdict": "INCONCLUSIVE", "checklist": ["historical estimate vintage unverified"],
                "all": {"nDates": 400}, "holdout": {"nDates": 80},
                "sufficiency": {"nStreams": 20},
            }},
            "config": {"studyVersion": "pead-abnormal-v2-forward-vintage"},
        }, f)
    latest = client.get("/evaluate-events/status").json()["latestReport"]
    assert latest["file"].startswith("event-report-")
    assert latest["verdict"] == "INCONCLUSIVE"
    assert latest["studyVersion"] == "pead-abnormal-v2-forward-vintage"
    assert latest["byHorizon"]["10"]["pooled"]["holdout"]["nDates"] == 80


def test_status_returns_the_log_tail(client, out_dir, spawned):
    spawned.exit_code["code"] = 0
    started = client.post("/evaluate/run").json()
    with open(os.path.join(out_dir, started["logFile"]), "w") as f:
        f.write("\n".join(f"line {i}" for i in range(200)))
    spawned.procs[0].finish()
    assert _wait_for(lambda: not os.path.exists(os.path.join(out_dir, evalrun.LOCK_NAME)))

    tail = client.get("/evaluate/status").json()["logTail"]
    assert len(tail) == evalrun.LOG_TAIL_LINES
    assert tail[-1] == "line 199"


def test_estimate_status_surfaces_the_collector_result(client, out_dir, spawned):
    started = client.post("/estimate-snapshots/run").json()
    result = {
        "state": "done", "apiCalls": 3, "due": 2,
        "captured": [{"ticker": "NVDA", "stage": "t_minus_1"}],
        "skippedExisting": [], "actualsRefreshed": ["NVDA"],
        "providerErrors": [], "quotaExhausted": False,
    }
    with open(os.path.join(out_dir, started["logFile"]), "w") as f:
        json.dump(result, f)
    spawned.procs[0].finish()
    assert _wait_for(
        lambda: not os.path.exists(os.path.join(out_dir, evalrun.ESTIMATE_LOCK_NAME))
    )

    status = client.get("/estimate-snapshots/status").json()
    assert status["state"] == "done"
    assert status["collectorResult"] == result
    assert status["latestReport"] is None


# --------------------------------------------------------------------------- status: verdicts + current

def _write_served_record(models_dir, ticker, tf, horizon, *, cost_bps, upper, lower, allow_short):
    d = os.path.join(models_dir, f"{ticker}_{tf}_{horizon}")
    os.makedirs(d, exist_ok=True)
    with open(os.path.join(d, "record.json"), "w") as f:
        json.dump({
            "ticker": ticker, "timeframe": tf, "horizon": horizon,
            "trainedOnSynthetic": False,
            "dataPolicy": features.FEATURE_FRAME_POLICY,
            "report": {"passed": True, "costBps": cost_bps, "allowShort": allow_short,
                       "thresholds": {"upper": upper, "lower": lower}},
        }, f)


def _sufficient_evidence():
    return {
        "nDates": 900, "minDates": 252, "holdoutDates": 180, "minHoldoutDates": 60,
        "nStreams": 30, "minTickers": 10, "configuredTickers": 30,
        "coverage": 1.0, "minCoverage": 0.7,
    }


def test_status_surfaces_each_verdict_with_the_current_flag(client, out_dir, tmp_path, monkeypatch):
    """`current` is the flag paper gate 4 actually spends — computed by verdicts.evaluation_block
    against the SERVED RECORD, not recomputed here. The operator sees from the app whether gate 4
    would now pass."""
    models = tmp_path / "models"
    models.mkdir()
    monkeypatch.setattr(store, "MODELS_DIR", str(models))

    matching = strategy.strategy_version(6.0, 0.55, 0.45, False)
    _write_served_record(str(models), "NVDA", "1D", 5, cost_bps=6.0, upper=0.55, lower=0.45, allow_short=False)
    verdicts.write_verdict("NVDA", "1D", 5, verdict="NO EDGE", evaluated_at="2026-08-24T12:00:00+00:00",
                           report_file="report-20260824T120000Z.json", strategy_version=matching,
                           out_dir=out_dir, sufficiency=_sufficient_evidence(),
                           data_policy=features.FEATURE_FRAME_POLICY)

    # Same ticker, a record trained under DIFFERENT parameters: the stored verdict describes a
    # strategy this record does not run, so it may not be spent.
    _write_served_record(str(models), "NVDA", "1D", 10, cost_bps=0.0, upper=0.90, lower=0.10, allow_short=True)
    verdicts.write_verdict("NVDA", "1D", 10, verdict="EDGE", evaluated_at="2026-08-24T12:00:00+00:00",
                           report_file="report-20260824T120000Z.json", strategy_version=matching,
                           out_dir=out_dir, sufficiency=_sufficient_evidence(),
                           data_policy=features.FEATURE_FRAME_POLICY)

    rows = {(r["ticker"], r["horizon"]): r for r in client.get("/evaluate/status").json()["verdicts"]}
    assert rows[("NVDA", 5)]["verdict"] == "NO EDGE"
    assert rows[("NVDA", 5)]["current"] is True
    assert rows[("NVDA", 5)]["evaluatedAt"] == "2026-08-24T12:00:00+00:00"
    assert rows[("NVDA", 10)]["current"] is False
    assert rows[("NVDA", 10)]["expectedStrategyVersion"] != matching


def test_a_verdict_with_no_trained_record_is_never_current(client, out_dir, tmp_path, monkeypatch):
    """Fail closed: an identity that cannot be computed is not an identity that matches."""
    models = tmp_path / "models"
    models.mkdir()
    monkeypatch.setattr(store, "MODELS_DIR", str(models))
    verdicts.write_verdict("TSLA", "1D", 5, verdict="EDGE", evaluated_at="2026-08-24T12:00:00+00:00",
                           report_file="r.json", strategy_version="sv-whatever", out_dir=out_dir)

    row = client.get("/evaluate/status").json()["verdicts"][0]
    assert row["ticker"] == "TSLA"
    assert row["verdict"] == "EDGE"
    assert row["current"] is False


def test_an_unreadable_verdict_file_is_reported_not_swallowed(client, out_dir):
    os.makedirs(verdicts.verdict_dir(out_dir), exist_ok=True)
    with open(os.path.join(verdicts.verdict_dir(out_dir), "BROKEN_1D_5.json"), "w") as f:
        f.write("{not json")

    row = client.get("/evaluate/status").json()["verdicts"][0]
    assert row["unreadable"] is True
    assert row["current"] is False
