"""Prompt / schema / model / generation versioning (Wave 1 Lane 1D).

One module, one job: produce the `identity` block `EVENT_CONTRACTS.md` §5 specifies, so Waves 2, 3
and 5 all stamp the SAME field names. Nothing persisted today records anything but `modelUsed`, and
without versions the Wave 5 ablation cannot attribute a result to a cause: two runs that differ only
because someone edited `SYSTEM` are indistinguishable in the store.

THE BUMP RULE
-------------
**Changing `SYSTEM`, `SCHEMA` or `REQUIRED_KEYS` in `prompt.py` requires bumping the corresponding
version** — `prompt.PROMPT_VERSION` for `SYSTEM`, `prompt.SCHEMA_VERSION` for `SCHEMA` /
`REQUIRED_KEYS` — using the `name@n` form (`read@1` -> `read@2`).

The rule is enforced, not merely documented: `prompt_fingerprint()` hashes the exact current text
and `PINNED_PROMPT_FINGERPRINT` records it. `services/llm/tests/test_modes.py` asserts the two
match, so an unversioned prompt edit fails CI. When you bump a version you re-pin the hash in the
same commit; the test prints the new value.

SEVEN KEYS, AND `temperature` LIVES IN EXACTLY ONE PLACE (contract §9.40)
-------------------------------------------------------------------------
`identity` is `modelUsed`, `quantization`, `reasoningMode`, `promptVersion`, `schemaVersion`,
`toolSchemaVersion`, `generationSettings{...}` — and `temperature` is inside
`generationSettings`, nowhere else.

This lane first shipped eight, adding a top-level `temperature`, on the grounds that
`QWEN_IMPLEMENTATION_PLAN.md` AD-4 enumerated it. That reading was wrong: AD-4's prose
double-counted, listing `temperature` *and* `generationSettings` when the second already contains
the first. The contract outranks the plan by the stated hierarchy, and §9.40 settles it — AD-4's
wording has since been corrected, so re-reading it will no longer say eight.

The shape is also simply better. One canonical location per value; a duplicated field drifts the
first time someone writes one and not the other, and Wave 3 Lane 3C and Wave 5A both read `identity`
expecting §5's shape. `IDENTITY_KEYS` is the pin, and `test_modes.py` asserts the exact tuple.
"""
from __future__ import annotations

import hashlib
import json
import os

from .prompt import PROMPT_VERSION, REQUIRED_KEYS, SCHEMA, SCHEMA_VERSION, SYSTEM
from .tools import TOOL_SCHEMA_VERSION

__all__ = [
    "PROMPT_VERSION", "SCHEMA_VERSION", "TOOL_SCHEMA_VERSION",
    "QUANTIZATION_ENV", "DEFAULT_QUANTIZATION", "IDENTITY_KEYS",
    "PINNED_PROMPT_FINGERPRINT", "prompt_fingerprint", "quantization", "identity_stamp",
]

# --- version constants for what exists TODAY ----------------------------------------------------
# `PROMPT_VERSION` / `SCHEMA_VERSION` are re-exported from prompt.py so there is exactly one home
# for each and the import runs one way only (versioning -> prompt, never back).

#: The Wave 3 tool surface (`EVENT_CONTRACTS.md` §6). The registry LANDED in Wave 3 Lane 3A, so this
#: is no longer a placeholder: `tools.py` is now its one home and this is a re-export, exactly as
#: `PROMPT_VERSION` / `SCHEMA_VERSION` are re-exported from `prompt.py` above. Bumping it is a
#: contract event, not a refactor — and with one home there is only one line to bump.

#: Env var carrying the served model's quantization (e.g. `q8_0`, `awq-int4`, `bf16`). It cannot be
#: derived — the server does not report it — so it is declared. Named in the AI_* family beside
#: AI_MODEL. Needs adding to `.env.example`; see the handoff.
QUANTIZATION_ENV = "AI_QUANTIZATION"
DEFAULT_QUANTIZATION = "unknown"

