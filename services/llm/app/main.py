"""LLM service (Python · FastAPI). Phase 3: structured JSON read with validation + one retry,
persisted as a daily snapshot so reads can be diffed over time."""
from __future__ import annotations

from fastapi import FastAPI, Request
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import JSONResponse
from pydantic import BaseModel

from .analyst_router import analyst_router
from .auth import current_user_id
from . import db

from .brief import (
    build_brief_prompt,
    needs_brief_retry,
    stub_brief,
    validate_brief,
)
from .assistant import (
    ASSISTANT_DISCLAIMER,
    build_assistant_prompt,
    build_chat_messages,
    needs_assistant_retry,
    stub_assistant,
    stub_chat_reply,
    validate_assistant,
)
from .earnings_score import (
    build_score_prompt,
    needs_score_retry,
    stub_score,
    validate_score,
)
from .llm import call_model, call_model_text, runtime_status, safe_json, stub_structured
from .prompt import (
    build_prompt,
    needs_retry,
    structured_to_markdown,
    validate_structured,
)
from .committee import COMMITTEE_DISCLAIMER, run_committee
from .personas import (
    create_persona,
    delete_persona,
    get_persona,
    list_personas,
    update_persona,
)
from .digest import run_digest
from .investigation import run_investigation
from .lens import (
    BUILTIN_LENS_IDS,
    LENS_VERSION,
    TARGET_TYPES,
    TASKS,
    committee_to_review,
    run_lens,
)
from .scenario import run_scenario
from .portfolio import run_portfolio_review, run_portfolio_scenario
from .thesis import run_thesis_check
from .transcript import (
    analyze_transcript,
    list_transcript_analyses,
    save_transcript_analysis,
)
from .store import diff_latest, list_committee, list_reads, save_committee, save_read

app = FastAPI(title="NVDA Platform — LLM Service", version="0.3.0")
app.add_middleware(
    CORSMiddleware, allow_origins=["*"], allow_methods=["*"], allow_headers=["*"]
)
# Wave 0 seam. Empty today; Wave 3 Lane B decorates analyst_router from its own analyst.py so it
# never edits this file. See analyst_router.py.
app.include_router(analyst_router)


class ReadRequest(BaseModel):
    ticker: str
    regime: dict
    recentBars: list[dict] = []
    persist: bool = True
    timeframe: str = "1D"  # only 1D reads are persisted to the daily-snapshot history
    confluence: dict | None = None  # multi-timeframe picture; when present the read comments on it


class BriefRequest(BaseModel):
    ticker: str  # a symbol, or "MARKET" for the market-wide brief
    context: dict = {}  # the day's ALREADY-COLLECTED facts, assembled by the gateway


class ScoreEarningsRequest(BaseModel):
    ticker: str
    asOf: str = ""   # the date the text was public (for the model's context; no future info)
    text: str = ""   # event-time earnings text to score (release / headline / filing excerpt)


class CommitteeRequest(BaseModel):
    ticker: str
    context: dict = {}  # gateway-assembled facts: regime/confluence/earnings/news/sentiment
    persist: bool = True
    # When a committee run is invoked FROM a thesis, the gateway sends the resolved thesis target and
    # its authorized lens context; the response then also carries a review body (§3.6). Absent for a
    # plain /committee call, which behaves exactly as before.
    target: dict | None = None
    lensContext: dict = {}


class LensRequest(BaseModel):
    """One request shape for every lens (§3.1).

    `context` is SERVER-BUILT by the gateway — resolved thesis, authorized evidence records,
    deterministic chart context, the resolved lens/persona, and any prior review. This service never
    resolves an id itself, so a client cannot smuggle in evidence it does not own.
    """
    lensId: str = ""
    task: str
    target: dict = {}
    ticker: str = ""
    timeframe: str = "1D"
    userInstruction: str = ""
    context: dict = {}


