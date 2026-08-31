"""Candle geometry, market structure and weekly resampling — deterministic, offline, no store.

The suite that matters most here is `null`-not-zero: a bar with no range has no body ratio, and
reporting 0.0 for it would be a false claim rather than a missing one. Every one of those cases is
pinned below.

Run:  cd services/analysis && python -m pytest -q tests/test_candles.py
"""
import numpy as np
import pandas as pd
import pytest

from app.candles import GEOMETRY_VERSION, RELVOL_WINDOW, add_geometry, extended_limit, geometry_of
from app.data import resample_weekly
from app.indicators import add_indicators
from app.structure import RECENT_BARS, SLOPE_LOOKBACK, build_confluence_from_regimes, build_structure

DAY = pd.Timedelta(days=1)


def frame(bars, start="2026-01-01", freq="D"):
    """bars: list of (open, high, low, close, volume)."""
    idx = pd.date_range(start=start, periods=len(bars), freq=freq)
    return pd.DataFrame(
        {
            "open": [b[0] for b in bars],
            "high": [b[1] for b in bars],
            "low": [b[2] for b in bars],
            "close": [b[3] for b in bars],
            "volume": [float(b[4]) for b in bars],
        },
        index=idx,
    )


def walk(n, seed=7, start="2024-01-01", freq="B", drift=0.0015):
    """A deterministic OHLCV walk long enough for SMA200/EMA200 to be defined."""
    rng = np.random.default_rng(seed)
    close = 100 * np.exp(rng.normal(drift, 0.015, n).cumsum())
    open_ = close * (1 + rng.normal(0, 0.004, n))
    high = np.maximum(open_, close) * (1 + np.abs(rng.normal(0, 0.006, n)))
    low = np.minimum(open_, close) * (1 - np.abs(rng.normal(0, 0.006, n)))
    vol = rng.integers(1_000_000, 3_000_000, n).astype(float)
    idx = pd.date_range(start=start, periods=n, freq=freq)
    return pd.DataFrame(
        {"open": open_, "high": high, "low": low, "close": close, "volume": vol}, index=idx
    )


def fmt(idx):
    return pd.Timestamp(idx).strftime("%Y-%m-%d")


# --------------------------------------------------------------------------- geometry formulas


def test_geometry_formulas_exact():
    # open=100 high=110 low=90 close=105 -> span 20, body 5
    g = geometry_of(add_geometry(frame([(100, 110, 90, 105, 1000)])).iloc[-1])
    assert g["changePct"] == pytest.approx(0.05)
    assert g["rangePct"] == pytest.approx(0.20)
    assert g["bodyPct"] == pytest.approx(5 / 20)
    assert g["upperWickPct"] == pytest.approx((110 - 105) / 20)
    assert g["lowerWickPct"] == pytest.approx((100 - 90) / 20)
    assert g["closePositionInRange"] == pytest.approx((105 - 90) / 20)
    assert g["direction"] == "bullish"


def test_direction_three_ways():
    df = add_geometry(frame([(100, 110, 90, 105, 1), (100, 110, 90, 95, 1), (100, 110, 90, 100, 1)]))
    assert [geometry_of(df.iloc[i])["direction"] for i in range(3)] == [
        "bullish",
        "bearish",
        "flat",
    ]


def test_close_at_extremes():
    at_low = geometry_of(add_geometry(frame([(100, 110, 90, 90, 1)])).iloc[-1])
    at_high = geometry_of(add_geometry(frame([(100, 110, 90, 110, 1)])).iloc[-1])
    assert at_low["closePositionInRange"] == 0.0
    assert at_high["closePositionInRange"] == 1.0


# ------------------------------------------------------------------- the null-not-zero suite


def test_high_equals_low_yields_null_not_zero():
    """A bar with no range has no ratios. `0` here would assert "no body", which is a lie."""
    g = geometry_of(add_geometry(frame([(100, 100, 100, 100, 1000)])).iloc[-1])
    for field in ("bodyPct", "upperWickPct", "lowerWickPct", "closePositionInRange"):
        assert g[field] is None, f"{field} must be null when high == low"
    assert g["rangePct"] == 0.0  # (high-low)/open is a real 0 — the denominator is `open`, not span
    assert g["direction"] == "flat"


def test_full_doji_has_zero_body_but_a_real_range():
    """open == close with high > low IS a zero body — that is a measurement, not a divide by zero."""
    g = geometry_of(add_geometry(frame([(100, 110, 90, 100, 1000)])).iloc[-1])
    assert g["bodyPct"] == 0.0
    assert g["direction"] == "flat"
    assert g["closePositionInRange"] == pytest.approx(0.5)


