"""The ablation ladder's calibration layer — Wave 5 Lane 5A.

The tests that matter are the refusals: scoring the split you fitted on is a LEAK and raises, and a
bucket below the sample floor comes back uncalibrated rather than smoothed. Everything else here is
arithmetic.

Nothing in this file observes a model. `confidenceBucket` values come from fixtures; what is being
tested is the counting, the leakage guard and the coverage bookkeeping.
"""
from __future__ import annotations

import json

import pytest

from app import ablation_calibration as cal


def record(*, split="dev", direction="bullish", bucket="high", realized=0.02,
           resolved=True, abstain=False, horizons=("20d",), experiment="C"):
    return {
        "experiment": experiment,
        "split": split,
        "forecast": {
            "confidenceBucket": bucket,
            "horizons": {h: {"direction": direction, "abstain": abstain} for h in horizons},
        },
        "outcomes": {h: {"realizedReturn": realized, "resolved": resolved} for h in horizons},
    }


def many(n, **kw):
    return [record(**kw) for _ in range(n)]


# ── the module is not the signal's calibrator ────────────────────────────────────────────────────

def test_this_module_did_not_replace_the_signals_calibrator():
    """`QWEN_IMPLEMENTATION_PLAN.md`'s Wave 5 table names `calibration.py` for this lane, but that
    file already exists and is Step 05's isotonic/PAV calibrator for the quant signal. Overwriting
    it would have silently replaced the calibrator behind the ONE directional output invariant #2
    permits. Both must still be importable, and they must be different objects."""
    from app import calibration as signal_calibration

    assert hasattr(signal_calibration, "fit_isotonic")
    assert hasattr(signal_calibration, "Calibrator")
    assert not hasattr(signal_calibration, "LeakageRefused")
    assert not hasattr(cal, "fit_isotonic")


# ── fitting ──────────────────────────────────────────────────────────────────────────────────────

def test_fit_counts_hits_per_direction_bucket_and_horizon():
    records = many(15, direction="bullish", bucket="high", realized=0.02) + \
        many(10, direction="bullish", bucket="high", realized=-0.02)
    fitted = cal.fit(records, split="dev", experiment="C")
    bin_ = fitted.bin_for("bullish", "high", "20d")
    assert bin_.n == 25 and bin_.correct == 15
    assert bin_.calibrated is True
    assert bin_.probability == pytest.approx(0.6)


def test_a_thin_bucket_is_uncalibrated_not_smoothed():
    """Three observations give an empirical frequency of 0, ⅓, ⅔ or 1 — a rounding of a handful of
    points, not a probability. Below the floor the answer is `None`, and every consumer excludes it
    and reports coverage instead."""
    fitted = cal.fit(many(3), split="dev", experiment="C")
    bin_ = fitted.bin_for("bullish", "high", "20d")
    assert bin_.n == 3
    assert bin_.calibrated is False
    assert bin_.probability is None
    assert fitted.coverage["calibratedObservations"] == 0


def test_abstentions_and_unresolved_and_flat_outcomes_never_enter_a_bucket():
    records = (
        many(5, abstain=True, direction="neutral")
        + many(5, resolved=False)
        + many(5, realized=0.0)              # a flat market is not a miss
    )
    fitted = cal.fit(records, split="dev", experiment="C")
    assert fitted.bins == {}


def test_fitting_across_two_splits_is_refused():
    with pytest.raises(ValueError, match="belongs to neither"):
        cal.fit(many(3, split="dev") + many(3, split="test"), split="dev", experiment="C")


# ── the leakage guard ────────────────────────────────────────────────────────────────────────────

def test_scoring_the_fit_split_raises_and_there_is_no_override():
    fitted = cal.fit(many(25), split="dev", experiment="C")
    with pytest.raises(cal.LeakageRefused) as excinfo:
        cal.apply_to(fitted, many(25), split="dev")
    assert "recovers the training frequencies" in str(excinfo.value)
    # There must be no escape hatch — a keyword that turns the guard off is the guard not existing.
    import inspect
    signature = inspect.signature(cal.apply_to)
    assert set(signature.parameters) == {"calibration", "records", "split"}


