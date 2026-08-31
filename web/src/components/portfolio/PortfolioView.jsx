import { useCallback, useEffect, useMemo, useState } from "react";
import { useAuth } from "../../auth/AuthContext.jsx";
import {
  createPortfolio,
  createPortfolioReview,
  createPortfolioSnapshot,
  deletePortfolio,
  getPortfolioEventImpact,
  getPortfolioIntelligence,
  listPortfolioSnapshots,
  listPortfolioReviews,
  listPortfolios,
  runPortfolioScenario,
  updatePortfolio,
} from "../../lib/portfolioApi.js";
import { fmtRatioPct, relFromISO, relFromUnix } from "../../lib/format.js";
import { cx } from "../../lib/cx.js";
import { Button, EmptyState, Field, Label, Panel, Stat, controlClass } from "../ui/index.js";
import { DestinationHeader } from "../shell/DestinationHeader.jsx";
import { Tag } from "../terminal/bits.jsx";
import EventImpactPanel from "./EventImpactPanel.jsx";

const clone = (value) => JSON.parse(JSON.stringify(value));
const numberOrNull = (value) => (value === "" || value == null ? null : Number(value));
const percentInput = (value) => (value == null ? "" : String(Number(value) * 100));
const percentValue = (value) => (value === "" ? null : Number(value) / 100);

const emptyPortfolio = () => ({
  name: "My portfolio",
  baseCurrency: "USD",
  positions: [],
  cash: [{ currency: "USD", amount: 0 }],
  targets: [],
  profile: {
    objective: "",
    horizon: "",
    lossTolerance: "",
    constraints: {
      noLeverage: true,
      excludedSectors: [],
      minimumCashWeight: null,
      maximumPositionWeight: null,
    },
  },
});

function money(value, currency = "USD") {
  if (value == null || !Number.isFinite(Number(value))) return "—";
  try {
    return new Intl.NumberFormat("en-US", {
      style: "currency",
      currency,
      maximumFractionDigits: 2,
    }).format(Number(value));
  } catch {
    return `${Number(value).toLocaleString("en-US", { maximumFractionDigits: 2 })} ${currency}`;
  }
}

function FormNumber({ value, onChange, ...props }) {
  return (
    <input
      type="number"
      className={controlClass}
      value={value ?? ""}
      onChange={(event) => onChange(event.target.value)}
      {...props}
    />
  );
}

function PortfolioCreator({ onCreated, onCancel }) {
  const [name, setName] = useState("My portfolio");
  const [currency, setCurrency] = useState("USD");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const submit = async (event) => {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      const portfolio = emptyPortfolio();
      portfolio.name = name;
      portfolio.baseCurrency = currency.toUpperCase();
      portfolio.cash[0].currency = portfolio.baseCurrency;
      onCreated(await createPortfolio(portfolio));
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Panel variant="glass" title="Create a portfolio">
      <form onSubmit={submit} className="grid gap-3 md:grid-cols-[1fr_180px_auto] md:items-end">
        <Field label="Portfolio name">
          <input className={controlClass} value={name} onChange={(event) => setName(event.target.value)} />
        </Field>
        <Field label="Base currency">
          <input
            className={controlClass}
            maxLength={3}
            value={currency}
            onChange={(event) => setCurrency(event.target.value.toUpperCase())}
          />
        </Field>
        <div className="flex gap-2">
          <Button type="submit" variant="primary" disabled={busy || !name.trim()}>
            {busy ? "Creating…" : "Create"}
          </Button>
          {onCancel && <Button onClick={onCancel}>Cancel</Button>}
        </div>
      </form>
      {error && <p className="mt-3 text-[12px] text-down">{error}</p>}
    </Panel>
  );
}

