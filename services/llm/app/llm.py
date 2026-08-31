"""Model layer (original doc §3.4). Asks a local model for a parseable JSON object (Phase 3),
one model at a time. Two backends are supported so you can serve the model however you like:

  LLM_BACKEND=openai   (code default) -> any OpenAI-compatible server (a managed Qwen endpoint, LM Studio)
                                         /chat/completions with response_format={"type":"json_object"}
  LLM_BACKEND=ollama                  -> Ollama native /api/chat, format="json"

If the server is unreachable, returns a deterministic structured stub built from the same computed
facts, so the pipeline stays whole.

Wave 1 Lane 1D adds **reasoning-mode control** on both transports (`modes.py`):

  * `non_thinking` is requested as a transport parameter — `chat_template_kwargs:
    {"enable_thinking": false}` on OpenAI-compatible servers, `think: false` on Ollama — never as
    prompt text.
  * A server that rejects the field is retried **once** without it and the call continues with
    thinking left on. A rejection never becomes a stub: the model answered, and pretending
    otherwise corrupts both the audit trail and the offline-state UX. The degradation is recorded
    (`reasoningModeRequested` / `reasoningModeUsed` / `degraded`) and the per-provider verdict is
    cached so one rejection does not cost every later call a round trip.
  * A transport error that is NOT a field rejection falls through to the next provider and then to
    the stub, exactly as before. A 500 or a timeout is not a mode question.
  * `<think>…</think>` is stripped from every raw response here, so every caller — /read, /brief,
    /assistant, /chat, the committee, the transcript analyser — benefits without knowing about it.

`call_model` / `call_model_text` keep their exact signatures and 2-tuple returns; `mode` is
keyword-only and defaults to None, which sends nothing and reproduces the old behaviour byte for
byte. `call_model_meta` is the 3-tuple variant, and `last_mode_meta()` lets a persistence layer
stamp the mode without any signature change.
"""
from __future__ import annotations

import copy
import json
import logging
import os
import threading
from datetime import datetime, timezone

import requests

from .runtime import (
    STUB_RUNTIME,
    Runtime,
    configured_runtime,
    descriptor as runtime_descriptor,
    is_stub,
    runtime_report,
    served_runtime,
)
from .modes import (
    NON_THINKING,
    REJECTED,
    SUPPORTED,
    THINKING,
    TRANSPORT_OLLAMA,
    TRANSPORT_OLLAMA_NO_THINK,
    TRANSPORT_OPENAI,
    TRANSPORT_STUB,
    UNKNOWN,
    ModeResult,
    strip_thinking,
)

log = logging.getLogger("llm")


def _load_dotenv(path: str = ".env") -> None:
    """Minimal .env loader for local `uvicorn` runs (docker compose injects env itself; this fills
    the gap where services/llm/.env existed but nothing read it). Never overrides variables that
    are already set in the environment. No python-dotenv dependency on purpose."""
    try:
        with open(path) as fh:
            for line in fh:
                line = line.strip()
                if not line or line.startswith("#") or "=" not in line:
                    continue
                k, v = line.split("=", 1)
                k, v = k.strip(), v.strip().strip("'").strip('"')
                if k and k not in os.environ:
                    os.environ[k] = v
    except OSError:
        pass


_load_dotenv()  # cwd is services/llm when started per the README/CLAUDE.md commands

# --- provider chain (tried in order; first success wins, else the deterministic stub) -----------
# 0) THE CONFIGURED RUNTIME (Wave 5C, `runtime.py`) — a self-hosted, KEYLESS endpoint named by
#    MODEL_RUNTIME_URL + MODEL_RUNTIME_MODEL and read with NO DEFAULT. Absent ⇒ absent from the
#    chain, and this process serves `stub:offline` explicitly. Tried FIRST because it is the only
#    entry an operator declared on purpose; everything below it is legacy convenience.
# 1) a managed OpenAI-compatible endpoint — creds auto-injected as AI_BASE_URL / AI_API_KEY.
# 2) explicit LLM_* backend (LM Studio / Ollama) as a fallback. Old OLLAMA_* vars still work.
#    Keys ONLY ever come from the environment.
#
# NO ENDPOINT IN THIS FUNCTION HAS A DEFAULT ANY MORE — that is the Wave 5C change, and it is the
# whole of the "configuration has no default" clause in code. Two defaults were removed:
# `http://localhost:1234/v1` for the OpenAI dialect and `http://localhost:11434` for Ollama. Each
# meant an unconfigured process could open a socket nobody asked it to open, and — worse — made a
# run that reached some other machine's model indistinguishable after the fact from a run that fell
# back to the stub. An absent variable now REMOVES the provider from the chain. It never invents an
# address for it.

