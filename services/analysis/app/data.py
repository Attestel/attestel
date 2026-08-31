"""Price data layer.

Source order (no-KYC first — Alpaca requires broker KYC and is now optional):
  1. Alpaca Market Data (free "Basic"/IEX) — intraday + daily bars — ONLY if APCA_* keys set.
  2. Twelve Data — official, email-only signup, free 800 req/day — ONLY if TWELVEDATA_API_KEY set.
  3. yfinance (Yahoo) — NO account/key; the real-data default. Unofficial scraper (fragile but fine
     for a personal single-user tool). Serves daily + intraday.
  4. Tiingo (official EOD) — daily only, if TIINGO_API_KEY set.
  5. synthetic seed series so the whole stack renders offline with NO keys and NO network.

Every path returns the SAME DataFrame shape: index=date/time, columns=[open,high,low,close,volume],
and a source label ("alpaca:iex" | "twelvedata" | "yfinance" | "tiingo" | "synthetic"). Downstream
(indicators, regime) is source-agnostic — the original doc's decoupling principle.
"""
from __future__ import annotations

import os
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone

import numpy as np
import pandas as pd
import requests

from . import store

COLS = ["open", "high", "low", "close", "volume"]

# The weekly timeframe is resampled from stored dailies and is deliberately NOT a member of
# TIMEFRAMES: that dict is indexed by every provider function, so a weekly key would send "1W" to a
# provider that has no such interval.
WEEKLY = store.WEEKLY_TIMEFRAME

# `_validate` already refuses a live frame with fewer than 30 rows. The store read reuses the same
# number as its coverage threshold so "enough bars" means one thing in this service.
MIN_BARS = 30

# contract §9.10 — both spellings fixed here so analysis and events need not agree in prose.
COVERAGE_OK = "ok"
COVERAGE_INSUFFICIENT = "insufficient"

# Weekly buckets are built from dailies, so a request for N weeks has to read ~5N dailies. The slack
# covers holidays and a partial leading week.
_DAILY_BARS_PER_WEEK = 7
_WEEKLY_READ_SLACK = 40

# Supported timeframes -> (Alpaca interval, pandas synthetic freq, lookback days for a live fetch).
# "1D" is the default and preserves the original daily-primary behavior.
TIMEFRAMES: dict[str, tuple[str, str, int]] = {
    "1D": ("1Day", "B", 500),
    "1H": ("1Hour", "1h", 60),
    "15m": ("15Min", "15min", 15),
    "5m": ("5Min", "5min", 7),
}

# Twelve Data intervals (https://api.twelvedata.com/time_series?interval=...).
TWELVEDATA_INTERVALS: dict[str, str] = {
    "1D": "1day",
    "1H": "1h",
    "15m": "15min",
    "5m": "5min",
}

# yfinance (interval, period, fallback_period). Yahoo caps history per interval: 60m ~730d, and
# sub-hour intervals ~60d. `fallback_period` is retried if the primary period errors/returns empty
# (e.g. 60m sometimes rejects 730d) — None means no retry, just fall through to the next provider.
YFINANCE_TIMEFRAMES: dict[str, tuple[str, str, str | None]] = {
    "1D": ("1d", "2y", None),
    "1H": ("60m", "730d", "60d"),
    "15m": ("15m", "60d", None),
    "5m": ("5m", "60d", None),
}


class DataError(Exception):
    """Raised loudly on empty/malformed frames (original GAP-1 mitigation)."""


def normalize_timeframe(tf: str | None) -> str:
    """Map a requested timeframe to a supported key; default 1D. Accepts a few aliases.

    "1W" is recognised here but is NOT a member of TIMEFRAMES — see the WEEKLY comment above."""
    if not tf:
        return "1D"
    t = tf.strip()
    aliases = {"1d": "1D", "day": "1D", "daily": "1D", "1h": "1H", "hour": "1H",
               "15m": "15m", "15min": "15m", "5m": "5m", "5min": "5m",
               "1w": WEEKLY, "week": WEEKLY, "weekly": WEEKLY}
    if t in TIMEFRAMES or t == WEEKLY:
        return t
    return aliases.get(t.lower(), "1D")


