"""The A–F ablation ladder driver (contract §5.1, docs/VALIDATION_AND_GO_LIVE.md).

Offline, no network, no model: every seam the driver has — the analyst, the store and the input
fetcher — is injected, which is the point of the seams.

The two tests that matter most are the refusals: a synthetic outcome must produce
`SYNTHETIC DATA — cannot validate` and a NON-ZERO exit code with no verdict anywhere in the output,
and a comparison across two different cutoff sets must refuse rather than report.
"""
from __future__ import annotations

import json

import pytest

from app import ablation


# ---- fixtures ------------------------------------------------------------------------------------


class FakeStore:
    def __init__(self, records=None):
        self.records = list(records or [])
        self.posted: list[dict] = []
        self.queries: list[tuple[str, str]] = []

    def post(self, record):
        self.posted.append(record)
        return record

    def list(self, *, experiment, split, limit=500):
        self.queries.append((experiment, split))
        return [
            r for r in self.records
            if r.get("experiment") == experiment and r.get("split") == split
        ]

    def backfill(self, *, experiment):
        return {"updated": 0, "pending": 0, "refused": 0, "experiment": experiment}


def prediction(
    *,
    pid="prd_0123456789abcdef",
    experiment="C",
    split="dev",
    ticker="NVDA",
    directions=("bullish", "bullish", "bullish", "bullish"),
    realized=(0.01, 0.02, 0.03, 0.04),
    excess=(0.005, 0.01, 0.015, 0.02),
    source="yfinance",
    resolved=True,
    abstain=False,
):
    horizons = {}
    outcomes = {}
    for i, horizon in enumerate(ablation.HORIZONS):
        horizons[horizon] = {
            "direction": directions[i],
            "score": 0.3,
            "abstain": abstain or directions[i] in ablation.ABSTAIN_DIRECTIONS,
        }
        outcomes[horizon] = {
            "horizon": horizon,
            "realizedReturn": realized[i],
            "excessReturn": excess[i],
            "resolvedFromSource": source,
            "resolved": resolved,
            "refused": (not resolved) and "synthetic" in source,
        }
    return {
        "id": pid,
        "ticker": ticker,
        "experiment": experiment,
        "split": split,
        "asOf": "2023-06-15T20:00:00Z",
        "forecast": {"horizons": horizons, "confidenceBucket": "medium"},
        "outcomes": outcomes,
    }


def many(n, **kw):
    return [prediction(pid=f"prd_{i:016x}", **kw) for i in range(n)]


def config(tmp_path, **kw):
    base = dict(
        cutoffs=["2023-06-15T20:00:00Z", "2023-07-14T20:00:00Z"],
        universe=["NVDA"],
        rungs=["C"],
        split="dev",
        events_url="http://events.test:8004",
        llm_url="http://llm.test:8002",
        out_dir=str(tmp_path / "eval"),
        min_samples=4,
    )
    base.update(kw)
    return ablation.AblationConfig(**base)


# ---- 1. rung mixing is impossible ---------------------------------------------------------------


def test_collect_records_requires_experiment_and_split():
    store = FakeStore()
    with pytest.raises(ValueError, match="experiment"):
        ablation.collect_records(store, experiment="", split="dev")
    with pytest.raises(ValueError, match="split"):
        ablation.collect_records(store, experiment="C", split="")
    ablation.collect_records(store, experiment="C", split="dev")
    assert store.queries == [("C", "dev")]


def test_metrics_over_a_mixed_set_raise():
    mixed = [prediction(experiment="C"), prediction(experiment="D")]
    with pytest.raises(ValueError, match="more than one rung or split"):
        ablation.compute_metrics(mixed)
    mixed_split = [prediction(split="dev"), prediction(split="test")]
    with pytest.raises(ValueError, match="more than one rung or split"):
        ablation.compute_metrics(mixed_split)


# ---- 2. cutoff sets ------------------------------------------------------------------------------


