"""Tests for the offline edge-evaluation harness (app/evaluate.py).

The pure-math, permutation, synthetic-refusal and no-leakage tests run WITHOUT LightGBM or a network
(the harness's honesty guarantees must be testable offline). The one end-to-end test that exercises
the model is guarded by importorskip.

Run:  cd services/prediction && python -m pytest -q tests/test_evaluate.py
"""
import numpy as np
import pandas as pd
import pytest

from app import evaluate
from app.evaluate import (
    EvalConfig,
    Stream,
    decide_verdict,
    evaluate_ticker,
    make_stream,
    permutation_test,
    pool,
    portfolio_returns,
)
from app.features import build_dataset


# --------------------------------------------------------------------------- fixtures

def _cfg(**over) -> EvalConfig:
    base = dict(
        universe=["TEST"], timeframe="1D", horizons=[5], history_days=3650,
        holdout_frac=0.2, cost_bps=6.0, permutations=50, min_trades=100, seed=42,
        upper=0.55, lower=0.45, allow_short=False, analysis_url="http://analysis",
        out_dir="/tmp/eval-test",
    )
    base.update(over)
    return EvalConfig(**base)


def _enriched(n: int = 400, seed: int = 3) -> pd.DataFrame:
    """A stand-in enriched frame with every raw column build_features consumes (mirrors the leakage
    tests). Values are arbitrary but finite."""
    rng = np.random.default_rng(seed)
    close = np.abs(100 + np.cumsum(rng.normal(0, 1, n))) + 10
    idx = pd.bdate_range(end="2026-07-01", periods=n)
    return pd.DataFrame(
        {
            "close": close,
            "volume": rng.integers(1_000_000, 5_000_000, n).astype(float),
            "rsi": rng.uniform(20, 80, n),
            "macd_hist": rng.normal(0, 1, n),
            "stoch_k": rng.uniform(0, 100, n),
            "stoch_d": rng.uniform(0, 100, n),
            "adx": rng.uniform(10, 40, n),
            "atr": rng.uniform(1, 5, n),
            "sma50": close * rng.uniform(0.97, 1.03, n),
            "sma200": close * rng.uniform(0.95, 1.05, n),
            "ema20": close * rng.uniform(0.98, 1.02, n),
            "bb_upper": close * 1.05,
            "bb_lower": close * 0.95,
            "vol_sma20": rng.integers(1_000_000, 5_000_000, n).astype(float),
        },
        index=idx,
    )


# --------------------------------------------------------------------------- 1. metric math
#
# `pool` reports two kinds of number and they must not be confused: COUNTING stats (trades, hit rate,
# active bars) are per-decision and concatenate honestly; STATISTICAL stats (expectancy, Sharpe,
# drawdown, equity, total return) come from a DATE-ALIGNED equal-weight 1/N portfolio. With ONE
# stream the two coincide, which is why the single-stream expectations below are unchanged.

def test_pool_metric_math_known_series():
    # one stream, no costs: net == ret. positions all long, ret alternates +0.10 / -0.05.
    s = make_stream([1.0, 1.0, 1.0, 1.0], [0.10, -0.05, 0.10, -0.05])
    m = pool([s], cost_bps=0.0, timeframe="1D")

    assert m["expectancy"] == pytest.approx(0.025, abs=1e-9)          # mean net per portfolio day
    assert m["avgReturnPct"] == pytest.approx(2.5, abs=1e-6)          # mean net per ACTIVE bar, %
    assert m["hitRate"] == 0.5                                        # long into 2 up, 2 down bars
    assert m["profitFactor"] == pytest.approx(2.0, abs=1e-9)          # 0.20 gains / 0.10 losses
    assert m["nTrades"] == 1                                          # one entry, then held
    assert m["nBars"] == 4 and m["nActiveBars"] == 4
    assert m["nDates"] == 4                                           # one stream: dates == bars
    assert m["nStreams"] == 1
    assert m["sharpe"] > 0                                            # positive expectancy -> +Sharpe
    assert m["maxDrawdown"] == pytest.approx(0.05, abs=1e-4)          # single -5% bar off the peak
    assert m["method"] == "portfolio-v4"