def is_weekly(tf: str | None) -> bool:
    return normalize_timeframe(tf) == WEEKLY


def is_intraday(tf: str) -> bool:
    """Weekly is a HIGHER timeframe than daily, not an intraday one — it must not be swept into the
    intraday branch of formatting, VWAP or retention."""
    return normalize_timeframe(tf) not in ("1D", WEEKLY)


def _validate(df: pd.DataFrame, ticker: str) -> pd.DataFrame:
    if df is None or df.empty:
        raise DataError(f"empty frame for {ticker}")
    missing = [c for c in COLS if c not in df.columns]
    if missing:
        raise DataError(f"missing columns {missing} for {ticker}")
    df = df.dropna(subset=["close"])
    if len(df) < 30:
        raise DataError(f"only {len(df)} rows for {ticker}; need >=30")
    return df[COLS]


# ---------------- Alpaca (primary; intraday + daily) ----------------

ALPACA_DATA_URL = os.getenv("ALPACA_DATA_URL", "https://data.alpaca.markets/v2")


def _alpaca_headers() -> dict[str, str] | None:
    key, secret = os.getenv("APCA_API_KEY_ID"), os.getenv("APCA_API_SECRET_KEY")
    if not key or not secret:
        return None
    return {"APCA-API-KEY-ID": key, "APCA-API-SECRET-KEY": secret}


def _from_alpaca(ticker: str, timeframe: str, lookback_days: int | None = None) -> pd.DataFrame:
    headers = _alpaca_headers()
    if headers is None:
        raise DataError("no APCA_API_KEY_ID/APCA_API_SECRET_KEY")
    interval, _freq, lookback = TIMEFRAMES[timeframe]
    if lookback_days:  # caller wants a longer window (training); never shrink the default
        lookback = max(lookback, int(lookback_days))
    start = (datetime.now(timezone.utc) - timedelta(days=lookback)).strftime("%Y-%m-%dT%H:%M:%SZ")
    url = f"{ALPACA_DATA_URL}/stocks/{ticker}/bars"
    params = {
        "timeframe": interval,
        "start": start,
        "limit": 10000,
        "feed": "iex",           # free "Basic" plan = IEX (real-time for liquid mega-caps)
        "adjustment": "all",     # split/dividend adjusted
        "sort": "asc",
    }
    r = requests.get(url, params=params, headers=headers, timeout=15)
    r.raise_for_status()
    bars = r.json().get("bars") or []
    if not bars:
        raise DataError("alpaca returned no bars")
    df = pd.DataFrame(bars)
    # Alpaca fields: t (RFC3339), o,h,l,c,v. Index by timestamp (tz-naive UTC).
    idx = pd.to_datetime(df["t"], utc=True).dt.tz_localize(None)
    if timeframe == "1D":
        idx = idx.dt.normalize()  # daily bars -> pure date, matches Tiingo/synthetic
    df = df.rename(columns={"o": "open", "h": "high", "l": "low", "c": "close", "v": "volume"})
    df.index = idx
    return df[["open", "high", "low", "close", "volume"]]


def _alpaca_snapshot(ticker: str) -> dict:
    """One-call latest trade + previous close for a cheap quote. Raises on no keys / error."""
    headers = _alpaca_headers()
    if headers is None:
        raise DataError("no Alpaca keys")
    url = f"{ALPACA_DATA_URL}/stocks/{ticker}/snapshot"
    r = requests.get(url, params={"feed": "iex"}, headers=headers, timeout=10)
    r.raise_for_status()
    snap = r.json() or {}
    trade = snap.get("latestTrade") or {}
    price = trade.get("p")
    if price is None:
        raise DataError("alpaca snapshot: no latest trade")
    prev = (snap.get("prevDailyBar") or {}).get("c")
    change_pct = round((price / prev - 1) * 100, 2) if prev else None
    return {
        "price": round(float(price), 2),
        "changePct": change_pct,
        "asOf": trade.get("t"),
        "source": "alpaca:iex",
    }


# ---------------- Tiingo (daily-only fallback) ----------------

