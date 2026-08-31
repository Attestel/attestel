"""Regime detection — pure rules, no ML (original doc §3.2).

Collapses the latest indicator row into plain-language facts the LLM can reason over, and
computes ALL key levels deterministically (original GAP-2: never let the model invent a number
it could be handed). Unit-testable offline against saved fixtures.
"""
from __future__ import annotations

from typing import Any

import numpy as np
import pandas as pd


def _f(x: Any) -> float | None:
    if x is None or (isinstance(x, float) and np.isnan(x)):
        return None
    return round(float(x), 4)


def swing_levels(df: pd.DataFrame, lookback: int = 60, window: int = 5) -> dict:
    """Support/resistance from recent local swing highs/lows (computed, not LLM-derived)."""
    recent = df.tail(lookback)
    highs, lows = recent["high"].to_numpy(), recent["low"].to_numpy()
    res, sup = [], []
    for i in range(window, len(recent) - window):
        seg_h, seg_l = highs[i - window : i + window + 1], lows[i - window : i + window + 1]
        if highs[i] == seg_h.max():
            res.append(round(float(highs[i]), 2))
        if lows[i] == seg_l.min():
            sup.append(round(float(lows[i]), 2))
    last = float(recent["close"].iloc[-1])
    resistance = sorted({r for r in res if r > last})[:3]
    support = sorted({s for s in sup if s < last}, reverse=True)[:3]
    return {"support": support, "resistance": resistance}


def detect_regime(df: pd.DataFrame, n_recent: int = 5, timeframe: str = "1D") -> dict:
    """Latest indicator-enriched frame -> dict of facts + computed key levels.

    `timeframe` (1D/1H/15m/5m) is echoed back so every read states which bars it came from
    (resolves the original doc's GAP-5). The indicator/regime math is identical across timeframes;
    long windows (SMA200 etc.) already degrade to None on short intraday history."""
    last = df.iloc[-1]
    close = float(last["close"])
    sma50, sma200, ema20 = _f(last["sma50"]), _f(last["sma200"]), _f(last["ema20"])
    rsi_v = _f(last["rsi"])
    macd_hist = _f(last["macd_hist"])
    atr_v, atr_avg = _f(last["atr"]), _f(df["atr"].tail(20).mean())

    # Trend
    if sma200 is not None:
        trend = "uptrend" if close > sma200 else "downtrend"
    else:
        trend = "insufficient history for SMA200"

    # Short-term trend / MA structure
    if sma50 is not None and sma200 is not None:
        cross = "50>200 (golden-cross structure)" if sma50 > sma200 else "50<200 (death-cross structure)"
        short = f"close {'above' if close > sma50 else 'below'} SMA50; {cross}"
    else:
        short = "insufficient history"

    # Momentum
    if rsi_v is None:
        momentum = "unknown"
    elif rsi_v < 30:
        momentum = f"oversold (RSI {rsi_v})"
    elif rsi_v > 70:
        momentum = f"overbought (RSI {rsi_v})"
    else:
        momentum = f"neutral (RSI {rsi_v})"

    # MACD state + recent cross
    hist_recent = df["macd_hist"].tail(n_recent).to_numpy()
    macd_state = "unknown"
    if macd_hist is not None:
        sign = "bullish" if macd_hist > 0 else "bearish"
        crossed = ""
        for k in range(1, len(hist_recent)):
            a, b = hist_recent[k - 1], hist_recent[k]
            if not np.isnan(a) and not np.isnan(b) and (a <= 0 < b or a >= 0 > b):
                bars_ago = len(hist_recent) - 1 - k
                crossed = f"; {'bullish' if b > a else 'bearish'} cross {bars_ago} bar(s) ago"
        macd_state = f"{sign} histogram ({macd_hist}){crossed}"

    # Volatility
    if atr_v is not None and atr_avg:
        volatility = f"{'expanding' if atr_v > atr_avg else 'contracting'} (ATR {atr_v} vs 20-avg {atr_avg})"
    else:
        volatility = "unknown"

    # Price vs bands
    bb_u, bb_l = _f(last["bb_upper"]), _f(last["bb_lower"])
    if bb_u is not None and bb_l is not None:
        if close >= bb_u:
            bands = f"riding/above upper band ({bb_u})"
        elif close <= bb_l:
            bands = f"riding/below lower band ({bb_l})"
        else:
            bands = f"inside bands ({bb_l} – {bb_u})"
    else:
        bands = "unknown"

    # Volume confirmation
    vol, vol_sma = _f(last["volume"]), _f(last["vol_sma20"])
    if vol is not None and vol_sma:
        volume = f"{'above' if vol > vol_sma else 'below'} 20-day avg ({int(vol):,} vs {int(vol_sma):,})"
    else:
        volume = "unknown"

    # --- deeper-analysis facts (all deterministic; the LLM only comments on them) ---

    # Trend strength from ADX (direction-agnostic).
    adx_v = _f(last["adx"]) if "adx" in df.columns else None
    if adx_v is None:
        trend_strength = "unknown"
    elif adx_v < 20:
        trend_strength = f"weak (ADX {adx_v})"
    elif adx_v <= 25:
        trend_strength = f"developing (ADX {adx_v})"
    else:
        trend_strength = f"strong (ADX {adx_v})"

    # Stochastic overbought/oversold.
    stoch_k = _f(last["stoch_k"]) if "stoch_k" in df.columns else None
    stoch_d = _f(last["stoch_d"]) if "stoch_d" in df.columns else None
    if stoch_k is None:
        stochastic = "unknown"
    elif stoch_k > 80:
        stochastic = f"overbought (%K {stoch_k}, %D {stoch_d})"
    elif stoch_k < 20:
        stochastic = f"oversold (%K {stoch_k}, %D {stoch_d})"
    else:
        stochastic = f"neutral (%K {stoch_k}, %D {stoch_d})"

    # Volume trend from OBV slope over the last ~10 bars (accumulation vs distribution).
    volume_trend = "unknown"
    if "obv" in df.columns:
        obv_tail = df["obv"].tail(11).dropna()
        if len(obv_tail) >= 2:
            delta = float(obv_tail.iloc[-1] - obv_tail.iloc[0])
            mean_vol = float(df["volume"].tail(10).mean())
            # "flat" = net OBV flow smaller than half a typical bar's volume
            if abs(delta) < 0.5 * mean_vol:
                volume_trend = "flat"
            else:
                volume_trend = "accumulation" if delta > 0 else "distribution"

    # Session VWAP (intraday only; NaN on daily frames -> None).
    vwap_v = _f(last["vwap"]) if "vwap" in df.columns else None
    vwap_state = None
    if vwap_v is not None:
        vwap_state = f"{'above' if close > vwap_v else 'below'} VWAP ({vwap_v})"

    return {
        "timeframe": timeframe,  # original GAP-5: explicit + selectable, no longer hardcoded
        "lastClose": round(close, 2),
        "trend": trend,
        "shortTermTrend": short,
        "momentum": momentum,
        "macdState": macd_state,
        "volatility": volatility,
        "priceVsBands": bands,
        "volume": volume,
        "trendStrength": trend_strength,
        "stochastic": stochastic,
        "volumeTrend": volume_trend,
        "vwapState": vwap_state,  # None on 1D (session VWAP isn't meaningful on daily)
        "movingAverages": {"sma50": sma50, "sma200": sma200, "ema20": ema20},
        "keyLevels": swing_levels(df),
    }
