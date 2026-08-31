"""Probability calibration — Brier score, reliability curve, and isotonic regression (PAV).

Why: a LightGBM "63%" is often really a 53%. An advisor that says 55% and means 55% is more
valuable than one that says 63% and lies — the confidence number (and the human reading it) can
only be honest if the probability itself is honest.

Discipline (mirrors the walk-forward ethos in model.py):
  * the calibrator is fitted ONLY on walk-forward out-of-fold probabilities (out-of-sample by
    construction) and judged on the untouched holdout — never fitted on what it's judged on;
  * the trading THRESHOLDS keep operating on RAW probabilities, because that is what the gating
    backtest validated. Calibration changes what we *display and believe*, not what we *trade*;
  * hand-rolled numpy PAV, no sklearn dependency — deterministic, picklable, unit-testable.
"""
from __future__ import annotations

from dataclasses import dataclass

import numpy as np

MIN_FIT_ROWS = 50  # below this a fitted mapping is noise; better to show raw probabilities


@dataclass
class Calibrator:
    """A monotone piecewise-linear map raw->calibrated: np.interp over PAV block centers,
    clamped to the end values outside the fitted range. Picklable alongside the model."""
    x: np.ndarray  # block-center raw probabilities, strictly increasing
    y: np.ndarray  # calibrated probabilities, non-decreasing


def brier_score(prob: np.ndarray, y: np.ndarray) -> float:
    """Mean squared error of the probability vs the 0/1 outcome. 0.25 = uninformative coin flip;
    lower is better. The single best scalar for 'do the probabilities mean what they say'."""
    prob = np.asarray(prob, dtype=float)
    y = np.asarray(y, dtype=float)
    if len(prob) == 0:
        return 0.25
    return float(np.mean((prob - y) ** 2))


def reliability_curve(prob: np.ndarray, y: np.ndarray, bins: int = 10) -> list[dict]:
    """Binned 'predicted vs observed': for each probability bin, the mean predicted p(up) and the
    actual up-frequency. A calibrated model puts every point on the diagonal."""
    prob = np.asarray(prob, dtype=float)
    y = np.asarray(y, dtype=float)
    out: list[dict] = []
    edges = np.linspace(0.0, 1.0, bins + 1)
    for i in range(bins):
        lo, hi = edges[i], edges[i + 1]
        m = (prob >= lo) & (prob < hi) if i < bins - 1 else (prob >= lo) & (prob <= hi)
        if not m.any():
            continue
        out.append({
            "binLow": round(float(lo), 2),
            "binHigh": round(float(hi), 2),
            "meanPredicted": round(float(prob[m].mean()), 4),
            "observedUpRate": round(float(y[m].mean()), 4),
            "count": int(m.sum()),
        })
    return out


def fit_isotonic(prob: np.ndarray, y: np.ndarray) -> Calibrator | None:
    """Pool-adjacent-violators: the non-decreasing step fit of outcome on raw probability.
    Returns None when there is too little data to fit anything honest."""
    prob = np.asarray(prob, dtype=float)
    y = np.asarray(y, dtype=float)
    if len(prob) < MIN_FIT_ROWS or len(np.unique(y)) < 2:
        return None

    order = np.argsort(prob, kind="stable")
    xs, ts = prob[order], y[order]

    # PAV over blocks of (value-sum, weight, x-sum); merge while monotonicity is violated.
    val_sum: list[float] = []
    wgt: list[float] = []
    x_sum: list[float] = []
    for xi, ti in zip(xs, ts):
        val_sum.append(float(ti)); wgt.append(1.0); x_sum.append(float(xi))
        while len(val_sum) > 1 and val_sum[-2] / wgt[-2] > val_sum[-1] / wgt[-1]:
            val_sum[-2] += val_sum[-1]; wgt[-2] += wgt[-1]; x_sum[-2] += x_sum[-1]
            del val_sum[-1], wgt[-1], x_sum[-1]

    bx = np.array([xs_ / w for xs_, w in zip(x_sum, wgt)])   # block-center raw prob
    by = np.array([v / w for v, w in zip(val_sum, wgt)])     # block calibrated value
    keep = np.concatenate([[True], np.diff(bx) > 1e-12])     # np.interp needs increasing x
    return Calibrator(x=bx[keep], y=by[keep])


def apply_calibration(cal: Calibrator | None, prob):
    """Map raw probability(ies) through the fitted monotone curve; identity when cal is None.
    Accepts a scalar or an array; clamps to [0, 1]."""
    if cal is None:
        return prob
    mapped = np.interp(prob, cal.x, cal.y)
    return float(np.clip(mapped, 0.0, 1.0)) if np.isscalar(prob) else np.clip(mapped, 0.0, 1.0)
