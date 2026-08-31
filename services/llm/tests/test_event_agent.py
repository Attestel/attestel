"""Wave 2 Lane 2B — the Qwen event agent: schema, validation, retry, stub.

Deterministic: no network, no model. Every test drives `enrich_event` with a fake `call` that
returns a canned reply, so the assertions are on what this module would have posted.

Run:  cd services/llm && python -m pytest -q tests/test_event_agent.py
"""
from __future__ import annotations

import json

import pytest

from app import event_agent as ea
from app.prompt import BANNED

EVENT = {
    "id": "evt_1a2b3c4d5e6f7081",
    "eventType": "regulatory_action",
    "title": "New U.S. semiconductor export restrictions announced",
    "summary": "The Commerce Department published an interim final rule restricting exports of "
               "certain advanced accelerators to additional destinations.",
    "keyFacts": ["The rule takes effect thirty days after publication in the Federal Register."],
    "occurredAt": "2026-08-15T13:17:00Z",
    "publishedAt": "2026-08-15T13:21:00Z",
    "firstSeenAt": "2026-08-15T13:24:11Z",
    "sourceTier": "official",
    "officialConfirmed": True,
    "importance": 0.87,
    "novelty": 0.91,
    "documentCount": 7,
    "enrichmentState": "raw",
    "tickers": [
        {"ticker": "NVDA", "relevance": 0.94, "isPrimary": True},
        {"ticker": "AMD", "relevance": 0.62, "isPrimary": False},
    ],
    "sources": [],
    "audit": None,
}

AS_OF = "2026-08-16T13:21:00Z"
NOW = "2026-08-20T09:00:00Z"

META = {"reasoningModeRequested": "non_thinking", "reasoningModeUsed": "non_thinking",
        "degraded": False, "transport": "openai", "provider": "managed"}


def good(**overrides) -> dict:
    obj = {
        "whyItMatters": "A restriction of this scope can change which customers a supplier is "
                        "permitted to serve.",
        "keyFacts": [],
        "isNovel": True,
        "tickers": [{
            "ticker": "NVDA",
            "potentialDirection": "negative",
            "potentialStrength": 0.8,
            "expectedHorizon": "medium_term",
            "affectedDimensions": ["revenue_exposure", "growth", "margin"],
            "transmission": "A material share of its data-centre demand sits in the restricted "
                            "region.",
        }],
    }
    obj.update(overrides)
    return obj


def replier(*objects, model="qwen3-14b", meta=None):
    """A fake `call`: yields each object in turn, repeating the last one."""
    seen: list[dict] = []
    payloads = list(objects)

    def call(messages, *, json_mode=True, mode=None):
        seen.append({"messages": messages, "mode": mode})
        obj = payloads[min(len(seen) - 1, len(payloads) - 1)]
        raw = obj if isinstance(obj, str) else json.dumps(obj)
        return raw, model, dict(meta or META)

    call.seen = seen
    return call


def run(*objects, event=None, prior=None, **kw):
    return ea.enrich_event(event or EVENT, prior or [], as_of=AS_OF, now=NOW,
                           call=replier(*objects, **kw))


# =================================================================================================
# Versions, enums, and the format that must match the events service byte for byte
# =================================================================================================

def test_the_version_strings_are_the_contract_s():
    assert ea.PROMPT_VERSION == "event-agent@1"
    assert ea.SCHEMA_VERSION == "event@1"


def test_the_enums_are_the_contract_s():
    assert ea.DIRECTIONS == ("positive", "negative", "neutral", "unclear")
    assert ea.HORIZONS == ("intraday", "short_term", "medium_term", "long_term")
    assert ea.AFFECTED_DIMENSIONS == (
        "revenue_exposure", "margin", "growth", "regulation", "management",
        "litigation", "valuation", "demand", "supply", "rates", "currency",
    )


def test_the_prompt_fingerprint_is_pinned_to_its_version():
    """1D's bump rule, applied to this lane's surface: editing SYSTEM or SCHEMA without bumping
    PROMPT_VERSION / SCHEMA_VERSION fails here.

    New value: run this test — the assertion message prints it.
    """
    assert ea.prompt_fingerprint() == ea.PINNED_PROMPT_FINGERPRINT, (
        f"SYSTEM or SCHEMA changed without a version bump; re-pin to "
        f"{ea.prompt_fingerprint()!r} in the SAME commit as the bump"
    )
    assert ea.PINNED_FOR == (ea.PROMPT_VERSION, ea.SCHEMA_VERSION)