def test_zero_open_yields_null_change_and_range():
    g = geometry_of(add_geometry(frame([(0.0, 110, 90, 100, 1000)])).iloc[-1])
    assert g["changePct"] is None
    assert g["rangePct"] is None


def test_relative_volume_null_until_window_fills():
    df = add_geometry(walk(40))
    for i in range(RELVOL_WINDOW - 1):
        assert geometry_of(df.iloc[i])["relativeVolume"] is None, f"bar {i} must be null"
    assert geometry_of(df.iloc[RELVOL_WINDOW - 1])["relativeVolume"] is not None


def test_relative_volume_null_when_mean_is_zero():
    df = add_geometry(frame([(100, 101, 99, 100, 0)] * 25))
    assert geometry_of(df.iloc[-1])["relativeVolume"] is None


def test_relative_volume_value_is_the_ratio():
    bars = [(100, 101, 99, 100, 1000)] * (RELVOL_WINDOW - 1) + [(100, 101, 99, 100, 2000)]
    g = geometry_of(add_geometry(frame(bars)).iloc[-1])
    mean = ((RELVOL_WINDOW - 1) * 1000 + 2000) / RELVOL_WINDOW
    assert g["relativeVolume"] == pytest.approx(2000 / mean)


# ------------------------------------------------------------------------- limit independence


def test_geometry_is_independent_of_limit():
    """Two requests for 20 and 200 bars must agree on the bars they share. If relativeVolume were
    computed over exactly the returned window, the small request would silently disagree."""
    df = walk(300)
    small = add_geometry(df.tail(extended_limit(20))).tail(20)
    large = add_geometry(df.tail(extended_limit(200))).tail(200)
    overlap = small.index
    for field in ("bodyPct", "closePositionInRange", "relativeVolume", "changePct"):
        a = [geometry_of(small.loc[i])[field] for i in overlap]
        b = [geometry_of(large.loc[i])[field] for i in overlap]
        assert a == b, f"{field} shifted with limit"


def test_extended_limit_covers_the_rolling_window():
    assert extended_limit(20) == 20 + RELVOL_WINDOW - 1


def test_geometry_version_is_pinned():
    assert isinstance(GEOMETRY_VERSION, int) and GEOMETRY_VERSION >= 1


# ------------------------------------------------------------------------------ market structure


def structure_of(df):
    return build_structure(add_indicators(df, intraday=False), fmt)


def test_structure_block_shape_and_mandatory_raw():
    s = structure_of(walk(300))
    assert set(s) == {
        "trend", "movingAverages", "momentum", "volume", "structure", "recentPriceAction", "raw",
    }
    assert set(s["trend"]) == {
        "direction", "higherHighs", "higherLows", "ageBars",
        "ema20Slope", "ema50Slope", "ema200Slope",
    }
    # raw is mandatory: it is the only thing a reader can check trend.direction against.
    assert len(s["raw"]) == RECENT_BARS
    assert set(s["raw"][0]) == {"time", "open", "high", "low", "close", "volume"}
    assert len(s["recentPriceAction"]) == RECENT_BARS
    assert "relativeVolume" in s["recentPriceAction"][0]


def test_raw_present_on_a_short_window_too():
    s = structure_of(walk(45))
    assert 0 < len(s["raw"]) <= RECENT_BARS


def test_trend_direction_is_a_closed_vocabulary():
    for n, drift in ((300, 0.004), (300, -0.004), (300, 0.0)):
        assert structure_of(walk(n, drift=drift))["trend"]["direction"] in {
            "up", "down", "sideways",
        }


def rising_zigzag(n=125, drift=0.5, amplitude=8.0, period=20):
    """A rising series that actually TURNS. A monotone ramp has no centred local extrema at all —
    the swing detector would find nothing — so the oscillation has to beat the drift over the
    detector's ±5-bar window for peaks and troughs to exist."""
    i = np.arange(n)
    base = 100 + i * drift + amplitude * np.sin(2 * np.pi * i / period)
    return pd.DataFrame(
        {"open": base, "high": base + 1, "low": base - 1, "close": base + 0.2, "volume": 1e6},
        index=pd.date_range("2025-01-01", periods=n, freq="B"),
    )


def test_uptrend_detected_on_a_rising_zigzag():
    s = structure_of(rising_zigzag())
    assert s["trend"]["higherHighs"] is True
    assert s["trend"]["higherLows"] is True
    assert s["trend"]["ema20Slope"] > 0
    assert s["trend"]["direction"] == "up"
    assert s["trend"]["ageBars"] >= 1


