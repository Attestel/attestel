"""Deterministic portfolio risk calculations.

The portfolio service supplies already-valued position weights. This module reads point-in-time
price history through analysis.data.load_bars and calculates risk in code; no LLM is involved. Cash
is represented by weights summing to less than one and therefore contributes a zero return.
"""
from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass
import math
import re

import numpy as np
import pandas as pd

from .data import BarWindow, load_bars


PORTFOLIO_RISK_VERSION = "portfolio-risk-v1"
TRADING_DAYS = 252
MIN_OBSERVATIONS = 30
MAX_RISK_POSITIONS = 25
DEFAULT_LOOKBACK_DAYS = 520
_TICKER_RE = re.compile(r"^[A-Z0-9.\-]{1,16}$")


class PortfolioRiskInputError(ValueError):
    """The caller supplied an unsafe or ambiguous calculation input."""


@dataclass(frozen=True)
class RiskPosition:
    ticker: str
    weight: float


def _number(value, name: str) -> float:
    if isinstance(value, bool):
        raise PortfolioRiskInputError(f"{name} must be a number")
    try:
        out = float(value)
    except (TypeError, ValueError) as exc:
        raise PortfolioRiskInputError(f"{name} must be a number") from exc
    if not math.isfinite(out):
        raise PortfolioRiskInputError(f"{name} must be finite")
    return out


def parse_request(body: dict) -> tuple[list[RiskPosition], str, int, str | None]:
    if not isinstance(body, dict):
        raise PortfolioRiskInputError("request body must be an object")
    raw_positions = body.get("positions")
    if not isinstance(raw_positions, list) or not raw_positions:
        raise PortfolioRiskInputError("positions must be a non-empty array")
    if len(raw_positions) > MAX_RISK_POSITIONS:
        raise PortfolioRiskInputError(f"at most {MAX_RISK_POSITIONS} positions are allowed")
    positions: list[RiskPosition] = []
    seen: set[str] = set()
    for i, raw in enumerate(raw_positions):
        if not isinstance(raw, dict):
            raise PortfolioRiskInputError(f"positions[{i}] must be an object")
        ticker = str(raw.get("ticker") or "").strip().upper()
        if not _TICKER_RE.fullmatch(ticker):
            raise PortfolioRiskInputError(f"positions[{i}].ticker is invalid")
        if ticker in seen:
            raise PortfolioRiskInputError(f"duplicate ticker: {ticker}")
        seen.add(ticker)
        weight = _number(raw.get("weight"), f"positions[{i}].weight")
        if weight <= 0 or weight > 1:
            raise PortfolioRiskInputError(f"positions[{i}].weight must be in (0,1]")
        positions.append(RiskPosition(ticker=ticker, weight=weight))
    if sum(p.weight for p in positions) > 1.000001:
        raise PortfolioRiskInputError("position weights cannot sum to more than one")

    benchmark = str(body.get("benchmark") or "SPY").strip().upper()
    if not _TICKER_RE.fullmatch(benchmark):
        raise PortfolioRiskInputError("benchmark is invalid")
    raw_lookback = body.get("lookbackDays", DEFAULT_LOOKBACK_DAYS)
    if isinstance(raw_lookback, bool):
        raise PortfolioRiskInputError("lookbackDays must be an integer")
    try:
        lookback = int(raw_lookback)
    except (TypeError, ValueError) as exc:
        raise PortfolioRiskInputError("lookbackDays must be an integer") from exc
    if lookback < 60 or lookback > 3650:
        raise PortfolioRiskInputError("lookbackDays must be between 60 and 3650")
    as_of = body.get("asOf")
    if as_of is not None and not isinstance(as_of, str):
        raise PortfolioRiskInputError("asOf must be an RFC3339 string")
    return positions, benchmark, lookback, as_of


def _daily_close(window: BarWindow) -> pd.Series:
    if window.empty or "close" not in window.df:
        return pd.Series(dtype=float)
    series = pd.to_numeric(window.df["close"], errors="coerce").dropna()
    if series.empty:
        return series
    index = pd.DatetimeIndex(pd.to_datetime(series.index)).normalize()
    series = pd.Series(series.to_numpy(dtype=float), index=index)
    # Provider rows may contain multiple timestamps on one date. Risk is daily-primary, so the
    # final close for that date is the one observation used.
    return series.groupby(level=0).last().sort_index()


def _round(value: float | None) -> float | None:
    if value is None or not math.isfinite(float(value)):
        return None
    return round(float(value), 8)


