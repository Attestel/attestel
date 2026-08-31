"""Strategy identity + persisted evaluator verdicts (docs/PAPER_EXECUTION_CONTRACT.md §4.3).

Run:  cd services/prediction && ./.venv/bin/python -m pytest -q tests/test_strategy_verdicts.py

What is being pinned here, and why each one matters:

  * `strategy_version(...)` is STABLE across processes. If it moved between runs, every stored
    verdict would read `current: false` forever and the paper engine would be permanently, silently
    refused for a reason that is not true.
  * ...and it MOVES when a constituent moves — including the PARAMETERS (`cost_bps`, the two
    thresholds, `allowShort`), which are arguments rather than module defaults. The masquerade
    repro below is the reason: an evaluation run under EVAL_* overrides used to be stamped with the
    DEFAULT strategy's id and served `current: true`, so a verdict about one strategy could be spent
    by another. That defeats gate 4 entirely.
  * `current` is computed from the SERVED RECORD'S own report, never from module defaults, and also
    requires the evaluator's methodology tag — a verdict from a superseded methodology may not be
    spent.
  * The verdict a run produced is actually written down, per horizon, with the report it came from.
  * /predict and /backtest serve the block on EVERY branch, and `None` when there is no verdict.
    Fail-closed depends on the absence being SERVED, not inferred from a missing key.

Nothing here calls a model, the network, or lightgbm.
"""
from __future__ import annotations

import json
import os
import subprocess
import sys

import numpy as np
import pytest

from app import main as main_mod
from app import verdicts as verdicts_mod
from app.evaluate import EvalConfig, Stream, TickerEval, _write_verdicts, run_strategy_version
from app.features import FEATURE_FRAME_POLICY
from app.strategy import (
    COST_BPS,
    DEFAULT_LOWER,
    DEFAULT_UPPER,
    default_strategy_version,
    strategy_inputs,
    strategy_version,
)

# The service defaults, as a keyword bundle — spelled out so a test never silently means something
# other than what it says.
DEFAULTS = dict(cost_bps=COST_BPS, upper=DEFAULT_UPPER, lower=DEFAULT_LOWER, allow_short=False)


def _report(cost_bps=COST_BPS, upper=DEFAULT_UPPER, lower=DEFAULT_LOWER, allow_short=False) -> dict:
    """The subset of a trained record's report that the strategy identity is derived from."""
    return {
        "passed": True,
        "thresholds": {"upper": upper, "lower": lower},
        "costBps": cost_bps,
        "allowShort": allow_short,
    }


def _stream(n: int = 4) -> Stream:
    return Stream(np.ones(n), np.full(n, 0.01), np.arange(n), "test")


# --------------------------------------------------------------------------- strategy_version

def test_strategy_version_is_stable_within_a_process():
    assert strategy_version(**DEFAULTS) == strategy_version(**DEFAULTS)
    assert default_strategy_version() == strategy_version(**DEFAULTS)


def test_strategy_version_is_stable_across_runs():
    """A fresh interpreter must produce the SAME id — no time, no randomness, no hash seed, no
    dict-ordering dependence. Run twice under different PYTHONHASHSEEDs to prove the last one."""
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    code = "from app.strategy import default_strategy_version; print(default_strategy_version())"

    def run(seed: str) -> str:
        env = {**os.environ, "PYTHONHASHSEED": seed}
        out = subprocess.run([sys.executable, "-c", code], cwd=root, env=env,
                             capture_output=True, text=True, check=True)
        return out.stdout.strip()

    assert run("0") == run("12345") == default_strategy_version()


def test_there_is_no_zero_argument_strategy_version():
    """A zero-argument form would silently mean "the defaults", which is exactly how a custom
    evaluation run came to be stamped as the default strategy."""
    with pytest.raises(TypeError):
        strategy_version()  # type: ignore[call-arg]


