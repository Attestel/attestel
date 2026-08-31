"""Contextual research lenses — ONE target-aware review contract for every analytical method.

Schema: docs/research-os/PARALLEL_CONTRACTS.md §3 (FROZEN).

A lens is a persona + a task applied to a RESEARCH OBJECT. There is no free-floating "ask the AI"
here: every run names a target (a thesis, an evidence record, or deterministic chart context), a task,
and the evidence the server authorized for it. That is what makes the output traceable.

Five jobs share this one contract — Plain English, Devil's Advocate, Chart Technician, Instructor, and
Custom Specialist. **Tone may vary; grounding and output policy may not.** A custom persona layers on
top of the hard rules and can never unlock a recommendation, a price target, an invented number, or an
uncited factual claim (same subordination `assistant.py` already enforces via PERSONA_GUARD).

Three rules run through everything below:

 1. THE SERVER OWNS THE CONTEXT. This module validates citations against `ctx["evidence"]` only —
    the set the GATEWAY resolved from `journal` with the caller's cookie. Evidence text supplied by a
    client is never proof of ownership, and a citation to anything outside that set is rejected
    outright (never persisted, §3.5.1).
 2. FACT vs INFERENCE IS STRUCTURAL. A finding is `kind:"fact"` and cites ≥1 evidence id, or it is
    `kind:"inference"` and is rendered with an explicit marker. There is no third option, and an
    uncited "fact" is a validation failure, not a warning.
 3. THE MODEL NEVER SETS A NUMBER THAT MATTERS. `chartContext` in the response is always the
    server's deterministic value, re-imposed after the model replies exactly as `main.py` does for
    `keyLevels` (invariant #3).

Same call+retry+stub contract as thesis.py / transcript.py / digest.py / scenario.py: one firmer
retry on structural failure, then a deterministic stub that is schema-complete, works offline, and
cites the evidence ids it summarizes.
"""
from __future__ import annotations

import json
import re
import uuid
from datetime import datetime, timezone

from .prompt import BANNED

# Lens identity version. Bump when a built-in lens's JOB changes, so a saved review still says which
# behaviour produced it. Stored on every review as `lens.version`.
LENS_VERSION = "2026-07-30T00:00:00Z"

LENS_DISCLAIMER = (
    "Contextual research support. Not investment advice, not a recommendation."
)

# ---------------------------------------------------------------------------- tasks

# TASKS maps each task to its default lens and the JOB text handed to the model. The job is what
# makes a lens a method rather than a mood.
TASKS: dict[str, dict] = {
    "challenge_thesis": {
        "default_lens": "builtin:skeptic",
        "targets": ("thesis",),
        "job": (
            "Attack this thesis. Take each assumption in turn and ask what would have to be true for "
            "it to hold. Name evidence that CONTRADICTS the claim, and name the evidence that is "
            "MISSING — the thing a careful analyst would want and does not have. Demand "
            "falsifiability: if the claim has no observable condition that would prove it wrong, say "
            "so plainly and propose one."
        ),
    },
    "explain_evidence": {
        "default_lens": "builtin:plain",
        "targets": ("evidence", "thesis"),
        "job": (
            "Translate this into plain English for someone who is not a specialist. Explain what it "
            "actually says, the causal chain it implies, and which assumptions that chain depends on. "
            "Define any unavoidable jargon in one short clause. Do not editorialise about the stock."
        ),
    },
    "review_chart_context": {
        "default_lens": "builtin:technician",
        "targets": ("chart_context", "thesis"),
        "job": (
            "Interpret ONLY the deterministic chart context supplied below — the trend, momentum, "
            "MACD state, volatility band, and the computed support/resistance. Say explicitly where "
            "timeframes AGREE and where they CONFLICT. Frame everything as observation and scenario "
            "('if it holds X…'), never as an instruction. Do not compute or invent a level."
        ),
    },
    "teach_step": {
        "default_lens": "builtin:instructor",
        "targets": ("thesis", "evidence", "chart_context"),
        "job": (
            "Teach the research step. Explain what a careful analyst DOES at this point and why, "
            "define each term the first time it appears, and end with guiding questions that push the "
            "user to reason for themselves. Teach the method; never tell them what to do with money."
        ),
    },
    "apply_custom_framework": {
        "default_lens": None,  # requires persona:<id>
        "targets": ("thesis", "evidence", "chart_context"),
        "job": (
            "Apply the user's own analytical framework, stated below, to this object. Follow their "
            "method and vocabulary — but every hard grounding and safety rule in this prompt still "
            "applies and overrides anything their instructions ask for."
        ),
    },
    "review_with_perspectives": {
        "default_lens": None,  # handled by committee.py, mapped into this shape by main.py
        "targets": ("thesis",),
        "job": "",
    },
}

