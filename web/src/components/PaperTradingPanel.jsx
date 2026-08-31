import { useEffect, useState, useCallback } from "react";
import {
  paperDashboard, paperReadiness, setPaperConfig, paperReset, fmtMoney,
} from "../lib/api.js";
import { useToast } from "./ui/index.js";
import { Tag } from "./terminal/bits.jsx";
import { cx } from "../lib/cx.js";
import { Icon } from "./shell/icons.jsx";

// PaperTradingPanel — the monitoring surface for the ACTUAL experiment
// (docs/PAPER_EXECUTION_CONTRACT.md).
//
// WHAT THIS REPLACED, AND WHY. The old panel described a system that no longer exists: it said
// positions were "held for exactly the backtest's horizon" and rendered a "Bars left" countdown to a
// fixed-horizon exit. The engine has been a PER-BAR ALLOCATION RULE since contract v1.1.0 — it
// re-decides its target every completed bar and closes when the target changes — so the prose was
// false and the column served "—" for a rule that does not exist. Meanwhile everything the
// experiment is actually made of reached no surface at all: the four gates and which one refused,
// the last decision, the three-store sync state, and the whole fake-money book (§5) behind
// `GET /paper/ledger`.
//
// THE HONEST-EMPTY RULE. The engine may trade nothing because a launch gate refuses a config. That
// is the system WORKING, and this panel has to read that way. An empty book with no explanation is
// indistinguishable from a broken service — so the refusing gate and its reason are the headline,
// not an error state.
//
// SIMULATION ONLY — no execution, no broker, no money movement.

const POLL_MS = 30000;
const pct = (n, d = 2) => (n == null ? "—" : `${(n * 100).toFixed(d)}%`);
const num = (n, d = 2) => (n == null ? "—" : Number(n).toFixed(d));
const money = (n) => (n == null ? "—" : `$${Number(n).toLocaleString("en-US", { maximumFractionDigits: 2 })}`);
const cap = (s) => (s ? s.charAt(0).toUpperCase() + s.slice(1) : s);

const colHead = "label-mono text-muted";
const GATE_ORDER = ["no-synthetic-data", "fresh-data", "backtest-passed", "evaluator-verdict"];

function SecHeader({ title, borderTop, children }) {
  return (
    <div className={cx("flex h-11 items-center gap-2.5 border-b border-line px-[22px]", borderTop && "border-t border-t-line")}>
      <span className="text-[14px] font-semibold text-fg">{title}</span>
      {children}
    </div>
  );
}

function CountPill({ n, tone = "auto" }) {
  const on = tone === "auto" ? n > 0 : tone === "on";
  return (
    <span className={cx("num rounded-full px-2 py-0.5 text-[12px] font-semibold leading-none", on ? "bg-accent text-bg" : "bg-line text-muted")}>
      {n}
    </span>
  );
}

// Stat is one number from the book, with its unit spelled out underneath. A number with no unit is
// how the live column and the backtest column got compared as though they were the same thing.
function Stat({ label, value, unit, tone }) {
  return (
    <div className="min-w-0">
      <div className={colHead}>{label}</div>
      <div className={cx("num mt-1 text-[15px] font-semibold tabular-nums", tone || "text-fg")}>{value}</div>
      {unit && <div className="mt-0.5 text-[11px] leading-snug text-muted/70">{unit}</div>}
    </div>
  );
}

// GateDot renders one gate's verdict. `undefined` (no decision recorded yet) is deliberately its own
// state — "not evaluated" is not "passed".
function GateDot({ gate, name }) {
  const state = gate == null ? "unknown" : gate.ok ? "pass" : "refuse";
  return (
    <span
      title={gate?.detail || "this gate has not been evaluated yet"}
      className={cx(
        "inline-flex items-center gap-1.5 rounded-sm px-2 py-1 font-mono text-[11px]",
        state === "pass" && "bg-accent/12 text-accent",
        state === "refuse" && "bg-warn/12 text-warn",
        state === "unknown" && "bg-line text-muted"
      )}
    >
      <span className="text-[10px]">{state === "pass" ? "✓" : state === "refuse" ? "✕" : "·"}</span>
      {name}
    </span>
  );
}

// EquityCurve is a dependency-free sparkline of the book's daily snapshot equity. It draws only what
// was MEASURED: gap dates produce no snapshot and therefore no point, which is the whole reason the
// gap list is rendered beside it rather than smoothed into the line.
function PerformanceChart({ series }) {
  if (!series?.length) return null;
  const measured = series.map((p, i) => ({ ...p, i })).filter((p) => p.equity != null);
  if (!measured.length) return null;
  const equities = measured.map((p) => Number(p.equity));
  const min = Math.min(...equities);
  const max = Math.max(...equities);
  const span = max - min || 1;
  const x = (i) => series.length === 1 ? 50 : (i / (series.length - 1)) * 100;
  const equityPoints = measured
    .map((p) => `${x(p.i)},${30 - ((Number(p.equity) - min) / span) * 28}`)
    .join(" ");
  const maxDrawdown = Math.max(...measured.map((p) => Number(p.drawdown) || 0), 0.000001);
  const drawdownPoints = measured
    .map((p) => `${x(p.i)},${36 + ((Number(p.drawdown) || 0) / maxDrawdown) * 12}`)
    .join(" ");
  const up = equities[equities.length - 1] >= equities[0];
  const gaps = series.map((p, i) => ({ ...p, i })).filter((p) => p.gap);
  return (
    <div>
      <div className="mb-1 flex items-center justify-between text-[10px] font-mono uppercase tracking-wide text-muted/60">
        <span>Equity</span><span>Drawdown</span>
      </div>
      <svg viewBox="0 0 100 50" preserveAspectRatio="none" className="h-[92px] w-full" role="img"
        aria-label="Dated simulated equity and drawdown series">
        <line x1="0" y1="34" x2="100" y2="34" className="stroke-line" strokeWidth="0.5" />
        {gaps.map((p) => (
          <line key={`gap-${p.date}`} x1={x(p.i)} y1="1" x2={x(p.i)} y2="49"
            className="stroke-warn/70" strokeWidth="0.6" strokeDasharray="1.5 1.5" />
        ))}
        <polyline points={equityPoints} fill="none" strokeWidth="1.25" vectorEffect="non-scaling-stroke"
          className={up ? "stroke-accent" : "stroke-down"} />
        <polyline points={drawdownPoints} fill="none" strokeWidth="1" vectorEffect="non-scaling-stroke"
          className="stroke-down/80" />
      </svg>
      <div className="mt-1 flex justify-between font-mono text-[10px] text-muted/60">
        <span>{series[0]?.date || "—"}</span>
        {gaps.length > 0 && <span className="text-warn">dashed = missing mark</span>}
        <span>{series[series.length - 1]?.date || "—"}</span>
      </div>
    </div>
  );
}

