"""Market structure (contract §4.3, doc §14.4) — deterministic, computed here, never by a model.

This is the block that lets a reader argue with the backend. `trend.direction` is one word, and one
word is exactly the kind of thing a model will accept uncritically — so the block always ships
`raw`, the actual last N OHLCV bars, alongside it. Doc §14.4's rule: never reduce the evidence to a
single backend label.

Two reuse decisions worth stating, because both were deliberate:

- **Swing points come from the same centred-local-extrema pass `regime.swing_levels` uses**, with
  the same lookback and window. Structure and key levels therefore cannot disagree — they are
  reading the same extrema. `swing_levels` remains THE support/resistance algorithm; this module
  extends around it and does not replace it.
- **`ema50` / `ema200` do not exist as indicator columns** and are not added to `indicators.py`.
  They are computed here with the identical house convention, `ewm(span=N, adjust=False)`, so they
  agree with the existing `ema20` column to the last digit.
"""
from __future__ import annotations

import numpy as np
import pandas as pd

from .candles import add_geometry, candle_payload, geometry_of
from .confluence import TF_RANK, _EXTREMES, _build_note, _momentum_label, _short_trend, _trend_vote
from .regime import swing_levels

# Same window `swing_levels` uses, so the two passes see the same extrema.
SWING_LOOKBACK = 60
SWING_WINDOW = 5

# Bars between the two endpoints of every slope / direction comparison in this module.
SLOPE_LOOKBACK = 5

# relativeVolume at or above this makes a breakout volume-confirmed.
BREAKOUT_VOLUME_MULT = 1.5

# How many bars `recentPriceAction` and `raw` carry.
RECENT_BARS = 10

# 20-bar volume trend comparison distance (vol_sma20 now vs one full window ago).
VOLUME_TREND_LOOKBACK = 20

# Weekly is the highest timeframe, so it sorts before 1D. confluence.TF_RANK predates 1W and starts
# at 1D=0; extending it here rather than editing confluence.py keeps that module untouched.
MTF_RANK = {"1W": -1, **TF_RANK}


def _f(value, places: int = 4) -> float | None:
    """NaN / inf / missing -> None. Never a zero standing in for "unknown"."""
    if value is None:
        return None
    try:
        f = float(value)
    except (TypeError, ValueError):
        return None
    if np.isnan(f) or np.isinf(f):
        return None
    return round(f, places)


def _ema(close: pd.Series, span: int) -> pd.Series:
    """House convention EMA, with a length gate.

    The convention itself is `indicators.add_indicators`': `ewm(span=N, adjust=False)`, which
    produces a value from the very first bar. That is fine for ema20 on any real window, but it
    means a 40-bar intraday frame would report an "ema200" built from 40 bars. Values before the
    span has filled are therefore masked, so the series never claims an average it does not have.
    """
    series = close.ewm(span=span, adjust=False).mean()
    if len(series) < span:
        return pd.Series(np.nan, index=series.index)
    masked = series.copy()
    masked.iloc[: span - 1] = np.nan
    return masked


def _slope(series: pd.Series, pos: int, lookback: int = SLOPE_LOOKBACK) -> float | None:
    """Normalised slope `(v[pos] - v[pos-k]) / v[pos-k]`; None when either endpoint is missing."""
    if pos - lookback < 0 or pos >= len(series):
        return None
    now, then = series.iloc[pos], series.iloc[pos - lookback]
    if pd.isna(now) or pd.isna(then) or float(then) == 0.0:
        return None
    return _f((float(now) - float(then)) / float(then), 6)


