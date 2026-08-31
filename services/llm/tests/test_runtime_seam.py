"""The model runtime seam — Wave 5 Lane 5C.

WHAT THESE TESTS ARE, AND WHAT THEY ARE NOT. They assert the SHAPE of the configuration seam: that
an absent endpoint yields the stub explicitly, that a set one is honoured, that a credential is
refused rather than dropped, and that nothing anywhere invents an address. Not one of them observes
a model. `stub:offline` never licenses a claim about the model, and a green run here says the seam
behaves — never that the runtime behind it does.
"""
from __future__ import annotations

import pytest

from app import llm, runtime


# ── no default ───────────────────────────────────────────────────────────────────────────────────

def test_absent_url_yields_no_runtime_and_says_why():
    rt, refusals = runtime.configured_runtime(env={})
    assert rt is None
    assert refusals, "a None runtime with no reason reproduces the unexplained stub this seam ends"
    assert runtime.RUNTIME_URL_ENV in refusals[0]
    # The sentence must not read as an error: this is the shipped, empty-`.env`, invariant #1 state.
    assert "not an error" in refusals[0]


def test_blank_url_is_the_same_as_absent():
    rt, refusals = runtime.configured_runtime(env={runtime.RUNTIME_URL_ENV: "   "})
    assert rt is None and refusals


def test_url_without_model_is_refused_not_guessed():
    rt, refusals = runtime.configured_runtime(env={runtime.RUNTIME_URL_ENV: "http://box:8000"})
    assert rt is None
    assert runtime.RUNTIME_MODEL_ENV in refusals[0]
    assert "cannot be reproduced" in refusals[0]


def test_configured_runtime_is_keyless_and_named():
    rt, refusals = runtime.configured_runtime(env={
        runtime.RUNTIME_URL_ENV: "http://model-host:8000",
        runtime.RUNTIME_MODEL_ENV: "qwen3-14b",
    })
    assert refusals == []
    assert rt is not None
    assert rt.kind == "openai"                 # a dialect default is not an endpoint default
    assert rt.base == "http://model-host:8000/v1"
    assert rt.model == "qwen3-14b"
    assert rt.name == "openai@model-host:8000"  # derived from what was typed, not defaulted
    assert rt.as_provider()["key"] == ""        # keyless, by contract


def test_explicit_name_and_ollama_kind_are_honoured():
    rt, _ = runtime.configured_runtime(env={
        runtime.RUNTIME_URL_ENV: "http://box:11434/",
        runtime.RUNTIME_MODEL_ENV: "qwen3:14b",
        runtime.RUNTIME_KIND_ENV: "OLLAMA",
        runtime.RUNTIME_NAME_ENV: "prod-a",
    })
    assert rt is not None
    assert rt.kind == "ollama"
    assert rt.base == "http://box:11434"        # no /v1 on the native dialect
    assert rt.name == "prod-a"


def test_unknown_kind_is_refused_rather_than_coerced():
    rt, refusals = runtime.configured_runtime(env={
        runtime.RUNTIME_URL_ENV: "http://box:8000",
        runtime.RUNTIME_MODEL_ENV: "m",
        runtime.RUNTIME_KIND_ENV: "vllm",
    })
    assert rt is None
    assert "not one of" in refusals[0]


# ── keyless is a contract, not a convenience ─────────────────────────────────────────────────────

def test_a_credential_refuses_the_runtime_it_does_not_silently_drop_it():
    """A runtime that requires a credential is a change to invariant #1 and needs its own
    amendment. Ignoring the variable would send unauthenticated traffic an operator believes is
    authenticated — worse than refusing, because it looks like it worked."""
    rt, refusals = runtime.configured_runtime(env={
        runtime.RUNTIME_URL_ENV: "http://box:8000",
        runtime.RUNTIME_MODEL_ENV: "qwen3-14b",
        runtime.RUNTIME_FORBIDDEN_KEY_ENV: "sk-something",
    })
    assert rt is None
    assert "KEYLESS" in refusals[0]
    assert "invariant #1" in refusals[0]


# ── the provider chain invents no address ────────────────────────────────────────────────────────

