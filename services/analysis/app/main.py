"""Analysis service (Python · FastAPI).

Owns the deterministic layer from the original doc: fetch OHLCV -> indicators -> regime.
No LLM here. The gateway calls these endpoints and merges the result into the dashboard.

Swing-first phase: every endpoint takes a `timeframe` (1D/1H/15m/5m); a cheap /quote endpoint
serves the fresh header price. 1D is the default and reproduces the original daily behavior.
"""
from __future__ import annotations

import math
from datetime import datetime, timedelta, timezone

import pandas as pd
from fastapi import FastAPI, HTTPException
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import JSONResponse

from . import store
from .candles import add_geometry, candle_payload, extended_limit
from .confluence import build_confluence
from .data import (
    COVERAGE_INSUFFICIENT,
    WEEKLY,
    AsOfError,
    BarWindow,
    DataError,
    is_intraday,
    latest_quote,
    load_bars,
    normalize_timeframe,
    resolve_as_of,
)
from .expected import expected_close, expected_close_from_row
from .indicators import add_indicators
from .regime import detect_regime
from .portfolio_risk import PortfolioRiskInputError, build_portfolio_risk
from .structure import build_confluence_from_regimes, build_structure

CONFLUENCE_DEFAULT = "1D,1H,15m"

# Bars read beyond what an endpoint returns, so SMA200 and the EMAs are warm at the first returned
# bar rather than degrading to null there.
INDICATOR_WARMUP = 250

# Bars /technical-context and /multi-timeframe-context build their structure over.
TECHNICAL_WINDOW = 260

# doc §14.5: the swing default is weekly + daily + hourly.
HORIZON_TIMEFRAMES = {
    "1d": ["1H", "1D"],
    "5d": ["1H", "1D", WEEKLY],
    "20d": ["1D", WEEKLY],
    "60d": ["1D", WEEKLY],
}
HORIZON_DEFAULT = ["1H", "1D", WEEKLY]

app = FastAPI(title="NVDA Platform — Analysis Service", version="0.4.0")
app.add_middleware(
    CORSMiddleware, allow_origins=["*"], allow_methods=["*"], allow_headers=["*"]
)


def _clean(v):
    if isinstance(v, float) and (math.isnan(v) or math.isinf(v)):
        return None
    return v


def _fmt_time(idx, intraday: bool):
    """Chart time key. Daily -> 'YYYY-MM-DD' (business day, unchanged). Intraday -> UNIX seconds
    (UTC) so multiple bars per day stay distinct and lightweight-charts renders them correctly."""
    if intraday:
        return int(pd.Timestamp(idx).timestamp())
    return pd.Timestamp(idx).strftime("%Y-%m-%d")


def _ohlcv_payload(df: pd.DataFrame, intraday: bool, n: int = 260) -> list[dict]:
    tail = df.tail(n)
    return [
        {
            "time": _fmt_time(idx, intraday),
            "open": round(float(r.open), 2),
            "high": round(float(r.high), 2),
            "low": round(float(r.low), 2),
            "close": round(float(r.close), 2),
            "volume": int(r.volume),
        }
        for idx, r in tail.iterrows()
    ]


def _indicator_series(df: pd.DataFrame, intraday: bool, n: int = 260) -> dict:
    tail = df.tail(n)
    def series(col):
        return [
            {"time": _fmt_time(idx, intraday), "value": _clean(round(float(v), 4)) if pd.notna(v) else None}
            for idx, v in tail[col].items()
        ]
    return {
        "sma50": series("sma50"),
        "sma200": series("sma200"),
        "ema20": series("ema20"),
        "bbUpper": series("bb_upper"),
        "bbLower": series("bb_lower"),
        "rsi": series("rsi"),
        "macd": series("macd"),
        "macdSignal": series("macd_signal"),
        "macdHist": series("macd_hist"),
    }


def _reject_weekly(tf: str) -> None:
    """1W is served by the three point-in-time endpoints only.

    The four endpoints that predate this lane cannot honour it: `expected.py`'s `_BAR_MINUTES` has
    no weekly entry and `confluence.TF_RANK` would sort the highest timeframe last — and neither
    file is this lane's to change. Before 1W existed these endpoints silently coerced it to 1D via
    `normalize_timeframe`'s unknown-value default; failing loudly on a brand-new value beats
    answering with daily bars under a weekly label.
    """
    if tf == WEEKLY:
        raise HTTPException(
            status_code=400,
            detail="1W is available on /candles, /technical-context and /multi-timeframe-context",
        )


