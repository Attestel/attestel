// onboardingApi.js — the guided-first-session adapter (Step 11).
//
// Its own module, against the journal directly, exactly like `thesisApi.js`: `lib/api.js` is on the
// shared-file list and this lane adds no helper to it.
//
// Two things are deliberately NOT in here:
//
//   1. No analytics call of any kind. Events are emitted SERVER-SIDE from durable writes
//      (PARALLEL_CONTRACTS.md §6.1, D-13 a). There is no SDK, no beacon, no fetch to a third party,
//      and no client-side counter anywhere in this file — if the browser could report progress, the
//      activation metric would measure what the UI believes rather than what was saved.
//   2. No completion claim. The client reports which STEP it is on; the server decides whether the
//      user is activated by re-reading their durable thesis, evidence, and review records.
//
// A signed-out visitor can still start: their progress lives in localStorage and the server records
// only the two guest-visible events (§6.3), attributed to a hashed random session id that is not a
// cookie, not an account, and not linked to one.

import { AuthRequiredError } from "./api.js";

export { AuthRequiredError };

const JOURNAL_BASE = import.meta.env.VITE_JOURNAL_URL || "http://localhost:8096";

const GUEST_SESSION_KEY = "attestel.onboarding.session";
const GUEST_STATE_KEY = "attestel.onboarding.guest";

// ---- the seven steps (mirrors journal/onboarding.go; the server validates) ---------------------
//
// One company research review, in the order the loop actually runs. `section` is the Research section
// the step happens in, so the guidance can point at the work instead of describing it.

export const STEPS = [
  {
    id: "choose_company",
    title: "Choose a company to research",
    help: "Pick the company you actually want an answer about. Everything else attaches to it.",
    section: "investigations",
    stage: "Orient",
  },
  {
    id: "orient_snapshot",
    title: "Run one focused investigation",
    help: "Ask a question, choose the evidence to include, and inspect the completed research trail.",
    section: "investigations",
    stage: "Orient",
  },
  {
    id: "write_claim",
    title: "Write one falsifiable claim",
    help: "One sentence you could be proven wrong about. Not “I like it” — something checkable.",
    section: "thesis",
    stage: "Form",
    needsAccount: true,
  },
  {
    id: "name_condition",
    title: "Name what would break it",
    help: "One assumption it rests on, or one condition that would invalidate it.",
    section: "thesis",
    stage: "Form",
    needsAccount: true,
  },
  {
    id: "attach_evidence",
    title: "Attach one source-backed item",
    help: "A fact, filing, headline or computed level — with its source — linked to the claim.",
    section: "evidence",
    stage: "Understand",
    needsAccount: true,
  },
  {
    id: "challenge_review",
    title: "Run Devil's Advocate",
    help: "Have the claim attacked: what is missing, what contradicts it, what is not falsifiable yet.",
    section: "perspectives",
    stage: "Challenge",
    needsAccount: true,
  },
  {
    id: "commit_next",
    title: "Choose your next question",
    help: "Record what you still need to know, or when you will review this again.",
    section: "thesis",
    stage: "Challenge",
    needsAccount: true,
  },
];

export const STEP_IDS = STEPS.map((s) => s.id);

export function stepDef(id) {
  return STEPS.find((s) => s.id === id) || STEPS[0];
}

export function stepIndex(id) {
  const i = STEP_IDS.indexOf(id);
  return i < 0 ? 0 : i;
}

// ---- guest session + local progress ------------------------------------------------------------

// guestSessionId returns (creating once) an opaque random id for this browser. It is NOT a login, it
// carries no personal data, and the server only ever stores its salted hash. localStorage may be
// unavailable (private mode, blocked storage) — then onboarding simply is not measured for guests,
// which is better than failing the page.
export function guestSessionId() {
  try {
    let id = localStorage.getItem(GUEST_SESSION_KEY);
    if (!id) {
      const buf = new Uint8Array(8);
      crypto.getRandomValues(buf);
      id = Array.from(buf, (b) => b.toString(16).padStart(2, "0")).join("");
      localStorage.setItem(GUEST_SESSION_KEY, id);
    }
    return id;
  } catch {
    return "";
  }
}

export function loadGuestProgress() {
  try {
    const raw = localStorage.getItem(GUEST_STATE_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw);
    return parsed && typeof parsed === "object" ? parsed : null;
  } catch {
    return null;
  }
}

export function saveGuestProgress(state) {
  try {
    localStorage.setItem(GUEST_STATE_KEY, JSON.stringify(state));
  } catch {
    /* storage unavailable — guidance still works, it just will not survive a reload */
  }
}

export function clearGuestProgress() {
  try {
    localStorage.removeItem(GUEST_STATE_KEY);
  } catch {
    /* ignore */
  }
}

// ---- server calls ------------------------------------------------------------------------------

async function journalJSON(path, opts = {}) {
  const res = await fetch(`${JOURNAL_BASE}${path}`, { credentials: "include", ...opts });
  const method = (opts.method || "GET").toUpperCase();
  if (res.status === 401 && method !== "GET") throw new AuthRequiredError();
  if (!res.ok) {
    let msg = `onboarding ${res.status}`;
    try {
      const b = await res.json();
      if (b?.error) msg = b.error;
    } catch {
      /* ignore */
    }
    throw new Error(msg);
  }
  return res.json();
}

// getOnboarding returns { state, steps, hasThesis, hasEvidenceLink, hasChallenge, guest }. Reading is
// open to guests (they get an empty guide) so the section renders without a failed request.
export async function getOnboarding() {
  const data = await journalJSON("/onboarding");
  return data?.onboarding || null;
}

// saveOnboarding reports progress. `status` is never sent — the server owns completion.
export async function saveOnboarding(patch = {}) {
  const body = {};
  if (patch.step) body.step = patch.step;
  if (patch.ticker) body.ticker = patch.ticker;
  if (patch.thesisId) body.thesisId = patch.thesisId;
  if (patch.level) body.level = patch.level;
  if (typeof patch.dismissed === "boolean") body.dismissed = patch.dismissed;
  if (patch.sessionId) body.sessionId = patch.sessionId;
  if (patch.resumed) body.resumed = true;

  const data = await journalJSON("/onboarding", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  return data?.onboarding || null;
}
