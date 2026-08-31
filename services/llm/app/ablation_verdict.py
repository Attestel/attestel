"""Serving the ablation verdict — contract §9.61, Wave 5 Lane 5A's exit criterion.

§9.20 gates the `direction` word on a stored ablation verdict under `data/eval/ablation`. That path
is on the SERVER and the gate is applied in the BROWSER, so as written the clause named a fact the
party responsible for enforcing it could not observe. §9.61 resolved it: **the analyst envelope
carries the verdict**, and binding 5 makes SERVING it — not producing it — 5A's exit criterion. A
ladder that writes a number to disk and exposes no field leaves the gate closed for the identical
reason one wave later.

THE THREE KEYS, AND WHY THIS FILE PROJECTS DOWN TO THEM
--------------------------------------------------------
`ablationVerdict: {validated, rung, horizon}`. `services/prediction/app/ablation.py::write_verdict`
writes those three plus a block of EVIDENCE (the verdict word, the split, the sample size, the
accuracy, the cutoff fingerprint, the runtimes). The evidence is for a human reading the file. What
reaches the browser is the three keys and nothing else — a client that matched on `verdict == "EDGE"`
instead of on `validated` would be reimplementing the ladder's own bar in JavaScript, and §9.61
binding 3 is explicit that the client matches, it does not judge.

FAIL CLOSED, THREE WAYS
-----------------------
1. No file, unreadable file, wrong schema ⇒ `None` ⇒ the gate stays closed and the surface reads
   "not yet validated". That is the ORDINARY state and must stay renderable (§9.61 binding 1).
2. A verdict for a different rung or horizon ⇒ not returned. §9.20 gates per rung AND horizon.
3. `validated: false` IS returned. It is a real answer — the ablation ran and did not license the
   direction — and it is not the same as an absent verdict. Both withhold the word; only one of
   them is evidence (binding 4). Swallowing it here would destroy that distinction on the way to
   the screen.

WHICH RUNG. `ABLATION_RUNG` names the rung this deployment's analyst pipeline corresponds to. It has
NO DEFAULT, for the same reason `MODEL_RUNTIME_URL` has none: a defaulted rung would match a verdict
earned by a different input bundle, which is §9.61 binding 3's coincidence with extra steps. Unset ⇒
no verdict is served and the gate stays closed.

NO MODEL CALL, NO CACHE, NO CLOCK. This is one small JSON read per analyst run — the same run that
just spent minutes on eight generations — so a cache would save nothing measurable and would let a
freshly-written verdict go unserved for its lifetime.
"""
from __future__ import annotations

import json
import logging
import os

from . import db

__all__ = ["VERDICT_SCHEMA", "RUNG_ENV", "VERDICT_PATH_ENV", "DEFAULT_VERDICT_PATH",
           "configured_rung", "verdict_path", "load_verdicts", "verdict_for"]

log = logging.getLogger("llm.ablation")

#: Written by `services/prediction/app/ablation.py`. Asserted on both sides, because a schema agreed
#: by comment is a schema that drifts.
VERDICT_SCHEMA = "ablation-verdict@1"

RUNG_ENV = "ABLATION_RUNG"
VERDICT_PATH_ENV = "ABLATION_VERDICT_PATH"

#: §9.20 names `data/eval/ablation` and this is that path. A PATH may have a default where an
#: ENDPOINT may not: a missing file is observed and returns `None`, so nothing is dialled, nothing
#: is invented, and the failure direction is closed.
DEFAULT_VERDICT_PATH = os.path.join("data", "eval", "ablation", "verdict.json")


def configured_rung() -> str:
    """The rung this deployment's analyst corresponds to, or `""`. No default — see the header."""
    return str(os.getenv(RUNG_ENV, "") or "").strip().upper()


def verdict_path() -> str:
    return str(os.getenv(VERDICT_PATH_ENV, "") or "").strip() or DEFAULT_VERDICT_PATH


def load_verdicts(path: str | None = None) -> dict:
    """`{"<RUNG>|<horizon>": {...}}`, or `{}`.

    Every failure returns `{}`: absent, unreadable, not JSON, wrong schema. An ablation verdict is a
    claim that licenses a directional word on a screen, and the correct behaviour when we cannot
    read the claim is to behave as though it was never made.
    """
    target = path or verdict_path()
    try:
        if path is None and db.enabled():
            raw = db.load_prediction_artifact("ablation-verdict.json")
            if raw is None:
                return {}
            payload = json.loads(raw)
        else:
            with open(target, encoding="utf-8") as fh:
                payload = json.load(fh)
    except FileNotFoundError:
        return {}
    except (OSError, ValueError, RuntimeError) as exc:
        log.warning("ablation verdict at %s is unreadable (%s); the direction gate stays closed",
                    target, exc)
        return {}
    except Exception as exc:  # PostgreSQL driver errors must fail closed too.
        log.warning("ablation verdict database read failed (%s); the direction gate stays closed",
                    type(exc).__name__)
        return {}
    if not isinstance(payload, dict) or payload.get("schema") != VERDICT_SCHEMA:
        log.warning("ablation verdict at %s has schema %r, expected %r; the gate stays closed",
                    target, (payload or {}).get("schema"), VERDICT_SCHEMA)
        return {}
    verdicts = payload.get("verdicts")
    return verdicts if isinstance(verdicts, dict) else {}


def verdict_for(horizon: str, *, rung: str | None = None, verdicts: dict | None = None):
    """§9.61's three keys for one horizon, or `None`.

    `verdicts` is passed in by `run_analyst` so one analyst run reads the file once rather than once
    per horizon — four identical reads would be four chances for the file to change mid-run and for
    two theses in one envelope to disagree about whether the ladder has run.
    """
    active = configured_rung() if rung is None else str(rung or "").strip().upper()
    if not active or not horizon:
        return None
    table = load_verdicts() if verdicts is None else verdicts
    row = table.get(f"{active}|{horizon}")
    if not isinstance(row, dict):
        return None
    if str(row.get("rung") or "").strip().upper() != active:
        # The key said one rung and the row says another. Two readings of the same fact, so trust
        # neither: a verdict this confused cannot be matched against the read it would license.
        log.warning("ablation verdict row for %s|%s names rung %r; refusing it",
                    active, horizon, row.get("rung"))
        return None
    if str(row.get("horizon") or "") != horizon:
        return None
    if not isinstance(row.get("validated"), bool):
        # `validated` must be a real boolean. A missing or stringly-typed one would be truthy in
        # the wrong places, and this is the field the whole gate turns on.
        return None
    # PROJECTED to the three keys. The evidence in the file is for a human reading the file; a
    # client that could see `verdict: "EDGE"` would eventually match on it instead of `validated`.
    return {"validated": row["validated"], "rung": active, "horizon": horizon}
