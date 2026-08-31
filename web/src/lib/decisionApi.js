import { AuthRequiredError } from "./api.js";

// decisionApi.js — the decision lane's own API adapter.
//
// Everything goes through the GATEWAY (`/api/decisions…`, `/api/outcomes…`), never the journal
// directly, for the same reason the evidence lane does: the SERVER decides what the record says about
// the state it was made in. This module has no way to send a snapshot, a P&L number, or a model
// signal — the server assembles all three. What it sends is what the user actually owns: their
// intent, their reasoning, their confidence, and the ids of what they looked at.
//
// Two rules are visible in the shape of this file rather than only in prose:
//
//   * `updateDecision` takes ONE field. A decision is write-once apart from its trade link, so there
//     is no patch object to build — changing your mind means recording a new decision.
//   * `reviewOutcome` never accepts a market result. That is derived from the linked trade or it is
//     absent, so a review can never assert a number the trade ledger would disagree with.
//
// Contract: PARALLEL_CONTRACTS.md §5. Errors carry a machine `code` the UI switches on:
//   unknown_reference · immutable_field · outcome_exists · validation_failed · store_unreadable ·
//   journal_unreachable

const BASE = import.meta.env.VITE_GATEWAY_URL || "";

export { AuthRequiredError };

// DecisionError carries the server's code and its extra fields (`unknown[]`, `field`, `outcomeId`)
// so a caller can explain a refusal precisely instead of showing "request failed".
export class DecisionError extends Error {
  constructor(message, { code = "", status = 0, ...extra } = {}) {
    super(message);
    this.name = "DecisionError";
    this.code = code;
    this.status = status;
    Object.assign(this, extra);
  }
}

async function decisionJSON(path, opts = {}) {
  const method = (opts.method || "GET").toUpperCase();
  const res = await fetch(`${BASE}${path}`, {
    credentials: "include",
    headers: opts.body ? { "Content-Type": "application/json" } : undefined,
    ...opts,
  });

  let body = null;
  try {
    body = await res.json();
  } catch {
    body = null;
  }

  if (res.status === 401 && method !== "GET") {
    throw new AuthRequiredError(body?.error || "sign in required");
  }
  if (!res.ok) {
    const { error, code, ...extra } = body || {};
    throw new DecisionError(error || `request failed (${res.status})`, {
      code,
      status: res.status,
      ...extra,
    });
  }
  return body || {};
}

// ---- vocabulary (D-05 option c) ------------------------------------------------------------------
//
// Three intents, and the copy for each says what the record IS rather than what the app thinks you
// should do. `do_not_act` and `wait` exist because a trade record cannot express them, and they are
// the reason the decision loop is measurable at all.

export const INTENTS = [
  {
    id: "act",
    label: "I acted",
    blurb: "You opened, added to, trimmed, or exited a position.",
    requiresAction: true,
  },
  {
    id: "do_not_act",
    label: "I decided not to act",
    blurb: "A deliberate non-action. Worth recording — it is a decision, and it is reviewable.",
    requiresAction: false,
  },
  {
    id: "wait",
    label: "I'm waiting",
    blurb: "Not yet. Note what you are waiting for in the conditions below.",
    requiresAction: false,
  },
];

export const ACTION_KINDS = [
  { id: "open", label: "Opened" },
  { id: "add", label: "Added" },
  { id: "trim", label: "Trimmed" },
  { id: "exit", label: "Exited" },
];

export const SIDES = [
  { id: "long", label: "Long" },
  { id: "short", label: "Short" },
];

export const HORIZONS = ["days", "weeks", "months", "quarters", "years"];

// The user's own conviction, on the same 5-step scale the thesis editor uses (D-04): one stored
// numeric value, one ordinal control, so confidences are comparable across records.
export const CONFIDENCE_STEPS = [
  { step: 1, label: "Low", value: 20 },
  { step: 2, label: "Modest", value: 40 },
  { step: 3, label: "Moderate", value: 60 },
  { step: 4, label: "High", value: 80 },
  { step: 5, label: "Very high", value: 95 },
];