def _swing_points(df: pd.DataFrame) -> tuple[list[dict], list[dict]]:
    """Centred local extrema over the last SWING_LOOKBACK bars — the same pass `swing_levels` runs.

    Positions are ABSOLUTE (indices into `df`). `confirmedAt` is the bar at which a centred extreme
    becomes knowable: an extreme at i needs SWING_WINDOW bars on its right, so it is not confirmed
    until i + SWING_WINDOW. Respecting that is what keeps the bar-by-bar trend evaluation causal.
    """
    recent = df.tail(SWING_LOOKBACK)
    offset = len(df) - len(recent)
    highs, lows = recent["high"].to_numpy(), recent["low"].to_numpy()
    swing_highs: list[dict] = []
    swing_lows: list[dict] = []
    for i in range(SWING_WINDOW, len(recent) - SWING_WINDOW):
        seg_h = highs[i - SWING_WINDOW : i + SWING_WINDOW + 1]
        seg_l = lows[i - SWING_WINDOW : i + SWING_WINDOW + 1]
        if highs[i] == seg_h.max():
            swing_highs.append(
                {"pos": offset + i, "value": round(float(highs[i]), 2),
                 "confirmedAt": offset + i + SWING_WINDOW}
            )
        if lows[i] == seg_l.min():
            swing_lows.append(
                {"pos": offset + i, "value": round(float(lows[i]), 2),
                 "confirmedAt": offset + i + SWING_WINDOW}
            )
    return swing_highs, swing_lows


def _ascending(points: list[dict], at: int) -> bool | None:
    """Are the last two points confirmed at or before `at` ascending? None when there are <2."""
    known = [p for p in points if p["confirmedAt"] <= at]
    if len(known) < 2:
        return None
    return known[-1]["value"] > known[-2]["value"]


def _direction_at(
    pos: int, swing_highs: list[dict], swing_lows: list[dict], ema20: pd.Series
) -> str:
    """`up | down | sideways` at bar `pos`, using only information confirmed at or before it.

    Deliberately NOT `detect_regime`'s `trend` string: that answers "is price above SMA200" and
    speaks a different vocabulary ("uptrend" / "insufficient history for SMA200"). Different
    question, different words, and `regime.py` is not this lane's to change.
    """
    hh = _ascending(swing_highs, pos)
    hl = _ascending(swing_lows, pos)
    slope = _slope(ema20, pos)
    if hh and hl and slope is not None and slope > 0:
        return "up"
    if hh is False and hl is False and slope is not None and slope < 0:
        return "down"
    return "sideways"


def _age_bars(
    df: pd.DataFrame, swing_highs: list[dict], swing_lows: list[dict], ema20: pd.Series
) -> int:
    """Bars since `direction` last changed, evaluated bar by bar with the same rule.

    Bounded to the swing window: if the direction has held for the whole window the answer is the
    window length, which is a floor rather than an estimate.
    """
    last = len(df) - 1
    if last < 0:
        return 0
    current = _direction_at(last, swing_highs, swing_lows, ema20)
    age = 1
    floor = max(0, len(df) - SWING_LOOKBACK)
    for pos in range(last - 1, floor - 1, -1):
        if _direction_at(pos, swing_highs, swing_lows, ema20) != current:
            break
        age += 1
    return age


def _ordering(price: float | None, ema20, ema50, ema200) -> str:
    """"price>ema20>ema50>ema200", omitting members that are null. A 40-bar intraday window has no
    ema200 and must not claim one."""
    members = [("price", price), ("ema20", ema20), ("ema50", ema50), ("ema200", ema200)]
    present = [(name, value) for name, value in members if value is not None]
    present.sort(key=lambda kv: kv[1], reverse=True)
    return ">".join(name for name, _ in present)


def _trichotomy(now, then, rising: str, falling: str, flat: str = "flat") -> str:
    if now is None or then is None:
        return flat
    if now > then:
        return rising
    if now < then:
        return falling
    return flat


