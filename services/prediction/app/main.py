"""Prediction service (Python · FastAPI).

A LightGBM directional classifier over the indicator features the analysis service already computes,
VALIDATED by a walk-forward backtest. A Buy/Hold/Sell bias is returned ONLY with an attached passing
backtest and a confidence number; otherwise signal=null with reason "insufficient validation". The
app never trades — this is a suggestion; the human executes.
"""
from __future__ import annotations

from datetime import datetime, timezone

from fastapi import FastAPI, HTTPException, Request
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import JSONResponse

from .config import (
    ALPHAVANTAGE_API_KEY,
    EARNINGS_CACHE_DIR,
    ENABLE_CHRONOS,
)
from .calibration import apply_calibration
from .committee_features import load_committee_history
from . import autonomy, db, evalrun
from .config import MIN_COMMITTEE_SNAPSHOTS
from .context import fetch_context, load_earnings
from .features import (
    FEATURE_FRAME_POLICY,
    MODEL_FEATURES,
    build_dataset,
    fetch_feature_frame,
    latest_feature_row,
)
from .model import (
    confidence_breakdown,
    derive_direction,
    predict_prob,
    train_and_backtest,
)
from .store import (
    active_model_version,
    deploy_model_version,
    list_model_records,
    load_model,
    load_record,
    load_version_record,
    save_candidate,
    was_model_deployed,
)
from .strategy import COST_BPS, EXECUTION_CONTRACT_VERSION, default_strategy_version
from .verdicts import TRADEABLE_VERDICT, evaluation_block, expected_strategy_version

app = FastAPI(title="NVDA Platform — Prediction Service", version="0.1.0")
app.add_middleware(
    CORSMiddleware, allow_origins=["*"], allow_methods=["*"], allow_headers=["*"]
)

VALID_TF = {"1D", "1H", "15m", "5m"}
PREDICT_LOOKBACK = 400  # enough bars for every indicator (SMA200) when marking the current row
# COST_BPS lives in strategy.py: it is part of the strategy IDENTITY (it is hashed into
# strategyVersion), so it must have exactly one definition.


def _tf(timeframe: str) -> str:
    return timeframe if timeframe in VALID_TF else "1D"


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def _data_epoch(value) -> float | None:
    try:
        parsed = datetime.fromisoformat(str(value).replace("Z", "+00:00"))
    except (TypeError, ValueError):
        return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.timestamp()


def _backtest_summary(report: dict) -> dict:
    """The compact track record shown next to the signal (and next to a null one)."""
    return {
        "hitRate": report.get("directionalHitRate"),
        "sharpe": report.get("strategySharpe"),
        "expectancy": report.get("expectancy"),
        "maxDrawdown": report.get("maxDrawdown"),
        "numTrades": report.get("numTrades"),
        "suspect": report.get("suspect", False),
        "passed": report.get("passed", False),
        # Served so a consumer can record what the model was validated under rather than assume it:
        # `costBps` is the cost the backtest charged on every position change, and `allowShort` says
        # whether a Sell was ever backtested at all (docs/PAPER_EXECUTION_CONTRACT.md §1.1, §3.3).
        "costBps": report.get("costBps"),
        "allowShort": report.get("allowShort", False),
    }


def _meta(record: dict | None) -> dict:
    if not record:
        return {"modelVersion": None, "trainedAt": None, "dataThrough": None,
                "trainedOnSynthetic": False, "dataPolicy": None, "dataPolicyCurrent": False}
    return {
        "modelVersion": record.get("modelVersion"),
        "strategyVersion": record.get("strategyVersion") or expected_strategy_version(record.get("report")),
        "trainedAt": record.get("trainedAt"),
        "dataThrough": record.get("dataThrough"),
        "trainedOnSynthetic": record.get("trainedOnSynthetic", False),
        "dataPolicy": record.get("dataPolicy"),
        "dataPolicyCurrent": record.get("dataPolicy") == FEATURE_FRAME_POLICY,
    }


