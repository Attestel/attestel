"""Feature/label construction for the directional classifier.

Pulls the indicator-enriched frame from the analysis service (the SAME features the UI shows), then
derives a compact set of STATIONARY model features (oscillators + distances-to-MA + returns), lags
them one bar (belt-and-suspenders against look-ahead), and builds a forward-horizon binary label.

Cardinal rule (docs/PREDICTION_MODELS.md): features at row t use ONLY bars up to t; after the extra
lag, X_t uses only bars up to t-1. The label uses the forward window t..t+H. Rows without a full
lagged-feature row or a full forward window are dropped.
"""
from __future__ import annotations

from dataclasses import dataclass

import pandas as pd
import requests

from .committee_features import COMMITTEE_FEATURES, build_committee_features
from .config import ANALYSIS_URL
from .context import (
    CONTEXT_FEATURES,
    EARNINGS_FEATURES,
    build_context_features,
    build_earnings_features,
)

# The model's stationary feature set, derived from the raw indicator columns. Kept small and
# interpretable on purpose (LightGBM feature importances stay meaningful).
PRICE_FEATURES = [
    "rsi",
    "macd_hist",
    "stoch_k",
    "stoch_d",
    "adx",
    "atr_pct",     # atr / close
    "dist_sma50",  # close / sma50 - 1
    "dist_sma200", # close / sma200 - 1
    "dist_ema20",  # close / ema20 - 1
    "bb_pos",      # position of close within the Bollinger band
    "ret1",        # 1-bar return
    "ret5",        # 5-bar return
    "vol_ratio",   # volume / 20-bar avg volume
]
# ...plus ORTHOGONAL context (market/sector, earnings cycle) from context.py, plus the analyst-
# committee stance history (committee_features.py — neutral until enough real snapshots exist).
# These all fail soft to neutral constants (zero-keys invariant), and are lagged with everything
# else in build_dataset.
MODEL_FEATURES = PRICE_FEATURES + CONTEXT_FEATURES + EARNINGS_FEATURES + COMMITTEE_FEATURES

# A trained record must name the bar-completeness policy it used. Records made before this field
# can contain a still-forming live candle and are refused until retrained.
FEATURE_FRAME_POLICY = "completed-bars-v1"


@dataclass
class Dataset:
    X: pd.DataFrame        # lagged feature matrix (rows = decision points)
    y: pd.Series           # binary label: 1 if forward H-bar return > 0
    ret_next: pd.Series    # next-bar simple return, aligned to X (for the backtest)
    index: pd.Index        # the time index of the valid rows


def fetch_feature_frame(
    ticker: str, timeframe: str = "1D", lookback_days: int = 1500, analysis_url: str | None = None
) -> tuple[pd.DataFrame, str, bool]:
    """GET the enriched frame from analysis. Returns (df, source, sourceIsSynthetic)."""
    base = (analysis_url or ANALYSIS_URL).rstrip("/")
    resp = requests.get(
        f"{base}/analysis/{ticker.upper()}/features",
        params={"timeframe": timeframe, "lookbackDays": lookback_days, "completedOnly": "true"},
        timeout=60,
    )
    resp.raise_for_status()
    data = resp.json()
    rows = data.get("rows") or []
    df = pd.DataFrame(rows)
    if "time" in df.columns:
        df = df.set_index("time")
    for c in df.columns:
        df[c] = pd.to_numeric(df[c], errors="coerce")
    return df, data.get("source", "unknown"), bool(data.get("sourceIsSynthetic", False))


def build_features(
    df: pd.DataFrame,
    ctx: dict | None = None,
    earnings: list | None = None,
    committee: list | None = None,
) -> pd.DataFrame:
    """Derive the stationary MODEL_FEATURES from the raw indicator frame, plus context/earnings/
    committee features (neutral constants when the extras are None — the zero-keys path). All
    columns are CAUSAL (use only the current and past bars; committee snapshots are prior-day-only).
    No shifting here — the lag is in build_dataset."""
    close = df["close"]
    out = pd.DataFrame(index=df.index)
    out["rsi"] = df["rsi"]
    out["macd_hist"] = df["macd_hist"]
    out["stoch_k"] = df["stoch_k"]
    out["stoch_d"] = df["stoch_d"]
    out["adx"] = df["adx"]
    out["atr_pct"] = df["atr"] / close
    out["dist_sma50"] = close / df["sma50"] - 1.0
    out["dist_sma200"] = close / df["sma200"] - 1.0
    out["dist_ema20"] = close / df["ema20"] - 1.0
    band = (df["bb_upper"] - df["bb_lower"]).replace(0.0, pd.NA)
    out["bb_pos"] = (close - df["bb_lower"]) / band
    out["ret1"] = close.pct_change(1)
    out["ret5"] = close.pct_change(5)
    out["vol_ratio"] = df["volume"] / df["vol_sma20"]
    out = pd.concat(
        [
            out,
            build_context_features(close, ctx),
            build_earnings_features(df.index, earnings or []),
            build_committee_features(df.index, committee or []),
        ],
        axis=1,
    )
    return out[MODEL_FEATURES]


def forward_return(close: pd.Series, horizon: int) -> pd.Series:
    """H-bar forward simple return at row t: close_{t+H}/close_t - 1 (NaN for the last H rows)."""
    return close.shift(-horizon) / close - 1.0


def build_dataset(
    df: pd.DataFrame,
    horizon: int,
    lag: int = 1,
    ctx: dict | None = None,
    earnings: list | None = None,
    committee: list | None = None,
) -> Dataset:
    """Assemble (X, y, ret_next) for training + backtest.

    X_t = features_{t-lag}      (lagged -> uses only data strictly before the decision bar)
    y_t = 1[ forward H-bar return from t > 0 ]
    ret_next_t = close_{t+1}/close_t - 1   (what a position opened at t earns next bar)

    Rows missing any lagged feature, the forward label, or the next-bar return are dropped.
    ctx/earnings/committee (context.py, committee_features.py) flow into build_features and are
    lagged identically.
    """
    feats = build_features(df, ctx, earnings, committee).shift(lag)
    fwd = forward_return(df["close"], horizon)
    ret_next = df["close"].pct_change().shift(-1)  # return earned over the bar AFTER t

    valid = feats.notna().all(axis=1) & fwd.notna() & ret_next.notna()
    X = feats[valid].astype(float)
    y = (fwd[valid] > 0).astype(int)
    rn = ret_next[valid].astype(float)
    return Dataset(X=X, y=y, ret_next=rn, index=X.index)


def latest_feature_row(
    df: pd.DataFrame,
    lag: int = 1,
    ctx: dict | None = None,
    earnings: list | None = None,
    committee: list | None = None,
) -> tuple[pd.Series | None, object]:
    """The most recent usable (lagged) feature row for a live prediction, plus the as-of time.
    Returns (None, asOf) if the last lagged row has any missing feature."""
    feats = build_features(df, ctx, earnings, committee).shift(lag)
    if len(feats) == 0:
        return None, None
    as_of = df.index[-1]
    row = feats.iloc[-1]
    if row.isna().any():
        return None, as_of
    return row.astype(float), as_of
