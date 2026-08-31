import { AuthRequiredError, fetchTickers } from "./api.js";

// libraryApi.js — the Library lane's retrieval adapter.
//
// Library is a VIEW over objects that are already durable somewhere else. This module reads those
// stores and normalizes what they return into one row shape. It does not persist anything, it owns no
// store, and it never copies a record into a second place to make it appear here. If an object is not
// already saved by some other lane, it does not belong in Library.
//
// The retrieval strategy is deliberately unfashionable: **fetch bounded lists, filter in the browser.**
// Every source is already capped (evidence pages at 100, transcripts at 12/ticker, reads and committee
// snapshots at 10/ticker, personas are a handful), so a full sweep is a few hundred rows. That is far
// below the point where an index earns its cost, and the prompt is explicit — no Elasticsearch, no
// vector database, no embeddings, no new service. When a store outgrows this, the fix is server-side
// filtering on that store's own endpoint, not a search tier in front of six of them.
//
// Two rules run through the whole file:
//
//   1. **NOTHING HERE RUNS A MODEL.** Every endpoint below is a directory listing or a JSON file read.
//      `GET /api/committee/{t}/history` reads stored snapshots; `GET /api/committee/{t}` (no
//      `/history`) RUNS the agent pipeline and must never be called from Library. Opening an item
//      re-reads nothing at all — the detail view renders the record already in memory.
//   2. **A SOURCE THAT IS DOWN SAYS SO.** Each loader reports its own status, so the UI can distinguish
//      "you have not saved anything yet" from "this store could not be read" from "this contract has
//      not shipped yet". Collapsing those three into an empty list would quietly lie about coverage.

const BASE = import.meta.env.VITE_GATEWAY_URL || "";
const JOURNAL_BASE = import.meta.env.VITE_JOURNAL_URL || "http://localhost:8096";

export { AuthRequiredError };

// Bounds. Each is a real cap, and when one bites the UI says so rather than silently showing a slice.
export const LIMITS = {
  evidencePerPage: 100,
  evidencePages: 4, // 400 captures; beyond that the browser says the view is truncated
  transcriptsPerTicker: 12,
  readsPerTicker: 10,
  committeePerTicker: 10,
  reviews: 100,
};

// ---- object types ------------------------------------------------------------------------------

// OBJECT_TYPES is the closed set Library can show. Each entry names the store that actually owns the
// object — Library is a reader of all of them and the owner of none.
export const OBJECT_TYPES = [
  {
    id: "evidence",
    label: "Evidence",
    owner: "journal",
    scope: "user",
    blurb: "Facts, filings, headlines, quotations and notes you captured against a company.",
  },
  {
    id: "review",
    label: "Lens reviews",
    owner: "journal",
    scope: "user",
    blurb: "Saved results of a research lens applied to a thesis, evidence item, or chart context.",
  },
  {
    id: "committee",
    label: "Perspective reviews",
    owner: "llm",
    scope: "company",
    blurb: "Stored multi-analyst runs with their deterministic consensus.",
  },
  {
    id: "transcript",
    label: "Transcript analyses",
    owner: "llm",
    scope: "company",
    blurb: "Stored earnings-call analyses. The call text itself is never retained.",
  },
  {
    id: "read",
    label: "Daily reads",
    owner: "llm",
    scope: "company",
    blurb: "One structured technical read per company per day, kept so you can see what moved.",
  },
  {
    id: "lens",
    label: "Research lenses",
    owner: "llm",
    scope: "mixed",
    blurb: "Built-in analytical methods and the Custom Specialist lenses you have written.",
  },
];

export const OBJECT_TYPE_LABELS = Object.fromEntries(OBJECT_TYPES.map((t) => [t.id, t.label]));

// SOURCE_STATUS values a loader can report.
//   ok          — read succeeded (the item list may still be empty, which is a real answer)
//   unavailable — the owning service could not be reached; coverage is INCOMPLETE and says so
//   pending     — the contract for this object type has not shipped yet (see loadReviews)
//   guest       — the object is user-owned and nobody is signed in
export const SOURCE_STATUS = { ok: "ok", unavailable: "unavailable", pending: "pending", guest: "guest" };

// ---- low-level fetch ---------------------------------------------------------------------------

