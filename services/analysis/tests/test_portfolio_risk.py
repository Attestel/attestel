from __future__ import annotations

import math

import numpy as np
import pandas as pd
import pytest

from app.data import BarWindow
from app.portfolio_risk import (
    MIN_OBSERVATIONS,
    PORTFOLIO_RISK_VERSION,
    PortfolioRiskInputError,
    RiskPosition,
    build_portfolio_risk,
    calculate_portfolio_risk,
    parse_request,
)


def prices(returns: list[float], start: float = 100.0) -> pd.Series:
    values = [start]
    for value in returns:
        values.append(values[-1] * (1.0 + value))
    return pd.Series(values, index=pd.date_range("2025-01-02", periods=len(values), freq="B"))


def window(ticker: str, series: pd.Series, source: str = "fixture") -> BarWindow:
    frame = pd.DataFrame(
        {
            "open": series,
            "high": series,
            "low": series,
            "close": series,
            "volume": 1_000_000,
        },
        index=series.index,
    )
    return BarWindow(
        df=frame,
        timeframe="1D",
        source=source,
        source_is_synthetic=source == "synthetic",
        coverage="ok",
        coverage_from=series.index.min().strftime("%Y-%m-%dT00:00:00Z"),
        coverage_to=series.index.max().strftime("%Y-%m-%dT00:00:00Z"),
        as_of=series.index.max().strftime("%Y-%m-%dT00:00:00Z"),
        historical=False,
    )


def test_exact_weighted_volatility_beta_drawdown_and_correlation():
    n = 80
    benchmark_returns = [0.01 if i % 2 == 0 else -0.006 for i in range(n)]
    a_returns = [2 * r for r in benchmark_returns]
    b_returns = [0.5 * r for r in benchmark_returns]
    positions = [RiskPosition("AAA", 0.4), RiskPosition("BBB", 0.2)]
    result = calculate_portfolio_risk(
        positions,
        {"AAA": prices(a_returns), "BBB": prices(b_returns)},
        "SPY",
        prices(benchmark_returns),
        sources={"AAA": "fixture", "BBB": "fixture", "SPY": "fixture"},
    )

    expected_returns = pd.Series([0.4 * a + 0.2 * b for a, b in zip(a_returns, b_returns)])
    expected_vol = expected_returns.std(ddof=1) * math.sqrt(252)
    expected_beta = expected_returns.cov(pd.Series(benchmark_returns)) / pd.Series(benchmark_returns).var(ddof=1)
    wealth = pd.concat([pd.Series([1.0]), (1 + expected_returns).cumprod()], ignore_index=True)
    expected_drawdown = abs((wealth / wealth.cummax() - 1).min())

    assert result["modelVersion"] == PORTFOLIO_RISK_VERSION
    assert result["available"] is True
    assert result["complete"] is True
    assert result["equityWeight"] == 0.6  # the uninvested 40% is zero-return cash
    assert result["annualizedVolatility"] == pytest.approx(expected_vol, abs=1e-8)
    assert result["beta"] == pytest.approx(expected_beta, abs=1e-8)
    assert result["maximumDrawdown"] == pytest.approx(expected_drawdown, abs=1e-8)
    assert result["correlations"] == [
        {"tickerA": "AAA", "tickerB": "BBB", "correlation": 1.0, "observations": n}
    ]


def test_missing_position_withholds_aggregate_metrics_instead_of_treating_it_as_cash():
    returns = [0.01, -0.005] * 30
    result = calculate_portfolio_risk(
        [RiskPosition("AAA", 0.5), RiskPosition("MISSING", 0.25)],
        {"AAA": prices(returns)},
        "SPY",
        prices(returns),
    )
    assert result["available"] is False
    assert result["missingTickers"] == ["MISSING"]
    assert result["coveredWeight"] == 0.5
    assert result["annualizedVolatility"] is None
    assert result["beta"] is None


@pytest.mark.parametrize(
    "body,message",
    [
        ({}, "positions must be a non-empty array"),
        ({"positions": [{"ticker": "NVDA", "weight": -0.2}]}, "must be in (0,1]"),
        (
            {"positions": [{"ticker": "NVDA", "weight": 0.6}, {"ticker": "AMD", "weight": 0.5}]},
            "cannot sum to more than one",
        ),
        (
            {"positions": [{"ticker": "NVDA", "weight": 0.2}, {"ticker": "nvda", "weight": 0.2}]},
            "duplicate ticker",
        ),
        ({"positions": [{"ticker": "NVDA", "weight": 0.2}], "lookbackDays": 10}, "between 60 and 3650"),
    ],
)
def test_request_validation(body, message):
    with pytest.raises(PortfolioRiskInputError, match=message.replace("(", r"\(")):
        parse_request(body)


def test_builder_uses_point_in_time_loader_and_labels_synthetic_sources():
    returns = [0.004, -0.002] * (MIN_OBSERVATIONS + 5)
    calls = []

    def loader(ticker, timeframe, **kwargs):
        calls.append((ticker, timeframe, kwargs))
        return window(ticker, prices(returns), "synthetic" if ticker == "NVDA" else "fixture")

    result = build_portfolio_risk(
        {
            "positions": [{"ticker": "NVDA", "weight": 0.7}],
            "benchmark": "SPY",
            "lookbackDays": 420,
            "asOf": "2025-12-31T00:00:00Z",
        },
        loader=loader,
    )
    assert [c[0] for c in calls] == ["NVDA", "SPY"]
    assert all(c[1] == "1D" and c[2]["as_of"] == "2025-12-31T00:00:00Z" for c in calls)
    assert result["sourceIsSynthetic"] is True
    assert result["syntheticTickers"] == ["NVDA"]
    assert result["asOf"] == "2025-12-31T00:00:00Z"


def test_non_finite_input_is_rejected():
    with pytest.raises(PortfolioRiskInputError, match="finite"):
        parse_request({"positions": [{"ticker": "NVDA", "weight": np.nan}]})
