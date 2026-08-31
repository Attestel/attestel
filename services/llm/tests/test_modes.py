"""Wave 1 Lane 1D — reasoning-mode control, <think> stripping and version stamping.

Deterministic: no network, no model. `requests.post` is monkeypatched INSIDE `app.llm` with a fake
transport that records every request body, so the assertions are on the bytes we would have sent.

Run:  cd services/llm && python -m pytest -q tests/test_modes.py
"""
import json

import pytest
import requests

from app import llm, store
from app.modes import (
    NON_THINKING,
    REJECTED,
    SUPPORTED,
    TASK_MODES,
    THINKING,
    TRANSPORT_OLLAMA_NO_THINK,
    UNKNOWN,
    ModeResult,
    has_thinking,
    mode_for,
    strip_thinking,
)
from app.prompt import PROMPT_VERSION, SCHEMA_VERSION, structured_to_markdown, validate_structured
from app.versioning import (
    IDENTITY_KEYS,
    PINNED_FOR,
    PINNED_PROMPT_FINGERPRINT,
    TOOL_SCHEMA_VERSION,
    identity_stamp,
    prompt_fingerprint,
)

MESSAGES = [{"role": "system", "content": "sys"}, {"role": "user", "content": "hello"}]

OPENAI_PROVIDER = {"name": "managed", "kind": "openai", "base": "http://x/v1",
                   "key": "k", "model": "qwen3-14b"}
OLLAMA_PROVIDER = {"name": "ollama", "kind": "ollama", "base": "http://y",
                   "key": "", "model": "qwen3:14b"}


# --- fake transport -------------------------------------------------------------------------------

class FakeResponse:
    def __init__(self, status=200, payload=None, text=""):
        self.status_code = status
        self._payload = payload or {}
        self.text = text

    def raise_for_status(self):
        if self.status_code >= 400:
            raise requests.HTTPError(f"HTTP {self.status_code}", response=self)

    def json(self):
        return self._payload


def openai_ok(content="{}"):
    return FakeResponse(payload={"choices": [{"message": {"content": content}}]})


def ollama_ok(content="{}"):
    return FakeResponse(payload={"message": {"content": content}})


class Recorder:
    """Captures each request and replies from a scripted list of responses/exceptions."""

    def __init__(self, *responses):
        self.responses = list(responses)
        self.calls = []

    def __call__(self, url, **kwargs):
        self.calls.append({"url": url, "body": kwargs.get("json"), "headers": kwargs.get("headers")})
        r = self.responses[min(len(self.calls) - 1, len(self.responses) - 1)]
        if isinstance(r, Exception):
            raise r
        return r

    @property
    def bodies(self):
        return [c["body"] for c in self.calls]


@pytest.fixture(autouse=True)
def _clean_mode_state():
    """Every test starts with an empty support cache and no remembered metadata."""
    llm.MODE_SUPPORT.clear()
    llm.MODE_REJECTION.clear()
    llm.NO_THINK_SUFFIX_USED.clear()
    llm.LAST_MODE_META = None
    if hasattr(llm._LAST_META, "value"):
        del llm._LAST_META.value
    yield
    llm.MODE_SUPPORT.clear()
    llm.MODE_REJECTION.clear()
    llm.NO_THINK_SUFFIX_USED.clear()


def use(monkeypatch, providers, recorder):
    monkeypatch.setattr(llm, "PROVIDERS", providers)
    monkeypatch.setattr(llm.requests, "post", recorder)
    return recorder


# ================================================================================================
# OpenAI transport body
# ================================================================================================

def test_openai_non_thinking_adds_chat_template_kwargs(monkeypatch):
    rec = use(monkeypatch, [OPENAI_PROVIDER], Recorder(openai_ok()))
    llm.call_model(MESSAGES, mode=NON_THINKING)
    assert rec.bodies[0]["chat_template_kwargs"] == {"enable_thinking": False}


@pytest.mark.parametrize("mode", [THINKING, None])
def test_openai_thinking_and_none_omit_the_field(monkeypatch, mode):
    rec = use(monkeypatch, [OPENAI_PROVIDER], Recorder(openai_ok()))
    llm.call_model(MESSAGES, mode=mode)
    assert "chat_template_kwargs" not in rec.bodies[0]