def _from_tiingo(ticker: str, start: str) -> pd.DataFrame:
    key = os.getenv("TIINGO_API_KEY")
    if not key:
        raise DataError("no TIINGO_API_KEY")
    url = f"https://api.tiingo.com/tiingo/daily/{ticker}/prices"
    r = requests.get(
        url,
        params={"startDate": start, "token": key},
        headers={"Content-Type": "application/json"},
        timeout=15,
    )
    r.raise_for_status()
    rows = r.json()
    if not rows:
        raise DataError("tiingo returned no rows")
    df = pd.DataFrame(rows)
    df["date"] = pd.to_datetime(df["date"]).dt.tz_localize(None)
    df = df.set_index("date").rename(columns={"adjVolume": "volume"})
    return df[["open", "high", "low", "close", "volume"]]


# ---------------- Twelve Data (official, email-only key; intraday + daily) ----------------

def _from_twelvedata(ticker: str, timeframe: str) -> pd.DataFrame:
    key = os.getenv("TWELVEDATA_API_KEY")
    if not key:
        raise DataError("no TWELVEDATA_API_KEY")
    interval = TWELVEDATA_INTERVALS[timeframe]
    r = requests.get(
        "https://api.twelvedata.com/time_series",
        params={
            "symbol": ticker,
            "interval": interval,
            "outputsize": 5000,
            "apikey": key,
            "order": "ASC",
        },
        timeout=15,
    )
    r.raise_for_status()
    payload = r.json()
    # Error shape: {"status":"error","code":...,"message":...}. Success carries "values".
    if not isinstance(payload, dict) or payload.get("status") == "error" or "values" not in payload:
        msg = payload.get("message") if isinstance(payload, dict) else "unexpected response"
        raise DataError(f"twelvedata: {msg}")
    values = payload.get("values") or []
    if not values:
        raise DataError("twelvedata returned no values")
    df = pd.DataFrame(values)
    idx = pd.DatetimeIndex(pd.to_datetime(df["datetime"]))
    if idx.tz is not None:
        idx = idx.tz_localize(None)
    if timeframe == "1D":
        idx = idx.normalize()  # daily -> pure date, matches other daily sources
    df.index = idx
    for c in COLS:
        df[c] = pd.to_numeric(df.get(c), errors="coerce")
    df["volume"] = df["volume"].fillna(0)
    return df[COLS].sort_index()


# ---------------- yfinance (Yahoo; no key — the real-data default) ----------------

def _yf_period_for(lookback_days: int) -> str:
    """Smallest yfinance period string covering lookback_days (daily bars only)."""
    for days, period in ((730, "2y"), (1825, "5y"), (3650, "10y")):
        if lookback_days <= days:
            return period
    return "max"


def _from_yfinance(ticker: str, timeframe: str, lookback_days: int | None = None) -> pd.DataFrame:
    # Lazy import so a missing optional dep degrades to the next provider instead of breaking the
    # whole module at import time.
    try:
        import yfinance as yf
    except Exception as e:  # noqa: BLE001
        raise DataError(f"yfinance not installed: {e}") from e

    interval, period, fallback = YFINANCE_TIMEFRAMES[timeframe]
    # Daily history is uncapped at Yahoo, so honor a longer requested window (training needs it).
    # Intraday stays on the table values — Yahoo hard-caps those per interval regardless.
    if lookback_days and timeframe == "1D":
        period = _yf_period_for(int(lookback_days))

    def _download(p: str) -> pd.DataFrame:
        return yf.Ticker(ticker).history(period=p, interval=interval, auto_adjust=True)

    try:
        raw = _download(period)
        if raw is None or raw.empty:
            raise DataError(f"yfinance empty for {ticker} ({interval}/{period})")
    except Exception:  # noqa: BLE001 — Yahoo can reject a too-long period for the interval
        if not fallback:
            raise
        raw = _download(fallback)
        if raw is None or raw.empty:
            raise DataError(f"yfinance empty for {ticker} ({interval}/{fallback})")

    df = raw.rename(
        columns={"Open": "open", "High": "high", "Low": "low", "Close": "close", "Volume": "volume"}
    )
    idx = pd.DatetimeIndex(df.index)
    if idx.tz is not None:
        idx = idx.tz_localize(None)  # tz-naive, matching the other sources
    if timeframe == "1D":
        idx = idx.normalize()
    df.index = idx
    return df[COLS]


