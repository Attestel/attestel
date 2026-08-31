"""Orthogonal context features — market/sector context (SPY, SMH) and earnings (SUE, recency).

Why: the 13 price-derived technicals all look at the same NVDA series. These features add
information the model literally could not see before: what the MARKET and the SECTOR are doing,
and where we are in the EARNINGS cycle (post-earnings drift is one of the few documented,
persistent anomalies — see docs/PREDICTION_MODELS.md).

Everything here FAILS SOFT to neutral constants, so the zero-keys / zero-network invariant holds:
missing context -> 0.0 columns; missing earnings cache -> (days_since_earn=1.0, last_sue=0.0).
Neutral constants produce no LightGBM splits, so offline/demo behavior is unchanged.

NO LOOK-AHEAD:
  * context closes are as-of the same bar dates as the ticker frame, then lagged one extra bar in
    build_dataset like every other feature;
  * earnings features at bar date d use only events reported STRICTLY BEFORE d (the same
    "public by then" rule as the event-study entry in events.py), and SUE is prior-only
    (events.compute_sue).
  * synthetic context is DISCARDED (never joined onto real bars) — see fetch_context.
"""
from __future__ import annotations

import bisect
from datetime import date

import numpy as np
import pandas as pd

from .events import EarningsEvent, compute_sue, get_earnings

MARKET_TICKER = "SPY"   # broad market
SECTOR_TICKER = "SMH"   # semiconductor sector ETF

CONTEXT_FEATURES = [
    "mkt_ret5",     # SPY 5-bar return (market tailwind/headwind)
    "mkt_vol20",    # SPY 20-bar return std (market vol regime; free VIX proxy)
    "rel_sect20",   # ticker 20-bar return minus SMH 20-bar return (sector relative strength)
]
EARNINGS_FEATURES = [
    "days_since_earn",  # min(days since last report, 90)/90 — 1.0 = long ago / unknown
    "last_sue",         # standardized surprise of the LAST PRIOR report, clipped to ±5; 0 = unknown
]

DAYS_SINCE_CAP = 90.0
SUE_CLIP = 5.0


# ------------------------------------------------------------------------ fetch (fail-soft)

def fetch_context(
    timeframe: str, lookback_days: int, analysis_url: str | None = None
) -> dict[str, pd.Series] | None:
    """SPY + SMH close series via the analysis service. Returns {ticker: close} or None.
    Fail-soft on any error, and REFUSES synthetic sources: joining synthetic SPY onto real NVDA
    bars would inject junk, and in full-synthetic demo mode neutral fills are the honest value."""
    from .features import fetch_feature_frame  # local import — features.py imports this module

    ctx: dict[str, pd.Series] = {}
    for t in (MARKET_TICKER, SECTOR_TICKER):
        try:
            df, _source, is_syn = fetch_feature_frame(t, timeframe, lookback_days, analysis_url)
        except Exception:  # noqa: BLE001 — analysis down / unknown ticker: neutral fill
            continue
        if is_syn or "close" not in df.columns:
            continue
        close = df["close"].dropna()
        if len(close):
            ctx[t] = close
    return ctx or None


def load_earnings(ticker: str, cache_dir: str, api_key: str = "") -> list[EarningsEvent]:
    """Cache-first earnings history with prior-only SUE filled in. With api_key="" this is
    cache-ONLY (never burns Alpha Vantage quota) — pass the key to allow a one-time fetch+cache.
    Returns [] when nothing is available (-> neutral features)."""
    try:
        events, _src = get_earnings(ticker, cache_dir, api_key)
    except Exception:  # noqa: BLE001 — corrupt cache / IO error: neutral fill
        return []
    if events:
        compute_sue(events)
    return events


# ------------------------------------------------------------------------ build (pure, causal)

def build_context_features(close: pd.Series, ctx: dict[str, pd.Series] | None) -> pd.DataFrame:
    """CONTEXT_FEATURES aligned to close.index. All columns causal (rolling/pct_change over past
    bars only). Missing context -> neutral 0.0; leading warm-up NaNs -> 0.0 ("no information")."""
    out = pd.DataFrame(index=close.index)
    spy = ctx.get(MARKET_TICKER) if ctx else None
    smh = ctx.get(SECTOR_TICKER) if ctx else None

    if spy is not None:
        s = spy.reindex(close.index).ffill()
        out["mkt_ret5"] = s.pct_change(5).fillna(0.0)
        out["mkt_vol20"] = s.pct_change().rolling(20).std().fillna(0.0)
    else:
        out["mkt_ret5"] = 0.0
        out["mkt_vol20"] = 0.0

    if smh is not None:
        m = smh.reindex(close.index).ffill()
        out["rel_sect20"] = (close.pct_change(20) - m.pct_change(20)).fillna(0.0)
    else:
        out["rel_sect20"] = 0.0
    return out[CONTEXT_FEATURES]


def _bar_date(idx_value) -> date | None:
    try:
        return date.fromisoformat(str(idx_value)[:10])
    except ValueError:
        return None


def build_earnings_features(index: pd.Index, events: list[EarningsEvent]) -> pd.DataFrame:
    """EARNINGS_FEATURES aligned to index. For bar date d, only events with
    reported_date STRICTLY BEFORE d are visible (public by then). No events -> neutral."""
    out = pd.DataFrame(index=index)
    if not events:
        out["days_since_earn"] = 1.0
        out["last_sue"] = 0.0
        return out

    reported = [e.reported_date[:10] for e in events]  # oldest-first (parse_earnings sorts)
    days = np.full(len(index), 1.0)
    sues = np.zeros(len(index))
    for i, idx_value in enumerate(index):
        d = _bar_date(idx_value)
        if d is None:
            continue
        j = bisect.bisect_left(reported, d.isoformat())  # events with reported_date < d
        if j == 0:
            continue  # no PRIOR event -> neutral
        ev = events[j - 1]
        try:
            delta = (d - date.fromisoformat(ev.reported_date[:10])).days
        except ValueError:
            continue
        days[i] = min(max(float(delta), 0.0), DAYS_SINCE_CAP) / DAYS_SINCE_CAP
        sues[i] = float(np.clip(ev.sue, -SUE_CLIP, SUE_CLIP)) if ev.sue is not None else 0.0
    out["days_since_earn"] = days
    out["last_sue"] = sues
    return out[EARNINGS_FEATURES]
