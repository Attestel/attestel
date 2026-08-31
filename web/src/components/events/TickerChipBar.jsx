import { cx } from "../../lib/cx.js";

// TickerChipBar — MULTI-SELECT, not a radio (doc §16.2).
//
// Selecting NVDA and AMD narrows the feed to events touching EITHER, and it does so without
// creating a watchlist and WITHOUT switching the app's active company. Those are three different
// states and the product has been careful to keep them apart: following is a subscription, the
// active company is what Research has open, and this is a filter over what is already on screen.
//
// `All` is its own state, not "everything selected". Deselecting the last chip returns to All
// rather than to an empty feed, because an empty feed here would be an artefact of the control
// rather than an answer about the market.

function Chip({ label, selected, onClick, title }) {
  return (
    <button
      type="button"
      aria-pressed={selected}
      title={title}
      onClick={onClick}
      className={cx(
        // `full` radius per the design system. `accent` tint when selected — accent is brand and
        // state, which is what "selected" is; it is never a direction here.
        "flex-none rounded-full border px-3 py-[5px] font-mono text-[10px] uppercase tracking-[.12em] transition-colors duration-200 ease-premium",
        selected
          ? "border-accent/50 bg-accent/15 text-fg"
          : "border-line text-muted hover:border-line2 hover:text-fg"
      )}
    >
      {label}
    </button>
  );
}

export default function TickerChipBar({
  tickers = [],
  selected = [],
  onChange,
  actions,
  className,
}) {
  const allSelected = selected.length === 0;
  const toggle = (t) => {
    const next = selected.includes(t) ? selected.filter((x) => x !== t) : [...selected, t];
    onChange?.(next);
  };
  return (
    <div className={cx("flex items-center gap-2", className)}>
      {/* Scrolls horizontally at narrow widths rather than wrapping into a wall of chips. */}
      <div
        className="flex min-w-0 flex-1 items-center gap-1.5 overflow-x-auto pb-1 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"
        role="group"
        aria-label="Filter by company"
      >
        <Chip
          label="All"
          selected={allSelected}
          onClick={() => onChange?.([])}
          title="All followed companies"
        />
        {tickers.map((t) => (
          <Chip key={t} label={t} selected={selected.includes(t)} onClick={() => toggle(t)} />
        ))}
      </div>
      {actions && <div className="flex flex-none items-center gap-2">{actions}</div>}
    </div>
  );
}
