"""Earnings-event data for the offline event-study harness (post-earnings drift / PEAD).

Two data sources, kept honest and cached:
  * Earnings history — Alpha Vantage EARNINGS (function=EARNINGS): per ticker, quarterly
    fiscalDateEnding, reportedDate, reportedEPS, estimatedEPS, surprise, surprisePercentage. Free
    tier is ~25 req/day, so each ticker's FULL history is fetched ONCE and cached to
    data/eval/earnings/{TICKER}.json; the run reuses cache and only fetches MISSING tickers — a large
    universe can be completed across a couple of days (or with a smaller universe). If no key is set
    and nothing is cached, the harness refuses. (Alternative provider: FMP earnings-surprises — not
    wired here; see docs.)
  * Price bars — the analysis /features endpoint (REAL prices), same as the price harness.

NO LOOK-AHEAD: entry is the OPEN of the first full trading day STRICTLY AFTER reportedDate, so all
earnings info is already public by then. build_events asserts entry_date > reportedDate for every
event, and SUE is computed from PRIOR surprises only.
"""
from __future__ import annotations

import bisect
import json
import os
import re
from dataclasses import dataclass, field
from datetime import datetime, timezone

import numpy as np
import requests

from . import db as _db

AV_URL = "https://www.alphavantage.co/query"


def _f(x) -> float | None:
    """Parse Alpha Vantage's stringly-typed numerics ('None'/'' -> None)."""
    if x is None:
        return None
    if isinstance(x, (int, float)):
        return float(x)
    s = str(x).strip()
    if s == "" or s.lower() == "none":
        return None
    try:
        return float(s)
    except ValueError:
        return None


@dataclass
class EarningsEvent:
    ticker: str
    fiscal_date: str
    reported_date: str
    reported_eps: float | None
    estimated_eps: float | None
    surprise: float | None
    surprise_pct: float | None
    sue: float | None = None
    sue_source: str = field(default="")
    estimate_verified: bool = False
    estimate_snapshot_at: str = ""
    estimate_provider: str = ""
    estimate_payload_sha256: str = ""


# --------------------------------------------------------------------------- fetch + cache

def earnings_cache_path(cache_dir: str, ticker: str) -> str:
    return os.path.join(cache_dir, f"{ticker.upper()}.json")


def fetch_earnings_av(ticker: str, api_key: str, *, timeout: int = 30) -> dict:
    """Fetch a ticker's full quarterly earnings history from Alpha Vantage. Raises on missing key,
    network error, or a rate-limit / error payload (AV returns those with HTTP 200)."""
    if not api_key:
        raise RuntimeError("no ALPHAVANTAGE_API_KEY")
    resp = requests.get(
        AV_URL,
        params={"function": "EARNINGS", "symbol": ticker.upper(), "apikey": api_key},
        timeout=timeout,
    )
    resp.raise_for_status()
    data = resp.json()
    # rate-limit / error signals (HTTP 200 with a Note/Information/Error Message)
    for k in ("Note", "Information", "Error Message"):
        if data.get(k):
            raise RuntimeError(f"alpha vantage: {data[k]}")
    if not data.get("quarterlyEarnings"):
        raise RuntimeError("alpha vantage: no quarterlyEarnings")
    return data


def get_earnings(ticker: str, cache_dir: str, api_key: str) -> tuple[list[EarningsEvent], str]:
    """Durable-cache-first earnings history.

    Production reads/writes PostgreSQL. The filesystem remains a compatibility import/fallback for
    the zero-database local workflow; when PostgreSQL is enabled, a legacy file is imported on first
    use instead of remaining the authoritative copy. Returns (events, source).
    """
    if _db.enabled():
        stored = _db.load_earnings_payload(ticker)
        if stored is not None:
            try:
                return parse_earnings(stored["payload"], ticker), "database"
            except (TypeError, ValueError):
                pass

    path = earnings_cache_path(cache_dir, ticker)
    if os.path.exists(path):
        try:
            with open(path) as fh:
                raw = json.load(fh)
            parsed = parse_earnings(raw, ticker)
            if _db.enabled():
                _db.save_earnings_payload(
                    ticker, "legacy-file", raw, vintage_status="unverified"
                )
                return parsed, "database-import"
            return parsed, "cache"
        except (OSError, json.JSONDecodeError, ValueError):
            pass  # corrupt cache -> re-fetch below

    try:
        raw = fetch_earnings_av(ticker, api_key)
    except Exception:  # noqa: BLE001 — no key / rate-limit / network: caller skips this ticker
        return [], ""
    if _db.enabled():
        _db.save_earnings_payload(ticker, "alpha-vantage", raw, vintage_status="unverified")
    else:
        os.makedirs(cache_dir, exist_ok=True)
        tmp = path + ".tmp"
        with open(tmp, "w") as fh:
            json.dump(raw, fh)
        os.replace(tmp, path)
    return parse_earnings(raw, ticker), "live"


