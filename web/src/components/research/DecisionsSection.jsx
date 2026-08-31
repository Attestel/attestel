import { useEffect, useState } from "react";
import { listTrades } from "../../lib/api.js";
import { useAuth } from "../../auth/AuthContext.jsx";
import { SkeletonText } from "../ui/index.js";
import { Micro, Tag } from "../terminal/bits.jsx";
import { SectionShell } from "./Placeholder.jsx";
import { fmtPrice } from "../../lib/format.js";
import DecisionLog from "../decisions/DecisionLog.jsx";

// DecisionsSection — Research → Decision log: what you decided about THIS company, and why.
//
// The decision records lead, because they are the ones that carry reasoning. The trade ledger follows
// as a separate, clearly-labelled list of executions for the same ticker — and the two are NOT joined
// in the client. `Trade.thesis` is a free-text string with no thesis id (journal/trades.go), so
// inferring which decision a legacy trade belongs to would be a guess dressed as a record. A trade is
// linked to a decision only when the user linked it, and then the link is stored.

function LegacyTrades({ ticker }) {
  const { user } = useAuth();
  const [trades, setTrades] = useState(null);

  useEffect(() => {
    let alive = true;
    setTrades(null);
    listTrades("all", "all")
      .then((all) => alive && setTrades((all || []).filter((t) => t.ticker === ticker)))
      .catch(() => alive && setTrades("error"));
    return () => {
      alive = false;
    };
  }, [ticker, user?.id]);

  if (!user) return null;
  if (trades === null) {
    return (
      <div className="px-[22px] py-4">
        <SkeletonText lines={2} />
      </div>
    );
  }
  if (trades === "error") {
    return (
      <p className="px-[22px] py-4 text-[12.5px] text-muted">
        Your trade ledger is unavailable — the journal service may be unreachable.
      </p>
    );
  }
  if (trades.length === 0) {
    return (
      <p className="px-[22px] py-4 text-[12.5px] text-muted">
        No trades recorded for {ticker}.
      </p>
    );
  }

  return (
    <>
      <div className="flex h-8 items-center gap-2 border-b border-line bg-panel2/30 px-[22px]">
        <Micro>Executions</Micro>
        <span className="num text-[10.5px] text-muted/60">{trades.length}</span>
      </div>
      <ul className="divide-y divide-line">
        {trades
          .slice()
          .sort((a, b) => (b.createdAt || 0) - (a.createdAt || 0))
          .map((t) => (
            <li key={t.id} className="flex flex-wrap items-start gap-x-2.5 gap-y-1.5 px-[22px] py-3">
              <span className="num w-[82px] shrink-0 text-[11.5px] font-semibold text-fg/85">
                {t.entry?.date || "—"}
              </span>
              <Tag tone={t.side === "short" ? "down" : "accent"}>{t.side}</Tag>
              <Tag tone={t.mode === "paper" ? "warn" : "outline"}>
                {t.mode === "paper" ? "simulated" : "live"}
              </Tag>
              {t.origin === "signal" && <Tag tone="info">from model</Tag>}
              <span className="num text-[11.5px] text-muted">
                {fmtPrice(t.entry?.price)} · {t.entry?.size ?? "—"}
                {t.exit ? ` → ${fmtPrice(t.exit.price)}` : " · open"}
              </span>
              {t.attachedRead && <Tag tone="llm">read snapshot</Tag>}
              {t.attachedSignal && <Tag tone="info">signal snapshot</Tag>}
              {t.checklist && (
                <Tag tone={t.checklist.allPass ? "accent" : "outline"}>
                  checklist {t.checklist.allPass ? "complete" : "partial"}
                </Tag>
              )}
              {t.thesis && <p className="w-full text-[11.5px] leading-relaxed text-muted">{t.thesis}</p>}
            </li>
          ))}
      </ul>
    </>
  );
}

export default function DecisionsSection({ ticker }) {
  return (
    <div className="flex flex-col gap-3">
      <SectionShell
        title={`${ticker} · Decision log`}
        tags={<Tag tone="outline">your record</Tag>}
        note="Why you acted on this company — or deliberately did not. Each record keeps the thesis and evidence as they stood at the time, so editing the thesis later never rewrites what you decided."
      >
        <DecisionLog
          ticker={ticker}
          title="Decisions"
          emptyHint="Record what you decided about this company while the reasoning is fresh. A deliberate non-action counts — it is a decision, and it is reviewable."
        />
      </SectionShell>

      <SectionShell
        title="Executions"
        tags={<Tag tone="outline">trade ledger</Tag>}
        note="Trades you recorded for this company. Shown separately on purpose: a trade is one possible consequence of a decision, and an old trade carries no thesis id — nothing here guesses which decision it belonged to. Paper and live stay labelled and are never summed together."
      >
        <LegacyTrades ticker={ticker} />
      </SectionShell>
    </div>
  );
}
