import { Label, Tabs } from "../ui/index.js";
import { navSubviewsForLevel } from "../../lib/routes.js";

// SubNav — the secondary strip inside a destination that has subviews (Research, Watchlist, Journal,
// Library, Settings). Entirely data-driven from lib/routes.js and filtered by experience level, so a
// destination gains or loses a tab by editing the route table, never by editing JSX.
//
// Renders nothing when fewer than two subviews are visible at this level — a lone tab is chrome, not
// navigation.
export function SubNav({ view, subview, onChange, level, title, hint }) {
  const subs = navSubviewsForLevel(view, level);
  if (subs.length < 2) return null;

  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
      {title && (
        <Label>{title}</Label>
      )}
      <Tabs
        items={subs.map((s) => ({ value: s.id, label: s.label, title: s.group ? `${s.group} · ${s.label}` : s.label }))}
        value={subview}
        onChange={onChange}
        size="sm"
        ariaLabel={`${view} sections`}
      />
      {hint && <span className="text-[11.5px] text-muted">{hint}</span>}
    </div>
  );
}
