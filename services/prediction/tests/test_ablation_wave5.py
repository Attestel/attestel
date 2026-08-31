"""Wave 5 Lane 5A — what the ladder gained: the runtime refusal, the sizing, the full metric
battery, and the SERVED verdict.

READ THIS BEFORE CITING ANY OF IT. Not one test here observes a model. They assert that the driver
REFUSES a fixture-served run, that its arithmetic is the arithmetic it claims, and that the verdict
reaches a file the llm service can serve from. Whether the model produces a verdict worth serving is
unanswered by this repo and stays unanswered until the ladder runs on a prod box.
"""
from __future__ import annotations

import json

import pytest

from app import ablation
from app import ablation_calibration as cal


# ---- fixtures ------------------------------------------------------------------------------------


def answer(model="qwen3-14b", runtime=None):
    env = {
        "identity": {"modelUsed": model},
        "forecast": {"horizons": {"1d": {"direction": "bullish", "score": 0.55,
                                         "abstain": False}},
                     "confidenceBucket": "medium"},
    }
    if runtime is not None:
        env["runtime"] = runtime
    return env


def prediction(*, pid="prd_0", experiment="C", split="dev", model="qwen3-14b",
               directions=("bullish", "bullish", "bullish", "bullish"),
               realized=(0.01, 0.02, 0.03, 0.04), excess=(0.005, 0.01, 0.015, 0.02),
               scores=(0.55, 0.55, 0.55, 0.55), as_of="2023-06-15T20:00:00Z",
               bucket="high", source="yfinance"):
    horizons, outcomes = {}, {}
    for i, horizon in enumerate(ablation.HORIZONS):
        horizons[horizon] = {"direction": directions[i], "score": scores[i],
                             "abstain": directions[i] in ablation.ABSTAIN_DIRECTIONS}
        outcomes[horizon] = {"horizon": horizon, "realizedReturn": realized[i],
                             "excessReturn": excess[i], "resolvedFromSource": source,
                             "resolved": True}
    return {"id": pid, "ticker": "NVDA", "experiment": experiment, "split": split,
            "asOf": as_of, "identity": {"modelUsed": model},
            "forecast": {"horizons": horizons, "confidenceBucket": bucket},
            "outcomes": outcomes}


class FakeStore:
    def __init__(self, records=None):
        self.records = list(records or [])
        self.posted: list[dict] = []

    def post(self, record):
        self.posted.append(record)
        return record

    def list(self, *, experiment, split, limit=500):
        return [r for r in self.records
                if r.get("experiment") == experiment and r.get("split") == split]

    def backfill(self, *, experiment):
        return {"updated": 0, "pending": 0, "refused": 0, "experiment": experiment}


def config(tmp_path, **kw):
    base = dict(cutoffs=["2023-06-15T20:00:00Z", "2023-07-14T20:00:00Z"], universe=["NVDA"],
                rungs=["C"], split="dev", events_url="http://events.test:8004",
                llm_url="http://llm.test:8002", out_dir=str(tmp_path / "eval"), min_samples=4)
    base.update(kw)
    return ablation.AblationConfig(**base)


# ═════════════════════════════════════════════ 1. the runtime refusal


def test_the_runtime_is_read_from_the_served_block_then_from_identity():
    assert ablation.runtime_of(answer(runtime={"served": "prod-a"})) == "prod-a"
    assert ablation.runtime_of(answer(model="qwen3-14b")) == "qwen3-14b"
    assert ablation.runtime_of({}) == "unknown"


def test_a_stub_served_answer_is_refused_loudly():
    with pytest.raises(ablation.StubRuntimeRefused) as excinfo:
        ablation.assert_real_runtime(answer(model="stub:offline"))
    message = str(excinfo.value)
    assert "FIXTURE" in message
    assert "§9.20" in message
    assert "MODEL_RUNTIME_URL" in message


def test_stub_quota_is_refused_as_firmly_as_stub_offline():
    """Both are fixtures. A gate that knew only `stub:offline` would license a claim about the
    model from the other one."""
    with pytest.raises(ablation.StubRuntimeRefused):
        ablation.assert_real_runtime(answer(model="stub:quota"))
    assert ablation.is_stub_runtime("stub:anything") is True
    assert ablation.is_stub_runtime("qwen3-14b") is False