def test_pool_costs_and_pooling_across_streams():
    # two streams pooled; costs bite on the entry turnover of each.
    pos = [1.0, 1.0]
    streams = [make_stream(pos, [0.02, 0.02]), make_stream(pos, [0.03, 0.03])]
    free = pool(streams, cost_bps=0.0, timeframe="1D")
    costed = pool(streams, cost_bps=10.0, timeframe="1D")
    assert free["nTrades"] == 2                                       # one entry per stream
    assert free["nBars"] == 4                                         # DECISIONS across both streams
    assert free["nDates"] == 2                                        # ...but only 2 portfolio days
    assert free["expectancy"] == pytest.approx(0.025, abs=1e-9)       # 1/N mean of 0.02 and 0.03
    assert costed["expectancy"] < free["expectancy"]                 # costs strictly reduce edge
    assert costed["profitFactor"] is None or costed["profitFactor"] > 0
    # no losing days -> profit factor undefined (None), not a divide-by-zero
    assert free["profitFactor"] is None


def test_pool_sharpe_sign_follows_expectancy():
    # returns need variance for Sharpe to be defined (a zero-variance series has Sharpe 0 by design,
    # matching the gated backtest). Positive-mean -> +Sharpe; negative-mean -> -Sharpe.
    up_ret = np.tile([0.02, 0.00], 30)   # mean +0.01, nonzero std
    dn_ret = np.tile([-0.02, 0.00], 30)  # mean -0.01, nonzero std
    up = pool([make_stream(np.ones(60), up_ret)], cost_bps=0.0, timeframe="1D")
    dn = pool([make_stream(np.ones(60), dn_ret)], cost_bps=0.0, timeframe="1D")
    assert up["sharpe"] > 0 and up["expectancy"] > 0
    assert dn["sharpe"] < 0 and dn["expectancy"] < 0


# --------------------------------------------------------------------------- 1b. the pooling defect
#
# THE BUG THIS REPLACED. `pool` used to CONCATENATE every stream end to end and compute Sharpe,
# drawdown, equity and the permutation null on the splice. Two perfectly correlated streams — the
# same ticker's returns arriving twice, which is exactly what two horizons of one name are — then
# doubled the apparent sample and shrank the null, so `EDGE` could be granted on evidence that did
# not exist. A date-aligned portfolio is immune: duplicating a stream changes nothing at all.

def test_duplicating_a_correlated_stream_does_not_change_the_portfolio():
    rng = np.random.default_rng(11)
    ret = rng.normal(0.001, 0.01, 200)
    pos = rng.choice([0.0, 1.0], size=200)
    dates = np.arange(200)

    one = pool([Stream(pos, ret, dates, "a")], cost_bps=6.0, timeframe="1D")
    twin = pool([Stream(pos, ret, dates, "a"), Stream(pos, ret, dates, "a-duplicate")],
                cost_bps=6.0, timeframe="1D")

    # The statistical metrics are IDENTICAL: the same day held twice is still one day's return.
    assert twin["expectancy"] == pytest.approx(one["expectancy"], abs=1e-12)
    assert twin["sharpe"] == pytest.approx(one["sharpe"], abs=1e-9)
    assert twin["maxDrawdown"] == pytest.approx(one["maxDrawdown"], abs=1e-9)
    assert twin["totalReturn"] == pytest.approx(one["totalReturn"], abs=1e-9)
    # The effective sample does NOT double...
    assert twin["nDates"] == one["nDates"] == 200
    # ...while the honest per-decision counts do, because two decisions really were taken.
    assert twin["nTrades"] == 2 * one["nTrades"]
    assert twin["nBars"] == 2 * one["nBars"]


