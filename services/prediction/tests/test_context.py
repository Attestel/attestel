"""context.py — earnings + market/sector features: no look-ahead, fail-soft neutrality, causality.

Run:  cd services/prediction && python -m pytest -q tests/test_context.py
"""
import numpy as np
import pandas as pd

from app.context import (
    CONTEXT_FEATURES,
    EARNINGS_FEATURES,
    MARKET_TICKER,
    SECTOR_TICKER,
    build_context_features,
    build_earnings_features,
)
from app.committee_features import COMMITTEE_FEATURES
from app.events import EarningsEvent
from app.features import MODEL_FEATURES, PRICE_FEATURES, build_features


def _close(n=100, seed=1):
    rng = np.random.default_rng(seed)
    idx = pd.bdate_range(end="2026-07-01", periods=n)
    return pd.Series(np.abs(100 + np.cumsum(rng.normal(0, 1, n))) + 10, index=idx)


def _ev(reported, sue):
    return EarningsEvent(ticker="NVDA", fiscal_date=reported, reported_date=reported,
                         reported_eps=1.0, estimated_eps=0.9, surprise=0.1, surprise_pct=11.0,
                         sue=sue)


def test_model_features_extended():
    assert MODEL_FEATURES == PRICE_FEATURES + CONTEXT_FEATURES + EARNINGS_FEATURES + COMMITTEE_FEATURES


def test_neutral_fill_without_context_or_earnings():
    """The zero-keys path: no ctx, no earnings -> inert constants, every row still valid."""
    close = _close()
    ctx_f = build_context_features(close, None)
    assert (ctx_f == 0.0).all().all()
    earn_f = build_earnings_features(close.index, [])
    assert (earn_f["days_since_earn"] == 1.0).all()
    assert (earn_f["last_sue"] == 0.0).all()


def test_earnings_visible_strictly_after_reported_date():
    """The 'public by then' rule: the bar ON reportedDate must NOT see that event yet."""
    idx = pd.bdate_range("2026-01-01", periods=60)
    reported = idx[30].strftime("%Y-%m-%d")
    out = build_earnings_features(idx, [_ev(reported, sue=2.0)])
    assert out["last_sue"].iloc[30] == 0.0            # same-day: not visible
    assert out["last_sue"].iloc[31] == 2.0            # next bar: visible
    assert out["days_since_earn"].iloc[31] == 1.0 / 90.0
    assert (out["last_sue"].iloc[:31] == 0.0).all()   # nothing before it, ever


def test_earnings_cap_and_clip():
    idx = pd.bdate_range("2026-01-01", periods=300)
    out = build_earnings_features(idx, [_ev("2026-01-02", sue=-12.0)])
    assert out["last_sue"].min() == -5.0              # clipped to ±5
    assert out["days_since_earn"].iloc[-1] == 1.0     # capped at 90 days


def test_context_features_are_causal():
    """Perturbing a FUTURE spy/smh bar must not change earlier context features."""
    close = _close(120)
    rng = np.random.default_rng(7)
    spy = pd.Series(np.abs(400 + np.cumsum(rng.normal(0, 2, 120))), index=close.index)
    smh = pd.Series(np.abs(200 + np.cumsum(rng.normal(0, 2, 120))), index=close.index)
    base = build_context_features(close, {MARKET_TICKER: spy, SECTOR_TICKER: smh})
    k = 90
    spy2, smh2 = spy.copy(), smh.copy()
    spy2.iloc[k] *= 1.5
    smh2.iloc[k] *= 1.5
    pert = build_context_features(close, {MARKET_TICKER: spy2, SECTOR_TICKER: smh2})
    assert np.allclose(base.to_numpy()[:k], pert.to_numpy()[:k], rtol=1e-9, atol=1e-9)


def test_partial_context_fails_soft_per_column():
    close = _close()
    spy = close * 4.0
    out = build_context_features(close, {MARKET_TICKER: spy})  # SMH missing
    assert (out["rel_sect20"] == 0.0).all()
    assert not (out["mkt_ret5"] == 0.0).all()


def test_build_features_produces_full_column_set():
    """build_features must emit exactly MODEL_FEATURES with or without context/earnings."""
    n = 60
    rng = np.random.default_rng(5)
    close = _close(n).to_numpy()
    df = pd.DataFrame(
        {
            "close": close,
            "volume": rng.integers(1_000_000, 5_000_000, n).astype(float),
            "rsi": rng.uniform(20, 80, n),
            "macd_hist": rng.normal(0, 1, n),
            "stoch_k": rng.uniform(0, 100, n),
            "stoch_d": rng.uniform(0, 100, n),
            "adx": rng.uniform(10, 40, n),
            "atr": rng.uniform(1, 5, n),
            "sma50": close * 1.01,
            "sma200": close * 0.99,
            "ema20": close * 1.0,
            "bb_upper": close * 1.05,
            "bb_lower": close * 0.95,
            "vol_sma20": rng.integers(1_000_000, 5_000_000, n).astype(float),
        },
        index=pd.bdate_range(end="2026-07-01", periods=n),
    )
    out = build_features(df)
    assert list(out.columns) == MODEL_FEATURES
    out2 = build_features(df, {MARKET_TICKER: df["close"] * 4}, [_ev("2026-05-01", 1.5)])
    assert list(out2.columns) == MODEL_FEATURES