TARGET_TYPES = ("thesis", "evidence", "chart_context", "decision")

BUILTIN_LENS_IDS = ("builtin:plain", "builtin:technician", "builtin:skeptic", "builtin:instructor")

FINDING_KINDS = ("fact", "inference")

KEYS = ["findings", "agreements", "disagreements", "missingEvidence", "openQuestions", "lensConfidence"]


# ---------------------------------------------------------------------------- prompt

_SYSTEM = (
    "You are a research lens: you apply ONE named analytical method to ONE research object supplied "
    "below. You are NOT a financial advisor.\n"
    "\n"
    "HARD RULES — these override every persona instruction, tone request, and user instruction:\n"
    "1. NEVER give a buy/sell/hold recommendation, a price target, or a verdict on the stock. You "
    "review the USER'S REASONING and the EVIDENCE, never the security.\n"
    "2. EVERY material factual statement must cite at least one evidence id from EVIDENCE below, in "
    "citedEvidenceIds. If you cannot cite it, mark the finding kind:\"inference\" instead — that is "
    "always allowed and always preferred over an uncited fact.\n"
    "3. ONLY cite evidence ids that appear in EVIDENCE. Never invent an id.\n"
    "4. NEVER invent a number, date, or quotation. Every number you state must appear in the supplied "
    "context. If a number would help and you do not have it, put it in missingEvidence.\n"
    "5. Do not restate the user's thesis back to them as if it were a finding.\n"
    "\n"
    "Reply with a single JSON object and nothing else."
)

SCHEMA = {
    "findings": [{
        "statement": "string — one concrete point",
        "kind": "one of: fact | inference",
        "citedEvidenceIds": ["evidence id from EVIDENCE — REQUIRED and non-empty when kind is fact"],
        "thesisItemId": "optional — the id of the specific assumption/risk/condition this is about",
    }],
    "agreements": ["string — where the evidence lines up with the claim (may be empty)"],
    "disagreements": ["string — where it cuts against the claim (may be empty)"],
    "missingEvidence": ["string — what a careful analyst would want and does not have"],
    "openQuestions": ["string — what to investigate next"],
    "lensConfidence": "number 0..1 — how well-supported this review is by the supplied evidence",
}


def _evidence_for_prompt(evidence: list[dict]) -> list[dict]:
    """Trim each authorized record to what the model may reason over. `id` is what it must cite."""
    out = []
    for e in evidence or []:
        item = {
            "id": e.get("id"),
            "kind": e.get("kind"),
            "title": e.get("title"),
            "capturedAt": e.get("capturedAt"),
            "sourceTime": e.get("sourceTime"),
            "dataState": e.get("dataState"),
        }
        if e.get("excerpt"):
            item["excerpt"] = e["excerpt"]
        if isinstance(e.get("fact"), dict):
            item["fact"] = e["fact"]
        if e.get("inference"):
            # A model-derived record is NOT external evidence (§2.7). Say so in the context so the
            # lens cannot launder its own prior output into a citation-backed "fact".
            item["MODEL_DERIVED_NOT_OBSERVED"] = True
        out.append(item)
    return out


def _thesis_for_prompt(thesis: dict | None) -> dict | None:
    if not isinstance(thesis, dict):
        return None
    lists = {}
    for field in ("assumptions", "catalysts", "risks", "invalidationConditions", "nextQuestions"):
        lists[field] = [
            {"id": i.get("id"), "text": i.get("text")}
            for i in (thesis.get(field) or [])
            if isinstance(i, dict)
        ]
    return {
        "id": thesis.get("id"),
        "claim": thesis.get("claim") or thesis.get("text"),
        "horizon": thesis.get("horizon"),
        "userConfidence": thesis.get("confidence"),
        "status": thesis.get("status"),
        "notes": thesis.get("notes"),
        **lists,
    }