// Reasoning quality is 1..5 and is about PROCESS. The labels deliberately never mention profit:
// "good result" and "good reasoning" are different questions, and conflating them is the single most
// common way a trading journal teaches the wrong lesson.
export const REASONING_QUALITY = [
  { value: 1, label: "Poor", blurb: "I skipped steps I know matter." },
  { value: 2, label: "Weak", blurb: "Thin evidence, or I ignored what contradicted me." },
  { value: 3, label: "Adequate", blurb: "Defensible, with gaps I can name." },
  { value: 4, label: "Good", blurb: "Evidence-led, and I wrote down what would falsify it." },
  { value: 5, label: "Strong", blurb: "I would make this decision the same way again." },
];

export const ASSUMPTION_VERDICTS = [
  { id: "held", label: "Held" },
  { id: "failed", label: "Failed" },
  { id: "unknown", label: "Too early to tell" },
];

export const INTENT_LABEL = Object.fromEntries(INTENTS.map((i) => [i.id, i.label]));
export const ACTION_LABEL = Object.fromEntries(ACTION_KINDS.map((a) => [a.id, a.label]));

// ---- decisions -----------------------------------------------------------------------------------

export async function listDecisions({ ticker = "", thesisId = "", intent = "", limit = 100, cursor = "" } = {}) {
  const qs = new URLSearchParams();
  if (ticker) qs.set("ticker", ticker);
  if (thesisId) qs.set("thesisId", thesisId);
  if (intent) qs.set("intent", intent);
  if (limit) qs.set("limit", String(limit));
  if (cursor) qs.set("cursor", cursor);
  const data = await decisionJSON(`/api/decisions${qs.toString() ? `?${qs}` : ""}`);
  return {
    decisions: data.decisions || [],
    nextCursor: data.nextCursor || "",
    // Which decisions have been reviewed — the numerator of "decisions that later got a review".
    reviewedIds: new Set(data.reviewedDecisionIds || []),
  };
}

export async function getDecision(id) {
  const data = await decisionJSON(`/api/decisions/${encodeURIComponent(id)}`);
  return { decision: data.decision, outcome: data.outcome || null };
}

// recordDecision sends only user-owned fields. The snapshot of the thesis, the cited evidence
// metadata, the price provenance and the validated-signal state are all composed server-side.
export async function recordDecision({
  ticker,
  thesisId,
  thesisVersionId = "",
  checkpointId = "",
  intent,
  actionKind = "none",
  actionSide = null,
  horizon = null,
  rationale,
  userConfidence = null,
  evidenceIds = [],
  reviewIds = [],
  changeItemIds = [],
  monitoringRuleIds = [],
  openQuestions = [],
  conditionsToMonitor = [],
  tradeId = null,
  decidedAt = 0,
}) {
  const body = {
    ticker,
    thesisId,
    intent,
    action: { kind: actionKind, side: actionKind === "none" ? null : actionSide },
    rationale,
    userConfidence,
    evidenceIds,
    reviewIds,
    changeItemIds,
    monitoringRuleIds,
    openQuestions,
    conditionsToMonitor,
  };
  if (thesisVersionId) body.thesisVersionId = thesisVersionId;
  if (checkpointId) body.checkpointId = checkpointId;
  if (horizon) body.horizon = horizon;
  if (tradeId) body.tradeId = tradeId;
  if (decidedAt) body.decidedAt = decidedAt;

  const data = await decisionJSON("/api/decisions", { method: "POST", body: JSON.stringify(body) });
  return data.decision;
}

// linkDecisionTrade is the ONLY mutation a decision accepts. Pass null to unlink.
export async function linkDecisionTrade(id, tradeId) {
  const data = await decisionJSON(`/api/decisions/${encodeURIComponent(id)}`, {
    method: "PATCH",
    body: JSON.stringify({ tradeId }),
  });
  return data.decision;
}