def test_a_stub_answer_ends_the_run_rather_than_skipping_one_cell(tmp_path):
    """Raised, not skipped. One stub answer means the endpoint is down or misconfigured and the
    remaining cells would be fixtures too; discovering that in the verdict costs the whole run."""
    cfg = config(tmp_path)
    with pytest.raises(ablation.StubRuntimeRefused):
        ablation.run_rung("C", cfg, analyst=lambda b: answer(model="stub:offline"),
                          store=FakeStore(), fetch=lambda *a: {"k": 1},
                          now="2026-08-20T00:00:00Z")


def test_the_run_records_which_runtime_served_each_cell(tmp_path):
    cfg = config(tmp_path)
    report = ablation.run_rung("C", cfg, analyst=lambda b: answer(runtime={"served": "prod-a"}),
                               store=FakeStore(), fetch=lambda *a: {"k": 1},
                               now="2026-08-20T00:00:00Z")
    assert report.runtimes == {"prod-a": 2}


def test_opting_out_of_the_runtime_gate_takes_an_explicit_false(monkeypatch):
    monkeypatch.delenv("ABLATE_REQUIRE_REAL_RUNTIME", raising=False)
    assert ablation.AblationConfig.from_env().require_real_runtime is True
    monkeypatch.setenv("ABLATE_REQUIRE_REAL_RUNTIME", "0")
    assert ablation.AblationConfig.from_env().require_real_runtime is True   # only `false` opts out
    monkeypatch.setenv("ABLATE_REQUIRE_REAL_RUNTIME", "false")
    assert ablation.AblationConfig.from_env().require_real_runtime is False


def test_stub_served_records_are_caught_at_read_back_too(tmp_path):
    """`run_rung` refuses at write time, but a store can hold records from an earlier run, another
    operator, or a window before the runtime was configured."""
    cfg = config(tmp_path)
    store = FakeStore([prediction(pid=f"p{i}", model="stub:offline") for i in range(6)])
    result, code = ablation.evaluate_rung("C", cfg, store=store, now="2026-08-20T00:00:00Z")
    assert code == ablation.EXIT_STUB_RUNTIME
    assert result["verdict"] is None
    assert len(result["stubRefusal"]) == 6
    markdown = ablation.render_markdown(result)
    assert "STUB-SERVED RECORDS — cannot validate" in markdown
    for word in ("EDGE", "NO EDGE", "INCONCLUSIVE", "SUSPECT"):
        assert word not in markdown.replace("cannot validate", "")


def test_the_precheck_reads_the_llm_services_runtime_block(tmp_path):
    cfg = config(tmp_path)

    class Resp:
        def __init__(self, body):
            self._body = body

        def json(self):
            return self._body

    ok, detail = ablation.runtime_precheck(
        cfg, get=lambda url: Resp({"runtime": {"configured": True, "name": "prod-a",
                                               "model": "qwen3-14b"}}))
    assert ok and "prod-a" in detail

    ok, detail = ablation.runtime_precheck(
        cfg, get=lambda url: Resp({"runtime": {"configured": False,
                                               "refusals": ["MODEL_RUNTIME_URL is unset"]}}))
    assert not ok and "MODEL_RUNTIME_URL is unset" in detail

    ok, detail = ablation.runtime_precheck(cfg, get=lambda url: (_ for _ in ()).throw(OSError("x")))
    assert not ok and "did not answer" in detail

    # THE SHAPE THE SERVICE ACTUALLY SERVES. `services/llm/app/main.py::health` returns
    # `{"status", "service", "llm": runtime_status()}`, so the runtime block is NESTED one level.
    # The flat form above is accepted too, because a health payload is not this lane's to pin — but
    # the nested one is the one that has to work, and asserting only the flat form would be a test
    # passing against a shape nothing serves.
    ok, detail = ablation.runtime_precheck(
        cfg, get=lambda url: Resp({"status": "ok", "service": "llm",
                                   "llm": {"runtime": {"configured": True, "name": "prod-a",
                                                       "model": "qwen3-14b"}}}))
    assert ok and "prod-a" in detail


