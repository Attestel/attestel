"""Wave 2 Lane 2B — the enrichment worker, and the five tests invariant #4 is made of.

`CLAUDE.md` #4 says the LLM stays out of any fast or polling loop. D-25 amends it to permit exactly
one background component: a single-pass, queue-draining enrichment worker that is off by default,
holds a cross-process model lease interactive work always wins, and is reachable only by an explicit
operator command. Everything else stays forbidden — **no timer, interval, cron or scheduler tick may
cause a model call anywhere** (contract §9.22), and no UI event may either.

The five tests below are the proof, not the prose:

  1. `test_worker_is_not_imported_by_the_app`  — nothing in the service reaches this module.
  2. `test_disabled_by_default`                — cleared environment ⇒ zero generations.
  3. `test_no_route_reaches_enrichment`        — no HTTP route resolves into this lane.
  4. `test_no_generation_without_the_lease`    — every generation happened inside a held lease.
  5. `test_enabled_flag_is_the_only_door`      — the flag is necessary and sufficient.

Deterministic: no network, no model. The events service is a recording fake; the transport is
monkeypatched and asserts on the bytes we would have sent.

Run:  cd services/llm && python -m pytest -q tests/test_enrich_worker.py
"""
from __future__ import annotations

import ast
import inspect
import json
import pathlib
import re

import pytest

APP_DIR = pathlib.Path(__file__).resolve().parents[1] / "app"

#: The three modules Lane 2B owns. Nothing the SERVICE can reach may name the first two.
LANE_MODULES = ("enrich_worker", "event_agent", "lease")
#: The two that must stay unreachable from the service. `lease.py` is deliberately NOT here —
#: contract §9.21 requires `analyst.py` to import it in Wave 3, and that is the point of the file.
UNREACHABLE = ("enrich_worker", "event_agent")

#: Modules that are PROCESSES, not part of the served application: each one is invoked as
#: `python -m app.<name>` by an operator command and is imported by nothing that `app.main` can
#: reach. D-25 permits the worker to exist exactly as such a process, so a second process that
#: drives it is in the same category — what D-25 forbids is the SERVICE reaching it.
#:
#: This list is not a way to opt out of the rule. `test_the_standalone_processes_are_unreachable`
#: proves every name here is genuinely outside `app.main`'s import closure, so adding a module here
#: that the service can actually reach fails immediately.
STANDALONE_PROCESSES = ("enrich_worker", "thesis_resynth", "automation", "smoke", "ablation_verdict")


def _import_closure(entry: str) -> set[str]:
    """Every module under `app/` transitively reachable from `entry`, by AST.

    Transitive, not direct: `main.py` importing a module that imports the worker would satisfy a
    one-level check and still put the worker inside the served application. Both `import x` and
    `from x import y` are followed, and `importlib.import_module("app.x")` is caught separately by
    the literal-string test below.
    """
    seen: set[str] = set()
    frontier = [entry]
    while frontier:
        name = frontier.pop()
        if name in seen:
            continue
        seen.add(name)
        path = APP_DIR / f"{name}.py"
        if not path.exists():
            continue
        tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
        for node in ast.walk(tree):
            if isinstance(node, ast.Import):
                for alias in node.names:
                    frontier.append(alias.name.split(".")[-1])
            elif isinstance(node, ast.ImportFrom):
                if node.module:
                    frontier.append(node.module.split(".")[-1])
                for alias in node.names:
                    if (APP_DIR / f"{alias.name}.py").exists():
                        frontier.append(alias.name)
    return seen


# =================================================================================================
# 1. Nothing in the service imports the worker
# =================================================================================================

def test_the_standalone_processes_are_unreachable_from_the_service():
    """The premise of every allowance below: nothing `app.main` imports, transitively, is one of
    the operator-invoked processes. If this fails, the allowlist has become a loophole."""
    reachable = _import_closure("main")
    leaked = sorted(set(STANDALONE_PROCESSES) & reachable)
    assert leaked == [], (
        "a standalone process is inside the served application's import closure; it is no longer "
        f"a separately invoked process and D-25's allowance no longer covers it: {leaked}"
    )


