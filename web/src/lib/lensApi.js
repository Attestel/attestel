// lensApi.js — the LENS-specific frontend adapter (Step 07, Wave 3 Lane A).
//
// A separate module for the same reason thesisApi.js and evidenceApi.js are: `lib/api.js` is on
// PARALLEL_EXECUTION.md's shared-file list, and Step 10 (Library) is building against the frozen
// review READ contract concurrently. This module owns the lens surface and nothing else.
//
// One thing this file deliberately CANNOT do: send evidence content. A lens request carries ids and
// the requested method, and the gateway resolves those ids against the caller's own records. There
// is no parameter here for evidence text, and that is the point — the browser is not a source of
// truth about what the user owns.

import { AuthRequiredError } from "./api.js";

export { AuthRequiredError };

const BASE = import.meta.env.VITE_GATEWAY_URL || "";

// ---- errors ------------------------------------------------------------------------------------

export class LensError extends Error {
  constructor(message, { status = 0, code = "", field = "", evidenceIds = [] } = {}) {
    super(message);
    this.name = "LensError";
    this.status = status;
    this.code = code;
    this.field = field;
    this.evidenceIds = evidenceIds;
  }

  get isUnavailable() {
    return this.status === 0 || this.status >= 500 ||
      this.code === "llm_unreachable" || this.code === "journal_unreachable" ||
      this.code === "analysis_unreachable";
  }
}

async function lensFetch(path, opts = {}) {
  const method = (opts.method || "GET").toUpperCase();
  let res;
  try {
    res = await fetch(`${BASE}${path}`, { credentials: "include", ...opts });
  } catch {
    throw new LensError("the service is unreachable", { status: 0, code: "unreachable" });
  }
  if (res.status === 204) return null;

  let body = null;
  try {
    body = await res.json();
  } catch {
    /* non-JSON body; status still drives the error */
  }

  if (res.status === 401 && method !== "GET") {
    throw new AuthRequiredError(body?.error || "sign in required");
  }
  if (!res.ok) {
    throw new LensError(body?.error || `gateway ${res.status}`, {
      status: res.status,
      code: body?.code || "",
      field: body?.field || "",
      evidenceIds: body?.evidenceIds || [],
    });
  }
  return body;
}

const jsonBody = (obj) => ({
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify(obj),
});

// ---- tasks -------------------------------------------------------------------------------------

// TASKS — the contextual actions, phrased as the research question they answer. These are the ONLY
// entry points: there is no empty chat box that runs a lens, and nothing here fires on page load,
// ticker change, or any poll (§3.8).
export const TASKS = [
  {
    id: "challenge_thesis",
    label: "Challenge this thesis",
    short: "Challenge",
    defaultLens: "builtin:skeptic",
    targets: ["thesis"],
    blurb: "Attack the assumptions, name contradicting and missing evidence, demand falsifiability.",
  },
  {
    id: "explain_evidence",
    label: "Explain this evidence",
    short: "Explain",
    defaultLens: "builtin:plain",
    targets: ["evidence", "thesis"],
    blurb: "Plain English: what it says, what it implies, and what that rests on.",
  },
  {
    id: "review_chart_context",
    label: "Review this chart context",
    short: "Chart",
    defaultLens: "builtin:technician",
    targets: ["chart_context", "thesis"],
    blurb: "Reads only the computed trend, momentum, levels, and timeframe conflict.",
  },
  {
    id: "teach_step",
    label: "Teach me this step",
    short: "Teach",
    defaultLens: "builtin:instructor",
    targets: ["thesis", "evidence", "chart_context"],
    blurb: "Explains the research method and asks guiding questions.",
  },
  {
    id: "apply_custom_framework",
    label: "Apply my specialist framework",
    short: "My framework",
    defaultLens: null,
    targets: ["thesis", "evidence", "chart_context"],
    blurb: "Your own method, applied inside the same grounding and safety rules.",
  },
  {
    id: "review_with_perspectives",
    label: "Review with perspectives",
    short: "Perspectives",
    defaultLens: null,
    targets: ["thesis"],
    blurb: "Several analysts examine different slices, argue both sides, and a judge summarises.",
  },
];

