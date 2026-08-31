"""transcript/thesis/digest/scenario — deterministic tests (no model, no network): chunking,
quote grounding, validation, stubs, and the no-advice invariant across all four modules.

Run:  cd services/llm && python -m pytest -q tests/test_research.py
"""
import json

from app.prompt import BANNED
from app.digest import prune_invalid_clusters, run_digest, stub_digest, validate_digest
from app.investigation import (
    run_investigation,
    stub_investigation,
    validate_investigation,
)
from app.scenario import run_scenario, stub_scenario, validate_scenario
from app.thesis import run_thesis_check, stub_thesis_check, validate_thesis_check
from app.transcript import (
    chunk_text,
    enforce_grounding,
    quote_in_text,
    stub_analysis,
    validate_final,
)


def offline_call(_messages):
    return "", "stub:offline"


def sj(raw):
    try:
        return json.loads(raw)
    except Exception:  # noqa: BLE001
        return None


TRANSCRIPT = (
    "Operator: welcome to the call. "
    "Analyst: Can you quantify the impact of export restrictions on data-center revenue next quarter? "
    "CEO: You know, we're really excited about the roadmap, and I think our teams are executing well. "
    "Analyst: And gross margins? CFO: Margins were strong at a record level this quarter. "
) * 40  # long enough to exercise chunking


# ---------------- transcript ----------------

def test_chunking_covers_text_and_respects_bounds():
    chunks = chunk_text(TRANSCRIPT)
    assert len(chunks) >= 2
    assert sum(len(c) for c in chunks) >= len(TRANSCRIPT[:160_000]) * 0.98  # boundary trims only
    assert all(len(c) <= 13_000 for c in chunks)
    assert chunk_text("short") == ["short"]


def test_quote_grounding_drops_fabrications():
    good_q = "Can you quantify the impact of export restrictions on data-center revenue next quarter?"
    good_a = "we're really excited about the roadmap, and I think our teams are executing well"
    obj = {"evasiveMoments": [
        {"question": good_q, "answer": good_a, "why": "deflected to roadmap"},
        {"question": "Made-up question that never happened in this call?", "answer": good_a, "why": "x"},
        {"question": good_q, "answer": "short", "why": "too short to verify"},
    ]}
    warnings = enforce_grounding(obj, TRANSCRIPT)
    assert len(obj["evasiveMoments"]) == 1
    assert len(warnings) == 2
    assert quote_in_text(good_a.upper(), TRANSCRIPT)  # case/whitespace-insensitive


def test_transcript_stub_is_deterministic_and_valid():
    a = stub_analysis("NVDA", TRANSCRIPT, prior=None)
    b = stub_analysis("NVDA", TRANSCRIPT, prior=None)
    assert a == b
    assert validate_final(a) == []
    assert a["evasiveMoments"] == []  # the stub never fabricates quotes
    assert a["toneShift"] is None
    prior = {"label": "2026-Q1", "structured": {"tone": {"confidence": "low"}}}
    assert "2026-Q1" in (stub_analysis("NVDA", TRANSCRIPT, prior)["toneShift"] or "")


# ---------------- thesis ----------------

def test_thesis_offline_is_neutral_and_advice_free():
    out = run_thesis_check("NVDA", "Buying because data-center demand exceeds supply.",
                           {"news": [{"headline": "x"}]}, offline_call, sj)
    st = out["structured"]
    assert st["verdict"] == "neutral" and st["confidence"] == 0.0 and st["_stub"]
    blob = json.dumps(out).lower()
    assert not any(b in blob for b in BANNED)


def test_thesis_validation_rejects_bad_verdicts():
    w = validate_thesis_check({"verdict": "buy", "confidence": 0.5, "evidence": [],
                               "watchFor": [], "summary": "s"})
    assert any("invalid verdict" in x for x in w)
    assert validate_thesis_check(stub_thesis_check("NVDA", "t", {})) == []


# ---------------- digest ----------------