@pytest.mark.parametrize(
    "attr,value",
    [
        ("EXECUTION_CONTRACT_VERSION", "9.9.9"),
        ("MODEL_FEATURES", ["rsi"]),
    ],
)
def test_strategy_version_changes_when_a_code_constituent_changes(monkeypatch, attr, value):
    before = strategy_version(**DEFAULTS)
    monkeypatch.setattr(f"app.strategy.{attr}", value)
    assert strategy_version(**DEFAULTS) != before, f"{attr} is not part of the strategy identity"


@pytest.mark.parametrize(
    "over",
    [
        {"cost_bps": 0.0},
        {"upper": 0.90},
        {"lower": 0.10},
        {"allow_short": True},
    ],
)
def test_strategy_version_changes_when_a_parameter_changes(over):
    """`allowShort` is in here because it changes the POSITIONS: at the same probabilities a
    long-only run is flat where a shorting run is -1. It used not to be hashed at all."""
    assert strategy_version(**{**DEFAULTS, **over}) != strategy_version(**DEFAULTS)


def test_strategy_version_changes_when_a_model_parameter_changes(monkeypatch):
    before = strategy_version(**DEFAULTS)
    from app.strategy import LGB_PARAMS

    monkeypatch.setattr("app.strategy.LGB_PARAMS", {**LGB_PARAMS, "num_leaves": 31})
    assert strategy_version(**DEFAULTS) != before


def test_strategy_inputs_are_json_serialisable_and_named():
    blob = json.dumps(strategy_inputs(**DEFAULTS), sort_keys=True, default=str)
    for key in ("executionContract", "features", "lgbParams", "thresholds", "costBps", "allowShort"):
        assert key in blob


# --------------------------------------------------------------------------- verdict persistence

@pytest.fixture()
def verdict_dir(tmp_path, monkeypatch):
    """Point BOTH the writer and the reader at a temp EVAL_OUT_DIR."""
    monkeypatch.setattr(verdicts_mod, "EVAL_OUT_DIR", str(tmp_path))
    return tmp_path


def _cfg(out_dir, **over) -> EvalConfig:
    base = dict(
        universe=["NVDA", "GOOGL"], timeframe="1D", horizons=[5, 10], history_days=100,
        holdout_frac=0.2, cost_bps=COST_BPS, permutations=10, min_trades=100, seed=42,
        upper=DEFAULT_UPPER, lower=DEFAULT_LOWER, allow_short=False, analysis_url="",
        out_dir=str(out_dir),
    )
    base.update(over)
    return EvalConfig(**base)


def _run_report(**by_horizon) -> dict:
    return {
        "verdict": "NO EDGE",
        "generatedAt": "2026-08-23T12:00:00+00:00",
        "files": {"json": "report-20260823T120000Z.json"},
        "byHorizon": by_horizon,
    }


def _sufficient() -> dict:
    return {
        "nDates": 900, "minDates": 252,
        "holdoutDates": 180, "minHoldoutDates": 60,
        "nStreams": 30, "minTickers": 10,
        "configuredTickers": 30, "coverage": 1.0, "minCoverage": 0.7,
        "failedTickers": [], "skippedTickers": [],
    }