class TranscriptRequest(BaseModel):
    ticker: str
    label: str = ""      # e.g. "2026-Q2"; used as the snapshot key for QoQ tone comparison
    text: str            # the transcript itself (gateway may have fetched it from a URL)
    persist: bool = True


class ThesisCheckRequest(BaseModel):
    ticker: str
    thesis: str          # the user's own thesis text
    context: dict = {}   # gateway-assembled facts (news/earnings/regime/filings)


class DigestRequest(BaseModel):
    ticker: str
    items: list[dict] = []  # the collected headline feed (gateway supplies)


class ScenarioRequest(BaseModel):
    ticker: str
    question: str        # the what-if
    context: dict = {}   # suppliers/roadmap/risks/regime/news


class PortfolioReviewRequest(BaseModel):
    context: dict


class PortfolioScenarioRequest(BaseModel):
    question: str
    context: dict


class InvestigationRequest(BaseModel):
    ticker: str
    question: str
    addons: list[str] = []
    context: dict = {}   # source bundle assembled and authorized by the gateway


class AssistantRequest(BaseModel):
    ticker: str
    timeframe: str = "1D"
    context: dict = {}  # live facts assembled by the gateway (regime/confluence/signal/expectedClose/…)


class ChatRequest(BaseModel):
    ticker: str
    timeframe: str = "1D"
    context: dict = {}
    messages: list[dict] = []  # running [{role, content}] history from the frontend
    personaId: str | None = None  # optional "instructor" persona (style only; guardrails stay)


class PersonaCreate(BaseModel):
    name: str
    instructions: str
    emoji: str = ""


class PersonaUpdate(BaseModel):
    name: str | None = None
    instructions: str | None = None
    emoji: str | None = None


@app.get("/health")
def health():
    """Includes the model runtime status so 'assistant offline' is diagnosable: which backend/model/
    URL is configured, whether a key is set, and the LAST model-call error (None = healthy)."""
    storage = "files"
    if db.enabled():
        storage = "postgresql"
        try:
            db.check()
        except Exception as exc:
            return JSONResponse(
                status_code=503,
                content={
                    "status": "unavailable", "service": "llm", "storage": storage,
                    "database": {"ok": False, "error": type(exc).__name__},
                    "llm": runtime_status(),
                },
            )
    return {"status": "ok", "service": "llm", "storage": storage, "llm": runtime_status()}


@app.post("/brief")
def brief(req: BriefRequest):
    """AI Market Brief: read + compress the day's collected facts into a structured, plain-language
    digest. Grounded only in `context`; descriptive, never prescriptive. Same call_model + one-retry +
    deterministic-stub fallback as /read, so it always returns a usable brief."""
    ticker = req.ticker.upper()
    ctx = req.context or {}

    # 1. structured attempt
    raw, model_used = call_model(build_brief_prompt(ticker, ctx))
    obj = safe_json(raw)
    warnings = validate_brief(obj)
    retried = False

    # 2. one firmer retry on structural failure
    if not model_used.startswith("stub:") and needs_brief_retry(warnings):
        retried = True
        raw, model_used = call_model(build_brief_prompt(ticker, ctx, firmer=True))
        obj = safe_json(raw)
        warnings = validate_brief(obj)

    # 3. deterministic stub if still unusable (model down, or non-conforming output)
    if obj is None or needs_brief_retry(warnings):
        obj = stub_brief(ticker, ctx)
        if not model_used.startswith("stub:"):
            model_used = f"{model_used} (fell back to stub)"
        warnings = validate_brief(obj)

    return {
        "ticker": ticker,
        "structured": obj,
        "modelUsed": model_used,
        "warnings": warnings,
        "retried": retried,
        "disclaimer": "AI-generated summary — informational, not investment advice",
    }