export const TASK_BY_ID = Object.fromEntries(TASKS.map((t) => [t.id, t]));

export function tasksForTarget(type) {
  return TASKS.filter((t) => t.targets.includes(type));
}

// ---- calls -------------------------------------------------------------------------------------

export async function listLenses() {
  const data = await lensFetch("/api/lenses");
  return { lenses: data?.lenses || [], tasks: data?.tasks || [] };
}

// runLens is the ONLY way a review is produced, and it is always a direct result of a click.
export async function runLens({
  task,
  target,
  lensId = "",
  ticker = "",
  timeframe = "1D",
  evidenceIds = [],
  priorReviewId = "",
  userInstruction = "",
}) {
  const data = await lensFetch("/api/lens", {
    method: "POST",
    ...jsonBody({ task, target, lensId, ticker, timeframe, evidenceIds, priorReviewId, userInstruction }),
  });
  return data?.review || null;
}

// listReviews / getReview are the frozen READ contract Step 10's Library consumes. Anything that
// changes here has to change there, so it does not change.
export async function listReviews({ ticker = "", thesisId = "", lensId = "", task = "", limit = 50 } = {}) {
  const q = new URLSearchParams();
  if (ticker) q.set("ticker", ticker);
  if (thesisId) q.set("thesisId", thesisId);
  if (lensId) q.set("lensId", lensId);
  if (task) q.set("task", task);
  if (limit) q.set("limit", String(limit));
  const data = await lensFetch(`/api/reviews?${q.toString()}`);
  return { reviews: data?.reviews || [], truncated: Boolean(data?.truncated) };
}

export async function getReview(id) {
  const data = await lensFetch(`/api/reviews/${encodeURIComponent(id)}`);
  return data?.review || null;
}

// rateReview / setCitationFeedback are the ONLY mutations a saved review accepts. The body of a
// review is immutable — if the analysis should change, you run a new one.
export async function rateReview(id, rating) {
  const data = await lensFetch(`/api/reviews/${encodeURIComponent(id)}`, {
    method: "PATCH",
    ...jsonBody({ rating }),
  });
  return data?.review || null;
}

export async function setCitationFeedback(id, citationFeedback) {
  const data = await lensFetch(`/api/reviews/${encodeURIComponent(id)}`, {
    method: "PATCH",
    ...jsonBody({ citationFeedback }),
  });
  return data?.review || null;
}

export async function deleteReview(id) {
  return lensFetch(`/api/reviews/${encodeURIComponent(id)}`, { method: "DELETE" });
}

// ---- display -----------------------------------------------------------------------------------

// FINDING_KIND — how a finding is LABELLED in the UI. The distinction is the product: a cited fact
// and the lens's own reasoning must never look the same, and neither may rely on colour alone.
export const FINDING_KIND = {
  fact: { label: "Cited", glyph: "❝", tone: "text-fg", help: "Backed by evidence you saved." },
  inference: {
    label: "Inference",
    glyph: "◈",
    tone: "text-warn",
    help: "The lens's own reasoning — not a cited fact.",
  },
};

export function fmtReviewTime(unixOrIso) {
  if (!unixOrIso) return "unknown";
  const d = typeof unixOrIso === "number" ? new Date(unixOrIso * 1000) : new Date(unixOrIso);
  if (Number.isNaN(d.getTime())) return "unknown";
  return d.toLocaleString();
}

export function lensErrorMessage(err) {
  if (err instanceof AuthRequiredError) return "Sign in to run a review.";
  if (err instanceof LensError) {
    if (err.code === "not_found" && err.evidenceIds?.length) {
      return "Some of the selected evidence could not be found. Refresh and try again.";
    }
    if (err.code === "not_found") return "That research object could not be found.";
    if (err.isUnavailable) {
      return "The review service is not responding. Nothing was saved — try again shortly.";
    }
    return err.message;
  }
  return err?.message || "Something went wrong.";
}
