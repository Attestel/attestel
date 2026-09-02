// agencyApi.js — the Hermes research-agency adapter.
//
// A separate module beside analystApi.js / monitoringApi.js / thesisApi.js for the reason that file
// gives: `lib/api.js` is shared, and a lane that needs new calls adds its own `*Api.js`.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// TWO FUNCTIONS REACH THE MODEL AND FOUR DO NOT, AND THE SPLIT IS THE WHOLE DESIGN.
//
// `startAgencyRun` is the only call that can cause work on the owner's machine — and even it does
// not cause it directly. It writes a QUEUED ROW and returns 202. Nothing runs until a bridge on the
// owner's own computer chooses to claim that row. The hosted deployment has no path to the model at
// all in this lane, which is a stronger property than invariant #4 requires.
//
// `fetchAgencyRun` / `pollAgencyRun` read a stored row. They reach no upstream beyond the journal,
// they cannot claim, resume, retry or extend anything, and they are therefore the only calls here
// that may be polled or called from an effect. The gateway and the journal both enforce that split.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// WHAT THIS MODULE STRUCTURALLY CANNOT DO:
//   * it cannot ask for a profile, a toolset, a model, a provider, a path or a prompt — the create
//     body has three fields and the server refuses any fourth (DisallowUnknownFields);
//   * it cannot turn a research artifact into a signal — the artifact has no such field;
//   * it cannot present a research priority as a rating — see PRIORITY_LABELS.

import { AuthRequiredError } from "./api.js";

export { AuthRequiredError };

const BASE = import.meta.env?.VITE_GATEWAY_URL || "";

export class AgencyError extends Error {
  constructor(message, { code = "", status = 0 } = {}) {
    super(message);
    this.name = "AgencyError";
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
    throw new AgencyError(body?.error || `request failed (${res.status})`, {
      code: body?.code || "",
      status: res.status,
    });
  }
  return body ?? {};
}

// The seven run states the server can report. Spelled once so a renderer cannot invent an eighth.
export const RUN_QUEUED = "queued";
export const RUN_CLAIMED = "claimed";
export const RUN_RUNNING = "running";
export const RUN_COMPLETED = "completed";
export const RUN_FAILED = "failed";
export const RUN_CANCELLED = "cancelled";
export const RUN_EXPIRED = "expired";

const TERMINAL = new Set([RUN_COMPLETED, RUN_FAILED, RUN_CANCELLED, RUN_EXPIRED]);

export function isTerminal(status) {
  return TERMINAL.has(status);
}

// The four Hermes profiles, in order, so the UI can show honest progress: which stage the worker
// says it is on, out of how many. The server validates the same list on every artifact.
export const PROFILE_CHAIN = [
  "stock-scout",
  "stock-fundamentals",
  "stock-risk",
  "stock-chair",
];

// PROVENANCE_LABELS is what makes an artifact readable rather than merely present. A `sourced`
// claim and an `inferred` one look identical as sentences; they are not the same kind of thing, and
// the UI must never render them as though they were.
export const PROVENANCE_LABELS = {
  sourced: { label: "Sourced", tone: "accent", help: "A cited source states this." },
  calculated: { label: "Calculated", tone: "accent", help: "Derived by arithmetic from cited values; the basis is shown." },
  inferred: { label: "Inferred", tone: "warn", help: "The model's own reading. Not a fact." },
  unknown: { label: "Unknown", tone: "muted", help: "Explicitly not established from the sources found." },
};

// PRIORITY_LABELS — research instructions, never ratings. `investigate` means "this is worth more
// research"; it does not mean buy, and the copy here is written so it cannot be read that way.
export const PRIORITY_LABELS = {
  investigate: { label: "Investigate further", tone: "accent" },
  watch: { label: "Watch", tone: "muted" },
  reject: { label: "Question rejected", tone: "down" },
  unknown: { label: "Unknown", tone: "muted" },
};

// startAgencyRun queues ONE bounded research run. USER-INITIATED ONLY — wire it to a click and
// nothing else. It returns as soon as the row is written; `created:false` means an identical run
// was already in flight and this call attached to it rather than starting a second one.
export function startAgencyRun(ticker, question) {
  return request(`${BASE}/api/agency/runs`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ ticker, question }),
  });
}

// listAgencyRuns reads the caller's own runs. Cheap and safe to call on mount.
export function listAgencyRuns(limit = 10) {
  return request(`${BASE}/api/agency/runs?limit=${encodeURIComponent(limit)}`);
}

// fetchAgencyRun reads ONE run's status and, once there is one, its artifact. Model-free and safe
// to poll: it cannot start, resume or extend anything.
//
// `signal` is forwarded to fetch so an unmounting component aborts the request in flight rather
// than leaving it to resolve into a dead component. See pollAgencyRun.
export function fetchAgencyRun(runId, { signal = null } = {}) {
  return request(`${BASE}/api/agency/runs/${encodeURIComponent(runId)}`, { signal });
}

export function cancelAgencyRun(runId) {
  return request(`${BASE}/api/agency/runs/${encodeURIComponent(runId)}/cancel`, {
    method: "POST",
  });
}

// POLL_FLOOR_MS keeps a bad server-supplied cadence from turning this into a hot loop against a
// route that is cheap but not free. The cadence otherwise comes from the server's `pollAfterMs`.
const POLL_FLOOR_MS = 2000;

// pollAgencyRun follows a run to a terminal state. `onTick` sees every intermediate status so the
// UI can show which stage is running; `signal` aborts the loop when the component unmounts.
//
// It NEVER re-creates a run on its own. A run the server no longer knows about resolves as an
// error, and the UI offers to start a new one — a page load must not be able to queue work.
export async function pollAgencyRun(runId, { onTick = null, signal = null } = {}) {
  let waitMs = POLL_FLOOR_MS;
  for (;;) {
    if (signal?.aborted) return null;
    await new Promise((resolve) => setTimeout(resolve, waitMs));
    if (signal?.aborted) return null;

    // The signal is passed INTO the fetch so an abort tears the request down rather than waiting
    // for it, and it is checked AGAIN after the await.
    //
    // THAT SECOND CHECK IS NOT BELT-AND-BRACES, IT IS THE FIX FOR A REAL RACE. A request that was
    // already in flight when the caller aborted still resolves; without the re-check, `onTick`
    // fired afterwards and wrote a stale run into the state of a component that had unmounted or
    // moved on to a different run — so switching runs mid-poll could repaint the previous one's
    // status over the new one. An aborted fetch rejects here instead, and a fetch that beat the
    // abort is discarded by the guard.
    let run;
    try {
      run = await fetchAgencyRun(runId, { signal });
    } catch (err) {
      if (signal?.aborted || err?.name === "AbortError") return null;
      throw err;
    }
    if (signal?.aborted) return null;

    waitMs = Math.max(POLL_FLOOR_MS, Number(run?.pollAfterMs) || POLL_FLOOR_MS);
    onTick?.(run);
    if (isTerminal(run?.status)) return run;
  }
}

// stageProgress reports how far along the chain a run says it is, for honest progress display.
// Returns { index, total, label } — index is 0 when nothing has started, and it is derived from the
// server's validated `stage` field, never guessed from elapsed time.
export function stageProgress(run) {
  const total = PROFILE_CHAIN.length;
  const idx = PROFILE_CHAIN.indexOf(run?.stage);
  if (idx < 0) return { index: 0, total, label: null };
  return { index: idx + 1, total, label: run.stage };
}
