"""Backtest-math tests on crafted series with known answers (prediction phase / DoD #1).

If these numbers are wrong, every Sharpe/expectancy the UI shows is wrong — so they're pinned to
closed-form values, not the model.

Run:  cd services/prediction && python -m pytest -q tests/test_backtest.py
"""
import numpy as np

from app.backtest import (
    ANNUALIZATION,
    derive_positions,
    is_passing,
    max_drawdown,
    run_backtest,
)


def test_derive_positions_thresholds():
    p = np.array([0.9, 0.5, 0.1, 0.56, 0.44])
    long_only = derive_positions(p, upper=0.55, lower=0.45, allow_short=False)
    assert list(long_only) == [1, 0, 0, 1, 0]
    with_short = derive_positions(p, upper=0.55, lower=0.45, allow_short=True)
    assert list(with_short) == [1, 0, -1, 1, -1]


def test_constant_up_series_long():
    n = 50
    ret_next = np.full(n, 0.01)
    positions = np.ones(n)
    rep = run_backtest(ret_next, positions, cost_bps=0.0, timeframe="1D")
    assert rep["expectancy"] == 0.01
    assert rep["directionalHitRate"] == 1.0
    assert rep["precision"] == 1.0
    assert rep["numTrades"] == 1  # one entry, then held
    assert rep["maxDrawdown"] == 0.0
    assert abs(rep["totalReturn"] - (1.01 ** n - 1)) < 1e-3  # report rounds totalReturn to 4dp


def test_costs_reduce_return_on_turnover():
    # perfect long/short foresight, flipping every bar -> heavy turnover -> costs bite
    ret_next = np.array([0.02, -0.02] * 25)
    positions = np.sign(ret_next)  # perfect: +1 when up, -1 when down
    free = run_backtest(ret_next, positions, cost_bps=0.0, timeframe="1D")
    costed = run_backtest(ret_next, positions, cost_bps=10.0, timeframe="1D")
    assert free["directionalHitRate"] == 1.0
    assert free["expectancy"] > costed["expectancy"]          # costs strictly reduce edge
    assert free["numTrades"] == 50                             # opens a new position every bar
    assert costed["totalReturn"] < free["totalReturn"]


def test_max_drawdown_known():
    # equity: 1.0 -> 1.2 -> 0.9 -> 1.0 ; worst peak-to-trough = 1 - 0.9/1.2 = 0.25
    equity = np.array([1.0, 1.2, 0.9, 1.0])
    assert abs(max_drawdown(equity) - 0.25) < 1e-9


def test_sharpe_above_3_flagged_suspect():
    # tiny variance, steady positive -> absurd Sharpe -> must be flagged as probable leakage
    rng = np.random.default_rng(0)
    ret_next = 0.01 + rng.normal(0, 0.0005, 300)
    positions = np.ones(300)
    rep = run_backtest(ret_next, positions, cost_bps=0.0, timeframe="1D")
    assert rep["strategySharpe"] > 3.0
    assert rep["suspect"] is True
    assert is_passing(rep) is False   # suspect can never pass


def test_is_passing_gate():
    good = {"expectancy": 0.001, "strategySharpe": 1.2, "numTrades": 40, "suspect": False}
    assert is_passing(good) is True
    assert is_passing({**good, "numTrades": 10}) is False        # too few trades
    assert is_passing({**good, "strategySharpe": 3.5}) is False  # suspect Sharpe
    assert is_passing({**good, "expectancy": -0.001}) is False   # negative edge
    assert is_passing({**good, "strategySharpe": 0.0}) is False  # no measurable edge


def test_annualization_factors():
    assert ANNUALIZATION["1D"] == 252.0
    assert ANNUALIZATION["5m"] > ANNUALIZATION["1D"]