def _window(
    ticker: str,
    tf: str,
    as_of: str | None,
    limit: int | None,
    n: int = 260,
    lookback_days: int | None = None,
) -> BarWindow:
    """Every endpoint's single door to bars. Converts a malformed as_of into a 400 rather than
    letting it degrade to "now" — a silent "now" on a typo'd cutoff is a leak that looks like data."""
    try:
        return load_bars(
            ticker, tf, as_of=as_of, limit=limit, n=n, lookback_days=lookback_days
        )
    except AsOfError as e:
        raise HTTPException(status_code=400, detail=str(e)) from e


class InsufficientCoverage(Exception):
    """A historical request the store cannot satisfy (contract §9.38.1).

    Carried as an exception rather than returned, so the four pre-existing endpoints can signal it
    from anywhere in their body without restructuring around an early return.
    """

    def __init__(self, window: BarWindow):
        self.window = window
        super().__init__("insufficient_coverage")


def _insufficient_coverage_response(window: BarWindow) -> JSONResponse:
    """§9.38.1's body. Deliberately NOT an `HTTPException`, which would wrap this in `detail`.

    The four pre-existing endpoints have no in-body `coverage` field — §4.4 grants that only to the
    three new ones — so this status is how they say "I cannot serve this cutoff". An error body is a
    different shape from a success body anyway, so the byte-identity guarantee, which only ever
    covered successful requests without `as_of`, is untouched.
    """
    return JSONResponse(
        status_code=409,
        content={
            "error": "insufficient_coverage",
            "coverageFrom": window.coverage_from,
            "coverageTo": window.coverage_to,
        },
    )


@app.exception_handler(InsufficientCoverage)
def _handle_insufficient_coverage(_request, exc: InsufficientCoverage) -> JSONResponse:
    return _insufficient_coverage_response(exc.window)


def _enriched(window: BarWindow, tf: str, min_rows: int = 1):
    """Indicator-enriched frame from a resolved window, or a refusal when the store cannot serve it.

    Two different refusals, because they are two different failures:

    - **Historical** (`as_of` in the past) and the store lacks coverage -> **409** with §9.38.1's
      body. There is no fallback fetch, so this is a permanent, informative "not in the record",
      not a transient upstream problem.
    - **Live** and the frame is unusable -> the pre-existing **502**, unchanged, because that is
      what these endpoints already answer on a `DataError` and live behaviour is byte-identical.
    """
    unusable = window.empty or len(window.df) < min_rows
    if window.historical and (unusable or window.coverage == COVERAGE_INSUFFICIENT):
        raise InsufficientCoverage(window)
    if unusable:
        raise HTTPException(
            status_code=502,
            detail=(
                f"data error: no bars for {tf} at or before {window.as_of} "
                f"(coverage: {window.coverage})"
            ),
        )
    return add_indicators(window.df, intraday=is_intraday(tf))


def _completed_feature_rows(df: pd.DataFrame, tf: str, now: datetime | None = None) -> pd.DataFrame:
    """Drop the newest bar while it is still forming, using the paper contract's §2.1 rule.

    Charts may legitimately show a live candle. A training/evaluation matrix may not label an
    unfinished close as though it were final, so `/features?completedOnly=true` uses this choke
    point while the default endpoint remains backwards-compatible for other readers.
    """
    if df is None or df.empty:
        return df
    now = (now or datetime.now(timezone.utc)).astimezone(timezone.utc)
    idx = pd.DatetimeIndex(pd.to_datetime(df.index, utc=True)).tz_convert(None)
    if tf == "1D":
        cutoff = pd.Timestamp(now.date())
        mask = idx.normalize() < cutoff
    else:
        duration = {"1H": timedelta(hours=1), "15m": timedelta(minutes=15),
                    "5m": timedelta(minutes=5)}.get(tf)
        if duration is None:
            return df.iloc[0:0]
        cutoff = pd.Timestamp(now.replace(tzinfo=None)) - duration
        mask = idx <= cutoff
    return df.loc[mask]


