import { useState } from "react";
import { Badge, Button, ProvenanceBadge } from "../ui/index.js";
import { cx } from "../../lib/cx.js";
import { DATA_STATE_LABELS } from "../../lib/evidenceApi.js";
import SaveEvidenceDialog from "./SaveEvidenceDialog.jsx";

// CaptureStrip — the contextual "Save to thesis" affordance beside the objects a section is already
// showing.
//
// It is a strip of the CURRENT savable objects rather than a button hidden inside each panel, for one
// concrete reason: each row states the source and the live/seed/synthetic state of that specific
// object BEFORE the user commits. A bare icon on a chart cannot do that, and a save flow whose first
// honest disclosure arrives after the click is the wrong way round.
//
// Every row hands the dialog a SELECTOR, never a finished record. The strip does not know or claim the
// provenance that will be stored — it only names which object the server should resolve.

export default function CaptureStrip({
  ticker,
  timeframe = "1D",
  items = [],
  title = "Save as evidence",
  hint,
  emptyNote,
  onSaved,
  className,
}) {
  const [active, setActive] = useState(null);

  return (
    <section className={cx("rounded-lg border border-line bg-panel2/30", className)}>
      <header className="flex flex-wrap items-center gap-x-2.5 gap-y-1.5 border-b border-line px-3.5 py-2.5">
        <span className="text-[11px] font-semibold uppercase tracking-[0.08em] text-fg/70">{title}</span>
        <Badge variant="outline">links to a thesis</Badge>
        {hint && <span className="w-full text-[11px] leading-relaxed text-muted sm:w-auto sm:flex-1">{hint}</span>}
      </header>

      {items.length === 0 ? (
        <p className="px-3.5 py-3 text-[12px] leading-relaxed text-muted">
          {emptyNote || "Nothing here can be captured with a source yet."}
        </p>
      ) : (
        <ul className="divide-y divide-line">
          {items.map((item) => (
            <li key={item.key} className="flex flex-wrap items-center gap-x-3 gap-y-2 px-3.5 py-2.5">
              <div className="min-w-0 flex-1">
                <p className="truncate text-[12.5px] text-fg" title={item.title}>
                  {item.title}
                </p>
                <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-1">
                  {item.dataState && (
                    <ProvenanceBadge
                      kind={item.dataState}
                      label={DATA_STATE_LABELS[item.dataState] || item.dataState}
                    />
                  )}
                  {item.subtitle && (
                    <span className="truncate text-[11px] text-muted" title={item.subtitle}>
                      {item.subtitle}
                    </span>
                  )}
                </div>
              </div>
              <Button variant="secondary" size="xs" onClick={() => setActive(item)}>
                Save to thesis
              </Button>
            </li>
          ))}
        </ul>
      )}

      <SaveEvidenceDialog
        open={!!active}
        item={active}
        ticker={ticker}
        timeframe={timeframe}
        onClose={() => setActive(null)}
        onSaved={onSaved}
      />
    </section>
  );
}
