// thesisApi.js — the THESIS-specific frontend adapter (Step 05, Wave 2 Lane A).
//
// A separate module on purpose. `lib/api.js` is on PARALLEL_EXECUTION.md's shared-file list, and its
// v1 thesis helpers (`listTheses`, `createThesis`, …) are still consumed by SnapshotSection and
// TodayView, which are NOT this lane's files. Those helpers keep working untouched because Step 04
// mirrors `text = claim` on every write (PARALLEL_CONTRACTS.md §1.5); this module is the structured
// v2 surface the Thesis workspace needs on top of them.
//
// Everything here is a thin, honest mapping of the Step 04 journal API (PARALLEL_CONTRACTS.md §1).
// It invents no field, defaults no user-owned value, and derives nothing the server did not send.

import { AuthRequiredError } from "./api.js";

export { AuthRequiredError };

const JOURNAL_BASE = import.meta.env.VITE_JOURNAL_URL || "http://localhost:8096";
const GATEWAY_BASE = import.meta.env.VITE_GATEWAY_URL || "";

// ---- server limits, mirrored exactly (journal/theses.go:45-62) ----------------------------------
// Duplicated as constants so the form can warn BEFORE a round-trip; the server stays the authority.
export const LIMITS = {
  claim: 2000, // truncated server-side on overflow, never rejected (§1.2)
  notes: 4000,
  itemText: 500,
  itemsPerList: 20,
  activeTheses: 3,
  reason: 280,
};

// ---- vocabularies, mirrored exactly (§1.3) ------------------------------------------------------

export const HORIZONS = [
  { value: "days", label: "Days" },
  { value: "weeks", label: "Weeks" },
  { value: "months", label: "Months" },
  { value: "quarters", label: "Quarters" },
  { value: "years", label: "Years" },
];

export const CLOSE_REASONS = [
  { value: "invalidated", label: "Invalidated — the evidence went against it" },
  { value: "played_out", label: "Played out — it resolved as expected" },
  { value: "position_exited", label: "Position exited" },
  { value: "superseded", label: "Superseded by a newer thesis" },
  { value: "abandoned", label: "Abandoned" },
];

// STATUS_META — lifecycle presentation. `glyph` and `label` carry the state on their own so nothing
// depends on colour alone (accessibility requirement).
export const STATUS_META = {
  draft: { label: "Draft", glyph: "◌", tone: "text-muted", help: "Not active yet — still being written." },
  active: { label: "Active", glyph: "●", tone: "text-accent", help: "A live belief you are tracking." },
  closed: { label: "Closed", glyph: "■", tone: "text-info", help: "Concluded, with a reason on the record." },
  archived: { label: "Archived", glyph: "◇", tone: "text-muted/70", help: "Kept for the record, out of the way." },
};

// LEGAL_TRANSITIONS mirrors journal/theses.go legalThesisTransition. `closed -> active` is absent by
// design: a reopen goes through `archived` so it is always visible in history (§1.3).
export const LEGAL_TRANSITIONS = {
  draft: ["active", "archived"],
  active: ["closed", "archived"],
  closed: ["archived"],
  archived: ["active"],
};

// CONFIDENCE_STEPS — the fixed 5-step ordinal control from §1.6, so stored values stay comparable
// across users. A value that did not come from this control (an API client sending 63) is rendered
// at the nearest step with its exact number shown, never clamped.
export const CONFIDENCE_STEPS = [
  { step: 1, label: "Low", value: 20 },
  { step: 2, label: "Modest", value: 40 },
  { step: 3, label: "Moderate", value: 60 },
  { step: 4, label: "High", value: 80 },
  { step: 5, label: "Very high", value: 95 },
];

export function nearestConfidenceStep(value) {
  if (value == null) return null;
  return CONFIDENCE_STEPS.reduce((best, s) =>
    Math.abs(s.value - value) < Math.abs(best.value - value) ? s : best
  );
}