@app.get("/health")
def health():
    storage = "files"
    if db.enabled():
        storage = "postgresql"
        try:
            with db.connection() as conn:
                conn.execute("SELECT 1")
        except Exception as exc:
            return JSONResponse(
                status_code=503,
                content={
                    "status": "unavailable", "service": "prediction",
                    "storage": storage, "database": {"ok": False, "error": type(exc).__name__},
                },
            )
    return {
        "status": "ok", "service": "prediction", "chronos": ENABLE_CHRONOS,
        "storage": storage,
        # This service's DEFAULT strategy identity (docs/PAPER_EXECUTION_CONTRACT.md §4.3). A stored
        # verdict is checked against the version computed from the SERVED RECORD'S own thresholds /
        # cost / allowShort, not against this one — a record trained with allowShort=true is a
        # different strategy from the default, and must not be able to spend the default's verdict.
        "defaultStrategyVersion": default_strategy_version(),
        # Kept under the old key too so existing readers do not break; it means the same thing it
        # always displayed (the defaults), and now says so above.
        "strategyVersion": default_strategy_version(),
        "executionContract": EXECUTION_CONTRACT_VERSION,
    }


@app.get("/automation/status")
def automation_status():
    """Read-only evidence for the autonomous candidate/evaluator controller."""
    return autonomy.status()


@app.post("/train/{ticker}")
def train(ticker: str, timeframe: str = "1D", horizon: int = 5, allowShort: bool = False,
          lookbackDays: int = 1500):
    ticker, tf = ticker.upper(), _tf(timeframe)
    try:
        df, _source, is_syn = fetch_feature_frame(ticker, tf, lookbackDays)
    except Exception as e:  # noqa: BLE001 — analysis down / bad response
        raise HTTPException(status_code=502, detail=f"feature fetch failed: {e}") from e

    # Orthogonal context (market/sector + earnings cycle) — both fail soft to neutral features,
    # so the zero-keys path trains exactly as before.
    ctx = fetch_context(tf, lookbackDays)
    earnings = load_earnings(ticker, EARNINGS_CACHE_DIR, ALPHAVANTAGE_API_KEY)
    # Analyst-committee stance history (hybrid path). Neutral until MIN_COMMITTEE_SNAPSHOTS real
    # snapshot days exist — LLM stances cannot be backfilled without hindsight bias, so the
    # committee only influences the model once it has a genuine out-of-sample track record.
    committee = load_committee_history(ticker)

    ds = build_dataset(df, horizon, ctx=ctx, earnings=earnings, committee=committee)
    committee_active = len(committee) >= MIN_COMMITTEE_SNAPSHOTS
    result = train_and_backtest(ds, tf, horizon, allowShort, cost_bps=COST_BPS)
    if result is None:
        return {
            "trained": False,
            "reason": "insufficient data to validate a model (need more history / both up & down labels)",
            "ticker": ticker, "timeframe": tf, "horizon": horizon,
            "trainedOnSynthetic": is_syn, "rows": int(len(ds.X)),
        }
    model, report, cal = result
    record = save_candidate(ticker, tf, horizon, model, report, is_syn, df.index[-1],
                            features=MODEL_FEATURES, calibrator=cal,
                            data_policy=FEATURE_FRAME_POLICY)
    return {**record, "trained": True, "candidate": True, "active": False,
            "deploymentState": "candidate", "promotionRequired": True,
            "activeModelVersion": active_model_version(ticker, tf, horizon), "committeeFeatures":
            {"active": committee_active, "snapshots": len(committee),
             "required": MIN_COMMITTEE_SNAPSHOTS}}


