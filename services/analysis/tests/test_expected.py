"""Expected-close RANGE tests — deterministic, offline.

The range must (a) BRACKET the last price, (b) be centered on it, (c) SHRINK as the session runs out
and collapse to the price at the close, and (d) scale with ATR. The number is computed here, never by
the LLM.
"""
from datetime import datetime
from zoneinfo import ZoneInfo

import pytest

from app.expected import (
    RANGE_LABEL,
    expected_close_payload,
    session_remaining_pct,
)

_ET = ZoneInfo("America/New_York")


# --------------------------------------------------------------------------- payload math

def test_range_brackets_and_centers_on_last_price():
    p = expected_close_payload(close=100.0, atr=4.0, remaining=1.0, timeframe="1D", source="yfinance")
    assert p["low"] <= p["lastClose"] <= p["high"]
    assert p["mid"] == 100.0                       # centered on the current price (no drift claim)
    assert p["low"] == 96.0 and p["high"] == 104.0  # ±1 ATR at full session
    assert p["band"] == 4.0
    assert p["label"] == RANGE_LABEL
    assert p["method"] == "±1 ATR × time-remaining"


def test_range_shrinks_as_session_runs_out():
    full = expected_close_payload(100.0, 4.0, 1.0, "1D", "yfinance")
    half = expected_close_payload(100.0, 4.0, 0.5, "1D", "yfinance")
    late = expected_close_payload(100.0, 4.0, 0.1, "1D", "yfinance")
    widths = [q["high"] - q["low"] for q in (full, half, late)]
    assert widths[0] > widths[1] > widths[2]       # monotonically narrower as time runs out
    assert half["band"] == pytest.approx(2.0)      # 4.0 * 0.5


def test_range_collapses_to_price_at_close():
    p = expected_close_payload(100.0, 4.0, 0.0, "1D", "yfinance")
    assert p["low"] == p["high"] == p["mid"] == 100.0   # no time left -> no uncertainty band
    assert p["band"] == 0.0


def test_range_scales_with_atr():
    small = expected_close_payload(100.0, 1.0, 1.0, "1D", "yfinance")
    big = expected_close_payload(100.0, 8.0, 1.0, "1D", "yfinance")
    assert (big["high"] - big["low"]) > (small["high"] - small["low"])


def test_missing_atr_is_safe():
    p = expected_close_payload(100.0, None, 1.0, "1D", "synthetic")
    assert p["atr"] is None and p["band"] == 0.0
    assert p["low"] == p["high"] == 100.0
    assert p["sourceIsSynthetic"] is True


# --------------------------------------------------------------------------- session-remaining clock

def test_session_remaining_full_before_open_and_weekend():
    pre = datetime(2026, 7, 6, 8, 0, tzinfo=_ET)   # Monday pre-market
    assert session_remaining_pct("1D", pre) == 1.0
    sat = datetime(2026, 7, 11, 12, 0, tzinfo=_ET)  # Saturday
    assert session_remaining_pct("1D", sat) == 1.0


def test_session_remaining_zero_after_close():
    post = datetime(2026, 7, 6, 16, 30, tzinfo=_ET)  # Monday after the close
    assert session_remaining_pct("1D", post) == 0.0


def test_session_remaining_midday_is_between():
    # Monday 12:45 ET: 3h15m of a 6.5h session elapsed -> half remaining
    noon = datetime(2026, 7, 6, 12, 45, tzinfo=_ET)
    r = session_remaining_pct("1D", noon)
    assert r == pytest.approx(0.5, abs=1e-6)


def test_intraday_bar_remaining_resets_each_bar():
    # 15m bars align to 9:30. At 9:35 we're 5m into the first bar -> 10/15 remaining.
    t = datetime(2026, 7, 6, 9, 35, tzinfo=_ET)
    assert session_remaining_pct("15m", t) == pytest.approx(10.0 / 15.0, abs=1e-6)
    # at 9:45 a new bar just started -> full again
    t2 = datetime(2026, 7, 6, 9, 45, tzinfo=_ET)
    assert session_remaining_pct("15m", t2) == pytest.approx(1.0, abs=1e-6)