def test_worker_is_not_imported_by_the_app():
    """Every module the SERVICE can reach, parsed, and asserted to import neither `enrich_worker`
    nor `event_agent`.

    A source-level AST walk, not a runtime check: an import that only fires on a rare branch is
    still an import, and `main.py` growing an `@app.on_event("startup")` hook is exactly the
    regression this test exists to catch.

    The scope is the served application, not the directory. `app/automation.py` — Phase 1's
    operator-invoked runner — imports the worker deliberately: it is itself a `python -m` process
    that drives the same bounded pass `make enrich-once` drives, and the test above proves it is
    outside `main`'s closure. D-25 forbids the SERVICE reaching the worker; it explicitly permits
    the worker to be reached by an explicit operator command.
    """
    offenders = []
    for path in sorted(APP_DIR.glob("*.py")):
        if path.stem in LANE_MODULES or path.stem in STANDALONE_PROCESSES:
            continue
        tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
        for node in ast.walk(tree):
            if isinstance(node, ast.Import):
                for alias in node.names:
                    if alias.name.split(".")[-1] in UNREACHABLE:
                        offenders.append(f"{path.name}:{node.lineno} import {alias.name}")
            elif isinstance(node, ast.ImportFrom):
                module = (node.module or "").split(".")[-1]
                if module in UNREACHABLE:
                    offenders.append(f"{path.name}:{node.lineno} from {node.module}")
                for alias in node.names:
                    if alias.name in UNREACHABLE:
                        offenders.append(f"{path.name}:{node.lineno} from … import {alias.name}")
    assert offenders == [], (
        "a module in services/llm/app reaches the enrichment worker; D-25 permits it only as a "
        f"separately invoked process: {offenders}"
    )


def test_the_app_module_source_never_mentions_the_worker():
    """The blunt companion to the AST walk: an `importlib.import_module("app.enrich_worker")`
    hides from `ast.Import`, so also assert the literal strings are absent."""
    offenders = []
    for path in sorted(APP_DIR.glob("*.py")):
        if path.stem in LANE_MODULES or path.stem in STANDALONE_PROCESSES:
            continue
        text = path.read_text(encoding="utf-8")
        for name in UNREACHABLE:
            if name in text:
                offenders.append(f"{path.name}: names {name!r}")
    assert offenders == []


# =================================================================================================
# 2. Off by default
# =================================================================================================

def test_disabled_by_default(monkeypatch, tmp_path):
    """With the environment cleared, `run_once()` performs no generation at all.

    The transport is patched to raise, so a single model call would surface as an exception rather
    than as a passing test — §9.44's rule that absence of a log line is not evidence applies here
    too, which is why this asserts on a *positive* trap rather than on silence.
    """
    from app import enrich_worker

    _clear_env(monkeypatch)
    monkeypatch.setenv("READS_DIR", str(tmp_path))

    def explode(*a, **kw):  # pragma: no cover - the assertion is that this never runs
        raise AssertionError("a model call escaped a disabled worker")

    monkeypatch.setattr(enrich_worker.llm, "call_model_meta", explode)
    monkeypatch.setattr(enrich_worker, "_http", explode)

    report = enrich_worker.run_once()

    assert report["enabled"] is False
    assert report["generated"] == 0
    assert report["posted"] == 0
    assert report["stopped"] == "disabled"


# =================================================================================================
# 3. No route reaches this lane
# =================================================================================================

def test_no_route_reaches_enrichment():
    """Instantiate the real FastAPI app, enumerate every route, and resolve each endpoint callable
    back to the module that defines it. None may be one of this lane's."""
    from app.main import app

    reached = []
    for route in app.routes:
        endpoint = getattr(route, "endpoint", None)
        if endpoint is None:
            continue
        module = inspect.getmodule(inspect.unwrap(endpoint))
        name = (getattr(module, "__name__", "") or "").split(".")[-1]
        if name in LANE_MODULES:
            reached.append(f"{getattr(route, 'path', '?')} -> {name}")
    assert reached == [], (
        "an HTTP route resolves into the enrichment lane; invariant #5 says this lane adds no "
        f"route, and silence is not permission: {reached}"
    )


