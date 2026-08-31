"""Value-sanity tests for the deeper-analysis indicators — offline, deterministic.

Checks the properties that matter (bounds, direction, intraday-only VWAP) rather than exact
values, so the tests stay robust while still catching a broken formula.
"""
import numpy as np
import pandas as pd

from app.indicators import add_indicators, adx, obv, session_vwap, stochastic


def _daily(n: int = 260, trend: str = "up") -> pd.DataFrame:
    dates = pd.bdate_range(end="2026-07-01", periods=n)
    drift = 0.004 if trend == "up" else -0.004
    close = 100 * np.exp(np.linspace(0, drift * n, n))
    return pd.DataFrame(
        {"open": close * 0.999, "high": close * 1.01, "low": close * 0.99,
         "close": close, "volume": np.full(n, 3.0e8)},
        index=dates,
    )


def _intraday(n: int = 200, freq: str = "15min") -> pd.DataFrame:
    idx = pd.date_range(end="2026-07-01 20:00", periods=n, freq=freq)
    rng = np.random.default_rng(0)
    close = 100 + np.cumsum(rng.normal(0, 0.3, n))
    return pd.DataFrame(
        {"open": close, "high": close + 0.5, "low": close - 0.5, "close": close,
         "volume": rng.integers(1_000_000, 5_000_000, n).astype(float)},
        index=idx,
    )


def test_obv_rises_on_rising_series():
    df = _daily(trend="up")
    o = obv(df["close"], df["volume"])
    assert (o.diff().dropna() >= 0).all()   # never falls when price only rises
    assert o.iloc[-1] > o.iloc[0]           # net accumulation


def test_obv_falls_on_falling_series():
    df = _daily(trend="down")
    o = obv(df["close"], df["volume"])
    assert o.iloc[-1] < o.iloc[0]           # net distribution


def test_adx_in_0_100():
    a = adx(_daily()).dropna()
    assert len(a) > 0
    assert (a >= 0).all() and (a <= 100).all()


def test_stochastic_in_0_100():
    s = stochastic(_daily()).dropna()
    assert (s["stoch_k"].between(0, 100)).all()
    assert (s["stoch_d"].between(0, 100)).all()


def test_vwap_intraday_only_and_within_range():
    intr = add_indicators(_intraday(), intraday=True)
    assert intr["vwap"].notna().iloc[-1]
    # VWAP must sit within the observed price envelope
    assert intr["low"].min() <= intr["vwap"].iloc[-1] <= intr["high"].max()
    daily = add_indicators(_daily(), intraday=False)
    assert daily["vwap"].isna().all()       # session VWAP is meaningless on daily bars


def test_vwap_resets_each_session():
    # first bar of a session anchors VWAP to that bar's typical price
    df = _intraday()
    v = session_vwap(df)
    day = pd.DatetimeIndex(df.index).normalize()
    first_idx = df.index[day == day[-1]][0]
    tp0 = (df.loc[first_idx, "high"] + df.loc[first_idx, "low"] + df.loc[first_idx, "close"]) / 3.0
    assert abs(v.loc[first_idx] - tp0) < 1e-6


def test_add_indicators_infers_timeframe():
    assert add_indicators(_daily())["vwap"].isna().all()          # daily inferred
    assert add_indicators(_intraday())["vwap"].notna().iloc[-1]   # intraday inferred


def test_existing_indicators_preserved():
    out = add_indicators(_daily())
    for c in ("rsi", "macd", "macd_signal", "macd_hist", "sma50", "sma200",
              "ema20", "bb_upper", "bb_lower", "atr", "vol_sma20",
              "stoch_k", "stoch_d", "obv", "adx", "vwap"):
        assert c in out.columns
