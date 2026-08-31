"""Qwen3 reasoning-mode control (Wave 1 Lane 1D).

Qwen3 can run with thinking on or off. Which mode a generation actually ran in is *provenance*: a
silent fallback that leaves thinking on invalidates the Wave 5 mode ablation, because the run is
then attributed to a cause it did not have. This module owns three things and nothing else:

  1. the two mode values, spelled exactly as `EVENT_CONTRACTS.md` §5 (`identity.reasoningMode`) and
     §7 (`audit.reasoningMode`) spell them: ``thinking`` and ``non_thinking``;
  2. the doc §8 routing table AS DATA (`mode_for`), where an unknown task **raises** — a silent
     default is how an ablation ends up measuring the wrong thing;
  3. `strip_thinking`, which removes ``<think>…</think>`` spans before anything tries to parse or
     persist the text.

Mode is a **transport parameter, not prompt text** — OpenAI-compatible servers take
``chat_template_kwargs: {"enable_thinking": false}``, Ollama takes ``think: false``. The one
prompt-level exception is the documented ``/no_think`` suffix for older Ollama builds, used strictly
as a last-resort fallback by `llm.py` and never persisted. Nothing here is appended to `SYSTEM`.

"doc §N" means a section of `docs/research-os/QWEN3_RESEARCH.md` (`EVENT_CONTRACTS.md` §9.37).
"""
from __future__ import annotations

import re
from dataclasses import dataclass

# --- the two mode values (contract §5 / §7 vocabulary; do not respell) --------------------------

THINKING = "thinking"
NON_THINKING = "non_thinking"
MODES = (THINKING, NON_THINKING)


# --- doc §8 routing table, as data -------------------------------------------------------------
# Eight task keys, no more. The right-hand column is doc §8's "Suggested mode" verbatim.

TASK_MODES: dict[str, str] = {
    "headline_extraction": NON_THINKING,        # headline/article extraction — structured transform
    "news_deduplication": NON_THINKING,         # classification / matching
    "sentiment_tagging": NON_THINKING,          # sentiment / relevance tagging — strict schema
    "tool_routing": NON_THINKING,               # keep orchestration cheap and predictable
    "technical_interpretation": THINKING,       # combine signals, resolve contradictions
    "macro_impact": THINKING,                   # Fed / macro causal mapping
    "news_vs_technical_conflict": THINKING,     # explicit evidence weighting
    "final_thesis_synthesis": THINKING,         # highest-value reasoning step
}

TASKS = tuple(TASK_MODES)


def mode_for(task: str) -> str:
    """The reasoning mode doc §8 assigns to `task`.

    An unknown task raises `ValueError` rather than defaulting. This is deliberate: a silent default
    would let a new call site join the ablation under a mode nobody chose, and the resulting number
    would be attributed to the wrong cause.
    """
    try:
        return TASK_MODES[task]
    except KeyError:
        raise ValueError(
            f"unknown task {task!r}; doc §8 defines exactly {sorted(TASK_MODES)}"
        ) from None


# --- <think> stripping --------------------------------------------------------------------------

# Non-greedy, DOTALL, case-insensitive. `<think ...>` with attributes is tolerated; `</think >` too.
_THINK_BLOCK = re.compile(r"<think\b[^>]*>.*?</think\s*>", re.DOTALL | re.IGNORECASE)
# An UNCLOSED opener: the model was cut off mid-thought. Everything from it to the end is thinking.
_THINK_OPEN = re.compile(r"<think\b[^>]*>.*\Z", re.DOTALL | re.IGNORECASE)


def strip_thinking(text: str) -> str:
    """Remove every ``<think>…</think>`` span from `text`.

    - well-formed spans (any number, spanning newlines) are removed;
    - a remaining **unclosed** ``<think>`` drops everything from it to the end of the string;
    - whitespace is stripped **only when something was actually removed**.

    That last clause is the reconciliation of two requirements that would otherwise conflict: the
    lane spec asks for a trailing whitespace strip *and* for text containing no tags to pass through
    byte for byte. Conditioning the strip on a removal satisfies both, and costs nothing in
    practice because `llm.py` already applies `.strip()` to every raw response as it does today.

    Idempotent: `strip_thinking(strip_thinking(x)) == strip_thinking(x)`.
    """
    if not text:
        return text
    out = _THINK_BLOCK.sub("", text)
    out = _THINK_OPEN.sub("", out)
    if out == text:
        return text  # no tags — byte-identical passthrough
    return out.strip()


def has_thinking(text: str) -> bool:
    """True if `text` still carries a `<think>` opener. Used by tests as a leak assertion."""
    return bool(text) and bool(re.search(r"<think\b", text, re.IGNORECASE))


# --- the result object AD-4 calls out: "which mode was actually used must be recorded" -----------

# Per-provider support verdict vocabulary.
UNKNOWN = "unknown"
SUPPORTED = "supported"
REJECTED = "rejected"
SUPPORT_VERDICTS = (UNKNOWN, SUPPORTED, REJECTED)

# `transport` values. The last one records that the mode was forced through the documented
# `/no_think` prompt suffix rather than through a transport parameter.
TRANSPORT_OPENAI = "openai"
TRANSPORT_OLLAMA = "ollama"
TRANSPORT_OLLAMA_NO_THINK = "ollama:no_think_suffix"
TRANSPORT_STUB = "stub"


@dataclass(frozen=True)
class ModeResult:
    """What actually happened to the mode request on one model call.

    Field names are camelCase on purpose: they are the contract's `identity` / `audit` vocabulary,
    and `as_dict()` is copied straight into a persisted artefact.

    - `reasoningModeRequested` — what the caller asked for (`None` = "send nothing, leave the
      server's default", which reproduces the pre-Wave-1 behaviour byte for byte).
    - `reasoningModeUsed` — what the generation actually ran in, or `None` when no generation
      happened (the deterministic stub) or when no mode was requested and the server's default is
      therefore unknown to us.
    - `degraded` — True when the requested mode could not be applied as a clean transport
      parameter: either the server rejected the field (then `reasoningModeUsed` differs from
      `reasoningModeRequested`) or it had to be forced with the `/no_think` suffix.
    - `transport` — one of the TRANSPORT_* values above.
    - `provider` — the provider-chain name that answered (`managed`, `llm`, `ollama`) or None.
    """

    reasoningModeRequested: str | None = None
    reasoningModeUsed: str | None = None
    degraded: bool = False
    transport: str | None = None
    provider: str | None = None

    def as_dict(self) -> dict:
        return {
            "reasoningModeRequested": self.reasoningModeRequested,
            "reasoningModeUsed": self.reasoningModeUsed,
            "degraded": self.degraded,
            "transport": self.transport,
            "provider": self.provider,
        }
