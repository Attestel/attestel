"""Phase 4 — what the market did after a stored event, and what that does NOT license us to say.

TWO HALVES, AND THE SECOND ONE IS THE HARD PART
-----------------------------------------------
`capture_once` records reactions. `sensitivity` aggregates them. Recording is arithmetic; the
aggregation is where a system either tells the truth about how little it knows or quietly stops.

So the gate is explicit and it is a refusal, not a caveat: below `MIN_SAMPLE` matured, non-synthetic
observations, `sensitivity` returns `insufficient history` and **no number at all** — no median, no
frequency, no direction. Not a greyed-out number, not a number with a warning: nothing. A
percentage on a screen is acted on whatever is printed beside it.

THE RULES THIS FILE EXISTS TO HOLD
-----------------------------------
* **Stored bars only.** Every price comes from `GET {ANALYSIS_URL}/candles` (§9.25) — the same and
  only seam `predictions.py` uses. No direct read of the analysis schema, no `/quote`, no provider.
* **No future bars before a window matures.** A horizon resolves only when the required number of
  trading bars STRICTLY AFTER the reference bar already exists in the store. Until then the row is
  `pending`, which is a different fact from `unavailable`.
* **Sessions are handled deterministically.** Before the open, inside the session, after the close,
  and on a non-trading day are four different cases with four different reference closes. They are
  computed from the event's own timestamp in US/Eastern, never guessed.
* **Missing benchmark data is NULL, never zero.** An excess return computed against an invented
  zero is a fabricated number.
* **Synthetic bars are recorded and disqualified.** Recorded so the sample gap is explainable;
  disqualified so no empirical claim can rest on them (§9.46, invariant #10).
* **Association, not causation.** Nothing here says an event CAUSED a move, and the aggregate's own
  fields say so.
* **No model.** Qwen may summarise an already-computed result elsewhere; it may not compute one, and
  this module contains no model seam at all.
"""
from __future__ import annotations

import math
import os
from datetime import datetime, timedelta, timezone
from zoneinfo import ZoneInfo

from .db import Connection
from .predictions import (
    BENCHMARK_TICKER,
    SYNTHETIC_SOURCE,
    CandleUnavailable,
    fetch_candles,
)

#: Bumped when the ARITHMETIC changes. A stored reaction always says which rules produced it, so an
#: aggregate can refuse to mix two versions rather than silently averaging them.
CALC_VERSION = "reaction@1"
SENSITIVITY_VERSION = "sensitivity@1"

#: The horizons, in trading bars after the reference bar.
HORIZONS: dict[str, int] = {"1d": 1, "5d": 5, "20d": 20}

STATE_PENDING = "pending"
STATE_RESOLVED = "resolved"
STATE_UNAVAILABLE = "unavailable"

SESSION_BEFORE = "before_market"
SESSION_REGULAR = "regular"
SESSION_AFTER = "after_market"
SESSION_NON_TRADING = "non_trading_day"

REASON_NO_REFERENCE = "no_reference_bar"
REASON_IMMATURE = "window_not_matured"
REASON_SYNTHETIC = "synthetic_bars"
REASON_BARS_UNAVAILABLE = "bars_unavailable"
REASON_ZERO_REFERENCE = "reference_close_is_zero"

#: THE SAMPLE FLOOR. Below this, no statistic is reported — see the module docstring.
#:
#: Twelve is three years of quarterly earnings, or a year of monthly macro prints. It is not a
#: statistical guarantee and is not presented as one; it is the point below which a median is more
#: misleading than useful. It is a named constant so raising it is a one-line, reviewable decision.
MIN_SAMPLE = 12
MIN_SAMPLE_ENV = "REACTION_MIN_SAMPLE"

#: US market session boundaries, US/Eastern. Regular hours; this system has no extended-session bars.
MARKET_OPEN = (9, 30)
MARKET_CLOSE = (16, 0)
EASTERN = "America/New_York"

#: Trading bars of pre-event history used as the volume/range baseline.
BASELINE_BARS = 20

#: How many bars one capture reads per ticker. Enough for the baseline, the reference bar and the
#: longest horizon, with slack for holidays.
READ_BARS = BASELINE_BARS + max(HORIZONS.values()) + 15