def test_main_refuses_before_it_measures_when_no_runtime_is_configured(monkeypatch, tmp_path,
                                                                      capsys):
    monkeypatch.setenv("ABLATE_CUTOFFS", "2023-06-15T20:00:00Z")
    monkeypatch.setenv("EVAL_OUT_DIR", str(tmp_path))
    monkeypatch.setattr(ablation, "runtime_precheck", lambda cfg, **kw: (False, "unset"))
    touched = []
    monkeypatch.setattr(ablation, "PredictionStoreClient",
                        lambda *a, **kw: touched.append(1) or FakeStore())
    assert ablation.main(["C"]) == ablation.EXIT_STUB_RUNTIME
    assert touched == [], "the store must not be opened once the runtime gate has refused"
    out = capsys.readouterr().out
    assert "VERDICT" not in out
    assert "§9.20" in out


# ═════════════════════════════════════════════ 2. sizing, in runs


def test_the_ladder_is_sized_in_runs_and_generations_never_in_minutes(tmp_path):
    cfg = config(tmp_path, cutoffs=["2023-06-15T20:00:00Z", "2023-07-14T20:00:00Z",
                                    "2026-01-05T20:00:00Z"],
                 universe=["NVDA", "AMD"], split="dev")
    plan = ablation.plan_runs(cfg, ["B", "C", "D"])
    assert plan["cutoffsRequested"] == 3
    assert plan["cutoffsInSplit"] == 2          # the 2026 cutoff is in `test`, not `dev`
    assert plan["runsPerRung"] == 4             # 2 cutoffs x 2 tickers
    assert plan["analystRuns"] == 12
    assert plan["modelGenerations"] == 12 * ablation.GENERATIONS_PER_ANALYST_RUN
    # The wall clock is a MULTIPLICATION waiting on 5C's measurement, not a number nobody took.
    assert plan["wallClock"].startswith("unmeasured")
    assert "secondsPerAnalystRun" in plan["wallClock"]


def test_main_prints_the_size_before_anything_runs(monkeypatch, tmp_path, capsys):
    monkeypatch.setenv("ABLATE_CUTOFFS", "2023-06-15T20:00:00Z")
    monkeypatch.setenv("EVAL_OUT_DIR", str(tmp_path))
    monkeypatch.setattr(ablation, "runtime_precheck", lambda cfg, **kw: (False, "unset"))
    ablation.main(["C"])
    out = capsys.readouterr().out
    assert "LADDER SIZE" in out
    assert out.index("LADDER SIZE") < out.index("REFUSED")


def test_main_generates_each_rung_before_it_evaluates(monkeypatch, tmp_path):
    monkeypatch.setenv("ABLATE_CUTOFFS", "2023-06-15T20:00:00Z")
    monkeypatch.setenv("EVAL_OUT_DIR", str(tmp_path))
    monkeypatch.setattr(ablation, "runtime_precheck", lambda cfg, **kw: (True, "fake"))
    monkeypatch.setattr(ablation, "PredictionStoreClient", lambda *a, **kw: FakeStore())
    sequence = []

    def generated(rung, cfg, **_kwargs):
        sequence.append(("generate", rung))
        return ablation.RunReport(rung=rung, split=cfg.split, cutoffs=cfg.cutoffs, written=1)

    def evaluated(rung, cfg, **_kwargs):
        sequence.append(("evaluate", rung))
        return {"experiment": rung, "verdict": "INCONCLUSIVE"}, ablation.EXIT_NO_DATA

    monkeypatch.setattr(ablation, "run_rung", generated)
    monkeypatch.setattr(ablation, "evaluate_rung", evaluated)
    monkeypatch.setattr(ablation, "write_report", lambda *_a, **_k: {"md": "x", "json": "y"})
    monkeypatch.setattr(ablation, "print_result", lambda *_a, **_k: None)
    monkeypatch.setattr(ablation, "write_verdict", lambda *_a, **_k: None)

    assert ablation.main(["B", "C"]) == ablation.EXIT_NO_DATA
    assert sequence == [
        ("generate", "B"), ("evaluate", "B"),
        ("generate", "C"), ("evaluate", "C"),
    ]


