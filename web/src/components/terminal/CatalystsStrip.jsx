import { useEffect, useState } from "react";
import { fetchCalendar } from "../../lib/api.js";
import { cx } from "../../lib/cx.js";

// CatalystsStrip — a compact one-line "next catalysts" row for the Terminal: the ticker's next
// earnings date + the next 1-2 high-importance macro events, each with a live|seed badge. DESCRIPTIVE
// dates only — never a trade trigger (invariant #2). Fail-silent: renders nothing if there's nothing
// to show (no earnings date, calendar endpoint down), so it never breaks the cockpit.

function fmtShort(s) {
  const [y, m, d] = String(s || "").split("-").map(Number);
  if (!y || !m || !d) return s;
  return new Date(y, m - 1, d).toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

// Prefer a parenthetical abbreviation ("Consumer Price Index (CPI)" -> "CPI") to keep the strip short.
function shortName(name) {
  const m = String(name || "").match(/\(([^)]+)\)/);
  return m ? m[1] : name;
}

function Dot({ source }) {
  const seed = !source || source === "seed";
  return (
    <span
      aria-hidden="true"
      title={seed ? "seed" : source.replace("live:", "")}
      className={cx("h-[5px] w-[5px] shrink-0 rounded-full", seed ? "bg-muted/50" : "bg-accent")}
    />
  );
}

export default function CatalystsStrip({ ticker = "NVDA", nextEarnings = null }) {
  const [macro, setMacro] = useState([]);

  useEffect(() => {
    let alive = true;
    fetchCalendar()
      .then((d) => {
        if (!alive) return;
        setMacro((d.events || []).filter(
          (e) => e.importance === "high" && !e.ticker && e.kind !== "earnings"
        ).slice(0, 2));
      })
      .catch(() => {}); // fail-silent
    return () => {
      alive = false;
    };
  }, []);

  const items = [];
  if (nextEarnings?.date) {
    items.push({ label: `${ticker} earnings`, date: nextEarnings.date, source: nextEarnings.source });
  }
  // Macro events carry their ET release time (from the calendar) — descriptive "when it prints".
  for (const m of macro) items.push({ label: shortName(m.name), date: m.date, time: m.timeET, source: m.source });

  if (items.length === 0) return null;

  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded-lg border border-line2 bg-panel2/40 px-3.5 py-2 text-[12px]">
      <span className="label-mono text-muted">Next catalysts</span>
      {items.map((it, i) => (
        <span key={`${it.label}-${it.date}-${i}`} className="inline-flex items-center gap-1.5">
          <Dot source={it.source} />
          <span className="num text-fg/70">{fmtShort(it.date)}</span>
          <span className="text-fg/90">{it.label}</span>
          {it.time && <span className="num text-muted/70">{it.time} ET</span>}
        </span>
      ))}
      <span className="ml-auto hidden label-mono text-muted sm:inline">
        not a trade trigger
      </span>
    </div>
  );
}
