import { cx } from "../../../lib/cx.js";
import {
  HORIZONS,
  STATUS_META,
  fmtWhen,
  isUnstructured,
  nearestConfidenceStep,
} from "../../../lib/thesisApi.js";

// ThesisList — the selection rail. Multiple theses per ticker are legitimate (D-03: up to 3 active),
// so this is a real list with a real selected item, not a single-record form.
//
// Each row carries: a concise claim, horizon, the user's confidence, when it last changed, and the
// latest MODEL review state. Lifecycle and review state are each rendered as glyph + word, so nothing
// depends on colour alone.

const horizonLabel = (h) => HORIZONS.find((x) => x.value === h)?.label || null;

const REVIEW = {
  confirming: { glyph: "▲", label: "Confirming", tone: "text-up" },
  neutral: { glyph: "•", label: "Neutral", tone: "text-muted" },
  challenged: { glyph: "▼", label: "Challenged", tone: "text-down" },
};

// GROUPS orders the rail so the user's live beliefs come first and the record stays reachable below.
const GROUPS = [
  { key: "active", label: "Active", match: (t) => t.status === "active" },
  { key: "draft", label: "Drafts", match: (t) => t.status === "draft" },
  { key: "closed", label: "Closed", match: (t) => t.status === "closed" },
  { key: "archived", label: "Archived", match: (t) => t.status === "archived" },
];

function Row({ thesis, selected, dirty, onSelect }) {
  const meta = STATUS_META[thesis.status] || STATUS_META.draft;
  const claim = thesis.claim || thesis.text || "";
  const conf = nearestConfidenceStep(thesis.confidence);
  const review = thesis.lastCheck ? REVIEW[thesis.lastCheck.verdict] : null;
  const hz = horizonLabel(thesis.horizon);

  return (
    <li>
      <button
        type="button"
        onClick={onSelect}
        aria-current={selected ? "true" : undefined}
        className={cx(
          "flex w-full flex-col gap-1 border-l-2 px-3 py-2.5 text-left transition-colors",
          selected
            ? "border-l-accent bg-accent/[0.07]"
            : "border-l-transparent hover:border-l-line2 hover:bg-panel2/60"
        )}
      >
        <div className="flex items-center gap-1.5">
          <span className={cx("text-[10px]", meta.tone)} aria-hidden="true">
            {meta.glyph}
          </span>
          <span className={cx("text-[10px] font-bold uppercase tracking-wide", meta.tone)}>
            {meta.label}
          </span>
          {isUnstructured(thesis) && (
            <span className="rounded border border-line px-1 py-px font-mono text-[9px] uppercase tracking-wide text-muted/70">
              text only
            </span>
          )}
          {dirty && (
            <span className="ml-auto rounded bg-warn/15 px-1.5 py-px text-[9.5px] font-bold uppercase tracking-wide text-warn">
              unsaved
            </span>
          )}
        </div>

        {/* break-words: a long unbroken token in a user's claim must not widen the rail at 360px. */}
        <p className={cx("line-clamp-2 break-words text-[12.5px] leading-snug", selected ? "text-fg" : "text-fg/80")}>
          {claim || <span className="italic text-muted/60">No claim written</span>}
        </p>

        <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[10.5px] text-muted/70">
          {hz && <span>{hz}</span>}
          {conf && (
            <span>
              Conviction: <span className="text-muted">{conf.label}</span>
            </span>
          )}
          <span>Updated {fmtWhen(thesis.updatedAt)}</span>
          {review && (
            <span className={review.tone}>
              <span aria-hidden="true">{review.glyph}</span> {review.label}
            </span>
          )}
          {!review && <span className="text-muted/50">No model review</span>}
        </div>
      </button>
    </li>
  );
}

export default function ThesisList({ theses, selectedId, dirtyId, onSelect, onNew, canCreate }) {
  const groups = GROUPS.map((g) => ({ ...g, items: theses.filter(g.match) })).filter(
    (g) => g.items.length > 0
  );
  const activeCount = theses.filter((t) => t.status === "active").length;

  return (
    <div className="flex min-w-0 flex-col">
      <div className="flex items-center gap-2 border-b border-line px-3 py-2">
        <span className="text-[11px] font-semibold uppercase tracking-wide text-muted">
          Theses
          <span className="ml-1.5 font-mono normal-case tracking-normal text-muted/50">
            {theses.length}
          </span>
        </span>
        <button
          type="button"
          onClick={onNew}
          disabled={!canCreate}
          className="ml-auto rounded-full border border-line bg-panel2 px-2 py-1 text-[11px] font-semibold text-fg transition-colors hover:border-accent/60 hover:text-accent disabled:opacity-40"
        >
          + New
        </button>
      </div>

      {/* The cap is a stated rule, not a surprise 409. Shown before the user hits it. */}
      {activeCount >= 3 && (
        <p className="border-b border-line bg-warn/[0.06] px-3 py-1.5 text-[10.5px] leading-relaxed text-warn">
          3 of 3 active theses used for this ticker. Close or archive one to activate another — drafts
          are unlimited.
        </p>
      )}

      <div className="max-h-[320px] overflow-y-auto lg:max-h-[calc(100vh-320px)]">
        {groups.map((g) => (
          <div key={g.key}>
            <p className="sticky top-0 bg-panel px-3 py-1 label-mono text-muted">
              {g.label} · {g.items.length}
            </p>
            <ul className="divide-y divide-line/60">
              {g.items.map((t) => (
                <Row
                  key={t.id}
                  thesis={t}
                  selected={t.id === selectedId}
                  dirty={t.id === dirtyId}
                  onSelect={() => onSelect(t.id)}
                />
              ))}
            </ul>
          </div>
        ))}
      </div>
    </div>
  );
}
