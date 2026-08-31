"""Earnings-call transcript analyzer — reading "between the lines" of management language.

The model does what a 14B LLM is actually good at: reading a long document and structuring it.
It does NOT predict anything. Outputs:

  * keyPoints        — the 5 most decision-relevant points of the call
  * evasiveMoments   — Q&A exchanges where management dodged; every moment must carry VERBATIM
                       quotes, and any quote that cannot be found in the source text is DROPPED
                       (grounding is enforced in code, not trusted to the model)
  * tone             — descriptive management-tone read (confidence / hedging / notable language)
  * toneShift        — comparison against the STORED prior-quarter analysis (we feed the previous
                       snapshot back in; the model never relies on its own memory of past calls)
  * riskFlags, summary

Long transcripts are handled map-reduce style: chunk -> per-chunk extraction -> one synthesis
call, all sequential (one model call in flight at a time). Deterministic stub offline.
Production persistence: PostgreSQL `llm.snapshots`; the READS_DIR layout is the local fallback and
legacy import source.
"""
from __future__ import annotations

import json
import os
import re
from datetime import datetime, timezone

from .prompt import BANNED
from .store import READS_DIR
from . import db

MAX_TEXT_CHARS = 160_000          # hard cap on submitted transcripts
CHUNK_CHARS = 12_000              # per-chunk size for map-reduce
MAX_CHUNKS = 12

TRANSCRIPT_DISCLAIMER = (
    "AI reading of a transcript — descriptive interpretation of management language, not "
    "investment advice and not a verdict on the stock."
)

_SYSTEM = (
    "You are an earnings-call analyst, NOT a financial advisor. You read transcripts and describe "
    "what management said and how they said it. You NEVER give buy/sell recommendations, price "
    "targets, or verdicts on the stock. Every quote you output must be VERBATIM from the supplied "
    "text. You reply with a single JSON object and nothing else."
)

CHUNK_SCHEMA = {
    "keyPoints": ["string (up to 4, only genuinely important points from THIS chunk)"],
    "evasiveCandidates": [{
        "question": "verbatim quote of the analyst's question (trim to <=300 chars)",
        "answer": "verbatim quote of the evasive part of the reply (trim to <=300 chars)",
        "why": "string — what was dodged and how",
    }],
    "toneWords": ["string (verbatim hedging/confidence phrases used by management)"],
}

FINAL_SCHEMA = {
    "keyPoints": ["string (exactly the 5 most critical points of the whole call)"],
    "evasiveMoments": [{
        "question": "verbatim quote", "answer": "verbatim quote",
        "why": "what was avoided", "topic": "string (e.g. margins, competition, guidance)",
    }],
    "tone": {
        "confidence": "one of: high | medium | low",
        "hedging": "one of: low | medium | high",
        "notableLanguage": ["string (verbatim phrases that reveal tone)"],
    },
    "toneShift": "string or null — ONLY if a PRIOR-QUARTER analysis is supplied, compare tone vs it",
    "riskFlags": ["string (0-5 concerns raised by the language itself)"],
    "summary": "string (3-4 sentences, descriptive)",
}

FINAL_KEYS = ["keyPoints", "evasiveMoments", "tone", "riskFlags", "summary"]

HEDGE_WORDS = (
    "we'll see", "too early", "not going to comment", "can't comment", "we don't guide",
    "wait and see", "hard to say", "uncertain", "headwinds", "challenging", "as i said",
    "i think", "we believe", "hopefully", "cautious", "difficult to predict",
)
CONFIDENCE_WORDS = (
    "record", "strong", "confident", "exceptional", "ahead of plan", "raising", "accelerating",
    "robust", "outperform", "exceeded",
)


# ------------------------------------------------------------------------ chunking