#: How many (event, ticker) pairs one bounded capture pass examines.
DEFAULT_CAPTURE_LIMIT = 100
CAPTURE_LIMIT_ENV = "REACTION_CAPTURE_LIMIT"

ENABLED_ENV = "REACTION_CAPTURE_ENABLED"


def _flag(name: str, default: bool = False) -> bool:
    raw = os.getenv(name, "").strip().lower()
    if not raw:
        return default
    return raw in ("1", "true", "yes", "on")


def enabled() -> bool:
    """The lane's own gate. Default false, like every other background job in this repository."""
    return _flag(ENABLED_ENV, False)


def _positive_int(name: str, default: int) -> int:
    try:
        value = int(os.getenv(name, "").strip() or default)
    except ValueError:
        return default
    return value if value > 0 else default


def min_sample() -> int:
    return _positive_int(MIN_SAMPLE_ENV, MIN_SAMPLE)


def capture_limit() -> int:
    return _positive_int(CAPTURE_LIMIT_ENV, DEFAULT_CAPTURE_LIMIT)


# ── deterministic session and window arithmetic ──────────────────────────────────────────────────


def _iso(moment: datetime) -> str:
    return moment.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _parse(value) -> datetime | None:
    if isinstance(value, datetime):
        return value if value.tzinfo else value.replace(tzinfo=timezone.utc)
    text = str(value or "").strip()
    if not text:
        return None
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    try:
        parsed = datetime.fromisoformat(text)
    except ValueError:
        return None
    return parsed if parsed.tzinfo else parsed.replace(tzinfo=timezone.utc)


def classify_session(event_at, *, trading_days=None) -> str:
    """Which part of the trading day the event landed in, in US/Eastern.

    `trading_days` is the set of dates the STORE actually has bars for. Passing it is what makes
    holidays deterministic without a holiday calendar in this repository: a weekday with no stored
    bar is a non-trading day, observed rather than assumed. Absent, only weekends are recognised —
    which is the honest degradation, since we then genuinely do not know.
    """
    moment = _parse(event_at)
    if moment is None:
        return SESSION_NON_TRADING
    local = moment.astimezone(ZoneInfo(EASTERN))
    day = local.date().isoformat()

    if local.weekday() >= 5:
        return SESSION_NON_TRADING
    if trading_days is not None and day not in trading_days:
        return SESSION_NON_TRADING

    minutes = local.hour * 60 + local.minute
    if minutes < MARKET_OPEN[0] * 60 + MARKET_OPEN[1]:
        return SESSION_BEFORE
    if minutes < MARKET_CLOSE[0] * 60 + MARKET_CLOSE[1]:
        return SESSION_REGULAR
    return SESSION_AFTER


def reference_index(bars: list[dict], event_at, session: str) -> int | None:
    """Index of the bar whose close is the LAST price the event could not have moved.

    The four cases, stated rather than inferred:

    * `before_market` — the event lands before the day's session, so the day's own close already
      reflects it. Reference is the last bar STRICTLY BEFORE that day.
    * `regular` — the event lands inside the session, so that day's close reflects it too. Same
      reference.
    * `after_market` — the day's session closed before the event. Reference is that day's close.
    * `non_trading_day` — a weekend or a holiday. Reference is the last bar at or before it.

    `None` when no stored bar precedes the event, which is `no_reference_bar`, not zero.
    """
    moment = _parse(event_at)
    if moment is None or not bars:
        return None
    day = moment.astimezone(ZoneInfo(EASTERN)).date().isoformat()

    strictly_before = session in (SESSION_BEFORE, SESSION_REGULAR)
    chosen = None
    for index, bar in enumerate(bars):
        bar_day = str(bar.get("time") or "")[:10]
        if not bar_day:
            continue
        if strictly_before:
            if bar_day < day:
                chosen = index
        elif bar_day <= day:
            chosen = index
    return chosen


def _close(bar) -> float | None:
    try:
        value = float(bar.get("close"))
    except (TypeError, ValueError):
        return None
    return value if math.isfinite(value) else None


def _mean(values) -> float | None:
    usable = [v for v in values if v is not None and math.isfinite(v)]
    if not usable:
        return None
    return sum(usable) / len(usable)