// getJSON is a credentialed read. Library only ever reads, so there is no write path in this module
// and no 401-to-AuthRequiredError conversion: a guest simply sees less.
async function getJSON(url, signal) {
  const res = await fetch(url, { credentials: "include", signal });
  if (!res.ok) {
    const err = new Error(`${res.status}`);
    err.status = res.status;
    throw err;
  }
  return res.json();
}

// ---- normalization -----------------------------------------------------------------------------

// unixOf accepts the several time shapes these stores use (unix seconds, ISO instants, YYYY-MM-DD)
// and returns unix seconds, or 0 for "unknown". 0 is never replaced with now(): an item whose time we
// do not know renders as "date unknown", the same rule the evidence layer already follows.
function unixOf(value) {
  if (typeof value === "number" && Number.isFinite(value)) return value > 0 ? Math.floor(value) : 0;
  if (typeof value !== "string" || !value.trim()) return 0;
  const ms = Date.parse(value);
  return Number.isNaN(ms) ? 0 : Math.floor(ms / 1000);
}

// haystack builds the lowercase search text for a row. Only fields the owning store actually persists
// go in — for a transcript that is the DERIVED structure, never call text, because call text is not
// stored (D-08) and Library must not imply otherwise.
function haystack(...parts) {
  return parts
    .flat(3)
    .filter((p) => typeof p === "string" || typeof p === "number")
    .join("   ")
    .toLowerCase();
}

function firstLine(s, n = 160) {
  const t = String(s || "").replace(/\s+/g, " ").trim();
  return t.length > n ? `${t.slice(0, n - 1)}…` : t;
}

// ---- loaders -----------------------------------------------------------------------------------

// loadEvidence pages the caller's captures and joins each to the theses it is linked to. Both reads
// are per-user at the journal, so authorization stays where the data lives — Library never widens it.
async function loadEvidence(signal) {
  const items = [];
  let cursor = "";
  let truncated = false;
  try {
    for (let page = 0; page < LIMITS.evidencePages; page += 1) {
      const qs = new URLSearchParams({ limit: String(LIMITS.evidencePerPage) });
      if (cursor) qs.set("cursor", cursor);
      const data = await getJSON(`${BASE}/api/evidence?${qs}`, signal);
      items.push(...(data.evidence || []));
      cursor = data.nextCursor || "";
      if (!cursor) break;
      if (page === LIMITS.evidencePages - 1 && cursor) truncated = true;
    }
  } catch (e) {
    if (e.name === "AbortError") throw e;
    return { items: [], status: SOURCE_STATUS.unavailable, error: `evidence store unreachable (${e.message})` };
  }

  // Links are a separate store on purpose (unlinking must not touch a capture), so the thesis
  // association is a join here rather than a field on the record.
  let links = [];
  try {
    const data = await getJSON(`${BASE}/api/evidence/links`, signal);
    links = data.links || [];
  } catch (e) {
    if (e.name === "AbortError") throw e;
    // Non-fatal: captures still list, they just cannot show their thesis links. Reported as a warning
    // rather than swallowed, because "no thesis links" and "could not read thesis links" differ.
    links = null;
  }

  const linksByEvidence = new Map();
  (links || []).forEach((l) => {
    if (!linksByEvidence.has(l.evidenceId)) linksByEvidence.set(l.evidenceId, []);
    linksByEvidence.get(l.evidenceId).push(l);
  });

  return {
    items: items.map((rec) => {
      const recLinks = linksByEvidence.get(rec.id) || [];
      return {
        key: `evidence:${rec.id}`,
        type: "evidence",
        id: rec.id,
        ticker: rec.ticker || "",
        // A capture saved through the UI always has a server-built title; one created straight through
        // the API may not. Falling back to the user's own words beats a row that reads "(untitled)"
        // and tells the reader nothing about what they saved.
        title: rec.title || firstLine(rec.note || rec.excerpt, 90) || `${rec.kind} capture`,
        summary: firstLine(rec.excerpt || rec.note || (rec.fact ? `${rec.fact.key} = ${rec.fact.value}` : "")),
        at: rec.capturedAt || 0,
        sourceLabel: rec.provider || "",
        attribution: rec.attribution || "",
        dataState: rec.dataState || "",
        modelUsed: rec.inference ? rec.modelUsed : "",
        inference: !!rec.inference,
        stub: false,
        lensId: "",
        scope: "user",
        subtype: rec.kind,
        links: recLinks,
        thesisIds: recLinks.filter((l) => !l.revoked).map((l) => l.thesisId),
        raw: rec,
        searchText: haystack(
          rec.title, rec.excerpt, rec.note, rec.attribution, rec.provider, rec.kind,
          rec.ticker, rec.sourceUrl, rec.fact?.key, rec.fact?.value
        ),
      };
    }),
    status: SOURCE_STATUS.ok,
    // truncated is reported, never inferred: the UI must be able to say "there are older captures than
    // these" rather than presenting a page as the whole shelf.
    truncated,
    warning: links === null ? "Thesis links could not be read, so evidence rows may not show their thesis." : "",
  };
}