@pytest.mark.parametrize("env", [
    {},                                     # nothing at all
    {"LLM_BACKEND": "ollama"},              # the old silent http://localhost:11434 path
    {"LLM_BACKEND": "openai"},              # the old silent http://localhost:1234/v1 path
    {"LLM_BACKEND": "openai", "LLM_API_KEY": "k"},   # a key but no address
])
def test_empty_environment_builds_an_empty_provider_chain(monkeypatch, env):
    """The heart of the clause. Before Wave 5C, `LLM_BACKEND=ollama` with an empty `.env` produced
    a provider pointed at `http://localhost:11434` — a socket nobody asked it to open, and a run
    that could not afterwards be told apart from one the stub answered."""
    for key in ("MODEL_RUNTIME_URL", "MODEL_RUNTIME_MODEL", "MODEL_RUNTIME_KIND",
                "MODEL_RUNTIME_NAME", "MODEL_RUNTIME_API_KEY", "AI_BASE_URL", "AI_API_KEY",
                "LLM_BACKEND", "LLM_BASE_URL", "LLM_API_KEY", "OLLAMA_URL"):
        monkeypatch.delenv(key, raising=False)
    for key, value in env.items():
        monkeypatch.setenv(key, value)
    monkeypatch.setattr(llm, "RUNTIME", None)
    assert llm._build_providers() == []


def test_no_source_file_contains_a_default_model_endpoint():
    """A source-level assertion, because the defaults were spread across four files and a code
    review is a false-negative detector (§9.44). `localhost:11434` / `localhost:1234` may appear in
    a COMMENT documenting what an operator would type; they may not appear inside `os.getenv`."""
    import inspect
    import re

    source = inspect.getsource(llm._build_providers)
    for call in re.findall(r"os\.getenv\([^)]*\)", source):
        assert "http://" not in call, f"default endpoint reintroduced in: {call}"


def test_configured_runtime_leads_the_provider_chain(monkeypatch):
    rt = runtime.Runtime(name="prod-a", kind="openai", base="http://box:8000/v1", model="qwen3-14b")
    monkeypatch.setattr(llm, "RUNTIME", rt)
    monkeypatch.setenv("AI_BASE_URL", "http://legacy:9/v1")
    monkeypatch.setenv("AI_API_KEY", "k")
    chain = llm._build_providers()
    assert chain[0]["name"] == "prod-a"
    assert chain[0]["key"] == ""
    assert [p["name"] for p in chain][1:2] == ["managed"]


# ── the identity stamp names the runtime, and does not grow an eighth key ────────────────────────

def test_descriptor_names_the_runtime_that_actually_served():
    rt = runtime.Runtime(name="prod-a", kind="openai", base="http://box:8000/v1", model="qwen3-14b")
    assert runtime.descriptor(rt, "qwen3-14b")["served"] == "prod-a"
    # Configured but the stub answered: the two differ exactly when a reader most needs to see it.
    assert runtime.descriptor(rt, "stub:offline")["served"] == runtime.STUB_RUNTIME
    assert runtime.descriptor(rt, "stub:offline")["isStub"] is True
    assert runtime.descriptor(None, None)["configured"] is False


def test_descriptor_withholds_the_endpoint_from_the_browser_but_health_carries_it():
    rt = runtime.Runtime(name="prod-a", kind="openai", base="http://internal:8000/v1", model="m")
    assert "baseUrl" not in runtime.descriptor(rt, "m")
    assert runtime.runtime_report(rt, [])["baseUrl"] == "http://internal:8000/v1"


def test_is_stub_covers_every_stub_label():
    assert runtime.is_stub("stub:offline") and runtime.is_stub("stub:quota")
    assert runtime.is_stub("qwen3-14b (fell back to stub)") is False  # a real call that was discarded
    assert not runtime.is_stub("qwen3-14b")


def test_identity_stamp_still_has_exactly_seven_keys(monkeypatch):
    """§9.40. The runtime travels BESIDE identity; an eighth key here would be a contract violation
    dressed up as an audit improvement."""
    from app.versioning import IDENTITY_KEYS, identity_stamp

    rt = runtime.Runtime(name="prod-a", kind="openai", base="http://box:8000/v1", model="qwen3-14b")
    monkeypatch.setattr(llm, "RUNTIME", rt)
    stamp = identity_stamp("qwen3-14b")
    assert tuple(stamp.keys()) == IDENTITY_KEYS
    assert len(IDENTITY_KEYS) == 7
    assert "runtime" not in stamp


def test_health_reports_the_refusal_sentence(monkeypatch):
    monkeypatch.setattr(llm, "RUNTIME", None)
    monkeypatch.setattr(llm, "RUNTIME_REFUSALS", ["MODEL_RUNTIME_URL is unset"])
    status = llm.runtime_status()
    assert status["runtime"]["configured"] is False
    assert status["runtime"]["keyless"] is True
    assert status["runtime"]["refusals"] == ["MODEL_RUNTIME_URL is unset"]
    # Additive only: every pre-Wave-5 key keeps its name.
    for key in ("backend", "model", "baseUrl", "apiKeySet", "primary", "providers", "lastError"):
        assert key in status


