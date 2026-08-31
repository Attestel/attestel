"""The ONE implementation of golden normalisation, shared by the pytest and the gate script.

Two callers with two copies of "which fields are volatile" is how a gate quietly stops checking the
thing it names. `tests/test_golden.py` imports this; so does `scripts/check-golden.py`, which points
at a RUNNING container. If you change a rule here, both move together.

Nothing in here is fuzzy. Every normalisation is a named field at a named path, applied
unconditionally, and a field that is absent when a rule expects it is an error rather than a silent
skip — a golden that tolerates missing fields cannot detect a removed one.
"""
from __future__ import annotations

import json

#: Fields replaced before comparison, as (dotted path, placeholder). The path is matched against the
#: payload literally; `[]` means "every element of this list".
#:
#: THE COMPLETE LIST — there is no wildcard and no regex fallback:
#:
#:   health.barStore.target credential-free PostgreSQL host/database identity.
#:   health.barStore.schema per-test schema in pytest, `analysis` in a running container.
#:   health.barStore.rows   row count. Deterministic under the pytest seed and NOT normalised
#:   health.barStore.tickers        there; normalised only for the live-container comparison, where
#:   health.barStore.timeframes     it depends on how much the container has ingested.
#:
#: `/analysis/{ticker}` at a fixed `as_of` has NO volatile field: `price.asOf` is the timestamp of
#: the last stored bar at or before the cutoff, so it is pinned by the request, not by the clock.
#: That is the whole reason the golden is captured through the point-in-time path — see README.
VOLATILE: dict[str, tuple[tuple[str, object], ...]] = {
    "health": (
        ("barStore.target", "<POSTGRES>"),
        ("barStore.schema", "<SCHEMA>"),
    ),
    "health_live": (
        ("barStore.target", "<POSTGRES>"),
        ("barStore.schema", "<SCHEMA>"),
        ("barStore.rows", "<ROWS>"),
        ("barStore.tickers", "<TICKERS>"),
        ("barStore.timeframes", "<TIMEFRAMES>"),
    ),
}


class MissingField(KeyError):
    """A normalisation rule named a field the payload does not have."""


def _set(obj: object, path: list[str], value: object) -> None:
    cur = obj
    for i, key in enumerate(path):
        if not isinstance(cur, dict) or key not in cur:
            raise MissingField(".".join(path[: i + 1]))
        if i == len(path) - 1:
            cur[key] = value
        else:
            cur = cur[key]


def normalize(payload: object, ruleset: str) -> object:
    """Return a deep copy of `payload` with `VOLATILE[ruleset]` applied.

    Raises MissingField if a rule names a field that is not there — which is a real finding about
    the payload, not a reason to skip the rule.
    """
    out = json.loads(json.dumps(payload))
    for path, placeholder in VOLATILE[ruleset]:
        _set(out, path.split("."), placeholder)
    return out


def shape(value: object) -> object:
    """Collapse a payload to its STRUCTURE: every scalar leaf becomes its type name.

    This is what a live container can be compared against, because its numbers are real market data
    and change every day while its shape must not. A list becomes a ONE-element list holding the
    merged shape of *all* its elements (`"<empty list>"` when it has none), so adding a field to one
    bar in a 260-bar series is caught without pinning the series length.

    Merging over every element, rather than sampling the first, is what makes a single capture
    honest: `indicators.series.sma200` is null for its first 199 bars and a float after, so its leaf
    shape is `"null|float"`. Sampling element 0 would have pinned it to `"null"` and then failed
    against any window long enough to warm the average up — a golden that only matches the window it
    was captured in.

    `None` is still its own shape, not "any": a field that carries null *everywhere* records `"null"`
    and a regression that nulls out a number is caught.
    """
    if isinstance(value, dict):
        return {k: shape(v) for k, v in value.items()}
    if isinstance(value, list):
        if not value:
            return ["<empty list>"]
        merged = shape(value[0])
        for item in value[1:]:
            merged = widen(merged, shape(item))
        return [merged]
    if value is None:
        return "null"
    if isinstance(value, bool):
        return "bool"
    if isinstance(value, int):
        return "int"
    if isinstance(value, float):
        return "float"
    if isinstance(value, str):
        return "str"
    return type(value).__name__


def widen(a: object, b: object) -> object:
    """Merge two shapes into one that accepts either — `"null|float"` for a field that is null in one
    element and a number in another. `shape` folds this over every element of a list."""
    if isinstance(a, dict) and isinstance(b, dict):
        return {k: widen(a[k], b[k]) if k in a and k in b else a.get(k, b.get(k))
                for k in list(a) + [k for k in b if k not in a]}
    if isinstance(a, list) and isinstance(b, list):
        if a == ["<empty list>"]:
            return b
        if b == ["<empty list>"]:
            return a
        return [widen(a[0], b[0])]
    if a == b:
        return a
    return "|".join(sorted(set(str(a).split("|")) | set(str(b).split("|"))))


def shape_accepts(golden: object, actual: object, path: str = "") -> list[str]:
    """Diff `actual`'s shape against a `golden` shape. Returns a list of human-readable mismatches;
    empty means it matches. `"null|float"` in the golden accepts either."""
    problems: list[str] = []
    here = path or "$"
    if isinstance(golden, dict):
        if not isinstance(actual, dict):
            return [f"{here}: expected an object, got {actual}"]
        for k in golden:
            if k not in actual:
                problems.append(f"{here}.{k}: MISSING from the response")
            else:
                problems += shape_accepts(golden[k], actual[k], f"{here}.{k}")
        for k in actual:
            if k not in golden:
                problems.append(f"{here}.{k}: NEW field, not in the golden")
        return problems
    if isinstance(golden, list):
        if not isinstance(actual, list):
            return [f"{here}: expected a list, got {actual}"]
        if actual == ["<empty list>"] or golden == ["<empty list>"]:
            return problems
        return shape_accepts(golden[0], actual[0], f"{here}[]")
    # Both sides may be alternations (`"null|float"`); the response is accepted when every type it
    # actually produced is one the golden allows. A golden that allows MORE than the response
    # produced is fine — a warmup-length difference is not a shape regression.
    unexpected = set(str(actual).split("|")) - set(str(golden).split("|"))
    if unexpected:
        return [f"{here}: golden says {golden!r}, response has {actual!r}"]
    return problems
