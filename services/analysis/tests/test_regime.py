"""Deterministic regime tests — no network, no model (original doc §6 / DoD #3).

Builds a synthetic uptrend and downtrend frame and asserts the rules fire correctly.
Run:  cd services/analysis && python -m pytest -q
"""
import numpy as np
import pandas as pd

from app.indicators import add_indicators
from app.regime import detect_regime, swing_levels


def _frame(trend: str, n: int = 260) -> pd.DataFrame:
    dates = pd.bdate_range(end="2026-07-01", periods=n)
    drift = 0.004 if trend == "up" else -0.004
    close = 100 * np.exp(np.linspace(0, drift * n, n))
    df = pd.DataFrame(
        {
            "open": close * 0.999,
            "high": close * 1.01,
            "low": close * 0.99,
            "close": close,
            "volume": np.full(n, 300_000_000),
        },
        index=dates,
    )
    return add_indicators(df)


def test_uptrend_detected():
    reg = detect_regime(_frame("up"))
    assert reg["trend"] == "uptrend"
    assert reg["timeframe"] == "1D"  # default timeframe


def test_downtrend_detected():
    reg = detect_regime(_frame("down"))
    assert reg["trend"] == "downtrend"


def test_timeframe_is_echoed():
    # regime states which timeframe its bars came from (GAP-5); the math is identical.
    for tf in ("1D", "1H", "15m", "5m"):
        reg = detect_regime(_frame("up"), timeframe=tf)
        assert reg["timeframe"] == tf


def test_short_intraday_history_degrades_sma200():
    # < 200 bars: SMA200 is undefined, so trend falls back to the insufficient-history message
    # rather than raising — the graceful-degradation guarantee for short intraday windows.
    reg = detect_regime(_frame("up", n=120), timeframe="5m")
    assert reg["movingAverages"]["sma200"] is None
    assert "insufficient" in reg["trend"]


def test_key_levels_are_bracketing_and_numeric():
    df = _frame("up")
    levels = swing_levels(df)
    last = float(df["close"].iloc[-1])
    assert all(isinstance(x, float) for x in levels["support"] + levels["resistance"])
    assert all(s < last for s in levels["support"])
    assert all(r > last for r in levels["resistance"])


def test_regime_has_all_required_facts():
    reg = detect_regime(_frame("up"))
    for k in ("trend", "momentum", "macdState", "volatility", "priceVsBands", "volume", "keyLevels"):
        assert k in reg


def test_regime_has_deeper_facts():
    reg = detect_regime(_frame("up"))
    for k in ("trendStrength", "stochastic", "volumeTrend", "vwapState"):
        assert k in reg
    assert reg["vwapState"] is None                 # daily -> no session VWAP
    assert reg["volumeTrend"] == "accumulation"     # rising close + steady volume -> OBV climbs


def test_vwap_state_present_intraday():
    idx = pd.date_range(end="2026-07-01 20:00", periods=200, freq="15min")
    close = 100 + np.linspace(0, 5, 200)
    df = pd.DataFrame(
        {"open": close, "high": close + 0.3, "low": close - 0.3, "close": close,
         "volume": np.full(200, 1.0e6)},
        index=idx,
    )
    reg = detect_regime(add_indicators(df, intraday=True), timeframe="15m")
    assert reg["vwapState"] is not None
    assert "VWAP" in reg["vwapState"]