# ---------------- synthetic (offline; any timeframe) ----------------

def _synthetic(ticker: str, timeframe: str, n: int = 260) -> pd.DataFrame:
    """Deterministic pseudo-random walk so the stack renders with no keys/network.
    NOT market data — clearly labelled as synthetic upstream so nobody trades on it.
    Bar spacing follows the requested timeframe so intraday charts render sensibly."""
    tf = normalize_timeframe(timeframe)
    _interval, freq, _lookback = TIMEFRAMES[tf]
    rng = np.random.default_rng(abs(hash((ticker, timeframe))) % (2**32))
    end = pd.Timestamp(datetime.now(timezone.utc).replace(tzinfo=None)).floor("min")
    idx = pd.date_range(end=end, periods=n, freq=freq)
    # smaller per-bar drift/vol on shorter timeframes
    scale = {"1D": 1.0, "1H": 0.35, "15m": 0.18, "5m": 0.10}[tf]
    # Daily keeps its original upward drift (behavior unchanged). Intraday timeframes may drift the
    # OTHER way so multi-timeframe confluence has something to disagree about offline — the whole
    # point of the confluence view. Deterministic per (ticker, timeframe); still clearly synthetic.
    drift = 0.0007
    if tf != "1D":
        drift = 0.0007 * (1 if rng.random() < 0.5 else -1)
    steps = rng.normal(drift * scale, 0.025 * scale, n).cumsum()
    close = 120 * np.exp(steps)
    open_ = close * (1 + rng.normal(0, 0.006 * scale, n))
    high = np.maximum(open_, close) * (1 + np.abs(rng.normal(0, 0.008 * scale, n)))
    low = np.minimum(open_, close) * (1 - np.abs(rng.normal(0, 0.008 * scale, n)))
    vol = rng.integers(180_000_000, 520_000_000, n)
    return pd.DataFrame(
        {"open": open_, "high": high, "low": low, "close": close, "volume": vol}, index=idx
    )


def fetch_ohlcv(
    ticker: str, timeframe: str = "1D", n: int = 260, lookback_days: int | None = None
) -> tuple[pd.DataFrame, str]:
    """Returns (df, source). source in {alpaca:iex, twelvedata, yfinance, tiingo, synthetic}.

    No-KYC first: Alpaca (broker KYC) and Twelve Data run only when their key is set; yfinance needs
    no key and is the real-data default. Tiingo is daily-only, so it is only tried for 1D. Synthetic
    is the final, always-available fallback (offline) and is timeframe-aware. Each provider that
    raises falls through to the next.

    `n` sizes the SYNTHETIC fallback. `lookback_days` extends the LIVE providers' daily window past
    their dashboard defaults — the /features endpoint passes it so a walk-forward backtest trains on
    the full requested history instead of ~2 years (the trainRows=295 bug). It never shrinks a
    window, and intraday stays on the provider caps. Existing callers are unaffected."""
    tf = normalize_timeframe(timeframe)
    if tf == WEEKLY:
        # Defensive: every provider function indexes TIMEFRAMES, and the synthetic fallback does
        # too, so a weekly key would KeyError halfway down the cascade. Weekly is built from stored
        # dailies in load_bars and never reaches a provider.
        raise DataError("1W is resampled from stored 1D bars and is never fetched")
    start_days = max(420, int(lookback_days or 0))
    start = (datetime.utcnow() - timedelta(days=start_days)).strftime("%Y-%m-%d")

    sources: list[tuple[str, callable]] = [
        ("alpaca:iex", lambda: _from_alpaca(ticker, tf, lookback_days)),   # only if APCA_* keys set
        ("twelvedata", lambda: _from_twelvedata(ticker, tf)),  # only if TWELVEDATA_API_KEY set
        ("yfinance", lambda: _from_yfinance(ticker, tf, lookback_days)),   # no key — real-data default
    ]
    if tf == "1D":
        sources.append(("tiingo", lambda: _from_tiingo(ticker, start)))  # only if TIINGO_API_KEY set

    for name, fn in sources:
        try:
            return _validate(fn(), ticker), name
        except Exception:  # noqa: BLE001 — fall through to next source
            continue
    return _validate(_synthetic(ticker, tf, n=max(n, 30)), ticker), "synthetic"


