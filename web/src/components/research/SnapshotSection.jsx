import { useEffect, useState } from "react";
import { fetchBrief, listTheses } from "../../lib/api.js";
import { SkeletonText } from "../ui/index.js";
import { Micro, Tag } from "../terminal/bits.jsx";
import { SectionShell } from "./Placeholder.jsx";
import CatalystsStrip from "../terminal/CatalystsStrip.jsx";
import RegimeColumn from "../terminal/RegimeColumn.jsx";
import ConfluenceColumn from "../terminal/ConfluenceColumn.jsx";
import ReadColumn from "../terminal/ReadColumn.jsx";
import BeginnerTerminal from "../terminal/BeginnerTerminal.jsx";
import { fmtPrice, fmtSignedPct, deltaColor, relFromUnix } from "../../lib/format.js";
import { cx } from "../../lib/cx.js";

// SnapshotSection — "what do I know about this company right now".
//
// Deliberately NOT the old Terminal. The Terminal led with a full-width directional signal band; guide §9.4
// says prediction is one piece of EVIDENCE and must never be "a bright directional badge without its
// validation context". So the model lives in Evidence -> Model evidence and Snapshot only links to it.
// Chart/technical material is likewise framed as evidence (Evidence -> Chart context), not as the identity
// of the product.
//
// What Snapshot answers, in order: what changed (brief) · where does my belief stand (thesis status) ·
// what's imminent (catalysts) · what does the computed context say (regime + confluence + read).
//
// Cost: one cached brief call on mount/ticker-change and one deterministic journal read. No committee, no
// scenario, no transcript analysis, no chat — those stay explicitly invoked (guide §3.3, invariant #4).

const VERDICT_TONE = { confirming: "accent", neutral: "outline", challenged: "down" };

