"""Proves the Wave 0 analyst-router seam carries EXACTLY what Lane 3B attached to it, and nothing
else.

The seam exists so Wave 3 Lane B can add analyst routes from its own analyst.py without editing
main.py. Wave 0 shipped it empty and pinned the path set as a literal, scoping that assertion in its
own header: "Wave 3B updates this set in the same commit that adds its routes." Wave 3B has now
landed `services/llm/app/analyst.py`, which decorates the router with one route (§9.28: the lane
decorates the seam and hands the integrator NO include_router line — doing both would register the
routes twice), so this file is updated here, per that instruction and only that far.

EXISTING_PATHS stays a LITERAL rather than becoming `derived from the app`. Deriving it would make
this file assert `app == app`, and the whole point is that a route arriving without a lane
deliberately adding it to this list must fail.
"""
from __future__ import annotations

from app.analyst_router import analyst_router
from app import main as app_module
from app.main import app

# The exact path set the llm service exposes. A seam that registers anything NOT listed here breaks
# this, which is the point.
EXISTING_PATHS = {
    "/analyst",          # Wave 3 Lane 3B, via the seam — services/llm/app/analyst.py
    "/assistant",
    "/brief",
    "/chat",
    "/committee",
    "/committee/{ticker}",
    "/digest",
    "/health",
    "/investigation",
    "/lens",
    "/lenses",
    "/personas",
    "/personas/{persona_id}",
    "/portfolio-review",
    "/portfolio-scenario",
    "/read",
    "/reads/{ticker}",
    "/reads/{ticker}/diff",
    "/scenario",
    "/score-earnings",
    "/thesis-check",
    "/transcript",
    "/transcript/{ticker}",
}


def test_analyst_router_carries_exactly_one_route():
    """One route, POST-only. A GET invites a prefetch, and a prefetch on a route that costs eight
    sequential model generations is an invariant #4 violation nobody wrote a line of code to cause.
    """
    paths = {(r.path, tuple(sorted(r.methods))) for r in analyst_router.routes}
    assert paths == {("/analyst", ("POST",))}, paths


def test_openapi_paths_unchanged():
    assert set(app.openapi()["paths"].keys()) == EXISTING_PATHS


def _registered_paths(routes) -> list[str]:
    """Every path the app actually serves, flattened — a LIST, not a set, because the double
    registration this file exists to catch shows up only as a repeat.

    FastAPI 0.141 stopped splicing an included router's routes into the parent's `.routes`; it
    appends one opaque `_IncludedRouter` that keeps them on `.original_router` instead. `/analyst`
    is the seam's entire payload and arrives that way, so reading `app.routes` flat stopped seeing
    it at all and this test read "registered zero times" — a green route (the 405 test below still
    passes) reported as missing. Descending keeps the count a count of real registrations on both
    the pre- and post-0.141 shapes.
    """
    paths: list[str] = []
    for route in routes:
        path = getattr(route, "path", None)
        if path is not None:
            paths.append(path)
        included = getattr(route, "original_router", None)
        if included is not None:
            paths.extend(_registered_paths(included.routes))
    return paths


def test_the_analyst_route_arrived_through_the_seam_and_not_through_main():
    """§9.28: `main.py` includes `analyst_router` once and knows nothing else about this lane. If a
    second registration ever appears, the route is served twice and this catches it."""
    main_src = open(app_module.__file__, encoding="utf-8").read()
    assert main_src.count("include_router(analyst_router)") == 1
    assert "from .analyst import" not in main_src, (
        "main.py must not import analyst.py directly — the seam is the only coupling")

    assert _registered_paths(app.routes).count("/analyst") == 1


def test_the_analyst_route_rejects_a_get():
    from fastapi.testclient import TestClient

    with TestClient(app) as client:
        assert client.get("/analyst").status_code == 405