def _breakout_state(df: pd.DataFrame, breakout_volume: bool) -> tuple[bool, bool]:
    """(breakout, failedBreakout).

    A breakout is the close clearing the resistance that was nearest *as of the previous bar* —
    using the current bar's own levels would be circular, since the breakout bar's high becomes a
    level. A failed breakout is one that happened within the last SLOPE_LOOKBACK bars and whose
    level the close has since fallen back below.
    """
    if len(df) < 2:
        return False, False

    def nearest_resistance(upto: int) -> float | None:
        """Nearest resistance computed from bars strictly before `upto`."""
        if upto < 2:
            return None
        levels = swing_levels(df.iloc[:upto])["resistance"]
        return levels[0] if levels else None

    last_close = float(df["close"].iloc[-1])
    level = nearest_resistance(len(df) - 1)
    breakout = bool(level is not None and last_close > level and breakout_volume)

    failed = False
    for back in range(0, SLOPE_LOOKBACK):
        pos = len(df) - 1 - back
        if pos < 2:
            break
        past_level = nearest_resistance(pos)
        if past_level is None:
            continue
        if float(df["close"].iloc[pos]) > past_level and last_close < past_level:
            failed = True
            break
    return breakout, failed


def build_structure(enriched: pd.DataFrame, fmt_time) -> dict:
    """The §4.3 market-structure block from an indicator-enriched, store-derived frame.

    `enriched` must already carry `add_indicators` columns. `fmt_time` is the caller's time
    formatter so `raw` uses exactly the same time key as every other endpoint.
    """
    close = enriched["close"]
    last_pos = len(enriched) - 1
    last_close = float(close.iloc[-1])

    ema20_series = enriched["ema20"]
    ema50_series = _ema(close, 50)
    ema200_series = _ema(close, 200)

    swing_highs, swing_lows = _swing_points(enriched)
    direction = _direction_at(last_pos, swing_highs, swing_lows, ema20_series)

    ema20 = _f(ema20_series.iloc[-1], 2)
    ema50 = _f(ema50_series.iloc[-1], 2)
    ema200 = _f(ema200_series.iloc[-1], 2)

    geo = add_geometry(enriched)
    last_geo = geometry_of(geo.iloc[-1])
    relative_volume = last_geo["relativeVolume"]

    levels = swing_levels(enriched)
    supports, resistances = levels["support"], levels["resistance"]
    breakout_volume = relative_volume is not None and relative_volume >= BREAKOUT_VOLUME_MULT
    breakout, failed_breakout = _breakout_state(enriched, breakout_volume)

    vol_sma = enriched["vol_sma20"]
    vol_now = _f(vol_sma.iloc[-1], 2)
    vol_then = (
        _f(vol_sma.iloc[-1 - VOLUME_TREND_LOOKBACK], 2)
        if len(vol_sma) > VOLUME_TREND_LOOKBACK
        else None
    )

    rsi_now = _f(enriched["rsi"].iloc[-1], 2)
    rsi_then = (
        _f(enriched["rsi"].iloc[-1 - SLOPE_LOOKBACK], 2) if len(enriched) > SLOPE_LOOKBACK else None
    )
    macd_hist = _f(enriched["macd_hist"].iloc[-1], 6)

    return {
        "trend": {
            "direction": direction,
            "higherHighs": _ascending(swing_highs, last_pos),
            "higherLows": _ascending(swing_lows, last_pos),
            "ageBars": _age_bars(enriched, swing_highs, swing_lows, ema20_series),
            "ema20Slope": _slope(ema20_series, last_pos),
            "ema50Slope": _slope(ema50_series, last_pos),
            "ema200Slope": _slope(ema200_series, last_pos),
        },
        "movingAverages": {
            "ema20": ema20,
            "ema50": ema50,
            "ema200": ema200,
            "ordering": _ordering(round(last_close, 2), ema20, ema50, ema200),
            "priceVsEma20Pct": (
                _f((last_close - ema20) / ema20, 6) if ema20 not in (None, 0) else None
            ),
        },
        "momentum": {
            "rsi14": rsi_now,
            "rsiDirection": _trichotomy(rsi_now, rsi_then, "rising", "falling"),
            "macd": _f(enriched["macd"].iloc[-1], 4),
            "macdSignal": _f(enriched["macd_signal"].iloc[-1], 4),
            "macdHistDirection": (
                "flat" if macd_hist is None or macd_hist == 0
                else "positive" if macd_hist > 0 else "negative"
            ),
            "adx14": _f(enriched["adx"].iloc[-1], 2),
        },
        "volume": {
            "relativeVolume": relative_volume,
            "trend20d": _trichotomy(vol_now, vol_then, "rising", "falling"),
            "breakoutVolume": breakout_volume,
        },
        "structure": {
            "recentSwingHigh": swing_highs[-1]["value"] if swing_highs else None,
            "recentSwingLow": swing_lows[-1]["value"] if swing_lows else None,
            "nearestResistance": resistances[0] if resistances else None,
            "nearestSupport": supports[0] if supports else None,
            "supports": supports,
            "resistances": resistances,
            "breakout": breakout,
            "failedBreakout": failed_breakout,
        },
        # Mandatory, both of them. Omitting `raw` to save tokens is a contract violation, not an
        # optimisation — it is the only thing a reader can check `trend.direction` against.
        "recentPriceAction": candle_payload(geo, fmt_time, limit=RECENT_BARS),
        "raw": [
            {
                "time": fmt_time(idx),
                "open": round(float(r.open), 2),
                "high": round(float(r.high), 2),
                "low": round(float(r.low), 2),
                "close": round(float(r.close), 2),
                "volume": int(r.volume),
            }
            for idx, r in enriched.tail(RECENT_BARS).iterrows()
        ],
    }


