from __future__ import annotations

import json
import pickle
import asyncio

import pytest
from fastapi import HTTPException
from app import main, store
from app.features import FEATURE_FRAME_POLICY
from app.verdicts import expected_strategy_version


def _report(*, passed: bool = True) -> dict:
    return {
        "passed": passed,
        "suspect": False,
        "thresholds": {"upper": 0.55, "lower": 0.45},
        "costBps": 6.0,
        "allowShort": False,
    }


def test_training_candidate_never_replaces_the_active_file_model(tmp_path, monkeypatch):
    monkeypatch.setattr(store, "MODELS_DIR", str(tmp_path))
    monkeypatch.setattr(store.db, "enabled", lambda: False)

    first = store.save_candidate(
        "NVDA", "1D", 5, {"model": 1}, _report(), False, "2026-08-20",
        features=["rsi"], calibrator={"cal": 1}, data_policy=FEATURE_FRAME_POLICY,
    )
    assert store.load_model("NVDA", "1D", 5) == (None, None, None)

    store.deploy_model_version(
        "NVDA", "1D", 5, first["modelVersion"], action="promote",
        actor_uid="admin-1", reason="bootstrap",
    )
    model, calibrator, active = store.load_model("NVDA", "1D", 5)
    assert model == {"model": 1}
    assert calibrator == {"cal": 1}
    assert active["modelVersion"] == first["modelVersion"]

    second = store.save_candidate(
        "NVDA", "1D", 5, {"model": 2}, _report(passed=False), False, "2026-08-25",
        features=["rsi"], data_policy=FEATURE_FRAME_POLICY,
    )
    assert store.was_model_deployed("NVDA", "1D", 5, second["modelVersion"]) is False
    model, _, still_active = store.load_model("NVDA", "1D", 5)
    assert model == {"model": 1}
    assert still_active["modelVersion"] == first["modelVersion"]
    assert second["parentModelVersion"] == first["modelVersion"]

    candidate_model, candidate_calibrator, candidate_record = store.load_version_model(
        "NVDA", "1D", 5, second["modelVersion"]
    )
    assert candidate_model == {"model": 2}
    assert candidate_calibrator is None
    assert candidate_record["modelVersion"] == second["modelVersion"]

    rows = store.list_model_records("NVDA", "1D", 5)
    assert {row["modelVersion"] for row in rows} == {
        first["modelVersion"], second["modelVersion"]
    }
    assert next(row for row in rows if row["active"])["modelVersion"] == first["modelVersion"]


def test_promote_then_rollback_is_only_a_pointer_change(tmp_path, monkeypatch):
    monkeypatch.setattr(store, "MODELS_DIR", str(tmp_path))
    monkeypatch.setattr(store.db, "enabled", lambda: False)
    first = store.save_candidate(
        "NVDA", "1D", 5, "first", _report(), False, "2026-08-20",
        data_policy=FEATURE_FRAME_POLICY,
    )
    store.deploy_model_version(
        "NVDA", "1D", 5, first["modelVersion"], action="promote",
        actor_uid="admin-1", reason="first",
    )
    assert store.was_model_deployed("NVDA", "1D", 5, first["modelVersion"]) is True
    second = store.save_candidate(
        "NVDA", "1D", 5, "second", _report(), False, "2026-08-25",
        data_policy=FEATURE_FRAME_POLICY,
    )
    store.deploy_model_version(
        "NVDA", "1D", 5, second["modelVersion"], action="promote",
        actor_uid="admin-1", reason="refresh",
    )
    assert store.load_model("NVDA", "1D", 5)[0] == "second"
    store.deploy_model_version(
        "NVDA", "1D", 5, first["modelVersion"], action="rollback",
        actor_uid="admin-1", reason="runtime regression",
    )
    assert store.load_model("NVDA", "1D", 5)[0] == "first"


