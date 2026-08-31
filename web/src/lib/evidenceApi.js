import { AuthRequiredError } from "./api.js";

// evidenceApi.js — the evidence lane's own API adapter.
//
// Everything goes through the GATEWAY (`/api/evidence…`), never the journal directly. That is not
// just tidiness: the capture endpoint (`/api/evidence/from-fact`) exists precisely because the
// SERVER must be the one that decides where a fact came from. A browser that could POST a finished
// record to the journal could also invent its provenance, and then every source badge in the product
// would be decoration.
//
// So this module has no way to send `provider`, `dataState`, `attribution`, `sourceTime` or an
// excerpt. It sends a SELECTOR — "the RSI on the 1D chart", "this headline", "quote 3 of the Q1
// call" — plus the one thing the user genuinely owns: their note.
//
// Contract: PARALLEL_CONTRACTS.md §2.2/§2.6/§2.8. Errors carry a machine `code` the UI switches on:
//   provenance_incomplete · evidence_linked · immutable_field · quote_unverified · source_stale ·
//   value_unavailable · store_unreadable · active_thesis_cap
// Anything the server refuses to attribute, it refuses to store. Callers surface that refusal
// verbatim rather than retrying with weaker claims.

const BASE = import.meta.env.VITE_GATEWAY_URL || "";

export { AuthRequiredError };

// EvidenceError carries the server's machine code and extra fields (missing[], linkCount,
// thesisIds[]) so a caller can explain the refusal instead of showing "request failed".
export class EvidenceError extends Error {
  constructor(message, { code = "", status = 0, ...extra } = {}) {
    super(message);
    this.name = "EvidenceError";
    this.code = code;
    this.status = status;
    Object.assign(this, extra);
  }
}