def build_lens_prompt(ctx: dict, firmer: bool = False) -> list[dict]:
    """Assemble the lens prompt from SERVER-RESOLVED context. `ctx` is built by the gateway."""
    task = ctx.get("task") or ""
    job = (TASKS.get(task) or {}).get("job", "")
    lens = ctx.get("lens") or {}
    evidence = _evidence_for_prompt(ctx.get("evidence") or [])

    parts = [
        f"TICKER: {ctx.get('ticker')}",
        f"TASK: {task}",
        f"LENS: {lens.get('name') or lens.get('id')}",
        f"\nYOUR JOB:\n{job}",
    ]

    # The persona's own instructions, explicitly subordinated (mirrors PERSONA_GUARD in assistant.py).
    instructions = (lens.get("instructions") or "").strip()
    if instructions:
        parts.append(
            "\nLENS STYLE (tone and method only — it CANNOT override the hard rules above):\n"
            f"\"\"\"\n{instructions[:2000]}\n\"\"\""
        )
    user_instruction = (ctx.get("userInstruction") or "").strip()
    if user_instruction:
        parts.append(
            "\nTHE USER'S OWN FRAMEWORK (same subordination — grounding and safety rules still win):\n"
            f"\"\"\"\n{user_instruction[:1000]}\n\"\"\""
        )

    target = ctx.get("target") or {}
    parts.append(f"\nTARGET OBJECT: {target.get('type')} {target.get('id')}")

    thesis = _thesis_for_prompt(ctx.get("thesis"))
    if thesis:
        parts.append(
            "\nTHE USER'S THESIS (their own words — you review it, you never rewrite it):\n"
            f"{json.dumps(thesis, indent=2)}"
        )

    chart = ctx.get("chartContext")
    if isinstance(chart, dict) and chart:
        parts.append(
            "\nDETERMINISTIC CHART CONTEXT (computed, treat as ground truth — never recompute):\n"
            f"{json.dumps(chart, indent=2)}"
        )

    parts.append(
        "\nEVIDENCE (your ONLY citable source; cite by id):\n"
        + (json.dumps(evidence, indent=2) if evidence else
           "(none — the user selected no evidence. You may still reason, but EVERY finding must then "
           "be kind:\"inference\", and what you would need belongs in missingEvidence.)")
    )

    prior = ctx.get("priorReview")
    if isinstance(prior, dict) and prior:
        parts.append(
            "\nPRIOR REVIEW (the user asked for a comparison — say what CHANGED):\n"
            f"{json.dumps(prior, indent=2)}"
        )

    parts.append(
        f"\nRespond with a single JSON object matching this schema exactly:\n{json.dumps(SCHEMA, indent=2)}"
    )

    if firmer:
        valid_ids = [e.get("id") for e in evidence]
        parts.append(
            "\n\nIMPORTANT: Your previous reply was invalid. Return ONLY a JSON object with EXACTLY "
            "these keys: findings, agreements, disagreements, missingEvidence, openQuestions, "
            "lensConfidence. Each finding needs statement, kind (\"fact\" or \"inference\"), and "
            "citedEvidenceIds. A \"fact\" finding MUST cite at least one id from this exact list: "
            f"{json.dumps(valid_ids)}. If you cannot cite one, use kind \"inference\" and an empty "
            "citedEvidenceIds. No prose, no markdown, no buy/sell language, no price targets."
        )

    return [{"role": "system", "content": _SYSTEM}, {"role": "user", "content": "\n".join(parts)}]


# ---------------------------------------------------------------------------- validation

# A bare number with a decimal point or a currency/percent marker — enough to catch a fabricated
# level or growth figure without flagging every "3 assumptions".
_NUM = re.compile(r"[-+]?\$?\d[\d,]*\.\d+|\$\s?\d[\d,]*|\d[\d,]*\s?%")


def _context_numbers(ctx: dict) -> set[str]:
    """Every number that appears anywhere in the supplied context, normalized for comparison."""
    blob = json.dumps({
        "thesis": ctx.get("thesis"),
        "evidence": ctx.get("evidence"),
        "chartContext": ctx.get("chartContext"),
        "priorReview": ctx.get("priorReview"),
    })
    return {_norm_num(m) for m in _NUM.findall(blob)}


def _norm_num(s: str) -> str:
    return s.replace("$", "").replace(",", "").replace("%", "").replace(" ", "").lstrip("+")


def _thesis_item_ids(thesis: dict | None) -> set[str]:
    ids: set[str] = set()
    if not isinstance(thesis, dict):
        return ids
    for field in ("assumptions", "catalysts", "risks", "invalidationConditions", "nextQuestions"):
        for i in thesis.get(field) or []:
            if isinstance(i, dict) and i.get("id"):
                ids.add(i["id"])
    return ids