// ITEM_LISTS — the five repeatable fields. One table drives the form, the diff, and the version
// labels, so a sixth list cannot be added in one place and forgotten in another.
export const ITEM_LISTS = [
  {
    field: "assumptions",
    label: "Key assumptions",
    singular: "assumption",
    help: "Conditions that must stay true for the claim to hold.",
    placeholder: "e.g. Data-center capex keeps growing through FY27",
    // monitorKind mounts Step 08's per-item monitoring beside the item it watches, and says which
    // offer is honest there. It is set ONLY on the lists `alerts` can actually represent
    // (allowedIntents in alerts/rules.go): an assumption takes a review reminder, an invalidation
    // condition and a catalyst take a deterministic threshold. Risks and next questions have no
    // valid item-scoped rule, so they get no control rather than one that 400s.
    monitorKind: "assumption",
  },
  {
    field: "catalysts",
    label: "Catalysts",
    singular: "catalyst",
    help: "Events that would reveal whether this is working.",
    placeholder: "e.g. Q3 earnings call",
    hasDueAt: true,
    monitorKind: "catalyst",
    monitorEvaluable: true,
  },
  {
    field: "risks",
    label: "Risks",
    singular: "risk",
    help: "Known ways this could fail.",
    placeholder: "e.g. Export restrictions widen",
  },
  {
    field: "invalidationConditions",
    label: "Invalidation conditions",
    singular: "invalidation condition",
    help: "Observable things that would make you wrong. This is what makes the claim falsifiable.",
    placeholder: "e.g. Two consecutive quarters of declining data-center revenue",
    monitorKind: "invalidation",
    monitorEvaluable: true,
  },
  {
    field: "nextQuestions",
    label: "Next questions",
    singular: "next question",
    help: "What you still need to find out.",
    placeholder: "e.g. What is the actual CoWoS capacity commitment?",
    hasResolved: true,
  },
];

export const ITEM_LIST_LABELS = Object.fromEntries(
  ITEM_LISTS.map((l) => [l.field, l.label])
);

// ---- errors ------------------------------------------------------------------------------------

// ThesisApiError preserves the server's machine-readable envelope (journal writeAPIError: `error`,
// `code`, `field`, plus per-code extras like `cap`/`activeCount`). `field` is what lets the editor
// attach a server error to the input that caused it instead of firing a disconnected toast.
export class ThesisApiError extends Error {
  constructor(message, { status = 0, code = "", field = "", cap, activeCount } = {}) {
    super(message);
    this.name = "ThesisApiError";
    this.status = status;
    this.code = code;
    this.field = field;
    this.cap = cap;
    this.activeCount = activeCount;
  }

  // isUnavailable — the service is down or refused to touch the store. Distinct from a validation
  // failure: the user's input is fine, so the UI must not blame a field.
  get isUnavailable() {
    return this.status === 0 || this.status >= 500 || this.code === "store_unreadable";
  }
}

// journalFetch — credentialed (the session rides in an httpOnly cookie), and it maps a 401 on a
// write to the app's typed AuthRequiredError so callers route the user to sign-in rather than
// surfacing a dead 401. Mirrors `request()` in lib/api.js, which is module-private there.
async function journalFetch(path, opts = {}) {
  const method = (opts.method || "GET").toUpperCase();
  let res;
  try {
    res = await fetch(`${JOURNAL_BASE}${path}`, { credentials: "include", ...opts });
  } catch {
    throw new ThesisApiError("the journal service is unreachable", { status: 0, code: "unreachable" });
  }

  if (res.status === 204) return null;

  let body = null;
  try {
    body = await res.json();
  } catch {
    /* a non-JSON body stays null; the status still drives the error below */
  }

  if (res.status === 401 && method !== "GET") {
    throw new AuthRequiredError(body?.error || "sign in required");
  }
  if (!res.ok) {
    throw new ThesisApiError(body?.error || `journal ${res.status}`, {
      status: res.status,
      code: body?.code || "",
      field: body?.field || "",
      cap: body?.cap,
      activeCount: body?.activeCount,
    });
  }
  return body;
}

const jsonBody = (obj) => ({
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify(obj),
});

// ---- reads -------------------------------------------------------------------------------------

// listThesesV2 returns every thesis for a ticker in EVERY lifecycle state (no status filter), because
// the workspace has to show drafts and closed/archived records, not just active ones.
export async function listThesesV2(ticker) {
  const q = ticker ? `?ticker=${encodeURIComponent(ticker)}` : "";
  const data = await journalFetch(`/theses${q}`);
  return data?.theses || [];
}

export async function getThesis(id) {
  const data = await journalFetch(`/theses/${encodeURIComponent(id)}`);
  return data?.thesis || null;
}

// listThesisVersions returns the append-only history newest-first, plus whether the response is the
// whole history (the record caps at 200 entries and the query caps at `limit`).
export async function listThesisVersions(id, limit = 50) {
  const data = await journalFetch(
    `/theses/${encodeURIComponent(id)}/versions?limit=${encodeURIComponent(limit)}`
  );
  return { versions: data?.versions || [], truncated: Boolean(data?.truncated) };
}

