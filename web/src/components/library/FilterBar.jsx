import { Badge, Button, controlClass } from "../ui/index.js";
import { cx } from "../../lib/cx.js";
import { DEFAULT_FILTERS, OBJECT_TYPES, SORTS } from "../../lib/libraryApi.js";

// FilterBar — search and narrowing for the Library list.
//
// Two levels on purpose. The search box and the type chips are the two moves people actually make, so
// they are always visible and carry live counts. Everything else (company, source, lens, thesis link,
// date window) sits behind "More filters", because a wall of ten selects makes the common case slower.
//
// Every option here is derived from the rows that are actually loaded, so the UI can never offer a
// filter that only returns nothing. Counts come from the CURRENT result set, so a chip reading
// "Evidence 0" means "no evidence matches what you have typed", not "you have no evidence" — the
// distinction the empty state below picks up.

// SCOPE_HINT appears in each type chip's tooltip. Personal captures and company-level model artefacts
// sit in one list, so the list has to say which is which somewhere the user can reach at scan level.
const SCOPE_HINT = {
  user: "Stored in your account.",
  company: "Stored per company — shared by everyone using this instance, not personal to you.",
  mixed: "Built-ins are shared; Custom Specialists are yours.",
};

export default function FilterBar({ filters, onChange, facets, counts, resultCount, showMore, onShowMore }) {
  const set = (patch) => onChange({ ...filters, ...patch });
  const toggleType = (id) => {
    const next = filters.types.includes(id)
      ? filters.types.filter((t) => t !== id)
      : [...filters.types, id];
    set({ types: next });
  };

  const dirty =
    filters.query ||
    filters.types.length ||
    filters.ticker ||
    filters.source ||
    filters.lensId ||
    filters.thesisId ||
    filters.linkState ||
    filters.from ||
    filters.to;

  return (
    <div className="flex flex-col gap-2.5 border-b border-line px-[18px] py-3">
      <div className="flex flex-wrap items-center gap-2">
        <label className="min-w-[220px] flex-1">
          <span className="sr-only">Search saved research</span>
          <input
            type="search"
            className={controlClass}
            value={filters.query}
            onChange={(e) => set({ query: e.target.value })}
            placeholder="Search titles, notes, findings, quotations…"
          />
        </label>

        <label className="flex items-center gap-1.5">
          <span className="text-[11px] uppercase tracking-wide text-muted">Sort</span>
          <select
            className={cx(controlClass, "w-[132px]")}
            value={filters.sort}
            onChange={(e) => set({ sort: e.target.value })}
          >
            {SORTS.map((s) => (
              <option key={s.id} value={s.id} disabled={s.id === "relevance" && !filters.query.trim()}>
                {s.label}
              </option>
            ))}
          </select>
        </label>

        <Button variant="ghost" size="sm" onClick={onShowMore} aria-expanded={showMore}>
          {showMore ? "Fewer filters" : "More filters"}
        </Button>
        {dirty && (
          <Button variant="ghost" size="sm" onClick={() => onChange({ ...DEFAULT_FILTERS })}>
            Clear
          </Button>
        )}
      </div>

      <div className="flex flex-wrap items-center gap-1.5">
        <TypeChip
          label="All types"
          count={resultCount}
          active={filters.types.length === 0}
          onClick={() => set({ types: [] })}
        />
        {OBJECT_TYPES.map((t) => (
          <TypeChip
            key={t.id}
            label={t.label}
            title={`${t.blurb}${SCOPE_HINT[t.scope] ? ` ${SCOPE_HINT[t.scope]}` : ""}`}
            count={counts[t.id] || 0}
            active={filters.types.includes(t.id)}
            onClick={() => toggleType(t.id)}
          />
        ))}
      </div>

      {showMore && (
        <div className="grid grid-cols-1 gap-2 border-t border-line pt-2.5 sm:grid-cols-2 lg:grid-cols-3">
          <Select
            label="Company"
            value={filters.ticker}
            onChange={(v) => set({ ticker: v })}
            options={[{ value: "", label: "Any company" }, ...facets.tickers.map((t) => ({ value: t, label: t }))]}
          />
          <Select
            label="Source"
            value={filters.source}
            onChange={(v) => set({ source: v })}
            options={[{ value: "", label: "Any source" }, ...facets.sources.map((s) => ({ value: s, label: s }))]}
          />
          <Select
            label="Lens"
            value={filters.lensId}
            onChange={(v) => set({ lensId: v })}
            options={[{ value: "", label: "Any lens" }, ...facets.lenses.map((l) => ({ value: l.id, label: l.name }))]}
          />
          <Select
            label="Thesis link"
            value={filters.linkState}
            onChange={(v) => set({ linkState: v })}
            options={[
              { value: "", label: "Linked or not" },
              { value: "linked", label: "Linked to a thesis" },
              { value: "unlinked", label: "Not linked" },
            ]}
          />
          <label className="flex min-w-0 flex-col gap-1">
            <span className="text-[11px] font-medium uppercase tracking-wide text-muted">From</span>
            <input
              type="date"
              className={controlClass}
              value={filters.from}
              onChange={(e) => set({ from: e.target.value })}
            />
          </label>
          <label className="flex min-w-0 flex-col gap-1">
            <span className="text-[11px] font-medium uppercase tracking-wide text-muted">To</span>
            <input
              type="date"
              className={controlClass}
              value={filters.to}
              onChange={(e) => set({ to: e.target.value })}
            />
          </label>
          {(filters.from || filters.to) && (
            <p className="text-[11px] leading-relaxed text-muted sm:col-span-2 lg:col-span-3">
              A date filter excludes objects whose date is unknown, rather than guessing where they belong.
            </p>
          )}
        </div>
      )}
    </div>
  );
}

function TypeChip({ label, title, count, active, onClick }) {
  return (
    <button
      type="button"
      onClick={onClick}
      title={title}
      aria-pressed={active}
      className={cx(
        "inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-[11.5px] font-medium transition-colors",
        "focus:outline-none focus-visible:ring-1 focus-visible:ring-accent",
        active ? "border-accent bg-accent/10 text-fg" : "border-line text-muted hover:border-line2 hover:text-fg/90"
      )}
    >
      {label}
      <Badge variant={active ? "accent" : "outline"}>{count}</Badge>
    </button>
  );
}

function Select({ label, value, onChange, options }) {
  return (
    <label className="flex min-w-0 flex-col gap-1">
      <span className="text-[11px] font-medium uppercase tracking-wide text-muted">{label}</span>
      <select className={controlClass} value={value} onChange={(e) => onChange(e.target.value)}>
        {options.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
    </label>
  );
}