export default function SnapshotSection({
  ticker,
  data,
  quote,
  quoteLive,
  timeframe,
  onTimeframe,
  timeframes,
  level,
  onLogTrade,
  onOpenSection,
}) {
  const [brief, setBrief] = useState(null);
  const [loadingBrief, setLoadingBrief] = useState(true);
  const [theses, setTheses] = useState(null);

  useEffect(() => {
    let alive = true;
    setLoadingBrief(true);
    setBrief(null);
    fetchBrief(ticker)
      .then((b) => alive && setBrief(b))
      .catch(() => alive && setBrief({ error: true }))
      .finally(() => alive && setLoadingBrief(false));
    return () => {
      alive = false;
    };
  }, [ticker]);

  useEffect(() => {
    let alive = true;
    setTheses(null);
    listTheses(ticker, "active")
      .then((t) => alive && setTheses(t))
      .catch(() => alive && setTheses([]));
    return () => {
      alive = false;
    };
  }, [ticker]);

  // BEGINNER mode keeps its simpler chart-centric cockpit (docs/BEGINNER.md Piece 4) — chart + sizer +
  // pre-trade checklist. That surface used to be reached through Terminal, which dispatched on level; the
  // dispatch now lives here so Snapshot is the one company entry point at both levels and a beginner loses
  // nothing. The dense Standard composition below is the expert view.
  if (level !== "standard") {
    return (
      <BeginnerTerminal
        data={data}
        ticker={ticker}
        timeframe={timeframe}
        onLogTrade={onLogTrade}
      />
    );
  }

  const s = brief?.structured;
  const stub = String(brief?.modelUsed || "").includes("stub");
  const price = quote?.price ?? data?.price?.lastClose;
  const chg = quote?.changePct ?? data?.price?.changePct;
  const signal = data?.signal;
  const gated = Boolean(signal?.signal); // a signal only exists when the walk-forward gate passed

  return (
    <div className="flex flex-col gap-3">
      {/* Price + provenance header — the company's current state, no verdict. */}
      <SectionShell
        title={`${ticker} · Snapshot`}
        tags={
          <>
            {data?.priceSource && <Tag tone="outline">{data.priceSource}</Tag>}
            {data?.priceSourceIsSynthetic && <Tag tone="warn">synthetic</Tag>}
            {!loadingBrief && brief && !brief.error && (
              <Tag tone={stub ? "warn" : "llm"}>{brief.modelUsed || "model"}</Tag>
            )}
          </>
        }
        right={
          <>
            <span className="num text-[15px] font-semibold text-fg">{fmtPrice(price)}</span>
            <span className={cx("num text-[12.5px] font-semibold", deltaColor(chg))}>
              {fmtSignedPct(chg, 2)}
            </span>
            <span
              className={cx("h-1.5 w-1.5 rounded-full", quoteLive ? "bg-accent" : "bg-muted/50")}
              title={quoteLive ? "live quote" : "no live quote"}
              aria-hidden="true"
            />
          </>
        }
      >
        {/* What changed — the brief's lead only; the full brief is Evidence-adjacent detail. */}
        <div className="px-[22px] py-4">
          {loadingBrief ? (
            <SkeletonText lines={3} />
          ) : s ? (
            <>
              <p className="text-[14.5px] font-semibold leading-snug text-fg">{s.headline || "—"}</p>
              {s.summary && <p className="mt-1.5 text-[12.5px] leading-[1.55] text-muted">{s.summary}</p>}
              {brief.contextUsed?.length > 0 && (
                <p className="mt-2.5 label-mono text-muted">
                  grounded in: {brief.contextUsed.join(" · ")}
                </p>
              )}
            </>
          ) : (
            <p className="text-[12.5px] leading-relaxed text-muted">
              No company read available right now — the gateway or analysis service may be unreachable.
              The computed context below is unaffected.
            </p>
          )}
        </div>

        {/* Active-thesis status — the user's own belief, front and centre, distinct from any model output. */}
        <div className="border-t border-line bg-panel2/30 px-[22px] py-3">
          <Micro className="mb-2">Your thesis</Micro>
          {theses === null ? (
            <SkeletonText lines={1} />
          ) : theses.length === 0 ? (
            <div className="flex flex-wrap items-center gap-2.5">
              <span className="text-[12.5px] text-muted">No active thesis for {ticker}.</span>
              <button
                type="button"
                onClick={() => onOpenSection("thesis")}
                className="text-[12px] font-semibold text-accent transition-[filter] hover:brightness-125"
              >
                Write one →
              </button>
            </div>
          ) : (
            <ul className="flex flex-col gap-2">
              {theses.slice(0, 3).map((t) => (
                <li key={t.id} className="flex flex-wrap items-start gap-2">
                  <Tag tone={VERDICT_TONE[t.lastCheck?.verdict] || "warn"}>
                    {t.lastCheck?.verdict || "unchecked"}
                  </Tag>
                  <span className="min-w-0 flex-1 truncate text-[12.5px] text-fg/85">{t.text}</span>
                  {t.lastCheck?.at && (
                    <span className="num text-[10.5px] text-muted/70">{relFromUnix(t.lastCheck.at)}</span>
                  )}
                  <button
                    type="button"
                    onClick={() => onOpenSection("thesis")}
                    className="shrink-0 text-[11.5px] text-accent transition-[filter] hover:brightness-125"
                  >
                    Open →
                  </button>
                </li>
              ))}
            </ul>
          )}
        </div>

        {/* Model evidence — a one-line status with its validation state, never a bare direction. The full
            track record (hit rate, Sharpe, expectancy, drawdown, confidence) lives in Evidence. */}
        <div className="flex flex-wrap items-center gap-x-2.5 gap-y-1.5 border-t border-line px-[22px] py-3">
          <Micro>Model evidence</Micro>
          {gated ? (
            <>
              <Tag tone="info">validated model available</Tag>
              <span className="text-[11.5px] text-muted">
                A backtest-gated model has an opinion on {ticker}. Read it with its track record before
                weighing it.
              </span>
            </>
          ) : (
            <>
              <Tag tone="outline">no validated model</Tag>
              <span className="text-[11.5px] text-muted">
                {signal?.reason
                  ? `Withheld — ${signal.reason}.`
                  : "Nothing to show until a walk-forward backtest passes."}
              </span>
            </>
          )}
          <button
            type="button"
            onClick={() => onOpenSection("evidence", "model")}
            className="ml-auto shrink-0 text-[12px] text-accent transition-[filter] hover:brightness-125"
          >
            Model evidence →
          </button>
        </div>
      </SectionShell>

      {/* Imminent events (fail-silent, descriptive). */}
      <CatalystsStrip ticker={ticker} nextEarnings={data?.nextEarnings} />

      {/* Computed context — deterministic facts, labelled as chart evidence rather than as the answer. */}
      <SectionShell
        title="Computed context"
        tags={<Tag tone="outline">deterministic</Tag>}
        right={
          <button
            type="button"
            onClick={() => onOpenSection("evidence", "chart")}
            className="text-[12px] text-accent transition-[filter] hover:brightness-125"
          >
            Chart evidence →
          </button>
        }
        note="Indicators, regime and cross-timeframe agreement are computed in the analysis service — facts the model quotes, not a call. Support and resistance are server-computed and override anything the model returns."
      >
        {data ? (
          <div className="grid grid-cols-1 xl:grid-cols-3">
            <div className="border-b border-line xl:border-b-0 xl:border-r">
              <ConfluenceColumn
                confluence={data.confluence}
                timeframe={timeframe}
                onSelectTimeframe={onTimeframe}
              />
            </div>
            <div className="border-b border-line xl:border-b-0 xl:border-r">
              <RegimeColumn regime={data.regime} indicators={data.indicators} />
            </div>
            <div>
              <ReadColumn read={data.read} diff={data.readDiff} lastClose={data.price?.lastClose} />
            </div>
          </div>
        ) : (
          <div className="grid grid-cols-1 xl:grid-cols-3">
            {Array.from({ length: 3 }).map((_, i) => (
              <div
                key={i}
                className="border-b border-line px-[18px] py-5 xl:border-b-0 xl:border-r xl:last:border-r-0"
              >
                <SkeletonText lines={6} />
              </div>
            ))}
          </div>
        )}
      </SectionShell>
    </div>
  );
}