def _build_providers() -> list[dict]:
    provs: list[dict] = []

    # (0) the configured runtime. `RUNTIME_REFUSALS` carries the sentence saying why, when there is
    #     none — an unexplained stub is the state this seam exists to end (§9.44).
    if RUNTIME is not None:
        provs.append(RUNTIME.as_provider())

    # (1) the managed model. AI_BASE_URL is the root; the OpenAI path is /v1/chat/...,
    #     so ensure a /v1 suffix before we append /chat/completions.
    ai_base = os.getenv("AI_BASE_URL", "").strip().rstrip("/")
    ai_key = os.getenv("AI_API_KEY", "").strip()
    if ai_base and ai_key:
        if not ai_base.endswith("/v1"):
            ai_base += "/v1"
        provs.append({
            "name": "managed",
            "kind": "openai",
            "base": ai_base,
            "key": ai_key,
            "model": os.getenv("AI_MODEL", "qwen3-14b"),
        })

    # (2) explicit LLM_* backend (fallback). openai => LM Studio / any OpenAI-compatible server;
    #     else Ollama. The MODEL name still falls back through AI_MODEL — a model id is not an
    #     endpoint, and defaulting it opens no socket. The BASE URL does not fall back to anything.
    backend = os.getenv("LLM_BACKEND", "openai").lower()
    if backend == "openai":
        base = (os.getenv("LLM_BASE_URL") or os.getenv("AI_BASE_URL") or "").strip().rstrip("/")
        if base and not base.endswith("/v1"):
            base += "/v1"
        key = (os.getenv("LLM_API_KEY") or os.getenv("AI_API_KEY") or "").strip()
        # `base and key`: a keyless hosted endpoint would only 401, and an unset base has no address
        # to try. A keyless SELF-HOSTED endpoint belongs in provider (0), which is exactly what that
        # provider was added for.
        if base and key:
            provs.append({
                "name": "llm",
                "kind": "openai",
                "base": base,
                "key": key,
                "model": os.getenv("LLM_MODEL") or os.getenv("AI_MODEL") or "qwen3-14b",
            })
    else:
        base = (os.getenv("LLM_BASE_URL") or os.getenv("OLLAMA_URL") or "").strip().rstrip("/")
        if base:
            provs.append({
                "name": "ollama",
                "kind": "ollama",
                "base": base,
                "key": "",
                "model": os.getenv("LLM_MODEL") or os.getenv("OLLAMA_MODEL", "qwen2.5:7b"),
            })
    return provs


# Read ONCE, at import. Frozen for the process's life so a request cannot be served by a runtime
# that a later `os.environ` mutation invented.
RUNTIME, RUNTIME_REFUSALS = configured_runtime()

PROVIDERS = _build_providers()
if not PROVIDERS:
    # Not an error — this is the shipped, keyless, empty-`.env` state (invariant #1). It is logged
    # at WARNING with the REASON attached, because "why is everything a stub" must be answerable
    # from the log without reading this file.
    log.warning("no model provider configured — every read/chat will serve the offline stub. %s",
                " ".join(RUNTIME_REFUSALS) or "No MODEL_RUNTIME_URL, AI_* creds or LLM_API_KEY.")
elif RUNTIME is not None:
    log.info("model runtime configured: name=%s kind=%s model=%s base=%s (keyless)",
             RUNTIME.name, RUNTIME.kind, RUNTIME.model, RUNTIME.base)