def build_confluence_from_regimes(regimes: dict[str, dict]) -> dict:
    """PURE `{timeframe: regime} -> synthesis`. Same output shape and vocabulary as
    `confluence.build_confluence`, which this deliberately does not call.

    Why a second function rather than a call: `build_confluence` sorts timeframes by
    `confluence.TF_RANK`, which predates `1W` and would sort the highest timeframe LAST. Every
    word-producing helper is imported from that module rather than restated, so the two cannot grow
    two vocabularies for the same idea; only the ordering differs.
    """
    tfs = sorted(regimes.keys(), key=lambda t: MTF_RANK.get(t, 99))

    trend_by = {tf: regimes[tf].get("trend") for tf in tfs}
    strength_by = {tf: regimes[tf].get("trendStrength") for tf in tfs}
    mom_by = {tf: regimes[tf].get("momentum") for tf in tfs}
    stoch_by = {tf: regimes[tf].get("stochastic") for tf in tfs}

    votes = {tf: _trend_vote(trend_by[tf]) for tf in tfs}
    ups = sum(1 for v in votes.values() if v > 0)
    downs = sum(1 for v in votes.values() if v < 0)

    if ups > 0 and downs == 0:
        alignment = "aligned up"
    elif downs > 0 and ups == 0:
        alignment = "aligned down"
    elif ups == 0 and downs == 0:
        alignment = "no clear trend"
    else:
        alignment = "mixed"

    mlabels = {tf: _momentum_label(mom_by[tf]) for tf in tfs}
    conflicts: list[str] = []
    for i in range(len(tfs)):
        for j in range(i + 1, len(tfs)):
            a, b = tfs[i], tfs[j]
            if votes[a] != 0 and votes[b] != 0 and votes[a] != votes[b]:
                conflicts.append(f"{a} {_short_trend(trend_by[a])} but {b} {_short_trend(trend_by[b])}")
    for i in range(len(tfs)):
        for j in range(i + 1, len(tfs)):
            a, b = tfs[i], tfs[j]
            la, lb = mlabels[a], mlabels[b]
            if la != lb and (la in _EXTREMES or lb in _EXTREMES):
                conflicts.append(f"{a} {la} vs {b} {lb}")

    flags = [f"{tf} {mlabels[tf]}" for tf in tfs if mlabels[tf] in _EXTREMES]

    return {
        "timeframes": tfs,
        "trendByTf": trend_by,
        "trendStrengthByTf": strength_by,
        "momentumByTf": mom_by,
        "stochByTf": stoch_by,
        "trendAlignment": alignment,
        "alignmentScore": ups - downs,
        "alignmentDetail": {"up": ups, "down": downs},
        "momentumFlags": "; ".join(flags) if flags else "no momentum extremes",
        "conflicts": conflicts,
        "note": _build_note(tfs, alignment, mlabels),
    }