def _promotion_gates(record: dict | None, active: dict | None = None) -> tuple[bool, list[dict], dict | None]:
    """Deterministic candidate eligibility. It spends the same evidence paper gate 4 spends."""
    if record is None:
        return False, [{"name": "candidate-exists", "passed": False,
                        "detail": "the requested immutable model version does not exist"}], None
    report = record.get("report") if isinstance(record.get("report"), dict) else {}
    expected = expected_strategy_version(report)
    evaluation = evaluation_block(
        record.get("ticker", ""), record.get("timeframe", ""), int(record.get("horizon", 0)),
        report, data_policy=record.get("dataPolicy"),
    )
    gates = [
        {
            "name": "real-training-data",
            "passed": not bool(record.get("trainedOnSynthetic")),
            "detail": "candidate was trained on real data" if not record.get("trainedOnSynthetic")
            else "candidate was trained on synthetic data",
        },
        {
            "name": "current-data-policy",
            "passed": record.get("dataPolicy") == FEATURE_FRAME_POLICY,
            "detail": f"data policy is {record.get('dataPolicy') or 'missing'}; expected {FEATURE_FRAME_POLICY}",
        },
        {
            "name": "strategy-identity",
            "passed": bool(expected) and record.get("strategyVersion") == expected,
            "detail": f"stored={record.get('strategyVersion') or 'missing'} expected={expected or 'unknown'}",
        },
        {
            "name": "walk-forward-backtest",
            "passed": bool(report.get("passed")) and not bool(report.get("suspect")),
            "detail": "candidate backtest passed without leakage suspicion" if report.get("passed")
            and not report.get("suspect") else "candidate backtest did not pass cleanly",
        },
        {
            "name": "pooled-evaluator-verdict",
            "passed": bool(
                evaluation
                and evaluation.get("verdict") == TRADEABLE_VERDICT
                and evaluation.get("current")
                and evaluation.get("evidenceCurrent")
            ),
            "detail": (
                "current pooled EDGE verdict with sufficient evidence"
                if evaluation and evaluation.get("verdict") == TRADEABLE_VERDICT
                and evaluation.get("current") and evaluation.get("evidenceCurrent")
                else "no current, sufficiently evidenced pooled EDGE verdict for this strategy"
            ),
        },
    ]
    record_strategy = record.get("strategyVersion") or expected
    active_strategy = None if active is None else (
        active.get("strategyVersion") or expected_strategy_version(active.get("report"))
    )
    if active is not None and record_strategy == active_strategy:
        candidate_through = str(record.get("dataThrough") or "")
        active_through = str(active.get("dataThrough") or "")
        candidate_epoch = _data_epoch(candidate_through)
        active_epoch = _data_epoch(active_through)
        gates.append({
            "name": "refresh-is-not-older",
            "passed": candidate_epoch is not None and active_epoch is not None
                      and candidate_epoch >= active_epoch,
            "detail": f"candidate dataThrough={candidate_through or 'missing'}; "
                      f"active dataThrough={active_through or 'missing'}",
        })
    return all(g["passed"] for g in gates), gates, evaluation


@app.get("/models/{ticker}")
def models(ticker: str, timeframe: str | None = None, horizon: int | None = None):
    ticker = ticker.upper()
    tf = _tf(timeframe) if timeframe else None
    rows = []
    for record in list_model_records(ticker, tf, horizon):
        active = load_record(ticker, record.get("timeframe"), int(record.get("horizon")))
        eligible, gates, evaluation = _promotion_gates(record, active)
        rows.append({**record, "promotionEligible": eligible, "promotionGates": gates,
                     "evaluation": evaluation})
    return {"ticker": ticker, "timeframe": tf, "horizon": horizon, "models": rows}


