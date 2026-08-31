import { useEffect, useState } from "react";
import { Micro, Tag } from "../terminal/bits.jsx";
import { listDueReviews, applyResearchLink } from "../../lib/monitoringApi.js";

// DueReviews — the "what research review is due" half of Today's attention block (Step 08).
//
// Deterministic and cheap: one call to the alerts service, which computes due dates from the rule's
// own fields. No model call, no interval — it loads once when the panel mounts, which is why it is
// safe on the default landing destination (invariant #4).
//
// A due review is a RESEARCH commitment the user made to themselves. The action is always "review",
// never a trade action, and the label is composed server-side in code from typed fields so it cannot
// drift from what the rule actually says.
export default function DueReviews({ user, onOpenCompany, onOpenThesis }) {
  const [due, setDue] = useState([]);
  const [status, setStatus] = useState("loading");

  useEffect(() => {
    if (!user) {
      setDue([]);
      setStatus("ready");
      return undefined;
    }
    let alive = true;
    setStatus("loading");
    listDueReviews()
      .then((rows) => {
        if (!alive) return;
        setDue(rows);
        setStatus("ready");
      })
      .catch(() => alive && setStatus("down"));
    return () => {
      alive = false;
    };
  }, [user?.id]); // eslint-disable-line react-hooks/exhaustive-deps

  // Nothing due is the normal, good state — it needs no empty state of its own inside a panel that
  // already has one.
  if (status === "loading" || (status === "ready" && due.length === 0)) return null;

  if (status === "down") {
    return (
      <div className="border-t border-line px-[22px] py-2.5 text-[11.5px] text-muted">
        Scheduled reviews unavailable (the alerts service may be down).
      </div>
    );
  }

  return (
    <>
      <div className="flex h-8 items-center gap-2 border-y border-line bg-panel2/30 px-[22px]">
        <Micro>Reviews due</Micro>
        <span className="text-[10.5px] text-muted/60">commitments you scheduled</span>
      </div>
      <ul className="divide-y divide-line">
        {due.map((row) => (
          <li key={row.ruleId} className="flex flex-wrap items-start gap-x-3 gap-y-1 px-[22px] py-2.5">
            <div className="min-w-0 flex-1">
              <div className="flex flex-wrap items-center gap-2">
                <span className="num text-[12px] font-bold text-accent">{row.ticker}</span>
                {row.overdueDays > 0 ? (
                  <Tag tone="warn">
                    {row.overdueDays} {row.overdueDays === 1 ? "day" : "days"} overdue
                  </Tag>
                ) : (
                  <Tag tone="outline">due</Tag>
                )}
              </div>
              <p className="mt-1 text-[12.5px] leading-snug text-fg/85">{row.label}</p>
            </div>
            <button
              type="button"
              onClick={() =>
                applyResearchLink(row.researchLink, {
                  onOpenCompany: onOpenThesis || onOpenCompany,
                })
              }
              className="shrink-0 rounded-full border border-line2 px-3 py-1.5 text-[12px] font-semibold text-fg/90 transition-colors hover:border-accent/60"
            >
              Review →
            </button>
          </li>
        ))}
      </ul>
    </>
  );
}
