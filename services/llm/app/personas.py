"""Persona ("instructor") store — user-editable custom-instruction profiles for the grounded chat.

A persona is like a Gemini Gem: it sets the assistant's ROLE, tone, and teaching style for a chat
thread (e.g. a patient instructor who defines every term). It LAYERS ON TOP of the chat's hard
grounding rules in `assistant.py` — a persona can NEVER unlock a price prediction, a fabricated
number, or a buy/sell/hold call (invariant #2). That subordination is enforced in the prompt
(`PERSONA_GUARD`) and the model still runs through the same non-oracular grounding either way.

Built-in presets are defined in code and always available (so the app still ships useful personas
with zero setup and zero network). User personas are SCOPED PER USER in PostgreSQL's `llm` schema.
The historical file layout below is retained only for no-database fallback and one-time import:

    {PERSONAS_DIR}/{userId}/personas.json  ->  {"personas": [ {id, name, emoji, instructions, ...}, ... ]}

The user id ("" = guest) is resolved by the gateway from the session cookie and forwarded as
X-User-Id (see auth.py). Guests can LIST (built-ins only) but cannot create/update/delete. A pre-auth
{PERSONAS_DIR}/personas.json (from before accounts existed) is migrated ONCE into a "_legacy" bucket
so those personas are preserved (but not attributed to any account).
"""
from __future__ import annotations

import json
import os
import re
import uuid
from datetime import datetime, timezone

from . import db

# Explicit no-database fallback and one-time import location.
_READS_DIR = os.getenv("READS_DIR", os.path.join(os.getcwd(), "data", "reads"))
PERSONAS_DIR = os.getenv("PERSONAS_DIR", os.path.join(_READS_DIR, "_personas"))

NAME_MAX = 60
INSTRUCTIONS_MAX = 2000
EMOJI_MAX = 8

# --------------------------------------------------------------------------- built-in presets
# Always available, read-only via the API (can't be edited/deleted — duplicate to customize). Each
# one only shapes ROLE/tone/teaching style; none of them ask for a prediction or a recommendation.
BUILTIN_PERSONAS: list[dict] = [
    {
        "id": "builtin:instructor",
        "name": "Instructor",
        "emoji": "🎓",
        "builtin": True,
        "instructions": (
            "Act as a patient trading-and-technical-analysis teacher. Assume the user is learning. "
            "Define every term the first time you use it (RSI, MACD, VWAP, support/resistance, "
            "volatility band, etc.) in one short plain sentence. Explain WHY the current facts matter, "
            "not just what they are, and use a simple analogy where it helps. Where natural, end with "
            "one guiding question that nudges the user to reason for themselves. Teach the method; never "
            "tell them what to do with their money."
        ),
    },
    {
        "id": "builtin:technician",
        "name": "Chart Technician",
        "emoji": "📈",
        "builtin": True,
        "instructions": (
            "Act as a terse, price-action-focused technician. Lead with the levels and indicator states "
            "that are in the context (trend, momentum, MACD, the key support/resistance, the volatility "
            "band). Be concise and concrete; skip pleasantries. Frame everything as observation and "
            "scenario ('if it holds X…', 'a break of Y would…'), never as an instruction."
        ),
    },
    {
        "id": "builtin:skeptic",
        "name": "Devil's Advocate",
        "emoji": "🧐",
        "builtin": True,
        "instructions": (
            "Act as a constructive skeptic. Whatever the apparent setup, stress-test it: surface what "
            "could go wrong, what the bulls might be missing, and which assumptions the read depends on. "
            "Always give the other side of the argument. Stay grounded in the supplied facts and keep "
            "uncertainty language — you are pressure-testing thinking, not making a call."
        ),
    },
    {
        "id": "builtin:plain",
        "name": "Plain English",
        "emoji": "💬",
        "builtin": True,
        "instructions": (
            "Explain everything in plain, jargon-free English for a complete beginner. If a technical "
            "term is unavoidable, translate it immediately in parentheses. Keep sentences short and "
            "concrete. Prefer everyday analogies over finance vocabulary."
        ),
    },
]

_BUILTIN_BY_ID = {p["id"]: p for p in BUILTIN_PERSONAS}


# --------------------------------------------------------------------------- persistence helpers

LEGACY_BUCKET = "_legacy"


def _migrate_legacy() -> None:
    """Move a pre-auth {PERSONAS_DIR}/personas.json into the legacy bucket once (preserve, don't
    attribute). Idempotent; best-effort."""
    old = os.path.join(PERSONAS_DIR, "personas.json")
    if not os.path.isfile(old):
        return
    dst = os.path.join(PERSONAS_DIR, LEGACY_BUCKET, "personas.json")
    if os.path.exists(dst):
        return
    try:
        os.makedirs(os.path.dirname(dst), exist_ok=True)
        os.replace(old, dst)
    except OSError:
        pass