// ---- writes ------------------------------------------------------------------------------------

// createThesisV2 posts the full structured record. New items arrive without ids and the server mints
// them (`resolveItems`). Created as `draft` unless the caller says otherwise, so authoring does not
// consume one of the 3 active slots before the user has decided the thesis is live.
export async function createThesisV2(ticker, fields) {
  const data = await journalFetch("/theses", {
    method: "POST",
    ...jsonBody({ ticker, status: "draft", ...fields }),
  });
  return data?.thesis || null;
}

// patchThesis sends ONLY the keys the caller built (see buildPatch). Absent means "leave untouched";
// an explicit null clears a nullable scalar; an array replaces that list wholesale (§1.9).
export async function patchThesis(id, patch) {
  const data = await journalFetch(`/theses/${encodeURIComponent(id)}`, {
    method: "PATCH",
    ...jsonBody(patch),
  });
  return data?.thesis || null;
}

export async function deleteThesisV2(id) {
  return journalFetch(`/theses/${encodeURIComponent(id)}`, { method: "DELETE" });
}

// markThesisReviewed advances the "you have reviewed everything up to here" boundary (§1.8). Explicit
// user action only — nothing schedules this.
export async function markThesisReviewed(id, itemCount = 0) {
  const data = await journalFetch(`/theses/${encodeURIComponent(id)}/review-checkpoint`, {
    method: "POST",
    ...jsonBody({ itemCount, source: "user" }),
  });
  return data?.thesis || null;
}

// runThesisCheck re-runs the MODEL's evidence check through the gateway. On-demand only (invariant
// #4: the LLM stays out of every polling loop). The result lands in `lastCheck`, which is
// system-authored and never merged into a user-owned field.
export async function runThesisCheck(id) {
  const url = `${GATEWAY_BASE}/api/theses/${encodeURIComponent(id)}/check`;
  let res;
  try {
    res = await fetch(url, { method: "POST", credentials: "include" });
  } catch {
    throw new ThesisApiError("the check service is unreachable", { status: 0, code: "unreachable" });
  }
  let body = null;
  try {
    body = await res.json();
  } catch {
    /* ignore */
  }
  if (res.status === 401) throw new AuthRequiredError(body?.error || "sign in required");
  if (!res.ok) {
    throw new ThesisApiError(body?.error || `gateway ${res.status}`, {
      status: res.status,
      code: body?.code || "",
    });
  }
  return body;
}

// ---- form <-> record mapping -------------------------------------------------------------------

const EMPTY_LISTS = () => Object.fromEntries(ITEM_LISTS.map((l) => [l.field, []]));

// toForm projects a stored thesis (v1, dual-shape, or v2) into editable form state.
//
// Absence is preserved as absence. A legacy record with no horizon gets `""`, not a guessed value; no
// missing field is ever back-filled from `lastCheck` or from the model. This is the frontend half of
// Step 04's `normalizeThesis` guarantee.
export function toForm(t) {
  if (!t) {
    return {
      claim: "",
      horizon: "",
      confidence: null,
      notes: "",
      status: "draft",
      closeReason: "",
      closedByConditionId: "",
      ...EMPTY_LISTS(),
    };
  }
  const lists = Object.fromEntries(
    ITEM_LISTS.map((l) => [
      l.field,
      // Rows carry the server id when they have one. A blank id marks a row the user just added,
      // which the server will mint an id for on save.
      (t[l.field] || []).map((it) => ({
        id: it.id || "",
        text: it.text || "",
        createdAt: it.createdAt ?? null,
        resolvedAt: it.resolvedAt ?? null,
        dueAt: it.dueAt ?? null,
      })),
    ])
  );
  return {
    // `claim` falls back to `text` exactly as the server's compatibility reader does, so a legacy
    // text-only thesis shows its ORIGINAL text verbatim as the claim.
    claim: t.claim || t.text || "",
    horizon: t.horizon || "",
    confidence: t.confidence ?? null,
    notes: t.notes || "",
    status: t.status || "active",
    closeReason: t.closeReason || "",
    closedByConditionId: t.closedByConditionId || "",
    ...lists,
  };
}