def test_the_route_table_is_unchanged_by_this_lane():
    """Importing this lane's modules must not register anything. Belt and braces for the above."""
    import importlib

    from app.main import app as fastapi_app

    before = {getattr(r, "path", None) for r in fastapi_app.routes}
    for name in LANE_MODULES:
        importlib.import_module(f"app.{name}")
    after = {getattr(r, "path", None) for r in fastapi_app.routes}
    assert before == after


# =================================================================================================
# 4 & 5 — the lease and the flag. These need the fake events service, defined below.
# =================================================================================================

RAW_EVENT = {
    "id": "evt_0000000000000001",
    "eventType": "regulatory_action",
    "title": "New export restrictions announced",
    "summary": "The department published a rule restricting certain accelerator exports.",
    "occurredAt": "2026-08-01T13:00:00Z",
    "publishedAt": "2026-08-01T13:00:00Z",
    "firstSeenAt": "2026-08-01T13:05:00Z",
    "sourceTier": "official",
    "officialConfirmed": True,
    "importance": 0.9,
    "novelty": 0.8,
    "documentCount": 4,
    "enrichmentState": "raw",
    "tickers": [{"ticker": "NVDA", "relevance": 1.0, "isPrimary": True}],
    "sources": [],
    "audit": None,
}

GOOD_REPLY = json.dumps({
    "whyItMatters": "A rule of this kind can change which customers a supplier may serve.",
    "keyFacts": [],
    "isNovel": True,
    "tickers": [{
        "ticker": "NVDA",
        "potentialDirection": "negative",
        "potentialStrength": 0.7,
        "expectedHorizon": "medium_term",
        "affectedDimensions": ["revenue_exposure", "demand"],
        "transmission": "A meaningful share of its data-centre demand sits in the restricted region.",
    }],
})


def _event(idx: int, *, importance: float, published: str, ticker: str = "NVDA") -> dict:
    ev = json.loads(json.dumps(RAW_EVENT))
    ev["id"] = f"evt_{idx:016d}"
    ev["importance"] = importance
    ev["publishedAt"] = published
    ev["occurredAt"] = published
    ev["firstSeenAt"] = published
    ev["tickers"] = [{"ticker": ticker, "relevance": 1.0, "isPrimary": True}]
    return ev


class FakeEvents:
    """A recording stand-in for `services/events` on :8004.

    Records every request so the point-in-time assertions can be made on the bytes, and lets a test
    force a status code on the enrichment POST.
    """

    def __init__(self, events, *, post_status=200, get_status=200, raise_on_get=False):
        self.events = list(events)
        self.post_status = post_status
        self.get_status = get_status
        self.raise_on_get = raise_on_get
        self.gets: list[tuple[str, dict]] = []
        self.posts: list[tuple[str, dict]] = []

    def __call__(self, method, url, *, params=None, json_body=None, timeout=None):
        if method == "GET":
            self.gets.append((url, dict(params or {})))
            if self.raise_on_get:
                raise ConnectionError("events service is down")
            if self.get_status != 200:
                return self.get_status, {"detail": "boom"}
            as_of = (params or {}).get("as_of")
            tail = url.rsplit("/", 1)[-1]
            if tail.startswith("evt_"):
                # GET /events/{id} — the §9.13 projection at the event's own cutoff. 404 when the
                # event was first seen AFTER that cutoff, exactly as services/events answers.
                for candidate in self.events:
                    if candidate["id"] != tail:
                        continue
                    if as_of and candidate["firstSeenAt"] > as_of:
                        return 404, {"detail": "no such event at this cutoff"}
                    return 200, {"event": candidate, "documents": [],
                                 "tickers": candidate["tickers"], "asOf": as_of, "degraded": []}
                return 404, {"detail": "no such event"}
            state = (params or {}).get("enrichmentState")
            rows = self.events
            if as_of:
                rows = [e for e in rows
                        if e["publishedAt"] <= as_of and e["firstSeenAt"] <= as_of]
            if state == "raw":
                rows = [e for e in rows if e["enrichmentState"] == "raw"]
            rows = sorted(rows, key=lambda e: (e["publishedAt"], e["id"]), reverse=True)
            cursor = (params or {}).get("cursor")
            if cursor:
                ids = [e["id"] for e in rows]
                rows = rows[ids.index(cursor) + 1:] if cursor in ids else []
            limit = int((params or {}).get("limit", 50))
            page, more = rows[:limit], len(rows) > limit
            return 200, {"events": page, "asOf": as_of, "degraded": [],
                         "coverage": "ok" if page else "insufficient",
                         "nextCursor": page[-1]["id"] if (page and more) else None}
        self.posts.append((url, json_body))
        if self.post_status != 200:
            return self.post_status, {"detail": "relevance is deterministic (contract §9.15)"}
        return 200, {"event": {"id": url.rsplit("/", 2)[-2], "enrichmentState": "enriched"}}


