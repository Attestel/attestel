import { useEffect, useMemo, useState } from "react";
import SignalBand from "./SignalBand.jsx";
import RegimeColumn from "./RegimeColumn.jsx";
import CatalystsStrip from "./CatalystsStrip.jsx";
import { Tag } from "./bits.jsx";
import PriceChart from "../PriceChart.jsx";
import PositionSizer from "../risk/PositionSizer.jsx";
import PreTradeChecklist from "../checklist/PreTradeChecklist.jsx";
import { SkeletonText } from "../ui/index.js";
import { fetchPrediction } from "../../lib/api.js";
import { useSettings } from "../../settings/SettingsContext.jsx";
import { cx } from "../../lib/cx.js";

// BeginnerTerminal — the simplified, linear cockpit for beginner mode (docs/BEGINNER.md Piece 4). It
// collapses the dense "institutional attestel" (Confluence grid + LLM read cross-refs, hidden here) into a
// short path: the validated signal with its track record, a curated 2–3-indicator chart, the
// plain-language regime, and a risk-first "plan a trade" card (position sizer + pre-trade checklist).
//
// View/gating only — it unlocks NO trading action (invariant #2). "Log this plan →" hands the draft to
// the Journal, where the trade is actually recorded (and, in beginner mode, the checklist blocks the
// log until it's all-green). The SYNTHETIC banner (App shell + SignalBand) is never hidden here.

const CTL =
  "h-[34px] w-full rounded-md border border-line2 bg-panel2 px-2.5 text-[13px] text-fg focus:border-accent/60 focus:outline-none";
const microLabel = "mb-1 label-mono text-muted";

