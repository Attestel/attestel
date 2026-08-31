import { motion, useReducedMotion } from "framer-motion";
import { cx } from "../../lib/cx.js";
import { SkeletonText } from "../ui/index.js";
import { Micro, Sec, Tag } from "../terminal/bits.jsx";
import { Icon } from "../shell/icons.jsx";

// BriefPane — ONE structured AI Market Brief rendered as a dense "attestel" column (design_examples/
// 2a-brief): headline + summary, WHAT HAPPENED / WHY IT MOVED / WHAT TO WATCH / RISK FLAGS, then a
// provenance + advice-guard footer. AI SYNTHESIS of already-collected facts — never a buy/sell call.

// Tickers/acronyms rendered in mono; ± percentages and trend/beat/miss words colored — so the facts
// read like a live attestel without the model inventing anything (the strings come pre-built from the API).
const TICKERS = new Set([
  "SPY", "QQQ", "DIA", "IWM", "NVDA", "GOOGL", "GOOG", "TSLA", "AMD", "AAPL",
  "MSFT", "AMZN", "META", "AVGO", "INTC", "MU", "TSM", "ARM", "SMCI",
]);
const TOKEN_RE = /([+-]\d[\d.,]*%|\d+\.\d+|\buptrend\b|\bdowntrend\b|\bbeat\b|\bmiss(?:ed)?\b|\b[A-Z]{2,6}\b)/g;

function decorate(text) {
  return String(text)
    .split(TOKEN_RE)
    .map((p, i) => {
      if (!p) return null;
      if (/^[+-]\d[\d.,]*%$/.test(p))
        return (
          <span key={i} className={cx("num", p[0] === "-" ? "text-down" : "text-accent")}>
            {p}
          </span>
        );
      if (/^\d+\.\d+$/.test(p) || TICKERS.has(p)) return <span key={i} className="num text-fg">{p}</span>;
      if (/^uptrend$/i.test(p) || /^beat$/i.test(p)) return <span key={i} className="text-up">{p}</span>;
      if (/^downtrend$/i.test(p) || /^miss(ed)?$/i.test(p)) return <span key={i} className="text-down">{p}</span>;
      return <span key={i}>{p}</span>;
    });
}

function FactList({ items, marker = "•", markerClass = "text-muted/50", textClass = "text-fg/80", emptyNote }) {
  if (!items || items.length === 0) {
    return emptyNote ? <p className="text-[12.5px] italic leading-relaxed text-muted/70">{emptyNote}</p> : null;
  }
  return (
    <div className="flex flex-col gap-2.5">
      {items.map((x, i) => (
        <div key={i} className={cx("flex gap-2.5 text-[13px] leading-[1.5]", textClass)}>
          <span aria-hidden="true" className={cx("shrink-0", markerClass)}>
            {marker}
          </span>
          <span>{typeof x === "string" ? decorate(x) : JSON.stringify(x)}</span>
        </div>
      ))}
    </div>
  );
}

function Footer({ contextUsed, disclaimer }) {
  return (
    <Sec divide={false}>
      {contextUsed?.length > 0 && (
        <div className="border-t border-line pt-3.5">
          <div className="label-mono text-muted">
            grounded in: {contextUsed.join(" · ")}
          </div>
        </div>
      )}
      <div className="mt-2 text-[10.5px] text-muted/60">
        {disclaimer || "AI-generated summary — informational, not investment advice"}
      </div>
    </Sec>
  );
}

export default function BriefPane({ brief, title, loading, className }) {
  const reduce = useReducedMotion();

  const header = (badges) => (
    <div className="flex h-11 shrink-0 items-center gap-2.5 border-b border-line px-[22px]">
      <span className="text-[14px] font-semibold text-fg">{title}</span>
      {badges}
    </div>
  );

  if (loading) {
    return (
      <div className={cx("flex flex-col", className)}>
        {header(null)}
        <Sec divide={false}>
          <SkeletonText lines={7} />
        </Sec>
      </div>
    );
  }

  const s = brief?.structured;
  if (!brief || brief.error || !s) {
    return (
      <div className={cx("flex flex-col", className)}>
        {header(null)}
        <Sec divide={false}>
          <p className="text-[13px] font-semibold text-fg">Brief unavailable</p>
          <p className="mt-1.5 text-[12.5px] leading-relaxed text-muted">
            The brief couldn't be generated right now (the gateway or analysis service may be unreachable). Nothing else is
            affected — try Refresh in a moment.
          </p>
        </Sec>
      </div>
    );
  }

  const stub = String(brief.modelUsed || "").includes("stub");
  const warnings = brief.warnings || [];
  const riskFlags = Array.isArray(s.riskFlags) ? s.riskFlags.filter(Boolean) : [];

  const badges = (
    <>
      <Tag tone={stub ? "warn" : "llm"}>{brief.modelUsed || "model"}</Tag>
      {brief.retried && <Tag tone="outline">retried</Tag>}
      <Tag tone="info">AI Synthesis</Tag>
    </>
  );

  return (
    <div className={cx("flex flex-col", className)}>
      {header(badges)}

      {/* Lead: headline + summary + advice-guard warnings */}
      <Sec>
        <motion.div
          initial={reduce ? false : { opacity: 0, y: 6 }}
          animate={{ opacity: 1, y: 0 }}
          transition={{ duration: 0.22 }}
        >
          <div className="mb-1.5 text-[14px] font-semibold leading-snug text-fg">{s.headline || "—"}</div>
          {s.summary && <p className="text-[12.5px] leading-[1.55] text-muted">{s.summary}</p>}
        </motion.div>
        {warnings.length > 0 && (
          <div className="mt-3 flex flex-col gap-2">
            {warnings.map((w, i) => (
              <div
                key={i}
                className="rounded-sm border border-warn/22 bg-warn/[0.08] px-3 py-2 text-[12px] text-warn/90"
              >
                <Icon name="warning" size={13} className="mt-px flex-none" /> {w}
              </div>
            ))}
          </div>
        )}
      </Sec>

      {/* What happened */}
      <Sec>
        <Micro tone="llm" className="mb-3">What happened</Micro>
        <FactList items={s.whatHappened} emptyNote="No notable moves in the collected facts." />
      </Sec>

      {/* Why it moved */}
      <Sec>
        <Micro tone="llm" className="mb-3">Why it moved</Micro>
        <FactList
          items={s.whyItMoved}
          textClass="text-muted"
          emptyNote="Not enough sourced drivers to attribute today's moves."
        />
      </Sec>

      {/* What to watch */}
      <Sec divide={riskFlags.length > 0}>
        <Micro tone="llm" className="mb-3">What to watch</Micro>
        <FactList items={s.watch} marker="›" markerClass="text-llm" />
      </Sec>

      {/* Risk flags (only when present) */}
      {riskFlags.length > 0 && (
        <Sec divide={false}>
          <Micro tone="warn" className="mb-3 flex items-center gap-1.5"><Icon name="warning" size={11} /> Risk flags</Micro>
          <div className="flex flex-col gap-2">
            {riskFlags.map((x, i) => (
              <div
                key={i}
                className="rounded-sm border border-warn/22 bg-warn/[0.08] px-3 py-2.5 text-[12.5px] text-warn/90"
              >
                {typeof x === "string" ? x : JSON.stringify(x)}
              </div>
            ))}
          </div>
        </Sec>
      )}

      <Footer contextUsed={brief.contextUsed} disclaimer={brief.disclaimer} />
    </div>
  );
}