function DimensionCard({ label, value, note, tone = "neutral" }) {
  return (
    <div className="rounded-lg border border-line bg-panel2/35 px-3.5 py-3">
      <div className={colHead}>{label}</div>
      <div className={cx(
        "mt-1.5 font-mono text-[13px] font-semibold uppercase tracking-wide",
        tone === "good" && "text-accent",
        tone === "warn" && "text-warn",
        tone === "bad" && "text-down",
        tone === "neutral" && "text-fg/85",
      )}>{String(value || "unknown").replaceAll("-", " ")}</div>
      {note && <div className="mt-1 text-[11px] leading-snug text-muted/70">{note}</div>}
    </div>
  );
}

export default function PaperTradingPanel() {
  const [dashboard, setDashboard] = useState(null);
  const [status, setStatus] = useState(null);
  const [ledger, setLedger] = useState(null);
  const [ledgerErr, setLedgerErr] = useState(null);
  const [comparison, setComparison] = useState([]);
  const [units, setUnits] = useState({});
  const [minMeaningful, setMinMeaningful] = useState(30);
  const [synthetic, setSynthetic] = useState(false);
  const [down, setDown] = useState(false);
  const [resetting, setResetting] = useState(false);
  const [readiness, setReadiness] = useState(null);
  const [readinessErr, setReadinessErr] = useState("");
  const [checking, setChecking] = useState(false);
  const [editingConfigs, setEditingConfigs] = useState(false);
  const [configDraft, setConfigDraft] = useState("");
  const [savingConfigs, setSavingConfigs] = useState(false);
  const toast = useToast();

  const checkReadiness = useCallback(async () => {
    setChecking(true);
    try {
      setReadiness(await paperReadiness());
      setReadinessErr("");
    } catch (e) {
      setReadiness(null);
      setReadinessErr(e?.message || "launch readiness could not be checked");
    } finally {
      setChecking(false);
    }
  }, []);

  const refresh = useCallback(async () => {
    try {
      const dash = await paperDashboard(252);
      setDashboard(dash);
      setStatus(dash.status || null);
      setDown(false);
      if (dash.ledger?.equity != null) {
        setLedger(dash.ledger);
        setLedgerErr(null);
      } else {
        setLedger(null);
        setLedgerErr(dash.ledger?.note || "the fake-money ledger is not running");
      }
      const cmp = dash.comparison || {};
      setComparison(cmp.comparisons || []);
      setUnits(cmp.units || {});
      setMinMeaningful(cmp.minMeaningful || 30);
      setSynthetic(Boolean(cmp.synthetic));
    } catch {
      setDown(true);
    }
  }, []);

  useEffect(() => {
    refresh();
    checkReadiness();
    const id = setInterval(refresh, POLL_MS);
    return () => clearInterval(id);
  }, [refresh, checkReadiness]);

  async function saveConfigs() {
    setSavingConfigs(true);
    try {
      const result = await setPaperConfig(configDraft);
      setEditingConfigs(false);
      await refresh();
      await checkReadiness();
      toast({
        tone: result.officialClockInvalidated ? "warn" : "info",
        title: "Paper scope saved in PostgreSQL",
        message: result.note,
      });
    } catch (e) {
      toast({ tone: "warn", title: "Paper scope was not changed", message: e?.message });
    } finally {
      setSavingConfigs(false);
    }
  }

  async function doReset() {
    if (!window.confirm(
      "RESET is the official experiment start: it deletes every simulated paper trade, clears the " +
      "engine's state and archives the book, then the clock starts again from zero. Continue?"
    )) return;
    setResetting(true);
    try {
      const r = await paperReset();
      await refresh();
      await checkReadiness();
      toast({
        tone: "info",
        title: `Official clock started — ${r.officialStartedAt || "day 0"}`,
        message: `${r.deletedPaperTrades ?? 0} setup paper trades archived/deleted from the active book.`,
      });
    } catch (e) {
      if (e?.data?.readiness) setReadiness(e.data.readiness);
      // A reset that could not delete every journal trade wipes NOTHING and says which ones failed.
      toast({ tone: "warn", title: "Reset aborted — nothing was wiped", message: e?.message });
    } finally {
      setResetting(false);
    }
  }

  if (down) {
    return (
      <section className="overflow-hidden surface-card">
        <SecHeader title="Paper trading">
          <Tag tone="warn">service down</Tag>
        </SecHeader>
        <p className="px-[22px] py-5 text-[13px] leading-relaxed text-muted">
          The paper service (<code className="rounded bg-panel2 px-1">:8097</code>) is unreachable — the rest of the app is
          unaffected. Start it with <code className="rounded bg-panel2 px-1">cd paper &amp;&amp; go run .</code>
        </p>
      </section>
    );
  }

  const configs = status?.configs || [];
  const book = status?.book || null;
  const recon = status?.reconciliation || null;
  const metrics = ledger?.metrics || null;
  const snapshots = ledger?.snapshots || null;
  const gapDates = ledger?.gapDates || [];
  const positions = ledger?.positions || [];
  const officialStartedAt = ledger?.officialStartedAt || "";
  const refusing = configs.filter((c) => c.lastDecision?.gate);
  const allRefused = configs.length > 0 && refusing.length === configs.length;
  const blockers = readiness?.blockers || [];
  const dimensions = dashboard?.dimensions || {};
  const progress = dashboard?.progress || {};
  const history = dashboard?.decisionHistory || {};
  const shadow = dashboard?.shadow || {};
  const shadowRows = shadow.rows || [];
  const shadowRecent = shadow.recent || [];

  const dimensionTone = (kind, value) => {
    if (kind === "clock") return value === "running" ? "good" : "warn";
    if (kind === "integrity") return value === "healthy" ? "good" : "bad";
    if (kind === "sample") return value === "measurable" ? "good" : value === "collecting" ? "warn" : "neutral";
    if (kind === "result") return value === "tracking" ? "good" : value === "divergence" ? "bad" : "neutral";
    return "neutral";
  };

  return (
    <section className="overflow-hidden surface-card">
      <SecHeader title="Paper trading — the validation experiment">
        <Tag tone="outline">Simulation</Tag>
        {dashboard?.generation != null && <Tag tone="outline">generation {dashboard.generation}</Tag>}
        {book && !book.perUser && <Tag tone="outline">not your book</Tag>}
        {recon?.desyncedConfigs?.length > 0 && <Tag tone="down">desynced</Tag>}
      </SecHeader>

      {/* ---- Summary first: what state is this experiment actually in? ---- */}
      <div className="border-b border-line px-[22px] py-4">
        <div className="grid grid-cols-2 gap-2 lg:grid-cols-4">
          <DimensionCard label="Official clock" value={dimensions.clock}
            tone={dimensionTone("clock", dimensions.clock)}
            note={officialStartedAt ? `Day 0 ${officialStartedAt}` : "No evidence-bearing day 0 yet"} />
          <DimensionCard label="Integrity" value={dimensions.integrity}
            tone={dimensionTone("integrity", dimensions.integrity)}
            note={(dimensions.integrityReasons || [])[0] || "Stores, marks and provenance agree"} />
          <DimensionCard label="Sample" value={dimensions.sample}
            tone={dimensionTone("sample", dimensions.sample)}
            note={`${progress.snapshotDays || 0}/${progress.sharpeRequired || 20} measured snapshot days`} />
          <DimensionCard label="Result" value={dimensions.result}
            tone={dimensionTone("result", dimensions.result)}
            note="Never interpreted as permission to use real money" />
        </div>

        <div className="mt-3 grid grid-cols-2 gap-x-6 gap-y-3 rounded-lg border border-line bg-panel2/25 px-4 py-3 sm:grid-cols-5">
          <Stat label="Measured days" value={`${progress.snapshotDays || 0}/${progress.sharpeRequired || 20}`}
            unit="minimum before daily Sharpe" />
          <Stat label="Coverage" value={progress.coverage == null ? "UNMEASURED" : pct(progress.coverage, 1)}
            unit={`${progress.gapDates || 0} missing complete-book marks`} />
          <Stat label="Decisions" value={progress.settledDecisions ?? 0} unit="settled completed bars" />
          <Stat label="Closed episodes" value={`${progress.closedEpisodes || 0}/${progress.countingStatsRequired || 30}`}
            unit="only for per-episode counting stats" />
          <Stat label="As of" value={dashboard?.asOf ? dashboard.asOf.slice(0, 10) : "—"}
            unit="server-composed evidence snapshot" />
        </div>

        {(dimensions.integrityReasons || []).length > 1 && (
          <ul className="mt-3 list-disc space-y-1 pl-5 text-[11.5px] leading-relaxed text-warn">
            {dimensions.integrityReasons.slice(1).map((reason) => <li key={reason}>{reason}</li>)}
          </ul>
        )}
      </div>

      {/* ---- Independent first-seen signal evidence; never an official-gate bypass ---- */}
      <SecHeader title="Experimental shadow signals" borderTop>
        <Tag tone={shadow.recording === false ? "warn" : "outline"}>
          {shadow.recording === false ? "recording degraded" : shadow.contractVersion || "experimental"}
        </Tag>
        <Tag tone="outline">no real money</Tag>
      </SecHeader>
      <div className="border-b border-line px-[22px] py-4">
        <div className="rounded-lg border border-accent/25 bg-accent/[0.06] px-4 py-3 text-[12.5px] leading-[1.6] text-muted">
          <strong className="font-semibold text-fg/85">This ledger records calls the official book refuses.</strong>{" "}
          The first Buy, Sell or Hold and its contemporaneous quote are frozen for each completed bar, then measured
          after 1, 3, 5 and 10 bars. It cannot open a journal trade, satisfy an EDGE gate, promote a model or change the
          official paper account.
          {shadow.error && <span className="mt-1 block text-warn">Shadow storage: {shadow.error}</span>}
        </div>

        <div className="mt-3 grid grid-cols-2 gap-x-6 gap-y-3 rounded-lg border border-line bg-panel2/25 px-4 py-3 sm:grid-cols-5">
          <Stat label="Signals observed" value={shadow.observations ?? 0}
            unit="first signal per config and completed bar" />
          <Stat label="Executable quotes" value={`${shadow.executable ?? 0}/${shadow.observations ?? 0}`}
            unit="real, source-labelled and temporally usable" />
          <Stat label="Settled outcomes" value={shadow.eligibleOutcomes ?? 0}
            unit={`${shadow.settledOutcomes ?? 0} total including excluded data`} />
          <Stat label="Directions" value={`${shadow.directions?.Buy || 0} / ${shadow.directions?.Sell || 0} / ${shadow.directions?.Hold || 0}`}
            unit="Buy / Sell / Hold" />
          <Stat label="Episode notional" value={money(shadow.episodeNotional)}
            unit="fixed explanatory amount; episodes may overlap" />
        </div>

        {shadowRows.length === 0 ? (
          <p className="mt-3 text-[12.5px] leading-relaxed text-muted">
            No fixed-horizon outcome has matured yet. Signal observations appear immediately; the first H1 row appears
            only after the next completed real bar.
          </p>
        ) : (
          <div className="mt-3 overflow-x-auto rounded-lg border border-line">
            <div className="min-w-[820px] px-4">
              <div className={cx("grid grid-cols-[1.4fr_.7fr_.8fr_.9fr_1fr_1fr] gap-4 border-b border-line py-2.5", colHead)}>
                <span>Model config</span><span>Outcome</span><span>Settled</span><span>Correct</span>
                <span>Mean after costs</span><span className="justify-self-end">Episode P&amp;L</span>
              </div>
              {shadowRows.map((row, i) => (
                <div key={`${row.config}-${row.outcomeHorizon}`}
                  className={cx("grid grid-cols-[1.4fr_.7fr_.8fr_.9fr_1fr_1fr] items-center gap-4 py-3 text-[12px]",
                    i < shadowRows.length - 1 && "border-b border-line/50")}>
                  <span className="num text-fg">{row.ticker} · {row.timeframe} · H{row.modelHorizon}</span>
                  <span className="num text-fg/80">+{row.outcomeHorizon} bars</span>
                  <span className="num text-fg/80">{row.nSettled}</span>
                  <span className="num text-fg/80">
                    {row.directionalAccuracy == null ? "UNMEASURED" : `${row.nCorrect}/${row.nDirectional} · ${pct(row.directionalAccuracy, 1)}`}
                  </span>
                  <span className={cx("num", Number(row.meanEpisodeReturn) > 0 ? "text-accent" : Number(row.meanEpisodeReturn) < 0 ? "text-down" : "text-fg/80")}>
                    {row.meanEpisodeReturn == null ? "—" : pct(row.meanEpisodeReturn, 3)}
                  </span>
                  <span className={cx("num justify-self-end", Number(row.episodePnl) > 0 ? "text-accent" : Number(row.episodePnl) < 0 ? "text-down" : "text-fg/80")}>
                    {fmtMoney(row.episodePnl)}
                  </span>
                </div>
              ))}
            </div>
          </div>
        )}

        {shadowRecent.length > 0 && (
          <div className="mt-3 overflow-x-auto rounded-lg border border-line">
            <div className="min-w-[780px] px-4">
              <div className={cx("grid grid-cols-[1.25fr_.8fr_.8fr_1fr_1.2fr] gap-4 border-b border-line py-2.5", colHead)}>
                <span>First observed bar</span><span>Signal</span><span>Quote</span><span>Evaluator</span><span>Forward outcomes</span>
              </div>
              {shadowRecent.slice(0, 12).map(({ observation, outcomes }, i) => (
                <div key={`${observation.config}-${observation.signalBarUnix}`}
                  className={cx("grid grid-cols-[1.25fr_.8fr_.8fr_1fr_1.2fr] items-start gap-4 py-3 text-[12px]",
                    i < Math.min(shadowRecent.length, 12) - 1 && "border-b border-line/50")}>
                  <span>
                    <span className="num block text-fg">{observation.config}</span>
                    <span className="num block text-muted">{observation.signalBar}</span>
                  </span>
                  <span className={cx("font-mono font-semibold", observation.direction === "Buy" ? "text-accent" : observation.direction === "Sell" ? "text-down" : "text-muted")}>
                    {observation.direction} · {pct(observation.confidence)}
                  </span>
                  <span className="num text-fg/80">
                    {observation.entryEligible ? money(observation.entryPrice) : <Tag tone="warn">excluded</Tag>}
                  </span>
                  <span>{observation.evaluation || "none"}{observation.evaluationCurrent ? " · current" : ""}</span>
                  <span className="flex flex-wrap gap-1">
                    {(outcomes || []).length === 0 ? <span className="text-muted">pending</span> : outcomes.map((outcome) => (
                      <Tag key={outcome.horizon} tone={!outcome.eligible ? "warn" : outcome.correct === true ? "ok" : outcome.correct === false ? "down" : "outline"}>
                        H{outcome.horizon} {outcome.eligible ? pct(outcome.strategyReturn, 2) : "excluded"}
                      </Tag>
                    ))}
                  </span>
                </div>
              ))}
            </div>
          </div>
        )}

        <p className="mt-3 text-[11px] leading-relaxed text-muted/60">{shadow.note}</p>
      </div>

      {/* ---- What this is, and whose ---- */}
      <div className="border-b border-line px-[22px] py-4">
        <div className="rounded-lg border border-warn/30 bg-warn/[0.08] px-4 py-3 text-[13px] leading-[1.6] text-warn/90">
          <strong className="inline-flex items-center gap-1.5 font-bold text-warn">
            <Icon name="pencil" size={14} /> PAPER — simulated, no real money.
          </strong>{" "}
          The engine re-decides a target position on <strong>every completed bar</strong> and closes when the target
          changes — there is no holding period and no scheduled exit. Nothing here executes an order or touches a broker.
        </div>

        {book && !book.perUser && (
          <div className="mt-3 rounded-lg border border-line2 bg-panel2/60 px-4 py-3 text-[12.5px] leading-[1.6] text-muted">
            <strong className="font-semibold text-fg/85">
              This is the {book.label || "platform validation engine"}&rsquo;s book, not yours.
            </strong>{" "}
            {book.note || "Simulated positions opened by the platform's validation engine — not your own trades."}{" "}
            Your own records live under Journal &rarr; Trades and Journal &rarr; Decisions, and are never mixed with these.
            {book.recordingNote && <span className="mt-2 block text-warn">{book.recordingNote}</span>}
          </div>
        )}

        {synthetic && (
          <div className="mt-3 rounded-lg border border-warn/60 bg-warn/[0.12] px-4 py-3 text-[13px] leading-[1.6] text-warn">
            <strong className="inline-flex items-center gap-1.5 font-bold">
              <Icon name="warning" size={14} /> SYNTHETIC — this is not validation.
            </strong>{" "}
            A model trained on invented prices cannot be validated by anything. Gate 1 refuses to trade it, which is why
            the book below is empty.
          </div>
        )}

        {/* The single most important sentence on this panel when it is true. */}
        {allRefused && (
          <div className="mt-3 rounded-lg border border-line2 bg-panel2/60 px-4 py-3 text-[12.5px] leading-[1.6] text-muted">
            <strong className="font-semibold text-fg/85">Every config is refused, and that is the system working.</strong>{" "}
            The engine is fail-closed behind four gates. Until all four pass for a config it takes no position, records
            which gate refused and why, and keeps marking the book. An empty table below means &ldquo;nothing was allowed
            to trade&rdquo; — not &ldquo;nothing happened&rdquo; and not an error.
          </div>
        )}

        <div className={cx(
          "mt-3 rounded-lg border px-4 py-3 text-[12.5px] leading-[1.6]",
          officialStartedAt ? "border-accent/35 bg-accent/[0.08] text-accent" :
            readiness?.ready ? "border-accent/35 bg-accent/[0.08] text-muted" : "border-line2 bg-panel2/60 text-muted"
        )}>
          {officialStartedAt ? (
            <>
              <strong className="text-accent">Official paper clock active.</strong>{" "}
              Day 0 is <span className="num">{officialStartedAt}</span>. The configured scope at start was{" "}
              <span className="num">{(ledger?.officialConfigs || []).join(", ") || "not recorded"}</span>.
            </>
          ) : readiness?.ready ? (
            <><strong className="text-accent">Ready to establish day 0.</strong> Every server-side launch check currently passes.</>
          ) : (
            <><strong className="text-fg/85">Official clock has not started.</strong> The server will refuse reset until every launch check passes.</>
          )}

          {readinessErr && <div className="mt-2 text-down">{readinessErr}</div>}
          {blockers.length > 0 && (
            <ul className="mt-2 list-disc space-y-1 pl-5 text-warn">
              {blockers.slice(0, 8).map((b) => <li key={b}>{b}</li>)}
              {blockers.length > 8 && <li>+{blockers.length - 8} more blocker(s)</li>}
            </ul>
          )}

          <div className="mt-3 flex flex-wrap gap-2">
            <button
              type="button"
              onClick={checkReadiness}
              disabled={checking || resetting}
              className="inline-flex h-11 items-center rounded-full border border-line2 bg-panel2 px-4 text-[12.5px] text-fg/85 disabled:opacity-50 sm:h-9"
            >
              {checking ? "Checking…" : "Check launch readiness"}
            </button>
            <button
              type="button"
              onClick={doReset}
              disabled={resetting || !readiness?.ready}
              title={!readiness?.ready ? "Every server-side launch check must pass first" : ""}
              className="inline-flex h-11 items-center rounded-full border border-line2 bg-panel2 px-4 text-[12.5px] text-fg/85 transition-colors hover:border-down/50 hover:text-down disabled:cursor-not-allowed disabled:opacity-40 sm:h-9"
            >
              {resetting ? "Starting…" : officialStartedAt ? "Reset — restart official test" : "Reset — start official test"}
            </button>
          </div>
        </div>
      </div>

      {/* ---- Per-config: gates, decision, position, sync ---- */}
      <SecHeader title="Configs — gates, last decision, sync">
        <CountPill n={configs.length} />
        {refusing.length > 0 && <Tag tone="warn">{refusing.length} refused</Tag>}
        <button
          type="button"
          onClick={() => {
            setConfigDraft(configs.map((c) => c.config).join(","));
            setEditingConfigs((v) => !v);
          }}
          className="ml-auto text-[11.5px] text-muted hover:text-fg"
        >
          {editingConfigs ? "Cancel" : "Edit scope"}
        </button>
      </SecHeader>
      {editingConfigs && (
        <div className="border-b border-line/60 bg-panel2/40 px-[22px] py-4">
          <label className="label-mono block text-muted" htmlFor="paper-configs">Paper configs</label>
          <div className="mt-2 flex flex-col gap-2 sm:flex-row">
            <input
              id="paper-configs"
              value={configDraft}
              onChange={(e) => setConfigDraft(e.target.value)}
              placeholder="NVDA:1D:5,GOOGL:1D:5"
              className="h-11 min-w-0 flex-1 rounded-md border border-line2 bg-panel px-3 font-mono text-[12px] text-fg outline-none focus:border-accent sm:h-9"
            />
            <button
              type="button"
              onClick={saveConfigs}
              disabled={savingConfigs || !configDraft.trim()}
              className="h-11 rounded-full border border-line2 bg-panel px-4 text-[12px] text-fg disabled:opacity-40 sm:h-9"
            >
              {savingConfigs ? "Saving…" : "Save to PostgreSQL"}
            </button>
          </div>
          <p className="mt-2 text-[11.5px] leading-relaxed text-muted">
            Use <span className="num text-fg/80">NVDA:1D:5,GOOGL:1D:5</span> to remove TSLA from the
            persisted scope. Changing scope stops an active official clock because it changes N and the experiment design;
            run readiness and establish a new day 0 afterward.
          </p>
        </div>
      )}
      {configs.length === 0 ? (
        <div className="px-[22px] py-10 text-center">
          <div className="mb-1.5 text-[14px] font-semibold text-fg/80">No configs are being papered</div>
          <p className="text-[12.5px] text-muted">Set <code className="rounded bg-panel2 px-1">CONFIGS</code> (TICKER:TF:HORIZON) to start.</p>
        </div>
      ) : (
        <div className="flex flex-col">
          {configs.map((c, i) => {
            const d = c.lastDecision;
            const gates = Object.fromEntries((d?.gates || []).map((g) => [g.name, g]));
            const sync = c.sync;
            const desynced = sync && sync.consistent === false;
            return (
              <div key={c.config} className={cx("px-[22px] py-4", i < configs.length - 1 && "border-b border-line/50")}>
                <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
                  <span className="num text-[14px] font-bold text-fg">{c.ticker}</span>
                  <span className="num text-[12px] text-muted">
                    {c.timeframe} · H{c.horizon}
                    <span className="ml-1 text-muted/60">(label horizon, not a hold)</span>
                  </span>
                  <span className={cx("num text-[13px]", c.position === "flat" ? "text-muted" : c.position === "long" ? "text-accent" : "text-down")}>
                    {cap(c.position)}
                  </span>
                  {c.entryPrice != null && (
                    <span className="num text-[12px] text-muted">entry ${num(c.entryPrice)} on bar {c.entryBar || "—"}</span>
                  )}
                </div>

                {/* The four gates, always all four, in the order the engine evaluates them. */}
                <div className="mt-2.5 flex flex-wrap gap-1.5">
                  {GATE_ORDER.map((g) => (
                    <GateDot key={g} name={g} gate={gates[g]} />
                  ))}
                  {desynced && (
                    <span className="inline-flex items-center gap-1.5 rounded-sm bg-down/12 px-2 py-1 font-mono text-[11px] text-down">
                      <span className="text-[10px]">✕</span> sync
                    </span>
                  )}
                </div>

                {/* The decision, in the engine's own words. */}
                {d ? (
                  <div className="mt-2.5 text-[12.5px] leading-[1.55] text-muted">
                    <span className="num text-fg/85">
                      bar {d.bar || "—"} → target {d.target} · {d.action}
                    </span>
                    {d.gate && <Tag tone="warn" className="ml-2">refused: {d.gate}</Tag>}
                    <div className="mt-1">{d.reason}</div>
                  </div>
                ) : (
                  <p className="mt-2.5 text-[12.5px] text-muted">
                    No decision recorded yet — the engine has not seen a completed bar for this config.
                  </p>
                )}

                {/* The three stores. Silence here would repeat the exact bug this block exists for. */}
                {sync && (
                  <div className={cx("mt-2.5 rounded-md px-3 py-2 text-[12px] leading-[1.55]",
                    desynced ? "bg-down/[0.08] text-down" : "bg-panel2/60 text-muted")}>
                    <strong className="font-semibold">
                      {desynced ? "DESYNCED" : "In sync"}
                    </strong>
                    {sync.pendingBookings > 0 && (
                      <span className="ml-2 num">{sync.pendingBookings} fill(s) owed to the book</span>
                    )}
                    <div className="mt-0.5">{sync.detail}</div>
                  </div>
                )}
              </div>
            );
          })}
        </div>
      )}

      {/* ---- The book (contract §5) ---- */}
      <SecHeader title="The book — simulated equity" borderTop>
        {ledger ? <Tag tone="outline">equal-weight 1/N</Tag> : <Tag tone="warn">not running</Tag>}
        {gapDates.length > 0 && <Tag tone="warn">{gapDates.length} gap {gapDates.length === 1 ? "date" : "dates"}</Tag>}
      </SecHeader>
      {!ledger ? (
        <div className="px-[22px] py-6 text-[12.5px] leading-[1.6] text-muted">
          <strong className="text-fg/85">The fake-money ledger is not running.</strong>{" "}
          {ledgerErr} There is no equity, no fee accounting and no daily mark; positions, if any were opened, would fall
          back to the deprecated fixed <code className="rounded bg-panel2 px-1">POSITION_SIZE</code> and would not be
          comparable with the evaluator&rsquo;s numbers.
        </div>
      ) : (
        <>
          <div className="grid grid-cols-2 gap-x-6 gap-y-4 px-[22px] py-4 sm:grid-cols-3 lg:grid-cols-4">
            <Stat label="Equity" value={money(ledger.equity)} unit="cash + marked positions" />
            <Stat label="Cash" value={money(ledger.cash)} unit={`opened at ${money(ledger.startingCash)}`} />
            <Stat label="Realized P&L" value={fmtMoney(ledger.realizedPnl)} unit="closed episodes, net of both legs' fees"
              tone={ledger.realizedPnl > 0 ? "text-accent" : ledger.realizedPnl < 0 ? "text-down" : "text-fg"} />
            <Stat label="Unrealized" value={fmtMoney(ledger.unrealized)} unit="open lots at their last real bar close, net of entry fees"
              tone={ledger.unrealized > 0 ? "text-accent" : ledger.unrealized < 0 ? "text-down" : "text-fg"} />
            <Stat label="Exposure" value={pct(ledger.exposure)} unit="gross market value / equity" />
            <Stat label="Fees paid" value={money(feesPaid(positions, ledger))} unit="entry fees on the open lots, at each model's validated cost" />
            <Stat label="Daily Sharpe" value={metrics?.dailySharpeAnnualized == null ? "UNMEASURED" : num(metrics.dailySharpeAnnualized)}
              unit={metrics?.dailySharpeAnnualized == null ? null : `annualized ×√${metrics.annualization} from ${metrics.nDailyReturns} daily portfolio returns`}
              tone={metrics?.dailySharpeAnnualized == null ? "text-muted" : "text-fg"} />
            <Stat label="Max drawdown" value={metrics?.maxDrawdown == null ? "—" : pct(metrics.maxDrawdown)}
              unit="largest peak-to-trough decline of the daily equity curve" />
          </div>

          {/* The Sharpe note is rendered VERBATIM. It is the service's own sentence about why a
              number is absent, and paraphrasing it here is how "null" becomes "0". */}
          {metrics?.dailySharpeAnnualized == null && metrics?.sharpeNote && (
            <p className="border-t border-line px-[22px] py-3 text-[12px] leading-[1.6] text-muted">{metrics.sharpeNote}</p>
          )}

          {ledger.series?.length > 0 && (
            <div className="border-t border-line px-[22px] py-3">
              <PerformanceChart series={ledger.series} />
            </div>
          )}

          <div className="border-t border-line px-[22px] py-3 text-[12px] leading-[1.6] text-muted">
            <span className="num text-fg/85">{snapshots?.n ?? 0}</span> daily snapshot{snapshots?.n === 1 ? "" : "s"}
            {snapshots?.first && <> ({snapshots.first} → {snapshots.last})</>}.{" "}
            {gapDates.length === 0 ? (
              <>No gap dates: every enabled config had a real bar close on every marked date.</>
            ) : (
              <>
                <strong className="text-warn">{gapDates.length} gap {gapDates.length === 1 ? "date" : "dates"}</strong> —
                dates the book could NOT mark, because a config&rsquo;s bar was synthetic or missing. A gap is the absence
                of a measurement, never a zero and never the previous close carried forward:{" "}
                <span className="num text-fg/70">{gapDates.slice(0, 12).join(", ")}{gapDates.length > 12 ? ` +${gapDates.length - 12} more` : ""}</span>
              </>
            )}
          </div>

          {positions.length > 0 && (
            <div className="overflow-x-auto border-t border-line">
              <div className="min-w-[720px] px-[22px]">
                <div className={cx("grid grid-cols-[1.4fr_.7fr_.8fr_.8fr_.8fr_1fr] gap-4 border-b border-line py-2.5", colHead)}>
                  <span>Lot</span>
                  <span>Side</span>
                  <span>Qty</span>
                  <span>Entry</span>
                  <span>Mark</span>
                  <span className="justify-self-end">Unrealized</span>
                </div>
                {positions.map((p, i) => (
                  <div key={p.config} className={cx("grid grid-cols-[1.4fr_.7fr_.8fr_.8fr_.8fr_1fr] items-center gap-4 py-3", i < positions.length - 1 && "border-b border-line/50")}>
                    <span className="num text-[13px] text-fg">{p.config}</span>
                    <span className={cx("num text-[13px]", p.side === "long" ? "text-accent" : "text-down")}>{cap(p.side)}</span>
                    <span className="num text-[13px] text-fg/80">{num(p.qty, 4)}</span>
                    <span className="num text-[13px] text-fg/80">${num(p.entryPrice)}</span>
                    <span className="num text-[13px] text-fg/80">
                      ${num(p.mark)}
                      {!p.marked && <Tag tone="warn" className="ml-1">unmarked</Tag>}
                    </span>
                    <span className={cx("num justify-self-end text-[13px]", p.unrealized >= 0 ? "text-accent" : "text-down")}>
                      {fmtMoney(p.unrealized)}
                    </span>
                  </div>
                ))}
              </div>
            </div>
          )}
        </>
      )}

      {/* ---- Live vs backtest, with the units kept ---- */}
      <SecHeader title="Live paper vs. backtest" borderTop>
        <Tag tone="outline">Out-of-sample check</Tag>
      </SecHeader>
      {comparison.length === 0 ? (
        <div className="px-[22px] py-8 text-center text-[12.5px] text-muted">
          No comparison yet — nothing has closed, because nothing has been allowed to open.
        </div>
      ) : (
        <>
          <div className="overflow-x-auto">
            <div className="min-w-[820px] px-[22px]">
              <div className={cx("grid grid-cols-[1.3fr_.8fr_1fr_1fr_1fr_1fr] gap-4 border-b border-line py-2.5", colHead)}>
                <span>Config</span>
                <span>Closed</span>
                <span>Live daily Sharpe</span>
                <span>Reference Sharpe</span>
                <span>Live mean daily</span>
                <span className="justify-self-end">Status</span>
              </div>
              {comparison.map((c, i) => {
                const live = c.portfolio?.live;
                const ref = c.portfolio?.reference;
                return (
                  <div key={c.config} className={cx("grid grid-cols-[1.3fr_.8fr_1fr_1fr_1fr_1fr] items-center gap-4 py-3",
                    i < comparison.length - 1 && "border-b border-line/50", c.divergence && "bg-down/[0.06]")}>
                    <span className="num text-[13px] text-fg">{c.ticker} · {c.timeframe} · H{c.horizon}</span>
                    <span className="num text-[13px] text-fg/80">
                      {c.live.nClosed}{c.nOpen ? ` (+${c.nOpen} open)` : ""}
                    </span>
                    <span className={cx("num text-[13px]", live?.dailySharpeAnnualized == null ? "text-muted/70" : "text-fg/80")}>
                      {live?.dailySharpeAnnualized == null ? "UNMEASURED" : num(live.dailySharpeAnnualized)}
                    </span>
                    <span className={cx("num text-[13px]", ref?.sharpe == null ? "text-muted/70" : "text-fg/80")}>
                      {num(ref?.sharpe)}
                      {ref?.source === "model-backtest" && <Tag tone="warn" className="ml-1">per-bar</Tag>}
                    </span>
                    <span className={cx("num text-[13px]", live?.meanDailyReturn == null ? "text-muted/70" : "text-fg/80")}>
                      {live?.meanDailyReturn == null ? "—" : pct(live.meanDailyReturn, 3)}
                    </span>
                    <span className="justify-self-end">
                      {c.divergence ? (
                        <span className="rounded-sm bg-down/12 px-2 py-1 font-mono text-[11px] text-down">divergence / decay</span>
                      ) : c.meaningful ? (
                        <span className="rounded-sm bg-accent/12 px-2 py-1 font-mono text-[11px] text-accent">tracking</span>
                      ) : (
                        <span className="rounded-sm bg-warn/12 px-2 py-1 font-mono text-[11px] text-warn">&lt;{minMeaningful} trades</span>
                      )}
                    </span>
                  </div>
                );
              })}
            </div>
          </div>

          <div className="flex flex-col gap-2 px-[22px] pb-4 pt-3.5">
            {comparison.map((c) => (
              <div key={c.config} className="text-[12.5px] leading-[1.5] text-muted">
                <strong className="num text-[12px] text-fg/85">{c.ticker}·{c.timeframe}·H{c.horizon}:</strong>{" "}
                {c.note}
                {c.portfolio?.note && <span className="mt-0.5 block text-muted/80">{c.portfolio.note}</span>}
              </div>
            ))}
          </div>
        </>
      )}

      {/* The units, as data from the service rather than as a sentence somebody has to remember. */}
      {units?.note && (
        <p className="border-t border-line px-[22px] py-3 text-[11px] leading-relaxed text-muted/60">
          <strong className="text-muted/80">Comparability:</strong> {units.note} Simulation only; not investment advice.
        </p>
      )}

      {/* ---- Settled completed-bar decisions, not polling attempts ---- */}
      <SecHeader title="Settled decision history" borderTop>
        <CountPill n={history.total || 0} />
        {(history.gates && Object.keys(history.gates).length > 0) && <Tag tone="warn">gate refusals recorded</Tag>}
      </SecHeader>
      {history.error ? (
        <p className="px-[22px] py-5 text-[12.5px] text-down">Decision history is unavailable: {history.error}</p>
      ) : !history.recent?.length ? (
        <div className="px-[22px] py-7 text-center text-[12.5px] text-muted">
          No settled completed-bar decisions in this experiment generation yet. Transient retries are intentionally not counted.
        </div>
      ) : (
        <div className="overflow-x-auto">
          <div className="min-w-[860px] px-[22px]">
            <div className={cx("grid grid-cols-[1.1fr_1.2fr_.8fr_1fr_1.2fr] gap-4 border-b border-line py-2.5", colHead)}>
              <span>Bar</span><span>Config</span><span>Action</span><span>Gate</span><span>Model lineage</span>
            </div>
            {history.recent.slice(0, 30).map((event, i) => {
              const decision = event.decision || {};
              return (
                <div key={`${event.generation}-${event.config}-${decision.barUnix}`}
                  className={cx("grid grid-cols-[1.1fr_1.2fr_.8fr_1fr_1.2fr] items-start gap-4 py-3 text-[12px]",
                    i < Math.min(history.recent.length, 30) - 1 && "border-b border-line/50")}>
                  <span className="num text-fg/80">{decision.bar || "—"}</span>
                  <span className="num text-fg">{event.config}</span>
                  <span className={cx("font-mono uppercase", decision.action === "none" ? "text-muted" : "text-accent")}>
                    {decision.action || "—"}
                  </span>
                  <span>{decision.gate ? <Tag tone="warn">{decision.gate}</Tag> : <span className="text-muted">passed</span>}</span>
                  <span className="min-w-0 font-mono text-[10.5px] text-muted" title={`${decision.modelVersion || ""} ${decision.strategyVersion || ""}`}>
                    <span className="block truncate">{decision.modelVersion || "unknown model"}</span>
                    <span className="block truncate text-muted/60">{decision.strategyVersion || "unknown strategy"}</span>
                  </span>
                </div>
              );
            })}
          </div>
          <p className="border-t border-line px-[22px] py-3 text-[11px] leading-relaxed text-muted/60">
            One row per settled completed bar. Network, quote and synchronization retries remain in current status and do not inflate experiment counts.
          </p>
        </div>
      )}
    </section>
  );
}

// feesPaid is what the book has actually been charged on the lots it currently holds, at the cost
// each model's own backtest was validated under. Exit fees land in `realizedPnl`, so this is the
// open half — labelled as such rather than presented as a lifetime total the payload does not serve.
function feesPaid(positions, ledger) {
  if (!positions?.length) return ledger?.equity == null ? null : 0;
  return positions.reduce((sum, p) => sum + (Number(p.entryFee) || 0), 0);
}