def test_out_of_sample_scoring_reports_coverage_ece_and_brier():
    fitted = cal.fit(many(20, realized=0.02) + many(20, realized=-0.02),
                     split="dev", experiment="C")   # p = 0.5 for bullish|high|20d
    scored = cal.apply_to(fitted, many(10, split="validation", realized=0.02),
                          split="validation")
    assert scored["fittedOnSplit"] == "dev" and scored["scoredSplit"] == "validation"
    assert scored["scored"] == 10 and scored["coverage"] == 1.0
    assert scored["brier"] == pytest.approx(0.25)     # predicted 0.5, observed 1.0, every time
    assert scored["expectedCalibrationError"] == pytest.approx(0.5)


def test_calls_in_an_uncalibrated_bucket_are_excluded_and_counted():
    fitted = cal.fit(many(25, bucket="high"), split="dev", experiment="C")
    scored = cal.apply_to(fitted, many(4, split="validation", bucket="low"), split="validation")
    assert scored["scored"] == 0
    assert scored["excluded"]["uncalibratedBin"] == 4
    assert scored["coverage"] == 0.0
    # `None`, never 0.0: a Brier of 0 is a PERFECT score and would read as the best possible result
    # for a set that scored nothing at all.
    assert scored["brier"] is None


def test_probability_for_returns_none_rather_than_inventing_one():
    assert cal.probability_for(None, record(), {"direction": "bullish"}, "20d") is None
    fitted = cal.fit(many(25), split="dev", experiment="C")
    assert cal.probability_for(fitted, record(), {"direction": "neutral"}, "20d") is None
    assert cal.probability_for(fitted, record(), {"direction": "bullish", "abstain": True},
                               "20d") is None


def test_no_arithmetic_mapping_from_score_to_probability_exists_in_this_module():
    """The invented mapping the header refuses. The failure mode is someone adding `(score + 1) / 2`
    in a hurry and a Brier score appearing that looks exactly like a real one.

    Comments and docstrings are STRIPPED before the check, so the module is free to name the banned
    expression in order to forbid it — a source-level gate that its own warning trips is a gate
    people delete.
    """
    import inspect
    import io
    import tokenize

    # NAME tokens compared for EQUALITY, not substring: `scored` and `brierSkillScore` are this
    # module's own vocabulary and must not trip a check aimed at reading the model's `score`.
    names = {
        token.string
        for token in tokenize.generate_tokens(io.StringIO(inspect.getsource(cal)).readline)
        if token.type == tokenize.NAME
    }
    assert "score" not in names, (
        "this module must never read a `score`: it maps an ORDINAL BUCKET to an empirical "
        "frequency, and touching the continuous score is how the invented mapping gets in")


# ── the reliability table and the skill score ────────────────────────────────────────────────────

def test_empty_reliability_bins_are_omitted_not_reported_as_zero():
    rows = [(0.15, True), (0.15, False), (0.85, True)]
    table = cal.reliability_table(rows)
    assert [cell["bin"] for cell in table] == ["[0.1,0.2)", "[0.8,0.9)"]
    assert all(cell["n"] > 0 for cell in table)


def test_brier_skill_score_propagates_none():
    assert cal.brier_skill_score(None, 0.5) is None
    assert cal.brier_skill_score(0.2, None) is None
    assert cal.brier_skill_score(0.2, 1.0) is None       # a degenerate base rate has no reference
    assert cal.brier_skill_score(0.125, 0.5) == pytest.approx(0.5)


# ── persistence ──────────────────────────────────────────────────────────────────────────────────

def test_a_written_calibration_round_trips_with_its_provenance(tmp_path):
    fitted = cal.fit(many(25), split="dev", experiment="C", now="2026-08-21T00:00:00Z")
    path = cal.write_calibration(fitted, str(tmp_path))
    body = json.loads(open(path, encoding="utf-8").read())
    assert body["fittedOnSplit"] == "dev" and body["fittedAt"] == "2026-08-21T00:00:00Z"

    loaded = cal.load_calibration(path)
    assert loaded.fitted_on_split == "dev"
    assert loaded.bin_for("bullish", "high", "20d").probability == 1.0
    # And the leakage guard survives the round trip — it is a property of the artefact, not of the
    # process that happened to fit it.
    with pytest.raises(cal.LeakageRefused):
        cal.apply_to(loaded, many(5, split="dev"), split="dev")


def test_a_missing_calibration_file_is_none_not_an_error(tmp_path):
    assert cal.load_calibration(str(tmp_path / "nope.json")) is None