# ═════════════════════════════════════════════ 3. the metric battery


def test_rank_ic_measures_the_models_own_ordering():
    """Directional accuracy cannot answer this: a pipeline can be right 55% of the time and rank
    its convictions backwards."""
    rising = [(0.1, 0.01), (0.3, 0.02), (0.6, 0.03), (0.9, 0.04)]
    assert ablation.rank_ic(rising) == pytest.approx(1.0)
    assert ablation.rank_ic([(x, -y) for x, y in rising]) == pytest.approx(-1.0)


def test_rank_ic_is_none_not_zero_when_there_is_no_dispersion():
    """0.0 would read as 'measured, and there is no relationship'. A constant score was never
    measured against anything."""
    assert ablation.rank_ic([(0.5, 0.01), (0.5, 0.02)]) is None
    assert ablation.rank_ic([(0.1, 0.0)]) is None


def test_rank_ic_averages_ties():
    """`score` takes at most six distinct values, so competition-ranking would distort every
    ladder result."""
    assert ablation._ranks([5.0, 5.0, 9.0]) == [1.5, 1.5, 3.0]


def test_sortino_is_none_when_nothing_fell_below_the_target():
    """Not an infinite Sortino: a sample with no losses is a sample too small or a window too kind,
    and a large number there reads as robustness."""
    assert ablation.sortino([0.01, 0.02, 0.03], "20d") is None
    assert ablation.sortino([0.02, -0.01, 0.03], "20d") is not None


def test_max_drawdown_compounds():
    """−50% then +50% is not flat, and reporting it as flat understates the risk this metric
    exists to surface."""
    assert ablation.max_drawdown([-0.5, 0.5]) == pytest.approx(0.5)
    assert ablation.max_drawdown([]) is None


def test_the_execution_rule_charges_both_sides_of_a_round_trip():
    out = ablation.execution_rule([(1.0, 0.01), (-1.0, -0.01)], cost_bps=10.0)
    # 0 -> +1 costs 1 unit of cost; +1 -> -1 costs 2. 10 bps = 0.001.
    assert out["totalCost"] == pytest.approx(0.003)
    assert out["grossReturns"] == pytest.approx([0.01, 0.01])
    assert out["netReturns"] == pytest.approx([0.009, 0.008])
    assert out["turnover"] == 1.0


def test_an_abstention_is_a_declined_opportunity_not_a_missing_one():
    out = ablation.execution_rule([(1.0, 0.01), (0.0, 0.05), (1.0, 0.01)], cost_bps=0.0)
    assert out["trades"] == 2
    assert out["turnover"] == pytest.approx(3 / 3)   # 0->1, 1->0, 0->1
    assert out["grossReturns"][1] == 0.0             # flat means it earned nothing, not 0.05


def test_brier_is_null_without_a_calibration_and_says_why(tmp_path):
    metrics = ablation.compute_metrics([prediction(pid=f"p{i}") for i in range(6)], min_samples=4)
    horizon = metrics["perHorizon"]["20d"]
    assert horizon["brier"] is None
    assert horizon["brierSamples"] == 0
    assert "inventing one" in horizon["brierBasis"]
    assert metrics["calibrated"] is False


def test_brier_is_computed_only_through_an_out_of_sample_calibration():
    fit_records = [prediction(pid=f"f{i}", split="dev", bucket="high") for i in range(12)] + \
        [prediction(pid=f"g{i}", split="dev", bucket="high",
                    realized=(-0.01, -0.02, -0.03, -0.04)) for i in range(12)]
    calibration = cal.fit(fit_records, split="dev", experiment="C")
    scored = [prediction(pid=f"s{i}", split="validation", bucket="high") for i in range(6)]
    metrics = ablation.compute_metrics(scored, min_samples=4, calibration=calibration)
    horizon = metrics["perHorizon"]["20d"]
    assert horizon["brierSamples"] == 6
    assert horizon["brier"] == pytest.approx(0.25)   # p = 0.5 in that bucket, every call correct
    assert "out-of-sample calibration fitted on 'dev'" in horizon["brierBasis"]
    assert metrics["calibrated"] is True


