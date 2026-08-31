from __future__ import annotations

import pandas as pd

from app import autonomy


def _cfg(**overrides):
    values = {
        "enabled": True,
        "configs": (autonomy.ModelConfig("NVDA", "1D", 5),),
        "base_url": "http://prediction",
        "poll_seconds": 300,
        "lease_seconds": 600,
        "min_new_bars": 5,
        "max_trials_per_config": 3,
        "lookback_days": 1500,
        "shadow_min_paired_bars": 20,
    }
    values.update(overrides)
    return autonomy.AutomationConfig(**values)


def test_disabled_controller_has_no_side_effects(monkeypatch):
    monkeypatch.setattr(autonomy.db, "enabled", lambda: True)
    monkeypatch.setattr(
        autonomy, "_http_json",
        lambda *_args, **_kwargs: (_ for _ in ()).throw(AssertionError("outbound call")),
    )
    result = autonomy.run_once(_cfg(enabled=False))
    assert result["reason"] == "disabled"
    assert result["trained"] == []


def test_completed_bar_batch_trains_candidates_then_starts_one_evaluation(monkeypatch):
    state = {"trials": [], "calls": [], "released": None}
    monkeypatch.setattr(autonomy.db, "enabled", lambda: True)
    monkeypatch.setattr(autonomy.db, "acquire_automation_lease", lambda *_: True)
    monkeypatch.setattr(
        autonomy.db, "release_automation_lease",
        lambda token, error=None: state.update(released=(token, error)),
    )
    monkeypatch.setattr(autonomy.db, "automation_trial_baseline", lambda *_: "2026-08-01")
    monkeypatch.setattr(autonomy.db, "count_automation_trials", lambda *_: len(state["trials"]))

    def list_trials(*, statuses=None, limit=100):
        rows = list(state["trials"])
        return [row for row in rows if not statuses or row["status"] in statuses][:limit]

    monkeypatch.setattr(autonomy.db, "list_automation_trials", list_trials)

    def reserve(ticker, timeframe, horizon, champion, trigger):
        row = {
            "id": 1, "ticker": ticker, "timeframe": timeframe, "horizon": horizon,
            "championModelVersion": champion, "triggerBar": trigger, "status": "reserved",
        }
        state["trials"].append(row)
        return row

    monkeypatch.setattr(autonomy.db, "reserve_automation_trial", reserve)

    def finish_training(trial_id, **values):
        row = state["trials"][0]
        row.update({
            "status": "training-failed" if values.get("error") else "trained",
            "candidateModelVersion": values.get("candidate_model_version"),
            "strategyVersion": values.get("strategy_version"),
            "dataThrough": values.get("data_through"),
        })

    monkeypatch.setattr(autonomy.db, "finish_automation_training", finish_training)

    def mark_evaluating(ids, started_at):
        assert ids == [1]
        state["trials"][0].update(status="evaluating", evaluationStartedAt=started_at)

    monkeypatch.setattr(autonomy.db, "mark_automation_trials_evaluating", mark_evaluating)
    monkeypatch.setattr(
        autonomy, "load_record", lambda *_: {
            "modelVersion": "champion-1", "report": {"allowShort": False},
        },
    )
    dates = pd.date_range("2026-08-01", periods=6, freq="D", tz="UTC")
    monkeypatch.setattr(
        autonomy, "fetch_feature_frame",
        lambda *_args, **_kwargs: (pd.DataFrame({"close": range(6)}, index=dates), "tiingo", False),
    )

    def http(_cfg_value, method, path, **kwargs):
        state["calls"].append((method, path, kwargs.get("params")))
        if path == "/evaluate/status":
            return {"state": "idle", "verdicts": []}
        if path == "/train/NVDA":
            return {
                "trained": True, "candidate": True, "modelVersion": "candidate-1",
                "strategyVersion": "strategy-1", "dataThrough": "2026-08-06",
            }
        if path == "/evaluate/run":
            return {"started": True, "startedAt": "2026-08-06T22:00:00+00:00"}
        raise AssertionError(path)

    monkeypatch.setattr(autonomy, "_http_json", http)
    result = autonomy.run_once(_cfg())

    assert result["trained"][0]["candidateModelVersion"] == "candidate-1"
    assert result["evaluation"]["trialIds"] == [1]
    assert state["trials"][0]["status"] == "evaluating"
    assert state["released"][1] is None
    called_paths = [path for _method, path, _params in state["calls"]]
    assert called_paths == ["/evaluate/status", "/train/NVDA", "/evaluate/run"]
    assert not any("promote" in path or "rollback" in path for path in called_paths)