def _clear_env(monkeypatch):
    for var in ("EVENT_ENRICH_ENABLED", "EVENTS_URL", "ENRICH_BATCH_SIZE",
                "ENRICH_LAG_HOURS", "ENRICH_MAX_PER_RUN", "ENRICH_IDLE_SECONDS"):
        monkeypatch.delenv(var, raising=False)


@pytest.fixture()
def wired(monkeypatch, tmp_path):
    """The worker, enabled, pointed at a fake events service, with a fake model transport."""
    from app import enrich_worker, llm

    _clear_env(monkeypatch)
    monkeypatch.setenv("EVENT_ENRICH_ENABLED", "true")
    monkeypatch.setenv("EVENTS_URL", "http://events.test:8004")
    monkeypatch.setenv("READS_DIR", str(tmp_path))

    calls: list[dict] = []

    def fake_call(messages, *, json_mode=True, mode=None):
        calls.append({"messages": messages, "mode": mode})
        return GOOD_REPLY, "qwen3-14b", {"reasoningModeRequested": mode,
                                         "reasoningModeUsed": mode, "degraded": False,
                                         "transport": "openai", "provider": "managed"}

    monkeypatch.setattr(llm, "call_model_meta", fake_call)
    monkeypatch.setattr(enrich_worker.llm, "call_model_meta", fake_call)
    return enrich_worker, calls, monkeypatch


def test_no_generation_without_the_lease(wired, tmp_path):
    """Patch the transport to raise unless the lease is held; run a batch; every call must have
    been made inside a held lease.

    This is the assertion D-25's "holding a global model lease" reduces to in code. It is a
    positive measurement — the trap fires on a violation — not a grep for an absent log line
    (contract §9.44).
    """
    worker, calls, monkeypatch = wired
    from app import lease

    seen_held: list[bool] = []

    def guarded(messages, *, json_mode=True, mode=None):
        held = lease.background_lease_is_held()
        seen_held.append(held)
        if not held:
            raise AssertionError("a generation ran without the background lease")
        calls.append({"messages": messages, "mode": mode})
        return GOOD_REPLY, "qwen3-14b", {"reasoningModeUsed": mode, "degraded": False}

    monkeypatch.setattr(worker.llm, "call_model_meta", guarded)
    fake = FakeEvents([_event(1, importance=0.9, published="2026-08-01T13:00:00Z")])
    monkeypatch.setattr(worker, "_http", fake)

    report = worker.run_once(now="2026-08-05T00:00:00Z")

    assert report["generated"] == 1
    assert seen_held == [True]
    assert lease.background_lease_is_held() is False  # released again


def test_enabled_flag_is_the_only_door(wired, tmp_path):
    """`EVENT_ENRICH_ENABLED` is necessary and sufficient: with everything else configured
    identically, `true` generates and anything else does not."""
    worker, calls, monkeypatch = wired
    events = [_event(1, importance=0.9, published="2026-08-01T13:00:00Z")]

    fake = FakeEvents(events)
    monkeypatch.setattr(worker, "_http", fake)
    on = worker.run_once(now="2026-08-05T00:00:00Z")
    assert on["enabled"] is True and on["generated"] == 1

    for value in ("false", "0", "no", "", "TRUE-ish"):
        calls.clear()
        fake2 = FakeEvents([_event(2, importance=0.9, published="2026-08-01T13:00:00Z")])
        monkeypatch.setattr(worker, "_http", fake2)
        monkeypatch.setenv("EVENT_ENRICH_ENABLED", value)
        off = worker.run_once(now="2026-08-05T00:00:00Z")
        assert off["enabled"] is False, value
        assert off["generated"] == 0, value
        assert calls == [], value
        assert fake2.gets == [], value  # not even the queue is read

    monkeypatch.delenv("EVENT_ENRICH_ENABLED", raising=False)
    assert worker.run_once(now="2026-08-05T00:00:00Z")["enabled"] is False


