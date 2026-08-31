"""Deterministic technical indicators — pure pandas/numpy, no pandas-ta dependency.

The original doc specifies pandas-ta, but pandas-ta has recurring numpy-compat breakage.
These hand-rolled implementations are equivalent for the v1 suite, dependency-light, and
unit-testable offline (no network, no model). Fixed suite per TECHNICAL_DOCUMENTATION.md §3.1,
extended in the deeper-analysis phase with stochastic / OBV / ADX / session-VWAP.
"""
from __future__ import annotations

import numpy as np
import pandas as pd


def rsi(close: pd.Series, length: int = 14) -> pd.Series:
    delta = close.diff()
    gain = delta.clip(lower=0.0)
    loss = -delta.clip(upper=0.0)
    # Wilder's smoothing
    avg_gain = gain.ewm(alpha=1 / length, min_periods=length, adjust=False).mean()
    avg_loss = loss.ewm(alpha=1 / length, min_periods=length, adjust=False).mean()
    rs = avg_gain / avg_loss.replace(0.0, np.nan)
    return 100 - (100 / (1 + rs))


def macd(close: pd.Series, fast: int = 12, slow: int = 26, signal: int = 9) -> pd.DataFrame:
    ema_fast = close.ewm(span=fast, adjust=False).mean()
    ema_slow = close.ewm(span=slow, adjust=False).mean()
    line = ema_fast - ema_slow
    sig = line.ewm(span=signal, adjust=False).mean()
    hist = line - sig
    return pd.DataFrame({"macd": line, "macd_signal": sig, "macd_hist": hist})


def _true_range(df: pd.DataFrame) -> pd.Series:
    high, low, close = df["high"], df["low"], df["close"]
    prev_close = close.shift(1)
    return pd.concat(
        [(high - low), (high - prev_close).abs(), (low - prev_close).abs()], axis=1
    ).max(axis=1)


def atr(df: pd.DataFrame, length: int = 14) -> pd.Series:
    return _true_range(df).ewm(alpha=1 / length, min_periods=length, adjust=False).mean()


def bollinger(close: pd.Series, length: int = 20, std: float = 2.0) -> pd.DataFrame:
    mid = close.rolling(length).mean()
    dev = close.rolling(length).std(ddof=0)
    return pd.DataFrame(
        {"bb_mid": mid, "bb_upper": mid + std * dev, "bb_lower": mid - std * dev}
    )


def stochastic(df: pd.DataFrame, k: int = 14, d: int = 3) -> pd.DataFrame:
    """Fast stochastic %K/%D. %K = position of close within the k-bar high/low range (0-100);
    %D = d-bar SMA of %K. Flat ranges (high==low) yield NaN rather than a divide-by-zero."""
    low_k = df["low"].rolling(k).min()
    high_k = df["high"].rolling(k).max()
    denom = (high_k - low_k).replace(0.0, np.nan)
    stoch_k = (100.0 * (df["close"] - low_k) / denom).clip(0.0, 100.0)
    stoch_d = stoch_k.rolling(d).mean()
    return pd.DataFrame({"stoch_k": stoch_k, "stoch_d": stoch_d})


def obv(close: pd.Series, volume: pd.Series) -> pd.Series:
    """On-balance volume: running sum of volume signed by the day's price direction. The absolute
    level is arbitrary (anchored at 0); only its slope/direction carries meaning."""
    direction = np.sign(close.diff().fillna(0.0))
    return (direction * volume).cumsum()


def adx(df: pd.DataFrame, length: int = 14) -> pd.Series:
    """Wilder's ADX (trend STRENGTH, direction-agnostic, 0-100). Computes +DI/-DI internally.
    High ADX = strong trend (either direction); low ADX = chop/range."""
    high, low = df["high"], df["low"]
    up_move = high.diff()
    down_move = -low.diff()
    plus_dm = pd.Series(np.where((up_move > down_move) & (up_move > 0), up_move, 0.0), index=df.index)
    minus_dm = pd.Series(np.where((down_move > up_move) & (down_move > 0), down_move, 0.0), index=df.index)

    atr_ = _true_range(df).ewm(alpha=1 / length, min_periods=length, adjust=False).mean()
    plus_di = 100.0 * plus_dm.ewm(alpha=1 / length, min_periods=length, adjust=False).mean() / atr_
    minus_di = 100.0 * minus_dm.ewm(alpha=1 / length, min_periods=length, adjust=False).mean() / atr_
    di_sum = (plus_di + minus_di).replace(0.0, np.nan)
    dx = 100.0 * (plus_di - minus_di).abs() / di_sum
    # Clip to the definitional [0,100]: a perfectly one-directional series gives DX==100 and float
    # division can nudge it a hair over, which downstream consumers treat as out-of-range.
    return dx.ewm(alpha=1 / length, min_periods=length, adjust=False).mean().clip(0.0, 100.0)


def session_vwap(df: pd.DataFrame) -> pd.Series:
    """Session-anchored VWAP: cumulative (typical price × volume) / cumulative volume, RESET at the
    start of each trading day. Only meaningful intraday — daily frames get NaN (see add_indicators).
    Requires a DatetimeIndex; anchors on the calendar date of each bar."""
    tp = (df["high"] + df["low"] + df["close"]) / 3.0
    day = pd.DatetimeIndex(df.index).normalize()
    cum_pv = (tp * df["volume"]).groupby(day).cumsum()
    cum_v = df["volume"].groupby(day).cumsum().replace(0.0, np.nan)
    return cum_pv / cum_v


def _infer_intraday(index: pd.Index) -> bool:
    """Infer intraday from bar spacing: median gap < 1 day => intraday. Daily/business frames are
    ~1 day apart. Used when the caller doesn't pass `intraday` explicitly."""
    idx = pd.DatetimeIndex(index)
    if len(idx) < 2:
        return False
    return bool(pd.Series(idx).diff().dropna().median() < pd.Timedelta(days=1))


def add_indicators(df: pd.DataFrame, intraday: bool | None = None) -> pd.DataFrame:
    """ticker OHLCV in -> same frame + fixed indicator columns.

    `intraday` controls session-VWAP: it is only computed for intraday frames (VWAP anchored to the
    trading session is meaningless on daily bars, so `vwap` is left NaN there). When None, it is
    inferred from the index spacing so existing callers keep working unchanged."""
    if intraday is None:
        intraday = _infer_intraday(df.index)
    out = df.copy()
    out["rsi"] = rsi(out["close"], 14)
    out = out.join(macd(out["close"], 12, 26, 9))
    out["sma50"] = out["close"].rolling(50).mean()
    out["sma200"] = out["close"].rolling(200).mean()
    out["ema20"] = out["close"].ewm(span=20, adjust=False).mean()
    out = out.join(bollinger(out["close"], 20, 2.0))
    out["atr"] = atr(out, 14)
    out["vol_sma20"] = out["volume"].rolling(20).mean()
    # deeper-analysis additions
    out = out.join(stochastic(out, 14, 3))
    out["obv"] = obv(out["close"], out["volume"])
    out["adx"] = adx(out, 14)
    out["vwap"] = session_vwap(out) if intraday else np.nan
    return out