def test_digest_prunes_invalid_clusters_and_stub_valid():
    items = [{"headline": f"h{i}", "category": "news", "sentiment": 0.5} for i in range(3)]
    obj = {"clusters": [
        {"topic": "ok", "headlineIdx": [0, 2], "impact": "high", "why": "w"},
        {"topic": "bad idx", "headlineIdx": [99], "impact": "low", "why": "w"},
        {"topic": "bad impact", "headlineIdx": [1], "impact": "huge", "why": "w"},
    ], "digest": "d", "noise": []}
    prune_invalid_clusters(obj, len(items))
    assert [c["topic"] for c in obj["clusters"]] == ["ok"]
    stub = stub_digest("NVDA", items)
    assert validate_digest(stub, len(items)) == []


def test_digest_offline_and_empty_paths():
    out = run_digest("NVDA", [], offline_call, sj)
    assert out["modelUsed"] == "stub:no-news"
    out2 = run_digest("NVDA", [{"headline": "h", "category": "filing"}], offline_call, sj)
    assert out2["structured"]["_stub"] and out2["structured"]["clusters"][0]["impact"] == "high"


# ---------------- scenario ----------------

def test_scenario_offline_stub_lists_facts_only():
    ctx = {"suppliers": [{"name": "TSMC"}, {"name": "SK Hynix"}], "risks": [{"risk": "export controls"}]}
    out = run_scenario("NVDA", "What if new sanctions hit China sales?", ctx, offline_call, sj)
    st = out["structured"]
    assert st["_stub"] and "TSMC" in st["firstOrder"][0]
    assert validate_scenario(st) == []
    blob = json.dumps(out).lower()
    assert not any(b in blob for b in BANNED)


def test_all_modules_reject_advice_phrases():
    bad = {"verdict": "confirming", "confidence": 0.5,
           "evidence": [{"fact": "strong buy signal", "bearing": "supports", "note": "n"}],
           "watchFor": [], "summary": "you should buy now"}
    assert any("advice phrase" in w for w in validate_thesis_check(bad))
    bad2 = {"clusters": [], "digest": "strong buy", "noise": []}
    assert any("advice phrase" in w for w in validate_digest(bad2, 0))
    bad3 = {k: [] for k in ("scenario", "assumptions", "firstOrder", "secondOrder", "mitigants", "watchFor")}
    bad3.update({"horizons": {}, "summary": "price target 500"})
    assert any("advice phrase" in w for w in validate_scenario(bad3))


# ---------------- bounded investigation ----------------

def test_investigation_offline_is_honest_and_source_grounded():
    ctx = {
        "technical_context": {"regime": {"trend": "downtrend", "momentum": "neutral"}},
        "news_events": [{"headline": "Company announces a new product"}],
    }
    out = run_investigation(
        "NVDA", "What could change demand next year?", ["news_events"], ctx, offline_call, sj
    )
    st = out["structured"]
    assert st["_stub"] and "offline" in st["answer"].lower()
    assert {f["sourceKey"] for f in st["keyFindings"]} <= set(ctx)
    assert validate_investigation(st, ctx) == []
    assert not any(b in json.dumps(out).lower() for b in BANNED)


def test_investigation_rejects_unknown_sources_and_bad_thesis_status():
    bad = {
        "answer": "Grounded answer.",
        "keyFindings": [
            {"finding": "Invented citation", "sourceKey": "internet", "sourceIndex": 0},
            {"finding": "Wrong record", "sourceKey": "news_events", "sourceIndex": 3},
        ],
        "counterEvidence": [],
        "uncertainty": "Missing source.",
        "thesisImpact": {"status": "bullish", "reason": "x"},
        "nextQuestion": "What next?",
    }
    warnings = validate_investigation(bad, {"news_events": [{"headline": "real"}]})
    assert any("unknown sourceKey" in w for w in warnings)
    assert any("invalid sourceIndex" in w for w in warnings)
    assert "invalid thesisImpact status" in warnings
    assert validate_investigation(stub_investigation("NVDA", "q", {}), {}) == []


def test_investigation_rejects_numbers_that_are_not_in_the_sources_or_question():
    obj = {
        "answer": "Revenue could reach 999 billion.",
        "keyFindings": [],
        "counterEvidence": [],
        "uncertainty": "Source coverage is limited.",
        "thesisImpact": {"status": "not_assessed", "reason": "No thesis supplied."},
        "nextQuestion": "What changes demand?",
    }
    warnings = validate_investigation(obj, {"news_events": [{"headline": "Demand is growing"}]})
    assert "ungrounded number: 999" in warnings