# =================================================================================================
# Phase 4 gates — ordering, caps, preemption, the 400 path, the events-down path, §9.12
# =================================================================================================

def test_queue_is_ordered_by_importance_then_recency(wired):
    """Contract-adjacent policy: a free-tier machine never drains the queue, so the ordering IS the
    policy. `GET /events` orders by `published_at DESC` only (§3.11), so the worker sorts the
    fetched window itself."""
    worker, calls, monkeypatch = wired
    events = [
        _event(1, importance=0.2, published="2026-08-03T00:00:00Z"),
        _event(2, importance=0.9, published="2026-08-01T00:00:00Z"),
        _event(3, importance=0.5, published="2026-08-02T00:00:00Z"),
        _event(4, importance=0.9, published="2026-08-02T12:00:00Z"),
    ]
    fake = FakeEvents(events)
    monkeypatch.setattr(worker, "_http", fake)

    worker.run_once(now="2026-08-06T00:00:00Z")

    posted = [url.rsplit("/", 2)[-2] for url, _ in fake.posts]
    assert posted == ["evt_0000000000000004", "evt_0000000000000002",
                      "evt_0000000000000003", "evt_0000000000000001"]


def test_max_per_run_caps_the_batch(wired):
    worker, calls, monkeypatch = wired
    monkeypatch.setenv("ENRICH_MAX_PER_RUN", "2")
    monkeypatch.setenv("ENRICH_BATCH_SIZE", "1")
    events = [_event(i, importance=1.0 - i / 100, published="2026-08-01T00:00:00Z")
              for i in range(1, 8)]
    fake = FakeEvents(events)
    monkeypatch.setattr(worker, "_http", fake)

    report = worker.run_once(now="2026-08-06T00:00:00Z")

    assert report["generated"] == 2
    assert len(fake.posts) == 2


def test_preemption_stops_the_batch_between_items(wired, tmp_path):
    """An interactive acquirer arriving mid-batch stops the worker at the next item boundary.

    An HTTP call already in flight against a 120 s timeout cannot be aborted safely, so the rule is
    *never start a new one*, not *kill the current one*.
    """
    worker, calls, monkeypatch = wired
    from app import lease

    events = [_event(i, importance=1.0 - i / 100, published="2026-08-01T00:00:00Z")
              for i in range(1, 6)]
    fake = FakeEvents(events)
    monkeypatch.setattr(worker, "_http", fake)

    def preempt_after_first(messages, *, json_mode=True, mode=None):
        calls.append({"messages": messages})
        if len(calls) == 1:
            lease.announce_interactive_waiting()  # what acquire_interactive() does first
        return GOOD_REPLY, "qwen3-14b", {"reasoningModeUsed": mode, "degraded": False}

    monkeypatch.setattr(worker.llm, "call_model_meta", preempt_after_first)

    report = worker.run_once(now="2026-08-06T00:00:00Z")

    assert report["generated"] == 1
    assert report["stopped"] == "preempted"
    assert len(fake.posts) == 1


def test_a_400_marks_the_event_failed_and_does_not_retry(wired):
    worker, calls, monkeypatch = wired
    fake = FakeEvents([_event(1, importance=0.9, published="2026-08-01T00:00:00Z")],
                      post_status=400)
    monkeypatch.setattr(worker, "_http", fake)

    report = worker.run_once(now="2026-08-06T00:00:00Z")

    assert len(fake.posts) == 1                       # posted once, not retried
    assert report["rejected"] == 1
    assert report["posted"] == 0
    assert "§9.15" in report["failures"][0]["detail"]
    assert report["stopped"] in (None, "drained")


def test_events_service_down_stops_the_batch(wired):
    worker, calls, monkeypatch = wired
    fake = FakeEvents([], raise_on_get=True)
    monkeypatch.setattr(worker, "_http", fake)

    report = worker.run_once(now="2026-08-06T00:00:00Z")

    assert report["generated"] == 0
    assert report["stopped"] == "events_unreachable"
    assert calls == []