def calculate_portfolio_risk(
    positions: list[RiskPosition],
    histories: dict[str, pd.Series],
    benchmark: str,
    benchmark_history: pd.Series,
    *,
    sources: dict[str, str] | None = None,
    synthetic_tickers: set[str] | None = None,
) -> dict:
    """Calculate risk from daily close histories.

    Aggregate metrics are withheld unless every held ticker has enough common observations. This is
    intentionally conservative: treating a missing position as cash would understate risk while
    looking precise. Pairwise correlations are still returned for pairs with usable overlap.
    """

    sources = sources or {}
    synthetic_tickers = synthetic_tickers or set()
    required = [p.ticker for p in positions]
    missing = sorted(t for t in required if t not in histories or len(histories[t].dropna()) < MIN_OBSERVATIONS + 1)
    correlations: list[dict] = []

    returns_by_ticker: dict[str, pd.Series] = {}
    for position in positions:
        series = histories.get(position.ticker, pd.Series(dtype=float))
        returns_by_ticker[position.ticker] = series.pct_change(fill_method=None).replace([np.inf, -np.inf], np.nan).dropna()

    ordered = sorted(required)
    for i, ticker_a in enumerate(ordered):
        for ticker_b in ordered[i + 1 :]:
            pair = pd.concat(
                [returns_by_ticker[ticker_a].rename("a"), returns_by_ticker[ticker_b].rename("b")],
                axis=1,
                join="inner",
            ).dropna()
            if len(pair) < MIN_OBSERVATIONS:
                continue
            correlations.append(
                {
                    "tickerA": ticker_a,
                    "tickerB": ticker_b,
                    "correlation": _round(pair["a"].corr(pair["b"])),
                    "observations": int(len(pair)),
                }
            )

    base = {
        "modelVersion": PORTFOLIO_RISK_VERSION,
        "available": False,
        "complete": False,
        "observations": 0,
        "coveredWeight": _round(sum(p.weight for p in positions if p.ticker not in missing)),
        "equityWeight": _round(sum(p.weight for p in positions)),
        "annualizedVolatility": None,
        "beta": None,
        "maximumDrawdown": None,
        "benchmark": benchmark,
        "benchmarkAvailable": False,
        "correlations": correlations,
        "from": None,
        "to": None,
        "sources": {ticker: sources.get(ticker, "unknown") for ticker in sorted(sources)},
        "sourceIsSynthetic": bool(synthetic_tickers),
        "syntheticTickers": sorted(synthetic_tickers),
        "missingTickers": missing,
    }
    if missing:
        return base

    frame = pd.concat(
        [returns_by_ticker[p.ticker].rename(p.ticker) for p in positions], axis=1, join="inner"
    ).dropna()
    if len(frame) < MIN_OBSERVATIONS:
        return base

    portfolio_returns = sum(frame[p.ticker] * p.weight for p in positions)
    volatility = portfolio_returns.std(ddof=1) * math.sqrt(TRADING_DAYS)
    wealth = pd.concat([pd.Series([1.0]), (1.0 + portfolio_returns).cumprod().reset_index(drop=True)], ignore_index=True)
    drawdown = wealth / wealth.cummax() - 1.0
    maximum_drawdown = abs(float(drawdown.min()))

    beta = None
    benchmark_available = False
    benchmark_returns = benchmark_history.pct_change(fill_method=None).replace([np.inf, -np.inf], np.nan).dropna()
    aligned = pd.concat(
        [portfolio_returns.rename("portfolio"), benchmark_returns.rename("benchmark")],
        axis=1,
        join="inner",
    ).dropna()
    if len(aligned) >= MIN_OBSERVATIONS:
        variance = float(aligned["benchmark"].var(ddof=1))
        if variance > 0 and math.isfinite(variance):
            beta = float(aligned["portfolio"].cov(aligned["benchmark"])) / variance
            benchmark_available = math.isfinite(beta)

    base.update(
        {
            "available": True,
            "complete": benchmark_available,
            "observations": int(len(frame)),
            "annualizedVolatility": _round(volatility),
            "beta": _round(beta),
            "maximumDrawdown": _round(maximum_drawdown),
            "benchmarkAvailable": benchmark_available,
            "from": frame.index.min().strftime("%Y-%m-%d"),
            "to": frame.index.max().strftime("%Y-%m-%d"),
        }
    )
    return base


def build_portfolio_risk(
    body: dict,
    loader: Callable[..., BarWindow] = load_bars,
) -> dict:
    positions, benchmark, lookback, as_of = parse_request(body)
    histories: dict[str, pd.Series] = {}
    sources: dict[str, str] = {}
    synthetic: set[str] = set()

    for ticker in sorted({p.ticker for p in positions} | {benchmark}):
        try:
            window = loader(
                ticker,
                "1D",
                as_of=as_of,
                limit=lookback,
                n=max(260, lookback),
                lookback_days=lookback,
            )
        except Exception:  # provider/store failure is represented as missing coverage, not a 500
            continue
        histories[ticker] = _daily_close(window)
        sources[ticker] = window.source
        if window.source_is_synthetic:
            synthetic.add(ticker)

    benchmark_history = histories.get(benchmark, pd.Series(dtype=float))
    position_histories = {p.ticker: histories.get(p.ticker, pd.Series(dtype=float)) for p in positions}
    result = calculate_portfolio_risk(
        positions,
        position_histories,
        benchmark,
        benchmark_history,
        sources=sources,
        synthetic_tickers=synthetic,
    )
    result["asOf"] = as_of
    result["lookbackDays"] = lookback
    return result
