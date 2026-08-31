"""The model runtime seam — Wave 5 Lane 5C.

ONE JOB: answer "which runtime is serving this process, and did the operator actually configure
one?" with no default, no guess and no silent localhost attempt.

WHY THIS FILE EXISTS
--------------------
Until Wave 5 the model endpoint was "wherever the process finds it". `llm.py::_build_providers`
read `os.getenv("LLM_BASE_URL", os.getenv("OLLAMA_URL", "http://localhost:11434"))` and the
OpenAI-compatible fallback defaulted to `http://localhost:1234/v1`. Both are endpoint DEFAULTS, and
§9.42 already settled that class of question for provider gates: a gate whose variable has a default
closes nothing. The model endpoint is the same class and larger — a defaulted endpoint means a run
can open a socket nobody asked it to open, and a run that fell back to the stub is indistinguishable
from a run that reached a model on some other machine.

The runtime is now **self-hosted on a prod box, keyless**. That is what makes invariant #1 survive
the move: there is no credential, so there is nothing an empty `.env` is missing.

THE FOUR RULES, AND WHERE EACH IS ENFORCED
------------------------------------------
1. **No default.** `MODEL_RUNTIME_URL` is read with no fallback value. Absent or blank ⇒
   `configured_runtime()` returns `None` and the process serves `stub:offline` — explicitly, with a
   stated reason, never a localhost attempt. Enforced by `configured_runtime`.
2. **Observably.** Every refusal carries a sentence saying which variable was missing or wrong.
   `runtime_report()` surfaces them on `/health` so "why am I getting the stub" is answerable
   without reading code. An absent observation is not an observation of absence (§9.44).
3. **Keyless.** There is no API-key variable for this runtime, deliberately. If one is set anyway —
   `MODEL_RUNTIME_API_KEY` — the runtime is REFUSED rather than used unauthenticated, because
   quietly dropping a credential the operator set would send unauthenticated traffic that the
   operator believes is authenticated. A runtime that genuinely needs a credential is a change to
   invariant #1 and needs its own amendment, not a config entry.
4. **The model name is part of the runtime's identity**, so it has no default either. A record
   stamped with a guessed model cannot be reproduced, and an unreproducible record in the Wave 5A
   ladder is worse than a gap (`ablation.py` says the same thing about `identity`).

WHAT THIS FILE DELIBERATELY DOES NOT DO
---------------------------------------
It does not add a key to the `identity` stamp. §9.40 pins `identity` at exactly seven keys and an
eighth would be a contract violation dressed up as an audit improvement. `identity["modelUsed"]`
already separates `stub:offline` from a served model; the runtime's NAME travels beside identity,
never inside it (`descriptor()`, rendered as the envelope's `runtime` block).

It also opens no socket. Nothing here calls a model, pings an endpoint or measures anything — the
measuring is `app.smoke`'s, and it runs on the prod box.
"""
from __future__ import annotations

import os
from dataclasses import dataclass

__all__ = [
    "RUNTIME_URL_ENV", "RUNTIME_MODEL_ENV", "RUNTIME_KIND_ENV", "RUNTIME_NAME_ENV",
    "RUNTIME_FORBIDDEN_KEY_ENV", "RUNTIME_KINDS", "STUB_RUNTIME",
    "Runtime", "configured_runtime", "runtime_report", "descriptor", "served_runtime",
    "is_stub",
]

#: The endpoint. Read with NO DEFAULT — this is the whole clause in one line.
RUNTIME_URL_ENV = "MODEL_RUNTIME_URL"
#: The served model id (`qwen3-14b`). No default: see rule 4 above.
RUNTIME_MODEL_ENV = "MODEL_RUNTIME_MODEL"
#: Transport dialect. This one MAY have a default — it is not an endpoint, and `openai` is the
#: OpenAI-compatible shape every self-hosted server in this programme's scope speaks (vLLM, SGLang,
#: llama.cpp's server, LM Studio). A wrong value is refused rather than coerced.
RUNTIME_KIND_ENV = "MODEL_RUNTIME_KIND"
#: An operator-chosen label for the deployment, so two prod boxes are distinguishable in an audit
#: record. Optional; falls back to `kind@host`, which is derived, not defaulted.
RUNTIME_NAME_ENV = "MODEL_RUNTIME_NAME"
#: NOT a configuration variable. Its presence is an error — see rule 3.
RUNTIME_FORBIDDEN_KEY_ENV = "MODEL_RUNTIME_API_KEY"

RUNTIME_KINDS = ("openai", "ollama")
DEFAULT_KIND = "openai"

#: `llm.py::_stub_label`'s token for "no model answered". Restated here rather than imported
#: because `llm.py` imports THIS module, and the dependency runs one way only.
STUB_RUNTIME = "stub:offline"


@dataclass(frozen=True)
class Runtime:
    """A configured, keyless model runtime. Frozen: read at import, never mutated at request time."""

    name: str
    kind: str
    base: str
    model: str

    def as_provider(self) -> dict:
        """The provider-chain dict `llm.py` already speaks. `key` is `""` — keyless, by rule 3.

        `_post_openai` sends `Authorization: Bearer ` with an empty key. A self-hosted server that
        ignores the header is the normal case; one that rejects it needs a credential, which rule 3
        says is an amendment, not a config entry.
        """
        return {"name": self.name, "kind": self.kind, "base": self.base,
                "key": "", "model": self.model}


def _clean(env, key: str) -> str:
    return str(env.get(key, "") or "").strip()


