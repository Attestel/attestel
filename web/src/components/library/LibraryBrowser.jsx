import { useCallback, useEffect, useMemo, useState } from "react";
import { Badge, Button, EmptyState, SkeletonText } from "../ui/index.js";
import { listTheses } from "../../lib/api.js";
import {
  DEFAULT_FILTERS,
  LIMITS,
  OBJECT_TYPES,
  SOURCE_STATUS,
  applyFilters,
  countsByType,
  facetsOf,
  loadLibrary,
} from "../../lib/libraryApi.js";
import FilterBar from "./FilterBar.jsx";
import ResultRow from "./ResultRow.jsx";
import ItemDetail from "./ItemDetail.jsx";

// LibraryBrowser — retrieval across every durable research object the user already owns.
//
// The whole surface is one list with one search box. That is the design: Library's job is to FIND a
// thing you saved, not to present another company dashboard. Six equally-weighted panels, one per
// object type, would be a second Research workspace with worse data.
//
// Everything is loaded once per visit and filtered in the browser. Concretely: up to ~400 captures
// plus a dozen transcripts, ten reads and ten committee snapshots per company, plus your lenses. That
// is a few hundred rows — small enough that a search index would cost more than it saves, and the
// prompt forbids adding one. When a store outgrows this, its own endpoint gains the filter.
//
// Coverage is stated, never implied. If a store cannot be read, the list says which one and that the
// results are incomplete; if a contract has not shipped, it says that instead of showing zero.

// EMPTY_BY_TYPE gives each object type its own honest first-run copy and a route back to the place
// that creates it. A single generic "nothing found" would leave a new user with no next step.
const EMPTY_BY_TYPE = {
  evidence: {
    title: "No saved evidence yet",
    body: "Evidence is what you capture while researching a company — a computed fact, a filing, a headline, a quotation, or your own note.",
    cta: "Go to Research → Evidence",
    hash: "#research/evidence",
  },
  review: {
    title: "No saved lens reviews yet",
    body: "A lens review is the saved result of applying a research method to a thesis or an evidence item.",
    cta: "Go to Research → Perspectives",
    hash: "#research/perspectives",
  },
  committee: {
    title: "No perspective reviews stored yet",
    body: "Running Review with perspectives on a company stores the analyst run and its deterministic consensus.",
    cta: "Go to Research → Perspectives",
    hash: "#research/perspectives",
  },
  transcript: {
    title: "No transcript analyses stored yet",
    body: "Paste an earnings call into Research → Evidence → Transcripts to analyse it. The analysis is stored; the call text never is.",
    cta: "Go to Research → Transcripts",
    hash: "#research/evidence",
  },
  read: {
    title: "No daily reads stored yet",
    body: "A read is stored once per company per day when the company analysis is opened.",
    cta: "Go to Research",
    hash: "#research/investigations",
  },
  lens: {
    title: "No lenses available",
    body: "The built-in lenses should always be here. If this is empty, the lens service could not be reached.",
    cta: "Go to Research",
    hash: "#research/investigations",
  },
};