def _honest_expected_close(last_row, tf: str, window: BarWindow) -> dict:
    """Two corrections `expected.py` cannot make for itself, since it is not this lane's to change.

    1. `sourceIsSynthetic` there is an exact `source == "synthetic"` match, which a window mixing
       providers ("synthetic+yfinance") would silently fail. Re-imposed from the window — the
       honesty guardrail may be made prettier, never quieter.
    2. `sessionRemainingPct` defaults to the wall clock. On a historical request that makes the same
       cutoff return a different band every run, so the session fraction is evaluated AT the cutoff.
    """
    now = window.as_of_dt if window.historical else None
    payload = expected_close_from_row(last_row, tf, window.source, now=now)
    payload["sourceIsSynthetic"] = window.source_is_synthetic
    return payload


def _coverage_block(window: BarWindow) -> dict:
    return {
        "coverage": window.coverage,
        "coverageFrom": window.coverage_from,
        "coverageTo": window.coverage_to,
    }


@app.get("/health")
def health():
    return {"status": "ok", "service": "analysis", "barStore": store.store_health()}


@app.get("/quote/{ticker}")
def quote(ticker: str):
    """Cheap fresh quote for the header (polled by the frontend). Always available — falls back to
    the last close from synthetic/Tiingo bars when no Alpaca key is set."""
    try:
        return latest_quote(ticker.upper())
    except DataError as e:
        raise HTTPException(status_code=502, detail=f"quote error: {e}") from e


@app.post("/portfolio-risk")
def portfolio_risk(body: dict):
    """Deterministic historical portfolio risk from server-supplied position weights.

    The caller values holdings first; this endpoint owns only returns, volatility, beta, drawdown,
    and correlations. It remains usable offline because every history read follows the normal
    provider cascade to the clearly labelled synthetic fallback.
    """
    try:
        return build_portfolio_risk(body)
    except PortfolioRiskInputError as e:
        raise HTTPException(status_code=400, detail=str(e)) from e



@app.get("/analysis/{ticker}")
def analysis(ticker: str, timeframe: str = "1D", n: int = 260, as_of: str | None = None):
    ticker = ticker.upper()
    tf = normalize_timeframe(timeframe)
    _reject_weekly(tf)
    intraday = is_intraday(tf)
    window = _window(ticker, tf, as_of, limit=max(n, 260) + INDICATOR_WARMUP)
    source = window.source
    enriched = _enriched(window, tf, min_rows=2)
    last = enriched.iloc[-1]
    prev = enriched.iloc[-2]
    change_pct = round((float(last.close) / float(prev.close) - 1) * 100, 2)
    return {
        "ticker": ticker,
        "timeframe": tf,
        "source": source,
        "sourceIsSynthetic": window.source_is_synthetic,
        "price": {
            "lastClose": round(float(last.close), 2),
            "changePct": change_pct,
            "asOf": _fmt_time(enriched.index[-1], intraday),
            "ohlcv": _ohlcv_payload(enriched, intraday, n),
        },
        "indicators": {
            "latest": {
                "rsi": _clean(round(float(last.rsi), 2)) if pd.notna(last.rsi) else None,
                "macdHist": _clean(round(float(last.macd_hist), 4)),
                "sma50": _clean(round(float(last.sma50), 2)) if pd.notna(last.sma50) else None,
                "sma200": _clean(round(float(last.sma200), 2)) if pd.notna(last.sma200) else None,
                "ema20": _clean(round(float(last.ema20), 2)),
                "atr": _clean(round(float(last.atr), 2)) if pd.notna(last.atr) else None,
                "stochK": _clean(round(float(last.stoch_k), 2)) if pd.notna(last.stoch_k) else None,
                "stochD": _clean(round(float(last.stoch_d), 2)) if pd.notna(last.stoch_d) else None,
                "adx": _clean(round(float(last.adx), 2)) if pd.notna(last.adx) else None,
                "obv": _clean(int(last.obv)) if pd.notna(last.obv) else None,
                "vwap": _clean(round(float(last.vwap), 2)) if pd.notna(last.vwap) else None,
            },
            "series": _indicator_series(enriched, intraday, n),
        },
        "regime": detect_regime(enriched, timeframe=tf),
        # Deterministic volatility band for where THIS candle might close (not a prediction). Folded
        # in so the gateway dashboard gets it without an extra round-trip.
        "expectedClose": _honest_expected_close(last, tf, window),
    }