# ── Phase 5 preflight: the two seams can disagree, and that must be visible ───────────────────────
#
# Found by executing the preflight rather than by reading the code: a developer machine had
# MODEL_RUNTIME_URL unset (so `runtime.configured` was false and `runtime.served` reported
# `stub:offline`) while a `services/llm/.env` configured the legacy chain against a keyed
# third-party endpoint that would really have answered. An envelope produced by a real but
# UNDECLARED model was therefore labelled as a fixture.
#
# `served` is deliberately NOT changed — see the long comment on `llm.runtime_warnings`. These tests
# pin the reporting instead, and pin the fail-safe direction of the mislabel so a future change
# cannot quietly make it fail-open.


def _reload_llm(monkeypatch, tmp_path, **env):
    """Reload `app.llm` against a controlled environment.

    `monkeypatch.chdir` is load-bearing, not tidiness: `llm._load_dotenv()` runs at IMPORT time and
    reads `services/llm/.env` relative to the working directory, so a reload performed in the
    service directory silently reinstates whatever a developer has in that file — which is exactly
    the configuration these tests exist to describe, and would make every one of them lie.
    """
    import importlib
    from app import llm as llm_module

    monkeypatch.chdir(tmp_path)
    for name in ("MODEL_RUNTIME_URL", "MODEL_RUNTIME_MODEL", "MODEL_RUNTIME_KIND",
                 "MODEL_RUNTIME_NAME", "MODEL_RUNTIME_API_KEY",
                 "LLM_BACKEND", "LLM_BASE_URL", "LLM_API_KEY", "LLM_MODEL",
                 "AI_BASE_URL", "AI_API_KEY", "AI_MODEL", "OLLAMA_URL"):
        monkeypatch.delenv(name, raising=False)
    for name, value in env.items():
        monkeypatch.setenv(name, value)
    return importlib.reload(llm_module)


def test_no_warning_when_nothing_at_all_is_configured(monkeypatch, tmp_path):
    """A genuinely offline deployment is a healthy state, not a discrepancy."""
    llm_module = _reload_llm(monkeypatch, tmp_path)
    assert llm_module.runtime_warnings() == []


def test_no_warning_when_the_runtime_is_properly_declared(monkeypatch, tmp_path):
    llm_module = _reload_llm(
        monkeypatch, tmp_path,
        MODEL_RUNTIME_URL="http://model-host:8000",
        MODEL_RUNTIME_MODEL="qwen3-14b",
        MODEL_RUNTIME_KIND="openai",
    )
    assert llm_module.runtime_warnings() == []


def test_an_undeclared_legacy_provider_is_reported(monkeypatch, tmp_path):
    llm_module = _reload_llm(
        monkeypatch, tmp_path,
        LLM_BACKEND="openai",
        LLM_BASE_URL="https://example.invalid/v1",
        LLM_API_KEY="not-a-real-key",
        LLM_MODEL="some-other-model",
    )
    warnings = llm_module.runtime_warnings()
    assert len(warnings) == 1
    assert "some-other-model" in warnings[0]
    assert "MODEL_RUNTIME_URL" in warnings[0]
    # The warning must not leak the key.
    assert "not-a-real-key" not in warnings[0]


def test_the_warning_reaches_health(monkeypatch, tmp_path):
    llm_module = _reload_llm(
        monkeypatch, tmp_path,
        LLM_BACKEND="openai",
        LLM_BASE_URL="https://example.invalid/v1",
        LLM_API_KEY="not-a-real-key",
        LLM_MODEL="some-other-model",
    )
    status = llm_module.runtime_status()
    assert status["runtime"]["configured"] is False
    assert len(status["runtimeWarnings"]) == 1


def test_the_mislabel_stays_fail_safe_so_the_gates_keep_refusing(monkeypatch, tmp_path):
    """The load-bearing property.

    With no declared runtime, `served` must remain `stub:offline` even for a non-stub answer —
    because `ablation.py` and `app.smoke` refuse a stub, and that refusal is what stops an
    undeclared model producing a verdict. A future change that made `served` name the undeclared
    model would turn a fail-safe mislabel into a fail-open one, and must fail here first.
    """
    llm_module = _reload_llm(
        monkeypatch, tmp_path,
        LLM_BACKEND="openai",
        LLM_BASE_URL="https://example.invalid/v1",
        LLM_API_KEY="not-a-real-key",
        LLM_MODEL="some-other-model",
    )
    block = llm_module.runtime_block("some-other-model")
    assert block["configured"] is False
    assert block["served"] == "stub:offline"
    assert block["isStub"] is True