@app.post("/score-earnings")
def score_earnings(req: ScoreEarningsRequest):
    """AI earnings scorer: read event-time TEXT into { tone, guidanceDirection, confidence,
    riskFlags }. Scores only the supplied text (no price, no future). call_model + one retry +
    deterministic keyword stub. Used by the event-study harness's Test 2 (AI marginal value)."""
    ticker = req.ticker.upper()
    text = req.text or ""
    if not text.strip():
        # nothing to score — return a neutral, honest zero-confidence read (no model call)
        return {
            "ticker": ticker, "asOf": req.asOf,
            "structured": {"tone": 0.0, "guidanceDirection": "flat", "confidence": 0.0,
                           "riskFlags": [], "_stub": True},
            "modelUsed": "stub:no-text", "warnings": [], "retried": False,
            "disclaimer": "AI-generated summary — informational, not investment advice",
        }

    raw, model_used = call_model(build_score_prompt(ticker, req.asOf, text))
    obj = safe_json(raw)
    warnings = validate_score(obj)
    retried = False

    if not model_used.startswith("stub:") and needs_score_retry(warnings):
        retried = True
        raw, model_used = call_model(build_score_prompt(ticker, req.asOf, text, firmer=True))
        obj = safe_json(raw)
        warnings = validate_score(obj)

    if obj is None or needs_score_retry(warnings):
        obj = stub_score(ticker, text)
        if not model_used.startswith("stub:"):
            model_used = f"{model_used} (fell back to stub)"
        warnings = validate_score(obj)

    return {
        "ticker": ticker, "asOf": req.asOf,
        "structured": obj,
        "modelUsed": model_used,
        "warnings": warnings,
        "retried": retried,
        "disclaimer": "AI-generated summary — informational, not investment advice",
    }


@app.post("/assistant")
def assistant(req: AssistantRequest):
    """Situational note: a short grounded read of the setup RIGHT NOW ({ situation, whatToWatch,
    wouldChangeIt, rangeComment }). call_model + one retry + deterministic stub. Grounded in the
    supplied live context (incl. the COMPUTED expected-close range); never fabricates a number."""
    ticker, tf, ctx = req.ticker.upper(), req.timeframe, (req.context or {})

    raw, model_used = call_model(build_assistant_prompt(ticker, tf, ctx))
    obj = safe_json(raw)
    warnings = validate_assistant(obj)
    retried = False

    if not model_used.startswith("stub:") and needs_assistant_retry(warnings):
        retried = True
        raw, model_used = call_model(build_assistant_prompt(ticker, tf, ctx, firmer=True))
        obj = safe_json(raw)
        warnings = validate_assistant(obj)

    if obj is None or needs_assistant_retry(warnings):
        obj = stub_assistant(ticker, tf, ctx)
        if not model_used.startswith("stub:"):
            model_used = f"{model_used} (fell back to stub)"
        warnings = validate_assistant(obj)

    return {
        "ticker": ticker, "timeframe": tf,
        "structured": obj,
        "modelUsed": model_used,
        "warnings": warnings,
        "retried": retried,
        "disclaimer": ASSISTANT_DISCLAIMER,
    }


@app.post("/chat")
def chat(req: ChatRequest, request: Request):
    """Grounded chat: a FREE-TEXT reply to the user's follow-up, grounded in the ticker's live
    context. Uncertainty language, no fabricated numbers, decision + risk are the user's. Falls back
    to a deterministic context summary when the model is offline (never errors out)."""
    ticker, tf, ctx = req.ticker.upper(), req.timeframe, (req.context or {})
    # Resolve the persona in the CALLER'S scope (gateway-verified X-User-Id): a signed-in user's
    # custom persona resolves; a guest resolves built-ins only. Chat replies stay open to guests.
    persona = get_persona(req.personaId, current_user_id(request))  # None -> plain grounded chat
    reply, model_used = call_model_text(build_chat_messages(ticker, tf, ctx, req.messages, persona))
    if not reply:
        reply = stub_chat_reply(ticker, {**ctx, "timeframe": tf})
        if not model_used.startswith("stub:"):  # keep "stub:quota" distinct from plain offline
            model_used = "stub:offline"
    return {
        "ticker": ticker, "timeframe": tf,
        "reply": reply,
        "modelUsed": model_used,
        "personaId": persona["id"] if persona else None,
        "personaName": persona["name"] if persona else None,
        "disclaimer": ASSISTANT_DISCLAIMER,
    }