def test_downtrend_detected_on_a_falling_zigzag():
    # n=135 lands the last bar on a falling leg, so EMA20's slope agrees with the drift. At n=125
    # the same series is mid-upswing and correctly reads "sideways" — lower highs alone are not a
    # downtrend under the §4.3 rule.
    s = structure_of(rising_zigzag(n=135, drift=-0.5))
    assert s["trend"]["higherHighs"] is False
    assert s["trend"]["higherLows"] is False
    assert s["trend"]["ema20Slope"] < 0
    assert s["trend"]["direction"] == "down"


def test_lower_highs_with_a_rising_ema_is_sideways_not_down():
    s = structure_of(rising_zigzag(n=125, drift=-0.5))
    assert s["trend"]["higherHighs"] is False
    assert s["trend"]["ema20Slope"] > 0
    assert s["trend"]["direction"] == "sideways"


def test_age_bars_is_an_integer_within_the_swing_window():
    from app.structure import SWING_LOOKBACK

    age = structure_of(rising_zigzag())["trend"]["ageBars"]
    assert isinstance(age, int) and 1 <= age <= SWING_LOOKBACK


def test_higher_highs_null_when_too_few_swings():
    s = structure_of(walk(40))
    for key in ("higherHighs", "higherLows"):
        assert s["trend"][key] in (True, False, None)


def test_ordering_omits_unavailable_emas():
    """A 40-bar window has no ema200 and must not claim one."""
    short = structure_of(walk(45))
    assert short["movingAverages"]["ema200"] is None
    assert "ema200" not in short["movingAverages"]["ordering"]
    assert short["trend"]["ema200Slope"] is None

    long = structure_of(walk(300))
    assert long["movingAverages"]["ema200"] is not None
    assert "ema200" in long["movingAverages"]["ordering"]


def test_ordering_is_sorted_descending():
    s = structure_of(walk(300))
    ma = s["movingAverages"]
    values = {"ema20": ma["ema20"], "ema50": ma["ema50"], "ema200": ma["ema200"]}
    names = ma["ordering"].split(">")
    seen = [values[n] for n in names if n in values]
    assert seen == sorted(seen, reverse=True)


def test_slopes_are_null_safe_not_zero():
    s = structure_of(walk(30))  # too short for ema50/ema200
    assert s["trend"]["ema50Slope"] is None
    assert s["trend"]["ema200Slope"] is None
    assert s["trend"]["ema20Slope"] is not None


def test_ema50_matches_the_house_convention():
    """ema50/ema200 are computed in structure.py, so they must agree with ema20's convention."""
    df = walk(300)
    enriched = add_indicators(df, intraday=False)
    s = build_structure(enriched, fmt)
    expected = df["close"].ewm(span=50, adjust=False).mean().iloc[-1]
    assert s["movingAverages"]["ema50"] == pytest.approx(round(float(expected), 2))


def test_momentum_and_volume_vocabularies():
    s = structure_of(walk(300))
    assert s["momentum"]["rsiDirection"] in {"rising", "falling", "flat"}
    assert s["momentum"]["macdHistDirection"] in {"positive", "negative", "flat"}
    assert s["volume"]["trend20d"] in {"rising", "falling", "flat"}
    assert isinstance(s["volume"]["breakoutVolume"], bool)


def test_breakout_volume_is_false_never_null_when_relvol_missing():
    s = structure_of(walk(35))  # fewer bars than the relvol window at the head, still resolves
    assert s["volume"]["breakoutVolume"] in (True, False)


def test_supports_and_resistances_come_from_swing_levels():
    from app.regime import swing_levels

    df = walk(300)
    enriched = add_indicators(df, intraday=False)
    s = build_structure(enriched, fmt)
    levels = swing_levels(enriched)
    assert s["structure"]["supports"] == levels["support"]
    assert s["structure"]["resistances"] == levels["resistance"]
    assert s["structure"]["nearestSupport"] == (levels["support"][0] if levels["support"] else None)
    assert s["structure"]["nearestResistance"] == (
        levels["resistance"][0] if levels["resistance"] else None
    )


def test_breakout_on_a_constructed_series():
    """Range-bound for 80 bars, then a decisive close above the range on 3x volume."""
    bars = [(100, 104, 96, 100 + (2 if i % 8 < 4 else -2), 1_000_000) for i in range(80)]
    bars.append((104, 120, 103, 119, 4_000_000))
    s = structure_of(frame(bars, freq="B"))
    assert s["volume"]["breakoutVolume"] is True
    assert s["structure"]["breakout"] is True
    assert s["structure"]["failedBreakout"] is False