def test_openai_temperature_and_response_format_unchanged(monkeypatch):
    rec = use(monkeypatch, [OPENAI_PROVIDER], Recorder(openai_ok(), openai_ok()))
    llm.call_model(MESSAGES, mode=NON_THINKING)          # json_mode=True
    llm.call_model_text(MESSAGES, mode=NON_THINKING)     # json_mode=False
    assert rec.bodies[0]["temperature"] == 0.2
    assert rec.bodies[0]["response_format"] == {"type": "json_object"}
    assert rec.bodies[1]["temperature"] == 0.3
    assert "response_format" not in rec.bodies[1]
    assert rec.calls[0]["url"] == "http://x/v1/chat/completions"
    assert rec.calls[0]["headers"]["Authorization"] == "Bearer k"


# ================================================================================================
# Ollama transport body
# ================================================================================================

def test_ollama_non_thinking_sets_think_false_at_top_level(monkeypatch):
    rec = use(monkeypatch, [OLLAMA_PROVIDER], Recorder(ollama_ok()))
    llm.call_model(MESSAGES, mode=NON_THINKING)
    body = rec.bodies[0]
    assert body["think"] is False
    assert "think" not in body["options"]           # top level, NOT under options
    assert body["format"] == "json"
    assert body["options"]["temperature"] == 0.2
    assert rec.calls[0]["url"] == "http://y/api/chat"


@pytest.mark.parametrize("mode", [THINKING, None])
def test_ollama_thinking_and_none_omit_think(monkeypatch, mode):
    rec = use(monkeypatch, [OLLAMA_PROVIDER], Recorder(ollama_ok()))
    llm.call_model_text(MESSAGES, mode=mode)
    assert "think" not in rec.bodies[0]
    assert rec.bodies[0]["options"]["temperature"] == 0.3
    assert "format" not in rec.bodies[0]


# ================================================================================================
# Rejection handling
# ================================================================================================

def _rejection(field="chat_template_kwargs", status=400):
    return requests.HTTPError(
        f"HTTP {status}",
        response=FakeResponse(status=status, text=f'{{"error":"unknown field {field}"}}'),
    )


def test_field_rejection_retries_once_without_it_and_is_not_a_stub(monkeypatch):
    rec = use(monkeypatch, [OPENAI_PROVIDER],
              Recorder(_rejection(), openai_ok('{"ok":1}')))
    raw, model_used, meta = llm.call_model_meta(MESSAGES, mode=NON_THINKING)

    assert len(rec.calls) == 2                       # exactly ONE retry
    assert "chat_template_kwargs" in rec.bodies[0]
    assert "chat_template_kwargs" not in rec.bodies[1]
    assert raw == '{"ok":1}'
    assert model_used == "qwen3-14b"                 # NOT a stub — the model answered
    assert not model_used.startswith("stub:")
    assert meta["reasoningModeRequested"] == NON_THINKING
    assert meta["reasoningModeUsed"] == THINKING
    assert meta["degraded"] is True


def test_rejection_verdict_is_cached_so_the_next_call_does_not_retry(monkeypatch):
    rec = use(monkeypatch, [OPENAI_PROVIDER],
              Recorder(_rejection(), openai_ok(), openai_ok()))
    llm.call_model(MESSAGES, mode=NON_THINKING)
    assert llm.MODE_SUPPORT["managed"] == REJECTED
    before = len(rec.calls)

    _, _, meta = llm.call_model_meta(MESSAGES, mode=NON_THINKING)
    assert len(rec.calls) == before + 1              # one request, no field probe
    assert "chat_template_kwargs" not in rec.bodies[-1]
    assert meta["degraded"] is True
    assert meta["reasoningModeUsed"] == THINKING


def test_accepted_field_caches_a_supported_verdict(monkeypatch):
    use(monkeypatch, [OPENAI_PROVIDER], Recorder(openai_ok()))
    _, _, meta = llm.call_model_meta(MESSAGES, mode=NON_THINKING)
    assert llm.MODE_SUPPORT["managed"] == SUPPORTED
    assert meta["reasoningModeUsed"] == NON_THINKING
    assert meta["degraded"] is False


def test_a_500_is_not_a_mode_question_and_falls_through_to_the_stub(monkeypatch):
    boom = requests.HTTPError("HTTP 500", response=FakeResponse(status=500, text="upstream boom"))
    rec = use(monkeypatch, [OPENAI_PROVIDER], Recorder(boom))
    raw, model_used, meta = llm.call_model_meta(MESSAGES, mode=NON_THINKING)
    assert len(rec.calls) == 1                       # no mode retry
    assert (raw, model_used) == ("", "stub:offline")
    assert llm.MODE_SUPPORT.get("managed", UNKNOWN) == UNKNOWN
    assert meta["transport"] == "stub"