def test_a_5xx_on_post_stops_the_batch(wired):
    worker, calls, monkeypatch = wired
    events = [_event(i, importance=1.0 - i / 100, published="2026-08-01T00:00:00Z")
              for i in range(1, 5)]
    fake = FakeEvents(events, post_status=503)
    monkeypatch.setattr(worker, "_http", fake)

    report = worker.run_once(now="2026-08-06T00:00:00Z")

    assert len(fake.posts) == 1
    assert report["stopped"] == "events_unreachable"


# --- §9.12, the clause that decides whether the ablation is honest -------------------------------

def test_enrichment_as_of_is_published_at_plus_the_lag(wired):
    """Every posted enrichment carries `enrichmentAsOf == publishedAt + ENRICH_LAG_HOURS`, and the
    prior-events novelty read for that event uses the SAME value (contract §9.12).

    Never "the last 30 days from now": a backlog event enriched today must not see events that
    happened after it, and no network call reveals the leak — every leakage test in the programme
    patches the HTTP client and would pass.
    """
    worker, calls, monkeypatch = wired
    monkeypatch.setenv("ENRICH_LAG_HOURS", "24")
    events = [
        _event(1, importance=0.9, published="2026-08-01T13:00:00Z"),
        _event(2, importance=0.8, published="2026-07-15T06:30:00Z"),
    ]
    fake = FakeEvents(events)
    monkeypatch.setattr(worker, "_http", fake)

    worker.run_once(now="2026-08-20T00:00:00Z")

    expected = {
        "evt_0000000000000001": "2026-08-02T13:00:00Z",
        "evt_0000000000000002": "2026-07-16T06:30:00Z",
    }
    assert len(fake.posts) == 2
    for url, body in fake.posts:
        event_id = url.rsplit("/", 2)[-2]
        assert body["enrichmentAsOf"] == expected[event_id]

    # the prior-events read for each event used that same cutoff, not "now"
    prior_reads = [p for _, p in fake.gets if "tickers" in p]
    assert sorted(p["as_of"] for p in prior_reads) == sorted(expected.values())
    assert all(p["as_of"] != "2026-08-20T00:00:00Z" for p in prior_reads)


def test_the_queue_selection_hides_events_whose_cutoff_has_not_arrived(wired):
    """§9.12 rule 1 binds the queue selection too. An event whose own cutoff is still in the future
    is not eligible: enriching it would stamp a cutoff the enrichment had not actually reached."""
    worker, calls, monkeypatch = wired
    monkeypatch.setenv("ENRICH_LAG_HOURS", "24")
    events = [
        _event(1, importance=0.9, published="2026-08-01T00:00:00Z"),   # cutoff 08-02, past
        _event(2, importance=0.9, published="2026-08-05T23:00:00Z"),   # cutoff 08-06, future
    ]
    events[1]["firstSeenAt"] = "2026-08-05T23:00:00Z"
    fake = FakeEvents(events)
    monkeypatch.setattr(worker, "_http", fake)

    worker.run_once(now="2026-08-06T00:00:00Z")

    # The queue itself is read LIVE — a global `as_of = now − lag` would also filter on
    # first_seen_at and hide every freshly ingested backlog event (see _fetch_queue).
    queue_reads = [p for _, p in fake.gets if p.get("enrichmentState") == "raw"]
    assert queue_reads[0]["as_of"] == "2026-08-06T00:00:00Z"
    posted = [url.rsplit("/", 2)[-2] for url, _ in fake.posts]
    assert posted == ["evt_0000000000000001"]


def test_the_prior_events_read_excludes_the_event_itself(wired):
    worker, calls, monkeypatch = wired
    events = [_event(1, importance=0.9, published="2026-08-01T00:00:00Z")]
    fake = FakeEvents(events)
    monkeypatch.setattr(worker, "_http", fake)

    worker.run_once(now="2026-08-06T00:00:00Z")

    prompt = calls[0]["messages"][-1]["content"]
    assert "PRIOR EVENTS" in prompt
    # the event under enrichment is the EVENT block, and must not also appear as its own prior
    assert prompt.count("evt_0000000000000001") <= 1


# --- source-level gate (contract §9.22) ----------------------------------------------------------

