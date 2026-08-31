// monitoringApi.js — the MONITORING-specific frontend adapter (Step 08, Wave 4 Lane A).
//
// A separate module for the same reason thesisApi.js, evidenceApi.js, lensApi.js and libraryApi.js
// are: `lib/api.js` is on PARALLEL_EXECUTION.md's shared-file list, and Step 09 is building the
// decision adapter concurrently. This module owns the monitoring surface and nothing else.
//
// Two upstreams, deliberately different:
//   * the ALERTS service directly (rules, events, due reviews) — the same base `api.js` already uses,
//     because alerts verifies the session cookie itself with no gateway hop;
//   * the GATEWAY for the change set, because only the gateway can resolve a thesis, its evidence
//     links, its saved reviews and its alert events against the CALLER'S cookie and compose them.
//
// What this module cannot do, by construction:
//   * it cannot write a thesis field — there is no function here that PATCHes a thesis;
//   * it cannot fabricate a change — `fetchChanges` reads a set the server computed;
//   * it cannot poll a model. `summarizeChanges` is the only model-backed call and it exists solely
//     to be wired to an explicit button (contract §4.4, invariant #4).

import { AuthRequiredError } from "./api.js";

export { AuthRequiredError };

const BASE = import.meta.env.VITE_GATEWAY_URL || "";
const ALERTS_BASE = import.meta.env.VITE_ALERTS_URL || "http://localhost:8095";

// MonitoringError carries the server's machine code so a caller can explain a refusal instead of
// showing "request failed".
export class MonitoringError extends Error {
  constructor(message, { code = "", status = 0 } = {}) {
    super(message);
    this.name = "MonitoringError";
    this.code = code;
    this.status = status;
  }
}

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
    throw new MonitoringError(body?.error || `request failed (${res.status})`, {
      code: body?.code || "",
      status: res.status,
    });
  }
  return body ?? {};
}

const json = (body) => ({
  method: "POST",
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify(body),
});

// ---- monitoring rules (alerts service) ----------------------------------------------------------

// listRules returns the caller's rules. `thesisId` filters client-side because the alerts rules
// endpoint is already scoped to the caller and the list is small.
export async function listRules(thesisId = "") {
  const { rules = [] } = await request(`${ALERTS_BASE}/rules`);
  return thesisId ? rules.filter((r) => r.thesisId === thesisId) : rules;
}

// createRule sends the linkage fields verbatim. Note what it does NOT send: `userCreatedFromItem`.
// That flag is derived server-side from whether this body names a thesis item, which is what makes
// D-10's "the user asserted the causal link" precondition unforgeable from the browser.
export async function createRule({
  ticker,
  type,
  params = {},
  timeframe = "1D",
  cooldownSec,
  thesisId = "",
  thesisItemId = "",
  intent = "",
  reviewAt = 0,
}) {
  const body = { ticker, type, params, timeframe };
  if (cooldownSec != null) body.cooldownSec = cooldownSec;
  if (thesisId) body.thesisId = thesisId;
  if (thesisItemId) body.thesisItemId = thesisItemId;
  if (intent) body.intent = intent;
  if (reviewAt) body.reviewAt = reviewAt;
  const { rule } = await request(`${ALERTS_BASE}/rules`, json(body));
  return rule;
}

// watchCondition is the "watch this invalidation condition / catalyst" action rendered beside a
// thesis item. It is the ONLY path that can later produce a system-authored evidence link, and it
// exists as its own function so that intent is visible at the call site.
export function watchCondition({ ticker, thesisId, item, kind, type, params, timeframe = "1D" }) {
  const intent = kind === "invalidation" ? "invalidation" : "catalyst";
  return createRule({ ticker, type, params, timeframe, thesisId, thesisItemId: item, intent });
}

// scheduleReview creates a review reminder. This is the honest home for an assumption no evaluator
// can test: the product says "come back and look at this" rather than pretending to measure it.
export function scheduleReview({ ticker, thesisId, thesisItemId = "", everyDays = 0, reviewAt = 0 }) {
  const params = everyDays ? { everyDays } : {};
  return createRule({
    ticker,
    type: "thesis_review",
    params,
    thesisId,
    thesisItemId,
    intent: thesisItemId ? "assumption_review" : "thesis_review",
    reviewAt,
  });
}

