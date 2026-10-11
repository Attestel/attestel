// experimentReviewApi.js — the Hermes experiment-reviewer adapter.
//
// A separate module beside agencyApi.js for the reason that file gives: `lib/api.js` is shared, and
// a lane that needs new calls adds its own `*Api.js`.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// TWO CALLS DO SOMETHING AND THE REST ARE READS, AND THE SPLIT IS THE WHOLE DESIGN.
//
// `createExperimentSnapshot` reaches the live deployment. It is the only call here that causes any
// upstream work at all — the journal reads the paper service over GET and stores what it found. It
// CANNOT mutate the experiment: the journal has no method that could, so neither does anything
// reachable from this file. Wire it to a click and nothing else.
//
// `startExperimentReview` writes a QUEUED ROW and returns 202. Nothing runs until a bridge on the
// owner's own computer chooses to claim that row, so even this call does not reach a model. Wire it
// to a click and nothing else.
//
// `fetchExperimentSnapshot`, `listExperimentSnapshots` and the run reads re-exported from
// agencyApi.js are model-free store reads. They are the only calls here that may be polled.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// NOTHING IN THIS MODULE RUNS ON ITS OWN. There is no interval, no timer, no retry loop and no
// effect. Every function here is called from an event handler, or from `pollExperimentReview` after
// a review has already been started by one.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// WHAT THIS MODULE STRUCTURALLY CANNOT DO:
//   * it cannot ask for a prompt, a profile, a toolset, a model, a provider, a path or a URL — the
//     review body has two fields and the server refuses any third (DisallowUnknownFields);
//   * it cannot reset an experiment, write to the paper ledger or change a model — there is no
//     function here that issues any request but the four below;
//   * it cannot turn a review into a signal — the review artifact has no such field.

import { AuthRequiredError } from "./api.js";
import { AgencyError, pollAgencyRun, fetchAgencyRun, isTerminal } from "./agencyApi.js";

export { AuthRequiredError, AgencyError, fetchAgencyRun, isTerminal };

const BASE = import.meta.env?.VITE_GATEWAY_URL || "";

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
    throw new AgencyError(body?.error || `request failed (${res.status})`, {
      code: body?.code || "",
      status: res.status,
    });
  }
  return body ?? {};
}

// The three status vocabularies, spelled once so a renderer cannot invent a fourth value.
export const PAPER_STATUS = ["unjudged", "collecting", "measurable"];
export const CANDIDATE_STATUS = ["edge", "no_edge", "inconclusive", "unknown"];
export const OPERATIONAL_STATUS = ["verified", "degraded", "unavailable"];

// The three source states. `unavailable`, `stale` and a measured zero are DIFFERENT ANSWERS and the
// UI must never render them alike — see SOURCE_LABELS.
export const SOURCE_STATES = ["live", "stale", "unavailable"];

// PAPER_LABELS and CANDIDATE_LABELS carry the copy that keeps the two apart.
//
// THIS IS THE MOST IMPORTANT OBJECT IN THIS FILE. "The paper experiment has not been measured" and
// "the candidate strategy produced negative evidence" are different facts about different things,
// and a reader who conflates them concludes either that a working strategy failed or that a failed
// one is merely untested. The help text is written so neither reading is available.
export const PAPER_LABELS = {
  unjudged: {
    label: "Not measured",
    tone: "muted",
    help: "The paper experiment has produced no measured result — the official clock has not started, or the dashboard could not be read. This is a fact about the EXPERIMENT and says nothing about whether the strategy works.",
  },
  collecting: {
    label: "Collecting",
    tone: "accent",
    help: "The experiment clock is running and observations are accumulating, but there are not yet enough to measure against.",
  },
  measurable: {
    label: "Measurable",
    tone: "accent",
    help: "Enough observations have accumulated for the paper result to be measured.",
  },
};

export const CANDIDATE_LABELS = {
  edge: {
    label: "Edge",
    tone: "accent",
    help: "The offline evaluator established an edge, under the current strategy version and with current sample evidence.",
  },
  no_edge: {
    label: "No edge — negative evidence",
    tone: "down",
    help: "The offline evaluator ran with an adequate sample and found no edge in the CANDIDATE STRATEGY. This is a real, negative result about the strategy. It says nothing about whether the paper experiment has run.",
  },
  inconclusive: {
    label: "Inconclusive",
    tone: "warn",
    help: "The evaluator ran but could not decide — the sample was insufficient, the verdict is not spendable under the current strategy version, or the measurement was flagged as untrustworthy.",
  },
  unknown: {
    label: "Unknown",
    tone: "muted",
    // DELIBERATELY SILENT ON WHETHER A VERDICT WAS READ.
    //
    // This copy used to say "No usable evaluator verdict was read", which is FALSE in the state this
    // deployment is actually in: the evaluator ran and returned `NO EDGE`, about a model trained on
    // synthetic data. The conclusion is `unknown` because that result cannot be applied — not
    // because nobody measured anything. Whether a verdict was read is now answered by the separate
    // `evaluatorResult` / `evidenceValidity` pair the panel renders beside this, and this text says
    // only what the CONCLUSION means.
    help: "No conclusion about the candidate strategy can be drawn from this evidence. See the evaluator result beside this for what was actually recorded, and its validity for why it does or does not apply.",
  },
};