def _bar_range(bar) -> float | None:
    try:
        high, low = float(bar.get("high")), float(bar.get("low"))
    except (TypeError, ValueError):
        return None
    if not (math.isfinite(high) and math.isfinite(low)):
        return None
    return high - low


def _volume(bar) -> float | None:
    try:
        value = float(bar.get("volume"))
    except (TypeError, ValueError):
        return None
    return value if math.isfinite(value) else None


def _ratio(after: float | None, before: float | None) -> float | None:
    """`after / before - 1`, or None. A zero baseline yields None — never an infinity, and never 0."""
    if after is None or before is None or before == 0:
        return None
    return after / before - 1.0


def compute_windows(
    bars: list[dict], reference: int, *, now: datetime, horizons=None
) -> dict[str, dict]:
    """Every horizon's outcome from ONE stored bar series. Pure, and the look-ahead guard lives here.

    A horizon resolves only when `reference + n` exists in the series AND that bar's own timestamp
    is at or before `now`. The second clause is belt and braces — the store cannot contain a future
    bar — but it is the clause that makes "never use future bars before their window matures"
    something the code states rather than something the storage happens to guarantee.
    """
    wanted = horizons or HORIZONS
    reference_close = _close(bars[reference]) if 0 <= reference < len(bars) else None

    pre = bars[max(0, reference - BASELINE_BARS + 1): reference + 1]
    pre_volume = _mean([_volume(b) for b in pre])
    pre_range = _mean([_bar_range(b) for b in pre])

    out: dict[str, dict] = {}
    for name, n in wanted.items():
        target = reference + n
        if reference_close is None:
            out[name] = {"state": STATE_UNAVAILABLE, "missingReason": REASON_NO_REFERENCE}
            continue
        if reference_close == 0:
            out[name] = {"state": STATE_UNAVAILABLE, "missingReason": REASON_ZERO_REFERENCE}
            continue
        if target >= len(bars):
            # The window has not matured, or the store has not caught up. Either way it is PENDING;
            # `unavailable` is reserved for an attempted read with no usable data. Both are retried.
            out[name] = {"state": STATE_PENDING, "missingReason": REASON_IMMATURE}
            continue

        end = bars[target]
        end_ts = str(end.get("time") or "")
        end_moment = _parse(end_ts if len(end_ts) > 10 else end_ts + "T00:00:00Z")
        if end_moment is not None and end_moment > now:
            out[name] = {"state": STATE_PENDING, "missingReason": REASON_IMMATURE}
            continue

        end_close = _close(end)
        if end_close is None:
            out[name] = {"state": STATE_UNAVAILABLE, "missingReason": REASON_BARS_UNAVAILABLE}
            continue

        path = bars[reference + 1: target + 1]
        out[name] = {
            "state": STATE_RESOLVED,
            "missingReason": None,
            "endTs": end_ts,
            "endClose": end_close,
            "rawReturn": end_close / reference_close - 1.0,
            "volumeChange": _ratio(_mean([_volume(b) for b in path]), pre_volume),
            "rangeChange": _ratio(_mean([_bar_range(b) for b in path]), pre_range),
            "barsUsed": len(path),
        }
    return {
        "windows": out,
        "referenceTs": str(bars[reference].get("time") or "") if 0 <= reference < len(bars) else None,
        "referenceClose": reference_close,
        "preVolumeAvg": pre_volume,
        "preRangeAvg": pre_range,
    }


# ── capture ──────────────────────────────────────────────────────────────────────────────────────


def _is_synthetic(payload: dict) -> bool:
    # §9.46 / predictions.py: `/candles` joins EVERY distinct source with '+', so the value can be
    # 'yfinance+synthetic'. A substring test is the correct check; equality is a hole.
    return SYNTHETIC_SOURCE in str(payload.get("source") or "").lower() or bool(
        payload.get("sourceIsSynthetic")
    )


def _read_series(ticker: str, as_of: str) -> tuple[list[dict], str, bool]:
    payload = fetch_candles(ticker, as_of=as_of, limit=READ_BARS)
    return (
        list(payload.get("bars") or []),
        str(payload.get("source") or ""),
        _is_synthetic(payload),
    )