# The most recent model-call failure, exposed via /health so "assistant offline" is diagnosable
# instead of silent. None = no failure since startup.
LAST_ERROR: dict | None = None

# --- reasoning-mode support cache ---------------------------------------------------------------
# Per-provider verdict on the mode transport parameter: 'unknown' | 'supported' | 'rejected'.
# Cached so ONE rejection does not cost every subsequent call an extra round trip. Keyed by the
# provider-chain name, because two providers can be different servers with different builds.
MODE_SUPPORT: dict[str, str] = {}
# Verbatim server complaint per provider, for the handoff / diagnosis. Never surfaced as an error:
# the call succeeded on the retry.
MODE_REJECTION: dict[str, str] = {}
# Ollama-only: whether the documented `/no_think` prompt suffix had to be used for this provider.
NO_THINK_SUFFIX_USED: dict[str, bool] = {}

# The metadata of the last model call, so a persistence layer can stamp the mode without any
# signature change (store.py's eventual one-line change reads this). Thread-local because FastAPI
# runs sync endpoints in a threadpool: the handler, its model call and its save_read() all run on
# one thread, so a thread-local is exactly the right scope. The module-level fallback exists for
# callers on a thread that never made a call.
_LAST_META = threading.local()
LAST_MODE_META: dict | None = None

# Appended to the last user message on an old Ollama build that understands neither `think: false`
# nor `chat_template_kwargs`. Prompt-level, last resort, and it NEVER reaches a persisted artefact:
# it is applied to a copy of the message list and only the model's reply is returned.
NO_THINK_SUFFIX = " /no_think"


def _record_error(e: Exception, provider: dict | None = None) -> None:
    global LAST_ERROR
    provider = provider or {}
    detail = f"{type(e).__name__}: {e}"
    resp = getattr(e, "response", None)  # requests.HTTPError: include the server's error body
    if resp is not None:
        try:
            detail += " | body: " + resp.text[:500]
        except Exception:  # noqa: BLE001
            pass
    LAST_ERROR = {
        "at": datetime.now(timezone.utc).isoformat(),
        "provider": provider.get("name"),
        "model": provider.get("model"),
        "baseUrl": provider.get("base"),
        "error": detail[:800],
    }
    log.warning("model call failed via %s (trying next provider / stub): %s",
                provider.get("name", "?"), detail)


def _stub_label(e: Exception | None) -> str:
    """'stub:quota' for HTTP 429 (a key WORKS, the free-tier quota is exhausted — a different
    user-facing state than 'offline'), 'stub:offline' for everything else."""
    resp = getattr(e, "response", None)
    if resp is not None and getattr(resp, "status_code", None) == 429:
        return "stub:quota"
    return "stub:offline"


def runtime_status() -> dict:
    """What /health reports: the provider chain (keys never leaked) + the last error. Legacy
    top-level fields mirror the PRIMARY provider so older consumers keep working.

    Wave 1 Lane 1D extends this ADDITIVELY only: every pre-existing key keeps its exact name and
    meaning (the gateway and /health consumers read them). The new keys are the per-provider
    reasoning-mode support verdict and the last mode actually used."""
    primary = PROVIDERS[0] if PROVIDERS else {}
    last = last_mode_meta() or {}
    return {
        "backend": primary.get("kind"),
        "model": primary.get("model"),
        "baseUrl": primary.get("base"),
        "apiKeySet": bool(primary.get("key")) or primary.get("kind") == "ollama",
        "primary": primary.get("name"),
        "providers": [
            {"name": p["name"], "kind": p["kind"], "model": p["model"],
             "baseUrl": p["base"], "apiKeySet": bool(p["key"]) or p["kind"] == "ollama",
             # additive: 'unknown' | 'supported' | 'rejected'
             "reasoningModeSupport": MODE_SUPPORT.get(p["name"], UNKNOWN),
             "noThinkSuffixUsed": NO_THINK_SUFFIX_USED.get(p["name"], False)}
            for p in PROVIDERS
        ],
        "lastError": LAST_ERROR,
        # additive: what the last generation actually ran in, and whether it was degraded.
        "lastReasoningMode": last.get("reasoningModeUsed"),
        "lastReasoningModeRequested": last.get("reasoningModeRequested"),
        "lastReasoningModeDegraded": bool(last.get("degraded", False)),
        # additive (Wave 5C): the configured runtime and, when there is none, the SENTENCE saying
        # why. `refusals` is the observable half of "absent ⇒ stub:offline, explicitly".
        "runtime": runtime_report(RUNTIME, RUNTIME_REFUSALS),
        # additive (Phase 5 preflight): the discrepancy between the two seams, named out loud.
        "runtimeWarnings": runtime_warnings(),
    }