def test_write_verdicts_persists_one_record_per_evaluated_config(verdict_dir):
    cfg = _cfg(verdict_dir)
    report = _run_report(**{"5": {"verdict": "NO EDGE"}, "10": {"verdict": "INCONCLUSIVE"}})
    evals = [
        TickerEval("NVDA", 5, "tiingo", _stream(), _stream(), 300, 4),
        TickerEval("GOOGL", 10, "tiingo", _stream(), _stream(), 300, 4),
    ]

    written = _write_verdicts(report, cfg, evals, now=None)
    assert len(written) == 2

    rec = json.load(open(verdict_dir / "verdicts" / "NVDA_1D_5.json"))
    assert rec["verdict"] == "NO EDGE"
    assert rec["evaluatedAt"] == "2026-08-23T12:00:00+00:00"
    assert rec["report"] == "report-20260823T120000Z.json"   # the source report, by name
    assert rec["strategyVersion"] == run_strategy_version(cfg) == default_strategy_version()
    assert rec["method"] == verdicts_mod.EVALUATION_METHOD
    assert rec["dataPolicy"] == FEATURE_FRAME_POLICY
    # The verdict is pooled — the record must say so rather than read as a per-ticker judgement.
    assert rec["scope"] == "pooled-universe-h5"

    # ...and each horizon records ITS OWN verdict, not the run headline.
    other = json.load(open(verdict_dir / "verdicts" / "GOOGL_1D_10.json"))
    assert other["verdict"] == "INCONCLUSIVE"
    # ...and only for the configs actually evaluated.
    assert not (verdict_dir / "verdicts" / "NVDA_1D_10.json").exists()


def test_a_verdict_record_carries_the_sample_it_was_made_on(verdict_dir):
    """A verdict is a claim about evidence. Under `portfolio-v2` the record said nothing about how
    much evidence it rested on, so a verdict from four tickers and forty portfolio dates was
    indistinguishable on disk from one from thirty tickers and ten years."""
    cfg = _cfg(verdict_dir)
    suff = {"nDates": 900, "minDates": 252, "holdoutDates": 180, "minHoldoutDates": 60,
            "nStreams": 30, "minTickers": 10, "configuredTickers": 30, "coverage": 1.0,
            "minCoverage": 0.7, "failedTickers": [], "skippedTickers": []}
    report = _run_report(**{"5": {"verdict": "EDGE", "sufficiency": suff}})
    _write_verdicts(report, cfg, [TickerEval("NVDA", 5, "tiingo", _stream(), _stream(), 300, 4)],
                    now=None)

    rec = json.load(open(verdict_dir / "verdicts" / "NVDA_1D_5.json"))
    assert rec["sufficiency"]["nDates"] == 900
    assert rec["sufficiency"]["coverage"] == 1.0
    assert rec["sufficiency"]["minTickers"] == 10


def test_a_verdict_minted_under_the_weaker_sufficiency_rules_cannot_be_spent(verdict_dir):
    """`portfolio-v3` added the sample-sufficiency floors. The pooling math did not change, and
    neither did `strategy_version()` — so nothing but the METHOD TAG stops a `portfolio-v2` EDGE
    from being spent by gate 4, and it has to, because that verdict was allowed to rest on a sample
    these floors now refuse."""
    assert verdicts_mod.EVALUATION_METHOD == "portfolio-v4"
    verdicts_mod.write_verdict("NVDA", "1D", 5, verdict="EDGE",
                               evaluated_at="2026-08-23T12:00:00+00:00", report_file="r.json",
                               strategy_version=default_strategy_version(), method="portfolio-v2",
                               data_policy=FEATURE_FRAME_POLICY)
    block = verdicts_mod.evaluation_block(
        "NVDA", "1D", 5, _report(), data_policy=FEATURE_FRAME_POLICY
    )
    assert block["verdict"] == "EDGE"
    assert block["current"] is False, "a v2 verdict must not be spendable under current evidence rules"


def test_a_model_without_the_completed_bars_policy_cannot_spend_a_verdict(verdict_dir):
    verdicts_mod.write_verdict(
        "NVDA", "1D", 5, verdict="EDGE", evaluated_at="2026-08-25T12:00:00+00:00",
        report_file="r.json", strategy_version=default_strategy_version(), sufficiency=_sufficient(),
        data_policy=FEATURE_FRAME_POLICY,
    )
    block = verdicts_mod.evaluation_block("NVDA", "1D", 5, _report(), data_policy=None)
    assert block["dataPolicyCurrent"] is False
    assert block["expectedDataPolicy"] == FEATURE_FRAME_POLICY
    assert block["current"] is False