def latest_quote(ticker: str) -> dict:
    """Cheap current price + change. Alpaca snapshot when keyed; else the last close from
    fetch_ohlcv — which now resolves through Twelve Data / yfinance, so this yields a REAL quote
    with no keys (source reported accordingly), and still falls back to synthetic bars offline."""
    try:
        q = _alpaca_snapshot(ticker)
        q["symbol"] = ticker
        return q
    except Exception:  # noqa: BLE001 — fall back to bar-derived last close
        pass
    df, source = fetch_ohlcv(ticker, "1D")
    last = float(df["close"].iloc[-1])
    change_pct = None
    if len(df) >= 2:
        prev = float(df["close"].iloc[-2])
        if prev:
            change_pct = round((last / prev - 1) * 100, 2)
    return {
        "symbol": ticker,
        "price": round(last, 2),
        "changePct": change_pct,
        "asOf": df.index[-1].strftime("%Y-%m-%dT%H:%M:%SZ"),
        "source": source,
    }


# --------------------------------------------------------------------------------------------
# The as_of choke point (contract §1, §4.1)
#
# `load_bars` is the ONE function every endpoint reads bars through. Everything above it stays a
# provider concern; everything below it is point-in-time.
#
# The rule it exists to enforce: when `as_of` is supplied and strictly in the past, no provider is
# called — not on a cache miss, not on a coverage gap, not behind a flag. A historical request that
# finds an empty store returns an empty window with `coverage: "insufficient"`, and the caller
# decides. There is no fallback fetch.
# --------------------------------------------------------------------------------------------


class AsOfError(ValueError):
    """A malformed `as_of`. Surfaced as a 400 — never silently treated as "now"."""


@dataclass(frozen=True)
class BarWindow:
    """A resolved window of bars plus the provenance every response has to state."""

    df: pd.DataFrame            # OHLCV columns, ascending DatetimeIndex (tz-naive UTC)
    timeframe: str
    source: str
    source_is_synthetic: bool
    coverage: str
    coverage_from: str | None
    coverage_to: str | None
    as_of: str                  # the RESOLVED cutoff actually used, TS_FMT
    historical: bool

    @property
    def empty(self) -> bool:
        return self.df is None or self.df.empty

    @property
    def as_of_dt(self) -> datetime:
        """The resolved cutoff as a tz-aware UTC datetime, for callers that need to evaluate
        wall-clock-dependent state (session progress) AT the cutoff rather than now."""
        return datetime.strptime(self.as_of, store.TS_FMT).replace(tzinfo=timezone.utc)


def parse_as_of(raw: str | None) -> datetime | None:
    """RFC3339 UTC -> tz-naive UTC datetime. None/blank -> None. Anything else raises."""
    if raw is None:
        return None
    text = str(raw).strip()
    if not text:
        return None
    candidate = text[:-1] + "+00:00" if text.endswith(("Z", "z")) else text
    try:
        parsed = datetime.fromisoformat(candidate)
    except ValueError as e:
        raise AsOfError(f"as_of must be RFC3339 UTC, got {raw!r}") from e
    if parsed.tzinfo is not None:
        parsed = parsed.astimezone(timezone.utc).replace(tzinfo=None)
    return parsed


def resolve_as_of(raw: str | None) -> tuple[str, bool]:
    """(resolved cutoff in TS_FMT, historical?). Historical means supplied AND strictly before now."""
    now = datetime.now(timezone.utc).replace(tzinfo=None)
    parsed = parse_as_of(raw)
    if parsed is None:
        return now.strftime(store.TS_FMT), False
    return parsed.strftime(store.TS_FMT), parsed < now


def _label_sources(sources) -> tuple[str, bool]:
    """Collapse the per-bar source column into one label. A window that mixes providers is labelled
    with all of them, sorted, so the label is deterministic — and any synthetic bar anywhere in the
    window makes the whole window synthetic. The honesty guardrail may be prettier, never quieter."""
    distinct = sorted({str(s) for s in sources if s})
    if not distinct:
        return "unknown", False
    return ("+".join(distinct), "synthetic" in distinct)


