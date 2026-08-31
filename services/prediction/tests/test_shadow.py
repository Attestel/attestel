from __future__ import annotations

import numpy as np
import pandas as pd

from app import shadow
from app.features import FEATURE_FRAME_POLICY


def _row(bar, candidate, champion, next_bar=None):
    return {
        "barTime": bar,
        "nextBarTime": next_bar,
        "candidateNetReturn": candidate,
        "championNetReturn": champion,
    }


def test_shadow_metrics_stay_unmeasured_below_paired_floor():
    result = shadow.metrics([
        _row("2026-08-01", 0.01, 0.005, "2026-08-02"),
        _row("2026-08-02", -0.002, 0.001, "2026-08-03"),
    ], 5)
    assert result["pairedBars"] == 2
    assert result["measured"] is False
    assert result["result"] == "unmeasured"
    assert result["candidateAheadBars"] == 1
    assert result["championAheadBars"] == 1


def test_shadow_metrics_compare_compounded_returns_only_after_floor():
    rows = [
        _row("2026-08-01", 0.01, 0.002, "2026-08-02"),
        _row("2026-08-02", 0.01, 0.002, "2026-08-03"),
        _row("2026-08-03", -0.001, 0.0, "2026-08-04"),
    ]
    result = shadow.metrics(rows, 3)
    assert result["measured"] is True
    assert result["result"] == "candidate-ahead"
    assert result["candidateTotalReturn"] > result["championTotalReturn"]
    assert result["lastSettledBar"] == "2026-08-04"


def test_version_score_uses_frozen_record_thresholds_and_cost(monkeypatch):
    class Model:
        def predict_proba(self, _frame):
            return np.asarray([[0.38, 0.62]])

    monkeypatch.setattr(shadow, "load_version_model", lambda *_args: (
        Model(), None, {
            "trainedOnSynthetic": False,
            "dataPolicy": FEATURE_FRAME_POLICY,
            "features": ["rsi"],
            "report": {
                "thresholds": {"upper": 0.60, "lower": 0.40},
                "costBps": 7.0, "allowShort": False,
            },
        }
    ))
    result = shadow._score_version(
        "NVDA", "1D", 5, "candidate-1", pd.Series({"rsi": 55.0})
    )
    assert result.target == 1
    assert result.probability == 0.62
    assert result.cost_bps == 7.0


def test_advance_trial_writes_pair_and_never_deploys(monkeypatch):
    candidate = shadow.VersionScore("candidate-1", 1, 0.61, 6.0)
    champion = shadow.VersionScore("champion-1", 0, 0.52, 6.0)
    monkeypatch.setattr(shadow, "score_pair", lambda *_args, **_kwargs: {
        "barTime": "2026-08-27T00:00:00+00:00", "close": 100.0, "source": "tiingo",
        "candidate": candidate, "champion": champion,
    })
    recorded = []
    statuses = []
    monkeypatch.setattr(
        shadow.db, "record_shadow_bar",
        lambda trial_id, **values: recorded.append((trial_id, values)) or {
            "appended": True, "settled": False, "complete": False, "pairedBars": 0,
        },
    )
    monkeypatch.setattr(shadow.db, "load_shadow_observations", lambda _trial_id: [])
    monkeypatch.setattr(
        shadow.db, "set_automation_trial_status",
        lambda trial_id, status, error=None: statuses.append((trial_id, status, error)),
    )
    result = shadow.advance_trial(
        {
            "id": 9, "ticker": "NVDA", "timeframe": "1D", "horizon": 5,
            "candidateModelVersion": "candidate-1", "championModelVersion": "champion-1",
            "status": "evaluated",
        },
        lookback_days=1500, min_paired_bars=20,
    )
    assert result["summary"]["result"] == "unmeasured"
    assert recorded[0][1]["candidate_target"] == 1
    assert recorded[0][1]["champion_target"] == 0
    assert statuses == [(9, "shadowing", None)]


def test_shadow_source_has_no_serving_or_official_book_mutation():
    source = open(shadow.__file__, encoding="utf-8").read()
    assert "deploy_model_version" not in source
    assert "/promote" not in source
    assert "/paper/reset" not in source
    assert "journal" not in source
