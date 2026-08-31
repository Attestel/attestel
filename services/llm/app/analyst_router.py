"""Wave 0 seam. An empty APIRouter that services/llm/app/main.py includes once, so Wave 3 Lane B can
add analyst routes from analyst.py without editing main.py.

Wave 3B writes services/llm/app/analyst.py and decorates it:

    from .analyst_router import analyst_router

    @analyst_router.post("/analyst")
    def analyst(req: AnalystRequest): ...

The tolerant import below is what makes that work: analyst.py does not exist yet, and its absence
must not break the service. Any OTHER missing module is a real error and is re-raised.

It uses importlib rather than `from . import analyst` deliberately. The `from package import name`
form falls back to an ATTRIBUTE lookup when the submodule is absent and then raises ImportError
("cannot import name 'analyst' from 'app'"), which a `except ModuleNotFoundError` clause does not
catch — so that spelling would break the whole service today. importlib.import_module raises
ModuleNotFoundError with a `name` we can compare, which is also the mechanism
services/events/app/main.py uses for its OPTIONAL_ROUTERS loop.

Binding rule (EVENT_CONTRACTS.md §9.28): Lane 3B decorates this router and hands the integrator
NOTHING — no manual include_router line for services/llm. Doing both registers the routes twice.
"""
from __future__ import annotations

import importlib

from fastapi import APIRouter

analyst_router = APIRouter()

try:  # pragma: no cover - exercised once analyst.py lands in Wave 3B
    # Imported for its side effect: analyst.py decorates analyst_router at module scope.
    importlib.import_module(".analyst", __package__)
except ModuleNotFoundError as exc:
    if exc.name != f"{__package__}.analyst":
        raise