// loadReviews reads the frozen lens-review contract (PARALLEL_CONTRACTS §3.7) at its exact path.
//
// That route is Step 07's to build and does not exist on this branch. Rather than inventing a
// temporary endpoint — which would have to be removed later and would make Library look complete when
// it is not — this asks for the real one and reports `pending` when the journal has no such route. A
// missing route answers with Go's plain-text 404; a real empty list answers with JSON. That is how the
// two are told apart.
async function loadReviews(signal) {
  try {
    const res = await fetch(`${JOURNAL_BASE}/reviews?limit=${LIMITS.reviews}`, {
      credentials: "include",
      signal,
    });
    if (res.status === 404) {
      return { items: [], status: SOURCE_STATUS.pending };
    }
    if (!res.ok) {
      return { items: [], status: SOURCE_STATUS.unavailable, error: `reviews store returned ${res.status}` };
    }
    const data = await res.json().catch(() => null);
    if (!data || !Array.isArray(data.reviews)) {
      return { items: [], status: SOURCE_STATUS.pending };
    }
    return {
      items: data.reviews.map((rev) => ({
        key: `review:${rev.id}`,
        type: "review",
        id: rev.id,
        ticker: rev.ticker || "",
        title: reviewTitle(rev),
        summary: firstLine(
          (rev.findings || []).map((f) => f.statement).join(" · ") ||
            (rev.agreements || []).join(" · ")
        ),
        at: unixOf(rev.savedAt || rev.createdAt),
        sourceLabel: rev.lens?.name || rev.lens?.id || "lens",
        attribution: rev.lens?.builtin ? "Built-in lens" : "Your custom lens",
        dataState: "",
        modelUsed: rev.modelUsed || "",
        inference: false,
        stub: !!rev.stub,
        retried: !!rev.retried,
        lensId: rev.lens?.id || "",
        lensVersion: rev.lens?.version || "",
        scope: "user",
        subtype: rev.task || "",
        thesisIds: rev.thesisId ? [rev.thesisId] : [],
        raw: rev,
        searchText: haystack(
          rev.ticker, rev.task, rev.lens?.name, rev.lens?.id,
          (rev.findings || []).map((f) => f.statement),
          rev.agreements, rev.disagreements, rev.missingEvidence, rev.openQuestions
        ),
      })),
      status: SOURCE_STATUS.ok,
    };
  } catch (e) {
    if (e.name === "AbortError") throw e;
    return { items: [], status: SOURCE_STATUS.unavailable, error: `reviews store unreachable (${e.message})` };
  }
}

function reviewTitle(rev) {
  const lens = rev.lens?.name || rev.lens?.id || "Lens";
  const task = String(rev.task || "review").replace(/_/g, " ");
  return `${lens} — ${task}`;
}

