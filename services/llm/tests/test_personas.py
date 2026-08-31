"""Persona ("instructor") store + guardrail-layering tests (no network, no model).

Covers per-user scoping: built-ins are global, user personas are private to their owner, and a guest
(user id "") can list built-ins but cannot create/update/delete.

Run:  cd services/llm && python -m pytest -q tests/test_personas.py
"""
import importlib

import pytest

from app.assistant import CHAT_SYSTEM, PERSONA_GUARD, build_chat_messages

USER_A = "u_aaaaaaaaaaaa"
USER_B = "u_bbbbbbbbbbbb"


@pytest.fixture
def personas(tmp_path, monkeypatch):
    """Fresh persona module pointed at a temp dir, so each user's store starts empty and isolated."""
    monkeypatch.setenv("PERSONAS_DIR", str(tmp_path / "_personas"))
    import app.personas as personas_mod
    importlib.reload(personas_mod)
    return personas_mod


def test_builtins_always_present_and_readonly(personas):
    listed = personas.list_personas(USER_A)
    ids = {p["id"] for p in listed}
    assert "builtin:instructor" in ids            # the teacher/instructor ships by default
    assert all(p["builtin"] for p in listed if p["id"].startswith("builtin:"))
    # built-ins resolve by id, for any user (they're global)
    teacher = personas.get_persona("builtin:instructor", USER_A)
    assert teacher and "teacher" in teacher["instructions"].lower()
    # and cannot be edited or deleted
    with pytest.raises(ValueError):
        personas.update_persona(USER_A, "builtin:instructor", {"name": "x"})
    with pytest.raises(ValueError):
        personas.delete_persona(USER_A, "builtin:instructor")


def test_user_persona_crud_roundtrip(personas):
    created = personas.create_persona(USER_A, "Options Coach", "Explain options greeks simply.", emoji="🧮")
    assert created["id"].startswith("p_") and created["builtin"] is False

    # it now lists and resolves for its owner
    assert any(p["id"] == created["id"] for p in personas.list_personas(USER_A))
    assert personas.get_persona(created["id"], USER_A)["name"] == "Options Coach"

    # update
    updated = personas.update_persona(USER_A, created["id"], {"name": "Greeks Coach"})
    assert updated["name"] == "Greeks Coach"
    assert personas.get_persona(created["id"], USER_A)["name"] == "Greeks Coach"

    # persistence: a fresh reload of the module reads it back from disk
    reloaded = importlib.reload(personas)
    assert reloaded.get_persona(created["id"], USER_A)["name"] == "Greeks Coach"

    # delete
    assert reloaded.delete_persona(USER_A, created["id"]) is True
    assert reloaded.get_persona(created["id"], USER_A) is None


def test_per_user_isolation(personas):
    """User A's personas are invisible + immutable to user B, and vice versa."""
    a = personas.create_persona(USER_A, "A's Coach", "A's instructions.")
    b = personas.create_persona(USER_B, "B's Coach", "B's instructions.")

    # each only sees their own (+ built-ins), never the other's
    a_ids = {p["id"] for p in personas.list_personas(USER_A)}
    b_ids = {p["id"] for p in personas.list_personas(USER_B)}
    assert a["id"] in a_ids and b["id"] not in a_ids
    assert b["id"] in b_ids and a["id"] not in b_ids

    # B cannot resolve, edit, or delete A's persona
    assert personas.get_persona(a["id"], USER_B) is None
    with pytest.raises(KeyError):
        personas.update_persona(USER_B, a["id"], {"name": "hijack"})
    assert personas.delete_persona(USER_B, a["id"]) is False

    # ...and A's persona is untouched
    assert personas.get_persona(a["id"], USER_A)["name"] == "A's Coach"


def test_guest_sees_only_builtins_and_cannot_write(personas):
    """A guest ("") lists built-ins only and cannot create/update/delete."""
    # a user has a persona; the guest must not see it
    a = personas.create_persona(USER_A, "A's Coach", "A's instructions.")
    guest_list = personas.list_personas("")
    assert all(p["builtin"] for p in guest_list)
    assert a["id"] not in {p["id"] for p in guest_list}

    # guest writes are refused
    with pytest.raises(ValueError):
        personas.create_persona("", "Nope", "should not persist")
    with pytest.raises(ValueError):
        personas.update_persona("", a["id"], {"name": "x"})
    with pytest.raises(ValueError):
        personas.delete_persona("", a["id"])


def test_create_validates_and_caps(personas):
    with pytest.raises(ValueError):
        personas.create_persona(USER_A, "", "some instructions")
    with pytest.raises(ValueError):
        personas.create_persona(USER_A, "Name", "   ")
    long = personas.create_persona(USER_A, "N" * 200, "I" * 5000)
    assert len(long["name"]) <= personas.NAME_MAX
    assert len(long["instructions"]) <= personas.INSTRUCTIONS_MAX


def test_get_persona_none_for_unknown(personas):
    assert personas.get_persona(None, USER_A) is None
    assert personas.get_persona("nope", USER_A) is None


def test_chat_layers_persona_above_locked_grounding():
    persona = {"id": "p_x", "name": "Instructor", "instructions": "Teach like a patient tutor."}
    ctx = {"price": {"last": 196.9}, "regime": {"trend": "uptrend"}}
    msgs = build_chat_messages("NVDA", "1D", ctx, [{"role": "user", "content": "hi"}], persona)

    # grounding is FIRST and authoritative; persona is a SECOND, subordinate system message
    assert msgs[0]["role"] == "system" and msgs[0]["content"] == CHAT_SYSTEM
    assert msgs[1]["role"] == "system"
    assert PERSONA_GUARD in msgs[1]["content"]
    assert "Teach like a patient tutor." in msgs[1]["content"]
    # the guard explicitly forbids the persona from unlocking a prediction / buy-sell call
    low = msgs[1]["content"].lower()
    assert "subordinate" in low and "buy/sell" in low
    # context turn still present after the persona
    assert any("ground truth" in m["content"] for m in msgs if m["role"] == "system")


def test_chat_without_persona_is_unchanged():
    ctx = {"price": {"last": 196.9}}
    msgs = build_chat_messages("NVDA", "1D", ctx, [{"role": "user", "content": "hi"}], None)
    systems = [m for m in msgs if m["role"] == "system"]
    # exactly two system messages (grounding + context) — no persona injected
    assert len(systems) == 2
    assert systems[0]["content"] == CHAT_SYSTEM
    assert PERSONA_GUARD not in systems[1]["content"]
