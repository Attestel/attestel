"""Honest walk-forward backtest math — pure numpy, no model dependency (so it is unit-testable on a
crafted series with a known answer).

The whole point of the prediction phase (docs/PREDICTION_MODELS.md): a directional label is only
allowed to be shown WITH a validated track record. This module turns out-of-fold probabilities into
positions, applies transaction costs + slippage on every position change, and reports the metrics
that gate the signal. A Sharpe above 3 is flagged suspect (probable leakage), never celebrated.
"""
from __future__ import annotations

import numpy as np

# Bars per year, per timeframe, for annualizing Sharpe (~252 trading days; ~6.5h/day intraday).
ANNUALIZATION = {"1D": 252.0, "1H": 252.0 * 7, "15m": 252.0 * 26, "5m": 252.0 * 78}


def annualization(timeframe: str) -> float:
    return ANNUALIZATION.get(timeframe, 252.0)


def derive_positions(prob_up: np.ndarray, upper: float, lower: float, allow_short: bool) -> np.ndarray:
    """Map p(up) -> position. Long when confident up; flat otherwise (short only if allowed)."""
    prob_up = np.asarray(prob_up, dtype=float)
    pos = np.zeros_like(prob_up)
    pos[prob_up >= upper] = 1.0
    if allow_short:
        pos[prob_up <= lower] = -1.0
    return pos


def max_drawdown(equity: np.ndarray) -> float:
    """Largest peak-to-trough decline of the equity curve, as a POSITIVE fraction (0.12 = 12%)."""
    if len(equity) == 0:
        return 0.0
    peak = np.maximum.accumulate(equity)
    dd = equity / peak - 1.0
    return float(-dd.min()) if len(dd) else 0.0


def net_returns(
    ret_next: np.ndarray, positions: np.ndarray, cost_bps: float
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    """The per-bar accounting atom, shared by the gated backtest AND the offline evaluator so the two
    can never drift apart. Returns (net, entries, active):

      net[t]     = positions[t]*ret_next[t] - cost_on_turnover[t]   (net return earned over bar t)
      entries[t] = True where a NEW nonzero position is opened at t  (a "trade")
      active[t]  = True where a nonzero position is held into bar t

    cost_bps (commission + slippage) is charged on |Δposition| (turnover) at every position change.
    """
    ret_next = np.asarray(ret_next, dtype=float)
    positions = np.asarray(positions, dtype=float)
    if len(ret_next) == 0:
        empty = np.array([], dtype=float)
        return empty, empty.astype(bool), empty.astype(bool)
    prev = np.concatenate([[0.0], positions[:-1]])
    turnover = np.abs(positions - prev)
    cost = (cost_bps / 10000.0) * turnover
    net = positions * ret_next - cost
    entries = (positions != prev) & (positions != 0.0)
    active = positions != 0.0
    return net, entries, active


def run_backtest(
    ret_next: np.ndarray,
    positions: np.ndarray,
    cost_bps: float,
    timeframe: str = "1D",
) -> dict:
    """Backtest a position series against next-bar returns, net of costs.

    ret_next[t]   = simple return earned over the bar following decision t (close_{t+1}/close_t - 1)
    positions[t]  = -1 / 0 / +1 held into that bar
    cost_bps      = commission + slippage, in basis points, charged on |Δposition| (turnover)

    Returns the report dict used to gate the signal.
    """
    ret_next = np.asarray(ret_next, dtype=float)
    positions = np.asarray(positions, dtype=float)
    net, entries, active = net_returns(ret_next, positions, cost_bps)
    n = len(net)
    if n == 0:
        return _empty_report()

    equity = np.cumprod(1.0 + net)
    total_return = float(equity[-1] - 1.0)
    mean, std = float(net.mean()), float(net.std(ddof=0))
    ann = annualization(timeframe)
    sharpe = float(mean / std * np.sqrt(ann)) if std > 1e-12 else 0.0

    # a "trade" = opening a (new) nonzero position
    num_trades = int(entries.sum())

    if active.any():
        correct = np.sign(positions[active]) == np.sign(ret_next[active])
        hit_rate = float(correct.mean())
    else:
        hit_rate = 0.0

    longs = positions > 0.0
    precision = float((ret_next[longs] > 0).mean()) if longs.any() else 0.0

    suspect = sharpe > 3.0  # Sharpe > 3 almost always means leakage (docs/PREDICTION_MODELS.md)

    return {
        "directionalHitRate": round(hit_rate, 4),
        "precision": round(precision, 4),
        "strategySharpe": round(sharpe, 3),
        "expectancy": round(mean, 6),         # mean net return per bar
        "totalReturn": round(total_return, 4),
        "maxDrawdown": round(max_drawdown(equity), 4),
        "numTrades": num_trades,
        "suspect": bool(suspect),
        "equityCurve": _downsample(equity, 120),
    }


def is_passing(report: dict, min_trades: int = 30) -> bool:
    """A backtest earns the right to show a signal only if it has a positive edge, a believable
    (not-suspect) Sharpe, and enough trades to mean something."""
    return bool(
        report.get("expectancy", 0) > 0
        and 0.0 < report.get("strategySharpe", 0) <= 3.0
        and report.get("numTrades", 0) >= min_trades
    )


def _downsample(equity: np.ndarray, max_points: int) -> list[float]:
    if len(equity) <= max_points:
        return [round(float(x), 5) for x in equity]
    idx = np.linspace(0, len(equity) - 1, max_points).astype(int)
    return [round(float(equity[i]), 5) for i in idx]


def _empty_report() -> dict:
    return {
        "directionalHitRate": 0.0, "precision": 0.0, "strategySharpe": 0.0,
        "expectancy": 0.0, "totalReturn": 0.0, "maxDrawdown": 0.0,
        "numTrades": 0, "suspect": False, "equityCurve": [],
    }