def test_a_stream_with_no_position_on_a_date_drags_the_portfolio():
    """Idle capital is a real cost of a 1/N allocation: half the book flat halves the return."""
    dates = np.arange(3)
    active = Stream(np.ones(3), np.full(3, 0.02), dates, "active")
    idle = Stream(np.zeros(3), np.full(3, 0.02), dates, "idle")
    _d, port = portfolio_returns([active, idle], cost_bps=0.0)
    assert port == pytest.approx([0.01, 0.01, 0.01], abs=1e-12)


def test_streams_on_disjoint_dates_are_not_concatenated():
    """Two streams that never trade on the same day still make ONE portfolio with one date axis —
    each contributes 1/N on its own days and 0 on the other's."""
    a = Stream(np.ones(2), np.full(2, 0.02), np.array([0, 1]), "a")
    b = Stream(np.ones(2), np.full(2, 0.04), np.array([2, 3]), "b")
    dates, port = portfolio_returns([a, b], cost_bps=0.0)
    assert list(dates) == [0, 1, 2, 3]
    assert port == pytest.approx([0.01, 0.01, 0.02, 0.02], abs=1e-12)


def test_pool_reports_the_method_it_used():
    assert pool([], cost_bps=0.0, timeframe="1D")["method"] == "portfolio-v4"


# --------------------------------------------------------------------------- 2. permutation p-value

def test_permutation_detects_real_edge():
    """Positions perfectly aligned with the sign of returns => a real edge => permutations that
    scramble the timing almost never match it => tiny p-value."""
    rng = np.random.default_rng(0)
    direction = rng.choice([1.0, -1.0], size=400)                    # the (known) daily direction
    ret = direction * 0.01 + rng.normal(0, 0.005, 400)              # mostly follows direction, with noise
    pos = direction                                                  # a model with real directional timing
    out = permutation_test([make_stream(pos, ret)], cost_bps=0.0, timeframe="1D", n_perms=300, seed=1)
    assert out["expectancy"]["pValue"] < 0.05
    assert out["sharpe"]["pValue"] < 0.05                            # net has variance -> Sharpe is meaningful
    assert out["expectancy"]["real"] > out["expectancy"]["nullMean"]


def test_permutation_on_pure_noise_is_not_significant():
    """Positions independent of returns => no edge => the real metric is just one draw from the null
    => p is NOT significant (and lands mid-distribution, not near 0)."""
    rng = np.random.default_rng(7)
    ret = rng.normal(0, 0.01, 600)
    pos = rng.choice([0.0, 1.0], size=600)                          # unrelated to ret
    out = permutation_test([make_stream(pos, ret)], cost_bps=0.0, timeframe="1D", n_perms=400, seed=3)
    assert out["expectancy"]["pValue"] > 0.05                        # no false edge
    assert 0.1 < out["expectancy"]["pValue"] < 0.9                   # mid-distribution (~0.5)


def test_permutation_null_preserves_position_run_lengths():
    """A circular shift keeps every run intact, so the null pays the SAME turnover cost as the real
    series. An i.i.d. shuffle does not, and would compare a cheap null against a costed real."""
    pos = np.array([1.0] * 10 + [0.0] * 10 + [-1.0] * 10)
    for offset in range(1, len(pos)):
        rolled = np.roll(pos, offset)
        assert int(np.abs(np.diff(rolled)).sum() > 0)
        assert sorted(np.unique(rolled, return_counts=True)[1].tolist()) == [10, 10, 10]


def test_permutation_is_seeded_and_reproducible():
    rng = np.random.default_rng(5)
    ret = rng.normal(0, 0.01, 200)
    pos = rng.choice([0.0, 1.0], size=200)
    kwargs = dict(cost_bps=6.0, timeframe="1D", n_perms=50, seed=99)
    a = permutation_test([make_stream(pos, ret)], **kwargs)
    b = permutation_test([make_stream(pos, ret)], **kwargs)
    assert a == b