@app.get("/expected-close/{ticker}")
def expected_close_endpoint(ticker: str, timeframe: str = "1D", as_of: str | None = None):
    """Deterministic expected-close RANGE for the current candle: latest close ± ATR(14) scaled by the
    fraction of the bar/session remaining. NOT a prediction — a statistical band; price can gap
    through it. Computed here, never by the LLM (which only explains it)."""
    ticker = ticker.upper()
    tf = normalize_timeframe(timeframe)
    _reject_weekly(tf)
    if as_of is None:
        # Unchanged path: `expected_close` fetches internally, which is correct for "now".
        try:
            return expected_close(ticker, tf)
        except DataError as e:
            raise HTTPException(status_code=502, detail=f"data error: {e}") from e
    # Historical: `expected_close` would fetch, so the store-derived last row goes through the pure
    # `expected_close_from_row` that /analysis already uses.
    window = _window(ticker, tf, as_of, limit=INDICATOR_WARMUP + 60)
    enriched = _enriched(window, tf)
    return _honest_expected_close(enriched.iloc[-1], tf, window)


# Indicator columns exposed by the feature matrix — the SAME suite add_indicators computes and the
# UI shows, so training (prediction service) and display stay consistent. Order is stable.
FEATURE_COLUMNS = [
    "rsi", "macd", "macd_signal", "macd_hist", "sma50", "sma200", "ema20",
    "bb_mid", "bb_upper", "bb_lower", "atr", "vol_sma20",
    "stoch_k", "stoch_d", "obv", "adx", "vwap",
]


@app.get("/analysis/{ticker}/features")
def features(ticker: str, timeframe: str = "1D", lookbackDays: int = 1500,
             as_of: str | None = None, completedOnly: bool = False):
    """Historical rows of the indicator-enriched frame (time + OHLCV + every indicator column).

    This is the training feature matrix for the prediction service. It runs the SAME pipeline the
    dashboard uses (fetch_ohlcv -> add_indicators), so the model trains on exactly the features the
    UI displays. Indicators are CAUSAL (row t uses only bars up to t); the prediction service lags
    them one more bar before training. `lookbackDays` sizes the synthetic fallback AND extends the
    live providers' daily window, so training actually sees the requested history."""
    ticker = ticker.upper()
    tf = normalize_timeframe(timeframe)
    _reject_weekly(tf)
    intraday = is_intraday(tf)
    # Map lookback to a bar count for the synthetic fallback. Daily ~1 bar/day; intraday packs many
    # bars per day, but cap so we never build an unreasonable frame.
    per_day = {"1D": 1, "1H": 7, "15m": 26, "5m": 78}[tf]
    n = max(260, min(int(lookbackDays) * per_day, 4000))
    window = _window(ticker, tf, as_of, limit=n, n=n, lookback_days=int(lookbackDays))
    source = window.source
    enriched = _enriched(window, tf)
    if completedOnly:
        completed_at = window.as_of_dt if window.historical else datetime.now(timezone.utc)
        enriched = _completed_feature_rows(enriched, tf, completed_at)
    cols = ["open", "high", "low", "close", "volume", *FEATURE_COLUMNS]
    rows = []
    for idx, r in enriched.iterrows():
        row = {"time": _fmt_time(idx, intraday)}
        for c in cols:
            row[c] = _clean(round(float(r[c]), 6)) if pd.notna(r[c]) else None
        rows.append(row)
    return {
        "ticker": ticker,
        "timeframe": tf,
        "source": source,
        "sourceIsSynthetic": window.source_is_synthetic,
        **({"completedOnly": True, "dataThrough": rows[-1]["time"] if rows else None}
           if completedOnly else {}),
        "featureColumns": FEATURE_COLUMNS,
        "rows": rows,
    }


