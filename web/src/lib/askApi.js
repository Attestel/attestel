// askApi.js — the ASK ATTESTEL frontend adapter (Wave 4 Lane 4C, locked decision 8).
//
// A separate module for the same reason monitoringApi.js, thesisApi.js, evidenceApi.js, lensApi.js
// and eventsApi.js are: `lib/api.js` is on PARALLEL_CONTRACTS.md's reserved shared-file list and
// this lane may not edit it, so the contextual-chat call lives in a `*Api.js` beside it. It imports
// `AuthRequiredError` from `api.js` and modifies nothing there.
//
// ONE upstream: the GATEWAY, `POST /api/chat/{ticker}?timeframe=…` — the SAME route `api.js`'s
// `sendChat` already posts to. This module adds no route and asks the server for nothing new.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// WHAT THIS MODULE DELIBERATELY CANNOT DO — locked decisions 6, 7 and 9.
//
//   * IT COMPOSES NO HIDDEN CONTEXT. There is no preamble, no system turn, no invisible prefix
//     anywhere in this file. The context the model receives is built in the COMPONENT as a visible
//     transcript turn and arrives here inside `messages` like any other user turn. Ask Attestel
//     sends the model nothing the user cannot read, which is an honesty property first and a
//     debugging property second: when the answer is wrong, the input is on screen.
//
//     `buildContextTurn` below is exported from this module because it is pure text assembly, and
//     it returns a MESSAGE — the caller must put it in the transcript. It cannot inject one.
//
//   * IT SENDS NOTHING ON OPEN. There is no function here that a mount could call. Opening the
//     drawer issues no request; only a send or a suggestion click does (locked decision 9).
//
//   * IT CANNOT ASK FOR A VERDICT. `DEFAULT_SUGGESTIONS` are questions about evidence and
//     mechanism. A suggested question may never be "should I buy this?" — invariant #2 governs the
//     model's output, and this lane must not build a surface that invites one.
// ─────────────────────────────────────────────────────────────────────────────────────────────

import { AuthRequiredError } from "./api.js";

export { AuthRequiredError };

const BASE = import.meta.env.VITE_GATEWAY_URL || "";

// AskError carries the server's machine code so a caller can explain a refusal instead of showing
// "request failed". Byte-for-byte the MonitoringError / EventsError contract.
export class AskError extends Error {
  constructor(message, { code = "", status = 0 } = {}) {
    super(message);
    this.name = "AskError";
    this.code = code;
    this.status = status;
  }
}

// request is the shared fetch wrapper: session cookie always sent so the gateway can resolve the
// signed-in user's persona, 401 raised as AuthRequiredError, everything else as AskError.
async function request(url, opts = {}) {
  const res = await fetch(url, { credentials: "include", ...opts });
  if (res.status === 401) throw new AuthRequiredError("sign in required");
  let body = null;
  try {
    body = await res.json();
  } catch {
    body = null;
  }
  if (!res.ok) {
    throw new AskError(body?.error || `request failed (${res.status})`, {
      code: body?.code || "",
      status: res.status,
    });
  }
  return body ?? {};
}

// sendContextualChat posts a transcript to the existing grounded chat route.
//
// `messages` is the transcript AS THE USER SEES IT — including the context turn, if one was
// attached, because that turn is a visible user message. This function does not know which turn is
// the context turn and does not need to; that is what "no hidden preamble" means in code.
export function sendContextualChat({ ticker, timeframe = "1D", messages = [], personaId = null }) {
  const qs = `?timeframe=${encodeURIComponent(timeframe)}`;
  return request(`${BASE}/api/chat/${encodeURIComponent(ticker)}${qs}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ messages, personaId }),
  });
}

// ─────────────────────────────────────────────────────────────────── the visible context turn

// buildContextTurn assembles the "here is what I am looking at" turn from data ALREADY ON SCREEN.
//
// Every line it emits came from something the user can see: the event's title and type, the
// companies it names, the key facts the card already lists, the ticker and the timeframe. Nothing
// is fetched, nothing is inferred, and nothing is added that the user did not already have in
// front of them — so reading the transcript is a complete account of what the model was given.
//
// Returns `null` when there is no context, which is the ordinary case and must stay working: the
// drawer's behaviour with no context is exactly what it was before this lane touched it.
export function buildContextTurn(context) {
  if (!context) return null;
  const label = String(context.label || "").trim();
  const facts = (context.facts || []).map((f) => String(f).trim()).filter(Boolean);
  if (!label && facts.length === 0) return null;

  const lines = [];
  lines.push(label ? `I am looking at: ${label}` : "I am looking at this company.");
  if (facts.length > 0) {
    lines.push("");
    lines.push("What is on screen:");
    for (const f of facts) lines.push(`- ${f}`);
  }
  return { role: "user", content: lines.join("\n"), isContext: true };
}

// DEFAULT_SUGGESTIONS — the fallbacks per `context.kind`, used when the launcher supplied none.
//
// They are QUESTIONS ABOUT EVIDENCE, and that is the constraint, not a style preference. Each one
// asks the model to explain a mechanism, name what would change its mind, or point at a source.
// None asks for a verdict, a level to act on, a price target, or a position. Invariant #2 governs
// what the model may say; this list governs what the product invites the user to ask, and a
// suggestion chip is an invitation with the product's name on it.
export const DEFAULT_SUGGESTIONS = {
  event: [
    "Why does this matter?",
    "How would this reach the company's revenue?",
    "What would tell me this is already priced in?",
  ],
  ticker: [
    "What does the evidence actually support right now?",
    "Which evidence layer is weakest, and why?",
    "What would change this read?",
  ],
  following: [
    "What changed across the companies I follow?",
    "Which of these is most material, and why?",
    "What am I likely to be missing here?",
  ],
};

// suggestionsFor picks the list to show: whatever the launcher supplied, else the default for this
// context kind, else the ticker defaults. Capped at three — doc §16.12 shows three, and a fourth
// turns a nudge into a menu.
export function suggestionsFor(context, supplied) {
  const list = (supplied || []).map((s) => String(s).trim()).filter(Boolean);
  if (list.length > 0) return list.slice(0, 3);
  return (DEFAULT_SUGGESTIONS[context?.kind] || DEFAULT_SUGGESTIONS.ticker).slice(0, 3);
}