def _unresolved(
    conn: Connection, *, limit: int, now: datetime, tickers=None
) -> list[dict]:
    """Pairs with a `pending` or last-attempt-`unavailable` horizon.

    `tickers` narrows the pass. It matters operationally: an event's relationship set legitimately
    reaches companies this deployment does not price (a supplier we do not cover). Retrying is
    required because stored bars can be backfilled and outages recover; narrowing to the configured
    universe bounds that work and is the operator's call, so it is a parameter rather than a rule.
    """
    # `unavailable` describes the last capture attempt, not a permanent fact. Stored bars may be
    # backfilled and a temporarily unreachable analysis service may recover, so retry those rows.
    # Resolved rows remain immutable through `_upsert_window` below.
    where = [
        "s.scheduled_at <= :now",
        "(w.state IS NULL OR w.state = :pending OR w.state = :unavailable)",
    ]
    params = {
        "now": _iso(now), "pending": STATE_PENDING, "unavailable": STATE_UNAVAILABLE,
        "limit": max(1, int(limit)),
    }
    if tickers:
        where.append("r.ticker = ANY(:tickers)")
        params["tickers"] = [str(t).strip().upper() for t in tickers if str(t).strip()]
    return conn.execute(
        "SELECT DISTINCT s.id AS event_id, r.ticker, s.scheduled_at "
        "FROM scheduled_events s JOIN event_relationships r ON r.event_id = s.id "
        "LEFT JOIN event_reaction_windows w ON w.event_id = s.id AND w.ticker = r.ticker "
        f"WHERE {' AND '.join(where)} "
        "ORDER BY s.scheduled_at ASC, r.ticker LIMIT :limit",
        params,
    ).fetchall()


def capture_once(
    conn: Connection, *, now: datetime | None = None, limit: int | None = None, tickers=None,
) -> dict:
    """ONE bounded capture pass over events whose windows are not yet resolved.

    Idempotent and restart-safe: every write is an upsert keyed on (event, ticker, horizon), and a
    horizon that is already `resolved` is never recomputed — so running this twice produces the same
    rows and running it after a crash resumes rather than duplicating.

    Returns `{examined, resolved, pending, unavailable, outstanding, degraded}`.
    """
    moment = (now or datetime.now(timezone.utc)).astimezone(timezone.utc)
    stamp = _iso(moment)
    report = {
        "examined": 0, "resolved": 0, "pending": 0, "unavailable": 0,
        "outstanding": 0, "degraded": [],
    }
    if not enabled():
        report["degraded"].append("reactions:disabled")
        return report

    pairs = _unresolved(
        conn, limit=limit or capture_limit(), now=moment, tickers=tickers)
    report["examined"] = len(pairs)
    if not pairs:
        return report

    # One benchmark read per distinct as-of date, cached for the pass. The benchmark is a market
    # fact, not a per-event one.
    benchmark_cache: dict[str, tuple[list[dict], str, bool] | None] = {}
    trading_days_cache: dict[str, set] = {}

    for pair in pairs:
        ticker = str(pair["ticker"]).strip().upper()
        event_at = str(pair["scheduled_at"] or "")
        # Read forward far enough for the longest horizon to be present when it has matured. The
        # store cannot contain a bar past `now`, so asking for a future as-of returns what exists —
        # which is exactly the maturity test.
        read_as_of = _iso(min(
            moment,
            (_parse(event_at) or moment) + timedelta(days=max(HORIZONS.values()) * 2 + 20),
        ))

        try:
            bars, source, synthetic = _read_series(ticker, read_as_of)
        except CandleUnavailable as exc:
            report["degraded"].append(f"reactions:{ticker}:bars")
            _write_unavailable(conn, pair, ticker, REASON_BARS_UNAVAILABLE, stamp, detail=str(exc))
            report["unavailable"] += len(HORIZONS)
            continue

        trading_days = trading_days_cache.setdefault(
            ticker, {str(b.get("time") or "")[:10] for b in bars}
        )
        session = classify_session(event_at, trading_days=trading_days)
        reference = reference_index(bars, event_at, session)
        if reference is None:
            _write_unavailable(conn, pair, ticker, REASON_NO_REFERENCE, stamp, session=session)
            report["unavailable"] += len(HORIZONS)
            continue

        computed = compute_windows(bars, reference, now=moment)

        benchmark_windows: dict[str, dict] = {}
        benchmark_synthetic = False
        if ticker != BENCHMARK_TICKER:
            cached = benchmark_cache.get(read_as_of, ...)
            if cached is ...:
                try:
                    cached = _read_series(BENCHMARK_TICKER, read_as_of)
                except CandleUnavailable:
                    cached = None
                benchmark_cache[read_as_of] = cached
            if cached is not None:
                b_bars, _b_source, benchmark_synthetic = cached
                b_reference = reference_index(b_bars, event_at, session)
                if b_reference is not None:
                    benchmark_windows = compute_windows(
                        b_bars, b_reference, now=moment
                    )["windows"]

        _write_reaction(
            conn, pair, ticker,
            session=session, computed=computed, source=source, synthetic=synthetic,
            benchmark_windows=benchmark_windows,
            benchmark_synthetic=benchmark_synthetic, stamp=stamp,
        )
        for outcome in computed["windows"].values():
            report[outcome["state"] if outcome["state"] != STATE_RESOLVED else "resolved"] += 1

    conn.commit()
    report["outstanding"] = conn.execute(
        "SELECT count(*) AS n FROM event_reaction_windows WHERE state = ?", (STATE_PENDING,)
    ).fetchone()["n"]
    return report