def test_metrics_name_the_runtimes_behind_them():
    mixed = [prediction(pid="a", model="qwen3-14b"), prediction(pid="b", model="stub:offline")]
    assert ablation.compute_metrics(mixed, min_samples=1)["runtimes"] == {
        "qwen3-14b": 1, "stub:offline": 1}


def test_the_execution_path_follows_cutoff_order_not_store_order():
    later = prediction(pid="late", as_of="2023-08-01T20:00:00Z", directions=("bearish",) * 4)
    earlier = prediction(pid="early", as_of="2023-06-15T20:00:00Z")
    a = ablation.compute_metrics([later, earlier], min_samples=1)
    b = ablation.compute_metrics([earlier, later], min_samples=1)
    assert a["perHorizon"]["20d"]["execution"] == b["perHorizon"]["20d"]["execution"]


# ═════════════════════════════════════════════ 4. the SERVED verdict (§9.61)


def test_the_verdict_is_per_horizon_because_the_gate_is_per_horizon():
    result = {
        "experiment": "C", "split": "dev", "verdict": "EDGE", "generatedAt": "2026-08-21T00:00:00Z",
        "cutoffFingerprint": "2@abc",
        "metrics": {"runtimes": {"qwen3-14b": 10}, "perHorizon": {
            "1d": {"scored": 40, "directionalAccuracy": 0.6, "meanExcessReturn": 0.004},
            "20d": {"scored": 40, "directionalAccuracy": 0.48, "meanExcessReturn": 0.004},
        }},
    }
    verdicts = ablation.horizon_verdicts(result)
    assert verdicts["1d"]["validated"] is True
    # A single run-level boolean would license the word on the 20d thesis because the 1d one cleared
    # the bar — §9.61 binding 3 calls that opening the gate on a coincidence.
    assert verdicts["20d"]["validated"] is False
    for row in verdicts.values():
        assert set(("validated", "rung", "horizon")) <= set(row)
        assert row["rung"] == "C"


def test_a_non_edge_verdict_writes_validated_false_which_is_a_real_answer(tmp_path):
    """§9.61 binding 4: the ablation ran and did not license the direction. Both withhold the word;
    only one of them is evidence."""
    result = {"experiment": "C", "split": "dev", "verdict": "NO EDGE",
              "generatedAt": "2026-08-21T00:00:00Z", "cutoffFingerprint": "2@abc",
              "metrics": {"runtimes": {}, "perHorizon": {
                  "1d": {"scored": 40, "directionalAccuracy": 0.7, "meanExcessReturn": 0.01}}}}
    path = ablation.write_verdict(result, str(tmp_path))
    body = json.loads(open(path, encoding="utf-8").read())
    assert body["verdicts"]["C|1d"]["validated"] is False
    assert body["verdicts"]["C|1d"]["verdict"] == "NO EDGE"


@pytest.mark.parametrize("verdict", ["INCONCLUSIVE", "SUSPECT"])
def test_an_unmeasured_run_writes_no_verdict_either(tmp_path, verdict):
    """§9.61 binding 4, at the place it is easiest to get wrong.

    `INCONCLUSIVE` is "too few resolved outcomes to say anything" and `SUSPECT` is "these numbers are
    probably a leak — stop". Neither is a measurement. Writing either as `validated: false` would
    tell the browser the ablation had answered and declined to license the direction, when in fact
    it never answered — and that is precisely the distinction binding 4 exists to preserve.
    """
    result = {"experiment": "C", "split": "dev", "verdict": verdict,
              "generatedAt": "2026-08-21T00:00:00Z", "cutoffFingerprint": "2@abc",
              "metrics": {"runtimes": {}, "perHorizon": {
                  "1d": {"scored": 2, "directionalAccuracy": 1.0, "meanExcessReturn": 0.01}}}}
    assert ablation.write_verdict(result, str(tmp_path)) is None
    assert not (tmp_path / ablation.VERDICT_FILENAME).exists()