def resample_weekly(daily: pd.DataFrame) -> pd.DataFrame:
    """Stored dailies -> weekly bars. Buckets anchored W-FRI; each bucket's ts is the LAST daily
    bar inside it, never a future Friday. The final bucket may be partial — that is correct, and
    padding it would invent a bar. Weekly bars are returned, never written."""
    if daily is None or daily.empty:
        return daily
    rows, index = [], []
    for _bucket, chunk in daily.groupby(pd.Grouper(freq="W-FRI")):
        if chunk.empty:
            continue
        rows.append(
            {
                "open": float(chunk["open"].iloc[0]),
                "high": float(chunk["high"].max()),
                "low": float(chunk["low"].min()),
                "close": float(chunk["close"].iloc[-1]),
                "volume": float(chunk["volume"].sum()),
            }
        )
        index.append(chunk.index[-1])
    if not rows:
        return daily.iloc[0:0][COLS]
    return pd.DataFrame(rows, index=pd.DatetimeIndex(index))


def _refresh_store(ticker: str, timeframe: str, n: int, lookback_days: int | None):
    """Live path: fetch through the existing cascade, persist, and hand the frame back so a store
    write failure can degrade to it. Never raises — a provider failure is already handled by the
    cascade, and a store failure must not fail the request."""
    try:
        df, source = fetch_ohlcv(ticker, timeframe, n=n, lookback_days=lookback_days)
    except DataError:
        return None
    # Synthetic bars are the offline fallback for THIS live request and are never persisted: a
    # fabricated bar in the store becomes a fabricated answer about history at some later cutoff.
    # `store.write_bars` refuses them too; this branch keeps the normal path from relying on an
    # exception to express an intention.
    if store.normalize_source(source) != store.SYNTHETIC_SOURCE:
        try:
            store.write_bars(ticker, timeframe, df, source)
        except Exception:  # noqa: BLE001 — the store is a cache of record, not the critical path
            pass
    return df, source


def load_bars(
    ticker: str,
    timeframe: str = "1D",
    as_of: str | None = None,
    limit: int | None = None,
    n: int = 260,
    lookback_days: int | None = None,
) -> BarWindow:
    """Read bars for (ticker, timeframe) as of a cutoff. THE choke point — see the block comment.

    Live requests fetch, write, then read back *from the store*. Reading back is not redundant: it
    makes the live and historical paths return the same rows through the same code, which is what
    makes a historical replay reproduce a live run.
    """
    ticker = ticker.upper()
    tf = normalize_timeframe(timeframe)
    resolved, historical = resolve_as_of(as_of)

    weekly = tf == WEEKLY
    read_tf = "1D" if weekly else tf
    read_limit = limit
    if weekly and limit is not None:
        read_limit = limit * _DAILY_BARS_PER_WEEK + _WEEKLY_READ_SLACK

    fetched = None
    if not historical:
        fetched = _refresh_store(ticker, read_tf, n=n, lookback_days=lookback_days)

    stored = store.read_bars(ticker, read_tf, resolved, read_limit)
    count, lo, hi = store.read_coverage(ticker, read_tf, resolved)

    if stored.empty and fetched is not None:
        # The store could not be written or read (read-only volume, disk full). Degrade to the
        # frame we already have rather than pretending the provider said nothing.
        df, source = fetched
        stored = df[COLS].copy()
        stored["source"] = store.normalize_source(source)
        count = len(stored)
        lo = store.format_ts(stored.index[0]) if count else None
        hi = store.format_ts(stored.index[-1]) if count else None

    label, synthetic = _label_sources(stored["source"] if "source" in stored else [])
    frame = stored[COLS].copy() if not stored.empty else pd.DataFrame(columns=COLS)

    if weekly and not frame.empty:
        frame = resample_weekly(frame)
        if limit is not None:
            frame = frame.tail(limit)

    return BarWindow(
        df=frame,
        timeframe=tf,
        source=label,
        source_is_synthetic=synthetic,
        coverage=COVERAGE_OK if count >= MIN_BARS else COVERAGE_INSUFFICIENT,
        coverage_from=lo,
        coverage_to=hi,
        as_of=resolved,
        historical=historical,
    )