// deleteDecision removes the record AND its outcome review. The UI must say so before calling it.
export async function deleteDecision(id) {
  return decisionJSON(`/api/decisions/${encodeURIComponent(id)}`, { method: "DELETE" });
}

// ---- outcome reviews -----------------------------------------------------------------------------

export async function listOutcomes({ decisionId = "", thesisId = "", ticker = "" } = {}) {
  const qs = new URLSearchParams();
  if (decisionId) qs.set("decisionId", decisionId);
  if (thesisId) qs.set("thesisId", thesisId);
  if (ticker) qs.set("ticker", ticker);
  const data = await decisionJSON(`/api/outcomes${qs.toString() ? `?${qs}` : ""}`);
  return data.outcomes || [];
}

// reviewOutcome records what happened and how the reasoning held up. There is no market-result
// parameter: that is derived from the linked trade, so a review can never claim a number.
export async function reviewOutcome({
  decisionId,
  observedOutcome,
  horizonElapsed = false,
  catalystsOccurred = [],
  assumptions = [],
  reasoningQuality = null,
  processLessons = [],
  retrospective = "",
}) {
  const data = await decisionJSON("/api/outcomes", {
    method: "POST",
    body: JSON.stringify({
      decisionId,
      observedOutcome,
      horizonElapsed,
      catalystsOccurred,
      assumptions,
      reasoningQuality,
      processLessons,
      retrospective,
    }),
  });
  return data.outcome;
}

// reviseOutcome updates the four user-authored fields. Everything else about a review is what was
// observed, and is not rewritable.
export async function reviseOutcome(id, patch) {
  const data = await decisionJSON(`/api/outcomes/${encodeURIComponent(id)}`, {
    method: "PATCH",
    body: JSON.stringify(patch),
  });
  return data.outcome;
}

// summariseDecisions asks the server for a recap of records the caller owns. It is assembled from
// stored fields in code — it cites ids, marks inference, and cannot grade the user.
export async function summariseDecisions(decisionIds) {
  return decisionJSON("/api/decisions/summary", {
    method: "POST",
    body: JSON.stringify({ decisionIds }),
  });
}

// ---- display helpers -----------------------------------------------------------------------------

export function fmtDecisionTime(unix) {
  if (!unix) return "time unknown";
  return new Date(unix * 1000).toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

// describeAction renders the user's own action, or the honest absence of one.
export function describeAction(decision) {
  const kind = decision?.action?.kind;
  if (!kind || kind === "none") return INTENT_LABEL[decision?.intent] || "Recorded";
  const side = decision.action.side ? ` ${decision.action.side}` : "";
  return `${ACTION_LABEL[kind] || kind}${side}`;
}

// dataStateTone maps a snapshot's data state onto the app's existing badge tones. `synthetic` stays
// loud — it is invariant #1's visible half and is never softened.
export const DATA_STATE_LABEL = {
  live: "live data",
  seed: "seed data",
  synthetic: "synthetic",
  unknown: "source unknown",
};

export function decisionErrorMessage(err) {
  if (err instanceof AuthRequiredError) return "Sign in to record decisions.";
  if (!(err instanceof DecisionError)) return err?.message || "Something went wrong.";
  switch (err.code) {
    case "unknown_reference":
      return `${err.message}${
        Array.isArray(err.unknown) && err.unknown.length ? ` (${err.unknown.join(", ")})` : ""
      }`;
    case "immutable_field":
      return "A decision record cannot be edited — record a new decision instead. History should show that you changed your mind.";
    case "outcome_exists":
      return "This decision already has a review. Open it to revise your assessment.";
    case "validation_failed":
      return err.message;
    case "store_unreadable":
      return "Your stored decisions could not be read, so nothing was overwritten. The file needs attention before new records can be saved.";
    case "journal_unreachable":
      return "The journal service is unreachable, so nothing was saved.";
    default:
      return err.message;
  }
}