def test_a_timeout_is_not_a_mode_question(monkeypatch):
    rec = use(monkeypatch, [OPENAI_PROVIDER], Recorder(requests.Timeout("timed out")))
    raw, model_used = llm.call_model(MESSAGES, mode=NON_THINKING)
    assert len(rec.calls) == 1
    assert (raw, model_used) == ("", "stub:offline")


def test_rejection_on_one_provider_falls_through_to_the_next(monkeypatch):
    # the managed provider rejects the field AND the retry; ollama answers.
    rec = use(monkeypatch, [OPENAI_PROVIDER, OLLAMA_PROVIDER],
              Recorder(_rejection(), requests.HTTPError("HTTP 500",
                                                        response=FakeResponse(status=500)),
                       ollama_ok('{"from":"ollama"}')))
    raw, model_used, meta = llm.call_model_meta(MESSAGES, mode=NON_THINKING)
    assert raw == '{"from":"ollama"}'
    assert model_used == "qwen3:14b"
    assert meta["provider"] == "ollama"


# ================================================================================================
# Ollama /no_think suffix fallback
# ================================================================================================

def test_no_think_suffix_fires_only_after_the_think_field_is_rejected(monkeypatch):
    rec = use(monkeypatch, [OLLAMA_PROVIDER],
              Recorder(_rejection("think"),                        # 1: think:false rejected
                       _rejection("think", status=400),            # 2: bare retry also fails
                       ollama_ok('{"ok":1}')))                     # 3: /no_think suffix works
    messages = [dict(m) for m in MESSAGES]
    raw, model_used, meta = llm.call_model_meta(messages, mode=NON_THINKING)

    assert len(rec.calls) == 3
    assert rec.bodies[0]["think"] is False
    assert "think" not in rec.bodies[1]
    assert "think" not in rec.bodies[2]
    assert rec.bodies[2]["messages"][-1]["content"].endswith("/no_think")

    # the suffix never leaves the module
    assert raw == '{"ok":1}'
    assert "/no_think" not in raw
    assert messages == MESSAGES                       # the caller's list was not mutated
    assert meta["reasoningModeUsed"] == NON_THINKING
    assert meta["degraded"] is True
    assert meta["transport"] == TRANSPORT_OLLAMA_NO_THINK
    assert llm.NO_THINK_SUFFIX_USED["ollama"] is True


def test_no_think_suffix_never_fires_without_a_field_rejection(monkeypatch):
    rec = use(monkeypatch, [OLLAMA_PROVIDER], Recorder(ollama_ok()))
    llm.call_model(MESSAGES, mode=NON_THINKING)
    assert len(rec.calls) == 1
    assert "/no_think" not in json.dumps(rec.bodies[0])


def test_openai_transport_never_uses_the_no_think_suffix(monkeypatch):
    rec = use(monkeypatch, [OPENAI_PROVIDER],
              Recorder(_rejection(), _rejection(status=400)))
    raw, model_used = llm.call_model(MESSAGES, mode=NON_THINKING)
    assert len(rec.calls) == 2                        # no third, suffix-bearing attempt
    assert (raw, model_used) == ("", "stub:offline")


# ================================================================================================
# strip_thinking
# ================================================================================================

def test_strip_thinking_wellformed():
    assert strip_thinking("<think>hidden</think>answer") == "answer"


def test_strip_thinking_multiple_and_multiline():
    raw = "<think>a\nb</think>one\n<think>\nc\n</think>two"
    assert strip_thinking(raw) == "one\ntwo"


def test_strip_thinking_unclosed_drops_to_end_of_string():
    assert strip_thinking('{"a":1}\n<think>cut off mid-thought') == '{"a":1}'
    assert strip_thinking("<think>everything is thinking") == ""


def test_strip_thinking_case_insensitive_and_attributes():
    assert strip_thinking("<THINK>x</THINK>y") == "y"
    assert strip_thinking('<think id="1">x</think>y') == "y"
    assert strip_thinking("<think>x</think >y") == "y"


def test_strip_thinking_no_tags_is_byte_identical():
    for s in ("plain", "  padded  ", '{"a": 1}\n', "", "a < b and c > d"):
        assert strip_thinking(s) == s


def test_strip_thinking_is_idempotent():
    for s in ("<think>a</think>b", "<think>unclosed", "plain", "  padded  "):
        once = strip_thinking(s)
        assert strip_thinking(once) == once