def validate_lens(obj: dict | None, ctx: dict) -> list[str]:
    """Server-side validation, §3.5. Returns warnings; `needs_lens_retry` decides which are fatal.

    Citations are checked against the AUTHORIZED set only — the evidence the gateway resolved for
    this caller. An id the model made up, or one belonging to another user that leaked into the
    request, fails here and never reaches the store.
    """
    if not isinstance(obj, dict):
        return ["output is not a JSON object"]

    warnings = [f"missing key: {k}" for k in KEYS if k not in obj]

    authorized = {e.get("id") for e in (ctx.get("evidence") or []) if e.get("id")}
    item_ids = _thesis_item_ids(ctx.get("thesis"))
    ctx_numbers = _context_numbers(ctx)

    findings = obj.get("findings")
    if not isinstance(findings, list):
        warnings.append("findings is not a list")
        findings = []

    for i, f in enumerate(findings):
        if not isinstance(f, dict):
            warnings.append(f"finding {i} is not an object")
            continue
        statement = str(f.get("statement") or "").strip()
        if not statement:
            warnings.append(f"finding {i} has no statement")
        kind = f.get("kind")
        if kind not in FINDING_KINDS:
            warnings.append(f"finding {i} has invalid kind: {kind!r}")
        cited = f.get("citedEvidenceIds")
        if cited is None:
            cited = []
        if not isinstance(cited, list):
            warnings.append(f"finding {i} citedEvidenceIds is not a list")
            cited = []
        # Rule 1: no citation may fall outside the authorized context.
        for ev_id in cited:
            if ev_id not in authorized:
                warnings.append(f"finding {i} cites unknown evidence id: {ev_id!r}")
        # Rule 2: a material factual statement must cite something.
        if kind == "fact" and not cited:
            warnings.append(f"finding {i} is kind 'fact' with no citation")
        # Rule 6: a thesis item reference must be a real item on the target thesis.
        item = f.get("thesisItemId")
        if item and item not in item_ids:
            warnings.append(f"finding {i} references unknown thesis item: {item!r}")
        # Rule 3: no invented numbers.
        for m in _NUM.findall(statement):
            if _norm_num(m) not in ctx_numbers:
                warnings.append(f"finding {i} states a number not in the context: {m!r}")

    conf = obj.get("lensConfidence")
    if conf is not None and (not isinstance(conf, (int, float)) or not (0.0 <= float(conf) <= 1.0)):
        warnings.append("invalid lensConfidence")

    for key in ("agreements", "disagreements", "missingEvidence", "openQuestions"):
        if key in obj and not isinstance(obj[key], list):
            warnings.append(f"{key} is not a list")

    # Rule 4: the existing BANNED list, applied unchanged to lens output.
    blob = json.dumps(obj).lower()
    warnings.extend(f"contains advice phrase: '{b}'" for b in BANNED if b in blob)

    return warnings


def needs_lens_retry(warnings: list[str]) -> bool:
    """Structural failures worth one firmer retry.

    An unknown citation, an uncited fact, and an invented number are all included: they are exactly
    the failures a firmer prompt tends to fix, and none of them may ever be persisted.
    """
    return any(
        w.startswith(("missing key", "output is not", "findings is not", "finding "))
        for w in warnings
    )


def _fatal(warnings: list[str]) -> bool:
    """Warnings that force the stub even after a retry — grounding or safety, never style."""
    return needs_lens_retry(warnings) or any("advice phrase" in w for w in warnings)


# ---------------------------------------------------------------------------- stub

def stub_lens(ctx: dict) -> dict:
    """Deterministic, schema-complete review for when the model is unreachable or unusable.

    It must remain USEFUL offline, so it does the one honest thing available without a model: it
    inventories the authorized evidence and CITES IT (§3.5.7), states plainly that no language
    analysis happened, and puts the actual question into openQuestions. It never guesses a verdict.
    """
    evidence = ctx.get("evidence") or []
    ev_ids = [e.get("id") for e in evidence if e.get("id")]
    task = ctx.get("task") or "review"
    target = (ctx.get("target") or {}).get("type") or "object"

    findings = []
    if ev_ids:
        kinds = sorted({str(e.get("kind")) for e in evidence if e.get("kind")})
        findings.append({
            "id": _finding_id(),
            "statement": (
                f"{len(ev_ids)} evidence record(s) are attached and were collected for this "
                f"{target} ({', '.join(kinds) or 'mixed kinds'}). They are listed here unread: the "
                "model was unavailable, so nothing below interprets them."
            ),
            "kind": "fact",
            "citedEvidenceIds": ev_ids,
            "thesisItemId": None,
        })
    else:
        findings.append({
            "id": _finding_id(),
            "statement": (
                "No evidence was attached to this review, and the model was unavailable, so there is "
                "nothing to report."
            ),
            "kind": "inference",
            "citedEvidenceIds": [],
            "thesisItemId": None,
        })

    return {
        "findings": findings,
        "agreements": [],
        "disagreements": [],
        "missingEvidence": [
            "A working model connection — this review was produced offline and contains no analysis."
        ],
        "openQuestions": [f"Re-run '{task}' when the model is reachable."],
        "lensConfidence": 0.0,
        "_stub": True,
    }