async def _deployment_request(
    ticker: str, model_version: str, timeframe: str, horizon: int,
    request: Request, action: str,
):
    actor = request.headers.get("X-Operator-Uid", "").strip()
    if not actor:
        raise HTTPException(status_code=403, detail="operator identity required")
    try:
        payload = await request.json()
    except Exception:  # noqa: BLE001 — an empty body means the documented default reason
        payload = {}
    reason = str((payload or {}).get("reason") or (
        "operator promoted an evidence-gated candidate"
        if action == "promote" else "operator rolled back the active model"
    )).strip()[:500]
    ticker, tf = ticker.upper(), _tf(timeframe)
    target = load_version_record(ticker, tf, horizon, model_version)
    active = load_record(ticker, tf, horizon)
    evidence = None
    if action == "promote":
        eligible, gates, evaluation = _promotion_gates(target, active)
        evidence = {"promotionGates": gates, "evaluation": evaluation}
        if not eligible:
            raise HTTPException(status_code=409, detail={
                "error": "candidate is not eligible for promotion", "promotionGates": gates,
            })
    elif not was_model_deployed(ticker, tf, horizon, model_version):
        # Rollback deliberately bypasses today's performance gates, but it is not an alternate
        # promotion route. Only a version that previously served may be restored this way.
        raise HTTPException(
            status_code=409,
            detail="rollback target has never been active; use evidence-gated promotion",
        )
    try:
        result = deploy_model_version(
            ticker, tf, horizon, model_version, action=action,
            actor_uid=actor, reason=reason, evidence=evidence,
        )
    except KeyError as exc:
        raise HTTPException(status_code=404, detail="model version not found") from exc
    return {"ok": True, "action": action, **result}


@app.post("/models/{ticker}/{model_version}/promote")
async def promote_model(
    ticker: str, model_version: str, request: Request, timeframe: str = "1D", horizon: int = 5
):
    return await _deployment_request(ticker, model_version, timeframe, horizon, request, "promote")


@app.post("/models/{ticker}/{model_version}/rollback")
async def rollback_model(
    ticker: str, model_version: str, request: Request, timeframe: str = "1D", horizon: int = 5
):
    return await _deployment_request(ticker, model_version, timeframe, horizon, request, "rollback")


@app.get("/predict/{ticker}")
def predict(ticker: str, timeframe: str = "1D", horizon: int = 5):
    ticker, tf = ticker.upper(), _tf(timeframe)
    model, cal, record = load_model(ticker, tf, horizon)
    base = {
        "ticker": ticker, "timeframe": tf, "horizon": horizon, "asOf": _now_iso(), **_meta(record),
        # The offline evaluator's persisted verdict for this config, or null when none exists
        # (docs/PAPER_EXECUTION_CONTRACT.md §4.3). Served on EVERY branch, including the ones that
        # return no signal: a consumer must be able to tell "not validated" from "not asked".
        "evaluation": evaluation_block(
            ticker, tf, horizon, (record or {}).get("report"),
            data_policy=(record or {}).get("dataPolicy"),
        ),
        # Provenance of the frame this prediction was actually computed from. null = not fetched
        # (an early return), which a caller must treat as unknown, not as clean.
        "currentData": None,
    }

    # no model at all, or the backtest didn't earn the right to show a call
    if record is None:
        return {**base, "signal": None, "reason": "insufficient validation", "backtest": None}
    report = record["report"]
    if model is None or not report.get("passed"):
        return {**base, "signal": None, "reason": "insufficient validation",
                "backtest": _backtest_summary(report)}
    if record.get("dataPolicy") != FEATURE_FRAME_POLICY:
        return {**base, "signal": None,
                "reason": "model was trained without the completed-bars data policy; retrain it",
                "backtest": _backtest_summary(report)}

    # compute the CURRENT feature row and score it
    try:
        df, source, is_syn = fetch_feature_frame(ticker, tf, PREDICT_LOOKBACK)
    except Exception:  # noqa: BLE001 — analysis down; we still return the (validated) track record
        return {**base, "signal": None, "reason": "current data unavailable",
                "backtest": _backtest_summary(report)}
    base["currentData"] = {"source": source, "synthetic": bool(is_syn)}
    ctx = fetch_context(tf, PREDICT_LOOKBACK)
    earnings = load_earnings(ticker, EARNINGS_CACHE_DIR, ALPHAVANTAGE_API_KEY)
    committee = load_committee_history(ticker)
    row, as_of = latest_feature_row(df, ctx=ctx, earnings=earnings, committee=committee)
    if row is None:
        return {**base, "signal": None, "reason": "insufficient current data",
                "backtest": _backtest_summary(report)}

    # Score with the EXACT feature set the model was trained on (feature-set versioning): the
    # record pins it; older pickles fall back to LightGBM's own feature_name_, then MODEL_FEATURES.
    cols = record.get("features") or list(getattr(model, "feature_name_", []) or []) or MODEL_FEATURES
    prob_raw = predict_prob(model, row, cols)
    thr = report.get("thresholds", {"upper": 0.55, "lower": 0.45})
    allow_short = report.get("allowShort", False)
    # DIRECTION stays on the RAW probability — that is what the walk-forward gate validated.
    # The DISPLAYED probability (and the confidence built on it) is the calibrated one: what the
    # number should honestly mean. See calibration.py.
    direction = derive_direction(prob_raw, thr["upper"], thr["lower"], allow_short)
    prob_up = float(apply_calibration(cal, prob_raw))
    breakdown = confidence_breakdown(prob_up, report)
    signal = {
        "direction": direction,
        "probUp": round(prob_up, 4),
        "probUpRaw": round(prob_raw, 4),
        "calibrated": cal is not None,
        "confidence": breakdown["confidence"],
        "confidenceBreakdown": breakdown,
        "threshold": thr,
    }
    return {**base, "asOf": str(as_of), "signal": signal, "backtest": _backtest_summary(report)}