# A CONFIGURATION THIS SERVICE CAN BE IN, AND COULD NOT PREVIOUSLY REPORT
# -----------------------------------------------------------------------
# `MODEL_RUNTIME_*` (Wave 5C, §9.64) and the legacy `LLM_*` / `AI_*` / `OLLAMA_*` chain are two
# different seams, and they can disagree. Measured on a developer machine during the Phase 5
# preflight, with `MODEL_RUNTIME_URL` unset but a `services/llm/.env` present:
#
#     /health.llm.runtime.configured        false
#     /health.llm.providers[0]              a keyed third-party OpenAI-compatible endpoint
#     runtime_block("<that model>").served  "stub:offline"      <-- and a real model answered
#
# `served_runtime` returns the stub whenever `RUNTIME is None`, so an envelope produced by a real
# but UNDECLARED model is labelled as a fixture. §9.65 says `stub:offline` means "a fixture produced
# everything in this envelope"; in this configuration that label is false.
#
# WHY THE LABEL IS NOT CHANGED HERE. It errs toward refusal: `ablation.py` and `app.smoke` both
# refuse a stub, so a ladder run in this state is REFUSED rather than accepted — which is the safe
# direction and exactly what happened during the preflight. Making `served` name the undeclared
# model would make those gates stop refusing, turning a fail-safe mislabel into a fail-open one.
# Correcting the semantics properly is a §9.40/§9.65 amendment, not a patch.
#
# So this reports it instead. An operator polling /health can now SEE the discrepancy, which is
# §9.44's rule applied to the one question Phase 5 exists to answer: which model actually served us.
def runtime_warnings() -> list[str]:
    """Discrepancies between the declared runtime and the chain that would actually answer.

    Empty in both healthy states — a properly configured `MODEL_RUNTIME_*`, and a genuinely
    offline deployment with no provider at all.
    """
    warnings: list[str] = []
    if RUNTIME is not None:
        return warnings

    live = [
        p for p in PROVIDERS
        if p.get("base") and (bool(p.get("key")) or p.get("kind") == "ollama")
    ]
    if not live:
        return warnings

    named = ", ".join(f'{p["name"]}:{p["model"]}' for p in live)
    warnings.append(
        "NO MODEL RUNTIME IS DECLARED (MODEL_RUNTIME_URL is unset), but the legacy provider chain "
        f"is configured and would answer: {named}. Generations will therefore be served by a model "
        "this deployment has not declared, while `runtime.served` reports `stub:offline` — the "
        "label is wrong in that state. The ablation ladder and `make smoke-model` both refuse a "
        "stub, so no verdict can be produced from it; but no claim about model behaviour may cite "
        "any output produced in this configuration either. Declare MODEL_RUNTIME_URL and "
        "MODEL_RUNTIME_MODEL, or clear the legacy LLM_*/AI_*/OLLAMA_* variables."
    )
    return warnings


def runtime_block(model_used=None) -> dict:
    """The `runtime` descriptor an artefact carries BESIDE its `identity` stamp.

    Never inside `identity`: §9.40 pins that block at exactly seven keys, and an eighth would be a
    contract violation dressed up as an audit improvement. `identity["modelUsed"]` already separates
    `stub:offline` from a served model; this says WHICH runtime the served one was.
    """
    return runtime_descriptor(RUNTIME, model_used)