def _upsert_reaction_row(conn, pair, ticker, *, session, stamp, computed=None,
                         source="", synthetic=0, benchmark=""):
    conn.execute(
        "INSERT INTO event_reactions (event_id, ticker, event_at, session, reference_ts, "
        " reference_close, reference_source, synthetic, pre_volume_avg, pre_range_avg, "
        " benchmark_ticker, calc_version, captured_at, updated_at) "
        "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?) "
        "ON CONFLICT (event_id, ticker) DO UPDATE SET "
        # A retry can fail after a short horizon has already resolved. In that case the incoming
        # unavailable attempt has no reference data and must not erase the point-in-time metadata
        # belonging to the resolved window. A later successful retry still replaces nulls normally.
        "  session = CASE WHEN EXCLUDED.reference_ts IS NULL "
        "    AND event_reactions.reference_ts IS NOT NULL THEN event_reactions.session "
        "    ELSE EXCLUDED.session END, "
        "  reference_ts = COALESCE(EXCLUDED.reference_ts, event_reactions.reference_ts), "
        "  reference_close = COALESCE(EXCLUDED.reference_close, event_reactions.reference_close), "
        "  reference_source = CASE WHEN EXCLUDED.reference_source = '' "
        "    THEN event_reactions.reference_source ELSE EXCLUDED.reference_source END, "
        "  synthetic = CASE WHEN EXCLUDED.reference_ts IS NULL "
        "    AND event_reactions.reference_ts IS NOT NULL THEN event_reactions.synthetic "
        "    ELSE EXCLUDED.synthetic END, "
        "  pre_volume_avg = COALESCE(EXCLUDED.pre_volume_avg, event_reactions.pre_volume_avg), "
        "  pre_range_avg = COALESCE(EXCLUDED.pre_range_avg, event_reactions.pre_range_avg), "
        "  benchmark_ticker = CASE WHEN EXCLUDED.benchmark_ticker = '' "
        "    THEN event_reactions.benchmark_ticker ELSE EXCLUDED.benchmark_ticker END, "
        "  updated_at = EXCLUDED.updated_at",
        (
            pair["event_id"], ticker, pair["scheduled_at"], session,
            (computed or {}).get("referenceTs"), (computed or {}).get("referenceClose"),
            source, synthetic, (computed or {}).get("preVolumeAvg"),
            (computed or {}).get("preRangeAvg"), benchmark, CALC_VERSION, stamp, stamp,
        ),
    )


def _write_unavailable(conn, pair, ticker, reason, stamp, *, session=SESSION_NON_TRADING,
                       detail=""):
    _upsert_reaction_row(conn, pair, ticker, session=session, stamp=stamp)
    for horizon in HORIZONS:
        _upsert_window(conn, pair["event_id"], ticker, horizon, {
            "state": STATE_UNAVAILABLE, "missingReason": (reason + (f": {detail}" if detail else ""))[:200],
        }, None, False, "", stamp)
    conn.commit()


