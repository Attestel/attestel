"""Model + walk-forward wiring test. Guarded by importorskip so the pure-python suite still passes
where LightGBM isn't installed; where it is, this proves the classifier actually learns a learnable
signal and the report is well-formed.

Run:  cd services/prediction && python -m pytest -q tests/test_model.py
"""
import numpy as np
import pandas as pd
import pytest

pytest.importorskip("lightgbm")

from app.features import MODEL_FEATURES, Dataset  # noqa: E402
from app.model import predict_prob, train_and_backtest, walk_forward  # noqa: E402


def _learnable_dataset(n: int = 500, seed: int = 0) -> Dataset:
    """A dataset where 'rsi' carries a real signal: high rsi -> up. The model should learn it and
    the walk-forward OOF hit-rate should beat a coin flip by a wide margin."""
    rng = np.random.default_rng(seed)
    f = rng.integers(0, 2, n)  # the hidden up/down driver, known at decision time
    idx = pd.bdate_range(end="2026-07-01", periods=n)
    X = pd.DataFrame(index=idx, columns=MODEL_FEATURES, dtype=float)
    for c in MODEL_FEATURES:
        X[c] = rng.normal(0, 1, n)
    X["rsi"] = np.where(f == 1, 70.0, 30.0) + rng.normal(0, 3, n)  # signal + noise
    y = pd.Series(f, index=idx)
    # a correct long earns +, a wrong one loses -, with noise (keeps Sharpe realistic, not absurd)
    ret_next = pd.Series(np.where(f == 1, 0.01, -0.01) + rng.normal(0, 0.02, n), index=idx)
    return Dataset(X=X, y=y, ret_next=ret_next, index=idx)


def test_walk_forward_learns_signal():
    ds = _learnable_dataset()
    oof, folds = walk_forward(ds.X, ds.y)
    assert folds >= 2
    # OOF accuracy on a learnable signal should clearly beat 0.5
    pred = (oof >= 0.5).astype(int)
    acc = (pred == ds.y.loc[oof.index]).mean()
    assert acc > 0.7


def test_train_and_backtest_report_shape():
    ds = _learnable_dataset()
    result = train_and_backtest(ds, timeframe="1D", horizon=5, allow_short=False, cost_bps=6.0)
    assert result is not None
    model, report, cal = result
    for k in ("directionalHitRate", "strategySharpe", "expectancy", "maxDrawdown",
              "numTrades", "suspect", "passed", "holdout", "featureImportances", "equityCurve",
              "calibration"):
        assert k in report, f"missing {k}"
    # calibration block is well-formed and fitted on this much data
    calrep = report["calibration"]
    assert calrep["fitted"] is (cal is not None)
    assert 0.0 <= calrep["oofBrier"] <= 1.0
    assert calrep["holdoutBrierRaw"] is not None
    # rsi should dominate feature importance (it carries the signal)
    imps = report["featureImportances"]
    assert imps["rsi"] == max(imps.values())
    # a confident up row scores higher than a confident down row
    up_row = ds.X.iloc[ds.y.to_numpy().argmax()].copy(); up_row["rsi"] = 75.0
    dn_row = up_row.copy(); dn_row["rsi"] = 25.0
    assert predict_prob(model, up_row, MODEL_FEATURES) > predict_prob(model, dn_row, MODEL_FEATURES)


def test_insufficient_data_returns_none():
    ds = _learnable_dataset(n=80)  # below MIN_ROWS
    assert train_and_backtest(ds, "1D", 5, False, 6.0) is None
