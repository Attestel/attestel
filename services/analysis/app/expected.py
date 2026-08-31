"""Expected-close RANGE — a deterministic, volatility-based band for where the CURRENT candle might
close. This is NOT a point prediction and NOT model output: it is a statistical range computed from
ATR and how much of the current bar/session is left.

Method: ±1 ATR × (fraction of the bar remaining). At the bar's open the full ATR is ahead, so the
band is widest; as the bar approaches its close there is less time for it to move, so the band shrinks
toward the current price. ~1 ATR ≈ a ~68% band. Price can gap straight through it — it is context,
not a forecast.
"""
from __future__ import annotations

from datetime import datetime, time
from zoneinfo import ZoneInfo

import pandas as pd

from .data import DataError, fetch_ohlcv, is_intraday, normalize_timeframe
from .indicators import add_indicators

# US regular trading hours (Eastern). We don't model half-days/holidays — the band is a rough,
# honest estimate, and being slightly generous outside those edge cases is fine.
_ET = ZoneInfo("America/New_York")
_OPEN = time(9, 30)
_CLOSE = time(16, 0)
_SESSION_MINUTES = 390.0  # 6.5h regular session

# Minutes in one bar of each timeframe (1D = the whole session).
_BAR_MINUTES = {"1D": _SESSION_MINUTES, "1H": 60.0, "15m": 15.0, "5m": 5.0}

_METHOD = "±1 ATR × time-remaining"
_CONFIDENCE = "~68% band"
RANGE_LABEL = "statistical range from volatility — not a prediction; price can gap through it"


def session_remaining_pct(timeframe: str, now: datetime | None = None) -> float:
    """Fraction (0..1) of the CURRENT bar still ahead. 1.0 at/ before the bar opens, →0 near its
    close. `now` may be passed (tz-aware) for testing; otherwise the current Eastern time is used."""
    tf = normalize_timeframe(timeframe)
    now = now.astimezone(_ET) if now is not None else datetime.now(_ET)

    # Weekend / outside-of-session handling: a full bar is ahead when the market (re)opens.
    if now.weekday() >= 5:
        return 1.0
    open_dt = now.replace(hour=_OPEN.hour, minute=_OPEN.minute, second=0, microsecond=0)
    close_dt = now.replace(hour=_CLOSE.hour, minute=_CLOSE.minute, second=0, microsecond=0)
    if now <= open_dt:
        return 1.0          # pre-market: the whole session/bar is still ahead
    if now >= close_dt:
        return 0.0          # after the close: the current candle is already done

    minutes_since_open = (now - open_dt).total_seconds() / 60.0
    if tf == "1D":
        return _clamp((_SESSION_MINUTES - minutes_since_open) / _SESSION_MINUTES)
    bar_len = _BAR_MINUTES[tf]
    into_bar = minutes_since_open % bar_len  # bars align to the 9:30 open
    return _clamp((bar_len - into_bar) / bar_len)


def _clamp(x: float) -> float:
    return max(0.0, min(1.0, x))


def expected_close_payload(close: float, atr: float | None, remaining: float,
                           timeframe: str, source: str) -> dict:
    """Build the expected-close range from a close, an ATR, and the fraction of the bar remaining.
    Pure (no I/O) so it is directly unit-testable. band = ATR × remaining; the band brackets `close`
    and collapses to it as remaining → 0."""
    band = (atr or 0.0) * remaining
    return {
        "lastClose": round(close, 2),
        "low": round(close - band, 2),
        "mid": round(close, 2),      # best estimate of the close is the current price (no drift claim)
        "high": round(close + band, 2),
        "band": round(band, 2),
        "atr": round(atr, 2) if atr is not None else None,
        "sessionRemainingPct": round(remaining, 4),
        "method": _METHOD,
        "confidence": _CONFIDENCE,
        "label": RANGE_LABEL,
        "timeframe": normalize_timeframe(timeframe),
        "source": source,
        "sourceIsSynthetic": source == "synthetic",
    }


def expected_close(ticker: str, timeframe: str = "1D", now: datetime | None = None) -> dict:
    """Deterministic expected-close range for the current candle: latest close ± ATR(14) scaled by the
    fraction of the bar remaining. Raises DataError upstream on a bad frame."""
    tf = normalize_timeframe(timeframe)
    df, source = fetch_ohlcv(ticker.upper(), tf)
    enriched = add_indicators(df, intraday=is_intraday(tf))
    last = enriched.iloc[-1]
    close = float(last.close)
    atr = float(last.atr) if pd.notna(last.atr) else None
    remaining = session_remaining_pct(tf, now)
    return expected_close_payload(close, atr, remaining, tf, source)


def expected_close_from_row(last_row, timeframe: str, source: str, now: datetime | None = None) -> dict:
    """Expected-close range from an already-computed enriched row (so the /analysis endpoint can fold
    it in without re-fetching). `last_row` is the last row of the enriched frame."""
    close = float(last_row.close)
    atr = float(last_row.atr) if pd.notna(last_row.atr) else None
    return expected_close_payload(close, atr, session_remaining_pct(timeframe, now), timeframe, source)