def test_the_iso_format_matches_the_events_service_byte_for_byte():
    """`services/events` compares every `as_of` LEXICOGRAPHICALLY in SQL (cluster.py::iso), so the
    width and the `Z` suffix are load-bearing: `+00:00` sorts BEFORE `Z` for the same instant,
    which would make an enrichment visible at a cutoff it was not computed at."""
    assert ea.iso("2026-08-15T13:21:00+00:00") == "2026-08-15T13:21:00Z"
    assert ea.iso("2026-08-15T13:21:00Z") == "2026-08-15T13:21:00Z"
    assert ea.iso("2026-08-15T15:21:00+02:00") == "2026-08-15T13:21:00Z"
    assert ea.iso("2026-08-15T13:21:00") == "2026-08-15T13:21:00Z"
    assert len(ea.iso(NOW)) == 20  # fixed width, or lexicographic order stops being chronological


def test_the_cutoff_is_published_at_plus_the_lag():
    assert ea.cutoff_for("2026-08-15T13:21:00Z", 24) == "2026-08-16T13:21:00Z"
    assert ea.cutoff_for("2026-08-15T13:21:00Z", 0) == "2026-08-15T13:21:00Z"
    assert ea.cutoff_for("2026-08-15T13:21:00Z", 1.5) == "2026-08-15T14:51:00Z"


def test_banned_is_imported_not_copied():
    source = (__import__("pathlib").Path(ea.__file__)).read_text(encoding="utf-8")
    assert "from .prompt import BANNED" in source
    assert "i recommend buying" not in source
    assert ea.BANNED is BANNED


# =================================================================================================
# The happy path
# =================================================================================================

def test_a_valid_reply_becomes_a_payload():
    result = run(good())
    assert result.state == ea.ENRICHED
    assert result.attempts == 1
    body = result.payload
    assert set(body) == {"whyItMatters", "keyFacts", "tickers", "audit", "enrichmentAsOf"}
    assert body["enrichmentAsOf"] == AS_OF
    assert body["tickers"][0]["ticker"] == "NVDA"
    assert body["tickers"][0]["potentialStrength"] == 0.8


def test_the_audit_block_is_the_contract_s_five_keys():
    result = run(good())
    assert result.payload["audit"] == {
        "modelUsed": "qwen3-14b",
        "promptVersion": "event-agent@1",
        "schemaVersion": "event@1",
        "reasoningMode": "non_thinking",
        "enrichedAt": NOW,
    }


def test_non_thinking_mode_is_requested_through_1d_s_routing_table():
    call = replier(good())
    ea.enrich_event(EVENT, [], as_of=AS_OF, now=NOW, call=call)
    assert call.seen[0]["mode"] == "non_thinking"


def test_a_degraded_mode_is_recorded_as_what_actually_ran():
    """The server refused the flag and thinking stayed on. Recording `non_thinking` anyway would
    attribute a Wave 5A ablation result to a cause it did not have."""
    result = run(good(), meta={"reasoningModeRequested": "non_thinking",
                               "reasoningModeUsed": "thinking", "degraded": True})
    assert result.payload["audit"]["reasoningMode"] == "thinking"
    assert result.reasoning_mode == "thinking"


def test_the_identity_block_is_1d_s_seven_keys():
    result = run(good())
    from app.versioning import IDENTITY_KEYS
    assert tuple(result.identity) == IDENTITY_KEYS
    assert result.identity["generationSettings"] == {"temperature": 0.2}
    assert "temperature" not in result.identity  # §9.40: one home only


# =================================================================================================
# Every validation rule
# =================================================================================================

@pytest.mark.parametrize("direction", ["positive", "negative", "neutral", "unclear"])
def test_every_direction_survives_unchanged(direction):
    """`unclear` is a first-class valid answer. A pipeline that cannot return it is a contract
    violation, and coercing it to `neutral` to make a card look decisive is not a UX choice."""
    obj = good()
    obj["tickers"][0]["potentialDirection"] = direction
    result = run(obj)
    assert result.state == ea.ENRICHED
    assert result.payload["tickers"][0]["potentialDirection"] == direction


@pytest.mark.parametrize("horizon", ["intraday", "short_term", "medium_term", "long_term"])
def test_every_horizon_is_accepted(horizon):
    obj = good()
    obj["tickers"][0]["expectedHorizon"] = horizon
    assert run(obj).state == ea.ENRICHED