@app.get("/analysis/{ticker}/confluence")
def confluence(ticker: str, timeframes: str = CONFLUENCE_DEFAULT, as_of: str | None = None):
    """Multi-timeframe confluence: run the SAME pipeline (fetch -> indicators -> regime) on each
    timeframe, then summarize agreement/conflict deterministically. Failed timeframes are skipped
    so the confluence still computes over whatever resolved."""
    ticker = ticker.upper()
    tfs: list[str] = []
    for raw in timeframes.split(","):
        tf = normalize_timeframe(raw.strip())
        _reject_weekly(tf)
        if raw.strip() and tf not in tfs:
            tfs.append(tf)
    if not tfs:
        tfs = [normalize_timeframe(t) for t in CONFLUENCE_DEFAULT.split(",")]

    regimes: dict[str, dict] = {}
    last_window: BarWindow | None = None
    for tf in tfs:
        try:
            window = _window(ticker, tf, as_of, limit=TECHNICAL_WINDOW + INDICATOR_WARMUP)
            last_window = window
            if window.empty:
                continue  # skip this timeframe; confluence covers the rest
            enriched = add_indicators(window.df, intraday=is_intraday(tf))
            regimes[tf] = detect_regime(enriched, timeframe=tf)
        except DataError:
            continue

    # Skipping SOME timeframes is this endpoint's normal behaviour. Serving NONE of them at a past
    # cutoff is the store saying it has no record of that moment — §9.38.1's 409, not an empty
    # confluence that reads like a genuine "no timeframe shows a clear trend".
    if last_window is not None and last_window.historical and not regimes:
        raise InsufficientCoverage(last_window)

    return {
        "ticker": ticker,
        "timeframes": list(regimes.keys()),
        "regimes": regimes,
        "confluence": build_confluence(regimes),
    }


# --------------------------------------------------------------------------------------------
# Point-in-time endpoints (contract §4.4). All three accept `as_of`, all three echo the RESOLVED
# cutoff they actually used, and none of them can reach a provider on a historical request —
# `load_bars` is the only door to bars and it enforces that.
# --------------------------------------------------------------------------------------------


@app.get("/candles/{ticker}")
def candles(ticker: str, timeframe: str = "1D", limit: int = 60, as_of: str | None = None):
    """Bars with per-bar §4.2 geometry.

    The window read is deliberately wider than `limit`: `relativeVolume` needs the 19 bars before
    the first returned bar, so requests for 20 and 200 bars report identical geometry for the bars
    they share. Geometry that shifts with `limit` would not be reproducible.
    """
    ticker = ticker.upper()
    tf = normalize_timeframe(timeframe)
    limit = max(1, int(limit))
    window = _window(ticker, tf, as_of, limit=extended_limit(limit))
    payload = {
        "ticker": ticker,
        "timeframe": tf,
        "asOf": window.as_of,
        "source": window.source,
        "sourceIsSynthetic": window.source_is_synthetic,
        "bars": [],
        **_coverage_block(window),
    }
    if window.empty:
        return payload
    geo = add_geometry(window.df)
    payload["bars"] = candle_payload(geo, lambda i: _fmt_time(i, is_intraday(tf)), limit=limit)
    return payload