# --------------------------------------------------------------------------- personas ("instructors")
# User-editable custom-instruction profiles (like Gemini Gems) that shape the chat's role/tone/teaching
# style. They LAYER ON TOP of the chat's locked grounding rules — never a way to unlock a prediction or
# a buy/sell call (invariant #2). Built-ins are read-only; user personas persist to disk.

# Per-user scoping: X-User-Id is the gateway-verified identity ("" = guest). GET lists built-ins +
# the caller's personas (guests: built-ins only); create/update/delete require a signed-in user (401).

@app.get("/personas")
def personas(request: Request):
    return {"personas": list_personas(current_user_id(request))}


@app.post("/personas")
def personas_create(req: PersonaCreate, request: Request):
    uid = current_user_id(request)
    if not uid:
        return JSONResponse(status_code=401, content={"error": "sign in to create a persona"})
    try:
        return {"persona": create_persona(uid, req.name, req.instructions, req.emoji)}
    except ValueError as e:
        return JSONResponse(status_code=400, content={"error": str(e)})


@app.patch("/personas/{persona_id}")
def personas_update(persona_id: str, req: PersonaUpdate, request: Request):
    uid = current_user_id(request)
    if not uid:
        return JSONResponse(status_code=401, content={"error": "sign in to edit personas"})
    patch = {k: v for k, v in req.model_dump().items() if v is not None}
    try:
        return {"persona": update_persona(uid, persona_id, patch)}
    except KeyError:
        return JSONResponse(status_code=404, content={"error": "persona not found"})
    except ValueError as e:
        return JSONResponse(status_code=400, content={"error": str(e)})


@app.delete("/personas/{persona_id}")
def personas_delete(persona_id: str, request: Request):
    uid = current_user_id(request)
    if not uid:
        return JSONResponse(status_code=401, content={"error": "sign in to delete personas"})
    try:
        if delete_persona(uid, persona_id):
            return {"deleted": persona_id}
        return JSONResponse(status_code=404, content={"error": "persona not found"})
    except ValueError as e:
        return JSONResponse(status_code=400, content={"error": str(e)})


@app.post("/transcript")
def transcript(req: TranscriptRequest):
    """Earnings-call transcript analysis: key points, quote-verified evasive moments, management
    tone, and a tone shift computed against the STORED prior-quarter snapshot. Long transcripts
    are processed in sequential chunks. Descriptive language analysis — never advice."""
    ticker = req.ticker.upper()
    label = (req.label or "").strip() or "unlabeled"
    if not (req.text or "").strip():
        return {"ticker": ticker, "label": label, "error": "empty transcript text"}
    result = analyze_transcript(ticker, label, req.text, call_model, safe_json)
    persisted = None
    if req.persist:
        try:
            persisted = save_transcript_analysis(ticker, label, result["structured"], result["modelUsed"])
        except OSError:
            persisted = None
    result["persisted"] = persisted
    return result


@app.get("/transcript/{ticker}")
def transcript_history(ticker: str, limit: int = 12):
    return {"ticker": ticker.upper(), "analyses": list_transcript_analyses(ticker, limit)}


@app.post("/thesis-check")
def thesis_check(req: ThesisCheckRequest):
    """Evaluate the user's OWN thesis against newly collected facts: confirming / neutral /
    challenged, with evidence citing the supplied context. A verdict on the thesis, not the stock."""
    ticker = req.ticker.upper()
    if not (req.thesis or "").strip():
        return {"ticker": ticker, "error": "empty thesis"}
    return run_thesis_check(ticker, req.thesis, req.context or {}, call_model, safe_json)


