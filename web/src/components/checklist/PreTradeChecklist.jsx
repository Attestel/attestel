import { useEffect, useMemo, useRef, useState } from "react";
import { evaluateChecklist } from "../../lib/checklist.js";
import { Tag } from "../terminal/bits.jsx";
import { cx } from "../../lib/cx.js";
import { Icon } from "../shell/icons.jsx";

// PreTradeChecklist — a finite yes/no gate shown before you commit (docs/BEGINNER.md Piece 3). It
// COMPOSES facts the platform already computes (validated signal, sizer, confluence, thesis) into a
// "N / 6" plan-completeness score. It never says buy/sell and blocks nothing here — it is ADVISORY
// (beginner mode makes it blocking in a later piece). No LLM, no network, no execution (invariant #2).
//
// The two self-attest answers (plan-not-FOMO, and item 4's explicit conflict acknowledgement) are
// owned here as local state and folded into the draft before evaluation; the full result is reported
// up via onChange so the parent can persist it with the trade.

function StatusDot({ passed }) {
  return (
    <span
      aria-hidden="true"
      className={cx(
        "mt-[3px] flex h-4 w-4 shrink-0 items-center justify-center rounded-full leading-none",
        passed ? "bg-accent/15 text-accent" : "bg-warn/15 text-warn"
      )}
    >
      <Icon name={passed ? "check" : "circle"} size={passed ? 9 : 7} />
    </span>
  );
}

function ItemRow({ item, control }) {
  return (
    <li className="flex items-start gap-2.5 py-1.5">
      {control || <StatusDot passed={item.passed} />}
      <div className="min-w-0">
        <div className={cx("text-[12.5px] leading-tight", item.passed ? "text-fg/90" : "text-fg/80")}>{item.label}</div>
        {item.detail && <div className="mt-0.5 text-[11px] leading-snug text-muted">{item.detail}</div>}
      </div>
    </li>
  );
}

export default function PreTradeChecklist({ dashboard = null, sizer = null, draft = null, mode = "standard", onChange }) {
  const [planNotFomo, setPlanNotFomo] = useState(false);
  const [conflictAck, setConflictAck] = useState(false);

  const result = useMemo(
    () => evaluateChecklist({ dashboard, sizer, draft: { ...(draft || {}), planNotFomo, conflictAck }, mode }),
    [dashboard, sizer, draft, planNotFomo, conflictAck, mode]
  );

  // Report the evaluated result up whenever it changes (via a ref so onChange need not be stable).
  const onChangeRef = useRef(onChange);
  onChangeRef.current = onChange;
  useEffect(() => {
    onChangeRef.current?.(result);
  }, [result]);

  const confluenceAligned = String(dashboard?.confluence?.trendAlignment || "").toLowerCase().startsWith("aligned");
  const showConflictAck = Boolean(dashboard?.confluence) && !confluenceAligned;

  const { passed, total } = result.score;
  const allGreen = passed === total;

  return (
    <div className="rounded-lg border border-line2 bg-panel2/40 p-4">
      <div className="mb-1 flex items-center gap-2.5">
        <span className="text-[13px] font-semibold text-fg">Pre-trade checklist</span>
        <Tag tone="outline">Plan check · Not advice</Tag>
        <span
          className={cx(
            "num ml-auto rounded-full px-2.5 py-0.5 text-[12px] font-bold leading-none",
            allGreen ? "bg-accent text-bg" : "bg-warn/15 text-warn"
          )}
        >
          {passed} / {total}
        </span>
      </div>
      <p className="mb-2.5 text-[11.5px] leading-relaxed text-muted">A finite check before you commit — not advice.</p>

      <ul className="divide-y divide-line/40">
        {result.items.map((item) => {
          if (item.key === "planNotFomo") {
            return (
              <ItemRow
                key={item.key}
                item={item}
                control={
                  <input
                    type="checkbox"
                    checked={planNotFomo}
                    onChange={(e) => setPlanNotFomo(e.target.checked)}
                    className="mt-0.5 h-5 w-5 shrink-0 accent-accent sm:h-4 sm:w-4"
                    aria-label={item.label}
                  />
                }
              />
            );
          }
          if (item.key === "confluenceOK") {
            return (
              <li key={item.key} className="py-1.5">
                <div className="flex items-start gap-2.5">
                  <StatusDot passed={item.passed} />
                  <div className="min-w-0">
                    <div className={cx("text-[12.5px] leading-tight", item.passed ? "text-fg/90" : "text-fg/80")}>{item.label}</div>
                    {item.detail && <div className="mt-0.5 text-[11px] leading-snug text-muted">{item.detail}</div>}
                    {showConflictAck && (
                      <label className="mt-1.5 flex w-fit cursor-pointer items-center gap-2 py-1 text-[11.5px] text-fg/80 sm:py-0">
                        <input
                          type="checkbox"
                          checked={conflictAck}
                          onChange={(e) => setConflictAck(e.target.checked)}
                          className="h-5 w-5 shrink-0 accent-accent sm:h-3.5 sm:w-3.5"
                        />
                        I see the conflict and I'm taking it anyway
                      </label>
                    )}
                  </div>
                </div>
              </li>
            );
          }
          return <ItemRow key={item.key} item={item} />;
        })}
      </ul>

      <p className="mt-2.5 border-t border-line/60 pt-2.5 text-[10.5px] leading-relaxed text-muted/70">
        Composes facts you already have into one decision. It scores plan completeness — it never says buy or sell,
        and it places no order.
      </p>
    </div>
  );
}