def chunk_text(text: str) -> list[str]:
    text = text[:MAX_TEXT_CHARS]
    if len(text) <= CHUNK_CHARS:
        return [text]
    chunks, i = [], 0
    while i < len(text) and len(chunks) < MAX_CHUNKS:
        end = min(i + CHUNK_CHARS, len(text))
        # break on a paragraph/sentence boundary where possible
        if end < len(text):
            cut = text.rfind("\n", i + CHUNK_CHARS // 2, end)
            if cut == -1:
                cut = text.rfind(". ", i + CHUNK_CHARS // 2, end)
            if cut != -1:
                end = cut + 1
        chunks.append(text[i:end])
        i = end
    return chunks


# ------------------------------------------------------------------------ prompts

def build_chunk_prompt(ticker: str, part: int, total: int, chunk: str, firmer: bool = False) -> list[dict]:
    firm = ""
    if firmer:
        firm = ("\n\nIMPORTANT: Return ONLY a JSON object with EXACTLY these keys: keyPoints, "
                "evasiveCandidates, toneWords. All quotes verbatim from the text.")
    user = (
        f"TICKER: {ticker} — earnings-call transcript, part {part}/{total}.\n\n"
        f"TRANSCRIPT PART:\n\"\"\"\n{chunk}\n\"\"\"\n\n"
        f"Extract per this schema (JSON only):\n{json.dumps(CHUNK_SCHEMA, indent=2)}\n\n"
        "Only report an evasive candidate when an analyst asked something specific and management "
        "visibly deflected, deferred, or answered a different question. Quotes VERBATIM." + firm
    )
    return [{"role": "system", "content": _SYSTEM}, {"role": "user", "content": user}]


def build_final_prompt(
    ticker: str, label: str, chunk_results: list[dict], prior: dict | None, firmer: bool = False
) -> list[dict]:
    firm = ""
    if firmer:
        firm = ("\n\nIMPORTANT: Return ONLY a JSON object with EXACTLY these keys: keyPoints, "
                "evasiveMoments, tone, toneShift, riskFlags, summary.")
    prior_block = ""
    if prior:
        prior_block = (
            "\n\nPRIOR-QUARTER ANALYSIS (stored from the previous call — use it ONLY for toneShift):\n"
            + json.dumps({
                "label": prior.get("label"),
                "tone": (prior.get("structured") or {}).get("tone"),
                "keyPoints": (prior.get("structured") or {}).get("keyPoints"),
            }, indent=2)
        )
    user = (
        f"TICKER: {ticker} — synthesize the call \"{label}\" from per-part extractions.\n\n"
        f"PER-PART EXTRACTIONS:\n{json.dumps(chunk_results, indent=2)}"
        f"{prior_block}\n\n"
        f"Respond with a single JSON object matching this schema exactly:\n"
        f"{json.dumps(FINAL_SCHEMA, indent=2)}\n\n"
        "Keep quotes verbatim (copy them unchanged from the extractions). toneShift must be null "
        "when no prior analysis is supplied. Describe; never advise." + firm
    )
    return [{"role": "system", "content": _SYSTEM}, {"role": "user", "content": user}]


# ------------------------------------------------------------------------ validation + grounding

def _norm(s: str) -> str:
    return re.sub(r"\s+", " ", (s or "").strip().lower())


def quote_in_text(quote: str, text: str) -> bool:
    """Grounding check: the (whitespace-normalized) quote must appear in the source text. Short
    fragments (<20 chars) are rejected outright — too easy to fabricate."""
    q = _norm(quote)
    return len(q) >= 20 and q in _norm(text)


def enforce_grounding(obj: dict, source_text: str) -> list[str]:
    """Drop any evasiveMoment whose quotes can't be found verbatim in the transcript. Returns
    warnings for what was dropped. The model doesn't get to invent quotes."""
    warnings: list[str] = []
    moments = obj.get("evasiveMoments")
    if not isinstance(moments, list):
        return warnings
    kept = []
    for m in moments:
        if not isinstance(m, dict):
            continue
        if quote_in_text(str(m.get("question", "")), source_text) and \
           quote_in_text(str(m.get("answer", "")), source_text):
            kept.append(m)
        else:
            warnings.append("dropped evasive moment with unverifiable quote")
    obj["evasiveMoments"] = kept
    return warnings


def validate_final(obj: dict | None) -> list[str]:
    if not isinstance(obj, dict):
        return ["output is not a JSON object"]
    warnings = [f"missing key: {k}" for k in FINAL_KEYS if k not in obj]
    tone = obj.get("tone")
    if not isinstance(tone, dict) or tone.get("confidence") not in ("high", "medium", "low"):
        warnings.append("invalid tone.confidence")
    if isinstance(obj.get("keyPoints"), list) and not obj["keyPoints"]:
        warnings.append("empty keyPoints")
    blob = json.dumps(obj).lower()
    warnings.extend(f"contains advice phrase: '{b}'" for b in BANNED if b in blob)
    return warnings


def needs_final_retry(warnings: list[str]) -> bool:
    return any(w.startswith(("missing key", "output is not")) or w == "invalid tone.confidence"
               for w in warnings)


# ------------------------------------------------------------------------ deterministic stub

def stub_analysis(ticker: str, text: str, prior: dict | None) -> dict:
    """Offline fallback: honest keyword counting, clearly labelled. No fabricated quotes —
    evasiveMoments stays empty because the stub cannot judge evasion."""
    low = text.lower()
    hedge = sum(low.count(w) for w in HEDGE_WORDS)
    conf = sum(low.count(w) for w in CONFIDENCE_WORDS)
    per_kchar = max(len(text), 1) / 1000.0
    hedging = "high" if hedge / per_kchar > 0.8 else "low" if hedge / per_kchar < 0.2 else "medium"
    confidence = "high" if conf > hedge else "low" if hedge > conf * 2 else "medium"
    tone_shift = None
    if prior:
        p = ((prior.get("structured") or {}).get("tone") or {})
        if p.get("confidence") and p["confidence"] != confidence:
            tone_shift = f"Keyword-based confidence moved {p['confidence']} -> {confidence} vs {prior.get('label')}."
        else:
            tone_shift = f"Keyword-based tone roughly unchanged vs {prior.get('label')}."
    return {
        "keyPoints": [
            f"Offline stub: {len(text):,} characters analyzed by keyword counting only.",
            f"{conf} confidence-signal phrases vs {hedge} hedging phrases detected.",
        ],
        "evasiveMoments": [],
        "tone": {"confidence": confidence, "hedging": hedging,
                 "notableLanguage": [w for w in HEDGE_WORDS if w in low][:5]},
        "toneShift": tone_shift,
        "riskFlags": ["Model offline — this is a deterministic keyword read, not a language analysis."],
        "summary": f"Deterministic offline read of the {ticker} transcript: "
                   f"hedging {hedging}, keyword confidence {confidence}. Descriptive only.",
        "_stub": True,
    }


# ------------------------------------------------------------------------ persistence

def _tdir(ticker: str) -> str:
    d = os.path.join(READS_DIR, ticker.upper(), "transcripts")
    os.makedirs(d, exist_ok=True)
    return d


def _safe_label(label: str) -> str:
    return re.sub(r"[^A-Za-z0-9_-]", "-", label.strip())[:40] or "unlabeled"


def save_transcript_analysis(ticker: str, label: str, structured: dict, model_used: str) -> str:
    key = _safe_label(label)
    record = {
        "label": label, "key": key,
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "ticker": ticker.upper(), "modelUsed": model_used, "structured": structured,
    }
    if db.enabled():
        db.save("transcript", ticker, key, record)
    else:
        with open(os.path.join(_tdir(ticker), f"{key}.json"), "w") as f:
            json.dump(record, f, indent=2)
    return key


def list_transcript_analyses(ticker: str, limit: int = 12) -> list[dict]:
    if db.enabled():
        rows = db.list_records("transcript", ticker, limit)
        rows.sort(key=lambda r: r.get("timestamp", ""), reverse=True)
        return rows[:limit]
    d = _tdir(ticker)
    out = []
    for name in os.listdir(d):
        if not name.endswith(".json"):
            continue
        try:
            with open(os.path.join(d, name)) as f:
                out.append(json.load(f))
        except (OSError, json.JSONDecodeError):
            continue
    out.sort(key=lambda r: r.get("timestamp", ""), reverse=True)
    return out[:limit]


def latest_prior(ticker: str, exclude_label: str) -> dict | None:
    for rec in list_transcript_analyses(ticker):
        if rec.get("label") != exclude_label:
            return rec
    return None


# ------------------------------------------------------------------------ orchestration

def analyze_transcript(ticker: str, label: str, text: str, call_model, safe_json) -> dict:
    """Full pipeline: chunk -> extract per chunk -> synthesize -> ground-check quotes -> stub
    fallback. Sequential model calls only."""
    text = (text or "")[:MAX_TEXT_CHARS]
    prior = latest_prior(ticker, label)
    chunks = chunk_text(text)

    chunk_results: list[dict] = []
    models: list[str] = []
    all_stub = True
    for i, chunk in enumerate(chunks, 1):
        raw, used = call_model(build_chunk_prompt(ticker, i, len(chunks), chunk))
        obj = safe_json(raw)
        if obj is None and not used.startswith("stub:"):
            raw, used = call_model(build_chunk_prompt(ticker, i, len(chunks), chunk, firmer=True))
            obj = safe_json(raw)
        models.append(used)
        if obj is not None and not used.startswith("stub:"):
            all_stub = False
            chunk_results.append({"part": i, **{k: obj.get(k) for k in ("keyPoints", "evasiveCandidates", "toneWords")}})

    warnings: list[str] = []
    if all_stub or not chunk_results:
        structured = stub_analysis(ticker, text, prior)
        model_used = "stub:offline"
        warnings = validate_final(structured)
    else:
        raw, model_used = call_model(build_final_prompt(ticker, label, chunk_results, prior))
        structured = safe_json(raw)
        warnings = validate_final(structured)
        if needs_final_retry(warnings) and not model_used.startswith("stub:"):
            raw, model_used = call_model(build_final_prompt(ticker, label, chunk_results, prior, firmer=True))
            structured = safe_json(raw)
            warnings = validate_final(structured)
        if structured is None or needs_final_retry(warnings):
            structured = stub_analysis(ticker, text, prior)
            model_used = f"{model_used} (fell back to stub)"
            warnings = validate_final(structured)
        else:
            warnings.extend(enforce_grounding(structured, text))
            if prior is None:
                structured["toneShift"] = None  # never compare against the model's imagination

    return {
        "ticker": ticker, "label": label,
        "structured": structured,
        "modelUsed": model_used, "chunkModels": models, "chunks": len(chunks),
        "priorLabel": prior.get("label") if prior else None,
        "warnings": warnings,
        "disclaimer": TRANSCRIPT_DISCLAIMER,
    }