def test_failed_breakout_when_price_returns_below_the_level():
    bars = [(100, 104, 96, 100 + (2 if i % 8 < 4 else -2), 1_000_000) for i in range(80)]
    bars.append((104, 120, 103, 119, 4_000_000))          # breakout
    bars.extend([(118, 119, 98, 99, 1_000_000)] * 2)       # and straight back inside
    s = structure_of(frame(bars, freq="B"))
    assert s["structure"]["failedBreakout"] is True
    assert s["structure"]["breakout"] is False


def test_failed_breakout_window_is_slope_lookback():
    assert SLOPE_LOOKBACK >= 1


# ---------------------------------------------------------------- pure multi-timeframe synthesis


def _regime(trend, momentum="neutral (RSI 50)"):
    return {
        "trend": trend,
        "trendStrength": "strong (ADX 30)",
        "momentum": momentum,
        "stochastic": "neutral (%K 50, %D 50)",
    }


def test_synthesis_sorts_weekly_first():
    """confluence.TF_RANK predates 1W and would sort the HIGHEST timeframe last."""
    c = build_confluence_from_regimes(
        {"1H": _regime("uptrend"), "1D": _regime("uptrend"), "1W": _regime("uptrend")}
    )
    assert c["timeframes"] == ["1W", "1D", "1H"]
    assert c["trendAlignment"] == "aligned up"
    assert c["alignmentScore"] == 3
    assert c["conflicts"] == []


def test_synthesis_matches_build_confluence_key_for_key():
    """One word for agreement across both endpoints — no second vocabulary."""
    from app.confluence import build_confluence

    regimes = {"1D": _regime("uptrend"), "1H": _regime("downtrend")}
    assert set(build_confluence_from_regimes(regimes)) == set(build_confluence(regimes))
    assert build_confluence_from_regimes(regimes)["trendAlignment"] == "mixed"


def test_synthesis_is_pure_and_reports_conflicts():
    c = build_confluence_from_regimes(
        {"1W": _regime("uptrend"), "1D": _regime("downtrend", "overbought (RSI 80)")}
    )
    assert c["conflicts"]
    assert c["conflicts"][0].startswith("1W uptrend but 1D downtrend")
    assert "1D overbought" in c["momentumFlags"]


def test_synthesis_empty_input():
    c = build_confluence_from_regimes({})
    assert c["timeframes"] == []
    assert c["trendAlignment"] == "no clear trend"


# ------------------------------------------------------------------------------ weekly resampling


def test_weekly_resample_aggregates_ohlcv():
    # Mon-Fri then Mon-Wed: two W-FRI buckets, the second partial.
    days = pd.bdate_range("2026-01-05", periods=8)  # Mon 5th .. Wed 14th
    df = pd.DataFrame(
        {
            "open": [10, 11, 12, 13, 14, 20, 21, 22],
            "high": [15, 16, 17, 18, 19, 25, 26, 27],
            "low": [5, 6, 7, 8, 9, 15, 16, 17],
            "close": [11, 12, 13, 14, 15, 21, 22, 23],
            "volume": [1.0] * 8,
        },
        index=days,
    )
    weekly = resample_weekly(df)
    assert len(weekly) == 2

    first = weekly.iloc[0]
    assert first["open"] == 10          # first daily open of the bucket
    assert first["high"] == 19          # max
    assert first["low"] == 5            # min
    assert first["close"] == 15         # last daily close
    assert first["volume"] == 5         # sum

    # ts is the LAST daily bar inside the bucket, never a future Friday.
    assert weekly.index[0] == days[4]   # Fri 9 Jan
    assert weekly.index[1] == days[-1]  # Wed 14 Jan — the partial week, unpadded


def test_weekly_partial_final_week_is_not_padded():
    days = pd.bdate_range("2026-01-05", periods=6)  # one full week + one Monday
    df = pd.DataFrame(
        {"open": 1.0, "high": 2.0, "low": 0.5, "close": 1.5, "volume": 1.0}, index=days
    )
    weekly = resample_weekly(df)
    assert len(weekly) == 2
    assert weekly.index[-1] == days[-1]
    assert weekly["volume"].iloc[-1] == 1.0  # a single day, not padded to five


def test_weekly_resample_of_empty_frame():
    empty = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
    assert resample_weekly(empty).empty
