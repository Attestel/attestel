import { cx } from "../../lib/cx.js";
import {
  Gauge,
  RSI_ZONES,
  RSI_TICKS,
  STOCH_ZONES,
  STOCH_TICKS,
  ADX_ZONES,
  ADX_TICKS,
  Sparkline,
  Tooltip,
} from "../ui/index.js";
import { TOOLTIPS, capFirst, trendTone } from "../../lib/indicatorInfo.js";
import { ColHeader, Micro, Sec } from "./bits.jsx";

// COL 2 — Regime: the deterministic, computed evidence (raw facts beside the LLM prose). Verdict
// chip + Momentum / Trend / Volatility / Volume clusters with gauges, sparklines and an MA ladder.
// Never model-invented — "computed · rules".

const lastVal = (arr) => {
  for (let i = (arr?.length || 0) - 1; i >= 0; i--) if (arr[i]?.value != null) return arr[i].value;
  return null;
};

const VERDICT_CHIP = {
  bull: "text-up bg-up/[0.08] border-up/22",
  bear: "text-down bg-down/[0.08] border-down/22",
  neutral: "text-fg bg-panel2 border-line2",
};

// MA ladder: price + EMA20/SMA50/SMA200 sorted high→low, each with distance from price.
function MaLadder({ lastClose, mas }) {
  const rows = [
    { name: "Price", val: lastClose, isPrice: true },
    { name: "EMA20", val: mas?.ema20 },
    { name: "SMA50", val: mas?.sma50 },
    { name: "SMA200", val: mas?.sma200 },
  ]
    .filter((r) => r.val != null && Number.isFinite(Number(r.val)))
    .sort((a, b) => b.val - a.val);

  return (
    <div className="flex flex-col">
      {rows.map((r) => {
        const dist = lastClose && !r.isPrice ? (lastClose / r.val - 1) * 100 : null;
        return (
          <div
            key={r.name}
            className={cx(
              "flex items-center justify-between px-2 py-2",
              r.isPrice
                ? "-mx-1 border-l-2 border-accent bg-accent/[0.06] px-3"
                : "border-b border-line/60 last:border-0"
            )}
          >
            <span className={cx("num text-[13px]", r.isPrice ? "font-bold text-accent" : "text-muted")}>{r.name}</span>
            <span className="flex items-baseline gap-3.5">
              <span className={cx("num text-[14px]", r.isPrice ? "font-bold text-fg" : "font-semibold text-fg")}>
                {Number(r.val).toFixed(2)}
              </span>
              {dist != null ? (
                <span className={cx("num w-12 text-right text-[12px]", dist >= 0 ? "text-accent" : "text-down")}>
                  {dist >= 0 ? "+" : ""}
                  {dist.toFixed(1)}%
                </span>
              ) : (
                <span className="w-12 text-right label-mono text-muted">last</span>
              )}
            </span>
          </div>
        );
      })}
    </div>
  );
}