def _write_reaction(conn, pair, ticker, *, session, computed, source, synthetic,
                    benchmark_windows, benchmark_synthetic, stamp):
    _upsert_reaction_row(
        conn, pair, ticker, session=session, stamp=stamp, computed=computed,
        source=source, synthetic=1 if synthetic else 0,
        benchmark=BENCHMARK_TICKER if benchmark_windows else "",
    )
    for horizon, outcome in computed["windows"].items():
        benchmark = benchmark_windows.get(horizon) if benchmark_windows else None
        _upsert_window(conn, pair["event_id"], ticker, horizon, outcome, benchmark,
                       synthetic or benchmark_synthetic, source, stamp)


def _upsert_window(conn, event_id, ticker, horizon, outcome, benchmark, synthetic, source, stamp):
    """Write one horizon. A row already `resolved` is NEVER overwritten.

    That is what makes the pass idempotent and restart-safe in the way that matters: a resolved
    reaction is a point-in-time record of what the store held when the window matured, and
    recomputing it later against revised bars would silently rewrite history.
    """
    benchmark_return = None
    excess = None
    if outcome.get("state") == STATE_RESOLVED and benchmark and benchmark.get("state") == STATE_RESOLVED:
        benchmark_return = benchmark.get("rawReturn")
        if benchmark_return is not None and outcome.get("rawReturn") is not None:
            excess = outcome["rawReturn"] - benchmark_return

    conn.execute(
        "INSERT INTO event_reaction_windows (event_id, ticker, horizon, state, missing_reason, "
        " end_ts, end_close, raw_return, benchmark_return, excess_return, volume_change, "
        " range_change, bars_used, bar_source, synthetic, resolved_at, updated_at) "
        "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) "
        "ON CONFLICT (event_id, ticker, horizon) DO UPDATE SET "
        "  state = EXCLUDED.state, missing_reason = EXCLUDED.missing_reason, "
        "  end_ts = EXCLUDED.end_ts, end_close = EXCLUDED.end_close, "
        "  raw_return = EXCLUDED.raw_return, benchmark_return = EXCLUDED.benchmark_return, "
        "  excess_return = EXCLUDED.excess_return, volume_change = EXCLUDED.volume_change, "
        "  range_change = EXCLUDED.range_change, bars_used = EXCLUDED.bars_used, "
        "  bar_source = EXCLUDED.bar_source, synthetic = EXCLUDED.synthetic, "
        "  resolved_at = EXCLUDED.resolved_at, updated_at = EXCLUDED.updated_at "
        "WHERE event_reaction_windows.state <> ?",
        (
            event_id, ticker, horizon, outcome.get("state", STATE_PENDING),
            outcome.get("missingReason"), outcome.get("endTs"), outcome.get("endClose"),
            outcome.get("rawReturn"), benchmark_return, excess,
            outcome.get("volumeChange"), outcome.get("rangeChange"), outcome.get("barsUsed"),
            source, 1 if synthetic else 0,
            stamp if outcome.get("state") == STATE_RESOLVED else None, stamp,
            STATE_RESOLVED,
        ),
    )


# ── empirical sensitivity ────────────────────────────────────────────────────────────────────────


def _median(values: list[float]) -> float:
    ordered = sorted(values)
    middle = len(ordered) // 2
    if len(ordered) % 2:
        return ordered[middle]
    return (ordered[middle - 1] + ordered[middle]) / 2


def _stdev(values: list[float]) -> float | None:
    if len(values) < 2:
        return None
    mean = sum(values) / len(values)
    return math.sqrt(sum((v - mean) ** 2 for v in values) / (len(values) - 1))


def _quantile(values: list[float], q: float) -> float:
    ordered = sorted(values)
    if len(ordered) == 1:
        return ordered[0]
    position = q * (len(ordered) - 1)
    low = int(math.floor(position))
    high = int(math.ceil(position))
    if low == high:
        return ordered[low]
    return ordered[low] + (ordered[high] - ordered[low]) * (position - low)


def _distribution(values: list[float]) -> dict:
    """A DISTRIBUTION, not a number. Every field here exists to stop a single figure being quoted."""
    return {
        "count": len(values),
        "median": _median(values),
        "mean": sum(values) / len(values),
        "stdev": _stdev(values),
        "p25": _quantile(values, 0.25),
        "p75": _quantile(values, 0.75),
        "min": min(values),
        "max": max(values),
        "positiveCount": sum(1 for v in values if v > 0),
        "negativeCount": sum(1 for v in values if v < 0),
        "positiveFrequency": sum(1 for v in values if v > 0) / len(values),
    }


