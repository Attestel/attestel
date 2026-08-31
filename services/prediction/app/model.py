"""LightGBM directional classifier + walk-forward backtest orchestration.

Design (docs/PREDICTION_MODELS.md): the model is ~20% of the work, the honest walk-forward backtest
is ~80%. We:
  1. Split off a FINAL untouched holdout (never seen during walk-forward).
  2. Expanding-window walk-forward over the rest -> out-of-fold probabilities (no look-ahead).
  3. Turn OOF probabilities into positions, backtest net of costs -> the headline report.
  4. Train a fresh model on all-of-walk-forward, score the holdout once -> a holdout sanity check.
  5. Train the PRODUCTION model on ALL labelled data for live /predict.
The signal is only allowed to show if the headline report passes (positive edge, 0<Sharpe<=3,
enough trades). Sharpe>3 is flagged suspect (probable leakage), never presented as a great result.
"""
from __future__ import annotations

import numpy as np
import pandas as pd

from .backtest import derive_positions, is_passing, run_backtest
from .calibration import apply_calibration, brier_score, fit_isotonic, reliability_curve
from .features import Dataset

# Conservative LightGBM settings: shallow trees, strong regularization, single-threaded and
# deterministic. The goal is a modest, honest signal — not a leaderboard score.
LGB_PARAMS = dict(
    objective="binary",
    n_estimators=200,
    learning_rate=0.05,
    num_leaves=15,
    max_depth=4,
    min_child_samples=30,
    subsample=0.8,
    subsample_freq=1,
    colsample_bytree=0.8,
    reg_lambda=1.0,
    random_state=42,
    n_jobs=1,
    verbosity=-1,
)

DEFAULT_UPPER = 0.55
DEFAULT_LOWER = 0.45
MIN_ROWS = 150  # below this we can't walk-forward honestly


def _lgb():
    import lightgbm as lgb  # imported lazily so pure tests don't need the wheel

    return lgb


def _fit_predict(x_tr: pd.DataFrame, y_tr: pd.Series, x_te: pd.DataFrame) -> np.ndarray:
    """Fit on the training window, return p(up) for the test window. A degenerate single-class
    window predicts that class's constant probability (no model, no leakage)."""
    if y_tr.nunique() < 2:
        return np.full(len(x_te), float(y_tr.iloc[0]))
    lgb = _lgb()
    model = lgb.LGBMClassifier(**LGB_PARAMS)
    model.fit(x_tr, y_tr)
    return model.predict_proba(x_te)[:, 1]


def walk_forward(X: pd.DataFrame, y: pd.Series, init_frac: float = 0.5) -> tuple[pd.Series, int]:
    """Expanding-window walk-forward. Train on [0:start], predict [start:start+step], roll forward.
    Returns out-of-fold p(up) aligned to the predicted rows, plus the number of folds."""
    n = len(X)
    init = max(int(n * init_frac), 60)
    step = max(int(n * 0.1), 20)
    oof = pd.Series(index=X.index, dtype=float)
    folds = 0
    start = init
    while start < n:
        end = min(start + step, n)
        oof.iloc[start:end] = _fit_predict(X.iloc[:start], y.iloc[:start], X.iloc[start:end])
        folds += 1
        start = end
    return oof.dropna(), folds