def parse_earnings(raw: dict, ticker: str) -> list[EarningsEvent]:
    """Parse AV's quarterlyEarnings into chronological (oldest-first) EarningsEvents."""
    out: list[EarningsEvent] = []
    for q in raw.get("quarterlyEarnings", []) or []:
        reported = str(q.get("reportedDate") or "").strip()
        if not reported:
            continue
        out.append(
            EarningsEvent(
                ticker=ticker.upper(),
                fiscal_date=str(q.get("fiscalDateEnding") or "").strip(),
                reported_date=reported,
                reported_eps=_f(q.get("reportedEPS")),
                estimated_eps=_f(q.get("estimatedEPS")),
                surprise=_f(q.get("surprise")),
                surprise_pct=_f(q.get("surprisePercentage")),
            )
        )
    out.sort(key=lambda e: e.reported_date)  # oldest first — required for prior-only SUE
    return out


def attach_forward_estimates(events: list[EarningsEvent], snapshots: list[dict]) -> int:
    """Replace provider-history estimates with snapshots captured before each report date.

    Matching is exact on ticker fiscal period. A snapshot captured on the report date is rejected:
    the provider calendar does not reliably encode before/after-market release time, so only a
    prior UTC date is unambiguously pre-release.
    """
    by_event: dict[tuple[str, str], list[tuple[datetime, dict]]] = {}
    for snapshot in snapshots:
        ticker = str(snapshot.get("ticker") or "").strip().upper()
        fiscal = str(snapshot.get("fiscalDate") or "")[:10]
        captured = str(snapshot.get("capturedAt") or "")
        provider = str(snapshot.get("provider") or "").strip()
        payload_sha = str(snapshot.get("payloadSha256") or "").strip().lower()
        stage = str(snapshot.get("stage") or "")
        if (
            not ticker
            or not provider
            or stage not in ("t_minus_7", "t_minus_1")
            or re.fullmatch(r"[0-9a-f]{64}", payload_sha) is None
        ):
            continue
        try:
            captured_at = datetime.fromisoformat(captured.replace("Z", "+00:00"))
            if captured_at.tzinfo is None:
                continue
            consensus = float(snapshot["consensusEPS"])
        except (KeyError, TypeError, ValueError):
            continue
        if not np.isfinite(consensus):
            continue
        by_event.setdefault((ticker, fiscal), []).append(
            (captured_at.astimezone(timezone.utc), snapshot)
        )

    attached = 0
    for event in events:
        try:
            release_cutoff = datetime.fromisoformat(event.reported_date[:10]).replace(
                tzinfo=timezone.utc
            )
        except ValueError:
            continue
        eligible = [
            item for item in by_event.get((event.ticker.upper(), event.fiscal_date[:10]), [])
            if item[0] < release_cutoff
        ]
        if not eligible or event.reported_eps is None:
            continue
        captured_at, snapshot = max(eligible, key=lambda item: item[0])
        estimate = float(snapshot["consensusEPS"])
        surprise = event.reported_eps - estimate
        event.estimated_eps = estimate
        event.surprise = surprise
        event.surprise_pct = (surprise / abs(estimate) * 100.0) if abs(estimate) > 1e-12 else None
        event.estimate_verified = True
        event.estimate_snapshot_at = captured_at.isoformat()
        event.estimate_provider = str(snapshot.get("provider") or "")
        event.estimate_payload_sha256 = str(snapshot.get("payloadSha256") or "")
        attached += 1
    return attached


# --------------------------------------------------------------------------- SUE