def sensitivity(
    conn: Connection,
    *,
    ticker: str | None = None,
    kind: str | None = None,
    series: str | None = None,
    horizon: str = "1d",
    as_of: str | None = None,
) -> dict:
    """Aggregate MATURED, NON-SYNTHETIC reactions — or refuse, with the count that was short.

    The refusal is the feature. Below the floor this returns `sufficient: false` and NO statistic:
    no median, no frequency, no direction, not even a rounded one. A caller that wants to show
    something anyway has nothing to show, which is the intent.

    `as_of` bounds the sample point-in-time: only reactions RESOLVED at or before the cutoff count,
    so a historical view cannot be inflated by outcomes that had not happened yet.
    """
    if horizon not in HORIZONS:
        raise ValueError(f"horizon must be one of {sorted(HORIZONS)}")

    where = [
        "w.horizon = :horizon",
        "w.state = :resolved",
        # THE SYNTHETIC EXCLUSION. Invariant #10: a synthetic bar may never validate an empirical
        # result. It is filtered here rather than at write time so the excluded rows remain
        # inspectable and the gap is explainable.
        "w.synthetic = 0",
        "r.synthetic = 0",
        "r.calc_version = :calc_version",
    ]
    params: dict = {
        "horizon": horizon, "resolved": STATE_RESOLVED, "calc_version": CALC_VERSION,
    }
    if ticker:
        where.append("w.ticker = :ticker")
        params["ticker"] = ticker.strip().upper()
    if kind:
        where.append("s.kind = :kind")
        params["kind"] = kind
    if series:
        where.append("s.series = :series")
        params["series"] = series
    if as_of:
        where.append("w.resolved_at <= :as_of")
        params["as_of"] = as_of

    rows = conn.execute(
        "SELECT w.raw_return, w.excess_return, w.benchmark_return, w.volume_change, "
        "       w.range_change, s.kind, s.series "
        "FROM event_reaction_windows w "
        "JOIN event_reactions r ON r.event_id = w.event_id AND r.ticker = w.ticker "
        "JOIN scheduled_events s ON s.id = w.event_id "
        f"WHERE {' AND '.join(where)}",
        params,
    ).fetchall()

    raw = [float(r["raw_return"]) for r in rows if r["raw_return"] is not None]
    excess = [float(r["excess_return"]) for r in rows if r["excess_return"] is not None]
    volume = [float(r["volume_change"]) for r in rows if r["volume_change"] is not None]
    range_change = [float(r["range_change"]) for r in rows if r["range_change"] is not None]

    floor = min_sample()
    taxonomy = {
        "ticker": (ticker or "").strip().upper() or None,
        "kind": kind, "series": series, "horizon": horizon,
    }
    base = {
        "taxonomy": taxonomy,
        "sampleCount": len(raw),
        "minimumSample": floor,
        "calcVersion": CALC_VERSION,
        "sensitivityVersion": SENSITIVITY_VERSION,
        "asOf": as_of,
        "syntheticExcluded": True,
        # Said in the payload, not only in a docstring: a consumer that renders this must be able to
        # render the disclaimer with it.
        "interpretation": "association, not causation: these are historical moves around events of "
                          "this type, not an estimate of what this event will do",
    }

    if len(raw) < floor:
        return {
            **base,
            "sufficient": False,
            "reason": "insufficient history",
            "shortBy": floor - len(raw),
            # NO statistic. Not a rounded one, not a hidden one.
            "raw": None, "excess": None, "benchmarkCoverage": None,
            "volumeChange": None, "rangeChange": None,
        }

    return {
        **base,
        "sufficient": True,
        "reason": None,
        "raw": _distribution(raw),
        "excess": _distribution(excess) if len(excess) >= floor else None,
        "benchmarkCoverage": len(excess) / len(raw) if raw else None,
        "volumeChange": _distribution(volume) if len(volume) >= floor else None,
        "rangeChange": _distribution(range_change) if len(range_change) >= floor else None,
    }


# ── HTTP surface (§9.28: the router is exported as `router`) ---------------------------------------
#
# READS ONLY. Both routes serve rows a capture pass already wrote; neither fetches a bar, and
# neither can start a capture. Opening Calendar or Portfolio therefore cannot cause a provider call.