export default function LibraryBrowser({ initialTypes = [], activeTicker, onOpenResearch, onApplyLens }) {
  const [state, setState] = useState({ status: "loading", items: [], sources: {}, universeUnavailable: false });
  const [thesisById, setThesisById] = useState({});
  const [filters, setFilters] = useState({ ...DEFAULT_FILTERS, types: initialTypes });
  const [showMore, setShowMore] = useState(false);
  const [open, setOpen] = useState(null);
  const [reloadNonce, setReloadNonce] = useState(0);

  // A subview change re-seeds the type filter, so arriving at "Saved transcripts" lands on transcripts
  // without locking the user out of the rest of the library.
  useEffect(() => {
    setFilters((f) => ({ ...f, types: initialTypes }));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [initialTypes.join(",")]);

  useEffect(() => {
    const ctrl = new AbortController();
    let alive = true;
    setState((s) => ({ ...s, status: s.items.length ? "reloading" : "loading" }));
    loadLibrary({ signal: ctrl.signal, activeTicker })
      .then((res) => alive && setState({ status: "ready", ...res }))
      .catch((e) => {
        if (!alive || e.name === "AbortError") return;
        setState({ status: "error", items: [], sources: {}, error: e.message });
      });
    return () => {
      alive = false;
      ctrl.abort();
    };
  }, [reloadNonce, activeTicker]);

  // Thesis titles for the "linked theses" list. Best-effort: without them a link still renders, just
  // with its id instead of the claim.
  useEffect(() => {
    let alive = true;
    listTheses()
      .then((list) => {
        if (!alive) return;
        setThesisById(Object.fromEntries((list || []).map((t) => [t.id, t])));
      })
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, [reloadNonce]);

  const reload = useCallback(() => setReloadNonce((n) => n + 1), []);

  const facets = useMemo(() => facetsOf(state.items), [state.items]);
  const results = useMemo(() => applyFilters(state.items, filters), [state.items, filters]);
  // Chip counts reflect the CURRENT query and non-type filters, so a chip reading 0 means "nothing of
  // this type matches what you typed", which is what the user is asking when they look at it.
  const counts = useMemo(
    () => countsByType(applyFilters(state.items, { ...filters, types: [] })),
    [state.items, filters]
  );
  const totalByType = useMemo(() => countsByType(state.items), [state.items]);

  const problems = useMemo(() => coverageProblems(state), [state]);
  const truncated = !!state.sources?.evidence?.truncated;

  if (state.status === "loading") {
    return (
      <Shell>
        <div className="px-[18px] py-5">
          <SkeletonText lines={7} />
        </div>
      </Shell>
    );
  }

  if (state.status === "error") {
    return (
      <Shell>
        <div className="px-[18px] py-5">
          <EmptyState
            title="Library could not be loaded"
            action={
              <Button variant="secondary" size="sm" onClick={reload}>
                Try again
              </Button>
            }
          >
            {state.error}
          </EmptyState>
        </div>
      </Shell>
    );
  }

  return (
    <Shell right={<Button variant="ghost" size="xs" onClick={reload}>Refresh</Button>}>
      <FilterBar
        filters={filters}
        onChange={setFilters}
        facets={facets}
        counts={counts}
        resultCount={applyFilters(state.items, { ...filters, types: [] }).length}
        showMore={showMore}
        onShowMore={() => setShowMore((v) => !v)}
      />

      <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5 border-b border-line px-[18px] py-2">
        <span className="text-[11.5px] text-muted">
          {results.length} of {state.items.length} saved object{state.items.length === 1 ? "" : "s"}
        </span>
        {state.status === "reloading" && <Badge variant="outline">refreshing…</Badge>}
        {truncated && (
          <span className="text-[11px] text-warn">
            Showing your most recent {LIMITS.evidencePerPage * LIMITS.evidencePages} captures — older ones
            are not in this list.
          </span>
        )}
      </div>

      {problems.length > 0 && (
        <ul className="border-b border-line bg-panel2/30 px-[18px] py-2">
          {problems.map((p) => (
            <li key={p} className="text-[11px] leading-relaxed text-warn">
              {p}
            </li>
          ))}
        </ul>
      )}

      {results.length === 0 ? (
        <NoResults filters={filters} totalByType={totalByType} sources={state.sources} onClear={() => setFilters({ ...DEFAULT_FILTERS })} />
      ) : (
        <ul className="divide-y divide-line">
          {results.map((item) => (
            <li key={item.key}>
              <ResultRow item={item} selected={open?.key === item.key} onOpen={setOpen} />
            </li>
          ))}
        </ul>
      )}

      <ItemDetail
        item={open}
        thesisById={thesisById}
        onClose={() => setOpen(null)}
        onChanged={reload}
        onOpenResearch={onOpenResearch}
        onApplyLens={onApplyLens}
      />
    </Shell>
  );
}

function Shell({ right, children }) {
  return (
    <section className="overflow-hidden surface-card">
      <div className="flex flex-wrap items-center gap-x-2.5 gap-y-2 border-b border-line px-[22px] py-3">
        <span className="text-[14px] font-semibold text-fg">Saved research</span>
        <span className="text-[11.5px] text-muted">
          Everything you have kept, in one searchable list. Nothing here is a copy — each row opens the
          record where it actually lives.
        </span>
        {right && <span className="ml-auto shrink-0">{right}</span>}
      </div>
      {children}
    </section>
  );
}

// coverageProblems turns per-source status into sentences. Each names the store and what it means for
// the result set, because "0 results" from a working store and from a dead one are different facts.
function coverageProblems(state) {
  const out = [];
  if (state.universeUnavailable) {
    out.push("The company list could not be read, so transcripts, daily reads and perspective reviews were not searched.");
  }
  OBJECT_TYPES.forEach((t) => {
    const s = state.sources?.[t.id];
    if (!s) return;
    if (s.status === SOURCE_STATUS.unavailable) {
      out.push(`${t.label} could not be read${s.error ? ` (${s.error})` : ""} — these results are incomplete.`);
    }
    if (s.status === SOURCE_STATUS.pending) {
      out.push(`${t.label} are not stored yet — the saved-review contract ships with the lens work, and Library reads it at its final path.`);
    }
    if (s.warning) out.push(s.warning);
  });
  return out;
}

// NoResults distinguishes three cases that must never share one message: a filter that matched
// nothing, a store that is genuinely empty for a chosen type, and a whole library with nothing in it.
function NoResults({ filters, totalByType, sources, onClear }) {
  const singleType = filters.types.length === 1 ? filters.types[0] : null;
  const filtering =
    filters.query || filters.ticker || filters.source || filters.lensId || filters.linkState || filters.from || filters.to;

  if (singleType && !filtering && (totalByType[singleType] || 0) === 0) {
    const meta = EMPTY_BY_TYPE[singleType];
    const pending = sources?.[singleType]?.status === SOURCE_STATUS.pending;
    return (
      <div className="px-[18px] py-6">
        <EmptyState
          title={pending ? `${meta.title.replace(/^No /, "No ")} — not stored yet` : meta.title}
          action={
            !pending && (
              <Button variant="secondary" size="sm" onClick={() => { window.location.hash = meta.hash; }}>
                {meta.cta}
              </Button>
            )
          }
        >
          {pending
            ? "This object type has no store yet. Nothing is missing from your account — there is simply nothing to retrieve until that work ships."
            : meta.body}
        </EmptyState>
      </div>
    );
  }

  const totalAll = Object.values(totalByType).reduce((a, b) => a + b, 0);
  if (totalAll === 0) {
    return (
      <div className="px-[18px] py-6">
        <EmptyState
          title="Nothing saved yet"
          action={
            <Button variant="secondary" size="sm" onClick={() => { window.location.hash = "#research/evidence"; }}>
              Go to Research → Evidence
            </Button>
          }
        >
          Library fills up as you work: capture a fact or a filing against a company, analyse a
          transcript, or run a review. It never invents entries of its own.
        </EmptyState>
      </div>
    );
  }

  return (
    <div className="px-[18px] py-6">
      <EmptyState
        title="Nothing matches these filters"
        action={
          <Button variant="secondary" size="sm" onClick={onClear}>
            Clear filters
          </Button>
        }
      >
        You have {totalAll} saved object{totalAll === 1 ? "" : "s"} — none of them match the current
        search and filters.
      </EmptyState>
    </div>
  );
}

export { EMPTY_BY_TYPE };
