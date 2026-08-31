import { useEffect, useState } from "react";
import { fetchDashboard, fetchPrediction } from "../lib/api.js";
import { Sparkline, Skeleton } from "./ui/index.js";
import { Tag } from "./terminal/bits.jsx";
import { fmtSignedPct } from "../lib/format.js";
import { cx } from "../lib/cx.js";

// Watchlist — redesigned as a dense "attestel" (design_examples/2b-watchlist): one bordered container,
// a column header, and one row per name (symbol · 1D spark · last · chg% · trend · signal). Clicking
// a row opens that company in Research. The SIGNAL column shows only "SETUP" / "NO SIGNAL" — never
// a bare Buy/Sell — so the guardrail holds: a direction appears only as model evidence, with its
// track record. Each row fetches its own dashboard + prediction; failures fail soft per-row.

const GRID = "grid-cols-[minmax(150px,180px)_minmax(120px,1fr)_110px_84px_116px_92px]";

function trend(t) {
  const s = String(t || "").toLowerCase();
  if (s.includes("up")) return { cls: "text-up bg-up/12", label: "▲ Uptrend" };
  if (s.includes("down")) return { cls: "text-down bg-down/12", label: "▼ Downtrend" };
  return { cls: "text-muted bg-panel2", label: "● Neutral" };
}

function Row({ t, active, timeframe, onSelect }) {
  const [st, setSt] = useState({ loading: true });

  useEffect(() => {
    let alive = true;
    (async () => {
      const [d, p] = await Promise.allSettled([
        fetchDashboard(t.symbol, timeframe),
        fetchPrediction(t.symbol, timeframe),
      ]);
      if (!alive) return;
      setSt({
        loading: false,
        data: d.status === "fulfilled" ? d.value : null,
        pred: p.status === "fulfilled" ? p.value : null,
        error: d.status !== "fulfilled",
      });
    })();
    return () => {
      alive = false;
    };
  }, [t.symbol, timeframe]);

  const data = st.data;
  const price = data?.price;
  const closes = (price?.ohlcv || []).map((b) => b.close).slice(-40);
  const chg = price?.changePct;
  const hasSignal = Boolean(st.pred?.signal);
  const synthetic = data?.priceSourceIsSynthetic;
  const tr = trend(data?.regime?.trend);
  const chgTone = chg == null || Math.abs(chg) < 0.005 ? "text-muted" : chg > 0 ? "text-accent" : "text-down";
  const sparkTone = chg == null || Math.abs(chg) < 0.005 ? "text-muted" : chg > 0 ? "text-accent" : "text-down";

  return (
    <button
      type="button"
      onClick={() => onSelect(t.symbol)}
      title={`Open ${t.symbol} in Research`}
      className={cx(
        "grid w-full items-center gap-4 border-b border-l-2 border-b-line/60 px-[22px] py-3.5 text-left transition-colors",
        GRID,
        active ? "border-l-accent bg-accent/[0.04]" : "border-l-transparent hover:bg-panel2/40"
      )}
    >
      {/* Symbol */}
      <div className="min-w-0">
        <div className="flex items-center gap-1.5">
          <span className="num text-[16px] font-bold text-fg">{t.symbol}</span>
          {synthetic && <Tag tone="warn">syn</Tag>}
        </div>
        <div className="mt-0.5 truncate text-[12px] text-muted">{t.name}</div>
      </div>

      {/* Sparkline */}
      {st.loading ? (
        <Skeleton className="h-10 w-full" />
      ) : (
        <Sparkline data={closes} width={320} height={40} className="w-full" fill={active} tone={sparkTone} />
      )}

      {/* Last */}
      {st.loading ? (
        <Skeleton className="h-5 w-16 justify-self-end" />
      ) : (
        <span className="num justify-self-end text-[17px] font-bold text-fg">
          {price?.lastClose != null ? Number(price.lastClose).toFixed(2) : "—"}
        </span>
      )}

      {/* Chg% */}
      {st.loading ? (
        <Skeleton className="h-4 w-12 justify-self-end" />
      ) : (
        <span className={cx("num justify-self-end text-[14px] font-semibold", chgTone)}>{fmtSignedPct(chg, 2)}</span>
      )}

      {/* Trend */}
      {st.loading ? (
        <Skeleton className="h-6 w-20" />
      ) : (
        <span className={cx("justify-self-start rounded-md px-2.5 py-1.5 font-mono text-[12px] font-semibold leading-none", tr.cls)}>
          {tr.label}
        </span>
      )}

      {/* Signal — SETUP / NO SIGNAL only (never a bare Buy/Sell) */}
      {st.loading ? (
        <Skeleton className="h-6 w-16 justify-self-end" />
      ) : hasSignal ? (
        <span className="justify-self-end rounded-md bg-llm/15 px-2.5 py-1.5 label-mono text-llm">
          Setup
        </span>
      ) : (
        <span className="justify-self-end rounded-md border border-line2 px-2.5 py-1.5 text-center label-mono text-muted">
          No signal
        </span>
      )}
    </button>
  );
}

export default function Watchlist({ tickers, onSelect, activeTicker, timeframe = "1D" }) {
  return (
    <section className="overflow-hidden surface-card">
      {/* Title bar */}
      <div className="flex h-11 items-center gap-3 border-b border-line px-[22px]">
        <span className="text-[14px] font-semibold text-fg">Watchlist</span>
        <Tag tone="outline">
          {tickers.length} {tickers.length === 1 ? "name" : "names"} · {timeframe}
        </Tag>
        <span className="ml-auto text-[11.5px] text-muted/60">click a row → open Research</span>
      </div>

      {/* Scrollable table (keeps the dense grid intact on narrow screens) */}
      <div className="overflow-x-auto">
        <div className="min-w-[820px]">
          {/* Column header */}
          <div
            className={cx(
              "grid items-center gap-4 border-b border-l-2 border-l-transparent border-b-line px-[22px] py-2.5 label-mono text-muted",
              GRID
            )}
          >
            <span>Symbol</span>
            <span>{timeframe} spark</span>
            <span className="justify-self-end">Last</span>
            <span className="justify-self-end">Chg%</span>
            <span>Trend</span>
            <span className="justify-self-end">Signal</span>
          </div>

          {/* Rows */}
          {tickers.map((t) => (
            <Row
              key={t.symbol}
              t={t}
              active={t.symbol === activeTicker}
              timeframe={timeframe}
              onSelect={onSelect}
            />
          ))}

          {/* Add row — the universe is server-configured; shown for parity with the design */}
          <div className="flex items-center gap-2.5 px-[22px] py-3.5 text-muted/60">
            <svg width="15" height="15" viewBox="0 0 24 24" fill="none" aria-hidden="true">
              <path d="M12 5v14M5 12h14" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" />
            </svg>
            <span className="text-[13px]">Add a symbol to the watchlist</span>
          </div>
        </div>
      </div>
    </section>
  );
}