def test_the_worker_has_no_loop_and_no_sleep():
    """`grep -n "while True\\|time.sleep" services/llm/app/enrich_worker.py` returns nothing.

    §9.22: one bounded pass, then exit. Repetition is the operator's cron, outside this codebase —
    a timer here would be a model call caused by a scheduler tick, which invariant #4 forbids
    without qualification.
    """
    source = (APP_DIR / "enrich_worker.py").read_text(encoding="utf-8")
    assert re.search(r"while\s+True", source) is None
    assert "time.sleep" not in source
    assert "ENRICH_INTERVAL_SECONDS" not in source
    tree = ast.parse(source)
    for node in ast.walk(tree):
        assert not isinstance(node, ast.While), "a while loop in the single-pass worker"


def test_no_scheduler_primitive_anywhere_in_this_lane():
    for name in LANE_MODULES:
        source = (APP_DIR / f"{name}.py").read_text(encoding="utf-8")
        for forbidden in ("sched.", "APScheduler", "croniter", "asyncio.sleep",
                          "ENRICH_INTERVAL_SECONDS"):
            assert forbidden not in source, f"{name}.py names {forbidden}"


def test_the_worker_is_runnable_as_a_module():
    """`python -m app.enrich_worker` is the entry point — a process, invoked deliberately."""
    from app import enrich_worker

    assert callable(enrich_worker.main)
    source = (APP_DIR / "enrich_worker.py").read_text(encoding="utf-8")
    assert '__name__ == "__main__"' in source


def test_runtime_dependencies_remain_explicitly_pinned():
    req = (APP_DIR.parent / "requirements.txt").read_text(encoding="utf-8")
    assert sorted(l.strip() for l in req.splitlines() if l.strip()) == [
        "fastapi==0.115.6", "psycopg[binary]==3.2.9", "pydantic==2.10.4",
        "requests==2.32.3", "uvicorn[standard]==0.34.0",
    ]


# --- the payload the worker actually posts -------------------------------------------------------

def test_the_posted_payload_carries_the_audit_block_and_no_forbidden_field(wired):
    worker, calls, monkeypatch = wired
    fake = FakeEvents([_event(1, importance=0.9, published="2026-08-01T00:00:00Z")])
    monkeypatch.setattr(worker, "_http", fake)

    worker.run_once(now="2026-08-06T00:00:00Z")

    body = fake.posts[0][1]
    assert set(body) == {"whyItMatters", "keyFacts", "tickers", "audit", "enrichmentAsOf"}
    assert body["audit"] == {
        "modelUsed": "qwen3-14b",
        "promptVersion": "event-agent@1",
        "schemaVersion": "event@1",
        "reasoningMode": "non_thinking",
        "enrichedAt": "2026-08-06T00:00:00Z",
    }
    blob = json.dumps(body)
    for forbidden in ("isNovel", "severity", "relevance", "importance", "novelty",
                      "clusterKey", "occurredAt", "publishedAt", "sourceTier"):
        assert forbidden not in blob, forbidden


def test_the_stub_path_posts_nothing(wired):
    """Offline, `_stub_label` gives `stub:offline` and the honest outcome is a failure record —
    never an invented `whyItMatters`.

    It also posts NOTHING, because `POST /events/{id}/enrichment` sets `enrichment_state =
    'enriched'` unconditionally: there is no way to record `failed` through it. See the handoff's
    "Required changes I did NOT make".
    """
    worker, calls, monkeypatch = wired
    fake = FakeEvents([_event(1, importance=0.9, published="2026-08-01T00:00:00Z")])
    monkeypatch.setattr(worker, "_http", fake)

    def offline(messages, *, json_mode=True, mode=None):
        return "", "stub:offline", {"reasoningModeUsed": None, "degraded": False}

    monkeypatch.setattr(worker.llm, "call_model_meta", offline)

    report = worker.run_once(now="2026-08-06T00:00:00Z")

    assert fake.posts == []
    assert report["failed"] == 1
    assert report["posted"] == 0
    assert report["failures"][0]["modelUsed"] == "stub:offline"


def test_the_worker_requests_non_thinking_mode(wired):
    worker, calls, monkeypatch = wired
    fake = FakeEvents([_event(1, importance=0.9, published="2026-08-01T00:00:00Z")])
    monkeypatch.setattr(worker, "_http", fake)
    worker.run_once(now="2026-08-06T00:00:00Z")
    assert calls[0]["mode"] == "non_thinking"