# --------------------------------------------------------------------------- 3. synthetic refusal

def test_refuses_verdict_on_synthetic(monkeypatch, capsys):
    monkeypatch.setattr(evaluate, "fetch_feature_frame",
                        lambda *a, **k: (pd.DataFrame(), "synthetic", True))
    report, code = evaluate.run(_cfg(), write=False)
    assert code == evaluate.EXIT_SYNTHETIC
    assert code != 0
    assert report["verdict"] is None
    assert report["refused"] == "synthetic"
    assert "SYNTHETIC DATA" in capsys.readouterr().out


def test_main_exits_nonzero_on_synthetic(monkeypatch):
    monkeypatch.setattr(evaluate.EvalConfig, "from_env", classmethod(lambda cls: _cfg()))
    monkeypatch.setattr(evaluate, "fetch_feature_frame",
                        lambda *a, **k: (pd.DataFrame(), "synthetic", True))
    assert evaluate.main() == evaluate.EXIT_SYNTHETIC


def test_refuses_when_nothing_fetched(monkeypatch, capsys):
    def _boom(*a, **k):
        raise RuntimeError("analysis unreachable")
    monkeypatch.setattr(evaluate, "fetch_feature_frame", _boom)
    report, code = evaluate.run(_cfg(), write=False)
    assert code == evaluate.EXIT_NO_DATA and code != 0
    assert report["verdict"] is None
    assert "NO DATA" in capsys.readouterr().out


# --------------------------------------------------------------------------- 4. no look-ahead

def test_temporal_holdout_has_no_leakage():
    """The guarantee evaluate_ticker relies on: the split is strictly chronological, so no fold can
    train on the holdout, and features are already lagged by build_dataset."""
    df = _enriched(400)
    ds = build_dataset(df, horizon=5)
    n = len(ds.X)
    holdout = max(int(n * 0.2), 30)
    wf_end = n - holdout
    assert ds.index[:wf_end].max() < ds.index[wf_end:].min()         # train strictly precedes holdout
    assert list(ds.index) == sorted(ds.index)                        # chronological, so "before" is in time


def test_evaluate_ticker_produces_oos_streams():
    pytest.importorskip("lightgbm")
    df = _enriched(600)
    te = evaluate_ticker(df, "TEST", "yfinance", 5, _cfg())
    assert te is not None
    for stream in (te.folds, te.holdout):
        assert len(stream.positions) == len(stream.ret_next) == len(stream.dates) > 0
        assert set(np.unique(stream.positions)).issubset({-1.0, 0.0, 1.0})
    # The DATES are the point: a stream that does not know when its bars happened cannot be aligned
    # with any other, and the portfolio math would silently fall back to splicing.
    assert te.folds.dates.max() < te.holdout.dates.min()             # strictly consecutive spans
    combined = te.combined()
    assert len(combined.positions) == len(te.folds.positions) + len(te.holdout.positions)
    assert list(combined.dates) == sorted(combined.dates)


# --------------------------------------------------------------------------- verdict logic

def _perm(p_exp, p_sharpe):
    return {"expectancy": {"pValue": p_exp, "real": 0.0, "nullMean": 0.0, "null95": 0.0},
            "sharpe": {"pValue": p_sharpe, "real": 0.0, "nullMean": 0.0, "null95": 0.0},
            "permutations": 300}


def _suff(**over):
    """A sample large enough to judge on. `decide_verdict` REQUIRES this block — a verdict that does
    not state how much evidence it rests on is the thing the sufficiency gates exist to stop — so
    every fixture states it explicitly and the failing cases override one field at a time."""
    base = {
        "nDates": 900, "minDates": 252,
        "holdoutDates": 180, "minHoldoutDates": 60,
        "nStreams": 30, "minTickers": 10,
        "configuredTickers": 30, "coverage": 1.0, "minCoverage": 0.7,
        "evaluatedTickers": [], "failedTickers": [], "skippedTickers": [],
    }
    base.update(over)
    return base