def test_an_unstamped_verdict_cannot_be_laundered_by_a_current_model(verdict_dir):
    verdicts_mod.write_verdict(
        "NVDA", "1D", 5, verdict="EDGE", evaluated_at="2026-08-25T12:00:00+00:00",
        report_file="r.json", strategy_version=default_strategy_version(), sufficiency=_sufficient(),
    )
    block = verdicts_mod.evaluation_block(
        "NVDA", "1D", 5, _report(), data_policy=FEATURE_FRAME_POLICY
    )
    assert block["dataPolicy"] is None
    assert block["servedDataPolicy"] == FEATURE_FRAME_POLICY
    assert block["dataPolicyCurrent"] is False
    assert block["current"] is False


def test_missing_verdict_is_none_not_a_default(verdict_dir):
    assert verdicts_mod.load_verdict("NVDA", "1D", 5) is None
    assert verdicts_mod.evaluation_block(
        "NVDA", "1D", 5, _report(), data_policy=FEATURE_FRAME_POLICY
    ) is None


def test_unparseable_verdict_is_none(verdict_dir):
    path = verdicts_mod.verdict_path("NVDA", "1D", 5)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write("{not json")
    assert verdicts_mod.evaluation_block(
        "NVDA", "1D", 5, _report(), data_policy=FEATURE_FRAME_POLICY
    ) is None


def test_evaluation_block_marks_a_matching_version_current(verdict_dir):
    verdicts_mod.write_verdict("NVDA", "1D", 5, verdict="EDGE",
                               evaluated_at="2026-08-23T12:00:00+00:00", report_file="r.json",
                               strategy_version=default_strategy_version(), sufficiency=_sufficient(),
                               data_policy=FEATURE_FRAME_POLICY)
    block = verdicts_mod.evaluation_block(
        "NVDA", "1D", 5, _report(), data_policy=FEATURE_FRAME_POLICY
    )
    assert block["verdict"] == "EDGE"
    assert block["current"] is True
    assert block["strategyVersion"] == block["expectedStrategyVersion"] == default_strategy_version()
    assert block["method"] == block["expectedMethod"] == verdicts_mod.EVALUATION_METHOD
    assert block["scope"] == "pooled-universe"
    assert block["report"] == "r.json"
    assert block["evaluatedAt"] == "2026-08-23T12:00:00+00:00"


def test_matching_edge_without_hard_floor_evidence_is_not_current(verdict_dir):
    weak = {**_sufficient(), "minDates": 0, "nDates": 40}
    verdicts_mod.write_verdict(
        "NVDA", "1D", 5, verdict="EDGE",
        evaluated_at="2026-08-23T12:00:00+00:00", report_file="r.json",
        strategy_version=default_strategy_version(), sufficiency=weak,
        data_policy=FEATURE_FRAME_POLICY,
    )
    block = verdicts_mod.evaluation_block(
        "NVDA", "1D", 5, _report(), data_policy=FEATURE_FRAME_POLICY
    )
    assert block["current"] is False
    assert block["evidenceCurrent"] is False
    assert any("minDates=0" in issue for issue in block["evidenceIssues"])
    assert any("nDates=40" in issue for issue in block["evidenceIssues"])


def test_evaluation_block_marks_a_stale_version_not_current(verdict_dir):
    verdicts_mod.write_verdict("NVDA", "1D", 5, verdict="EDGE",
                               evaluated_at="2026-08-23T12:00:00+00:00", report_file="r.json",
                               strategy_version="sv1-somethingelse", data_policy=FEATURE_FRAME_POLICY)
    block = verdicts_mod.evaluation_block(
        "NVDA", "1D", 5, _report(), data_policy=FEATURE_FRAME_POLICY
    )
    assert block["verdict"] == "EDGE"
    assert block["current"] is False, "a verdict about a different strategy must not read as current"


