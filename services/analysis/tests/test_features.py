"""Feature-matrix leakage tests (prediction phase / DoD #1).

The prediction service trains on GET /analysis/{ticker}/features. The whole backtest is only honest
if those features are CAUSAL: row t must be computable from bars up to t and must NOT change when a
FUTURE bar changes. These tests prove exactly that, offline, on synthetic data.

Run:  cd services/analysis && python -m pytest -q tests/test_features.py
"""
import numpy as np
import pandas as pd
from datetime import datetime, timezone
from fastapi.testclient import TestClient

from app.indicators import add_indicators
from app.main import FEATURE_COLUMNS, _completed_feature_rows, app


def _frame(n: int = 300, seed: int = 7) -> pd.DataFrame:
    rng = np.random.default_rng(seed)
    steps = rng.normal(0.0005, 0.02, n).cumsum()
    close = 100 * np.exp(steps)
    dates = pd.bdate_range(end="2026-07-01", periods=n)
    return pd.DataFrame(
        {
            "open": close * (1 + rng.normal(0, 0.003, n)),
            "high": np.maximum(close, close) * 1.01,
            "low": close * 0.99,
            "close": close,
            "volume": rng.integers(1_000_000, 5_000_000, n).astype(float),
        },
        index=dates,
    )


def test_indicators_are_causal_no_future_leakage():
    """Perturbing a FUTURE bar must not change any earlier feature row. If it does, the indicator
    peeks ahead — the cardinal sin the backtest must avoid."""
    df = _frame(300)
    base = add_indicators(df)

    k = 250  # perturb this future bar hard
    df2 = df.copy()
    df2.iloc[k, df2.columns.get_loc("close")] *= 1.5
    df2.iloc[k, df2.columns.get_loc("high")] *= 1.5
    df2.iloc[k, df2.columns.get_loc("low")] *= 1.5
    df2.iloc[k, df2.columns.get_loc("volume")] *= 3.0
    pert = add_indicators(df2)

    # Every feature column, for every row strictly before k, must be identical.
    causal_cols = [c for c in FEATURE_COLUMNS if c != "vwap"]  # vwap is all-NaN on daily
    for c in causal_cols:
        a = base[c].to_numpy()[:k]
        b = pert[c].to_numpy()[:k]
        # NaNs must line up too (warmup region)
        assert np.array_equal(np.isnan(a), np.isnan(b)), f"{c}: NaN mask changed before k"
        mask = ~np.isnan(a)
        assert np.allclose(a[mask], b[mask], rtol=1e-9, atol=1e-9), f"{c}: value changed before future bar {k}"


def test_perturbing_bar_k_changes_row_k_onward():
    """Sanity counterpart: the future bar DOES affect row k onward (so the test above isn't vacuous)."""
    df = _frame(300)
    base = add_indicators(df)
    k = 250
    df2 = df.copy()
    df2.iloc[k, df2.columns.get_loc("close")] *= 1.5
    pert = add_indicators(df2)
    assert not np.isclose(base["rsi"].to_numpy()[k], pert["rsi"].to_numpy()[k], equal_nan=True)


def test_features_endpoint_shape_offline(monkeypatch):
    # Force the genuine offline/synthetic path deterministically, independent of the test host's
    # network: unset live keys and make the no-key provider (yfinance) fail, so fetch_ohlcv falls
    # through to the synthetic fallback — exactly the zero-key/zero-network behavior asserted here.
    import app.data as data

    for var in ("APCA_API_KEY_ID", "APCA_API_SECRET_KEY", "TWELVEDATA_API_KEY", "TIINGO_API_KEY"):
        monkeypatch.delenv(var, raising=False)

    def _offline(*a, **k):
        raise data.DataError("offline (test)")

    monkeypatch.setattr(data, "_from_yfinance", _offline)

    client = TestClient(app)
    r = client.get("/analysis/NVDA/features", params={"timeframe": "1D", "lookbackDays": 800})
    assert r.status_code == 200
    body = r.json()
    assert body["sourceIsSynthetic"] is True  # offline -> synthetic
    assert body["featureColumns"] == FEATURE_COLUMNS
    assert len(body["rows"]) >= 700  # lookback honored by synthetic fallback
    row = body["rows"][-1]
    assert "time" in row and "close" in row
    for c in FEATURE_COLUMNS:
        assert c in row
    # daily vwap is not meaningful -> None everywhere
    assert all(r_["vwap"] is None for r_ in body["rows"])


def test_completed_feature_rows_exclude_a_forming_daily_bar():
    now = datetime(2026, 8, 25, 16, 0, tzinfo=timezone.utc)
    frame = pd.DataFrame(
        {"close": [100.0, 101.0]},
        index=pd.DatetimeIndex(["2026-08-24", "2026-08-25"]),
    )
    completed = _completed_feature_rows(frame, "1D", now)
    assert list(completed.index.strftime("%Y-%m-%d")) == ["2026-08-24"]