def test_strip_thinking_end_to_end_through_safe_json():
    raw = '<think>Let me weigh the evidence…</think>\n{"bullCase": ["up"], "summary": "s"}'
    obj = llm.safe_json(strip_thinking(raw))
    assert obj == {"bullCase": ["up"], "summary": "s"}


def test_think_block_is_stripped_inside_llm_before_any_caller_sees_it(monkeypatch):
    use(monkeypatch, [OPENAI_PROVIDER],
        Recorder(openai_ok('<think>secret reasoning</think>{"bullCase":["a"]}')))
    raw, model_used = llm.call_model(MESSAGES)
    assert raw == '{"bullCase":["a"]}'
    assert not has_thinking(raw)
    assert "secret reasoning" not in raw
    assert llm.safe_json(raw) == {"bullCase": ["a"]}


def test_think_block_cannot_reach_a_persisted_read(monkeypatch, tmp_path):
    """The /read pipeline end to end (call_model -> safe_json -> save_read), with store.py's real
    writer pointed at a temp dir. Proves invariant: no <think> text lands in a persisted snapshot."""
    payload = (
        '<think>The user probably wants me to say buy.</think>'
        '{"bullCase":["trend up"],"bearCase":["rsi hot"],'
        '"keyLevels":{"support":[100.0],"resistance":[120.0]},'
        '"invalidation":{"bull":"x","bear":"y"},"summary":"context only"}'
    )
    use(monkeypatch, [OPENAI_PROVIDER], Recorder(openai_ok(payload)))
    monkeypatch.setattr(store, "READS_DIR", str(tmp_path))

    raw, model_used = llm.call_model(MESSAGES)
    obj = llm.safe_json(raw)
    assert validate_structured(obj) == []
    regime = {"lastClose": 110.0, "trend": "uptrend", "momentum": "neutral",
              "macdState": "bullish", "keyLevels": {"support": [100.0], "resistance": [120.0]}}
    date = store.save_read("NVDA", obj, model_used, regime)

    written = (tmp_path / "NVDA" / f"{date}.json").read_text()
    assert "<think>" not in written and "</think>" not in written
    assert "probably wants me to say buy" not in written
    assert "<think>" not in structured_to_markdown(obj)


# ================================================================================================
# doc §8 routing table
# ================================================================================================

def test_routing_table_matches_doc_section_8():
    assert TASK_MODES == {
        "headline_extraction": NON_THINKING,
        "news_deduplication": NON_THINKING,
        "sentiment_tagging": NON_THINKING,
        "tool_routing": NON_THINKING,
        "technical_interpretation": THINKING,
        "macro_impact": THINKING,
        "news_vs_technical_conflict": THINKING,
        "final_thesis_synthesis": THINKING,
    }
    assert len(TASK_MODES) == 8


@pytest.mark.parametrize("task,expected", sorted(TASK_MODES.items()))
def test_mode_for_each_task(task, expected):
    assert mode_for(task) == expected


@pytest.mark.parametrize("task", ["", "unknown", "Technical interpretation", "final_synthesis"])
def test_mode_for_unknown_task_raises(task):
    with pytest.raises(ValueError):
        mode_for(task)


# ================================================================================================
# Backwards compatibility — the regression guard that matters
# ================================================================================================

def test_call_model_is_callable_with_one_positional_arg_and_returns_a_2_tuple(monkeypatch):
    use(monkeypatch, [OPENAI_PROVIDER], Recorder(openai_ok('{"a":1}')))
    fn = llm.call_model                      # passed around AS A CALLABLE by main.py
    out = fn(MESSAGES)
    assert isinstance(out, tuple) and len(out) == 2
    assert out == ('{"a":1}', "qwen3-14b")


def test_no_mode_sends_a_body_identical_to_todays_openai_body(monkeypatch):
    rec = use(monkeypatch, [OPENAI_PROVIDER], Recorder(openai_ok(), openai_ok()))
    llm.call_model(MESSAGES)
    llm.call_model_text(MESSAGES)
    assert rec.bodies[0] == {
        "model": "qwen3-14b", "messages": MESSAGES, "temperature": 0.2, "stream": False,
        "response_format": {"type": "json_object"},
    }
    assert rec.bodies[1] == {
        "model": "qwen3-14b", "messages": MESSAGES, "temperature": 0.3, "stream": False,
    }