export default function RegimeColumn({ regime, indicators }) {
  const header = <ColHeader title="Regime" tag="Computed · Rules" />;
  if (!regime) {
    return (
      <div className="flex flex-col">
        {header}
        <Sec divide={false}>
          <p className="text-[12px] text-muted">unavailable</p>
        </Sec>
      </div>
    );
  }

  const L = indicators?.latest || {};
  const S = indicators?.series || {};
  const rsiSeries = (S.rsi || []).map((p) => p.value).filter((v) => v != null).slice(-48);
  const macdSeries = (S.macdHist || []).map((p) => p.value).filter((v) => v != null).slice(-48);

  // Bollinger band edges — prefer the series, else parse "inside bands (lo – hi)".
  let bbLower = lastVal(S.bbLower);
  let bbUpper = lastVal(S.bbUpper);
  if (bbLower == null || bbUpper == null) {
    const m = String(regime.priceVsBands || "").match(/([\d.]+)\s*[–-]\s*([\d.]+)/);
    if (m) {
      bbLower = Number(m[1]);
      bbUpper = Number(m[2]);
    }
  }
  const bbPos =
    bbLower != null && bbUpper != null && bbUpper > bbLower
      ? Math.min(100, Math.max(0, ((regime.lastClose - bbLower) / (bbUpper - bbLower)) * 100))
      : null;

  const hTone = trendTone(regime.trend);
  const glyph = hTone === "bull" ? "▲" : hTone === "bear" ? "▼" : "●";
  const macdBull = /bullish/i.test(regime.macdState || "");
  const contracting = /contract/i.test(regime.volatility || "");

  return (
    <div className="flex flex-col">
      {header}

      {/* Verdict chip */}
      <Sec>
        <div className={cx("flex flex-wrap items-center gap-x-2 gap-y-1 rounded-lg border px-3.5 py-3", VERDICT_CHIP[hTone])}>
          <span className="text-[13px]">{glyph}</span>
          <span className="text-[14px] font-semibold">{capFirst(regime.trend)}</span>
          <span className="text-[12.5px] text-muted">
            · {capFirst(regime.momentum)} momentum · {capFirst(regime.volatility)} volatility
          </span>
        </div>
      </Sec>

      {/* Momentum */}
      <Sec>
        <Micro className="mb-3.5">Momentum</Micro>
        <div className="grid grid-cols-[1fr_auto] items-end gap-3">
          <Gauge value={L.rsi} zones={RSI_ZONES} ticks={RSI_TICKS} label="RSI" tooltip={TOOLTIPS.rsi} valueDigits={1} />
          <Sparkline data={rsiSeries} width={72} height={26} />
        </div>
        <Gauge
          className="mt-4"
          value={L.stochK}
          zones={STOCH_ZONES}
          ticks={STOCH_TICKS}
          label="Stochastic %K"
          tooltip={TOOLTIPS.stoch}
          valueDigits={1}
          sub={L.stochD != null ? `%D ${Number(L.stochD).toFixed(1)}` : undefined}
        />
        <div className="mt-4 flex items-center justify-between gap-2">
          <Tooltip label={TOOLTIPS.macd} underline>
            <span className="label-mono text-muted">MACD</span>
          </Tooltip>
          <div className="flex items-center gap-2.5">
            <Sparkline data={macdSeries} width={72} height={24} tone={macdBull ? "text-up" : "text-down"} />
            <span
              className={cx(
                "rounded-sm px-2 py-1 font-mono text-[11.5px] font-semibold leading-none",
                macdBull ? "bg-up/12 text-up" : "bg-down/12 text-down"
              )}
            >
              {macdBull ? "▲ Bullish" : "▼ Bearish"}
            </span>
          </div>
        </div>
      </Sec>

      {/* Trend */}
      <Sec>
        <Micro className="mb-3">Trend</Micro>
        <div className="mb-3.5 flex flex-wrap items-center gap-2">
          <span
            className={cx(
              "rounded-md px-2.5 py-1.5 font-mono text-[12px] font-semibold leading-none",
              hTone === "bull" ? "bg-up/12 text-up" : hTone === "bear" ? "bg-down/12 text-down" : "bg-panel2 text-muted"
            )}
          >
            {glyph} {capFirst(regime.trend)}
          </span>
          {regime.shortTermTrend && (
            <Tooltip label={regime.shortTermTrend} side="bottom">
              <span className="rounded-md border border-line2 px-2.5 py-1.5 text-[12px] text-muted">structure ⓘ</span>
            </Tooltip>
          )}
        </div>
        <Gauge
          value={L.adx}
          zones={ADX_ZONES}
          ticks={ADX_TICKS}
          label="ADX · trend strength"
          tooltip={TOOLTIPS.adx}
          valueDigits={1}
          sub={capFirst(regime.trendStrength)}
        />
        <div className="mt-4">
          <Tooltip label={TOOLTIPS.ma} underline>
            <Micro className="mb-2">MA ladder</Micro>
          </Tooltip>
          <MaLadder lastClose={regime.lastClose} mas={regime.movingAverages} />
        </div>
      </Sec>

      {/* Volatility + Volume */}
      <Sec divide={false} className="grid grid-cols-2 gap-0 !px-0 !py-0">
        <div className="border-r border-line px-[18px] py-4">
          <Micro className="mb-3">Volatility</Micro>
          <div className="flex items-baseline justify-between">
            <Tooltip label={TOOLTIPS.atr} underline>
              <span className="text-[13px] text-muted">ATR</span>
            </Tooltip>
            <span className="num text-[16px] font-bold text-fg">{L.atr != null ? Number(L.atr).toFixed(2) : "—"}</span>
          </div>
          <div className="mt-1 font-mono text-[11.5px] text-info">● {capFirst(regime.volatility)}</div>
          <Micro className="mb-2 mt-4">Price vs bands</Micro>
          <div className="relative h-1.5 rounded-full bg-gradient-to-r from-accent via-panel2 to-down">
            {bbPos != null && (
              <span
                className="absolute top-1/2 h-[11px] w-[11px] -translate-x-1/2 -translate-y-1/2 rounded-full border-2 border-bg bg-fg"
                style={{ left: `${bbPos}%` }}
                aria-hidden="true"
              />
            )}
          </div>
          <div className="mt-1.5 flex justify-between font-mono text-[11px] text-muted">
            <span>{bbLower != null ? Number(bbLower).toFixed(2) : "—"}</span>
            <span className="text-muted/60">mid</span>
            <span>{bbUpper != null ? Number(bbUpper).toFixed(2) : "—"}</span>
          </div>
        </div>
        <div className="px-[18px] py-4">
          <Micro className="mb-3">Volume</Micro>
          <div className="mb-2.5 flex items-center gap-2 rounded-md border border-line bg-panel2 px-2.5 py-1.5">
            <span className="h-1.5 w-1.5 rounded-full bg-muted" />
            <span className="text-[13px] text-fg">{capFirst(regime.volumeTrend)}</span>
          </div>
          <Tooltip label={regime.volume} side="bottom">
            <span className="text-[12px] text-muted">{capFirst(regime.volume)} vs 20-day ⓘ</span>
          </Tooltip>
          {regime.vwapState && (
            <div className="mt-3">
              <Tooltip label={TOOLTIPS.vwap} underline>
                <Micro className="mb-1">VWAP</Micro>
              </Tooltip>
              <div className="text-[12px] text-fg">{capFirst(regime.vwapState)}</div>
            </div>
          )}
        </div>
      </Sec>
    </div>
  );
}