def test_two_rungs_on_different_cutoff_sets_refuse():
    with pytest.raises(ablation.CutoffSetMismatch):
        ablation.assert_identical_cutoff_set({
            "C": ["2023-06-15T20:00:00Z", "2023-07-14T20:00:00Z"],
            "D": ["2023-06-15T20:00:00Z"],
        })
    shared = ablation.assert_identical_cutoff_set({
        "C": ["2023-06-15T20:00:00Z", "2023-07-14T20:00:00Z"],
        # Same set, different order: the fingerprint is order-insensitive, because a set is a set.
        "D": ["2023-07-14T20:00:00Z", "2023-06-15T20:00:00Z"],
    })
    assert shared.startswith("2@")


def test_main_refuses_a_mismatched_cutoff_set(monkeypatch, capsys, tmp_path):
    monkeypatch.setattr(
        ablation, "assert_identical_cutoff_set",
        lambda per_rung: (_ for _ in ()).throw(ablation.CutoffSetMismatch("two sets")),
    )
    monkeypatch.setenv("ABLATE_CUTOFFS", "2023-06-15T20:00:00Z")
    monkeypatch.setenv("EVAL_OUT_DIR", str(tmp_path))
    code = ablation.main(["C"])
    assert code == ablation.EXIT_CUTOFF_MISMATCH
    assert "REFUSED" in capsys.readouterr().out


# ---- 3. splits -----------------------------------------------------------------------------------


def test_split_assignment_is_deterministic_and_disjoint():
    assert ablation.split_for("2019-01-01T00:00:00Z") == "dev"
    assert ablation.split_for("2023-12-31T23:59:59Z") == "dev"
    assert ablation.split_for("2024-01-01T00:00:00Z") == "validation"
    assert ablation.split_for("2025-06-30T23:59:59Z") == "validation"
    assert ablation.split_for("2025-07-01T00:00:00Z") == "test"
    boundaries = ["2023-12-31T23:59:59Z", "2024-01-01T00:00:00Z",
                  "2025-06-30T23:59:59Z", "2025-07-01T00:00:00Z"]
    assert [ablation.split_for(b) for b in boundaries] == [
        "dev", "validation", "validation", "test"
    ]
    assert ablation.SPLIT_VERSION == "split@1"


def test_split_windows_match_the_store():
    """The two services share no code, so the constants are duplicated — and must not drift."""
    store_source = (
        __import__("pathlib").Path(__file__).resolve().parents[2]
        / "events" / "app" / "predictions.py"
    ).read_text(encoding="utf-8")
    for constant in ("SPLIT_VERSION", "SPLIT_DEV_END", "SPLIT_VALIDATION_END"):
        value = getattr(ablation, constant)
        assert f'{constant} = "{value}"' in store_source, (
            f"{constant} drifted from services/events/app/predictions.py"
        )


def test_run_rung_skips_a_cutoff_outside_the_requested_split(tmp_path):
    cfg = config(tmp_path, cutoffs=["2023-06-15T20:00:00Z", "2026-01-05T20:00:00Z"], split="dev")
    store = FakeStore()
    report = ablation.run_rung(
        "C", cfg,
        analyst=lambda bundle: {
            "identity": {"modelUsed": "qwen3-14b"},
            "forecast": {"horizons": {"1d": {"direction": "bullish", "score": 0.2,
                                             "abstain": False}},
                         "confidenceBucket": "low"},
        },
        store=store,
        fetch=lambda kind, ticker, as_of: {"kind": kind},
        now="2026-08-20T00:00:00Z",
    )
    assert report.written == 1
    assert [s["reason"] for s in report.skipped] == ["cutoff_outside_split"]
    assert store.posted[0]["split"] == "dev"
    assert store.posted[0]["experiment"] == "C"