def test_a_bad_direction_is_rejected():
    obj = good()
    obj["tickers"][0]["potentialDirection"] = "bullish"
    result = run(obj)
    assert result.state == ea.FAILED
    assert any("invalid enum" in f and "potentialDirection" in f for f in result.failures)


def test_a_bad_horizon_is_rejected():
    obj = good()
    obj["tickers"][0]["expectedHorizon"] = "next_week"
    assert run(obj).state == ea.FAILED


def test_a_dimension_outside_the_38_list_is_rejected():
    obj = good()
    obj["tickers"][0]["affectedDimensions"] = ["revenue_exposure", "vibes"]
    result = run(obj)
    assert result.state == ea.FAILED
    assert any("§3.8" in f for f in result.failures)


@pytest.mark.parametrize("strength", [-0.1, 1.1, 2, "0.8", True, None])
def test_a_strength_outside_zero_one_is_rejected(strength):
    obj = good()
    obj["tickers"][0]["potentialStrength"] = strength
    assert run(obj).state == ea.FAILED


@pytest.mark.parametrize("strength", [0.0, 0.5, 1.0, 1, 0])
def test_the_endpoints_of_zero_one_are_valid(strength):
    obj = good()
    obj["tickers"][0]["potentialStrength"] = strength
    assert run(obj).state == ea.ENRICHED


def test_an_invented_ticker_is_rejected():
    """The model may weaken, strengthen or annotate a link. It may not invent a company."""
    obj = good()
    obj["tickers"].append({
        "ticker": "INTC", "potentialDirection": "positive", "potentialStrength": 0.4,
        "expectedHorizon": "long_term", "affectedDimensions": ["demand"],
        "transmission": "A competitor may pick up displaced demand.",
    })
    result = run(obj)
    assert result.state == ea.FAILED
    assert any("invented ticker: INTC" in f for f in result.failures)


def test_a_subset_of_the_linked_tickers_is_fine():
    obj = good()  # NVDA only, while the event links NVDA and AMD
    assert run(obj).state == ea.ENRICHED


def test_a_model_authored_relevance_is_rejected():
    """§9.15: `relevance` is code's and write-once — Wave 5B thresholds notifications on it."""
    obj = good()
    obj["tickers"][0]["relevance"] = 0.9
    result = run(obj)
    assert result.state == ea.FAILED
    assert any("relevance is code's" in f for f in result.failures)


@pytest.mark.parametrize("phrase", list(BANNED))
def test_every_banned_phrase_is_caught(phrase):
    obj = good()
    obj["whyItMatters"] = f"Analysts say this is a {phrase} situation."
    result = run(obj)
    assert result.state == ea.FAILED
    assert any("contains advice phrase" in f for f in result.failures)


@pytest.mark.parametrize("text", [
    "Revenue exposure of $4 billion is at risk.",
    "This could cut demand by 30%.",
    "The level to watch is 118.50.",
    "Roughly 12 percent of shipments are affected.",
    "A 250 bps hit to margin is plausible.",
    "About 4 billion of revenue sits in the region.",
])
def test_a_numeric_claim_in_why_it_matters_is_rejected(text):
    """Invariant #3: the LLM never invents a number. Ordinal scores in [0,1] and enum labels only —
    no price, no level, no target, no percentage return."""
    result = run(good(whyItMatters=text))
    assert result.state == ea.FAILED
    assert any("numeric claim in whyItMatters" in f for f in result.failures)


def test_a_numeric_claim_in_transmission_is_rejected():
    obj = good()
    obj["tickers"][0]["transmission"] = "Around 45% of its data-centre revenue is exposed."
    result = run(obj)
    assert result.state == ea.FAILED
    assert any("numeric claim in tickers[0].transmission" in f for f in result.failures)


@pytest.mark.parametrize("text", [
    "The rule takes effect thirty days after publication.",
    "This is the third such action in 2026.",
    "Q3 guidance may be revisited.",
    "Two of its largest markets are affected.",
])
def test_ordinary_prose_with_plain_integers_survives(text):
    assert run(good(whyItMatters=text)).state == ea.ENRICHED


def test_a_missing_why_it_matters_is_a_failure():
    obj = good()
    del obj["whyItMatters"]
    result = run(obj, obj)
    assert result.state == ea.FAILED
    assert any(f.startswith("missing key: whyItMatters") for f in result.failures)


