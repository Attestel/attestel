"""Training provenance is a TRI-STATE, and a record that does not state it is not clean.

THE BUG THIS FILE PINS. ``_meta()`` served ``record.get("trainedOnSynthetic", False)``, and its
no-record branch returned a hard ``False``. So a model record that never stated its training
provenance — and even the case where there is NO MODEL AT ALL — told every consumer downstream
"this was trained on real data". The paper engine's gate 1 believed it, which is the exact inversion
that gate exists to prevent: its own comment reads *"unknown provenance is unknown, and unknown
refuses"*, and the field it checked first could not express unknown at all.

Records written by ``save_model_version`` always carry the flag, so the hole only ever opened for
records that predate it or were written by hand — i.e. exactly the records least likely to have been
trained on anything anybody verified.

This is the PREDICTION-SERVICE half of the chain. The other half —
``/predict`` null → paper ``predictResp`` nil → gate 1 refuses → ``/paper/provenance`` null →
snapshot blocker — is `journal/experiment_provenance_integration_test.go`, which runs the real paper
binary against a fake ``/predict`` serving exactly the payload asserted here.
"""
from app import main
from app.features import FEATURE_FRAME_POLICY
from app.store import synthetic_flag
from app.verdicts import expected_strategy_version


def _report() -> dict:
    return {
        "passed": True,
        "thresholds": {"upper": 0.55, "lower": 0.45},
        "costBps": 6.0,
        "allowShort": True,
    }


def test_synthetic_flag_is_a_tri_state():
    # The only value that means VERIFIED REAL is an explicit False.
    assert synthetic_flag({"trainedOnSynthetic": False}) is False
    assert synthetic_flag({"trainedOnSynthetic": True}) is True
    # Absent, explicitly null, and "no record at all" are the same answer: unknown.
    assert synthetic_flag({}) is None
    assert synthetic_flag({"trainedOnSynthetic": None}) is None
    assert synthetic_flag(None) is None
    # A truthy non-bool is still a claim of synthetic training, not a shrug.
    assert synthetic_flag({"trainedOnSynthetic": 1}) is True


def test_meta_serves_null_when_a_record_does_not_state_its_training_provenance():
    record = {
        "modelVersion": "m-1", "dataThrough": "2026-08-25",
        "dataPolicy": FEATURE_FRAME_POLICY,
        # NOTE: no `trainedOnSynthetic` key at all.
    }
    meta = main._meta(record)
    assert meta["trainedOnSynthetic"] is None, (
        "an unstated training provenance became a value; a consumer cannot tell silence from a "
        "denial"
    )
    # An explicit statement still travels as itself.
    assert main._meta({**record, "trainedOnSynthetic": False})["trainedOnSynthetic"] is False
    assert main._meta({**record, "trainedOnSynthetic": True})["trainedOnSynthetic"] is True


def test_meta_with_no_record_at_all_is_unknown_not_clean():
    # THE WORST CASE OF THE OLD DEFAULT. With no model record whatsoever, `_meta` claimed the
    # (nonexistent) model was trained on real data.
    assert main._meta(None)["trainedOnSynthetic"] is None
    assert main._meta({})["trainedOnSynthetic"] is None


def test_predict_payload_carries_null_for_an_unstated_provenance(monkeypatch):
    """The wire shape the paper engine actually decodes.

    `/predict` returns `_meta(record)` spread into every branch, including the early ones. This
    asserts the JSON-visible value is null rather than false, because that is the byte the Go side
    unmarshals into a `*bool`.
    """
    record = {
        "ticker": "NVDA", "timeframe": "1D", "horizon": 5,
        "modelVersion": "m-1", "dataThrough": "2026-08-25",
        "dataPolicy": FEATURE_FRAME_POLICY, "report": _report(),
        # no `trainedOnSynthetic`
    }
    monkeypatch.setattr(main, "load_model", lambda *a, **k: (None, None, record))
    monkeypatch.setattr(main, "evaluation_block", lambda *a, **k: None)

    payload = main.predict("NVDA", timeframe="1D", horizon=5)
    assert "trainedOnSynthetic" in payload, "the field must be present and null, never omitted"
    assert payload["trainedOnSynthetic"] is None
    # `currentData` is the sibling unknown, and it was already correct — asserted here so the two
    # stay consistent as a pair.
    assert payload["currentData"] is None


def test_promotion_refuses_a_candidate_whose_provenance_is_unstated(monkeypatch):
    """Only an explicit False may promote.

    A candidate whose training provenance nobody recorded is an unknown, and promoting an unknown is
    the thing this gate exists to stop.
    """
    report = _report()
    base = {
        "ticker": "NVDA", "timeframe": "1D", "horizon": 5,
        "modelVersion": "candidate-1", "dataThrough": "2026-08-25",
        "dataPolicy": FEATURE_FRAME_POLICY,
        "strategyVersion": expected_strategy_version(report), "report": report,
    }
    monkeypatch.setattr(main, "evaluation_block", lambda *a, **k: {
        "verdict": "EDGE", "current": True, "evidenceCurrent": True,
    })

    # No flag at all -> refused, with a reason that says so rather than claiming synthetic training.
    eligible, gates, _ = main._promotion_gates(base)
    gate = next(g for g in gates if g["name"] == "real-training-data")
    assert eligible is False
    assert gate["passed"] is False
    assert "does not state" in gate["detail"]

    # Explicit null -> same answer.
    eligible, gates, _ = main._promotion_gates({**base, "trainedOnSynthetic": None})
    assert eligible is False
    assert next(g for g in gates if g["name"] == "real-training-data")["passed"] is False

    # Explicit False -> the only value that promotes.
    eligible, gates, _ = main._promotion_gates({**base, "trainedOnSynthetic": False})
    assert eligible is True
    assert next(g for g in gates if g["name"] == "real-training-data")["passed"] is True


def test_shadow_refuses_a_version_whose_provenance_is_unstated(monkeypatch):
    """Shadow evidence informs a promotion review, so it inherits the same rule.

    `if record.get("trainedOnSynthetic")` was a truthiness test, and `None` is falsy — so an unstated
    provenance sailed through the check that is supposed to keep unverified models out of the
    evidence a human promotion decision rests on.
    """
    from app import shadow

    record = {"dataPolicy": FEATURE_FRAME_POLICY, "report": _report()}
    monkeypatch.setattr(shadow, "load_version_model", lambda *a, **k: ("model", None, record))

    try:
        shadow._score_version("NVDA", "1D", 5, "v-1", None)
    except shadow.ShadowInvalid as exc:
        assert "does not state" in str(exc)
    else:  # pragma: no cover - the assertion below reports it
        raise AssertionError("shadow accepted a model whose training provenance is unstated")

    # And an explicit False still gets past this particular check.
    monkeypatch.setattr(
        shadow, "load_version_model",
        lambda *a, **k: ("model", None, {**record, "trainedOnSynthetic": False}),
    )
    try:
        shadow._score_version("NVDA", "1D", 5, "v-1", None)
    except shadow.ShadowInvalid as exc:
        assert "synthetic" not in str(exc), (
            f"a clean record was refused by the synthetic check: {exc}"
        )
    except Exception:
        pass  # it fails later for unrelated reasons (no real row); the provenance gate passed