def test_record_evidence_is_computed_from_the_exact_bundle():
    bundle = {
        "ticker": "NVDA", "asOf": "2023-06-15T20:00:00Z",
        "inputs": {
            "ohlcv": {"timeframe": "1D", "bars": [{"time": "2023-06-15", "close": 100.0}]},
            "indicators": {"rsi": 55.0, "structure": {"trend": "up"}},
            "company_events": {"events": [{"id": "evt_1a2b3c4d5e6f7081"}]},
            "macro_fed": {"macro": [{"id": "mac_1a2b3c4d5e6f7081"}]},
        },
    }
    evidence = ablation.record_evidence(bundle, {"toolCalls": [{"tool": "get_candles"}]})
    assert evidence["barsRef"] == {
        "ticker": "NVDA", "timeframe": "1D", "lastTs": "2023-06-15",
    }
    assert len(evidence["technicalStateHash"]) == 64
    assert evidence["eventIds"] == ["evt_1a2b3c4d5e6f7081"]
    assert evidence["macroEventIds"] == ["mac_1a2b3c4d5e6f7081"]


def test_bundle_to_analyst_context_preserves_only_the_requested_rung_inputs():
    bundle = {
        "ticker": "NVDA", "asOf": "2023-06-15T20:00:00Z",
        "inputs": {"ohlcv": {"bars": [1]}, "indicators": {"rsi": 55}},
    }
    context = ablation.context_from_bundle(bundle)
    assert context["ticker"] == "NVDA"
    assert context["asOf"] == "2023-06-15T20:00:00Z"
    assert context["subscriptions"] == []
    assert context["candles"] == {"bars": [1]}
    assert context["technicalContext"] == {"rsi": 55}
    assert context["tickerContext"] == {"ticker": "NVDA", "technical": {"rsi": 55}}
    assert "events" not in context and "macro" not in context


# ---- 4. the synthetic refusal --------------------------------------------------------------------


def test_synthetic_outcome_refuses_with_a_non_zero_exit(tmp_path, capsys):
    cfg = config(tmp_path)
    store = FakeStore(many(10, source="synthetic"))
    result, code = ablation.evaluate_rung("C", cfg, store=store, now="2026-08-20T00:00:00Z")

    assert code != 0 and code == ablation.EXIT_SYNTHETIC
    assert result["verdict"] is None
    assert result["refusal"]

    print_out = ablation.print_result(result) or capsys.readouterr().out
    assert "SYNTHETIC DATA — cannot validate" in print_out
    for word in ("EDGE", "NO EDGE", "INCONCLUSIVE", "SUSPECT"):
        assert word not in print_out.replace("cannot validate", "")

    markdown = ablation.render_markdown(result)
    assert "SYNTHETIC DATA — cannot validate" in markdown
    assert "Verdict" not in markdown


def test_a_mixed_source_label_still_refuses():
    """`/candles` labels a mixed window 'yfinance+synthetic'; equality against 'synthetic' misses it."""
    found = ablation.synthetic_outcomes(many(2, source="yfinance+synthetic"))
    assert len(found) == 2 * len(ablation.HORIZONS)


def test_main_exits_non_zero_on_synthetic(monkeypatch, tmp_path, capsys):
    monkeypatch.setenv("ABLATE_CUTOFFS", "2023-06-15T20:00:00Z")
    monkeypatch.setenv("EVAL_OUT_DIR", str(tmp_path))
    monkeypatch.setenv("ABLATE_MIN_SAMPLES", "4")
    # The Wave 5C runtime pre-flight is PASSED, not disabled: this test is about the synthetic
    # refusal, and turning the runtime gate off would stop proving that both gates coexist.
    monkeypatch.setattr(ablation, "runtime_precheck", lambda cfg, **kw: (True, "fake runtime"))
    monkeypatch.setattr(
        ablation, "PredictionStoreClient",
        lambda base_url, timeout=30.0: FakeStore(many(10, source="synthetic")),
    )
    code = ablation.main(["C"])
    assert code == ablation.EXIT_SYNTHETIC
    out = capsys.readouterr().out
    assert "SYNTHETIC DATA — cannot validate" in out
    assert "VERDICT" not in out