// loadTranscripts sweeps the configured universe. Per-ticker endpoints mean one request per company;
// with a three-name universe that is three cheap directory reads, not a fan-out worth engineering around.
async function loadTranscripts(tickers, signal) {
  return sweepTickers(tickers, signal, async (sym) => {
    const data = await getJSON(`${BASE}/api/transcript/${sym}`, signal);
    return (data.analyses || []).map((rec) => {
      const s = rec.structured || {};
      const stub = String(rec.modelUsed || "").startsWith("stub:");
      return {
        key: `transcript:${sym}:${rec.key}`,
        type: "transcript",
        id: `${sym}/${rec.key}`,
        ticker: sym,
        title: `${rec.label || rec.key} — earnings call analysis`,
        summary: firstLine(s.summary || (s.keyPoints || [])[0] || ""),
        at: unixOf(rec.timestamp),
        sourceLabel: "transcript analysis",
        attribution: "Derived from a transcript you supplied. The call text itself is not retained.",
        dataState: "",
        modelUsed: rec.modelUsed || "",
        inference: false,
        stub,
        lensId: "",
        scope: "company",
        subtype: rec.label || rec.key,
        thesisIds: [],
        raw: rec,
        // Only the DERIVED structure is searchable, because only the derived structure is stored.
        searchText: haystack(
          sym, rec.label, s.summary, s.keyPoints, s.riskFlags, s.toneShift,
          s.tone?.notableLanguage,
          (s.evasiveMoments || []).map((m) => [m.question, m.answer, m.why, m.topic])
        ),
      };
    });
  });
}

async function loadReads(tickers, signal) {
  return sweepTickers(tickers, signal, async (sym) => {
    const data = await getJSON(`${BASE}/api/reads/${sym}?limit=${LIMITS.readsPerTicker}`, signal);
    return (data.reads || []).map((rec) => {
      const s = rec.structured || {};
      const stub = String(rec.modelUsed || "").startsWith("stub:");
      return {
        key: `read:${sym}:${rec.date}`,
        type: "read",
        id: `${sym}/${rec.date}`,
        ticker: sym,
        title: `${sym} daily read — ${rec.date}`,
        summary: firstLine(s.summary || s.headline || ""),
        at: unixOf(rec.timestamp || rec.date),
        sourceLabel: "daily read",
        attribution: "Structured technical read over the computed regime",
        dataState: "",
        modelUsed: rec.modelUsed || "",
        inference: false,
        stub,
        lensId: "",
        scope: "company",
        subtype: rec.date,
        thesisIds: [],
        raw: rec,
        searchText: haystack(
          sym, rec.date, s.headline, s.summary, s.whatHappened, s.watch, s.riskFlags,
          rec.regimeSnapshot?.trend, rec.regimeSnapshot?.momentum
        ),
      };
    });
  });
}

// loadCommittee reads STORED snapshots only — `/history`, never the bare committee route, which runs
// the whole agent pipeline. Opening Library must never cost a model call.
async function loadCommittee(tickers, signal) {
  return sweepTickers(tickers, signal, async (sym) => {
    const data = await getJSON(`${BASE}/api/committee/${sym}/history`, signal);
    return (data.snapshots || []).map((rec) => {
      const c = rec.consensus || {};
      const models = Object.values(rec.modelsUsed || {});
      const stub = models.some((m) => String(m).startsWith("stub:"));
      return {
        key: `committee:${sym}:${rec.date}`,
        type: "committee",
        id: `${sym}/${rec.date}`,
        ticker: sym,
        title: `${sym} perspective review — ${rec.date}`,
        summary: firstLine(
          c.label
            ? `Deterministic consensus: ${c.label} (agreement ${Math.round((c.agreement || 0) * 100)}%, ${c.n || 0} analysts)`
            : rec.judge?.summary || ""
        ),
        at: unixOf(rec.timestamp || rec.date),
        sourceLabel: "perspective review",
        attribution: "Consensus computed deterministically in code, never by the model",
        dataState: "",
        modelUsed: models.join(", "),
        inference: false,
        stub,
        lensId: "",
        scope: "company",
        subtype: c.label || "",
        thesisIds: [],
        raw: rec,
        searchText: haystack(
          sym, rec.date, c.label, rec.judge?.summary,
          Object.values(rec.analysts || {}).map((a) => [a?.stance, a?.summary]),
          // The bull/bear debate is where the sharpest argument usually is, so it is searchable —
          // finding "the run where someone raised hyperscaler concentration" is exactly the job.
          Object.values(rec.debate || {}).map((d) => [d?.stance, d?.summary]),
          (rec.judge?.agreements || []), (rec.judge?.disagreements || [])
        ),
      };
    });
  });
}