def _post_openai(p: dict, messages: list[dict], json_mode: bool, *, no_think: bool = False) -> str:
    body: dict = {
        "model": p["model"],
        "messages": messages,
        "temperature": 0.2 if json_mode else 0.3,
        "stream": False,
    }
    if json_mode:
        body["response_format"] = {"type": "json_object"}  # constrain to valid JSON
    if no_think:
        # Qwen3 chat-template switch on OpenAI-compatible servers (vLLM / SGLang / LM
        # Studio). Absent unless non_thinking was explicitly requested, so the default body is
        # byte-identical to the pre-Wave-1 one.
        body["chat_template_kwargs"] = {"enable_thinking": False}
    r = requests.post(
        f'{p["base"]}/chat/completions',
        headers={"Authorization": f'Bearer {p["key"]}', "Content-Type": "application/json"},
        json=body,
        timeout=120,
    )
    r.raise_for_status()
    return r.json()["choices"][0]["message"]["content"]


def _post_ollama(p: dict, messages: list[dict], json_mode: bool, *, no_think: bool = False) -> str:
    body: dict = {
        "model": p["model"],
        "messages": messages,
        "stream": False,
        "options": {"temperature": 0.2 if json_mode else 0.3},
    }
    if json_mode:
        body["format"] = "json"  # constrain Ollama to valid JSON
    if no_think:
        body["think"] = False  # Ollama's native thinking switch — TOP level, not under options
    r = requests.post(f'{p["base"]}/api/chat', json=body, timeout=120)
    r.raise_for_status()
    return r.json()["message"]["content"]


# --- mode negotiation ----------------------------------------------------------------------------

_FIELD_TOKENS = ("chat_template_kwargs", "enable_thinking", "think")
_OLD_BUILD_TOKENS = ("think", "unknown field", "unsupported", "unrecognized", "unexpected",
                     "invalid", "not supported")


def _status_of(e: Exception) -> int | None:
    resp = getattr(e, "response", None)
    return getattr(resp, "status_code", None) if resp is not None else None


def _body_of(e: Exception) -> str:
    resp = getattr(e, "response", None)
    if resp is None:
        return ""
    try:
        return (resp.text or "")[:2000].lower()
    except Exception:  # noqa: BLE001 — a body we cannot read is simply not evidence
        return ""


def _is_field_rejection(e: Exception) -> bool:
    """True when the server refused specifically because of the reasoning-mode field.

    Deliberately narrow: a 4xx that NAMES the field, or any error body that names it. A 500, a
    timeout or a connection error is a transport failure and must fall through to the next provider
    — turning one of those into a mode question would mask a real outage.
    """
    status = _status_of(e)
    body = _body_of(e)
    named = any(tok in body for tok in _FIELD_TOKENS)
    if named:
        return True
    return status in (400, 422)


def _suggests_old_build(e: Exception) -> bool:
    """Ollama only: the no-field retry ALSO failed in a way consistent with an old build that wants
    the documented `/no_think` prompt suffix instead of a request field."""
    status = _status_of(e)
    if status is not None and status >= 500:
        return False
    body = _body_of(e)
    return status in (400, 404, 422) or any(tok in body for tok in _OLD_BUILD_TOKENS)


def _with_no_think_suffix(messages: list[dict]) -> list[dict]:
    """A COPY of `messages` with `/no_think` appended to the last user message. The caller's list is
    never mutated and the suffix never leaves this module."""
    out = copy.deepcopy(list(messages))
    for m in reversed(out):
        if m.get("role") == "user":
            m["content"] = f'{m.get("content", "")}{NO_THINK_SUFFIX}'
            return out
    if out:
        out[-1]["content"] = f'{out[-1].get("content", "")}{NO_THINK_SUFFIX}'
    return out


