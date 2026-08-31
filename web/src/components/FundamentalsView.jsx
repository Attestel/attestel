import { fmtB } from "../lib/api.js";
import { cx } from "../lib/cx.js";
import { SkeletonText } from "./ui/index.js";
import { Tag } from "./terminal/bits.jsx";
import { Icon } from "./shell/icons.jsx";

// FundamentalsView — the "Fundamentals" tab, redesigned as one dense "attestel" (design_examples/
// 2c-fundamentals): a beat/miss quarter ledger with restatement caveats, live EPS-surprise history,
// and the product roadmap timeline — all inside a single bordered container with hairline dividers.
// Numbers are floats (correct for this domain); seed quarters/roadmap are NVDA-only and labelled.

const Q_GRID = "grid-cols-[150px_190px_1fr_1fr_1fr_1fr_76px]";
const EPS_GRID = "grid-cols-[110px_minmax(150px,180px)_1fr_64px]";

function SecHeader({ title, borderTop, children }) {
  return (
    <div
      className={cx(
        "flex h-11 items-center gap-2.5 border-b border-line px-[22px]",
        borderTop && "border-t border-t-line"
      )}
    >
      <span className="text-[14px] font-semibold text-fg">{title}</span>
      {children}
    </div>
  );
}

const dim = "text-fg/80"; // slightly recessed cell value

function QuarterRow({ q, last }) {
  const rev = q.revenue || {};
  const beat = rev.beat;
  return (
    <div className={cx("px-[22px] py-3.5", last ? "border-b border-line" : "border-b border-line/50")}>
      <div className={cx("grid items-baseline gap-3.5", Q_GRID)}>
        <div>
          <span className="text-[14px] font-semibold text-fg">{q.label}</span>
          <div className="num mt-0.5 text-[10.5px] text-muted/70">rep. {q.reported}</div>
        </div>
        <div>
          <span className="num text-[20px] font-bold text-fg">{fmtB(rev.actual)}</span>
          <div className="mt-0.5 text-[11px] text-muted">
            vs est {fmtB(rev.estimate)}
            {rev.yoyPct != null && (
              <>
                {" · "}
                <span className="text-accent">+{rev.yoyPct}% YoY</span>
              </>
            )}
          </div>
        </div>
        <span className={cx("num text-[15px]", dim)}>{fmtB(q.dataCenter?.actual)}</span>
        <span className={cx("num text-[15px]", dim)}>{q.epsNonGaap?.actual != null ? `$${q.epsNonGaap.actual}` : "—"}</span>
        <span className={cx("num text-[15px]", dim)}>{q.grossMargin?.nonGaap != null ? `${q.grossMargin.nonGaap}%` : "—"}</span>
        <span className={cx("num text-[15px]", dim)}>{q.nextGuide ? fmtB(q.nextGuide.revenue) : "—"}</span>
        {beat != null ? (
          <span
            className={cx(
              "justify-self-end rounded-sm px-2.5 py-1 font-mono text-[11px] font-bold tracking-[0.08em]",
              beat ? "bg-up/14 text-up" : "bg-down/14 text-down"
            )}
          >
            {beat ? "BEAT" : "MISS"}
          </span>
        ) : (
          <span />
        )}
      </div>
      {q.stockReaction && <div className="mt-2 text-[11.5px] text-muted/70">⌐ {q.stockReaction}</div>}
    </div>
  );
}

function EpsRow({ r, last, denom }) {
  const pct = Number(r.surprisePercentage);
  const beat = r.beat ?? (r.surprise != null ? Number(r.surprise) >= 0 : pct >= 0);
  const width = Number.isFinite(pct) ? Math.min(100, (Math.abs(pct) / denom) * 100) : 0;
  return (
    <div className={cx("grid items-center gap-4 py-2.5", !last && "border-b border-line/50", EPS_GRID)}>
      <span className="num text-[12.5px] font-semibold text-fg">{r.fiscalDateEnding || r.label || "—"}</span>
      <span className={cx("num truncate text-[13px]", dim)}>
        {r.reportedEPS != null ? `$${Number(r.reportedEPS).toFixed(2)}` : "—"}
        {r.estimatedEPS != null && (
          <>
            {" "}
            <span className="text-muted/60">vs</span> ${Number(r.estimatedEPS).toFixed(2)}
          </>
        )}
      </span>
      <div className="h-[5px] overflow-hidden rounded-full bg-line">
        <div className={cx("h-full rounded-full", beat ? "bg-accent" : "bg-down")} style={{ width: `${width}%` }} />
      </div>
      <span className={cx("num justify-self-end text-[12.5px]", beat ? "text-up" : "text-down")}>
        {Number.isFinite(pct) ? `${pct >= 0 ? "+" : ""}${pct.toFixed(1)}%` : "—"}
      </span>
    </div>
  );
}