def test_an_empty_tickers_array_is_a_failure():
    assert run(good(tickers=[]), good(tickers=[])).state == ea.FAILED


def test_a_non_object_reply_is_a_failure():
    result = run("not json at all", "still not json")
    assert result.state == ea.FAILED
    assert result.failures == ("output is not a JSON object",)


# =================================================================================================
# The retry, and only the retry
# =================================================================================================

def test_one_firmer_retry_then_success():
    call = replier({"tickers": []}, good())      # structural failure, then a good answer
    result = ea.enrich_event(EVENT, [], as_of=AS_OF, now=NOW, call=call)
    assert result.state == ea.ENRICHED
    assert result.attempts == 2
    assert len(call.seen) == 2
    firmer = call.seen[1]["messages"][-1]["content"]
    assert "YOUR PREVIOUS REPLY WAS REJECTED" in firmer
    assert "missing key: whyItMatters" in firmer   # the retry NAMES the field that failed


def test_two_structural_failures_reach_the_stub():
    call = replier({"tickers": []}, {"tickers": []})
    result = ea.enrich_event(EVENT, [], as_of=AS_OF, now=NOW, call=call)
    assert result.state == ea.FAILED
    assert result.attempts == 2
    assert len(call.seen) == 2
    assert result.payload is None       # state and audit, never hypotheses


def test_a_semantic_failure_is_never_retried():
    """"Never retry a semantically valid answer you dislike." An invented ticker or an advice
    phrase is not a model that misunderstood the shape; a second attempt is not a safety
    mechanism."""
    obj = good()
    obj["tickers"][0]["ticker"] = "INTC"
    call = replier(obj, good())
    result = ea.enrich_event(EVENT, [], as_of=AS_OF, now=NOW, call=call)
    assert result.state == ea.FAILED
    assert result.attempts == 1
    assert len(call.seen) == 1


def test_needs_retry_mirrors_prompts_semantics():
    assert ea.needs_retry(["missing key: tickers"]) is True
    assert ea.needs_retry(["output is not a JSON object"]) is True
    assert ea.needs_retry(["invalid enum: tickers[0].expectedHorizon must be one of []"]) is True
    assert ea.needs_retry(["out of range: tickers[0].potentialStrength=2 ∉ [0,1]"]) is True
    assert ea.needs_retry(["contains advice phrase: 'strong buy'"]) is False
    assert ea.needs_retry(["invented ticker: INTC is not linked to this event"]) is False
    assert ea.needs_retry(["numeric claim in whyItMatters: …"]) is False
    assert ea.needs_retry([]) is False


# =================================================================================================
# The stub path
# =================================================================================================

def test_an_offline_provider_chain_goes_straight_to_the_stub():
    call = replier(good(), model="stub:offline", meta={"reasoningModeUsed": None})
    result = ea.enrich_event(EVENT, [], as_of=AS_OF, now=NOW, call=call)
    assert result.state == ea.FAILED
    assert result.model_used == "stub:offline"
    assert result.attempts == 1              # a chain already exhausted is not retried
    assert len(call.seen) == 1
    assert result.payload is None


def test_a_quota_stub_is_the_same_path():
    call = replier(good(), model="stub:quota", meta={"reasoningModeUsed": None})
    result = ea.enrich_event(EVENT, [], as_of=AS_OF, now=NOW, call=call)
    assert result.state == ea.FAILED
    assert result.model_used == "stub:quota"


def test_the_stub_is_deterministic_across_two_calls():
    """Same input ⇒ same stub output, always. `now` is an input, which is why it is a parameter and
    not a wall-clock read inside the function."""
    def once():
        call = replier(good(), model="stub:offline", meta={"reasoningModeUsed": None})
        return ea.enrich_event(EVENT, [], as_of=AS_OF, now=NOW, call=call)

    a, b = once(), once()
    assert a == b
    assert json.dumps(a.identity, sort_keys=True) == json.dumps(b.identity, sort_keys=True)


def test_the_stub_never_invents_a_why_it_matters():
    """Invariant #1 is already satisfied by Lane 2A's raw event; a fabricated hypothesis would be
    worse than an absent one."""
    call = replier(good(), model="stub:offline", meta={"reasoningModeUsed": None})
    result = ea.enrich_event(EVENT, [], as_of=AS_OF, now=NOW, call=call)
    assert result.payload is None
    assert result.identity["modelUsed"] == "stub:offline"
    assert result.identity["reasoningMode"] is None
    assert result.identity["generationSettings"] == {}   # no request was made, so no settings