def _call_provider(p: dict, messages: list[dict], json_mode: bool,
                   mode: str | None) -> tuple[str, ModeResult]:
    """One provider, with mode negotiation. Returns (raw, ModeResult) or raises the transport error
    so the caller can move on to the next provider."""
    name = p["name"]
    transport = TRANSPORT_OPENAI if p["kind"] == "openai" else TRANSPORT_OLLAMA
    post = _post_openai if p["kind"] == "openai" else _post_ollama

    wants_field = mode == NON_THINKING
    verdict = MODE_SUPPORT.get(name, UNKNOWN)
    # A cached 'rejected' verdict means we already know the field will fail — do not pay for the
    # round trip again, and record the degradation up front.
    send_field = wants_field and verdict != REJECTED

    def result(used: str | None, degraded: bool, tr: str = transport) -> ModeResult:
        return ModeResult(reasoningModeRequested=mode, reasoningModeUsed=used,
                          degraded=degraded, transport=tr, provider=name)

    if wants_field and not send_field:
        # Known-rejected: one attempt, no field, honestly recorded as thinking.
        raw = post(p, messages, json_mode)
        return raw, result(THINKING, True)

    try:
        raw = post(p, messages, json_mode, no_think=send_field)
        if send_field:
            MODE_SUPPORT[name] = SUPPORTED
            return raw, result(NON_THINKING, False)
        # No mode requested, or `thinking` requested: we sent nothing, so the server's default is
        # what ran. `thinking` is Qwen3's default, so a `thinking` request is satisfied as-is.
        return raw, result(THINKING if mode == THINKING else None, False)
    except Exception as e:  # noqa: BLE001
        if not send_field or not _is_field_rejection(e):
            raise  # a real transport failure — next provider, then the stub

        MODE_SUPPORT[name] = REJECTED
        MODE_REJECTION[name] = f"{type(e).__name__}: {e} | body: {_body_of(e)[:300]}"
        log.warning("provider %s rejected the reasoning-mode field; retrying without it "
                    "(thinking stays ON, degraded=True): %s", name, MODE_REJECTION[name])

        # (2) retry once WITHOUT the field. A rejection must never become a stub.
        try:
            raw = post(p, messages, json_mode)
            return raw, result(THINKING, True)
        except Exception as e2:  # noqa: BLE001
            # (3) Ollama only: an old build may want the documented prompt suffix instead.
            if p["kind"] == "ollama" and _suggests_old_build(e2):
                raw = post(p, _with_no_think_suffix(messages), json_mode)
                NO_THINK_SUFFIX_USED[name] = True
                log.warning("provider %s: fell back to the /no_think prompt suffix", name)
                return raw, result(NON_THINKING, True, TRANSPORT_OLLAMA_NO_THINK)
            raise


def _remember_meta(meta: ModeResult) -> None:
    global LAST_MODE_META
    d = meta.as_dict()
    _LAST_META.value = d
    LAST_MODE_META = d


def last_mode_meta() -> dict | None:
    """Metadata of the last model call made on this thread (falling back to the last call made by
    any thread). `store.py` reads this to stamp a persisted artefact without a signature change."""
    return getattr(_LAST_META, "value", None) or LAST_MODE_META


def _run_meta(messages: list[dict], json_mode: bool,
              mode: str | None = None) -> tuple[str, str, dict]:
    """Try each provider in order; the first success wins. On total failure return ('', 'stub:*')
    and the caller falls back to the deterministic stub."""
    last_exc: Exception | None = None
    for p in PROVIDERS:
        try:
            raw, meta = _call_provider(p, messages, json_mode, mode)
            # Strip <think> INSIDE llm.py so a thinking block can never reach safe_json, a chat
            # reply or a persisted read. `.strip()` first keeps the tagless path byte-identical to
            # the pre-Wave-1 return value.
            text = strip_thinking((raw or "").strip())
            _remember_meta(meta)
            return text, p["model"], meta.as_dict()
        except Exception as e:  # noqa: BLE001 — record and try the next provider
            last_exc = e
            _record_error(e, p)
            continue
    # No generation happened, so no mode ran: `reasoningModeUsed` is None and `degraded` is False
    # (mode degradation is a different failure from provider unavailability).
    meta = ModeResult(reasoningModeRequested=mode, transport=TRANSPORT_STUB)
    _remember_meta(meta)
    return "", _stub_label(last_exc), meta.as_dict()


def _run(messages: list[dict], json_mode: bool, mode: str | None = None) -> tuple[str, str]:
    raw, model_used, _ = _run_meta(messages, json_mode, mode)
    return raw, model_used