// evidenceJSON is the single fetch path. A 401 on a write becomes AuthRequiredError (the app-wide
// signal to route to sign-in); every other failure becomes an EvidenceError with the code intact.
async function evidenceJSON(path, opts = {}) {
  const method = (opts.method || "GET").toUpperCase();
  const res = await fetch(`${BASE}/api/evidence${path}`, {
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
    throw new EvidenceError(error || `evidence request failed (${res.status})`, {
      code,
      status: res.status,
      ...extra,
    });
  }
  return body || {};
}

// ---- records ------------------------------------------------------------------------------------

// listEvidence reads the caller's captures, newest first. A guest gets an empty list, not an error.
export async function listEvidence({ ticker = "", kind = "", thesisId = "", limit = 100, cursor = "" } = {}) {
  const qs = new URLSearchParams();
  if (ticker) qs.set("ticker", ticker);
  if (kind) qs.set("kind", kind);
  if (thesisId) qs.set("thesisId", thesisId);
  if (limit) qs.set("limit", String(limit));
  if (cursor) qs.set("cursor", cursor);
  const data = await evidenceJSON(qs.toString() ? `?${qs}` : "");
  return { evidence: data.evidence || [], nextCursor: data.nextCursor || "" };
}

// saveFromFact is THE capture call. `kind` + `selector` name the object; the server resolves it and
// stamps the provenance. An optional thesisId/bearing links it in the same request, so a save can
// never half-succeed into an orphaned record the user never sees.
//
// Returns { evidence, deduplicated, link, linkUpdated }. `deduplicated: true` means this upstream
// item was already captured and the EXISTING record came back — the UI must say so rather than
// implying a new save.
export async function saveFromFact({
  ticker,
  kind,
  selector = {},
  timeframe = "1D",
  note = "",
  thesisId = "",
  bearing = "",
  thesisItemId = null,
  captureNew = false,
}) {
  const body = { ticker, kind, selector, timeframe, note, captureNew };
  if (thesisId) {
    body.thesisId = thesisId;
    body.bearing = bearing;
    if (thesisItemId) body.thesisItemId = thesisItemId;
  }
  return evidenceJSON("/from-fact", { method: "POST", body: JSON.stringify(body) });
}

// updateEvidenceNote edits the ONE mutable field. Everything else is capture state: a changed
// upstream value is a new capture, and the server rejects an attempt to edit one by name.
export async function updateEvidenceNote(id, note) {
  const data = await evidenceJSON(`/${id}`, { method: "PATCH", body: JSON.stringify({ note }) });
  return data.evidence;
}

// deleteEvidence throws EvidenceError{code:"evidence_linked", linkCount, thesisIds} when the record
// is still attached to a thesis. Pass force only after the user has been told what breaks.
export async function deleteEvidence(id, { force = false } = {}) {
  return evidenceJSON(`/${id}${force ? "?force=true" : ""}`, { method: "DELETE" });
}

// ---- links --------------------------------------------------------------------------------------

export async function listEvidenceLinks({ thesisId = "", evidenceId = "", includeRevoked = false } = {}) {
  const qs = new URLSearchParams();
  if (thesisId) qs.set("thesisId", thesisId);
  if (evidenceId) qs.set("evidenceId", evidenceId);
  if (includeRevoked) qs.set("includeRevoked", "true");
  const data = await evidenceJSON(`/links${qs.toString() ? `?${qs}` : ""}`);
  return data.links || [];
}

// linkEvidence creates the link, or changes the bearing of an existing one — (thesis, evidence) is
// unique, so moving an item from supporting to contradicting is a change of reading, not a new link.
export async function linkEvidence({ thesisId, evidenceId, bearing, thesisItemId = null, note = "" }) {
  const data = await evidenceJSON("/links", {
    method: "POST",
    body: JSON.stringify({ thesisId, evidenceId, bearing, thesisItemId, note }),
  });
  return { link: data.link, updated: !!data.updated };
}

export async function updateEvidenceLink(id, patch) {
  const data = await evidenceJSON(`/links/${id}`, { method: "PATCH", body: JSON.stringify(patch) });
  return data.link;
}

// unlinkEvidence removes the LINK ONLY. The evidence record survives with its provenance intact and
// can be linked again — "this doesn't bear on my thesis" is not "this never happened".
export async function unlinkEvidence(id) {
  return evidenceJSON(`/links/${id}`, { method: "DELETE" });
}

// ---- display helpers ----------------------------------------------------------------------------

export const BEARINGS = [
  {
    id: "supports",
    label: "Supports",
    blurb: "This is evidence for my claim.",
  },
  {
    id: "contradicts",
    label: "Contradicts",
    blurb: "This cuts against my claim. Recording it is the point.",
  },
  {
    id: "context",
    label: "Context",
    blurb: "Relevant background — it neither confirms nor challenges the claim.",
  },
];

export const EVIDENCE_KIND_LABELS = {
  deterministic_fact: "Computed fact",
  fundamental_fact: "Fundamental",
  chart_context: "Chart context",
  news: "News",
  filing: "Filing",
  transcript_excerpt: "Transcript quote",
  management_statement: "Management statement",
  catalyst: "Catalyst",
  user_note: "Your note",
};

// DATA_STATE_LABELS map onto the app's existing ProvenanceBadge kinds. `synthetic` stays loud — it is
// invariant #1's visible half and is never softened.
export const DATA_STATE_LABELS = {
  live: "live",
  seed: "seed data",
  synthetic: "synthetic",
};

// formatSourceTime renders a publication/event time. An unknown time (0) says so — it is NEVER
// replaced with the capture date, because that would present missing provenance as complete.
export function formatSourceTime(sourceTime) {
  if (!sourceTime) return "source time unknown";
  return new Date(sourceTime * 1000).toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

export function formatCapturedAt(capturedAt) {
  if (!capturedAt) return "capture time unknown";
  return new Date(capturedAt * 1000).toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

// ageLabel is the "how stale is this?" line the Thesis workspace shows next to each item.
export function ageLabel(unixSeconds) {
  if (!unixSeconds) return "age unknown";
  const s = Math.max(0, Math.floor(Date.now() / 1000 - unixSeconds));
  if (s < 3600) return `${Math.max(1, Math.floor(s / 60))}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  const d = Math.floor(s / 86400);
  if (d < 60) return `${d}d ago`;
  return `${Math.floor(d / 30)}mo ago`;
}

// evidenceErrorMessage turns a server refusal into a sentence that says what to do about it. The
// point is that a refusal to attribute is information, not noise: the user should learn that the
// source moved on, not that "something went wrong".
export function evidenceErrorMessage(err) {
  if (!(err instanceof EvidenceError)) return err?.message || "Something went wrong.";
  switch (err.code) {
    case "provenance_incomplete":
      return `${err.message}${Array.isArray(err.missing) && err.missing.length ? ` (missing: ${err.missing.join(", ")})` : ""}`;
    case "source_stale":
      return `${err.message}`;
    case "quote_unverified":
      return err.message;
    case "value_unavailable":
      return err.message;
    case "evidence_linked":
      return `Still linked to ${err.linkCount || "another"} thesis item${err.linkCount === 1 ? "" : "s"}.`;
    case "immutable_field":
      return "That part of a capture cannot be edited — save a new capture instead.";
    case "store_unreadable":
      return "Your stored evidence could not be read, so nothing was overwritten. The file needs attention before new captures can be saved.";
    case "journal_unreachable":
      return "The journal service is unreachable, so nothing was saved.";
    default:
      return err.message;
  }
}