# =================================================================================================
# The two fields that leave the payload (§9.16), and the fact filter (§9.14)
# =================================================================================================

def test_is_novel_never_reaches_the_payload():
    result = run(good(isNovel=True))
    assert "isNovel" not in json.dumps(result.payload)


def test_is_novel_false_skips_enrichment_without_writing_a_column():
    """The novelty JUDGEMENT is local to the skip decision. It never writes `novelty`, which is
    deterministic and code's (§3.6)."""
    result = run(good(isNovel=False))
    assert result.state == ea.SKIPPED
    assert result.payload is None
    assert result.skip_reason


def test_severity_and_relevance_are_stripped_from_the_payload():
    obj = good(severity="high")
    result = run(obj)
    blob = json.dumps(result.payload)
    assert "severity" not in blob
    assert "relevance" not in blob


def test_an_unrecognised_model_field_cannot_leak_into_the_payload():
    """The payload is CONSTRUCTED field by field, never copied, so tomorrow's model invention
    cannot ride along."""
    obj = good(importance=0.99, clusterKey="deadbeef", occurredAt="2030-01-01T00:00:00Z")
    obj["tickers"][0]["isPrimary"] = False
    result = run(obj)
    blob = json.dumps(result.payload)
    for forbidden in ("importance", "clusterKey", "occurredAt", "isPrimary"):
        assert forbidden not in blob, forbidden


def test_a_key_fact_quoted_verbatim_survives():
    fact = EVENT["keyFacts"][0]
    result = run(good(keyFacts=[fact]))
    assert result.payload["keyFacts"] == [fact]


def test_a_paraphrased_key_fact_is_dropped_not_rejected():
    """§9.14 rejects the WHOLE payload on one unverified string, taking every hypothesis with it.
    Dropping the fact keeps the hypotheses; that is the fail-safe direction."""
    result = run(good(keyFacts=["The rule becomes effective roughly a month from now."]))
    assert result.state == ea.ENRICHED
    assert result.payload["keyFacts"] == []
    assert result.payload["whyItMatters"]


def test_a_fact_is_verified_against_one_supplied_string_not_the_concatenation():
    """Joining `title` and `summary` mints n-grams that span the join and exist in no document —
    they would pass locally and 400 at the endpoint."""
    spanning = "export restrictions announced The Commerce Department published an interim"
    result = run(good(keyFacts=[spanning]))
    assert result.payload["keyFacts"] == []


def test_a_fact_below_the_twenty_character_floor_is_dropped():
    result = run(good(keyFacts=["The rule"]))
    assert result.payload["keyFacts"] == []


# =================================================================================================
# The prompt itself
# =================================================================================================

def test_the_prompt_carries_the_event_and_the_prior_events_and_nothing_else():
    prior = [{"eventType": "supply_chain", "title": "Earlier restriction reported",
              "publishedAt": "2026-07-01T00:00:00Z", "importance": 0.5,
              "summary": "should not be shown"}]
    messages = ea.build_messages(EVENT, prior)
    assert messages[0]["role"] == "system" and messages[0]["content"] == ea.SYSTEM
    user = messages[1]["content"]
    assert EVENT["title"] in user
    assert "Earlier restriction reported" in user
    assert "should not be shown" not in user     # prior events are title/type/date only
    assert "['NVDA', 'AMD']" in user             # the linked set, spelled out


def test_the_prior_events_list_is_bounded():
    prior = [{"eventType": "other", "title": f"headline {i}",
              "publishedAt": "2026-07-01T00:00:00Z"} for i in range(50)]
    user = ea.build_messages(EVENT, prior)[1]["content"]
    assert user.count("headline ") == ea.PRIOR_EVENT_LIMIT


def test_the_system_prompt_forbids_advice_and_invented_numbers():
    lowered = ea.SYSTEM.lower()
    assert "not a financial advisor" in lowered
    assert "never" in lowered
    assert "unclear" in lowered


def test_an_unknown_task_key_would_raise_rather_than_default():
    """`mode_for` raises on an unknown task so a new call site cannot join the Wave 5A ablation
    under a mode nobody chose."""
    from app.modes import NON_THINKING, mode_for
    assert mode_for(ea.TASK) == NON_THINKING
    with pytest.raises(ValueError):
        mode_for("event_enrichment_but_misspelled")
