"""§9.12 as the INTEGRATOR verified it: what the model actually saw, not what was requested.

Wave 2 Lane 2B's `test_enrichment_as_of_is_published_at_plus_the_lag` asserts the `as_of` REQUEST
PARAMETER on the prior-events read. That is necessary and not sufficient. The failure §9.12 exists
to prevent is a backlog event enriched today against events that happened after it, and that
failure is invisible to a parameter assertion if the parameter is right but the prompt is built
from something else. It is invisible to every leakage test in the programme too, because **no
network call occurs** — they all patch the HTTP client and pass regardless.

So this asserts on the prompt: the PRIOR EVENTS block the model reads must contain no event
published after the cutoff.

One trap worth recording, because it cost a rewrite: that block renders `eventType`, `title` and
`publishedAt` and **no `id`**. An assertion of the form `assert event_id not in prompt` is
therefore VACUOUS — the id never appears, leaked or not. The timestamps are what the model reads,
so the timestamps are what is asserted.
"""
from __future__ import annotations

import json

from tests.test_enrich_worker import FakeEvents, _event, wired  # noqa: F401

CUTOFF = "2026-07-02T12:00:00Z"       # the backlog event's publishedAt + 24 h
PRIOR_STAMP = "2026-06-20T12:00:00Z"  # the one event genuinely before that cutoff


def _prior_events_block(prompt: str) -> list[dict]:
    """The JSON array under `PRIOR EVENTS`, parsed by bracket matching rather than a regex."""
    start = prompt.index("[", prompt.index("PRIOR EVENTS"))
    depth = 0
    for i, ch in enumerate(prompt[start:], start):
        depth += (ch == "[") - (ch == "]")
        if depth == 0:
            return json.loads(prompt[start:i + 1])
    raise AssertionError("the PRIOR EVENTS array is unterminated")


def test_the_model_never_sees_an_event_from_after_the_cutoff(wired):
    worker, calls, monkeypatch = wired
    monkeypatch.setenv("ENRICH_LAG_HOURS", "24")

    backlog = _event(1, importance=0.9, published="2026-07-01T12:00:00Z", ticker="NVDA")

    # Same ticker, all AFTER the backlog event's cutoff. Already enriched, so they are not queue
    # candidates themselves — they exist only as events that could leak into the comparison.
    future = []
    for i, day in enumerate(("2026-07-10", "2026-08-01", "2026-08-15"), start=2):
        ev = _event(i, importance=0.8, published=f"{day}T12:00:00Z", ticker="NVDA")
        ev["enrichmentState"] = "enriched"
        future.append(ev)

    # One genuinely prior event. Without it the block would be empty and the test vacuous.
    prior = _event(9, importance=0.7, published=PRIOR_STAMP, ticker="NVDA")
    prior["enrichmentState"] = "enriched"

    fake = FakeEvents([backlog, prior] + future)
    monkeypatch.setattr(worker, "_http", fake)

    # `now` is six weeks after the backlog event. An implementation reading "the last 30 days from
    # now" would pick up all three future events here.
    worker.run_once(now="2026-08-20T00:00:00Z")

    posts = {u.rsplit("/", 2)[-2]: b for u, b in fake.posts}
    assert "evt_0000000000000001" in posts, "the backlog event was never enriched"
    assert posts["evt_0000000000000001"]["enrichmentAsOf"] == CUTOFF

    assert calls, "no model call was made"
    stamps = sorted(e["publishedAt"] for e in _prior_events_block(calls[0]["messages"][-1]["content"]))

    leaked = [t for t in stamps if t > CUTOFF]
    assert not leaked, (
        f"prior events {leaked} are AFTER the cutoff {CUTOFF} — the model is reasoning about a "
        "backlog event using events that had not happened yet, and no network call occurred, so "
        "every HTTP-patching leakage test in the programme passes on this"
    )
    assert stamps == [PRIOR_STAMP], (
        f"prior events = {stamps}, want exactly [{PRIOR_STAMP!r}] — an empty block would make this "
        "test vacuous"
    )


def test_the_id_based_form_of_this_assertion_would_be_vacuous(wired):
    """Guards the trap in this file's docstring: if the block ever starts carrying `id`, the note
    above is stale and should be removed. If it never does, this test documents why."""
    worker, calls, monkeypatch = wired
    fake = FakeEvents([_event(1, importance=0.9, published="2026-08-01T00:00:00Z")])
    monkeypatch.setattr(worker, "_http", fake)
    worker.run_once(now="2026-08-06T00:00:00Z")

    block = _prior_events_block(calls[0]["messages"][-1]["content"])
    for entry in block:
        assert "id" not in entry, (
            "the PRIOR EVENTS block now carries `id` — an id-based leakage assertion is no longer "
            "vacuous, and this file's docstring should be updated"
        )