@app.post("/lens")
def lens(req: LensRequest):
    """Apply ONE named research lens to ONE research object (PARALLEL_CONTRACTS.md §3).

    The gateway has ALREADY resolved every id against `journal` with the caller's cookie — the
    `context` below is the authorized, normalized set, and this service fetches nothing and trusts no
    client-supplied evidence text. Citations are validated against `context.evidence` only; an
    unknown id is rejected, retried once, then stubbed, and can never be persisted.

    On-demand only. Nothing in this service calls it (invariant #4)."""
    ctx = dict(req.context or {})
    ctx.update({
        "task": req.task,
        "ticker": (req.ticker or "").upper(),
        "target": req.target or {},
        "userInstruction": req.userInstruction or "",
    })

    if req.task not in TASKS:
        return JSONResponse(status_code=400, content={"error": f"unknown task: {req.task}"})
    target_type = (req.target or {}).get("type")
    if target_type not in TARGET_TYPES:
        return JSONResponse(status_code=400, content={"error": f"unknown target type: {target_type}"})

    return run_lens(ctx, call_model, safe_json)


@app.get("/lenses")
def lenses(request: Request):
    """The lens catalogue: the four built-ins plus the CALLER'S own personas, and the task table.

    This is what the Perspectives UI renders as named methods. A user persona appears as
    `persona:<id>` and is resolvable only by its owner (`get_persona` scopes by user)."""
    uid = current_user_id(request)
    out = []
    for p in list_personas(uid):
        pid = p["id"]
        out.append({
            "id": pid if pid in BUILTIN_LENS_IDS else f"persona:{pid}",
            "name": p.get("name"),
            "emoji": p.get("emoji", ""),
            "builtin": bool(p.get("builtin")),
            "version": LENS_VERSION,
        })
    return {
        "lenses": out,
        "tasks": [
            {"id": t, "defaultLens": spec["default_lens"], "targets": list(spec["targets"])}
            for t, spec in TASKS.items()
        ],
    }


@app.post("/digest")
def digest(req: DigestRequest):
    """Cluster the collected headline feed into stories, rate impact (low/medium/high), and write
    a 3-sentence day digest. Cluster indexes are validated in code. News triage, not advice."""
    return run_digest(req.ticker.upper(), req.items or [], call_model, safe_json)


@app.post("/scenario")
def scenario(req: ScenarioRequest):
    """Structured what-if reasoning grounded in the supplied supply-chain/roadmap/risk facts.
    Hypothetical, qualitative, uncertainty-labelled — never a prediction or a recommendation."""
    ticker = req.ticker.upper()
    if not (req.question or "").strip():
        return {"ticker": ticker, "error": "empty question"}
    return run_scenario(ticker, req.question, req.context or {}, call_model, safe_json)


@app.post("/portfolio-review")
def portfolio_review(req: PortfolioReviewRequest):
    """Explain a deterministic portfolio context after an explicit user review action."""
    return run_portfolio_review(req.context or {}, call_model, safe_json)


@app.post("/portfolio-scenario")
def portfolio_scenario(req: PortfolioScenarioRequest):
    """Qualitative hypothetical exposure analysis over recorded holdings; never a forecast."""
    question = (req.question or "").strip()
    if not question:
        return JSONResponse(status_code=400, content={"error": "empty question"})
    return run_portfolio_scenario(question, req.context or {}, call_model, safe_json)


@app.post("/investigation")
def investigation(req: InvestigationRequest):
    """One bounded research job over a gateway-built source bundle.

    Unlike /chat, this accepts no message history and returns a fixed report. It is invoked only by
    an explicit Run research action, validates every citation key, and degrades to an honest fact
    list when the model is offline.
    """
    ticker = req.ticker.upper()
    question = (req.question or "").strip()
    if not question:
        return JSONResponse(status_code=400, content={"error": "empty question"})
    return run_investigation(
        ticker, question, req.addons or [], req.context or {}, call_model, safe_json
    )