@app.get("/backtest/{ticker}")
def backtest(ticker: str, timeframe: str = "1D", horizon: int = 5):
    ticker, tf = ticker.upper(), _tf(timeframe)
    record = load_record(ticker, tf, horizon)
    if record is None:
        raise HTTPException(status_code=404, detail="no trained model for this ticker/timeframe/horizon")
    # Same `evaluation` block /predict serves: a passing report and a pooled EDGE verdict are two
    # different claims, and the record alone only carries the first (contract §4.2).
    return {**record, "evaluation": evaluation_block(
        ticker, tf, horizon, record.get("report"), data_policy=record.get("dataPolicy")
    )}


# --------------------------------------------------------------------------- evaluation runner
#
# `POST /evaluate/run` + `GET /evaluate/status` make the offline edge-evaluation harness reachable
# from the application, because production is the single-container deploy where the operator has NO
# shell and NO docker access — so `python -m app.evaluate` could not be run there at all, and
# docs/VALIDATION_AND_GO_LIVE.md §2.3 was an instruction nobody could follow.
#
# It adds a way to RUN the harness and no way to influence what it CONCLUDES. The verdict logic, the
# gates and the strategy-version machinery are untouched: `evaluate.py` decides and `verdicts.py`
# persists, exactly as from the CLI. See app/evalrun.py for the lock, the exit-code meanings and the
# no-parameters rule.
#
# MANUAL ONLY. Nothing schedules this and nothing may. The harness is also CPU-heavy — `/predict`
# latency degrades measurably while a run is in flight — which is an accepted trade on this
# single-box deployment, and another reason a run is an operator's deliberate act rather than
# anything a page load can cause.


@app.post("/evaluate/run")
async def evaluate_run(request: Request):
    """Start ONE evaluation as a subprocess. 202 immediately — the harness takes minutes and must
    never run inside the request.

    THE ENDPOINT ACCEPTS NO PARAMETERS, DELIBERATELY. The run uses only this deployment's own
    `EVAL_*` environment, so a verdict minted from the app is always stamped with the deployment's
    strategy version and can never be a custom-parameter run masquerading as the default strategy
    (docs/PAPER_EXECUTION_CONTRACT.md §4.3). A query string or body fields are refused with 400
    naming that rule, rather than ignored — a caller who believes they asked for something the run
    did not do would attribute the resulting verdict to a strategy nobody ran.
    """
    try:
        evalrun.assert_no_parameters(request.url.query, await request.body())
    except evalrun.ParametersRejected as e:
        raise HTTPException(status_code=400, detail=str(e)) from e
    try:
        started = evalrun.start()
    except evalrun.RunInFlight as e:
        # 409, carrying the running job's status: "one is already going" is only actionable next to
        # what that one is doing.
        return JSONResponse(status_code=409, content={
            "started": False,
            "error": "an evaluation run is already in flight",
            "status": e.status,
        })
    except OSError as e:
        raise HTTPException(status_code=500, detail=f"could not start the evaluator: {e}") from e
    return JSONResponse(status_code=202, content=started)


