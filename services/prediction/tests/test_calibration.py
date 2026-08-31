"""calibration.py — pure numpy, no model dependency: Brier, reliability, PAV isotonic.

Run:  cd services/prediction && python -m pytest -q tests/test_calibration.py
"""
import numpy as np

from app.calibration import (
    MIN_FIT_ROWS,
    apply_calibration,
    brier_score,
    fit_isotonic,
    reliability_curve,
)


def _overconfident(n=2000, seed=0):
    """Raw probs that are systematically too extreme: true p(up) = 0.5 + 0.4*(raw-0.5).
    A raw '0.9' is really a 0.66 — exactly the failure mode calibration must fix."""
    rng = np.random.default_rng(seed)
    raw = rng.uniform(0.05, 0.95, n)
    true_p = 0.5 + 0.4 * (raw - 0.5)
    y = (rng.uniform(0, 1, n) < true_p).astype(float)
    return raw, y


def test_brier_basics():
    y = np.array([1.0, 0.0, 1.0, 0.0])
    assert brier_score(np.array([1.0, 0.0, 1.0, 0.0]), y) == 0.0     # perfect
    assert brier_score(np.full(4, 0.5), y) == 0.25                   # coin flip
    assert brier_score(np.array([]), np.array([])) == 0.25           # empty -> uninformative


def test_reliability_curve_diagonal_when_calibrated():
    rng = np.random.default_rng(1)
    prob = rng.uniform(0, 1, 20000)
    y = (rng.uniform(0, 1, 20000) < prob).astype(float)              # perfectly calibrated
    for b in reliability_curve(prob, y):
        assert abs(b["meanPredicted"] - b["observedUpRate"]) < 0.05


def test_isotonic_fixes_overconfidence():
    raw, y = _overconfident()
    cal = fit_isotonic(raw, y)
    assert cal is not None
    assert np.all(np.diff(cal.y) >= -1e-12)                          # monotone by construction
    # held-out sample from the SAME miscalibrated process: calibrated Brier must beat raw
    raw2, y2 = _overconfident(seed=99)
    assert brier_score(apply_calibration(cal, raw2), y2) < brier_score(raw2, y2)
    # an overconfident 0.9 gets pulled toward its honest ~0.66
    assert apply_calibration(cal, 0.9) < 0.8


def test_apply_preserves_order_and_clips():
    raw, y = _overconfident()
    cal = fit_isotonic(raw, y)
    probs = np.array([0.1, 0.3, 0.5, 0.7, 0.9])
    out = apply_calibration(cal, probs)
    assert np.all(np.diff(out) >= -1e-12)                            # order preserved
    assert np.all((out >= 0.0) & (out <= 1.0))
    assert isinstance(apply_calibration(cal, 0.63), float)           # scalar in -> scalar out


def test_identity_when_unfitted():
    assert fit_isotonic(np.full(MIN_FIT_ROWS - 1, 0.6), np.ones(MIN_FIT_ROWS - 1)) is None
    assert apply_calibration(None, 0.63) == 0.63
