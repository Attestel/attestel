import { motion } from "framer-motion";
import { cx } from "../../lib/cx.js";
import { trendTone, oscillatorTone } from "../../lib/indicatorInfo.js";
import { SegmentedMeter, Tooltip } from "../ui/index.js";
import { TOOLTIPS } from "../../lib/indicatorInfo.js";
import { ColHeader, Micro, Sec } from "./bits.jsx";
import { Icon } from "../shell/icons.jsx";

// COL 1 — Multi-timeframe confluence. Deterministic agreement/conflict across timeframes (computed
// in the analysis service, never model-invented). Interactive matrix: click a row to load that
// timeframe. Grid columns: TF · TREND · MOM · STR · STOCH.

const GRID = "grid-cols-[46px_1fr_44px_40px_48px]";

const numIn = (s, re) => {
  const m = String(s || "").match(re);
  return m ? Math.round(Number(m[1])) : null;
};

const TREND_PILL = {
  up: "text-up bg-up/12",
  down: "text-down bg-down/12",
  neutral: "text-warn bg-warn/12",
};

const oscColor = (full) => {
  const t = oscillatorTone(full);
  return t === "caution" ? "text-down" : t === "bull" ? "text-up" : "text-warn";
};
const strColor = (full) => {
  const w = String(full || "").toLowerCase();
  return w.includes("strong") || w.includes("developing") ? "text-info" : "text-muted";
};

function trendCell(full) {
  const t = trendTone(full);
  return t === "bull"
    ? { tone: "up", label: "Up" }
    : t === "bear"
      ? { tone: "down", label: "Down" }
      : { tone: "neutral", label: "Flat" };
}

const COLS = [
  { key: "trend", label: "Trend", tip: "Trend direction on each timeframe (SMA structure)." },
  { key: "momentum", label: "Mom", tip: TOOLTIPS.rsi },
  { key: "strength", label: "Str", tip: TOOLTIPS.adx },
  { key: "stoch", label: "Stoch", tip: TOOLTIPS.stoch },
];

export default function ConfluenceColumn({ confluence, timeframe, onSelectTimeframe }) {
  const tfs = confluence?.timeframes || [];

  const header = (
    <ColHeader
      title="Confluence"
      tag="Computed · Rules"
      right={
        <Tooltip label={TOOLTIPS.confluence}>
          <span className="text-[11.5px] text-muted/70 underline decoration-dotted underline-offset-2">what's this?</span>
        </Tooltip>
      }
    />
  );

  if (!confluence || !tfs.length) {
    return (
      <div className="flex flex-col">
        {header}
        <Sec divide={false}>
          <p className="text-[12px] text-muted">unavailable (analysis service down)</p>
        </Sec>
      </div>
    );
  }

  const align = confluence.trendAlignment || "—";
  const score = confluence.alignmentScore ?? 0;
  const detail = confluence.alignmentDetail || {};
  const conflicts = confluence.conflicts || [];

  const status = align.startsWith("aligned up")
    ? { cls: "text-up bg-up/12", glyph: "▲", label: align }
    : align.startsWith("aligned down")
      ? { cls: "text-down bg-down/12", glyph: "▼", label: align }
      : { cls: "text-warn bg-warn/12", glyph: null, label: align };

  return (
    <div className="flex flex-col">
      {header}

      {/* Status */}
      <Sec>
        <div className="flex items-center gap-2.5">
          <span
            className={cx(
              "inline-flex items-center gap-1.5 rounded-sm px-2.5 py-1 font-mono text-[12px] font-semibold uppercase leading-none",
              status.cls
            )}
          >
            {status.glyph} {status.label}
          </span>
          <span className="ml-auto num text-[12.5px] text-muted">
            <span className="text-up">▲{detail.up ?? 0}</span> <span className="text-down">▼{detail.down ?? 0}</span> · net{" "}
            <span className={score >= 0 ? "text-accent" : "text-down"}>
              {score >= 0 ? "+" : ""}
              {score}
            </span>
          </span>
        </div>
        <SegmentedMeter value={score} max={Math.max(1, tfs.length)} className="mt-3" />
        {confluence.note && <p className="mt-3 text-[12.5px] leading-relaxed text-muted">{confluence.note}</p>}
      </Sec>

      {/* Matrix */}
      <Sec>
        <div className="mb-3 flex items-baseline justify-between">
          <Micro>Timeframe matrix</Micro>
          <span className="text-[10.5px] text-muted/60">click a row → load</span>
        </div>
        <div className={cx("grid gap-2 px-1 pb-2 text-right label-mono text-muted", GRID)}>
          <span className="text-left">TF</span>
          {COLS.map((c) => (
            <Tooltip key={c.key} label={c.tip} className={c.key === "trend" ? "justify-center" : "justify-end"}>
              <span className="underline decoration-dotted underline-offset-2">{c.label}</span>
            </Tooltip>
          ))}
        </div>
        <div className="flex flex-col gap-1">
          {tfs.map((tf, i) => {
            const active = tf === timeframe;
            const tc = trendCell(confluence.trendByTf?.[tf]);
            const mom = confluence.momentumByTf?.[tf];
            const str = confluence.trendStrengthByTf?.[tf];
            const stoch = confluence.stochByTf?.[tf];
            return (
              <motion.button
                key={tf}
                type="button"
                onClick={() => onSelectTimeframe?.(tf)}
                initial={{ opacity: 0, x: -6 }}
                animate={{ opacity: 1, x: 0 }}
                transition={{ duration: 0.2, delay: i * 0.04 }}
                aria-pressed={active}
                title={`Load ${tf}`}
                className={cx(
                  "grid items-center gap-2 rounded-sm px-1.5 py-2 text-left transition-colors",
                  GRID,
                  active
                    ? "border border-accent/28 bg-accent/[0.06]"
                    : "border border-transparent hover:border-line hover:bg-panel2/50"
                )}
              >
                <span className="num flex items-center gap-1 text-[13px] font-bold text-fg">
                  {tf}
                  {active && <span className="text-accent">●</span>}
                </span>
                <span
                  className={cx(
                    "rounded-sm py-1 text-center font-mono text-[11.5px] font-semibold",
                    TREND_PILL[tc.tone]
                  )}
                >
                  {tc.label}
                </span>
                <span className={cx("num text-right text-[12.5px]", oscColor(mom))}>{numIn(mom, /RSI\s+([\d.]+)/) ?? "—"}</span>
                <span className={cx("num text-right text-[12.5px]", strColor(str))}>{numIn(str, /ADX\s+([\d.]+)/) ?? "—"}</span>
                <span className={cx("num text-right text-[12.5px]", oscColor(stoch))}>{numIn(stoch, /%K\s+([\d.]+)/) ?? "—"}</span>
              </motion.button>
            );
          })}
        </div>
      </Sec>

      {/* Conflicts */}
      <Sec divide={false}>
        {conflicts.length > 0 ? (
          <>
            <Micro tone="warn" className="mb-2.5 flex items-center gap-1.5"><Icon name="warning" size={11} /> Conflicts</Micro>
            <div className="flex flex-col gap-2">
              {conflicts.map((c, i) => (
                <span
                  key={i}
                  className="rounded-sm border border-warn/22 bg-warn/[0.08] px-2.5 py-2 text-[12px] text-warn/90"
                >
                  {c}
                </span>
              ))}
            </div>
          </>
        ) : (
          <div className="flex items-center gap-1.5 text-[12.5px] text-up"><Icon name="check" size={13} /> Timeframes agree — no trend conflicts.</div>
        )}
      </Sec>
    </div>
  );
}