def test_synthetic_frame_is_refused_before_training(monkeypatch):
    monkeypatch.setattr(autonomy.db, "enabled", lambda: True)
    monkeypatch.setattr(autonomy.db, "acquire_automation_lease", lambda *_: True)
    monkeypatch.setattr(autonomy.db, "release_automation_lease", lambda *_args, **_kwargs: None)
    monkeypatch.setattr(autonomy.db, "list_automation_trials", lambda **_kwargs: [])
    monkeypatch.setattr(autonomy.db, "count_automation_trials", lambda *_: 0)
    monkeypatch.setattr(autonomy.db, "automation_trial_baseline", lambda *_: "2026-08-01")
    monkeypatch.setattr(
        autonomy, "fetch_feature_frame",
        lambda *_args, **_kwargs: (
            pd.DataFrame({"close": [1.0]}, index=["2026-08-06"]), "synthetic", True
        ),
    )
    calls = []

    def http(_cfg_value, method, path, **_kwargs):
        calls.append((method, path))
        return {"state": "idle", "verdicts": []}

    monkeypatch.setattr(autonomy, "_http_json", http)
    result = autonomy.run_once(_cfg())
    assert result["trained"] == []
    assert "synthetic" in result["skipped"][0]["reason"]
    assert calls == [("GET", "/evaluate/status")]


def test_finished_evaluation_is_attached_only_to_its_own_trial(monkeypatch):
    started = "2026-08-06T22:00:00+00:00"
    trial = {
        "id": 7, "ticker": "NVDA", "timeframe": "1D", "horizon": 5,
        "status": "evaluating", "evaluationStartedAt": started,
    }
    finished = []
    monkeypatch.setattr(autonomy.db, "enabled", lambda: True)
    monkeypatch.setattr(autonomy.db, "acquire_automation_lease", lambda *_: True)
    monkeypatch.setattr(autonomy.db, "release_automation_lease", lambda *_args, **_kwargs: None)
    monkeypatch.setattr(
        autonomy.db, "list_automation_trials",
        lambda *, statuses=None, limit=100: [trial] if statuses == ("evaluating",) else [],
    )
    monkeypatch.setattr(
        autonomy.db, "finish_automation_evaluation",
        lambda trial_id, **values: finished.append((trial_id, values)),
    )
    monkeypatch.setattr(autonomy.db, "count_automation_trials", lambda *_: 3)
    status = {
        "state": "done", "startedAt": started, "finishedAt": "2026-08-06T22:04:00+00:00",
        "latestReport": {"verdict": "NO EDGE"},
        "verdicts": [{
            "ticker": "NVDA", "timeframe": "1D", "horizon": 5,
            "verdict": "NO EDGE", "current": True,
        }],
    }
    monkeypatch.setattr(autonomy, "_http_json", lambda *_args, **_kwargs: status)
    result = autonomy.run_once(_cfg())
    assert result["evaluated"] == [7]
    assert finished[0][0] == 7
    assert finished[0][1]["evaluation"]["verdict"]["verdict"] == "NO EDGE"


def test_configuration_is_completed_bar_bounded(monkeypatch):
    monkeypatch.setenv("PREDICTION_AUTOMATION_ENABLED", "true")
    monkeypatch.setenv("PAPER_CONFIGS", "NVDA:1D:5,broken,GOOGL:1D:5,NVDA:1D:5")
    monkeypatch.setenv("PREDICTION_AUTOMATION_MIN_NEW_BARS", "0")
    monkeypatch.setenv("PREDICTION_AUTOMATION_MAX_TRIALS", "999")
    cfg = autonomy.AutomationConfig.from_env()
    assert [item.key for item in cfg.configs] == ["NVDA:1D:5", "GOOGL:1D:5"]
    assert cfg.min_new_bars == 1
    assert cfg.max_trials_per_config == 20
    assert cfg.shadow_min_paired_bars == 20


def test_controller_source_has_no_deployment_or_model_lane():
    source = open(autonomy.__file__, encoding="utf-8").read()
    assert "deploy_model_version" not in source
    assert "/promote" not in source
    assert "/rollback" not in source
    assert "app.llm" not in source


def test_status_attaches_forward_shadow_evidence_and_manual_policy(monkeypatch):
    trial = {
        "id": 12, "ticker": "NVDA", "timeframe": "1D", "horizon": 5,
        "status": "shadowing",
    }
    rows = [{
        "barTime": "2026-08-25T00:00:00+00:00",
        "nextBarTime": "2026-08-26T00:00:00+00:00",
        "candidateNetReturn": 0.02,
        "championNetReturn": 0.01,
    }]
    monkeypatch.setattr(autonomy.db, "enabled", lambda: True)
    monkeypatch.setattr(autonomy.db, "automation_controller_status", lambda: {"leaseHeld": False})
    monkeypatch.setattr(autonomy.db, "list_automation_trials", lambda **_kwargs: [trial.copy()])
    monkeypatch.setattr(autonomy.db, "load_shadow_observations", lambda trial_id: rows if trial_id == 12 else [])

    result = autonomy.status(_cfg(shadow_min_paired_bars=20))

    assert result["available"] is True
    assert result["policy"]["promotion"] == "manual-only"
    assert result["policy"]["thresholdTuning"] is False
    assert result["trials"][0]["shadow"]["pairedBars"] == 1
    assert result["trials"][0]["shadow"]["result"] == "unmeasured"
    assert result["trials"][0]["shadow"]["measured"] is False