function HoldingsEditor({ draft, setDraft }) {
  const positions = draft.positions || [];
  const cash = draft.cash || [];
  const updatePosition = (index, patch) =>
    setDraft((current) => ({
      ...current,
      positions: current.positions.map((row, i) => (i === index ? { ...row, ...patch } : row)),
    }));
  const updateCash = (index, patch) =>
    setDraft((current) => ({
      ...current,
      cash: current.cash.map((row, i) => (i === index ? { ...row, ...patch } : row)),
    }));

  return (
    <Panel title="Holdings and cash" badges={<Tag tone="outline">manual record</Tag>}>
      <div className="overflow-x-auto">
        <table className="w-full min-w-[900px] border-collapse text-left text-[12.5px]">
          <thead className="label-mono text-muted">
            <tr className="border-b border-line">
              <th className="pb-2 pr-2">Ticker</th>
              <th className="pb-2 px-2">Quantity</th>
              <th className="pb-2 px-2">Average cost</th>
              <th className="pb-2 px-2">Manual value</th>
              <th className="pb-2 px-2">Sector</th>
              <th className="pb-2 px-2">Industry</th>
              <th className="pb-2 pl-2" aria-label="Actions" />
            </tr>
          </thead>
          <tbody>
            {positions.map((row, index) => (
              <tr key={`${row.ticker}-${index}`} className="border-b border-line/70 align-top">
                <td className="py-2 pr-2">
                  <input
                    className={cx(controlClass, "font-mono uppercase")}
                    value={row.ticker || ""}
                    onChange={(event) => updatePosition(index, { ticker: event.target.value.toUpperCase() })}
                  />
                </td>
                <td className="p-2"><FormNumber min="0" step="any" value={row.quantity} onChange={(v) => updatePosition(index, { quantity: Number(v) })} /></td>
                <td className="p-2"><FormNumber min="0" step="any" value={row.averageCost ?? ""} onChange={(v) => updatePosition(index, { averageCost: numberOrNull(v) })} /></td>
                <td className="p-2"><FormNumber min="0" step="any" value={row.manualValue ?? ""} onChange={(v) => updatePosition(index, { manualValue: numberOrNull(v) })} /></td>
                <td className="p-2"><input className={controlClass} value={row.sector || ""} onChange={(event) => updatePosition(index, { sector: event.target.value })} /></td>
                <td className="p-2"><input className={controlClass} value={row.industry || ""} onChange={(event) => updatePosition(index, { industry: event.target.value })} /></td>
                <td className="py-2 pl-2">
                  <Button size="xs" variant="danger" onClick={() => setDraft((current) => ({ ...current, positions: current.positions.filter((_, i) => i !== index) }))}>Remove</Button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {positions.length === 0 && <p className="mt-2 text-[12px] text-muted">No holdings yet. Cash-only portfolios are valid.</p>}
      <Button
        size="sm"
        className="mt-3"
        onClick={() => setDraft((current) => ({
          ...current,
          positions: [...current.positions, { ticker: "", quantity: 0, averageCost: null, manualValue: null, sector: "", industry: "" }],
        }))}
      >
        + Add holding
      </Button>

      <div className="mt-5 border-t border-line pt-4">
        <div className="label-mono mb-2 text-muted">Cash balances</div>
        <div className="grid gap-2 md:grid-cols-2">
          {cash.map((row, index) => (
            <div key={`${row.currency}-${index}`} className="grid grid-cols-[110px_1fr_auto] gap-2">
              <input className={cx(controlClass, "font-mono uppercase")} maxLength={3} value={row.currency || ""} onChange={(event) => updateCash(index, { currency: event.target.value.toUpperCase() })} />
              <FormNumber min="0" step="any" value={row.amount} onChange={(v) => updateCash(index, { amount: Number(v) })} />
              <Button size="xs" variant="danger" onClick={() => setDraft((current) => ({ ...current, cash: current.cash.filter((_, i) => i !== index) }))}>Remove</Button>
            </div>
          ))}
        </div>
        <Button size="sm" className="mt-3" onClick={() => setDraft((current) => ({ ...current, cash: [...current.cash, { currency: current.baseCurrency, amount: 0 }] }))}>+ Add cash balance</Button>
      </div>
    </Panel>
  );
}

function TargetsEditor({ draft, setDraft }) {
  const targets = draft.targets || [];
  const update = (index, patch) => setDraft((current) => ({
    ...current,
    targets: current.targets.map((row, i) => (i === index ? { ...row, ...patch } : row)),
  }));
  return (
    <Panel title="Target ranges" badges={<Tag tone="outline">user-defined</Tag>}>
      <p className="mb-3 text-[12.5px] leading-relaxed text-muted">
        Targets are review bands you set. They identify drift; they do not create a trade instruction.
      </p>
      <div className="flex flex-col gap-2">
        {targets.map((target, index) => (
          <div key={`${target.kind}-${target.key}-${index}`} className="grid gap-2 rounded-xl border border-line p-3 md:grid-cols-[120px_1fr_130px_130px_130px_auto]">
            <select className={controlClass} value={target.kind || "ticker"} onChange={(event) => update(index, { kind: event.target.value })}>
              <option value="ticker">Ticker</option>
              <option value="sector">Sector</option>
              <option value="cash">Cash</option>
            </select>
            <input className={controlClass} placeholder="NVDA / Technology / USD" value={target.key || ""} onChange={(event) => update(index, { key: event.target.value })} />
            <Field label="Target %"><FormNumber min="0" max="100" step="0.1" value={percentInput(target.targetWeight)} onChange={(v) => update(index, { targetWeight: percentValue(v) })} /></Field>
            <Field label="Minimum %"><FormNumber min="0" max="100" step="0.1" value={percentInput(target.minWeight)} onChange={(v) => update(index, { minWeight: percentValue(v) })} /></Field>
            <Field label="Maximum %"><FormNumber min="0" max="100" step="0.1" value={percentInput(target.maxWeight)} onChange={(v) => update(index, { maxWeight: percentValue(v) })} /></Field>
            <Button size="xs" variant="danger" onClick={() => setDraft((current) => ({ ...current, targets: current.targets.filter((_, i) => i !== index) }))}>Remove</Button>
          </div>
        ))}
      </div>
      <Button size="sm" className="mt-3" onClick={() => setDraft((current) => ({ ...current, targets: [...current.targets, { kind: "ticker", key: "", targetWeight: null, minWeight: null, maxWeight: null }] }))}>+ Add target</Button>
    </Panel>
  );
}

function PolicyEditor({ draft, setDraft }) {
  const profile = draft.profile || emptyPortfolio().profile;
  const constraints = profile.constraints || {};
  const updateProfile = (patch) => setDraft((current) => ({ ...current, profile: { ...current.profile, ...patch } }));
  const updateConstraints = (patch) => setDraft((current) => ({
    ...current,
    profile: { ...current.profile, constraints: { ...current.profile.constraints, ...patch } },
  }));
  return (
    <Panel title="Portfolio policy" badges={<Tag tone="outline">deterministic constraints</Tag>}>
      <div className="grid gap-3 md:grid-cols-3">
        <Field label="Objective">
          <select className={controlClass} value={profile.objective || ""} onChange={(event) => updateProfile({ objective: event.target.value })}>
            <option value="">Not set</option><option value="capital_preservation">Capital preservation</option><option value="balanced">Balanced</option><option value="growth">Growth</option><option value="aggressive_growth">Aggressive growth</option>
          </select>
        </Field>
        <Field label="Investment horizon">
          <select className={controlClass} value={profile.horizon || ""} onChange={(event) => updateProfile({ horizon: event.target.value })}>
            <option value="">Not set</option><option value="under_1_year">Under 1 year</option><option value="1_3_years">1–3 years</option><option value="3_5_years">3–5 years</option><option value="5_plus_years">5+ years</option>
          </select>
        </Field>
        <Field label="Temporary-loss tolerance">
          <select className={controlClass} value={profile.lossTolerance || ""} onChange={(event) => updateProfile({ lossTolerance: event.target.value })}>
            <option value="">Not set</option><option value="low">Low</option><option value="medium">Medium</option><option value="high">High</option>
          </select>
        </Field>
        <Field label="Maximum single position %"><FormNumber min="0" max="100" step="0.1" value={percentInput(constraints.maximumPositionWeight)} onChange={(v) => updateConstraints({ maximumPositionWeight: percentValue(v) })} /></Field>
        <Field label="Minimum cash %"><FormNumber min="0" max="100" step="0.1" value={percentInput(constraints.minimumCashWeight)} onChange={(v) => updateConstraints({ minimumCashWeight: percentValue(v) })} /></Field>
        <Field label="Excluded sectors (comma-separated)">
          <input className={controlClass} value={(constraints.excludedSectors || []).join(", ")} onChange={(event) => updateConstraints({ excludedSectors: event.target.value.split(",").map((v) => v.trim()).filter(Boolean) })} />
        </Field>
      </div>
    </Panel>
  );
}

function IntelligenceSummary({ intelligence }) {
  if (!intelligence) return null;
  const risk = intelligence.historicalRisk || {};
  const largest = intelligence.concentration || {};
  return (
    <>
      <Panel variant="glass" title="Portfolio state" badges={<Tag tone="accent">calculated in code</Tag>}>
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4 xl:grid-cols-7">
          <Stat label="Total value" value={money(intelligence.totalValue, intelligence.baseCurrency)} size="lg" sub={intelligence.valuationComplete ? "complete valuation" : `${money(intelligence.knownValue, intelligence.baseCurrency)} known`} />
          <Stat label="Cash" value={fmtRatioPct(intelligence.cashWeight, 1)} sub={money(intelligence.cashValue, intelligence.baseCurrency)} />
          <Stat label="Largest position" value={fmtRatioPct(largest.largestWeight, 1)} sub={largest.largestTicker || "—"} />
          <Stat label="Top two" value={fmtRatioPct(largest.topTwoWeight, 1)} />
          <Stat label="Annual volatility" value={fmtRatioPct(risk.annualizedVolatility, 1)} sub={risk.available ? `${risk.observations || 0} observations` : "unavailable"} />
          <Stat label="Beta vs SPY" value={risk.beta == null ? "—" : Number(risk.beta).toFixed(2)} />
          <Stat label="Max drawdown" value={fmtRatioPct(risk.maximumDrawdown, 1)} sub={risk.from && risk.to ? `${risk.from} → ${risk.to}` : ""} />
        </div>
        {(intelligence.positions.some((position) => position.sourceIsSynthetic) || risk.sourceIsSynthetic) && (
          <p className="mt-4 rounded-xl border border-warn/30 bg-warn/5 px-3 py-2 text-[11.5px] text-warn">
            Synthetic price history is in use. These numbers demonstrate the workflow and are not market data.
          </p>
        )}
        {intelligence.degraded?.length > 0 && <p className="mt-3 text-[11.5px] text-muted">Unavailable inputs: {intelligence.degraded.join(" · ")}</p>}
      </Panel>

      <div className="grid gap-3 xl:grid-cols-[1.4fr_1fr]">
        <Panel title="Positions" flush>
          <div className="overflow-x-auto">
            <table className="w-full min-w-[760px] border-collapse text-left text-[12.5px]">
              <thead className="label-mono text-muted"><tr className="border-b border-line"><th className="px-4 py-3">Holding</th><th className="px-3 py-3">Value</th><th className="px-3 py-3">Weight</th><th className="px-3 py-3">Thesis check</th><th className="px-3 py-3">Target state</th><th className="px-4 py-3">Source</th></tr></thead>
              <tbody>
                {intelligence.positions.map((position) => (
                  <tr key={position.ticker} className="border-b border-line/70 last:border-0">
                    <td className="px-4 py-3"><span className="font-mono font-semibold">{position.ticker}</span><span className="ml-2 text-muted">{position.quantity} shares</span>{position.sector && <div className="text-[11px] text-dim">{position.sector}{position.industry ? ` · ${position.industry}` : ""}</div>}</td>
                    <td className="px-3 py-3 num">{money(position.marketValue, intelligence.baseCurrency)}</td>
                    <td className="px-3 py-3 num">{fmtRatioPct(position.weight, 1)}</td>
                    <td className="px-3 py-3">{position.thesis ? <><span className="text-fg">{position.thesis.latestCheckVerdict || "not checked"}</span><div className="max-w-[260px] truncate text-[11px] text-muted">{position.thesis.claim}</div></> : <span className="text-muted">No active thesis</span>}</td>
                    <td className="px-3 py-3"><span className={cx(position.target?.state === "above_range" || position.target?.state === "below_range" ? "text-warn" : "text-muted")}>{position.target?.state?.replaceAll("_", " ") || "No target"}</span>{position.target?.drift != null && <div className="num text-[11px] text-dim">drift {position.target.drift >= 0 ? "+" : ""}{(position.target.drift * 100).toFixed(1)} pp</div>}</td>
                    <td className="px-4 py-3 text-[11px] text-muted">{position.valuationSource}{position.quoteSource ? ` · ${position.quoteSource}` : ""}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {intelligence.positions.length === 0 && <div className="p-4 text-[12px] text-muted">Cash-only portfolio.</div>}
        </Panel>

        <Panel title="Attention">
          {intelligence.findings?.length ? (
            <div className="flex flex-col gap-2">
              {intelligence.findings.map((finding, index) => (
                <div key={`${finding.code}-${finding.subject}-${index}`} className="rounded-xl border border-warn/25 bg-warn/5 p-3">
                  <div className="flex items-center gap-2"><Tag tone="warn">review</Tag><span className="font-mono text-[11px] text-fg">{finding.subject}</span></div>
                  <p className="mt-1.5 text-[12.5px] leading-relaxed text-muted">{finding.summary}</p>
                </div>
              ))}
            </div>
          ) : <EmptyState title="No policy or range findings">Current calculated weights are inside every target and constraint you configured.</EmptyState>}
        </Panel>
      </div>

      <div className="grid gap-3 lg:grid-cols-2">
        <Panel title="Exposure">
          {intelligence.exposures?.length ? <div className="flex flex-col gap-2">{intelligence.exposures.map((exposure) => <div key={`${exposure.kind}-${exposure.key}`} className="grid grid-cols-[90px_1fr_auto] items-center gap-3"><span className="label-mono text-muted">{exposure.kind}</span><span className="text-[12.5px] text-fg">{exposure.key}</span><span className="num text-[12px]">{fmtRatioPct(exposure.weight, 1)}</span></div>)}</div> : <EmptyState title="No classified exposure">Add sector or industry labels to holdings to calculate exposure.</EmptyState>}
        </Panel>
        <Panel title="Historical correlations">
          {risk.correlations?.length ? <div className="flex flex-col gap-2">{risk.correlations.map((row) => <div key={`${row.tickerA}-${row.tickerB}`} className="grid grid-cols-[1fr_auto] gap-3"><span className="font-mono text-[12px]">{row.tickerA} / {row.tickerB}</span><span className="num text-[12px]">{Number(row.correlation).toFixed(2)}</span></div>)}</div> : <EmptyState title="No pairwise correlation">At least two holdings with sufficient common history are required.</EmptyState>}
        </Panel>
      </div>
    </>
  );
}

function ChangesPanel({ snapshot, onSnapshot, busy }) {
  const changes = snapshot?.changes || [];
  return (
    <Panel
      title="What changed"
      badges={snapshot ? <><Tag tone={snapshot.materialChangeCount ? "warn" : "outline"}>{snapshot.materialChangeCount} material</Tag><Tag tone="outline">{relFromUnix(snapshot.createdAt)}</Tag></> : <Tag tone="outline">no baseline</Tag>}
      actions={<Button size="sm" variant="primary" onClick={onSnapshot} disabled={busy}>{busy ? "Calculating…" : snapshot ? "Save review checkpoint" : "Establish baseline"}</Button>}
    >
      {!snapshot ? <EmptyState title="No review baseline">Save an explicit checkpoint. Later checkpoints compare holdings, weights, targets, thesis checks, cash, and concentration against it.</EmptyState> : changes.length === 0 ? <EmptyState title="No change from the prior context">This checkpoint is the baseline, or its material inputs matched the cached context.</EmptyState> : <div className="flex flex-col gap-2">{changes.map((change, index) => <div key={`${change.type}-${change.subject}-${index}`} className={cx("rounded-xl border p-3", change.material ? "border-warn/30 bg-warn/5" : "border-line bg-white/[.01]")}><div className="flex items-center gap-2"><Tag tone={change.material ? "warn" : "outline"}>{change.material ? "material" : "recorded"}</Tag><span className="font-mono text-[11px] text-fg">{change.subject || "PORTFOLIO"}</span>{change.impactWeight != null && <span className="ml-auto num text-[11px] text-muted">{fmtRatioPct(change.impactWeight, 1)} of portfolio</span>}</div><p className="mt-1.5 text-[12.5px] text-muted">{change.summary}</p></div>)}</div>}
    </Panel>
  );
}

function ReviewPanel({ review, currentContextVersion, onReview, busy }) {
  const structured = review?.structured;
  const current = review?.contextVersion && review.contextVersion === currentContextVersion;
  const list = (title, items, tone = "outline") => (
    <div>
      <div className="label-mono mb-2 text-muted">{title}</div>
      {items?.length ? <div className="flex flex-col gap-2">{items.map((item, index) => <div key={`${title}-${index}`} className="flex gap-2 text-[12.5px] leading-relaxed text-muted"><Tag tone={tone}>{tone === "warn" ? "!" : "·"}</Tag><span>{item}</span></div>)}</div> : <span className="text-[12px] text-dim">None stated.</span>}
    </div>
  );
  return (
    <Panel
      title="Portfolio review"
      badges={review ? <><Tag tone={current ? "accent" : "warn"}>{current ? "current context" : "prior context"}</Tag><Tag tone="llm">{review.modelUsed}</Tag><Tag tone="outline">{relFromUnix(review.createdAt)}</Tag></> : <Tag tone="outline">not generated</Tag>}
      actions={<Button size="sm" onClick={onReview} disabled={busy}>{busy ? "Reviewing…" : review && current ? "Reuse current review" : "Generate review"}</Button>}
    >
      {!structured ? <EmptyState title="No interpretive review">Generate only when you want Qwen to explain the deterministic portfolio context. It is never called by polling or page load.</EmptyState> : <>
        <div className="rounded-xl border border-line bg-white/[.015] p-3">
          <div className="label-mono text-accent">Posture</div>
          <p className="mt-1.5 text-[13px] leading-relaxed text-fg">{structured.posture}</p>
          <p className="mt-2 text-[12.5px] leading-relaxed text-muted">{structured.summary}</p>
        </div>
        <div className="mt-4 grid gap-4 md:grid-cols-3">
          {list("Supports", structured.supports)}
          {list("Threats", structured.threats, "warn")}
          {list("Invalidations", structured.invalidations)}
        </div>
        {structured.attention?.length > 0 && <div className="mt-4 border-t border-line pt-4"><div className="label-mono mb-2 text-warn">Attention</div><div className="grid gap-2 md:grid-cols-2">{structured.attention.map((item, index) => <div key={`${item.subject}-${index}`} className="rounded-xl border border-warn/25 bg-warn/5 p-3"><span className="font-mono text-[11px] text-fg">{item.subject}</span><p className="mt-1 text-[12px] leading-relaxed text-muted">{item.reason}</p></div>)}</div></div>}
        <p className="mt-4 text-[10.5px] leading-relaxed text-dim">{review.disclaimer}</p>
      </>}
    </Panel>
  );
}

function ScenarioPanel({ result, question, setQuestion, onRun, busy }) {
  const structured = result?.structured;
  return (
    <Panel title="Portfolio scenario" badges={<Tag tone="llm">qualitative what-if</Tag>}>
      <form onSubmit={(event) => { event.preventDefault(); onRun(); }} className="flex flex-col gap-2 sm:flex-row">
        <input className={controlClass} value={question} onChange={(event) => setQuestion(event.target.value)} placeholder="What happens to this portfolio if rates stay higher for longer?" maxLength={1500} />
        <Button type="submit" variant="primary" disabled={busy || !question.trim()}>{busy ? "Reasoning…" : "Run scenario"}</Button>
      </form>
      {!structured ? <p className="mt-3 text-[12px] leading-relaxed text-muted">The backend supplies current holdings, weights, exposures, risk, targets, and thesis context. Qwen explains a hypothetical chain without forecasting a price or proposing a transaction.</p> : <div className="mt-4 flex flex-col gap-3">
        <div className="flex flex-wrap items-center gap-2"><Tag tone="accent">Exposure: {structured.overallExposure}</Tag><Tag tone="outline">{result.modelUsed}</Tag></div>
        <p className="text-[13px] leading-relaxed text-fg">{structured.summary}</p>
        {structured.mostExposed?.length > 0 && <div><div className="label-mono mb-2 text-warn">Most exposed in this hypothetical</div><div className="grid gap-2 md:grid-cols-2">{structured.mostExposed.map((item, index) => <div key={`${item.ticker}-${index}`} className="rounded-xl border border-line p-3"><span className="font-mono text-[11px] text-fg">{item.ticker}</span><p className="mt-1 text-[12px] text-muted">{item.mechanism}</p></div>)}</div></div>}
        <div className="grid gap-4 md:grid-cols-3">{[["Secondary effects", structured.secondaryEffects], ["Mitigants", structured.mitigants], ["Uncertainties", structured.uncertainties]].map(([title, items]) => <div key={title}><div className="label-mono mb-2 text-muted">{title}</div>{items?.length ? <ul className="m-0 flex list-none flex-col gap-1.5 p-0 text-[12px] text-muted">{items.map((item, index) => <li key={index}>· {item}</li>)}</ul> : <span className="text-[12px] text-dim">None stated.</span>}</div>)}</div>
        {structured.invalidations?.length > 0 && <div className="rounded-xl border border-line p-3"><div className="label-mono mb-1.5 text-muted">What would invalidate this scenario</div>{structured.invalidations.map((item, index) => <p key={index} className="text-[12px] text-muted">· {item}</p>)}</div>}
        <p className="text-[10.5px] leading-relaxed text-dim">{result.disclaimer}</p>
      </div>}
    </Panel>
  );
}

export default function PortfolioView() {
  const { user, promptSignIn } = useAuth();
  const [portfolios, setPortfolios] = useState([]);
  const [selectedId, setSelectedId] = useState("");
  const [draft, setDraft] = useState(null);
  const [intelligence, setIntelligence] = useState(null);
  const [snapshots, setSnapshots] = useState([]);
  const [reviews, setReviews] = useState([]);
  const [reviewBusy, setReviewBusy] = useState(false);
  const [scenarioBusy, setScenarioBusy] = useState(false);
  const [scenarioQuestion, setScenarioQuestion] = useState("");
  const [scenarioResult, setScenarioResult] = useState(null);
  const [loading, setLoading] = useState(true);
  const [creating, setCreating] = useState(false);
  const [saving, setSaving] = useState(false);
  const [snapshotBusy, setSnapshotBusy] = useState(false);
  const [error, setError] = useState("");
  // Phase 3 — event impact. Kept in its own state slice with its own error, so an unreachable
  // events service degrades ONE panel instead of blanking the portfolio.
  const [eventImpact, setEventImpact] = useState(null);
  const [eventImpactError, setEventImpactError] = useState("");
  const [eventImpactLoading, setEventImpactLoading] = useState(false);

  const selected = useMemo(() => portfolios.find((portfolio) => portfolio.id === selectedId) || null, [portfolios, selectedId]);

  const refreshList = useCallback(async (preferredId = "") => {
    const rows = await listPortfolios();
    setPortfolios(rows);
    setSelectedId((current) => preferredId || (rows.some((row) => row.id === current) ? current : rows[0]?.id || ""));
    return rows;
  }, []);

  useEffect(() => {
    let alive = true;
    setLoading(true);
    refreshList().catch((err) => alive && setError(err.message)).finally(() => alive && setLoading(false));
    return () => { alive = false; };
  }, [refreshList, user?.id]);

  useEffect(() => {
    if (!selected) {
      setDraft(null);
      setIntelligence(null);
      setSnapshots([]);
      setReviews([]);
      setScenarioResult(null);
      setEventImpact(null);
      setEventImpactError("");
      return;
    }
    let alive = true;
    setDraft(clone(selected));
    setError("");
    Promise.all([getPortfolioIntelligence(selected.id), listPortfolioSnapshots(selected.id), listPortfolioReviews(selected.id)])
      .then(([nextIntelligence, nextSnapshots, nextReviews]) => {
        if (!alive) return;
        setIntelligence(nextIntelligence);
        setSnapshots(nextSnapshots);
        setReviews(nextReviews);
        setScenarioResult(null);
      })
      .catch((err) => alive && setError(err.message));

    setEventImpactLoading(true);
    setEventImpactError("");
    getPortfolioEventImpact(selected.id)
      .then((next) => { if (alive) setEventImpact(next); })
      .catch((err) => { if (alive) setEventImpactError(err.message); })
      .finally(() => { if (alive) setEventImpactLoading(false); });

    return () => { alive = false; };
  }, [selected]);

  const save = async () => {
    if (!draft) return;
    setSaving(true);
    setError("");
    try {
      const updated = await updatePortfolio(draft.id, {
        name: draft.name,
        baseCurrency: draft.baseCurrency,
        positions: draft.positions,
        cash: draft.cash,
        targets: draft.targets,
        profile: draft.profile,
      });
      setPortfolios((rows) => rows.map((row) => (row.id === updated.id ? updated : row)));
      setDraft(clone(updated));
      setIntelligence(await getPortfolioIntelligence(updated.id));
      setScenarioResult(null);
    } catch (err) {
      setError(err.message);
    } finally {
      setSaving(false);
    }
  };

  const saveSnapshot = async () => {
    if (!selectedId) return;
    setSnapshotBusy(true);
    setError("");
    try {
      const result = await createPortfolioSnapshot(selectedId);
      setIntelligence(result.snapshot.intelligence);
      setSnapshots((rows) => result.reused
        ? (rows.some((row) => row.id === result.snapshot.id) ? rows : [result.snapshot, ...rows])
        : [result.snapshot, ...rows]);
    } catch (err) {
      setError(err.message);
    } finally {
      setSnapshotBusy(false);
    }
  };

  const generateReview = async () => {
    if (!selectedId) return;
    setReviewBusy(true);
    setError("");
    try {
      const result = await createPortfolioReview(selectedId);
      setReviews((rows) => result.reused
        ? (rows.some((row) => row.id === result.review.id) ? rows : [result.review, ...rows])
        : [result.review, ...rows]);
    } catch (err) {
      setError(err.message);
    } finally {
      setReviewBusy(false);
    }
  };

  const runScenario = async () => {
    if (!selectedId || !scenarioQuestion.trim()) return;
    setScenarioBusy(true);
    setError("");
    try {
      setScenarioResult(await runPortfolioScenario(selectedId, scenarioQuestion.trim()));
    } catch (err) {
      setError(err.message);
    } finally {
      setScenarioBusy(false);
    }
  };

  const removePortfolio = async () => {
    if (!selected || !window.confirm(`Delete “${selected.name}” and its local portfolio record?`)) return;
    setError("");
    try {
      await deletePortfolio(selected.id);
      await refreshList();
    } catch (err) {
      setError(err.message);
    }
  };

  return (
    <div className="flex flex-col gap-3">
      <DestinationHeader view="following" className="mb-1" />
      <div className="flex flex-wrap items-center gap-2">
        <div>
          <div className="label-mono text-accent">Portfolio intelligence</div>
          <p className="mt-1 text-[12.5px] text-muted">Holdings, deterministic risk, thesis context, and explicit review checkpoints. No orders or personalized trade instructions.</p>
        </div>
        <div className="ml-auto flex flex-wrap items-center gap-2">
          {portfolios.length > 0 && <select className={cx(controlClass, "w-auto min-w-[190px]")} value={selectedId} onChange={(event) => setSelectedId(event.target.value)}>{portfolios.map((portfolio) => <option key={portfolio.id} value={portfolio.id}>{portfolio.name}</option>)}</select>}
          <Button size="sm" onClick={() => user ? setCreating(true) : promptSignIn()}>+ New portfolio</Button>
          {selected && <Button size="sm" variant="danger" onClick={removePortfolio}>Delete</Button>}
        </div>
      </div>

      {error && <div className="rounded-xl border border-down/35 bg-down/5 px-4 py-3 text-[12.5px] text-down">{error}</div>}
      {creating && <PortfolioCreator onCreated={(portfolio) => { setCreating(false); setPortfolios((rows) => [portfolio, ...rows]); setSelectedId(portfolio.id); }} onCancel={() => setCreating(false)} />}

      {loading ? <Panel><div className="py-8 text-center text-[12px] text-muted">Loading portfolios…</div></Panel> : !user ? <EmptyState title="Sign in to create a portfolio" action={<Button variant="primary" onClick={promptSignIn}>Sign in</Button>}>Portfolio holdings and reviews are private, per-user records.</EmptyState> : portfolios.length === 0 && !creating ? <EmptyState title="No portfolio yet" action={<Button variant="primary" onClick={() => setCreating(true)}>Create portfolio</Button>}>Start with the holdings and cash you already have. The first useful output is concentration and exposure—not a trading recommendation.</EmptyState> : draft ? (
        <>
          <ChangesPanel snapshot={snapshots[0]} onSnapshot={saveSnapshot} busy={snapshotBusy} />
          <EventImpactPanel data={eventImpact} loading={eventImpactLoading} error={eventImpactError} />
          <IntelligenceSummary intelligence={intelligence} />
          <ReviewPanel review={reviews[0]} currentContextVersion={intelligence?.contextVersion} onReview={generateReview} busy={reviewBusy} />
          <ScenarioPanel result={scenarioResult} question={scenarioQuestion} setQuestion={setScenarioQuestion} onRun={runScenario} busy={scenarioBusy} />
          <Panel title="Portfolio settings" actions={<div className="flex items-center gap-2"><span className="text-[11px] text-muted">Version {draft.version}</span><Button variant="primary" size="sm" onClick={save} disabled={saving}>{saving ? "Saving…" : "Save and recalculate"}</Button></div>}>
            <div className="grid gap-3 md:grid-cols-[1fr_160px]">
              <Field label="Name"><input className={controlClass} value={draft.name || ""} onChange={(event) => setDraft((current) => ({ ...current, name: event.target.value }))} /></Field>
              <Field label="Base currency"><input className={cx(controlClass, "font-mono uppercase")} maxLength={3} value={draft.baseCurrency || ""} onChange={(event) => setDraft((current) => ({ ...current, baseCurrency: event.target.value.toUpperCase() }))} /></Field>
            </div>
          </Panel>
          <HoldingsEditor draft={draft} setDraft={setDraft} />
          <TargetsEditor draft={draft} setDraft={setDraft} />
          <PolicyEditor draft={draft} setDraft={setDraft} />
          <p className="px-1 text-[10.5px] leading-relaxed text-dim">Context {intelligence?.contextVersion || "not calculated"} · calculated {intelligence?.asOf ? `${relFromISO(intelligence.asOf)} (${intelligence.asOf})` : "—"}. Portfolio monitoring is research tooling, not investment advice or trade execution.</p>
        </>
      ) : null}
    </div>
  );
}