export async function updateRule(id, patch) {
  const { rule } = await request(`${ALERTS_BASE}/rules/${encodeURIComponent(id)}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(patch),
  });
  return rule;
}

export function deleteRule(id) {
  return request(`${ALERTS_BASE}/rules/${encodeURIComponent(id)}`, { method: "DELETE" });
}

// ---- due reviews --------------------------------------------------------------------------------

// listDueReviews returns scheduled reviews that are due or overdue. Deterministic, no model call —
// safe to call on a panel mount.
export async function listDueReviews({ withinDays = 0, thesisId = "" } = {}) {
  const qs = new URLSearchParams();
  if (withinDays) qs.set("withinDays", String(withinDays));
  if (thesisId) qs.set("thesisId", thesisId);
  const suffix = qs.toString() ? `?${qs}` : "";
  const { due = [] } = await request(`${ALERTS_BASE}/monitoring/due${suffix}`);
  return due;
}

// fetchThesisMonitor is a read over deterministic monitor state. It never triggers a sweep or a
// model call, so it is safe on the personal Today view's explicit load/refresh cycle.
export function fetchThesisMonitor() {
  return request(`${BASE}/api/monitor/theses`);
}

// ---- events -------------------------------------------------------------------------------------

export async function listThesisEvents(thesisId, { since = 0, limit = 50 } = {}) {
  const qs = new URLSearchParams({ limit: String(limit), thesisId });
  if (since) qs.set("since", String(since));
  const { events = [] } = await request(`${ALERTS_BASE}/events?${qs}`);
  return events;
}

// ---- "what changed since your last review" (gateway) --------------------------------------------

// fetchChanges returns the DETERMINISTIC change set. It is the authority; the optional summary below
// only compresses it. `empty: true` is a real answer and must be rendered as one.
export function fetchChanges(thesisId) {
  return request(`${BASE}/api/theses/${encodeURIComponent(thesisId)}/changes`);
}

// summarizeChanges is the OPTIONAL on-demand model summary. Wire it to an explicit button only:
// never a mount, never an interval, never a ticker change (invariant #4). The reply is already
// validated server-side to cite only ids that exist in the change set.
export function summarizeChanges(thesisId) {
  return request(`${BASE}/api/theses/${encodeURIComponent(thesisId)}/changes/summary`, json({}));
}

// markReviewed advances the Step 04 review checkpoint through the gateway pass-through, so the next
// comparison has a stable boundary. This lane creates NO checkpoint store of its own (§4.3), and
// this is the only write it performs anywhere.
export function markReviewed(thesisId, itemCount) {
  return request(
    `${BASE}/api/theses/${encodeURIComponent(thesisId)}/review-checkpoint`,
    json({ itemCount, source: "user" }),
  );
}

// ---- display helpers ----------------------------------------------------------------------------

// CHANGE_KIND_LABELS covers exactly the seven kinds §4.4 allows. An unknown kind falls back to the
// raw string rather than being hidden — a change the UI cannot name is still a change.
export const CHANGE_KIND_LABELS = {
  thesis_version: "Thesis edit",
  evidence_linked: "Evidence linked",
  evidence_unlinked: "Evidence unlinked",
  lens_review: "Lens review",
  alert_event: "Monitoring alert",
  market_context: "Market context",
  transcript_comparison: "Transcript",
};

export const kindLabel = (kind) => CHANGE_KIND_LABELS[kind] || kind;

// bearingTone maps a bearing onto the shared Tag tones in components/terminal/bits.jsx. Only the
// tones defined there are valid (outline · llm · accent · warn · info · down) — anything else falls
// back to outline, which would silently render "strengthens" as neutral.
//
// `null` is the COMMON case and must read as neutral: an absent bearing means "nobody could
// determine one deterministically", not "no effect".
export function bearingTone(bearing) {
  if (bearing === "strengthens" || bearing === "supports") return "accent";
  if (bearing === "weakens" || bearing === "contradicts") return "down";
  if (bearing === "updates") return "warn";
  return "outline";
}

// applyResearchLink navigates using the structured link (§4.5). D-19 is unresolved — the company is
// not addressable in the URL — so the CLIENT sets the active company first and only then applies the
// hash. Doing it in the other order would land the user on the right section of the wrong company.
export function applyResearchLink(link, { onOpenCompany } = {}) {
  if (!link) return;
  if (link.ticker && onOpenCompany) onOpenCompany(link.ticker);
  if (link.hash) window.location.hash = link.hash;
}