// loadLenses lists the built-in methods plus the caller's own Custom Specialists. Guests see built-ins
// only, which is the llm service's existing behaviour, not a Library rule.
async function loadLenses(signal) {
  try {
    const data = await getJSON(`${BASE}/api/personas`, signal);
    const personas = data.personas || [];
    return {
      items: personas.map((p) => ({
        key: `lens:${p.id}`,
        type: "lens",
        id: p.id,
        ticker: "",
        title: p.name,
        summary: firstLine(p.instructions, 200),
        at: unixOf(p.updatedAt || p.createdAt),
        sourceLabel: p.builtin ? "built-in" : "yours",
        attribution: p.builtin ? "Built-in analytical method" : "Custom Specialist lens you wrote",
        dataState: "",
        modelUsed: "",
        inference: false,
        stub: false,
        lensId: p.id,
        builtin: !!p.builtin,
        scope: p.builtin ? "global" : "user",
        subtype: p.builtin ? "built-in" : "custom",
        thesisIds: [],
        raw: p,
        searchText: haystack(p.name, p.instructions, p.id),
      })),
      status: SOURCE_STATUS.ok,
    };
  } catch (e) {
    if (e.name === "AbortError") throw e;
    return { items: [], status: SOURCE_STATUS.unavailable, error: `lens list unreachable (${e.message})` };
  }
}

// sweepTickers runs a per-ticker loader across the universe. One company failing degrades coverage for
// that company only, and the failure is reported rather than folded into an empty result.
async function sweepTickers(tickers, signal, loader) {
  const syms = (tickers || []).filter(Boolean);
  if (syms.length === 0) return { items: [], status: SOURCE_STATUS.ok };
  const settled = await Promise.all(
    syms.map(async (sym) => {
      try {
        return { sym, items: await loader(sym) };
      } catch (e) {
        if (e.name === "AbortError") throw e;
        return { sym, items: [], failed: true };
      }
    })
  );
  const failed = settled.filter((r) => r.failed).map((r) => r.sym);
  return {
    items: settled.flatMap((r) => r.items),
    status: failed.length === syms.length ? SOURCE_STATUS.unavailable : SOURCE_STATUS.ok,
    error: failed.length ? `could not read: ${failed.join(", ")}` : "",
  };
}

// ---- the sweep ---------------------------------------------------------------------------------

// loadLibrary reads every source concurrently and returns rows plus a per-source status map. It
// resolves even when several sources fail — a partly-readable Library is useful, an all-or-nothing one
// is not — and the status map is what lets the UI say which part is missing.
export async function loadLibrary({ signal, activeTicker } = {}) {
  let tickers = [];
  let tickerStatus = SOURCE_STATUS.ok;
  try {
    tickers = (await fetchTickers()).map((t) => t.symbol).filter(Boolean);
  } catch {
    tickerStatus = SOURCE_STATUS.unavailable;
  }
  if (activeTicker && !tickers.includes(activeTicker)) tickers.push(activeTicker);

  const [evidence, reviews, transcripts, reads, committee, lenses] = await Promise.all([
    loadEvidence(signal),
    loadReviews(signal),
    loadTranscripts(tickers, signal),
    loadReads(tickers, signal),
    loadCommittee(tickers, signal),
    loadLenses(signal),
  ]);

  const sources = { evidence, review: reviews, transcript: transcripts, read: reads, committee, lens: lenses };
  const items = Object.values(sources).flatMap((s) => s.items);
  items.sort((a, b) => b.at - a.at || a.key.localeCompare(b.key));

  return {
    items,
    sources,
    tickers,
    // A universe we could not read means the per-ticker sweeps ran over nothing, which is a coverage
    // gap the UI must disclose rather than render as "no saved research".
    universeUnavailable: tickerStatus === SOURCE_STATUS.unavailable,
  };
}

// ---- search, filter, sort ----------------------------------------------------------------------

export const SORTS = [
  { id: "recent", label: "Most recent" },
  { id: "relevance", label: "Best match" },
];

// scoreMatch ranks a row against a query. Deliberately simple and deterministic: a title hit beats a
// body hit, an earlier hit beats a later one. No stemming, no fuzzy matching — an honest substring
// search the user can predict beats a clever one they cannot.
function scoreMatch(item, q) {
  const title = item.title.toLowerCase();
  const ti = title.indexOf(q);
  if (ti === 0) return 1000;
  if (ti > 0) return 800 - Math.min(ti, 200);
  const bi = item.searchText.indexOf(q);
  if (bi >= 0) return 400 - Math.min(Math.floor(bi / 10), 200);
  return -1;
}