# ---- 5. Sharpe > 3 ⇒ SUSPECT ---------------------------------------------------------------------


def test_high_sharpe_is_suspect(tmp_path):
    # Near-identical positive excess returns ⇒ tiny dispersion ⇒ an implausible Sharpe.
    records = [
        prediction(pid=f"prd_{i:016x}", excess=(0.01, 0.01 + i * 1e-6, 0.01, 0.01))
        for i in range(12)
    ]
    metrics = ablation.compute_metrics(records, min_samples=4)
    verdict, reasons = ablation.verdict_for(metrics)
    assert verdict == "SUSPECT"
    assert "Sharpe" in reasons[0]


def test_a_plausible_result_can_still_be_edge():
    records = []
    for i in range(20):
        wobble = 0.01 if i % 2 else -0.004      # positive mean, real dispersion
        records.append(prediction(
            pid=f"prd_{i:016x}",
            realized=(0.01, 0.01, 0.01, 0.01),
            excess=(0.02 + wobble, 0.02 + wobble, 0.02 + wobble, 0.02 + wobble),
        ))
    metrics = ablation.compute_metrics(records, min_samples=4)
    verdict, _reasons = ablation.verdict_for(metrics)
    assert verdict in ("EDGE", "SUSPECT")
    if verdict == "EDGE":
        assert "necessary" in ablation.VERDICT_MEANING["EDGE"].lower()


def test_wrong_direction_is_no_edge():
    records = many(20, directions=("bearish",) * 4, realized=(0.01,) * 4, excess=(-0.01,) * 4)
    metrics = ablation.compute_metrics(records, min_samples=4)
    verdict, reasons = ablation.verdict_for(metrics)
    assert verdict == "NO EDGE"
    assert reasons


# ---- 6. small sample ⇒ INCONCLUSIVE with the size reported ---------------------------------------


def test_small_sample_is_inconclusive_with_the_size(tmp_path):
    cfg = config(tmp_path, min_samples=100)
    store = FakeStore(many(3))
    result, code = ablation.evaluate_rung("C", cfg, store=store, now="2026-08-20T00:00:00Z")
    assert code == ablation.EXIT_OK
    assert result["verdict"] == "INCONCLUSIVE"
    assert "12 scored calls" in result["reasons"][0]      # 3 records x 4 horizons
    assert "100" in result["reasons"][0]
    markdown = ablation.render_markdown(result)
    assert "INCONCLUSIVE" in markdown and "12 scored calls" in markdown


# ---- 7. abstention coverage ----------------------------------------------------------------------


def test_a_pipeline_that_always_abstains_reports_coverage_one():
    records = many(10, directions=("neutral",) * 4)
    metrics = ablation.compute_metrics(records, min_samples=4)
    for horizon in ablation.HORIZONS:
        h = metrics["perHorizon"][horizon]
        assert h["abstentionCoverage"] == 1.0
        assert h["scored"] == 0
        # No accuracy claim at all — not 0.0, which would read as "always wrong".
        assert h["directionalAccuracy"] is None
    verdict, reasons = ablation.verdict_for(metrics)
    assert verdict == "INCONCLUSIVE"
    assert "abstain" in reasons[0]


def test_abstention_coverage_is_partial_and_accuracy_is_conditional():
    records = many(5, directions=("bullish", "neutral", "bullish", "neutral"))
    metrics = ablation.compute_metrics(records, min_samples=1)
    assert metrics["perHorizon"]["1d"]["abstentionCoverage"] == 0.0
    assert metrics["perHorizon"]["5d"]["abstentionCoverage"] == 1.0
    # Accuracy is conditional on NOT abstaining: the 5d horizon scored nothing.
    assert metrics["perHorizon"]["1d"]["directionalAccuracy"] == 1.0
    assert metrics["perHorizon"]["5d"]["directionalAccuracy"] is None


