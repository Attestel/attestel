"""Opt-in PostgreSQL restart coverage for the production prediction repository."""
from __future__ import annotations

import json
import os
import time
from concurrent.futures import ThreadPoolExecutor

import pytest

from app import ablation, db, store, verdicts


@pytest.mark.skipif(not os.getenv("PREDICTION_TEST_DATABASE_URL"), reason="test database not configured")
def test_prediction_evidence_survives_database_reopen(monkeypatch):
    url = os.environ["PREDICTION_TEST_DATABASE_URL"]
    schema = f"test_prediction_{time.time_ns()}"
    monkeypatch.setenv("PREDICTION_DATABASE_URL", url)
    monkeypatch.setenv("PREDICTION_DATABASE_SCHEMA", schema)

    try:
        record = store.save_candidate(
            "NVDA", "1D", 5, {"kind": "model"}, {"passed": True}, False,
            "2026-08-25", features=["rsi"], calibrator={"kind": "calibrator"},
        )
        assert store.load_model("NVDA", "1D", 5) == (None, None, None)
        store.deploy_model_version(
            "NVDA", "1D", 5, record["modelVersion"], action="promote",
            actor_uid="admin-1", reason="test bootstrap",
        )
        model, calibrator, reloaded = store.load_model("NVDA", "1D", 5)
        assert model == {"kind": "model"}
        assert calibrator == {"kind": "calibrator"}
        assert reloaded["modelVersion"] == record["modelVersion"]
        assert reloaded["active"] is True
        assert reloaded["deploymentState"] == "active"
        registry = store.list_model_records("NVDA", "1D", 5)
        assert registry[0]["modelVersion"] == record["modelVersion"]
        assert registry[0]["wasDeployed"] is True

        assert db.acquire_automation_lease("worker-1", 600) is True
        assert db.acquire_automation_lease("worker-2", 600) is False
        assert db.automation_controller_status()["leaseHeld"] is True
        db.release_automation_lease("worker-1")
        trial = db.reserve_automation_trial(
            "NVDA", "1D", 5, record["modelVersion"], "2026-08-26T00:00:00+00:00"
        )
        assert trial is not None
        assert db.reserve_automation_trial(
            "NVDA", "1D", 5, record["modelVersion"], "2026-08-26T00:00:00+00:00"
        ) is None
        db.finish_automation_training(
            trial["id"], candidate_model_version=record["modelVersion"],
            strategy_version=record.get("strategyVersion"), data_through="2026-08-26",
        )
        db.mark_automation_trials_evaluating(
            [trial["id"]], "2026-08-26T22:00:00+00:00"
        )
        db.finish_automation_evaluation(
            trial["id"], evaluation={"verdict": {"verdict": "NO EDGE"}},
            finished_at="2026-08-26T22:05:00+00:00",
        )
        stored_trial = db.list_automation_trials(limit=1)[0]
        assert stored_trial["status"] == "evaluated"
        assert stored_trial["evaluation"]["verdict"]["verdict"] == "NO EDGE"
        assert db.automation_trial_baseline("NVDA", "1D", 5) == "2026-08-26"
        first_shadow = db.record_shadow_bar(
            trial["id"], bar_time="2026-08-26T00:00:00+00:00", close=100.0,
            source="tiingo", candidate_target=1, champion_target=0,
            candidate_probability=0.60, champion_probability=0.52,
            candidate_cost_bps=6.0, champion_cost_bps=6.0, min_paired_bars=1,
        )
        assert first_shadow["appended"] is True
        settled_shadow = db.record_shadow_bar(
            trial["id"], bar_time="2026-08-27T00:00:00+00:00", close=110.0,
            source="tiingo", candidate_target=1, champion_target=1,
            candidate_probability=0.61, champion_probability=0.57,
            candidate_cost_bps=6.0, champion_cost_bps=6.0, min_paired_bars=1,
        )
        assert settled_shadow["complete"] is True
        shadow_rows = db.load_shadow_observations(trial["id"])
        assert len(shadow_rows) == 1
        assert shadow_rows[0]["candidateNetReturn"] == pytest.approx(0.0994)
        assert shadow_rows[0]["championNetReturn"] == pytest.approx(0.0)
        db.set_automation_trial_status(trial["id"], "shadow-complete")
        assert db.list_automation_trials(limit=1)[0]["status"] == "shadow-complete"

        first_bootstrap = store.save_candidate(
            "GOOGL", "1D", 5, {"kind": "first"}, {"passed": True}, False,
            "2026-08-25",
        )
        second_bootstrap = store.save_candidate(
            "GOOGL", "1D", 5, {"kind": "second"}, {"passed": True}, False,
            "2026-08-25",
        )

        def promote_bootstrap(candidate):
            return store.deploy_model_version(
                "GOOGL", "1D", 5, candidate["modelVersion"], action="promote",
                actor_uid="admin-1", reason="concurrent bootstrap",
            )

        with ThreadPoolExecutor(max_workers=2) as pool:
            promotions = list(pool.map(promote_bootstrap, [first_bootstrap, second_bootstrap]))
        # Even before a deployment row exists, one transaction must observe the other's pointer.
        # Two `from=None` events would mean the initial promotion race is not serialized.
        assert sum(result["fromModelVersion"] is None for result in promotions) == 1

        verdicts.write_verdict(
            "NVDA", "1D", 5, verdict="EDGE", evaluated_at="2026-08-25T00:00:00Z",
            report_file="report-20260825.json", strategy_version="strategy-v1",
            sufficiency={"nDates": 252},
        )
        assert verdicts.load_verdict("NVDA", "1D", 5)["verdict"] == "EDGE"
        db.save_artifact("report-20260825.json", "application/json", b'{"verdict":"EDGE"}')
        assert db.latest_artifact("report-", ".json") == (
            "report-20260825.json", b'{"verdict":"EDGE"}'
        )

        raw_earnings = {
            "symbol": "NVDA",
            "quarterlyEarnings": [{
                "reportedDate": "2026-05-27", "reportedEPS": "1.30",
                "estimatedEPS": "1.20", "surprise": "0.10",
            }],
        }
        db.save_earnings_payload(
            "NVDA", "alpha-vantage", raw_earnings, vintage_status="unverified"
        )
        stored_earnings = db.load_earnings_payload("NVDA")
        assert stored_earnings["payload"] == raw_earnings
        assert stored_earnings["provider"] == "alpha-vantage"
        assert stored_earnings["vintageStatus"] == "unverified"
        assert len(stored_earnings["payloadSha256"]) == 64
        assert stored_earnings["lastSeenAt"]
        assert stored_earnings["coverageEnd"] == "2026-05-27"

        estimate_payload = {"estimates": [{
            "date": "2026-07-31", "eps_estimate_average": "0.95",
        }]}
        assert db.save_estimate_snapshot(
            "NVDA", "2026-07-31", "2026-08-26", "t_minus_1", "alpha-vantage",
            estimate_payload, consensus_eps=0.95,
            captured_at="2026-08-25T20:00:00+00:00", analyst_count=38,
        ) is True
        assert db.save_estimate_snapshot(
            "NVDA", "2026-07-31", "2026-08-26", "t_minus_1", "alpha-vantage",
            estimate_payload, consensus_eps=0.95,
            captured_at="2026-08-25T20:00:00+00:00", analyst_count=38,
        ) is False
        assert db.estimate_snapshot_exists("NVDA", "2026-07-31", "t_minus_1") is True
        snapshots = db.list_estimate_snapshots("NVDA")
        assert len(snapshots) == 1
        assert snapshots[0]["consensusEPS"] == 0.95
        assert snapshots[0]["capturedAt"] == "2026-08-25T20:00:00+00:00"
        assert len(snapshots[0]["payloadSha256"]) == 64

        db.save_earnings_text(
            "NVDA", "2026-05-27", "issuer-release", "Revenue and guidance text"
        )
        stored_text = db.load_earnings_text("NVDA", "2026-05-27")
        assert stored_text["source"] == "issuer-release"
        assert stored_text["text"] == "Revenue and guidance text"
        assert len(stored_text["textSha256"]) == 64

        result = {
            "experiment": "C", "split": "dev", "verdict": "EDGE",
            "generatedAt": "2026-08-25T00:00:00Z", "cutoffFingerprint": "1@abc",
            "metrics": {"runtimes": {}, "perHorizon": {
                "1d": {"scored": 40, "directionalAccuracy": 0.6,
                       "meanExcessReturn": 0.004},
            }},
        }
        ablation.write_verdict(result, "/not-a-durable-directory")
        stored_verdict = json.loads(db.load_artifact("ablation-verdict.json"))
        assert stored_verdict["verdicts"]["C|1d"]["validated"] is True
    finally:
        psycopg, sql = db._driver()
        with psycopg.connect(url, autocommit=True) as conn:
            conn.execute(sql.SQL("DROP SCHEMA IF EXISTS {} CASCADE").format(sql.Identifier(schema)))