def test_verdict_edge():
    all_pool = {"nTrades": 200, "sharpe": 1.2}
    holdout = {"expectancy": 0.0012, "sharpe": 1.0}
    bh = {"sharpe": 0.4}
    v, p, checks = decide_verdict(all_pool, holdout, bh, _perm(0.01, 0.02), min_trades=100,
                                  suff=_suff())
    assert v == "EDGE"
    assert all(c.startswith("PASS") for c in checks)


def test_verdict_inconclusive_on_too_few_trades():
    v, _, _ = decide_verdict({"nTrades": 40, "sharpe": 1.2}, {"expectancy": 0.001, "sharpe": 1.0},
                             {"sharpe": 0.4}, _perm(0.01, 0.01), min_trades=100, suff=_suff())
    assert v == "INCONCLUSIVE"


def test_verdict_suspect_on_high_sharpe():
    v, _, _ = decide_verdict({"nTrades": 200, "sharpe": 4.5}, {"expectancy": 0.01, "sharpe": 3.9},
                             {"sharpe": 0.4}, _perm(0.001, 0.001), min_trades=100, suff=_suff())
    assert v == "SUSPECT"


def test_verdict_no_edge_on_negative_holdout_or_weak_pvalue():
    # positive folds but the untouched holdout is negative -> NO EDGE
    v1, _, _ = decide_verdict({"nTrades": 200, "sharpe": 1.2}, {"expectancy": -0.0003, "sharpe": -0.2},
                              {"sharpe": 0.4}, _perm(0.01, 0.01), min_trades=100, suff=_suff())
    assert v1 == "NO EDGE"
    # holdout positive but permutation says it's indistinguishable from random timing -> NO EDGE
    v2, _, _ = decide_verdict({"nTrades": 200, "sharpe": 1.2}, {"expectancy": 0.001, "sharpe": 1.0},
                              {"sharpe": 0.4}, _perm(0.30, 0.28), min_trades=100, suff=_suff())
    assert v2 == "NO EDGE"


# --- sample sufficiency (added with `portfolio-v3`) ---------------------------------------------
#
# These can only ever turn a verdict INTO `INCONCLUSIVE`. Each case below is a run whose STATISTICS
# are otherwise perfect — the same numbers that produce EDGE in test_verdict_edge — and the only
# thing that changes is how much evidence they rest on.

_EDGE_POOLS = ({"nTrades": 200, "sharpe": 1.2}, {"expectancy": 0.0012, "sharpe": 1.0}, {"sharpe": 0.4})


def _judge(suff):
    a, h, bh = _EDGE_POOLS
    return decide_verdict(a, h, bh, _perm(0.01, 0.02), min_trades=100, suff=suff)


def test_verdict_edge_requires_enough_portfolio_dates():
    v, _, checks = _judge(_suff(nDates=180))
    assert v == "INCONCLUSIVE"
    assert any("180 portfolio observation dates" in c and "252" in c for c in checks)


def test_verdict_edge_requires_a_long_enough_holdout():
    v, _, checks = _judge(_suff(holdoutDates=40))
    assert v == "INCONCLUSIVE"
    assert any("holdout is only 40 dates" in c for c in checks)


def test_verdict_edge_requires_a_minimum_cohort():
    v, _, checks = _judge(_suff(nStreams=4, configuredTickers=4, coverage=1.0))
    assert v == "INCONCLUSIVE"
    assert any("only 4 evaluated stream(s)" in c for c in checks)


def test_verdict_inconclusive_on_thin_coverage_and_names_the_missing_tickers():
    # 12 of 30 configured names survived: over the stream floor, under the coverage floor. The
    # tickers that did not make it are NAMED, so a verdict from a shrunken cohort says so.
    v, _, checks = _judge(_suff(
        nStreams=12, configuredTickers=30, coverage=0.4,
        failedTickers=["AMD", "INTC"], skippedTickers=["RIVN"],
    ))
    assert v == "INCONCLUSIVE"
    line = next(c for c in checks if "coverage" in c)
    assert "12/30" in line
    for ticker in ("AMD", "INTC", "RIVN"):
        assert ticker in line, f"{ticker} must be named in the coverage line: {line}"