def test_evaluation_block_without_a_stored_version_is_not_current(verdict_dir):
    path = verdicts_mod.verdict_path("NVDA", "1D", 5)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        json.dump({"verdict": "EDGE"}, f)
    assert verdicts_mod.evaluation_block(
        "NVDA", "1D", 5, _report(), data_policy=FEATURE_FRAME_POLICY
    )["current"] is False


def test_a_verdict_from_a_superseded_methodology_is_not_current(verdict_dir):
    """A verdict produced by the old concatenated-streams pooling is about numbers this repo no
    longer computes. Matching the strategy version is not enough."""
    verdicts_mod.write_verdict("NVDA", "1D", 5, verdict="EDGE",
                               evaluated_at="2026-08-23T12:00:00+00:00", report_file="r.json",
                               strategy_version=default_strategy_version(), method="concat-v1",
                               data_policy=FEATURE_FRAME_POLICY)
    block = verdicts_mod.evaluation_block(
        "NVDA", "1D", 5, _report(), data_policy=FEATURE_FRAME_POLICY
    )
    assert block["method"] == "concat-v1"
    assert block["current"] is False


@pytest.mark.parametrize(
    "report",
    [
        None,
        {},
        {"costBps": 6.0, "allowShort": False},                       # no thresholds
        {"thresholds": {"upper": 0.55, "lower": 0.45}, "allowShort": False},   # no cost
        {"thresholds": {"upper": 0.55, "lower": 0.45}, "costBps": 6.0},        # no allowShort
        {"thresholds": {"upper": 0.55}, "costBps": 6.0, "allowShort": False},  # half a threshold
    ],
)
def test_a_record_missing_its_parameters_is_never_current(verdict_dir, report):
    """Fail closed: an identity we cannot compute is not an identity that matches."""
    verdicts_mod.write_verdict("NVDA", "1D", 5, verdict="EDGE",
                               evaluated_at="2026-08-23T12:00:00+00:00", report_file="r.json",
                               strategy_version=default_strategy_version(),
                               data_policy=FEATURE_FRAME_POLICY)
    assert verdicts_mod.evaluation_block(
        "NVDA", "1D", 5, report, data_policy=FEATURE_FRAME_POLICY
    )["current"] is False


# --------------------------------------------------------------------------- the masquerade repro

def test_a_custom_parameter_run_cannot_masquerade_as_the_default_strategy(verdict_dir):
    """The reviewer's exact reproduction.

    Run the evaluator at 0 bps, 0.90/0.10, shorting enabled. Its verdict must NOT be spendable by a
    record backtested under the defaults — and must be spendable by a record backtested under those
    same parameters. Before the fix the verdict was stamped with the DEFAULT id and served
    `current: true` against the default record, which is gate 4 defeated.
    """
    custom = _cfg(verdict_dir, cost_bps=0.0, upper=0.90, lower=0.10, allow_short=True)
    assert run_strategy_version(custom) != default_strategy_version()

    report = _run_report(**{"5": {"verdict": "EDGE", "sufficiency": _sufficient()}})
    _write_verdicts(report, custom, [TickerEval("NVDA", 5, "tiingo", _stream(), _stream(), 300, 4)],
                    now=None)

    against_default = verdicts_mod.evaluation_block(
        "NVDA", "1D", 5, _report(), data_policy=FEATURE_FRAME_POLICY
    )
    assert against_default["verdict"] == "EDGE"
    assert against_default["current"] is False, (
        "a verdict earned at 0 bps / 0.90-0.10 / shorting must not be current for the default "
        "6 bps long-only strategy"
    )

    # The converse: the record it WAS made about serves it as current.
    matching = verdicts_mod.evaluation_block(
        "NVDA", "1D", 5, _report(cost_bps=0.0, upper=0.90, lower=0.10, allow_short=True),
        data_policy=FEATURE_FRAME_POLICY,
    )
    assert matching["current"] is True


# --------------------------------------------------------------------------- served on the API