from fastapi import APIRouter, HTTPException, Query  # noqa: E402

from .db import connect  # noqa: E402

router = APIRouter()


def _window_json(row) -> dict:
    return {
        "horizon": row["horizon"],
        "state": row["state"],
        "missingReason": row["missing_reason"],
        "endTs": row["end_ts"],
        "endClose": row["end_close"],
        "rawReturn": row["raw_return"],
        "benchmarkReturn": row["benchmark_return"],
        "excessReturn": row["excess_return"],
        "volumeChange": row["volume_change"],
        "rangeChange": row["range_change"],
        "barsUsed": row["bars_used"],
        "barSource": row["bar_source"],
        "synthetic": bool(row["synthetic"]),
        "resolvedAt": row["resolved_at"],
    }


@router.get("/reactions")
def get_reactions(
    event_id: str | None = Query(default=None, alias="eventId"),
    ticker: str | None = Query(default=None),
    limit: int = Query(default=50, ge=1, le=500),
) -> dict:
    """Stored before/after evidence for an event, or for a company's recent events.

    Every number carries the bars it came from, the bar source, the synthetic flag and the
    calculation version — so a reader can re-derive it, and so a look-ahead bug would be visible
    rather than merely untested.
    """
    if not event_id and not ticker:
        raise HTTPException(status_code=400, detail="eventId or ticker is required")

    where, params = [], {"limit": limit}
    if event_id:
        where.append("r.event_id = :event_id")
        params["event_id"] = event_id
    if ticker:
        where.append("r.ticker = :ticker")
        params["ticker"] = ticker.strip().upper()

    conn = connect()
    try:
        reactions = conn.execute(
            "SELECT r.*, s.title, s.kind, s.series, s.scheduled_at "
            "FROM event_reactions r JOIN scheduled_events s ON s.id = r.event_id "
            f"WHERE {' AND '.join(where)} "
            "ORDER BY r.event_at DESC, r.ticker LIMIT :limit",
            params,
        ).fetchall()
        keys = [(r["event_id"], r["ticker"]) for r in reactions]
        windows: dict[tuple, list] = {}
        for event, symbol in keys:
            windows[(event, symbol)] = conn.execute(
                "SELECT * FROM event_reaction_windows WHERE event_id = ? AND ticker = ? "
                "ORDER BY horizon",
                (event, symbol),
            ).fetchall()
    finally:
        conn.close()

    return {
        "reactions": [
            {
                "eventId": r["event_id"],
                "ticker": r["ticker"],
                "eventAt": r["event_at"],
                "title": r["title"],
                "kind": r["kind"],
                "series": r["series"],
                "session": r["session"],
                "referenceTs": r["reference_ts"],
                "referenceClose": r["reference_close"],
                "referenceSource": r["reference_source"],
                "synthetic": bool(r["synthetic"]),
                "preVolumeAvg": r["pre_volume_avg"],
                "preRangeAvg": r["pre_range_avg"],
                "benchmarkTicker": r["benchmark_ticker"] or None,
                "calcVersion": r["calc_version"],
                "capturedAt": r["captured_at"],
                "windows": [_window_json(w) for w in windows.get((r["event_id"], r["ticker"]), [])],
            }
            for r in reactions
        ],
        "horizons": sorted(HORIZONS),
        "degraded": [],
    }


@router.get("/sensitivity")
def get_sensitivity(
    ticker: str | None = Query(default=None),
    kind: str | None = Query(default=None),
    series: str | None = Query(default=None),
    horizon: str = Query(default="1d"),
    as_of: str | None = Query(default=None),
) -> dict:
    """Aggregated historical sensitivity — or `sufficient: false` and no statistic at all.

    A caller below the sample floor receives the count it is short by and NOTHING else. There is
    deliberately no "provisional" mode: a percentage is acted on whatever is printed beside it.
    """
    if horizon not in HORIZONS:
        raise HTTPException(status_code=400,
                            detail=f"horizon must be one of {sorted(HORIZONS)}")
    conn = connect()
    try:
        return sensitivity(
            conn, ticker=ticker, kind=kind, series=series, horizon=horizon, as_of=as_of,
        )
    finally:
        conn.close()