def test_a_degraded_mode_is_recorded_truthfully(wired):
    """The server refused the flag and thinking stayed on. Recording `non_thinking` anyway would
    invalidate the Wave 5A mode ablation by attributing a run to a cause it did not have."""
    worker, calls, monkeypatch = wired
    fake = FakeEvents([_event(1, importance=0.9, published="2026-08-01T00:00:00Z")])
    monkeypatch.setattr(worker, "_http", fake)

    def refused(messages, *, json_mode=True, mode=None):
        return GOOD_REPLY, "qwen3-14b", {"reasoningModeRequested": "non_thinking",
                                         "reasoningModeUsed": "thinking", "degraded": True}

    monkeypatch.setattr(worker.llm, "call_model_meta", refused)
    worker.run_once(now="2026-08-06T00:00:00Z")

    assert fake.posts[0][1]["audit"]["reasoningMode"] == "thinking"


def test_never_fetches_a_provider(wired):
    """The worker reads `services/events` and talks to the model. That is the whole surface."""
    source = (APP_DIR / "enrich_worker.py").read_text(encoding="utf-8")
    for provider in ("marketaux", "alphavantage", "sec.gov", "tiingo", "yfinance",
                     "news.google", "fred", "financialmodelingprep"):
        assert provider not in source.lower()


# --- §9.13: the event is read AT ITS OWN CUTOFF where the store can serve it ----------------------

def test_the_event_is_re_read_at_its_own_cutoff(wired):
    """§9.13: `as_of` controls row visibility, never field values, so the LIVE row carries the
    `importance`, `documentCount`, `sourceTier` and `officialConfirmed` it has after documents that
    joined the cluster AFTER the cutoff. The worker re-reads at the cutoff so the model sees the
    state that actually existed then."""
    worker, calls, monkeypatch = wired
    live = _event(1, importance=0.9, published="2026-08-01T00:00:00Z")
    live["documentCount"] = 99                      # what it grew into later
    live["title"] = "TITLE AT THE CUTOFF"
    fake = FakeEvents([live])
    monkeypatch.setattr(worker, "_http", fake)

    report = worker.run_once(now="2026-08-06T00:00:00Z")

    projections = [p for _, p in fake.gets if set(p) == {"as_of"}]
    assert projections == [{"as_of": "2026-08-02T00:00:00Z"}]
    assert report["liveFallback"] == 0
    assert "TITLE AT THE CUTOFF" in calls[0]["messages"][-1]["content"]


def test_a_backlog_event_first_seen_after_its_cutoff_falls_back_and_is_counted(wired):
    """§1.1's as-of filter is `published_at <= as_of AND first_seen_at <= as_of`, so an event first
    seen AFTER its own cutoff genuinely did not exist at that cutoff and the read 404s.

    Measured against a running `services/events`, not assumed. Falling back to the live row is the
    only alternative to never enriching backlog at all — and the fallback is COUNTED, so the Wave 5A
    ablation can tell the two populations apart.
    """
    worker, calls, monkeypatch = wired
    backlog = _event(1, importance=0.9, published="2026-01-01T00:00:00Z")
    backlog["firstSeenAt"] = "2026-08-05T00:00:00Z"   # ingested months later
    fake = FakeEvents([backlog])
    monkeypatch.setattr(worker, "_http", fake)

    report = worker.run_once(now="2026-08-06T00:00:00Z")

    assert report["liveFallback"] == 1
    assert report["posted"] == 1
    assert fake.posts[0][1]["enrichmentAsOf"] == "2026-01-02T00:00:00Z"   # still its OWN cutoff


def test_a_5xx_on_the_cutoff_projection_stops_the_batch(wired):
    worker, calls, monkeypatch = wired
    fake = FakeEvents([_event(1, importance=0.9, published="2026-08-01T00:00:00Z")],
                      get_status=500)
    monkeypatch.setattr(worker, "_http", fake)
    report = worker.run_once(now="2026-08-06T00:00:00Z")
    assert report["stopped"] == "events_unreachable"
    assert calls == []
