"""Per-bar candle geometry (contract §4.2, doc §14.3) — deterministic, computed here, never by a
model.

The point of this module is to hand a reader the *shape* of a bar as numbers rather than as an
adjective. "Long lower wick, closed near the high, on 1.6x volume" is three fields here; leaving it
to prose invites a model to invent it.

**Division by zero returns null, never 0.** A bar whose high equals its low has no body-to-range
ratio — reporting `0.0` would say "no body", which is a different and false claim. Every field with
`(high - low)` underneath is null on such a bar, and `changePct`/`rangePct` are null when `open` is
0. This is the one rule in here worth breaking a build over.
"""
from __future__ import annotations

import numpy as np
import pandas as pd

# Bumped whenever any formula below changes. Wave 3's prediction records stamp it so a result can be
# attributed to the geometry that produced it. Deliberately NOT part of any response body —
# contract §4.4 does not list it, and silence is not permission.
GEOMETRY_VERSION = 1

# Causal: the mean covers the current bar and the 19 before it.
RELVOL_WINDOW = 20

# Derived ratios are rounded at the payload edge only, so JSON is stable across runs.
GEOMETRY_ROUND = 6

GEOMETRY_FIELDS = (
    "changePct",
    "rangePct",
    "bodyPct",
    "upperWickPct",
    "lowerWickPct",
    "closePositionInRange",
    "direction",
    "relativeVolume",
)


def _safe_div(numerator: pd.Series, denominator: pd.Series) -> pd.Series:
    """Element-wise divide where a zero (or NaN) denominator yields NaN, not inf and not 0."""
    denom = denominator.replace(0.0, np.nan)
    return numerator / denom


def add_geometry(df: pd.DataFrame) -> pd.DataFrame:
    """OHLCV frame in -> same frame plus the §4.2 geometry columns.

    `relativeVolume` is computed over whatever series it is given, so callers MUST pass a frame
    extended by at least RELVOL_WINDOW - 1 bars beyond the window they intend to return. Otherwise
    the first bars of a small request would report a different relativeVolume than the same bars in
    a large request — see `extended_limit`.
    """
    out = df.copy()
    open_, high, low, close = out["open"], out["high"], out["low"], out["close"]

    span = high - low
    body_top = pd.concat([open_, close], axis=1).max(axis=1)
    body_bottom = pd.concat([open_, close], axis=1).min(axis=1)

    out["changePct"] = _safe_div(close - open_, open_)
    out["rangePct"] = _safe_div(span, open_)
    out["bodyPct"] = _safe_div((close - open_).abs(), span)
    out["upperWickPct"] = _safe_div(high - body_top, span)
    out["lowerWickPct"] = _safe_div(body_bottom - low, span)
    out["closePositionInRange"] = _safe_div(close - low, span)

    out["direction"] = np.where(close > open_, "bullish", np.where(close < open_, "bearish", "flat"))

    rolling_mean = out["volume"].rolling(RELVOL_WINDOW, min_periods=RELVOL_WINDOW).mean()
    out["relativeVolume"] = _safe_div(out["volume"], rolling_mean)
    return out


def extended_limit(limit: int) -> int:
    """How many bars to read so that `limit` bars can be returned with correct relativeVolume."""
    return int(limit) + RELVOL_WINDOW - 1


def _num(value) -> float | None:
    """NaN / inf -> None (never 0), otherwise a rounded float. This is the null-not-zero rule."""
    if value is None:
        return None
    try:
        f = float(value)
    except (TypeError, ValueError):
        return None
    if np.isnan(f) or np.isinf(f):
        return None
    return round(f, GEOMETRY_ROUND)


def geometry_of(row) -> dict:
    """The §4.2 geometry block for one row of an `add_geometry` frame."""
    return {
        "changePct": _num(row["changePct"]),
        "rangePct": _num(row["rangePct"]),
        "bodyPct": _num(row["bodyPct"]),
        "upperWickPct": _num(row["upperWickPct"]),
        "lowerWickPct": _num(row["lowerWickPct"]),
        "closePositionInRange": _num(row["closePositionInRange"]),
        "direction": str(row["direction"]),
        "relativeVolume": _num(row["relativeVolume"]),
    }


def candle_payload(df: pd.DataFrame, fmt_time, limit: int | None = None) -> list[dict]:
    """Geometry-bearing bars, newest last. OHLC keeps the existing 2dp / int-volume convention so a
    candle here and a candle in /analysis are the same numbers."""
    frame = df if limit is None else df.tail(limit)
    bars = []
    for idx, r in frame.iterrows():
        bar = {
            "time": fmt_time(idx),
            "open": round(float(r["open"]), 2),
            "high": round(float(r["high"]), 2),
            "low": round(float(r["low"]), 2),
            "close": round(float(r["close"]), 2),
            "volume": int(r["volume"]),
        }
        bar.update(geometry_of(r))
        bars.append(bar)
    return bars