def compute_sue(events: list[EarningsEvent], window: int = 8, min_prior: int = 4) -> None:
    """Fill each event's standardized unexpected earnings, IN PLACE, using ONLY prior surprises.

        SUE = (reportedEPS - estimatedEPS) / std(last `window` PRIOR surprises)   [prior-only -> no leakage]

    If absolute surprise is unavailable, surprisePercentage is standardized against PRIOR
    surprise percentages. Raw percentage/100 is deliberately not mixed with z-scores: those values
    live on different scales and made the cross-sectional tercile threshold meaningless. Early
    events without enough prior observations remain sue=None (skipped). Assumes chronological input.
    """
    surprises = [e.surprise for e in events]
    surprise_pcts = [e.surprise_pct for e in events]
    for i, e in enumerate(events):
        prior = [s for s in surprises[:i] if s is not None][-window:]
        if e.surprise is not None and len(prior) >= min_prior:
            sd = float(np.std(prior, ddof=1)) if len(prior) > 1 else 0.0
            if sd > 1e-9:
                e.sue = e.surprise / sd
                e.sue_source = "std"
                continue
        prior_pct = [s for s in surprise_pcts[:i] if s is not None][-window:]
        if e.surprise_pct is not None and len(prior_pct) >= min_prior:
            sd_pct = float(np.std(prior_pct, ddof=1)) if len(prior_pct) > 1 else 0.0
            if sd_pct > 1e-9:
                e.sue = e.surprise_pct / sd_pct
                e.sue_source = "pct-std"
                continue
        e.sue = None
        e.sue_source = ""


# --------------------------------------------------------------------------- event construction

def _first_after(dates: list[str], d: str) -> int | None:
    """Index of the first price date STRICTLY greater than d (the no-look-ahead entry bar)."""
    i = bisect.bisect_right(dates, d)  # first index whose date > d (dates sorted ascending)
    return i if i < len(dates) else None


def build_events(events: list[EarningsEvent], price_df, horizons: list[int]) -> list[dict]:
    """Attach the no-look-ahead entry (open of the first session AFTER reportedDate) and the forward
    return for each horizon. Drops events without a valid SUE or a full forward window.

    Returns JSON-friendly trade-event dicts:
      {ticker, reportedDate, entryDate, sue, sueSource, direction, surprisePct, returns:{H: ret}}
    """
    if price_df is None or len(price_df) == 0 or "open" not in price_df.columns:
        return []
    dates = [str(x)[:10] for x in price_df.index]
    opens = price_df["open"].to_numpy(dtype=float)

    out: list[dict] = []
    first_date = dates[0]
    for e in events:
        if e.sue is None or not e.reported_date:
            continue
        # The report must fall WITHIN our price coverage. If it predates the first bar we have, the
        # "first session after" would spuriously resolve to the window start (misaligned entry / a
        # subtle look-ahead) — so we drop it rather than invent an entry. (Yahoo's 1D window is ~2y.)
        if e.reported_date < first_date:
            continue
        entry_idx = _first_after(dates, e.reported_date)
        if entry_idx is None:
            continue
        entry_date = dates[entry_idx]
        # NO-LEAKAGE guard: the entry bar is strictly after the earnings release date.
        assert entry_date > e.reported_date, f"entry {entry_date} not strictly after report {e.reported_date}"
        entry_open = opens[entry_idx]
        if not np.isfinite(entry_open) or entry_open <= 0:
            continue
        rets: dict[int, float] = {}
        for h in horizons:
            j = entry_idx + h
            if j < len(opens) and np.isfinite(opens[j]) and opens[j] > 0:
                rets[h] = float(opens[j] / entry_open - 1.0)  # open-to-open, held H sessions
        if not rets:
            continue
        out.append({
            "ticker": e.ticker,
            "reportedDate": e.reported_date,
            "entryDate": entry_date,
            "sue": float(e.sue),
            "sueSource": e.sue_source,
            "direction": float(np.sign(e.sue)),
            "surprisePct": e.surprise_pct,
            "estimateVerified": e.estimate_verified,
            "estimateSnapshotAt": e.estimate_snapshot_at,
            "estimateProvider": e.estimate_provider,
            "estimatePayloadSha256": e.estimate_payload_sha256,
            "returns": rets,
        })
    return out