export function matchesQuery(item, query) {
  const q = query.trim().toLowerCase();
  if (!q) return true;
  return item.title.toLowerCase().includes(q) || item.searchText.includes(q);
}

export const DEFAULT_FILTERS = {
  query: "",
  types: [], // empty = all
  ticker: "",
  source: "",
  lensId: "",
  thesisId: "",
  linkState: "", // "" | linked | unlinked
  from: "", // YYYY-MM-DD
  to: "",
  sort: "recent",
};

// applyFilters is pure over an already-bounded list. Client-side because the list is small; the moment
// one store grows past this, the fix is a filter parameter on THAT store's endpoint.
export function applyFilters(items, filters) {
  const f = { ...DEFAULT_FILTERS, ...filters };
  const q = f.query.trim().toLowerCase();
  const fromTs = f.from ? unixOf(`${f.from}T00:00:00Z`) : 0;
  const toTs = f.to ? unixOf(`${f.to}T23:59:59Z`) : 0;

  const out = items.filter((it) => {
    if (f.types.length && !f.types.includes(it.type)) return false;
    if (f.ticker && it.ticker !== f.ticker) return false;
    if (f.source && it.sourceLabel !== f.source) return false;
    if (f.lensId && it.lensId !== f.lensId) return false;
    if (f.thesisId && !(it.thesisIds || []).includes(f.thesisId)) return false;
    if (f.linkState === "linked" && !(it.thesisIds || []).length) return false;
    if (f.linkState === "unlinked" && (it.thesisIds || []).length) return false;
    // A row with an unknown date is excluded by an explicit date filter rather than assumed to be in
    // range — guessing would put undated items wherever the user last looked.
    if (fromTs && (!it.at || it.at < fromTs)) return false;
    if (toTs && (!it.at || it.at > toTs)) return false;
    if (q && !matchesQuery(it, q)) return false;
    return true;
  });

  if (f.sort === "relevance" && q) {
    return out
      .map((it) => ({ it, s: scoreMatch(it, q) }))
      .sort((a, b) => b.s - a.s || b.it.at - a.it.at || a.it.key.localeCompare(b.it.key))
      .map((x) => x.it);
  }
  return out.sort((a, b) => b.at - a.at || a.key.localeCompare(b.key));
}

// facetsOf derives the filter options from the rows actually present, so the UI never offers a filter
// that can only return nothing.
export function facetsOf(items) {
  const tickers = new Set();
  const sources = new Set();
  const lenses = new Map();
  const types = new Set();
  items.forEach((it) => {
    if (it.ticker) tickers.add(it.ticker);
    if (it.sourceLabel) sources.add(it.sourceLabel);
    if (it.type === "lens") lenses.set(it.lensId, it.title);
    else if (it.lensId) lenses.set(it.lensId, it.sourceLabel || it.lensId);
    types.add(it.type);
  });
  return {
    tickers: [...tickers].sort(),
    sources: [...sources].sort(),
    lenses: [...lenses]
      .map(([id, name]) => ({ id, name }))
      .sort((a, b) => a.name.localeCompare(b.name)),
    types: [...types],
  };
}

// countsByType is what the empty states key off: "no evidence yet" and "the evidence store is
// unreachable" are different sentences and must not share one.
export function countsByType(items) {
  const out = {};
  OBJECT_TYPES.forEach((t) => {
    out[t.id] = 0;
  });
  items.forEach((it) => {
    out[it.type] = (out[it.type] || 0) + 1;
  });
  return out;
}

// ---- display helpers ---------------------------------------------------------------------------

export function fmtWhen(unix) {
  if (!unix) return "date unknown";
  return new Date(unix * 1000).toLocaleDateString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
  });
}

export function fmtWhenExact(unix) {
  if (!unix) return "date unknown";
  return new Date(unix * 1000).toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

export function ageLabel(unix) {
  if (!unix) return "age unknown";
  const s = Math.max(0, Math.floor(Date.now() / 1000 - unix));
  if (s < 3600) return `${Math.max(1, Math.floor(s / 60))}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  const d = Math.floor(s / 86400);
  if (d < 60) return `${d}d ago`;
  return `${Math.floor(d / 30)}mo ago`;
}