export default function BeginnerTerminal({ data, ticker, timeframe, onLogTrade }) {
  const { settings } = useSettings();

  // A local trade DRAFT — the sizer + checklist read it. This is a planning surface; the Journal is
  // where the trade is committed, so "Log this plan →" carries the draft there (prefill).
  const [side, setSide] = useState("long");
  const [entry, setEntry] = useState("");
  const [stop, setStop] = useState("");
  const [target, setTarget] = useState("");
  const [thesis, setThesis] = useState("");
  const [sizerResult, setSizerResult] = useState(null);
  const [checklistResult, setChecklistResult] = useState(null);
  const [prediction, setPrediction] = useState(null);

  // Validated-signal fact for the checklist's item 1 (fail-soft; falls back to the thesis otherwise).
  useEffect(() => {
    let alive = true;
    fetchPrediction(ticker, timeframe, settings.defaultHorizon)
      .then((p) => alive && setPrediction(p))
      .catch(() => alive && setPrediction(null));
    return () => {
      alive = false;
    };
  }, [ticker, timeframe, settings.defaultHorizon]);

  const checklistDraft = useMemo(() => ({ thesis, stop, tags: [] }), [thesis, stop]);
  const checklistDashboard = useMemo(
    () => ({ confluence: data?.confluence, signal: prediction?.signal, backtest: prediction?.backtest }),
    [data, prediction]
  );

  const lastClose = data?.price?.lastClose;

  function logPlan() {
    const p = { ticker, side, timeframe };
    if (entry) p.entry = Number(entry);
    if (stop) p.stop = Number(stop);
    if (target) p.target = Number(target);
    if (thesis) p.thesis = thesis;
    if (sizerResult?.shares > 0) p.size = sizerResult.shares;
    onLogTrade?.(p);
  }

  return (
    <div className="flex flex-col gap-3">
      {/* Next catalysts — next earnings + next high-importance macro (fail-silent, descriptive) */}
      <CatalystsStrip ticker={ticker} nextEarnings={data?.nextEarnings} />

      {/* Signal + curated chart + plain-language regime */}
      <section className="overflow-hidden surface-card">
        <SignalBand ticker={ticker} timeframe={timeframe} onLogTrade={onLogTrade} />
        {data ? (
          <div className="grid grid-cols-1 xl:grid-cols-[1.5fr_1fr]">
            <div className="border-b border-line p-[18px] xl:border-b-0 xl:border-r">
              <div className="mb-2.5 flex items-center gap-2.5">
                <span className="text-[14px] font-semibold text-fg">Price</span>
                <Tag tone="outline">Beginner · price + one trend MA</Tag>
              </div>
              <PriceChart
                price={data.price}
                indicators={data.indicators}
                levels={data.regime?.keyLevels}
                maKeys={["sma50"]}
                height={320}
              />
              <p className="mt-2 text-[11px] leading-relaxed text-muted/70">
                A curated set — one trend line (SMA50) plus computed support/resistance. Momentum is in the regime
                panel. Fewer, non-redundant indicators on purpose.
              </p>
            </div>
            <div>
              <RegimeColumn regime={data.regime} indicators={data.indicators} />
            </div>
          </div>
        ) : (
          <div className="p-[18px]">
            <SkeletonText lines={6} />
          </div>
        )}
      </section>

      {/* Plan a trade — sizer + checklist over the local draft */}
      <section className="overflow-hidden surface-card">
        <div className="flex h-11 items-center gap-2.5 border-b border-line px-[18px]">
          <span className="text-[14px] font-semibold text-fg">Plan a trade</span>
          <Tag tone="outline">Risk-first · Not advice</Tag>
        </div>
        <div className="flex flex-col gap-3 p-[18px]">
          {/* 2-up grid on phones so the boxes fill the row; the fixed-width flex layout is restored
              from sm up, so the desktop row is unchanged. */}
          <div className="grid grid-cols-2 gap-3 sm:flex sm:flex-wrap sm:items-end">
            <div className="w-full sm:w-[100px]">
              <div className={microLabel}>Side</div>
              <select value={side} onChange={(e) => setSide(e.target.value)} className={cx(CTL, side === "long" ? "text-accent" : "text-down")}>
                <option value="long">long</option>
                <option value="short">short</option>
              </select>
            </div>
            <div className="w-full sm:w-[120px]">
              <div className={microLabel}>Entry</div>
              <input
                type="number"
                step="any"
                value={entry}
                onChange={(e) => setEntry(e.target.value)}
                placeholder={lastClose != null ? String(lastClose) : "price"}
                className={cx(CTL, "num placeholder:text-muted/40")}
              />
            </div>
            <div className="w-full sm:w-[120px]">
              <div className={microLabel}>Stop</div>
              <input
                type="number"
                step="any"
                value={stop}
                onChange={(e) => setStop(e.target.value)}
                placeholder="stop"
                className={cx(CTL, "num placeholder:text-muted/40")}
              />
            </div>
            <div className="w-full sm:w-[120px]">
              <div className={microLabel}>Target (opt)</div>
              <input
                type="number"
                step="any"
                value={target}
                onChange={(e) => setTarget(e.target.value)}
                placeholder="target"
                className={cx(CTL, "num placeholder:text-muted/40")}
              />
            </div>
          </div>

          <PositionSizer
            dashboard={data}
            side={side}
            entry={entry}
            stop={stop}
            target={target}
            onSuggestStop={(v) => setStop(String(v))}
            onResult={setSizerResult}
          />

          <div>
            <div className={microLabel}>Thesis — why you'd take it</div>
            <input
              type="text"
              value={thesis}
              onChange={(e) => setThesis(e.target.value)}
              placeholder="the setup / reason"
              className={cx(CTL, "placeholder:text-muted/40")}
            />
          </div>

          <PreTradeChecklist
            dashboard={checklistDashboard}
            sizer={sizerResult}
            draft={checklistDraft}
            mode="beginner"
            onChange={setChecklistResult}
          />

          <div className="flex flex-wrap items-center gap-3">
            <button
              type="button"
              onClick={logPlan}
              className="pill-lift inline-flex h-10 items-center rounded-full bg-fg px-6 text-[13px] font-semibold text-bg transition-[transform,background-color,box-shadow] duration-200 ease-premium hover:-translate-y-px hover:bg-white hover:shadow-glow"
            >
              Log this plan →
            </button>
            {checklistResult && (
              <span className="text-[12px] text-muted">
                Checklist{" "}
                <span className={cx("num font-semibold", checklistResult.allPass ? "text-accent" : "text-warn")}>
                  {checklistResult.score.passed} / {checklistResult.score.total}
                </span>
              </span>
            )}
          </div>

          <p className="border-t border-line/60 pt-2.5 text-[10.5px] leading-relaxed text-muted/70">
            Planning only — the trade is recorded in the Journal, where beginner mode holds the log until the
            checklist is all-green. This app never places an order; you decide and execute.
          </p>
        </div>
      </section>
    </div>
  );
}