function roadTone(status = "") {
  const s = status.toLowerCase();
  if (s.includes("ship")) return { bar: "bg-accent", badge: "bg-accent/14 text-accent" };
  if (s.includes("ramp") || s.includes("production")) return { bar: "bg-warn", badge: "bg-warn/14 text-warn" };
  return { bar: "bg-dim/60", badge: "bg-line text-muted" };
}

function RoadRow({ r, last }) {
  const t = roadTone(r.status);
  return (
    <div className={cx("flex gap-3 py-3", !last && "border-b border-line/50")}>
      <div className={cx("w-0.5 shrink-0 self-stretch rounded-full", t.bar)} />
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-baseline gap-2.5">
          <span className="text-[14px] font-semibold text-fg">{r.gen}</span>
          <span className={cx("rounded px-1.5 py-0.5 label-mono", t.badge)}>
            {r.status}
          </span>
          <span className="num text-[12px] text-muted/70">{r.year}</span>
        </div>
        {r.products && <div className="num mt-1.5 text-[12px] text-muted">{r.products}</div>}
        {r.note && <div className="mt-1 text-[12.5px] text-muted/90">{r.note}</div>}
      </div>
    </div>
  );
}

export default function FundamentalsView({ data, ticker, hasSeed, loading }) {
  const quarters = data?.fundamentals?.quarters || [];
  const traps = data?.accountingTraps || [];
  const eps = (data?.earnings || []).filter((e) => e.reportedEPS != null).slice(0, 6);
  const roadmap = data?.catalysts?.roadmap || [];
  const live = String(data?.earningsSource || "").startsWith("live");
  const epsDenom = Math.max(10, ...eps.map((r) => Math.abs(Number(r.surprisePercentage) || 0)));

  return (
    <section className="overflow-hidden surface-card">
      <SecHeader title="Fundamentals — beat / miss">
        <Tag tone="llm">Seed · Verified</Tag>
      </SecHeader>

      {loading || !data ? (
        <div className="px-[22px] py-5">
          <SkeletonText lines={6} />
        </div>
      ) : (
        <>
          {/* Restatement caveats */}
          {hasSeed && traps.length > 0 && (
            <div className="flex flex-col gap-2 border-b border-line px-[22px] py-3.5">
              {traps.map((t, i) => (
                <div key={i} className="text-[12.5px] leading-snug text-warn/90">
                  <Icon name="warning" size={13} className="mt-px flex-none" /> {t}
                </div>
              ))}
            </div>
          )}

          {/* Quarter ledger (NVDA seed only) */}
          {hasSeed && quarters.length > 0 ? (
            <div className="overflow-x-auto">
              <div className="min-w-[920px]">
                <div
                  className={cx(
                    "grid items-center gap-3.5 border-b border-line px-[22px] py-2.5 label-mono text-muted",
                    Q_GRID
                  )}
                >
                  <span>Quarter</span>
                  <span>Revenue</span>
                  <span>Data center</span>
                  <span>EPS (non-GAAP)</span>
                  <span>Gross margin</span>
                  <span>Next guide</span>
                  <span className="justify-self-end">Result</span>
                </div>
                {quarters.map((q, i) => (
                  <QuarterRow key={q.label} q={q} last={i === quarters.length - 1} />
                ))}
              </div>
            </div>
          ) : (
            !hasSeed && (
              <div className="border-b border-line px-[22px] py-4">
                <p className="text-[13px] leading-relaxed text-muted">
                  Rich seed fundamentals and the supply-chain / roadmap tracker are curated for <strong className="text-fg">NVDA</strong>{" "}
                  only. For {ticker}, the EPS surprise history below relies on live Alpha Vantage + SEC EDGAR and may be sparse
                  without API keys.
                </p>
              </div>
            )
          )}

          {/* EPS surprise history */}
          <SecHeader title="EPS surprise history">
            {live ? (
              <span className="inline-flex items-center gap-1.5 rounded bg-llm/15 px-2 py-1 label-mono text-llm">
                <span className="h-[5px] w-[5px] rounded-full bg-accent" /> Live · Alphavantage
              </span>
            ) : (
              <Tag tone="outline">Seed</Tag>
            )}
          </SecHeader>
          <div className="px-[22px] pb-3.5 pt-1.5">
            {eps.length > 0 ? (
              <div className="overflow-x-auto">
                <div className="min-w-[560px]">
                  {eps.map((r, i) => (
                    <EpsRow key={i} r={r} last={i === eps.length - 1} denom={epsDenom} />
                  ))}
                </div>
              </div>
            ) : (
              <p className="py-2 text-[12.5px] italic text-muted/70">No EPS history available.</p>
            )}
          </div>

          {/* Roadmap & supply chain (NVDA seed only) */}
          {hasSeed && roadmap.length > 0 && (
            <>
              <SecHeader title="Roadmap & supply chain" borderTop />
              <div className="px-[22px] pb-4 pt-1.5">
                {roadmap.map((r, i) => (
                  <RoadRow key={r.gen} r={r} last={i === roadmap.length - 1} />
                ))}
              </div>
            </>
          )}
        </>
      )}
    </section>
  );
}