def test_predict_serves_the_evaluation_block_when_there_is_no_model(verdict_dir, monkeypatch):
    monkeypatch.setattr(main_mod, "load_model", lambda *a, **k: (None, None, None))

    resp = main_mod.predict("nvda", timeframe="1D", horizon=5)
    assert resp["signal"] is None
    assert "evaluation" in resp and resp["evaluation"] is None
    # Provenance of the frame is served too, and is null when no frame was fetched — a caller must
    # be able to tell "not fetched" from "fetched and clean" (contract §4, gate 1).
    assert resp["currentData"] is None

    verdicts_mod.write_verdict("NVDA", "1D", 5, verdict="NO EDGE",
                               evaluated_at="2026-08-23T12:00:00+00:00", report_file="r.json",
                               strategy_version=default_strategy_version(),
                               data_policy=FEATURE_FRAME_POLICY)
    resp = main_mod.predict("nvda", timeframe="1D", horizon=5)
    assert resp["evaluation"]["verdict"] == "NO EDGE"
    # No record => no parameters => nothing to match against => fail closed.
    assert resp["evaluation"]["current"] is False


def test_predict_marks_the_block_current_for_the_records_own_parameters(verdict_dir, monkeypatch):
    record = {"ticker": "NVDA", "timeframe": "1D", "horizon": 5, "report": _report(),
              "modelVersion": "v1", "trainedAt": "2026-08-01", "dataThrough": "2026-08-01",
              "trainedOnSynthetic": False, "dataPolicy": FEATURE_FRAME_POLICY}
    monkeypatch.setattr(main_mod, "load_model", lambda *a, **k: (None, None, dict(record)))
    verdicts_mod.write_verdict("NVDA", "1D", 5, verdict="EDGE",
                               evaluated_at="2026-08-23T12:00:00+00:00", report_file="r.json",
                               strategy_version=default_strategy_version(), sufficiency=_sufficient(),
                               data_policy=FEATURE_FRAME_POLICY)

    resp = main_mod.predict("nvda", timeframe="1D", horizon=5)
    assert resp["evaluation"]["current"] is True

    # ...and a record trained WITH shorting is a different strategy, so the same file is not current.
    shorting = {**record, "report": _report(allow_short=True)}
    monkeypatch.setattr(main_mod, "load_model", lambda *a, **k: (None, None, dict(shorting)))
    assert main_mod.predict("nvda", timeframe="1D", horizon=5)["evaluation"]["current"] is False


def test_backtest_serves_the_evaluation_block(verdict_dir, monkeypatch):
    record = {"ticker": "NVDA", "timeframe": "1D", "horizon": 5,
              "dataPolicy": FEATURE_FRAME_POLICY, "report": _report()}
    monkeypatch.setattr(main_mod, "load_record", lambda *a, **k: dict(record))

    resp = main_mod.backtest("nvda", timeframe="1D", horizon=5)
    assert resp["evaluation"] is None
    assert resp["report"] == _report()   # the record itself is untouched

    verdicts_mod.write_verdict("NVDA", "1D", 5, verdict="EDGE",
                               evaluated_at="2026-08-23T12:00:00+00:00", report_file="r.json",
                               strategy_version=default_strategy_version(), sufficiency=_sufficient(),
                               data_policy=FEATURE_FRAME_POLICY)
    served = main_mod.backtest("nvda", timeframe="1D", horizon=5)["evaluation"]
    assert served["verdict"] == "EDGE" and served["current"] is True


def test_backtest_summary_serves_the_cost_and_short_assumptions():
    """The paper engine records what the model was validated UNDER; it must not have to assume it."""
    summary = main_mod._backtest_summary(
        {"passed": True, "costBps": 6.0, "allowShort": True, "numTrades": 40}
    )
    assert summary["costBps"] == 6.0
    assert summary["allowShort"] is True
    assert main_mod._backtest_summary({})["allowShort"] is False
