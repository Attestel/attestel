"""confidence_breakdown / confidence_score — pure math, no LightGBM required.

The breakdown must (a) multiply out to the shown confidence, (b) reproduce the historical
confidence_score values exactly, and (c) explain the canonical 'BUY 63% / confidence 2%' case.

Run:  cd services/prediction && python -m pytest -q tests/test_confidence.py
"""
from app.model import confidence_breakdown, confidence_score


def test_breakdown_multiplies_to_confidence():
    report = {"strategySharpe": 0.5, "numTrades": 31, "suspect": False}
    bd = confidence_breakdown(0.63, report)
    prod = bd["base"] * bd["sharpeFactor"] * bd["tradesFactor"] * bd["suspectFactor"]
    assert abs(prod - bd["confidence"]) < 0.005  # equal up to the 2-dp rounding of confidence
    assert bd["confidence"] == confidence_score(0.63, report)


def test_weak_backtest_yields_two_percent():
    # the canonical case: p(up)=0.63 -> Buy, but Sharpe 0.5 with 31 trades -> ~2% confidence
    bd = confidence_breakdown(0.63, {"strategySharpe": 0.5, "numTrades": 31, "suspect": False})
    assert bd["base"] == 0.26
    assert bd["sharpeFactor"] == 0.25
    assert bd["tradesFactor"] == 0.31
    assert bd["suspectFactor"] == 1.0
    assert bd["confidence"] == 0.02


def test_strong_backtest_gives_full_quality_credit():
    bd = confidence_breakdown(0.63, {"strategySharpe": 2.5, "numTrades": 150, "suspect": False})
    assert bd["sharpeFactor"] == 1.0  # capped: Sharpe >= 2 is full credit (2.5 is NOT extra credit)
    assert bd["tradesFactor"] == 1.0
    assert bd["confidence"] == 0.26  # base alone — quality no longer subtracts


def test_suspect_backtest_is_penalized():
    report = {"strategySharpe": 3.5, "numTrades": 150, "suspect": True}
    bd = confidence_breakdown(0.9, report)
    assert bd["suspectFactor"] == 0.3
    assert bd["confidence"] == round(0.8 * 1.0 * 1.0 * 0.3, 2)


def test_negative_sharpe_zeroes_confidence():
    bd = confidence_breakdown(0.75, {"strategySharpe": -0.4, "numTrades": 200, "suspect": False})
    assert bd["sharpeFactor"] == 0.0
    assert bd["confidence"] == 0.0