#: The exact keys `identity_stamp()` emits, in order — the seven of contract §5. Nothing else, ever:
#: an audit field must describe what actually happened, an invented field is worse than a missing
#: one, and a DUPLICATED one is worse than both (§9.40). `temperature` is reachable only at
#: `identity["generationSettings"]["temperature"]`.
IDENTITY_KEYS = (
    "promptVersion",
    "schemaVersion",
    "toolSchemaVersion",
    "modelUsed",
    "reasoningMode",
    "quantization",
    "generationSettings",
)


# --- the bump guard ------------------------------------------------------------------------------

def prompt_fingerprint() -> str:
    """sha256 over the versioned prompt surface: SYSTEM, SCHEMA and REQUIRED_KEYS.

    `sort_keys=True` so a dict-literal reordering that changes nothing semantically does not force a
    version bump, while any text change does.
    """
    payload = json.dumps(
        {"system": SYSTEM, "schema": SCHEMA, "required": REQUIRED_KEYS},
        sort_keys=True, separators=(",", ":"), ensure_ascii=False,
    )
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


#: Pinned fingerprint of `PROMPT_VERSION` + `SCHEMA_VERSION` as they stand. Re-pin ONLY together
#: with a version bump (see THE BUMP RULE above).
PINNED_PROMPT_FINGERPRINT = "4b04cf7cee5bd11d2a5bcd8eef6c658e9e873a0844708df0912aae7744973e6c"

#: The versions the pin belongs to, so a bump without a re-pin is also caught.
PINNED_FOR = (PROMPT_VERSION, SCHEMA_VERSION)


# --- the identity stamp ---------------------------------------------------------------------------

def quantization() -> str:
    """The served model's quantization, from the environment. Read at call time so a container that
    sets it late, or a test that monkeypatches it, is honoured."""
    return (os.getenv(QUANTIZATION_ENV) or "").strip() or DEFAULT_QUANTIZATION


def identity_stamp(
    model_used: str,
    *,
    reasoning_mode: str | None = None,
    temperature: float | None = None,
    prompt_version: str = PROMPT_VERSION,
    schema_version: str = SCHEMA_VERSION,
    tool_schema_version: str = TOOL_SCHEMA_VERSION,
    quantization_label: str | None = None,
    generation_settings: dict | None = None,
) -> dict:
    """The `identity` block for a persisted artefact — exactly `IDENTITY_KEYS`, nothing else.

    Args:
        model_used: the value `llm.py` already returns, INCLUDING `stub:offline` / `stub:quota` —
            a stub-generated artefact must be identifiable as one, or the ablation silently
            averages deterministic text into the model's score.
        reasoning_mode: the mode ACTUALLY USED (`ModeResult.reasoningModeUsed` /
            `llm.last_mode_meta()["reasoningModeUsed"]`), never the mode requested. `None` when no
            mode was requested or no generation happened.
        temperature: the temperature the request actually carried — 0.2 in JSON mode, 0.3 for free
            text, per `llm.py`. `None` when nothing was sent. It lands in `generationSettings` and
            is NOT echoed at the top level (§9.40).
        generation_settings: overrides the derived `{"temperature": …}`. Only settings the request
            actually carried may appear; `None` values are dropped. There is no `topP` and no
            `seed` today, and claiming either would be worse than omitting it.
    """
    settings = dict(generation_settings) if generation_settings else {}
    if not settings and temperature is not None:
        settings["temperature"] = temperature
    settings = {k: v for k, v in settings.items() if v is not None}

    return {
        "promptVersion": prompt_version,
        "schemaVersion": schema_version,
        "toolSchemaVersion": tool_schema_version,
        "modelUsed": model_used,
        "reasoningMode": reasoning_mode,
        "quantization": quantization_label or quantization(),
        "generationSettings": settings,
    }


def identity_stamp_from_meta(model_used: str, meta: dict | None, *,
                             json_mode: bool = True, **kwargs) -> dict:
    """Convenience wrapper: build the stamp from `llm.last_mode_meta()` (or a `call_model_meta`
    3rd element). This is the one-liner `store.py` / `main.py` will use — see the handoff."""
    meta = meta or {}
    return identity_stamp(
        model_used,
        reasoning_mode=meta.get("reasoningModeUsed"),
        temperature=(0.2 if json_mode else 0.3) if not str(model_used).startswith("stub:") else None,
        **kwargs,
    )