def test_legacy_file_model_is_preserved_as_a_rollback_version(tmp_path, monkeypatch):
    monkeypatch.setattr(store, "MODELS_DIR", str(tmp_path))
    monkeypatch.setattr(store.db, "enabled", lambda: False)
    base = tmp_path / "NVDA_1D_5"
    base.mkdir()
    legacy_record = {
        "ticker": "NVDA", "timeframe": "1D", "horizon": 5,
        "modelVersion": "v20260820T120000", "trainedAt": "2026-08-20T12:00:00Z",
        "dataThrough": "2026-08-19", "report": _report(),
        "dataPolicy": FEATURE_FRAME_POLICY,
    }
    (base / "record.json").write_text(json.dumps(legacy_record))
    (base / "model.pkl").write_bytes(pickle.dumps({"model": "legacy", "calibrator": None}))

    candidate = store.save_candidate(
        "NVDA", "1D", 5, "new", _report(), False, "2026-08-25",
        data_policy=FEATURE_FRAME_POLICY,
    )
    assert store.load_model("NVDA", "1D", 5)[0] == "legacy"
    store.deploy_model_version(
        "NVDA", "1D", 5, candidate["modelVersion"], action="promote",
        actor_uid="admin-1", reason="refresh",
    )
    store.deploy_model_version(
        "NVDA", "1D", 5, legacy_record["modelVersion"], action="rollback",
        actor_uid="admin-1", reason="regression",
    )
    assert store.load_model("NVDA", "1D", 5)[0] == "legacy"


def test_promotion_gate_requires_clean_model_and_current_pooled_edge(monkeypatch):
    report = _report()
    record = {
        "ticker": "NVDA", "timeframe": "1D", "horizon": 5,
        "modelVersion": "candidate-1", "dataThrough": "2026-08-25",
        "trainedOnSynthetic": False, "dataPolicy": FEATURE_FRAME_POLICY,
        "strategyVersion": expected_strategy_version(report), "report": report,
    }
    monkeypatch.setattr(main, "evaluation_block", lambda *a, **k: {
        "verdict": "EDGE", "current": True, "evidenceCurrent": True,
    })
    eligible, gates, _ = main._promotion_gates(record)
    assert eligible is True
    assert all(gate["passed"] for gate in gates)

    older = {**record, "dataThrough": "2026-08-19"}
    active = {**record, "modelVersion": "active-1", "dataThrough": "2026-08-20"}
    eligible, gates, _ = main._promotion_gates(older, active)
    assert eligible is False
    assert next(g for g in gates if g["name"] == "refresh-is-not-older")["passed"] is False

    synthetic = {**record, "trainedOnSynthetic": True}
    eligible, gates, _ = main._promotion_gates(synthetic)
    assert eligible is False
    assert next(g for g in gates if g["name"] == "real-training-data")["passed"] is False

    monkeypatch.setattr(main, "evaluation_block", lambda *a, **k: {
        "verdict": "NO EDGE", "current": True, "evidenceCurrent": True,
    })
    eligible, gates, _ = main._promotion_gates(record)
    assert eligible is False
    assert next(g for g in gates if g["name"] == "pooled-evaluator-verdict")["passed"] is False


def test_rollback_cannot_bypass_promotion_for_a_never_deployed_candidate(monkeypatch):
    class Request:
        headers = {"X-Operator-Uid": "admin-1"}

        async def json(self):
            return {"reason": "try bypass"}

    monkeypatch.setattr(main, "load_version_record", lambda *a: {"modelVersion": "candidate-1"})
    monkeypatch.setattr(main, "load_record", lambda *a: None)
    monkeypatch.setattr(main, "was_model_deployed", lambda *a: False)
    monkeypatch.setattr(
        main, "deploy_model_version",
        lambda *a, **k: pytest.fail("a never-deployed candidate reached the rollback pointer write"),
    )
    with pytest.raises(HTTPException) as caught:
        asyncio.run(main._deployment_request(
            "NVDA", "candidate-1", "1D", 5, Request(), "rollback"
        ))
    assert caught.value.status_code == 409