def _finding_id() -> str:
    return "fnd_" + uuid.uuid4().hex[:16]


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


# ---------------------------------------------------------------------------- run

def _normalize(obj: dict, ctx: dict) -> dict:
    """Shape the validated model object into the frozen response body.

    Also the last line of defence: any finding that still carries an unauthorized citation has that
    citation STRIPPED and is demoted to `inference`, so a bad id can never reach the store even if a
    future validation path misses it.
    """
    authorized = {e.get("id") for e in (ctx.get("evidence") or []) if e.get("id")}
    item_ids = _thesis_item_ids(ctx.get("thesis"))

    findings = []
    for f in obj.get("findings") or []:
        if not isinstance(f, dict):
            continue
        statement = str(f.get("statement") or "").strip()
        if not statement:
            continue
        cited = [c for c in (f.get("citedEvidenceIds") or []) if c in authorized]
        kind = f.get("kind") if f.get("kind") in FINDING_KINDS else "inference"
        if kind == "fact" and not cited:
            kind = "inference"  # demote rather than persist an uncited fact
        item = f.get("thesisItemId")
        findings.append({
            "id": f.get("id") or _finding_id(),
            "statement": statement,
            "kind": kind,
            "citedEvidenceIds": cited,
            "thesisItemId": item if item in item_ids else None,
        })

    def strlist(key):
        return [str(x).strip() for x in (obj.get(key) or []) if str(x).strip()]

    conf = obj.get("lensConfidence")
    lens_confidence = None
    if isinstance(conf, (int, float)):
        lens_confidence = int(round(max(0.0, min(1.0, float(conf))) * 100))

    # Server-computed union, deduped, order-stable — never taken from the model.
    seen, cited_union = set(), []
    for f in findings:
        for c in f["citedEvidenceIds"]:
            if c not in seen:
                seen.add(c)
                cited_union.append(c)

    return {
        "findings": findings,
        "agreements": strlist("agreements"),
        "disagreements": strlist("disagreements"),
        "missingEvidence": strlist("missingEvidence"),
        "openQuestions": strlist("openQuestions"),
        "citedEvidenceIds": cited_union,
        "lensConfidence": lens_confidence,
    }


def run_lens(ctx: dict, call_model, safe_json) -> dict:
    """Call → validate → one firmer retry → deterministic stub. Returns the frozen response body.

    `ctx` must already be SERVER-RESOLVED: the gateway loaded the thesis and every evidence record
    from `journal` with the caller's cookie before calling this. This module never fetches anything.
    """
    raw, model_used = call_model(build_lens_prompt(ctx))
    obj = safe_json(raw)
    warnings = validate_lens(obj, ctx)
    retried = False
    stub = False

    if not model_used.startswith("stub:") and needs_lens_retry(warnings):
        retried = True
        raw, model_used = call_model(build_lens_prompt(ctx, firmer=True))
        obj = safe_json(raw)
        warnings = validate_lens(obj, ctx)

    if obj is None or _fatal(warnings):
        obj = stub_lens(ctx)
        stub = True
        if not model_used.startswith("stub:"):
            model_used = f"{model_used} (fell back to stub)"
        warnings = validate_lens(obj, ctx)

    body = _normalize(obj, ctx)

    lens = ctx.get("lens") or {}
    target = ctx.get("target") or {}
    body.update({
        "schemaVersion": 1,
        "ticker": ctx.get("ticker"),
        "target": {"type": target.get("type"), "id": target.get("id")},
        "thesisId": target.get("id") if target.get("type") == "thesis" else ctx.get("thesisId"),
        "task": ctx.get("task"),
        "lens": {
            "id": lens.get("id"),
            "name": lens.get("name"),
            "builtin": bool(lens.get("builtin")),
            "version": LENS_VERSION,
        },
        # Always the SERVER's deterministic value — re-imposed, never the model's (§3.5.5).
        "chartContext": ctx.get("chartContext"),
        "modelUsed": model_used,
        "retried": retried,
        "stub": stub,
        "warnings": warnings,
        "committeeRef": None,
        "priorReviewId": (ctx.get("priorReview") or {}).get("id") if ctx.get("priorReview") else None,
        "createdAt": _now_iso(),
        "disclaimer": LENS_DISCLAIMER,
    })
    return body