const sameItems = (a = [], b = []) =>
  a.length === b.length &&
  a.every((x, i) => {
    const y = b[i];
    return (
      x.id === y.id &&
      x.text.trim() === y.text.trim() &&
      (x.resolvedAt ?? null) === (y.resolvedAt ?? null) &&
      (x.dueAt ?? null) === (y.dueAt ?? null)
    );
  });

// buildPatch diffs the edited form against the record's own baseline and emits ONLY what changed.
//
// This matters for more than payload size: Step 04 appends a version event computed from the diff of
// stored-vs-incoming, so sending untouched fields is harmless but sending a field the user did not
// edit would make the history harder to read. `null` is used deliberately to CLEAR a nullable
// scalar, which is distinguishable from absent (§1.9).
export function buildPatch(form, baseline) {
  const patch = {};
  const base = toForm(baseline);

  if (form.claim.trim() !== base.claim.trim()) patch.claim = form.claim.trim();
  if (form.notes !== base.notes) patch.notes = form.notes;
  if (form.horizon !== base.horizon) patch.horizon = form.horizon || null;
  if ((form.confidence ?? null) !== (base.confidence ?? null)) {
    patch.confidence = form.confidence ?? null;
  }

  for (const l of ITEM_LISTS) {
    const next = (form[l.field] || []).filter((it) => it.text.trim() !== "");
    if (!sameItems(next, base[l.field])) {
      patch[l.field] = next.map((it) => {
        const row = { id: it.id || "", text: it.text.trim() };
        if (l.hasResolved) row.resolvedAt = it.resolvedAt ?? null;
        if (l.hasDueAt) row.dueAt = it.dueAt ?? null;
        return row;
      });
    }
  }
  return patch;
}

// validateForm mirrors the server's rules so the user sees a field-attached error without a
// round-trip. Returns { field: message }. The server stays the authority — this only front-runs it.
export function validateForm(form) {
  const errors = {};
  if (!form.claim.trim()) errors.claim = "A thesis needs a claim.";
  if (form.notes.length > LIMITS.notes) {
    errors.notes = `Notes must be at most ${LIMITS.notes} characters.`;
  }
  for (const l of ITEM_LISTS) {
    const rows = (form[l.field] || []).filter((it) => it.text.trim() !== "");
    if (rows.length > LIMITS.itemsPerList) {
      errors[l.field] = `At most ${LIMITS.itemsPerList} ${l.label.toLowerCase()}.`;
    } else if (rows.some((it) => it.text.trim().length > LIMITS.itemText)) {
      errors[l.field] = `Each entry must be at most ${LIMITS.itemText} characters.`;
    }
  }
  return errors;
}

// ---- display helpers ---------------------------------------------------------------------------

// fmtWhen renders a unix-seconds timestamp. 0 / null is "unknown" — Step 04 deliberately preserves a
// legacy record's missing createdAt rather than back-filling `now`, so the UI must say so honestly
// instead of printing the epoch (§1.5).
export function fmtWhen(unix) {
  if (!unix) return "unknown";
  return new Date(unix * 1000).toLocaleDateString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
  });
}

export function fmtWhenExact(unix) {
  if (!unix) return "unknown";
  return new Date(unix * 1000).toLocaleString();
}

// isUnstructured — the record has a claim and nothing else: no horizon, no confidence, and all five
// lists empty. Used only to LABEL the record and explain why the structured fields are blank; it never
// triggers a rewrite or a backfill.
//
// Deliberately NOT keyed on `schemaVersion`. Step 04's `normalizeThesis` stamps `schemaVersion: 2`
// on every record as it is read, so a v1 row on disk arrives here indistinguishable from a v2 one —
// by design, since normalization is in-memory and lossless. That means the frontend cannot prove a
// record "predates the structured form", so the copy this drives states what is observable (the
// fields are empty) rather than asserting a provenance we cannot verify.
export function isUnstructured(t) {
  if (!t) return false;
  if (t.horizon || t.confidence != null) return false;
  return ITEM_LISTS.every((l) => (t[l.field] || []).length === 0);
}

// countStructured — how many structured slots a thesis actually fills. Drives the completion hint in
// the list; it is descriptive, never a score or a grade.
export function countStructured(t) {
  let n = 0;
  if ((t?.claim || t?.text || "").trim()) n += 1;
  if (t?.horizon) n += 1;
  if (t?.confidence != null) n += 1;
  for (const l of ITEM_LISTS) if ((t?.[l.field] || []).length > 0) n += 1;
  return n;
}

export const STRUCTURED_SLOTS = 3 + ITEM_LISTS.length; // claim + horizon + confidence + five lists