def test_suspect_is_not_softened_to_inconclusive_by_a_small_sample():
    # SUSPECT is the WORST verdict. A leaky-looking run that is ALSO too small must still read
    # SUSPECT — the sufficiency gates may never make a headline less alarming than it was.
    v, _, _ = decide_verdict({"nTrades": 200, "sharpe": 4.5}, {"expectancy": 0.01, "sharpe": 3.9},
                             {"sharpe": 0.4}, _perm(0.001, 0.001), min_trades=100,
                             suff=_suff(nDates=30, nStreams=2, coverage=0.1))
    assert v == "SUSPECT"


def test_a_sufficient_edge_records_the_sample_it_stands_on():
    v, _, checks = _judge(_suff())
    assert v == "EDGE"
    assert all(c.startswith("PASS") for c in checks)
    joined = " ".join(checks)
    assert "portfolio dates >= 252" in joined
    assert "holdout dates >= 60" in joined
    assert "evaluated streams >= 10" in joined
    assert "ticker coverage >= 70%" in joined


def test_sufficiency_counts_failed_and_skipped_against_coverage():
    from app.evaluate import EvalConfig, sufficiency

    cfg = EvalConfig(
        universe=["A", "B", "C", "D", "E"], timeframe="1D", horizons=[5], history_days=3650,
        holdout_frac=0.2, cost_bps=6.0, permutations=10, min_trades=100, seed=42,
        upper=0.55, lower=0.45, allow_short=False, analysis_url="", out_dir="",
    )
    suff = sufficiency({"nDates": 900}, {"nDates": 180}, ["A", "B"], cfg,
                       failed=["C"], skipped=["D", "E"])
    assert suff["nStreams"] == 2 and suff["configuredTickers"] == 5
    assert suff["coverage"] == 0.4
    assert suff["failedTickers"] == ["C"] and suff["skippedTickers"] == ["D", "E"]
    # The defaults are the documented ones, and they travel on the block itself.
    assert suff["minDates"] == 252 and suff["minHoldoutDates"] == 60
    assert suff["minTickers"] == 10 and suff["minCoverage"] == 0.7


def test_environment_cannot_weaken_live_evidence_floors(monkeypatch):
    from app.evaluate import EvalConfig

    monkeypatch.setenv("EVAL_MIN_DATES", "0")
    monkeypatch.setenv("EVAL_MIN_HOLDOUT_DATES", "0")
    monkeypatch.setenv("EVAL_MIN_TICKERS", "0")
    monkeypatch.setenv("EVAL_MIN_COVERAGE", "0")
    with pytest.raises(ValueError, match="cannot be weakened"):
        EvalConfig.from_env()


def test_verdict_no_edge_when_worse_than_buy_and_hold():
    v, _, checks = decide_verdict({"nTrades": 200, "sharpe": 0.6}, {"expectancy": 0.0005, "sharpe": 0.5},
                                  {"sharpe": 1.5}, _perm(0.01, 0.01), min_trades=100, suff=_suff())
    assert v == "NO EDGE"
    assert any("FAIL" in c and "buy-and-hold" in c for c in checks)


# --------------------------------------------------------------------------- 5. per-horizon verdicts
#
# Two horizons of one ticker are two views of the SAME underlying returns. Pooling them into one
# number counts every market move twice, so each horizon is judged as its own portfolio and gets its
# own verdict; the headline is the most conservative of them, and any combined-horizons number is
# labelled OVERLAPPING and read by no check.