# ---------------------------------------------------------------------------- committee → review

def committee_to_review(result: dict, ctx: dict, snapshot_id: str | None = None) -> dict:
    """Map a Committee run into the common review shape (§3.6). "Review with perspectives".

    The PIPELINE IS UNTOUCHED: `run_committee` still runs 3 analysts → bull/bear debate → judge,
    sequentially, and `compute_consensus` still computes the consensus deterministically in code.
    This function only projects that result into the one review shape so a committee run saved from a
    thesis is readable alongside every other lens review.

    Two deliberate refusals:

      · `lensConfidence` stays None. The consensus is a STANCE (a lean), not a confidence in a
        review, and the contract forbids collapsing one into the other. The real consensus stays
        readable through `committeeRef`, which points at the untouched snapshot.
      · Analyst stances become `kind:"inference"`. They are reasoning over facts the committee was
        given, not citations of the user's evidence records — calling them "fact" would fake a
        traceability that does not exist. A stance only becomes a `fact` finding if it happens to
        quote an authorized evidence id, which the shared normalizer below checks like any other.
    """
    analysts = result.get("analysts") or {}
    judge = result.get("judge") or {}
    debate = result.get("debate") or {}

    findings = []
    for agent in ("technical", "fundamentals", "news"):
        a = analysts.get(agent)
        if not isinstance(a, dict):
            continue
        stance = a.get("stance")
        for reason in (a.get("reasoning") or [])[:4]:
            text = str(reason).strip()
            if not text:
                continue
            findings.append({
                "statement": f"[{agent}, leaning {stance}] {text}",
                "kind": "inference",
                "citedEvidenceIds": [],
                "thesisItemId": None,
            })
        watch = str(a.get("watchFor") or "").strip()
        if watch:
            findings.append({
                "statement": f"[{agent}] Would most change this read: {watch}",
                "kind": "inference",
                "citedEvidenceIds": [],
                "thesisItemId": None,
            })

    obj = {
        "findings": findings,
        # The judge's own digest of where the argument is strong on each side.
        "agreements": [str(x).strip() for x in (judge.get("strongestBull") or []) if str(x).strip()],
        "disagreements": [str(x).strip() for x in (judge.get("strongestBear") or []) if str(x).strip()],
        "missingEvidence": [
            str(x).strip() for x in ((debate.get("bear") or {}).get("rebuttals") or []) if str(x).strip()
        ],
        "openQuestions": [str(x).strip() for x in (judge.get("unresolved") or []) if str(x).strip()],
        "lensConfidence": None,  # see docstring — consensus is a stance, never a confidence
    }

    body = _normalize(obj, ctx)
    body["lensConfidence"] = None

    target = ctx.get("target") or {}
    models = result.get("modelsUsed") or {}
    used = sorted({str(v) for v in models.values()}) or ["stub:offline"]
    stub = all(str(v).startswith("stub:") for v in models.values()) if models else True

    body.update({
        "schemaVersion": 1,
        "ticker": ctx.get("ticker"),
        "target": {"type": target.get("type"), "id": target.get("id")},
        "thesisId": target.get("id") if target.get("type") == "thesis" else None,
        "task": "review_with_perspectives",
        "lens": {
            "id": "committee",
            "name": "Review with perspectives",
            "builtin": True,
            "version": LENS_VERSION,
        },
        "chartContext": ctx.get("chartContext"),
        "modelUsed": ", ".join(used),
        "retried": False,
        "stub": stub,
        "warnings": list(result.get("warnings") or []),
        # The pointer back to the untouched committee snapshot — where the deterministic consensus,
        # the full debate, and the featureVector the prediction service reads still live.
        "committeeRef": {"store": "llm-committee", "id": snapshot_id} if snapshot_id else None,
        "priorReviewId": None,
        "createdAt": _now_iso(),
        "disclaimer": LENS_DISCLAIMER,
        "consensus": result.get("consensus"),
    })
    return body
