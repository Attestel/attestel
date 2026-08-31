"""Leakage tests for feature/label construction (prediction phase / DoD #1).

Asserts (a) no model feature is computed from bars later than t, and (b) the lag + forward-label
alignment is exactly as intended: X_t uses features from t-1, y_t uses the forward H-bar window.

Run:  cd services/prediction && python -m pytest -q tests/test_features_leakage.py
"""
import numpy as np
import pandas as pd

from app import features
from app.features import MODEL_FEATURES, build_dataset, build_features


def _enriched(n: int = 200, seed: int = 3) -> pd.DataFrame:
    """A stand-in enriched frame with all raw columns build_features consumes. Values are arbitrary
    but finite; the leakage guarantee is structural, not value-dependent."""
    rng = np.random.default_rng(seed)
    close = 100 + np.cumsum(rng.normal(0, 1, n))
    close = np.abs(close) + 10
    idx = pd.bdate_range(end="2026-07-01", periods=n)
    return pd.DataFrame(
        {
            "close": close,
            "volume": rng.integers(1_000_000, 5_000_000, n).astype(float),
            "rsi": rng.uniform(20, 80, n),
            "macd_hist": rng.normal(0, 1, n),
            "stoch_k": rng.uniform(0, 100, n),
            "stoch_d": rng.uniform(0, 100, n),
            "adx": rng.uniform(10, 40, n),
            "atr": rng.uniform(1, 5, n),
            "sma50": close * rng.uniform(0.97, 1.03, n),
            "sma200": close * rng.uniform(0.95, 1.05, n),
            "ema20": close * rng.uniform(0.98, 1.02, n),
            "bb_upper": close * 1.05,
            "bb_lower": close * 0.95,
            "vol_sma20": rng.integers(1_000_000, 5_000_000, n).astype(float),
        },
        index=idx,
    )


def test_build_features_is_causal():
    """Perturbing a future bar k must not change any feature row before k."""
    df = _enriched(200)
    base = build_features(df)
    k = 150
    df2 = df.copy()
    for col in ("close", "volume", "rsi", "atr", "sma50"):
        df2.iloc[k, df2.columns.get_loc(col)] *= 1.5
    pert = build_features(df2)
    a, b = base.to_numpy()[:k], pert.to_numpy()[:k]
    amask, bmask = np.isnan(a), np.isnan(b)
    assert np.array_equal(amask, bmask)
    assert np.allclose(a[~amask], b[~bmask], rtol=1e-9, atol=1e-9)


def test_lag_and_forward_label_alignment():
    """X_t must equal features_{t-1}; y_t must reflect the forward H-bar return."""
    df = _enriched(120)
    H = 5
    ds = build_dataset(df, horizon=H, lag=1)
    feats = build_features(df)

    # pick a valid decision time and confirm X uses the PRIOR bar's features
    t = ds.index[10]
    pos = df.index.get_loc(t)
    prior_time = df.index[pos - 1]
    for c in MODEL_FEATURES:
        assert np.isclose(ds.X.loc[t, c], feats.loc[prior_time, c], rtol=1e-9)

    # label matches the sign of the forward H-bar return
    fwd_ret = df["close"].iloc[pos + H] / df["close"].iloc[pos] - 1.0
    assert ds.y.loc[t] == (1 if fwd_ret > 0 else 0)


def test_forward_window_rows_dropped():
    """The last H rows have no forward label and must not appear in the dataset."""
    df = _enriched(80)
    H = 5
    ds = build_dataset(df, horizon=H, lag=1)
    # none of the last H timestamps should be a decision row
    assert not set(df.index[-H:]).intersection(set(ds.index))
    # monotonic-up close would give all-up labels; here just assert y is binary
    assert set(ds.y.unique()).issubset({0, 1})


def test_dataset_perturbing_future_leaves_earlier_X_unchanged():
    df = _enriched(150)
    ds = build_dataset(df, horizon=5, lag=1)
    k = 140
    df2 = df.copy()
    df2.iloc[k, df2.columns.get_loc("close")] *= 2.0
    ds2 = build_dataset(df2, horizon=5, lag=1)
    common = ds.index[ds.index < df.index[k - 6]]  # rows safely before the perturbation window
    a = ds.X.loc[common].to_numpy()
    b = ds2.X.loc[common].to_numpy()
    assert np.allclose(a, b, rtol=1e-9, atol=1e-9)


def test_feature_fetch_requests_completed_bars(monkeypatch):
    """Training/evaluation must never receive today's still-forming daily candle."""
    captured = {}

    class Response:
        def raise_for_status(self):
            return None

        def json(self):
            return {
                "rows": [{"time": "2026-08-24", "close": 100.0}],
                "source": "yfinance",
                "sourceIsSynthetic": False,
            }

    def fake_get(url, params, timeout):
        captured.update(url=url, params=params, timeout=timeout)
        return Response()

    monkeypatch.setattr(features.requests, "get", fake_get)
    frame, source, synthetic = features.fetch_feature_frame("nvda", analysis_url="http://analysis")

    assert captured["params"]["completedOnly"] == "true"
    assert captured["url"] == "http://analysis/analysis/NVDA/features"
    assert list(frame.index) == ["2026-08-24"]
    assert source == "yfinance"
    assert synthetic is False