def train_and_backtest(
    ds: Dataset,
    timeframe: str,
    horizon: int,
    allow_short: bool,
    cost_bps: float,
    upper: float = DEFAULT_UPPER,
    lower: float = DEFAULT_LOWER,
    holdout_frac: float = 0.2,
) -> tuple[object, dict, object | None] | None:
    """Returns (production_model, report, calibrator) or None if there isn't enough data to
    validate honestly. The calibrator (calibration.py) is fitted on walk-forward OOF probabilities
    and judged on the holdout; None when there wasn't enough data to fit one."""
    n = len(ds.X)
    if n < MIN_ROWS or ds.y.nunique() < 2:
        return None

    holdout = max(int(n * holdout_frac), 30)
    wf_end = n - holdout
    x_wf, y_wf, rn_wf = ds.X.iloc[:wf_end], ds.y.iloc[:wf_end], ds.ret_next.iloc[:wf_end]

    # 2-3. walk-forward OOF -> positions -> headline backtest
    oof, folds = walk_forward(x_wf, y_wf)
    if len(oof) < 30:
        return None
    positions = derive_positions(oof.to_numpy(), upper, lower, allow_short)
    report = run_backtest(rn_wf.loc[oof.index].to_numpy(), positions, cost_bps, timeframe)
    report["numFolds"] = folds
    report["oofRows"] = int(len(oof))

    # 4. final untouched holdout: train on all walk-forward, score holdout once
    x_ho, y_ho, rn_ho = ds.X.iloc[wf_end:], ds.y.iloc[wf_end:], ds.ret_next.iloc[wf_end:]
    ho_prob = _fit_predict(x_wf, y_wf, x_ho)
    ho_pos = derive_positions(ho_prob, upper, lower, allow_short)
    ho_report = run_backtest(rn_ho.to_numpy(), ho_pos, cost_bps, timeframe)
    report["holdout"] = {
        k: ho_report[k]
        for k in ("directionalHitRate", "strategySharpe", "expectancy", "maxDrawdown", "numTrades", "suspect")
    }
    report["holdout"]["rows"] = int(len(x_ho))

    # 4b. calibration: fit isotonic on the OOF probabilities (out-of-sample by construction),
    # judge it on the untouched holdout. Positions above stay on RAW probabilities — calibration
    # changes what we display and believe, never what the gate validated.
    oof_np, y_oof = oof.to_numpy(), y_wf.loc[oof.index].to_numpy()
    cal = fit_isotonic(oof_np, y_oof)
    y_ho_np = y_ho.to_numpy()
    report["calibration"] = {
        "method": "isotonic (PAV), fitted on walk-forward OOF probabilities",
        "fitted": cal is not None,
        "oofBrier": round(brier_score(oof_np, y_oof), 4),
        "holdoutBrierRaw": round(brier_score(ho_prob, y_ho_np), 4),
        "holdoutBrierCalibrated": (
            round(brier_score(apply_calibration(cal, ho_prob), y_ho_np), 4) if cal is not None else None
        ),
        "reliability": reliability_curve(oof_np, y_oof),
    }

    report["thresholds"] = {"upper": upper, "lower": lower}
    report["allowShort"] = bool(allow_short)
    report["costBps"] = cost_bps
    report["horizon"] = horizon
    report["timeframe"] = timeframe
    report["trainRows"] = int(n)
    report["passed"] = is_passing(report)

    # 5. production model on ALL labelled data (for live /predict)
    lgb = _lgb()
    prod = lgb.LGBMClassifier(**LGB_PARAMS)
    prod.fit(ds.X, ds.y)
    report["featureImportances"] = {
        c: int(v) for c, v in zip(ds.X.columns, prod.feature_importances_)
    }
    return prod, report, cal


def predict_prob(model, feature_row: pd.Series, columns: list[str]) -> float:
    """p(up) for a single current feature row."""
    x = pd.DataFrame([feature_row[columns].to_dict()])[columns]
    return float(model.predict_proba(x)[:, 1][0])


def derive_direction(prob_up: float, upper: float, lower: float, allow_short: bool) -> str:
    """Map probability to a shown bias. Sell is only offered when shorting was actually backtested;
    otherwise low p(up) means 'don't be long' = Hold."""
    if prob_up >= upper:
        return "Buy"
    if allow_short and prob_up <= lower:
        return "Sell"
    return "Hold"


def confidence_breakdown(prob_up: float, report: dict) -> dict:
    """The factors behind the confidence number, so a '2%' is explainable in the UI:

      confidence = base × sharpeFactor × tradesFactor × suspectFactor

    base         = |p-0.5| / 0.5           — probability strength, 0..1
    sharpeFactor = clamp(Sharpe / 2, 0..1) — full credit only at Sharpe >= 2
    tradesFactor = clamp(trades / 100, 0..1) — full credit only at 100+ OOF trades
    suspectFactor = 0.3 if the backtest is leakage-suspect (Sharpe > 3), else 1.0

    Tempering is the point: a confident probability on a weakly-validated model must NOT read as a
    confident call."""
    base = min(abs(prob_up - 0.5) / 0.5, 1.0)
    sharpe_factor = min(max(report.get("strategySharpe", 0.0), 0.0) / 2.0, 1.0)
    trades_factor = min(report.get("numTrades", 0) / 100.0, 1.0)
    suspect_factor = 0.3 if report.get("suspect") else 1.0
    return {
        "base": round(base, 3),
        "sharpeFactor": round(sharpe_factor, 3),
        "tradesFactor": round(trades_factor, 3),
        "suspectFactor": suspect_factor,
        "confidence": round(base * sharpe_factor * trades_factor * suspect_factor, 2),
    }


def confidence_score(prob_up: float, report: dict) -> float:
    """|p-0.5| scaled to 0..1, TEMPERED by backtest quality: low when suspect, weak Sharpe, or few
    trades. See confidence_breakdown for the factor-by-factor decomposition."""
    return confidence_breakdown(prob_up, report)["confidence"]