def _fake_eval(ticker: str, horizon: int, seed: int):
    """A TickerEval with real dates and no model — the orchestration is what is under test."""
    from app.evaluate import Stream, TickerEval

    rng = np.random.default_rng(seed)
    dates = np.array(pd.bdate_range(end="2026-07-01", periods=120).to_numpy())
    ret = rng.normal(0.0005, 0.01, 120)
    pos = rng.choice([0.0, 1.0], size=120)
    return TickerEval(
        ticker=ticker, horizon=horizon, source="yfinance",
        folds=Stream(pos[:90], ret[:90], dates[:90], f"{ticker} folds"),
        holdout=Stream(pos[90:], ret[90:], dates[90:], f"{ticker} holdout"),
        n_rows=120, n_folds=3,
    )


def test_run_judges_each_horizon_separately(monkeypatch, capsys):
    monkeypatch.setattr(evaluate, "fetch_feature_frame",
                        lambda t, *a, **k: (pd.DataFrame(), "yfinance", False))
    monkeypatch.setattr(evaluate, "fetch_context", lambda *a, **k: None)
    monkeypatch.setattr(evaluate, "load_earnings", lambda *a, **k: None)
    monkeypatch.setattr(
        evaluate, "evaluate_ticker",
        lambda df, t, src, h, cfg, **k: _fake_eval(t, h, seed=hash((t, h)) % 1000),
    )

    cfg = _cfg(universe=["AAA", "BBB"], horizons=[5, 10], permutations=20, min_trades=5)
    report, code = evaluate.run(cfg, write=False)
    assert code == evaluate.EXIT_OK

    assert set(report["byHorizon"]) == {"5", "10"}
    for block in report["byHorizon"].values():
        assert block["pooled"]["all"]["nStreams"] == 2        # one stream per ticker, per horizon
        assert block["verdict"] in evaluate.VERDICT_MEANING
        assert block["checklist"]
        # Every judged horizon states the SAMPLE it was judged on, so the report and the verdict
        # record it feeds can both be audited later without re-running anything.
        suff = block["sufficiency"]
        assert suff["configuredTickers"] == 2 and suff["nStreams"] == 2
        assert suff["nDates"] > 0 and suff["minDates"] == 252
        # Two of two configured names: full coverage, but far under the ten-stream floor, so this
        # cohort cannot mint EDGE however good its statistics look.
        assert suff["coverage"] == 1.0
        assert block["verdict"] != "EDGE"
    # The headline may never read more permissive than any horizon it summarises.
    assert report["verdict"] == evaluate._headline_verdict(
        [b["verdict"] for b in report["byHorizon"].values()])
    # The floors this run judged against travel on the report's own config block.
    assert report["config"]["minDates"] == 252
    assert report["config"]["minHoldoutDates"] == 60
    assert report["config"]["minTickers"] == 10
    assert report["config"]["minCoverage"] == 0.7
    # The combined-horizons number exists but is labelled and is not a verdict.
    assert "OVERLAPPING" in report["overlapping"]["warning"]
    assert "verdict" not in report["overlapping"]
    # Method and the run's OWN strategy identity travel on the report.
    assert report["method"] == "portfolio-v4"
    assert report["config"]["strategyVersion"] == evaluate.run_strategy_version(cfg)
    # It renders without exploding, and prints the per-horizon breakdown.
    assert "Horizon 5" in evaluate._render_md(report)
    assert "horizon 10" in capsys.readouterr().out


def test_headline_verdict_is_the_most_conservative():
    assert evaluate._headline_verdict(["EDGE", "NO EDGE"]) == "NO EDGE"
    assert evaluate._headline_verdict(["EDGE", "INCONCLUSIVE"]) == "INCONCLUSIVE"
    assert evaluate._headline_verdict(["NO EDGE", "SUSPECT"]) == "SUSPECT"
    assert evaluate._headline_verdict(["EDGE", "EDGE"]) == "EDGE"
    assert evaluate._headline_verdict([]) == "INCONCLUSIVE"
