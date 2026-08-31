import { cx } from "../../lib/cx.js";
import { Icon } from "./icons.jsx";
import { navForLevel } from "../../lib/routes.js";

// LeftRail — persistent navigation over the NINE primary destinations of contract §9.34:
// Today · Following · Explore · Calendar · Research · Watchlist · Journal · Library · Settings.
// The destination table itself lives in lib/routes.js; this component only renders it.
//
// Icon-only on narrow screens, icon+label on lg, sticky under the top bar.
//
// THE RAIL IS THE SAME NINE AT BOTH EXPERIENCE LEVELS. D-28 is DECIDED (b) — density gating, not
// destination hiding — so `navForLevel` no longer removes anything from this list; the level varies
// what is dense INSIDE a destination (Research's specialist subviews stay `standard` and
// `hiddenInNav`). §9.34 pairs the nine-destination count with a reachability requirement: every
// string in docs/DISCLOSURE_CLASSIFICATION.md must be reachable from this rail, and two of them
// render behind Library, which is why Library is no longer `standard`. View-gating unlocks no
// trading action at any level (invariant #2).
//
// The unread notification count belongs to the TopBar bell, which stays deliberately OUTSIDE the seven
// destinations — the rail no longer carries a badge.
export function LeftRail({ view, onChange, level = "beginner" }) {
  return (
    <nav
      aria-label="Destinations"
      className="rail-h sticky top-[57px] z-30 flex w-14 shrink-0 flex-col gap-1 border-r border-line bg-panel/60 p-2 lg:w-52"
    >
      {navForLevel(level).map((item) => {
        const active = view === item.id;
        return (
          <button
            key={item.id}
            onClick={() => onChange(item.id)}
            aria-current={active ? "page" : undefined}
            title={item.stage ? `${item.label} — ${item.stage}` : item.label}
            className={cx(
              // min-h-[44px] gives a comfortable touch target on the phone/tablet icon rail; reverts to
              // the natural (compact) height on the lg+ desktop rail so that layout is unchanged.
              // Full-round pill, indigo when active — the landing's nav-link treatment. Indigo is
              // the BRAND/active colour here; it never marks a price or a direction.
              "group relative flex min-h-[44px] items-center gap-3 rounded-full px-3.5 py-2.5 text-[12.5px] font-medium transition-colors duration-200 ease-premium lg:min-h-0",
              active ? "bg-accent/12 text-accent" : "text-muted hover:bg-white/10 hover:text-fg"
            )}
          >
            <span className="relative shrink-0">
              <Icon name={item.icon} size={19} />
            </span>
            <span className="hidden truncate lg:block">{item.label}</span>
          </button>
        );
      })}
    </nav>
  );
}