// EVALUATOR_VALIDITY_LABELS describe the SECOND axis: whether a recorded result can be applied to
// the real market. It is independent of the result itself — `NO EDGE` + `invalid` is a real reading,
// and the pair is why the conclusion above can be `unknown` without anybody claiming the evaluator
// never ran.
export const EVALUATOR_VALIDITY_LABELS = {
  valid: {
    label: "Applies to the real market",
    tone: "accent",
    help: "The verdict rests on a model and a feature frame whose provenance is verified real, so it characterises the market rather than invented data.",
  },
  invalid: {
    label: "Does not apply",
    tone: "down",
    help: "A verdict WAS recorded, but the data underneath it is synthetic or of unstated origin — so it characterises invented data and cannot be applied to the real market. This is why the conclusion is `unknown` rather than the verdict itself.",
  },
  unknown: {
    label: "No verdict recorded",
    tone: "muted",
    help: "No evaluator verdict was recorded at all, so there is nothing to apply. This is different from a verdict that was recorded and cannot be used.",
  },
};

// evaluatorResultLabel renders the raw verdict for display. It is passed through VERBATIM — the
// string the evaluator actually wrote — and never mapped onto the conclusion vocabulary.
export function evaluatorResultLabel(result) {
  if (!result) return "none recorded";
  if (result === "MIXED") return "mixed across configs";
  return result;
}

export const OPERATIONAL_LABELS = {
  verified: {
    label: "Verified",
    tone: "accent",
    help: "Every source was read, the launch checklist passes, and the three stores agree.",
  },
  degraded: {
    label: "Degraded",
    tone: "warn",
    help: "Everything was read, but something is wrong: a failing launch check, an integrity reason, a desynchronized config, or a stale source.",
  },
  unavailable: {
    label: "Unavailable",
    tone: "down",
    help: "At least one source could not be read at all. What it would have reported is UNKNOWN — not zero, and not healthy.",
  },
};

export const SOURCE_LABELS = {
  live: { label: "live", tone: "accent", help: "Read, and its own timestamp agrees with the snapshot's cutoff." },
  stale: { label: "stale", tone: "warn", help: "Read, but the source's own timestamp is older than the snapshot's cutoff." },
  unavailable: { label: "unavailable", tone: "down", help: "Could not be read. Nothing it would have reported is known — this is not a zero." },
};

// The review chain, in order, so the UI can show honest progress. The server validates the same
// list on every artifact.
export const REVIEW_CHAIN = ["experiment-auditor", "evidence-skeptic", "experiment-chair"];

// createExperimentSnapshot freezes the live deployment's evidence under ONE cutoff and stores it.
//
// USER-INITIATED ONLY. It reaches the paper service, so it must never be wired to a mount, an
// interval, a ticker change or a timer.
//
// A 409 with `code: "mixed_generation"` means the deployment changed generation mid-read and NOTHING
// WAS STORED. That is the design working — the evidence would have straddled a reset — and the
// correct response is to offer a retry, never to store it anyway.
export function createExperimentSnapshot() {
  return request(`${BASE}/api/experiments/snapshots`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    // No body. There is nothing for the client to parameterise: which endpoints are composed, in
    // what order and under what cutoff are the server's decisions.
  });
}

// listExperimentSnapshots reads the owner's stored snapshots, payloads stripped. Cheap and safe.
export function listExperimentSnapshots(limit = 10) {
  return request(`${BASE}/api/experiments/snapshots?limit=${encodeURIComponent(limit)}`);
}

// fetchExperimentSnapshot reads one stored snapshot in full. Safe to re-read: a snapshot is
// immutable, so it can never return different evidence.
export function fetchExperimentSnapshot(id) {
  return request(`${BASE}/api/experiments/snapshots/${encodeURIComponent(id)}`);
}

// startExperimentReview queues ONE bounded review of a stored snapshot.
//
// USER-INITIATED ONLY. It returns as soon as the row is written; `created:false` means an identical
// review was already in flight and this call attached to it rather than starting a second one — the
// snapshot is immutable, so a second run could only produce the same reading.
export function startExperimentReview(snapshotId) {
  return request(`${BASE}/api/agency/reviews`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ workflow: "experiment_review_v1", snapshotId }),
  });
}

// pollExperimentReview follows a started review to a terminal state. It is `pollAgencyRun` — the
// same model-free run read the research lane polls — re-exported under a name that says what it is
// following. It NEVER creates a run on its own.
export function pollExperimentReview(runId, opts) {
  return pollAgencyRun(runId, opts);
}

// reviewStageProgress reports how far along the chain a review says it is, derived from the
// server's validated `stage` field and never guessed from elapsed time.
export function reviewStageProgress(run) {
  const total = REVIEW_CHAIN.length;
  const idx = REVIEW_CHAIN.indexOf(run?.stage);
  if (idx < 0) return { index: 0, total, label: null };
  return { index: idx + 1, total, label: run.stage };
}