def call_model(messages: list[dict], *, mode: str | None = None) -> tuple[str, str]:
    """Returns (raw_text, model_used). Raw text is expected to be a JSON string. model_used is the
    winning provider's model, or 'stub:offline'/'stub:quota' when every provider is unavailable.

    `mode` is keyword-only with a default of None = "send nothing, leave the server's default",
    which reproduces the pre-Wave-1 request body byte for byte. The signature stays callable with
    exactly one positional argument and keeps returning a 2-tuple, because `call_model` is passed
    around AS A CALLABLE (`analyze_transcript(..., call_model, safe_json)`, the committee, and every
    research module)."""
    return _run(messages, json_mode=True, mode=mode)


def call_model_text(messages: list[dict], *, mode: str | None = None) -> tuple[str, str]:
    """Like call_model but for FREE-TEXT replies (chat) — no JSON constraint."""
    return _run(messages, json_mode=False, mode=mode)


def call_model_meta(messages: list[dict], *, json_mode: bool = True,
                    mode: str | None = None) -> tuple[str, str, dict]:
    """The 3-tuple variant for callers that need the mode metadata: (raw, model_used, meta), where
    `meta` is `ModeResult.as_dict()`. Same transport behaviour as `call_model`."""
    return _run_meta(messages, json_mode, mode)


def safe_json(raw: str) -> dict | None:
    if not raw:
        return None
    try:
        obj = json.loads(raw)
        return obj if isinstance(obj, dict) else None
    except json.JSONDecodeError:
        # tolerate a JSON object embedded in extra text
        start, end = raw.find("{"), raw.rfind("}")
        if 0 <= start < end:
            try:
                return json.loads(raw[start : end + 1])
            except json.JSONDecodeError:
                return None
        return None


def stub_structured(facts: dict, confluence: dict | None = None) -> dict:
    """Deterministic structured read from the computed regime facts (no model). When multi-timeframe
    confluence is supplied it is woven in, so even the offline stub references cross-timeframe
    agreement/conflict — descriptive only, never a buy/sell call."""
    kl = facts.get("keyLevels", {}) or {}
    bull = [
        f"Trend reads as {facts.get('trend', 'n/a')}; {facts.get('shortTermTrend', 'n/a')}.",
        f"Trend strength {facts.get('trendStrength', 'n/a')}; MACD: {facts.get('macdState', 'n/a')}.",
        f"Volume {facts.get('volume', 'n/a')} ({facts.get('volumeTrend', 'n/a')}).",
    ]
    bear = [
        f"Momentum is {facts.get('momentum', 'n/a')}; stochastic {facts.get('stochastic', 'n/a')}.",
        f"Volatility {facts.get('volatility', 'n/a')}; {facts.get('priceVsBands', 'n/a')}.",
    ]
    vwap_state = facts.get("vwapState")
    if vwap_state:
        bear.append(f"Intraday location: {vwap_state}.")

    summary = (
        f"Directional context only — the attestel is {facts.get('trend', 'n/a')} with "
        f"{facts.get('momentum', 'n/a')} momentum. This is evidence, not a signal; the "
        "decision stays with you."
    )

    if confluence:
        alignment = confluence.get("trendAlignment", "n/a")
        conflicts = confluence.get("conflicts") or []
        bull.append(f"Cross-timeframe trend is {alignment} (1D/1H/15m).")
        if conflicts:
            bear.append("Timeframe conflict: " + "; ".join(conflicts) + ".")
        else:
            bear.append("No cross-timeframe conflicts — the timeframes agree on trend.")
        note = confluence.get("note")
        if note:
            summary += f" Across timeframes: {note}"

    return {
        "bullCase": bull,
        "bearCase": bear,
        "keyLevels": {"support": kl.get("support", []), "resistance": kl.get("resistance", [])},
        "invalidation": {
            "bull": "A daily close back below the nearest support.",
            "bear": "A daily close above the nearest resistance on above-average volume.",
        },
        "summary": summary,
        "_stub": True,
    }