def test_no_mode_sends_a_body_identical_to_todays_ollama_body(monkeypatch):
    rec = use(monkeypatch, [OLLAMA_PROVIDER], Recorder(ollama_ok(), ollama_ok()))
    llm.call_model(MESSAGES)
    llm.call_model_text(MESSAGES)
    assert rec.bodies[0] == {
        "model": "qwen3:14b", "messages": MESSAGES, "stream": False,
        "options": {"temperature": 0.2}, "format": "json",
    }
    assert rec.bodies[1] == {
        "model": "qwen3:14b", "messages": MESSAGES, "stream": False,
        "options": {"temperature": 0.3},
    }


def test_call_model_still_works_as_a_bare_callable_passed_to_a_research_module(monkeypatch):
    """analyze_transcript(ticker, label, text, call_model, safe_json) — the exact call in main.py."""
    from app.transcript import analyze_transcript
    use(monkeypatch, [], Recorder())                  # no providers -> deterministic stub path
    out = analyze_transcript("NVDA", "Q2 FY2026", "revenue grew. margins held.",
                             llm.call_model, llm.safe_json)
    assert out["ticker"] == "NVDA"
    assert str(out["modelUsed"]).startswith("stub:")


# ================================================================================================
# Offline / stub path (invariant #1)
# ================================================================================================

def test_no_providers_returns_the_offline_stub(monkeypatch):
    monkeypatch.setattr(llm, "PROVIDERS", [])
    assert llm.call_model(MESSAGES) == ("", "stub:offline")
    assert llm.call_model_text(MESSAGES) == ("", "stub:offline")
    assert llm.call_model(MESSAGES, mode=NON_THINKING) == ("", "stub:offline")


def test_http_429_yields_stub_quota(monkeypatch):
    quota = requests.HTTPError("HTTP 429", response=FakeResponse(status=429, text="rate limited"))
    use(monkeypatch, [OPENAI_PROVIDER], Recorder(quota))
    assert llm.call_model(MESSAGES) == ("", "stub:quota")


def test_stub_meta_records_no_mode_used(monkeypatch):
    monkeypatch.setattr(llm, "PROVIDERS", [])
    _, _, meta = llm.call_model_meta(MESSAGES, mode=NON_THINKING)
    assert meta["reasoningModeRequested"] == NON_THINKING
    assert meta["reasoningModeUsed"] is None
    assert meta["degraded"] is False
    assert meta["transport"] == "stub"


# ================================================================================================
# runtime_status is additive only
# ================================================================================================

LEGACY_STATUS_KEYS = {"backend", "model", "baseUrl", "apiKeySet", "primary", "providers",
                      "lastError"}
LEGACY_PROVIDER_KEYS = {"name", "kind", "model", "baseUrl", "apiKeySet"}


def test_runtime_status_keeps_every_legacy_key(monkeypatch):
    monkeypatch.setattr(llm, "PROVIDERS", [OPENAI_PROVIDER])
    st = llm.runtime_status()
    assert LEGACY_STATUS_KEYS <= set(st)
    assert st["backend"] == "openai" and st["model"] == "qwen3-14b"
    assert st["baseUrl"] == "http://x/v1" and st["apiKeySet"] is True
    assert st["primary"] == "managed" and st["lastError"] == llm.LAST_ERROR
    assert LEGACY_PROVIDER_KEYS <= set(st["providers"][0])


def test_runtime_status_gains_the_mode_verdict_and_last_mode(monkeypatch):
    rec = use(monkeypatch, [OPENAI_PROVIDER], Recorder(_rejection(), openai_ok()))
    assert llm.runtime_status()["providers"][0]["reasoningModeSupport"] == UNKNOWN
    llm.call_model(MESSAGES, mode=NON_THINKING)
    st = llm.runtime_status()
    assert st["providers"][0]["reasoningModeSupport"] == REJECTED
    assert st["lastReasoningMode"] == THINKING
    assert st["lastReasoningModeRequested"] == NON_THINKING
    assert st["lastReasoningModeDegraded"] is True


def test_last_mode_meta_accessor(monkeypatch):
    use(monkeypatch, [OPENAI_PROVIDER], Recorder(openai_ok()))
    llm.call_model(MESSAGES, mode=NON_THINKING)
    assert llm.last_mode_meta()["reasoningModeUsed"] == NON_THINKING


# ================================================================================================
# identity_stamp
# ================================================================================================