@app.get("/evaluate/status")
def evaluate_status():
    """Run state, the last exit code AND what it meant, a log tail, the newest report's verdict, and
    every stored verdict with the `current` flag paper gate 4 actually spends."""
    return evalrun.status()


@app.post("/evaluate-events/run")
async def evaluate_events_run(request: Request):
    """Start the separate PEAD audit. No request parameters and no shared price-evaluator lock."""
    try:
        evalrun.assert_no_parameters(request.url.query, await request.body(), kind="event")
    except evalrun.ParametersRejected as e:
        raise HTTPException(status_code=400, detail=str(e)) from e
    try:
        started = evalrun.start_event()
    except evalrun.RunInFlight as e:
        return JSONResponse(status_code=409, content={
            "started": False,
            "error": "a PEAD evaluation run is already in flight",
            "status": e.status,
        })
    except OSError as e:
        raise HTTPException(status_code=500, detail=f"could not start the PEAD evaluator: {e}") from e
    return JSONResponse(status_code=202, content=started)


@app.get("/evaluate-events/status")
def evaluate_events_status():
    """PEAD run state and newest event report; never serves price verdict records."""
    return evalrun.status_event()


@app.post("/estimate-snapshots/run")
async def estimate_snapshots_run(request: Request):
    """Start one bounded forward-estimate collection pass using deployment configuration only."""
    try:
        evalrun.assert_no_parameters(
            request.url.query, await request.body(), kind="estimate"
        )
    except evalrun.ParametersRejected as e:
        raise HTTPException(status_code=400, detail=str(e)) from e
    try:
        started = evalrun.start_estimates()
    except evalrun.RunInFlight as e:
        return JSONResponse(status_code=409, content={
            "started": False,
            "error": "an estimate snapshot pass is already in flight",
            "status": e.status,
        })
    except OSError as e:
        raise HTTPException(status_code=500, detail=f"could not start estimate collection: {e}") from e
    return JSONResponse(status_code=202, content=started)


@app.get("/estimate-snapshots/status")
def estimate_snapshots_status():
    """Latest bounded collector state, result counts and log; never starts provider work."""
    return evalrun.status_estimates()


@app.get("/forecast/{ticker}")
def forecast(ticker: str, timeframe: str = "1D", steps: int = 10):
    """OPTIONAL expected-path overlay (a prior, never a signal). Disabled unless ENABLE_CHRONOS=true.
    Chronos-Bolt-small would slot in here, run sequentially with qwen (never both loaded). Until
    then this is a naive drift extrapolation, explicitly labelled as a prior, not a trading call."""
    if not ENABLE_CHRONOS:
        return {"enabled": False,
                "note": "Optional expected-path overlay disabled (set ENABLE_CHRONOS=true). "
                        "It is a prior for a chart, never a buy/sell signal."}
    ticker, tf = ticker.upper(), _tf(timeframe)
    try:
        df, _source, is_syn = fetch_feature_frame(ticker, tf, PREDICT_LOOKBACK)
    except Exception as e:  # noqa: BLE001
        raise HTTPException(status_code=502, detail=f"feature fetch failed: {e}") from e
    close = df["close"].dropna()
    last = float(close.iloc[-1])
    drift = float(close.pct_change().tail(60).mean() or 0.0)
    vol = float(close.pct_change().tail(60).std() or 0.0)
    path = []
    for h in range(1, steps + 1):
        mid = last * (1 + drift) ** h
        band = mid * vol * (h ** 0.5)
        path.append({"h": h, "mid": round(mid, 2), "lo": round(mid - band, 2), "hi": round(mid + band, 2)})
    return {
        "enabled": True, "ticker": ticker, "timeframe": tf,
        "model": "naive-drift (Chronos-Bolt placeholder)",
        "path": path, "trainedOnSynthetic": is_syn,
        "note": "Expected-path PRIOR with an uncertainty band — one input among many, NOT a signal.",
    }
