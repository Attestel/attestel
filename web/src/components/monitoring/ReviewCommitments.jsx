import { useEffect, useMemo, useState } from "react";
import { listDueReviews, applyResearchLink } from "../../lib/monitoringApi.js";

// ReviewCommitments — the user's own scheduled research reviews, on the Calendar (Step 08).
//
// The economic calendar below it is a schedule of what the MARKET will do. This block is a schedule
// of what the USER said they would do. Keeping them visually separate but adjacent is the point: a
// review commitment is not a market event, and it must not be styled as one.
//
// Deterministic and fail-soft: due dates are computed by the alerts service from each rule's own
// fields. No model call. If the service is down the block renders nothing and the economic calendar
// is unaffected.
export default function ReviewCommitments({ withinDays = 90 }) {
  const [due, setDue] = useState([]);
  const [status, setStatus] = useState("loading");

  useEffect(() => {
    let alive = true;
    listDueReviews({ withinDays })
      .then((rows) => {
        if (!alive) return;
        setDue(rows);
        setStatus("ready");
      })
      .catch(() => alive && setStatus("down"));
    return () => {
      alive = false;
    };
  }, [withinDays]);

  const groups = useMemo(() => {
    const byDay = new Map();
    for (const row of due) {
      const day = new Date(row.dueAt * 1000).toISOString().slice(0, 10);
      if (!byDay.has(day)) byDay.set(day, []);
      byDay.get(day).push(row);
    }
    return [...byDay.entries()].sort(([a], [b]) => a.localeCompare(b));
  }, [due]);

  // A guest, an unreachable service, or simply nothing scheduled all render as absent. This block
  // earns its space only when the user actually has commitments.
  if (status !== "ready" || groups.length === 0) return null;

  return (
    <section className="mx-auto mb-3 max-w-[820px] overflow-hidden rounded-xl border border-line bg-panel">
      <div className="flex h-11 flex-wrap items-center gap-2.5 border-b border-line px-4 sm:px-[22px]">
        <span className="text-[14px] font-semibold text-fg">Your research commitments</span>
        <span className="rounded border border-line2 px-1.5 py-1 label-mono text-muted">
          {due.length}
        </span>
      </div>

      <p className="border-b border-line px-4 py-3 text-[12.5px] leading-relaxed text-muted sm:px-[22px]">
        Reviews you scheduled for your own theses. These are{" "}
        <strong className="text-fg/85">your commitments, not market events</strong> — nothing here
        fires a trade or changes a thesis.
      </p>

      <ul>
        {groups.map(([day, rows]) => (
          <li key={day} className="border-b border-line/50 last:border-b-0">
            <div className="flex items-baseline gap-2 bg-panel2/30 px-4 py-2 sm:px-[22px]">
              <span className="num text-[12.5px] font-semibold text-fg">
                {new Date(`${day}T00:00:00`).toLocaleDateString(undefined, {
                  weekday: "short",
                  month: "short",
                  day: "numeric",
                })}
              </span>
              {rows.some((r) => r.overdueDays > 0) && (
                <span className="label-mono text-warn">overdue</span>
              )}
            </div>
            {rows.map((row) => (
              <div key={row.ruleId} className="flex items-start gap-3 px-4 py-3 sm:px-[22px]">
                <span className="num mt-[1px] w-[52px] shrink-0 text-right font-mono text-[11px] font-bold text-accent">
                  {row.ticker}
                </span>
                <div className="min-w-0 flex-1">
                  <div className="text-[13.5px] leading-snug text-fg">{row.label}</div>
                  <div className="mt-0.5 label-mono text-muted">
                    {row.intent === "assumption_review" ? "assumption review" : "thesis review"}
                  </div>
                </div>
                <button
                  type="button"
                  onClick={() => applyResearchLink(row.researchLink)}
                  className="shrink-0 rounded-full border border-line2 px-3 py-1.5 text-[12px] font-semibold text-fg/90 transition-colors hover:border-accent/60"
                >
                  Review →
                </button>
              </div>
            ))}
          </li>
        ))}
      </ul>
    </section>
  );
}