def _path(user_id: str) -> str:
    d = os.path.join(PERSONAS_DIR, user_id)
    os.makedirs(d, exist_ok=True)
    return os.path.join(d, "personas.json")


def _load_user(user_id: str) -> list[dict]:
    """The user's own personas. A guest ("") owns none."""
    if not user_id:
        return []
    if db.enabled():
        rows = db.list_records("personas", user_id=user_id, limit=1)
        data = rows[0] if rows else {}
        items = data.get("personas", []) if isinstance(data, dict) else []
        return [p for p in items if isinstance(p, dict) and p.get("id")]
    _migrate_legacy()
    try:
        with open(_path(user_id)) as f:
            data = json.load(f)
        items = data.get("personas", []) if isinstance(data, dict) else []
        return [p for p in items if isinstance(p, dict) and p.get("id")]
    except (OSError, json.JSONDecodeError):
        return []


def _save_user(user_id: str, personas: list[dict]) -> None:
    record = {"personas": personas}
    if db.enabled():
        db.save("personas", "", "current", record, user_id=user_id)
    else:
        with open(_path(user_id), "w") as f:
            json.dump(record, f, indent=2)


def _now() -> str:
    return datetime.now(timezone.utc).isoformat()


def _clean(text: str | None, limit: int) -> str:
    return re.sub(r"\s+", " ", str(text or "")).strip()[:limit]


# --------------------------------------------------------------------------- public API

def list_personas(user_id: str = "") -> list[dict]:
    """Built-in presets (global) first, then the CALLER'S own personas (newest last). A guest
    ("") sees only the built-ins."""
    return [dict(p) for p in BUILTIN_PERSONAS] + _load_user(user_id)


def get_persona(persona_id: str | None, user_id: str = "") -> dict | None:
    """Resolve a persona by id from built-ins (global) or the CALLER'S store. None if not found /
    no id. A guest resolves built-ins only — another user's custom persona is never visible."""
    if not persona_id:
        return None
    if persona_id in _BUILTIN_BY_ID:
        return dict(_BUILTIN_BY_ID[persona_id])
    for p in _load_user(user_id):
        if p.get("id") == persona_id:
            return p
    return None


def create_persona(user_id: str, name: str, instructions: str, emoji: str = "") -> dict:
    """Create a persona owned by user_id. Raises ValueError on a missing user (guest), name, or
    instructions."""
    if not user_id:
        raise ValueError("sign in to create a persona")
    name = _clean(name, NAME_MAX)
    instructions = _clean(instructions, INSTRUCTIONS_MAX)
    if not name:
        raise ValueError("name is required")
    if not instructions:
        raise ValueError("instructions are required")
    persona = {
        "id": "p_" + uuid.uuid4().hex[:12],
        "name": name,
        "emoji": _clean(emoji, EMOJI_MAX),
        "instructions": instructions,
        "builtin": False,
        "createdAt": _now(),
        "updatedAt": _now(),
    }
    personas = _load_user(user_id)
    personas.append(persona)
    _save_user(user_id, personas)
    return persona


def update_persona(user_id: str, persona_id: str, patch: dict) -> dict:
    """Update one of user_id's OWN personas in place. Built-ins are read-only; another user's
    persona is invisible (KeyError). Raises ValueError/KeyError."""
    if not user_id:
        raise ValueError("sign in to edit personas")
    if persona_id in _BUILTIN_BY_ID:
        raise ValueError("built-in personas cannot be edited — duplicate it first")
    personas = _load_user(user_id)
    for p in personas:
        if p.get("id") == persona_id:
            if "name" in patch:
                name = _clean(patch["name"], NAME_MAX)
                if not name:
                    raise ValueError("name cannot be empty")
                p["name"] = name
            if "instructions" in patch:
                instr = _clean(patch["instructions"], INSTRUCTIONS_MAX)
                if not instr:
                    raise ValueError("instructions cannot be empty")
                p["instructions"] = instr
            if "emoji" in patch:
                p["emoji"] = _clean(patch["emoji"], EMOJI_MAX)
            p["updatedAt"] = _now()
            _save_user(user_id, personas)
            return p
    raise KeyError(persona_id)


def delete_persona(user_id: str, persona_id: str) -> bool:
    """Delete one of user_id's OWN personas. Built-ins can't be deleted; another user's persona is
    invisible (returns False). Returns True if one was removed."""
    if not user_id:
        raise ValueError("sign in to delete personas")
    if persona_id in _BUILTIN_BY_ID:
        raise ValueError("built-in personas cannot be deleted")
    personas = _load_user(user_id)
    kept = [p for p in personas if p.get("id") != persona_id]
    if len(kept) == len(personas):
        return False
    _save_user(user_id, kept)
    return True