def test_a_refused_run_writes_no_verdict_at_all(tmp_path):
    """Not even `validated: false`. `false` means 'we measured and it did not clear the bar', and a
    refused run did not measure."""
    for refusal in ({"refusal": [{"x": 1}]}, {"stubRefusal": [{"x": 1}]}):
        result = {"experiment": "C", "split": "dev", "verdict": None,
                  "generatedAt": "2026-08-21T00:00:00Z", "metrics": {"perHorizon": {}}, **refusal}
        assert ablation.write_verdict(result, str(tmp_path)) is None
    assert not (tmp_path / ablation.VERDICT_FILENAME).exists()


def test_writing_one_rungs_verdict_does_not_delete_another(tmp_path):
    """The ladder is run one rung at a time. An overwrite would make every run erase the last."""
    base = {"split": "dev", "verdict": "EDGE", "generatedAt": "2026-08-21T00:00:00Z",
            "cutoffFingerprint": "2@abc",
            "metrics": {"runtimes": {}, "perHorizon": {
                "1d": {"scored": 40, "directionalAccuracy": 0.6, "meanExcessReturn": 0.004}}}}
    ablation.write_verdict({**base, "experiment": "C"}, str(tmp_path))
    path = ablation.write_verdict({**base, "experiment": "D"}, str(tmp_path))
    body = json.loads(open(path, encoding="utf-8").read())
    assert set(body["verdicts"]) == {"C|1d", "D|1d"}


def test_the_verdict_lands_where_the_llm_service_reads_from(tmp_path):
    cfg = config(tmp_path)
    assert cfg.verdict_dir.endswith("eval/ablation")
    assert ablation.VERDICT_FILENAME == "verdict.json"


def test_main_serves_the_verdict_not_merely_produces_it(monkeypatch, tmp_path, capsys):
    """§9.61 binding 5 — 5A is not complete when the number exists, but when the browser can read
    it. This is the test of that clause, and it fails if `main` only writes a report."""
    monkeypatch.setenv("ABLATE_CUTOFFS", "2023-06-15T20:00:00Z")
    monkeypatch.setenv("EVAL_OUT_DIR", str(tmp_path))
    monkeypatch.setenv("ABLATE_MIN_SAMPLES", "4")
    monkeypatch.setattr(ablation, "runtime_precheck", lambda cfg, **kw: (True, "fake"))
    # The records carry REAL DISPERSION. Ten identical ones give a standard deviation of ~1e-18 and
    # therefore a Sharpe of 8.7e16, which the driver correctly calls SUSPECT — "in this domain that
    # is leakage or a bug far more often than an edge". A fixture that trips the leak detector is
    # the wrong fixture for a test about serving a verdict, and quietly relaxing the detector to fit
    # it would be the exact failure the detector exists to catch.
    # `excess` alternates around a small positive mean, so the annualised Sharpe lands near 1.6
    # rather than in leak territory; `realized` is positive on 7 of 10, so an all-`bullish` set
    # scores 0.7 — above a coin flip on every horizon, which is what EDGE requires.
    records = []
    for i in range(10):
        excess = 0.022 if i % 2 == 0 else -0.018
        realized = 0.01 if i < 7 else -0.01
        records.append(prediction(pid=f"p{i}", realized=(realized,) * 4, excess=(excess,) * 4))
    monkeypatch.setattr(ablation, "PredictionStoreClient", lambda *a, **kw: FakeStore(records))
    assert ablation.main(["C"]) == ablation.EXIT_OK
    served = tmp_path / "ablation" / ablation.VERDICT_FILENAME
    assert served.exists(), "the ladder produced a verdict the browser cannot observe (§9.61 b5)"
    body = json.loads(served.read_text(encoding="utf-8"))
    assert body["schema"] == "ablation-verdict@1"
    assert "C|20d" in body["verdicts"]
    assert "Verdict served" in capsys.readouterr().out
