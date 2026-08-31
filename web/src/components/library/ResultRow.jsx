import { Badge, ProvenanceBadge } from "../ui/index.js";
import { cx } from "../../lib/cx.js";
import { OBJECT_TYPE_LABELS, ageLabel, fmtWhen } from "../../lib/libraryApi.js";

// ResultRow — one saved object at scan level.
//
// Library is a retrieval surface, so the row is a LIST ROW, not a card: fixed height, one line of
// title, provenance and type readable without opening anything. A grid of equal cards would make
// twelve saved objects look like twelve dashboards and force the user to read all of them to find one.
//
// Four things must survive the scan, because they are what makes a result trustworthy at a glance:
// what kind of object it is, which company, when it is from, and where it came from. A row whose date
// is unknown says "date unknown" rather than borrowing today's.

const TYPE_TONE = {
  evidence: "accent",
  review: "info",
  committee: "info",
  transcript: "outline",
  read: "outline",
  lens: "default",
};

export default function ResultRow({ item, selected, onOpen }) {
  const linked = (item.thesisIds || []).length;

  return (
    <button
      type="button"
      onClick={() => onOpen(item)}
      aria-current={selected ? "true" : undefined}
      className={cx(
        "group flex w-full items-start gap-3 px-[18px] py-2.5 text-left transition-colors",
        "hover:bg-panel2/60 focus:outline-none focus-visible:bg-panel2/60 focus-visible:ring-1 focus-visible:ring-inset focus-visible:ring-accent",
        selected && "bg-panel2/70"
      )}
    >
      <span className="mt-[3px] w-[104px] shrink-0">
        <Badge variant={TYPE_TONE[item.type] || "outline"}>{OBJECT_TYPE_LABELS[item.type] || item.type}</Badge>
      </span>

      <span className="min-w-0 flex-1">
        <span className="flex flex-wrap items-baseline gap-x-2">
          {item.ticker && (
            <span className="font-mono text-[11px] font-semibold tracking-wide text-fg/70">{item.ticker}</span>
          )}
          <span className="min-w-0 flex-1 truncate text-[13px] font-medium text-fg" title={item.title}>
            {item.title}
          </span>
        </span>
        {item.summary && (
          <span className="mt-0.5 block truncate text-[11.5px] leading-relaxed text-muted" title={item.summary}>
            {item.summary}
          </span>
        )}
      </span>

      <span className="flex shrink-0 flex-col items-end gap-1">
        <span className="flex items-center gap-1.5">
          {item.dataState && <ProvenanceBadge kind={item.dataState} label={item.dataState} />}
          {item.stub && (
            <Badge variant="warn" title="Produced by the deterministic offline fallback, not the model.">
              stub
            </Badge>
          )}
          {item.inference && <Badge variant="info">model-derived</Badge>}
          {linked > 0 && (
            <Badge variant="outline" title={`Linked to ${linked} thesis${linked === 1 ? "" : "es"}`}>
              {linked === 1 ? "1 thesis" : `${linked} theses`}
            </Badge>
          )}
        </span>
        <span className="whitespace-nowrap text-[11px] text-muted/85" title={fmtWhen(item.at)}>
          {ageLabel(item.at)}
        </span>
      </span>
    </button>
  );
}