@app.post("/committee")
def committee(req: CommitteeRequest):
    """Analyst committee: 3 persona analysts -> bull/bear debate -> judge, run SEQUENTIALLY (one
    model call at a time). Stances are analytical leans, never buy/sell verdicts (invariant #2);
    consensus is a deterministic weighted vote computed in code, not by the model. Every agent
    degrades to a deterministic stub, so this runs with zero keys / zero network. On-demand only —
    the committee is NEVER called from a polling loop (invariant #4)."""
    ticker = req.ticker.upper()
    result = run_committee(ticker, req.context or {}, call_model, safe_json)

    persisted = None
    if req.persist:
        try:
            persisted = save_committee(ticker, result)
        except OSError:
            persisted = None
    result["persisted"] = persisted

    # "Review with perspectives": when the run came from a thesis, ALSO project it into the common
    # review shape so it is readable alongside every other lens review (§3.6). The snapshot above is
    # written first and is unchanged — `committeeRef` points at it, and `featureVector` stays exactly
    # where services/prediction/app/committee_features.py reads it (invariant #6).
    if req.target:
        ctx = dict(req.lensContext or {})
        ctx.update({"ticker": ticker, "target": req.target, "task": "review_with_perspectives"})
        snapshot_id = f"{ticker}/{persisted}" if persisted else None
        result["review"] = committee_to_review(result, ctx, snapshot_id)

    return result


@app.get("/committee/{ticker}")
def committee_history(ticker: str, limit: int = 10):
    """Persisted committee snapshots, newest first. Each carries the featureVector the prediction
    service may consume once enough history has accumulated."""
    return {
        "ticker": ticker.upper(),
        "snapshots": list_committee(ticker, limit),
        "disclaimer": COMMITTEE_DISCLAIMER,
    }


@app.post("/read")
def read(req: ReadRequest):
    ticker = req.ticker.upper()

    # 1. structured attempt
    raw, model_used = call_model(build_prompt(ticker, req.regime, req.recentBars, confluence=req.confluence))
    obj = safe_json(raw)
    warnings = validate_structured(obj)
    retried = False

    # 2. one firmer retry on structural failure (original GAP-3)
    if not model_used.startswith("stub:") and needs_retry(warnings):
        retried = True
        raw, model_used = call_model(
            build_prompt(ticker, req.regime, req.recentBars, firmer=True, confluence=req.confluence)
        )
        obj = safe_json(raw)
        warnings = validate_structured(obj)

    # 3. fall back to deterministic stub if still unusable
    if obj is None or needs_retry(warnings):
        obj = stub_structured(req.regime, req.confluence)
        if not model_used.startswith("stub:"):
            model_used = f"{model_used} (fell back to stub)"
        warnings = validate_structured(obj)

    # keep the computed levels authoritative regardless of what the model returned (GAP-2)
    computed = req.regime.get("keyLevels")
    if isinstance(computed, dict):
        obj["keyLevels"] = {
            "support": computed.get("support", []),
            "resistance": computed.get("resistance", []),
        }

    # Persist ONE snapshot per day for the daily read only — intraday reads (1H/15m/5m) are
    # on-demand and must not pollute the daily history / diff (docs/PHASE_SWING.md).
    persisted = None
    if req.persist and req.timeframe == "1D":
        try:
            persisted = save_read(ticker, obj, model_used, req.regime)
        except OSError:
            persisted = None

    return {
        "ticker": ticker,
        "timeframe": req.timeframe,
        "modelUsed": model_used,
        "structured": obj,
        "text": structured_to_markdown(obj),  # rendered fallback for display
        "warnings": warnings,
        "retried": retried,
        "persisted": persisted,
        "disclaimer": "Technical context only. Not investment advice. No buy/sell signal.",
    }


@app.get("/reads/{ticker}")
def reads(ticker: str, limit: int = 10):
    return {"ticker": ticker.upper(), "reads": list_reads(ticker, limit)}


@app.get("/reads/{ticker}/diff")
def reads_diff(ticker: str):
    return {"ticker": ticker.upper(), "diff": diff_latest(ticker)}