def _normalise_base(base: str, kind: str) -> str:
    """Trailing slash off; `/v1` appended for the OpenAI dialect if the operator omitted it.

    This is not a default — there is no endpoint here that the operator did not type. It is the
    same suffix repair `_build_providers` has always done for `AI_BASE_URL`, kept so that moving a
    working value from `AI_BASE_URL` to `MODEL_RUNTIME_URL` does not silently change behaviour.
    """
    base = base.rstrip("/")
    if kind == "openai" and base and not base.endswith("/v1"):
        base += "/v1"
    return base


def _host_of(base: str) -> str:
    without_scheme = base.split("://", 1)[-1]
    return without_scheme.split("/", 1)[0] or "unknown-host"


def configured_runtime(env=None) -> tuple[Runtime | None, list[str]]:
    """`(runtime, refusals)`. `runtime is None` ⇒ this process serves `stub:offline`.

    Every `None` comes with at least one refusal sentence. A silent `None` would reproduce the exact
    failure this seam exists to end: an operator who cannot tell a misconfigured endpoint from an
    absent one.
    """
    env = os.environ if env is None else env
    refusals: list[str] = []

    if _clean(env, RUNTIME_FORBIDDEN_KEY_ENV):
        # Checked FIRST and fatally. If we checked it later, a runtime with a complete URL and model
        # would be built and used while the operator's credential sat unread.
        refusals.append(
            f"{RUNTIME_FORBIDDEN_KEY_ENV} is set, and this runtime is KEYLESS by contract. A "
            "runtime that requires a credential is a change to invariant #1 and needs its own "
            "amendment, not a config entry. Refusing rather than sending unauthenticated requests "
            "an operator believes are authenticated."
        )
        return None, refusals

    base = _clean(env, RUNTIME_URL_ENV)
    model = _clean(env, RUNTIME_MODEL_ENV)
    kind = _clean(env, RUNTIME_KIND_ENV).lower() or DEFAULT_KIND

    if not base:
        # The ordinary, correct, shipped state: empty `.env` ⇒ stub. Not an error, and phrased so
        # nobody reads it as one.
        refusals.append(
            f"{RUNTIME_URL_ENV} is unset, so no model runtime is configured and this process "
            f"serves `{STUB_RUNTIME}`. That is the shipped default and it is not an error — the "
            "endpoint is read with no default value on purpose, so an unconfigured deployment can "
            "never open a socket nobody asked it to open."
        )
        return None, refusals

    if kind not in RUNTIME_KINDS:
        refusals.append(
            f"{RUNTIME_KIND_ENV}={kind!r} is not one of {list(RUNTIME_KINDS)}. Refused rather than "
            f"coerced to {DEFAULT_KIND!r}: guessing a transport dialect produces a run whose "
            "failures cannot be attributed."
        )
        return None, refusals

    if not model:
        refusals.append(
            f"{RUNTIME_URL_ENV} is set but {RUNTIME_MODEL_ENV} is unset. The model id is part of "
            "the runtime's identity and has no default: a record stamped with a guessed model "
            "cannot be reproduced, and an unreproducible record in the ablation ladder is worse "
            "than a gap."
        )
        return None, refusals

    normalised = _normalise_base(base, kind)
    name = _clean(env, RUNTIME_NAME_ENV) or f"{kind}@{_host_of(normalised)}"
    return Runtime(name=name, kind=kind, base=normalised, model=model), []


def is_stub(model_used) -> bool:
    """True for every `stub:*` label — `stub:offline`, `stub:quota` and any later sibling.

    A prefix test, not equality against `stub:offline`. `stub:quota` is a fixture too, and a check
    that only knew the one label would license a claim about the model from the other.
    """
    return str(model_used or "").strip().lower().startswith("stub:")


def served_runtime(model_used, runtime: Runtime | None) -> str:
    """The name of the runtime that ACTUALLY served this generation.

    Not "the runtime that is configured" — the two differ exactly when the configured endpoint was
    unreachable and the stub answered, which is the case a reader most needs to be able to see.
    """
    if is_stub(model_used) or runtime is None:
        return STUB_RUNTIME
    return runtime.name


def descriptor(runtime: Runtime | None, model_used=None) -> dict:
    """The `runtime` block served BESIDE `identity`, never inside it (§9.40 pins seven keys).

    `baseUrl` is deliberately absent: this block reaches the browser through the analyst envelope,
    and a deployment's internal hostname is not the browser's business. `/health`'s
    `runtime_report()` carries the URL for the operator, who is the party that needs it.
    """
    served = served_runtime(model_used, runtime)
    return {
        "configured": runtime is not None,
        "name": runtime.name if runtime else None,
        "kind": runtime.kind if runtime else None,
        "model": runtime.model if runtime else None,
        # `served` is the load-bearing key. `stub:offline` here means a fixture produced everything
        # in this envelope, and no claim about model behaviour may cite it.
        "served": served,
        "isStub": served == STUB_RUNTIME,
    }


def runtime_report(runtime: Runtime | None, refusals: list[str]) -> dict:
    """The operator-facing view, for `/health`. Carries the URL; carries no credential, because
    there is none."""
    return {
        "configured": runtime is not None,
        "name": runtime.name if runtime else None,
        "kind": runtime.kind if runtime else None,
        "baseUrl": runtime.base if runtime else None,
        "model": runtime.model if runtime else None,
        "keyless": True,
        "refusals": list(refusals),
        "envVars": {
            "url": RUNTIME_URL_ENV,
            "model": RUNTIME_MODEL_ENV,
            "kind": RUNTIME_KIND_ENV,
            "name": RUNTIME_NAME_ENV,
        },
    }