def test_identity_stamp_returns_exactly_the_seven_contract_keys():
    """Contract §5, pinned by §9.40. The tuple is spelled out here rather than only compared against
    IDENTITY_KEYS, so editing that constant cannot quietly re-widen the shape Wave 3 Lane 3C and
    Wave 5A read."""
    stamp = identity_stamp("qwen3-14b", reasoning_mode=THINKING, temperature=0.2)
    assert tuple(stamp) == IDENTITY_KEYS
    assert tuple(stamp) == (
        "promptVersion", "schemaVersion", "toolSchemaVersion",
        "modelUsed", "reasoningMode", "quantization", "generationSettings",
    )
    assert len(stamp) == 7


def test_identity_stamp_has_one_home_for_temperature():
    """§9.40: a duplicated field drifts the first time someone writes one and not the other."""
    stamp = identity_stamp("qwen3-14b", reasoning_mode=THINKING, temperature=0.2)
    assert "temperature" not in stamp
    assert stamp["generationSettings"]["temperature"] == 0.2


def test_identity_stamp_reflects_the_degraded_outcome(monkeypatch):
    rec = use(monkeypatch, [OPENAI_PROVIDER], Recorder(_rejection(), openai_ok()))
    _, model_used, meta = llm.call_model_meta(MESSAGES, mode=NON_THINKING)
    stamp = identity_stamp(model_used, reasoning_mode=meta["reasoningModeUsed"], temperature=0.2)
    # the mode ACTUALLY used, not the one requested
    assert stamp["reasoningMode"] == THINKING
    assert meta["reasoningModeRequested"] == NON_THINKING
    assert stamp["modelUsed"] == "qwen3-14b"


def test_identity_stamp_generation_settings_contain_only_what_was_sent():
    stamp = identity_stamp("qwen3-14b", temperature=0.3)
    assert stamp["generationSettings"] == {"temperature": 0.3}
    assert "topP" not in stamp["generationSettings"]
    assert "seed" not in stamp["generationSettings"]

    nothing = identity_stamp("stub:offline")
    assert nothing["generationSettings"] == {}
    assert nothing["reasoningMode"] is None


def test_identity_stamp_keeps_stub_labels_identifiable():
    for label in ("stub:offline", "stub:quota", "qwen3-14b (fell back to stub)"):
        assert identity_stamp(label)["modelUsed"] == label


def test_identity_stamp_versions_and_quantization(monkeypatch):
    monkeypatch.delenv("AI_QUANTIZATION", raising=False)
    stamp = identity_stamp("qwen3-14b")
    assert stamp["promptVersion"] == PROMPT_VERSION
    assert stamp["schemaVersion"] == SCHEMA_VERSION
    assert stamp["toolSchemaVersion"] == TOOL_SCHEMA_VERSION
    assert stamp["quantization"] == "unknown"

    monkeypatch.setenv("AI_QUANTIZATION", "q8_0")
    assert identity_stamp("qwen3-14b")["quantization"] == "q8_0"


def test_identity_stamp_is_json_serialisable():
    json.dumps(identity_stamp("qwen3-14b", reasoning_mode=NON_THINKING, temperature=0.2))


# ================================================================================================
# The prompt-bump guard
# ================================================================================================

def test_prompt_edit_requires_a_version_bump():
    assert prompt_fingerprint() == PINNED_PROMPT_FINGERPRINT, (
        "SYSTEM, SCHEMA or REQUIRED_KEYS changed. Bump prompt.PROMPT_VERSION and/or "
        "prompt.SCHEMA_VERSION (name@n -> name@n+1) and re-pin "
        f"versioning.PINNED_PROMPT_FINGERPRINT to {prompt_fingerprint()!r} in the SAME commit."
    )


def test_the_pin_belongs_to_the_current_versions():
    assert PINNED_FOR == (PROMPT_VERSION, SCHEMA_VERSION)


def test_versions_use_the_contract_name_at_n_form():
    for v in (PROMPT_VERSION, SCHEMA_VERSION, TOOL_SCHEMA_VERSION):
        name, _, n = v.partition("@")
        assert name and n.isdigit(), v


# ================================================================================================
# ModeResult
# ================================================================================================

def test_mode_result_as_dict_uses_contract_vocabulary():
    d = ModeResult(NON_THINKING, THINKING, True, "openai", "managed").as_dict()
    assert d == {"reasoningModeRequested": NON_THINKING, "reasoningModeUsed": THINKING,
                 "degraded": True, "transport": "openai", "provider": "managed"}