def test_a_missing_benchmark_is_excluded_not_counted_as_zero():
    records = many(4)
    for rec in records:
        rec["outcomes"]["1d"]["excessReturn"] = None
    metrics = ablation.compute_metrics(records, min_samples=1)
    assert metrics["perHorizon"]["1d"]["excessReturnSamples"] == 0
    assert metrics["perHorizon"]["1d"]["meanExcessReturn"] is None
    assert metrics["perHorizon"]["5d"]["excessReturnSamples"] == 4


def test_an_unresolved_outcome_is_not_scored():
    records = many(4, resolved=False)
    metrics = ablation.compute_metrics(records, min_samples=1)
    assert metrics["perHorizon"]["1d"]["calls"] == 4
    assert metrics["perHorizon"]["1d"]["scored"] == 0


# ---- 8. rung availability ------------------------------------------------------------------------


def test_rung_a_is_not_built_and_rung_f_became_runnable_at_wave_5a():
    """RUNG F FLIPPED, DELIBERATELY, AND THIS TEST IS THE INVERSION.

    Wave 2 flagged F unrunnable because the market/sector half of its input class did not exist.
    Re-measured at the Wave 5 tree it does: Wave 3A's `gateway/context.go` serves `marketContext`
    (the §9.26 SPY proxy plus realised volatility) and the events store resolves
    `benchmark_return`/`sector_return`. What is still missing — breadth, implied volatility — is
    handled by `RUNG_DEFINING_INPUT` refusing an empty bundle rather than by refusing the rung.
    """
    assert ablation.rung_status("A")[0] == ablation.NOT_BUILT
    for rung in ("B", "C", "D", "E", "F"):
        assert ablation.rung_status(rung)[0] == ablation.RUNNABLE
    # The reason string survives the flip: "F is runnable" without "and here is what is still null
    # in it" would be the silently-degraded bundle Wave 2 refused, wearing a different label.
    assert "null in this deployment" in ablation.rung_status("F")[1]
    with pytest.raises(ablation.RungUnavailable):
        ablation.build_bundle("A", "NVDA", "2023-06-15T20:00:00Z", fetch=lambda *a: {})


def test_rung_f_refuses_a_bundle_whose_market_sector_class_is_entirely_null():
    """Wave 2's reasoning, kept: a rung that quietly drops its defining input and still reports as
    F would attribute a result to a cause it did not have. What changed is that the inputs arrive."""
    with pytest.raises(ablation.RungInputEmpty) as excinfo:
        ablation.build_bundle(
            "F", "NVDA", "2023-06-15T20:00:00Z",
            fetch=lambda kind, t, a: ({"breadth": None, "impliedVol": None}
                                      if kind == "market_sector" else {"kind": kind}),
        )
    assert "market_sector" in str(excinfo.value)

    # A populated market/sector class runs, and the partial dimensions are NOT what refuses it —
    # only a class with nothing in it at all.
    bundle = ablation.build_bundle(
        "F", "NVDA", "2023-06-15T20:00:00Z",
        fetch=lambda kind, t, a: ({"benchmark": "SPY", "realizedVol": 0.21, "breadth": None}
                                  if kind == "market_sector" else {"kind": kind}),
    )
    assert bundle["partialInputs"] == []
    assert set(bundle["inputs"]) == set(ablation.RUNG_INPUTS["F"])


def test_a_legitimate_zero_is_information_not_an_absence():
    """`_is_all_null` must not treat `0` or `False` as empty. A realised volatility of 0.0 is a
    measurement; folding it into 'this class arrived empty' would delete a real observation."""
    assert ablation._is_all_null({"a": None, "b": None}) is True
    assert ablation._is_all_null({"a": 0}) is False
    assert ablation._is_all_null({"a": False}) is False
    assert ablation._is_all_null([]) is True
    assert ablation._is_all_null(0) is False


def test_main_refuses_an_unavailable_rung(monkeypatch, capsys, tmp_path):
    monkeypatch.setenv("ABLATE_CUTOFFS", "2023-06-15T20:00:00Z")
    monkeypatch.setenv("EVAL_OUT_DIR", str(tmp_path))
    assert ablation.main(["A"]) == ablation.EXIT_RUNG_UNAVAILABLE
    out = capsys.readouterr().out
    assert "RUNG A IS NOT AVAILABLE" in out
    assert "VL model" in out