@app.get("/technical-context/{ticker}")
def technical_context(ticker: str, timeframe: str = "1D", as_of: str | None = None):
    """Price + indicators + the §4.3 market-structure block, which always carries `raw`."""
    ticker = ticker.upper()
    tf = normalize_timeframe(timeframe)
    intraday = is_intraday(tf)
    window = _window(ticker, tf, as_of, limit=TECHNICAL_WINDOW + INDICATOR_WARMUP)
    if window.empty:
        # Nothing to build a structure from. `structure: null` is an honest absence; a block of
        # nulls would still carry claims ("breakout": false) that no bar supports. `coverage` says
        # why. This is a read, so it does NOT fall back to a fetch.
        return {
            "ticker": ticker,
            "timeframe": tf,
            "asOf": window.as_of,
            "source": window.source,
            "sourceIsSynthetic": window.source_is_synthetic,
            "price": None,
            "indicators": None,
            "structure": None,
            **_coverage_block(window),
        }
    enriched = add_indicators(window.df, intraday=intraday)
    last = enriched.iloc[-1]
    return {
        "ticker": ticker,
        "timeframe": tf,
        "asOf": window.as_of,
        "source": window.source,
        "sourceIsSynthetic": window.source_is_synthetic,
        "price": {
            "lastClose": round(float(last.close), 2),
            "asOf": _fmt_time(enriched.index[-1], intraday),
        },
        "indicators": {
            "rsi": _clean(round(float(last.rsi), 2)) if pd.notna(last.rsi) else None,
            "macd": _clean(round(float(last.macd), 4)) if pd.notna(last.macd) else None,
            "macdSignal": (
                _clean(round(float(last.macd_signal), 4)) if pd.notna(last.macd_signal) else None
            ),
            "macdHist": _clean(round(float(last.macd_hist), 4)) if pd.notna(last.macd_hist) else None,
            "sma50": _clean(round(float(last.sma50), 2)) if pd.notna(last.sma50) else None,
            "sma200": _clean(round(float(last.sma200), 2)) if pd.notna(last.sma200) else None,
            "ema20": _clean(round(float(last.ema20), 2)) if pd.notna(last.ema20) else None,
            "atr": _clean(round(float(last.atr), 2)) if pd.notna(last.atr) else None,
            "adx": _clean(round(float(last.adx), 2)) if pd.notna(last.adx) else None,
            "vwap": _clean(round(float(last.vwap), 2)) if pd.notna(last.vwap) else None,
        },
        "structure": build_structure(enriched, lambda i: _fmt_time(i, intraday)),
        **_coverage_block(window),
    }


@app.get("/multi-timeframe-context/{ticker}")
def multi_timeframe_context(ticker: str, horizon: str | None = None, as_of: str | None = None):
    """The same structure block on several timeframes, plus a deterministic synthesis.

    `synthesis` does NOT call `build_confluence`: that function is reached from
    `/analysis/{ticker}/confluence` via a loop that re-fetches per timeframe, and this endpoint must
    be answerable at a past cutoff with no provider reachable. `build_confluence_from_regimes` is
    the pure `{timeframe: regime} -> synthesis` equivalent.
    """
    ticker = ticker.upper()
    if horizon is not None and str(horizon).strip():
        key = str(horizon).strip().lower()
        if key not in HORIZON_TIMEFRAMES:
            raise HTTPException(
                status_code=400,
                detail=f"unknown horizon {horizon!r}; expected one of {sorted(HORIZON_TIMEFRAMES)}",
            )
        wanted = HORIZON_TIMEFRAMES[key]
    else:
        wanted = HORIZON_DEFAULT

    blocks: dict[str, dict] = {}
    regimes: dict[str, dict] = {}
    coverage: dict[str, str] = {}
    resolved: str | None = None

    for tf in wanted:
        intraday = is_intraday(tf)
        window = _window(ticker, tf, as_of, limit=TECHNICAL_WINDOW + INDICATOR_WARMUP)
        resolved = window.as_of
        coverage[tf] = window.coverage
        if window.empty:
            continue  # skipped, with its coverage recorded — never a 500
        enriched = add_indicators(window.df, intraday=intraday)
        regimes[tf] = detect_regime(enriched, timeframe=tf)
        last = enriched.iloc[-1]
        blocks[tf] = {
            "timeframe": tf,
            "source": window.source,
            "sourceIsSynthetic": window.source_is_synthetic,
            "price": {
                "lastClose": round(float(last.close), 2),
                "asOf": _fmt_time(enriched.index[-1], intraday),
            },
            "regime": regimes[tf],
            "structure": build_structure(enriched, lambda i, x=intraday: _fmt_time(i, x)),
            **_coverage_block(window),
        }

    if resolved is None:  # `wanted` is never empty, but resolve the cutoff rather than echo null
        try:
            resolved, _historical = resolve_as_of(as_of)
        except AsOfError as e:
            raise HTTPException(status_code=400, detail=str(e)) from e

    return {
        "ticker": ticker,
        "asOf": resolved,
        "timeframes": blocks,
        "synthesis": build_confluence_from_regimes(regimes),
        # Per-timeframe coverage, including the timeframes skipped above — otherwise a skip is
        # indistinguishable from a timeframe that was never asked for.
        "coverage": coverage,
    }
