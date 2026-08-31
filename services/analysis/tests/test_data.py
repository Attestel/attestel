"""Price-layer tests — deterministic, no network, no keys.

Covers the provider-chain guardrails: the timeframe->interval maps are complete and consistent
across every provider, the synthetic fallback is well-formed and deterministic, key-gated providers
raise (so the chain falls through) when unkeyed, and the synthetic path stays tagged synthetic.
Run:  cd services/analysis && python -m pytest -q
"""
import pytest

from app.data import (
    COLS,
    TIMEFRAMES,
    TWELVEDATA_INTERVALS,
    YFINANCE_TIMEFRAMES,
    DataError,
    _from_alpaca,
    _from_twelvedata,
    _synthetic,
    _validate,
    normalize_timeframe,
)

SUPPORTED = {"1D", "1H", "15m", "5m"}


def test_supported_timeframes():
    assert set(TIMEFRAMES) == SUPPORTED


def test_twelvedata_interval_map_complete():
    assert set(TWELVEDATA_INTERVALS) == SUPPORTED
    for interval in TWELVEDATA_INTERVALS.values():
        assert isinstance(interval, str) and interval


def test_yfinance_map_complete():
    assert set(YFINANCE_TIMEFRAMES) == SUPPORTED
    for interval, period, fallback in YFINANCE_TIMEFRAMES.values():  # exactly 3 fields each
        assert interval and period
        assert fallback is None or isinstance(fallback, str)
    # only the 60m/1H interval needs a shorter-period retry (Yahoo can reject 730d)
    assert YFINANCE_TIMEFRAMES["1H"] == ("60m", "730d", "60d")


def test_synthetic_frame_valid_and_deterministic():
    a = _synthetic("NVDA", "1D", n=260)
    b = _synthetic("NVDA", "1D", n=260)
    assert list(a.columns) == COLS
    assert len(a) == 260
    assert a["close"].round(6).equals(b["close"].round(6))  # deterministic within a process
    _validate(a, "NVDA")  # passes the same validation the fetch chain applies


@pytest.mark.parametrize("tf", sorted(SUPPORTED))
def test_synthetic_every_timeframe(tf):
    df = _validate(_synthetic("NVDA", tf, n=120), "NVDA")
    assert not df.empty and list(df.columns) == COLS


def test_unkeyed_providers_raise(monkeypatch):
    # Key-gated providers must raise (fall through) with no key set — checked before any network.
    for var in ("APCA_API_KEY_ID", "APCA_API_SECRET_KEY", "TWELVEDATA_API_KEY"):
        monkeypatch.delenv(var, raising=False)
    with pytest.raises(DataError):
        _from_alpaca("NVDA", "1D")
    with pytest.raises(DataError):
        _from_twelvedata("NVDA", "1D")


def test_normalize_timeframe_aliases():
    assert normalize_timeframe("daily") == "1D"
    assert normalize_timeframe("1h") == "1H"
    assert normalize_timeframe("15min") == "15m"
    assert normalize_timeframe(None) == "1D"
    assert normalize_timeframe("bogus") == "1D"