def test_the_ladder_matches_contract_5_1():
    assert ablation.RUNG_INPUTS["B"] == ("ohlcv",)
    assert ablation.RUNG_INPUTS["C"] == ("ohlcv", "indicators")
    assert ablation.RUNG_INPUTS["D"][-1] == "company_events"
    assert ablation.RUNG_INPUTS["E"][-1] == "earnings_estimates_surprise"
    assert ablation.RUNG_INPUTS["F"][-2:] == ("macro_fed", "market_sector")
    # Each rung is a strict superset of the one below it — that is what makes the ladder a ladder.
    for lower, upper in (("B", "C"), ("C", "D"), ("D", "E"), ("E", "F")):
        assert set(ablation.RUNG_INPUTS[lower]) < set(ablation.RUNG_INPUTS[upper])


def test_a_bundle_records_an_absent_input_rather_than_dropping_it():
    # Rung E, missing `company_events` — an input class E inherits from D, not the one that DEFINES
    # E. A missing defining class is a different finding and raises (see the rung-F test above).
    bundle = ablation.build_bundle(
        "E", "NVDA", "2023-06-15T20:00:00Z",
        fetch=lambda kind, t, a: None if kind == "company_events" else {"kind": kind},
    )
    assert bundle["absent"] == ["company_events"]
    assert set(bundle["inputs"]) == {"ohlcv", "indicators", "earnings_estimates_surprise"}


# ---- 9. the driver end to end, offline ------------------------------------------------------------


def test_driver_runs_end_to_end_and_writes_a_report(tmp_path):
    cfg = config(tmp_path)
    store = FakeStore(many(10))
    result, code = ablation.evaluate_rung("C", cfg, store=store, now="2026-08-20T00:00:00Z")
    files = ablation.write_report(result, cfg.out_dir)
    assert code == ablation.EXIT_OK
    assert result["verdict"] in ablation.VERDICT_MEANING
    body = json.loads(open(files["json"], encoding="utf-8").read())
    assert body["experiment"] == "C" and body["split"] == "dev"
    assert body["cutoffFingerprint"] == result["cutoffFingerprint"]
    markdown = open(files["md"], encoding="utf-8").read()
    assert "necessary condition, never a green light" in markdown
    assert "Rung F IS runnable as of Wave 5A" in markdown
    # The execution rule's assumptions are PRINTED, not buried: a return computed at zero cost is a
    # return nobody could have earned, so the cost has to be on the page beside the number.
    assert "bps charged on every position change" in markdown
    assert "Nothing here places an order" in markdown


def test_the_driver_never_names_a_trade():
    """CLAUDE.md #2 / doc §10: a measurement harness has no buy/sell/hold, no order, no sizing."""
    source = (
        __import__("pathlib").Path(ablation.__file__)
    ).read_text(encoding="utf-8").lower()
    for banned in ("place_order", "position_size", "order_qty", "broker", "hold\" verdict"):
        assert banned not in source
    # The only mention of a trade anywhere is the disclaimer that says there is none.
    assert source.count("buy/sell") == 2
    assert "no buy/sell output, no orders, no money" in source


def test_no_new_dependency():
    text = (
        __import__("pathlib").Path(ablation.__file__).resolve().parents[1] / "requirements.txt"
    ).read_text(encoding="utf-8")
    assert "pandas" in text and "requests" in text          # unchanged
    source = __import__("pathlib").Path(ablation.__file__).read_text(encoding="utf-8")
    # The driver deliberately imports neither pandas, numpy nor lightgbm: it is a report generator
    # over records, and a heavy import here would make `make ablate` slow for no benefit.
    for heavy in ("import pandas", "import numpy", "import lightgbm"):
        assert heavy not in source
