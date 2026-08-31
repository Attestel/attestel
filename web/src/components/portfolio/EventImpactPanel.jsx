import { Badge, EmptyState, Panel } from "../ui/index.js";
import HistoricalSensitivity from "./HistoricalSensitivity.jsx";

// EventImpactPanel — Phase 3: which scheduled events touch this portfolio, and how much weight is
// exposed to each.
//
// It renders SERVER-COMPUTED facts and adds no arithmetic of its own. Weights, the combined exposed
// weight and the no-double-counting rule all live in `journal/event_impact.go`; this file formats
// them. That split is not stylistic — a percentage computed in the browser is a percentage nobody
// can reproduce from the record.
//
// It is a REVIEW surface. There is no action here: no order, no size, no allocation, no
// "rebalance". The most it does is deep-link to the event, the company and the thesis so the user
// can go and look.

const RELATIONSHIP_LABEL = {
  direct: "Direct",
  sector: "Sector",
  supplier: "Supplier",
  customer: "Customer",
  competitor: "Competitor",
  macro: "Macro",
  factor: "Factor",
};

const BAND_TONE = { primary: "accent", secondary: "info", contextual: "dim" };

function pct(value) {
  if (value === null || value === undefined) return "—";
  return `${(value * 100).toFixed(1)}%`;
}

function when(iso) {
  if (!iso) return "—";
  return iso.replace("T", " ").replace(":00Z", "Z");
}

function Holding({ holding }) {
  return (
    <li className="flex flex-col gap-1 border-t border-line/50 px-4 py-3 first:border-t-0">
      <div className="flex flex-wrap items-center gap-2">
        <a className="num text-[13px] font-semibold text-fg hover:text-accent" href={holding.researchDeepLink}>
          {holding.ticker}
        </a>
        <Badge variant={BAND_TONE[holding.relevanceBand] || "dim"}>
          {RELATIONSHIP_LABEL[holding.relationship] || holding.relationship}
        </Badge>
        <span className="num text-[12px] text-muted">{pct(holding.weight)} of portfolio</span>
        {holding.sourceIsSynthetic && <Badge variant="warn">synthetic price</Badge>}
        {holding.valuationSource === "unavailable" && <Badge variant="warn">not valued</Badge>}
      </div>
      <p className="text-[11.5px] leading-relaxed text-muted">{holding.relationshipReason}</p>
      {holding.matchedCondition && (
        <p className="text-[11.5px] leading-relaxed text-dim">
          Touches a <span className="text-muted">{holding.matchedCondition.field}</span> on your
          thesis: “{holding.matchedCondition.text}”
        </p>
      )}
    </li>
  );
}

function Impact({ impact }) {
  return (
    <article className="rounded-xl border border-line bg-panel">
      <header className="flex flex-col gap-1 border-b border-line px-4 py-3">
        <div className="flex flex-wrap items-center gap-2">
          <a className="text-[13px] font-semibold text-fg hover:text-accent" href={impact.event.deepLink}>
            {impact.event.title}
          </a>
          <Badge variant={impact.event.confirmed ? "ok" : "outline"}>{impact.event.status}</Badge>
          <Badge variant="dim">{impact.event.sourceTier}</Badge>
        </div>
        <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-[11.5px] text-muted">
          <span className="num">{when(impact.event.scheduledAt)}</span>
          <span>
            exposed weight{" "}
            <span className="num text-fg">{pct(impact.exposedWeight)}</span>
            {!impact.weightsComplete && <span className="text-warn"> (incomplete)</span>}
          </span>
          <span>{impact.affectedHoldings.length} holding(s)</span>
          <span>{impact.relationshipTypes.join(" · ")}</span>
        </div>
        {impact.event.sourceUrl && (
          <a
            className="text-[11px] text-dim underline decoration-dotted hover:text-muted"
            href={impact.event.sourceUrl}
            target="_blank"
            rel="noreferrer noopener"
          >
            {impact.event.source} source
          </a>
        )}
      </header>

      <ul>
        {impact.affectedHoldings.map((holding) => (
          <li key={`${impact.eventId}-${holding.ticker}`}>
            <ul>
              <Holding holding={holding} />
            </ul>
          </li>
        ))}
      </ul>

      {/* Phase 4. Historical sensitivity for THIS event's taxonomy, rendered only when the
          evidence gate passes. The component itself owns the refusal: below the sample floor it
          shows "insufficient history" and no number at all. */}
      <details className="border-t border-line px-4 py-2.5">
        <summary className="cursor-pointer label-mono text-muted hover:text-fg">
          Historical sensitivity
        </summary>
        <div className="mt-2">
          <HistoricalSensitivity
            ticker={impact.event.ticker || impact.affectedHoldings[0]?.ticker}
            kind={impact.event.kind}
          />
        </div>
      </details>

      {impact.degraded?.length > 0 && (
        <p className="border-t border-line px-4 py-2 text-[11px] text-warn">
          {impact.degraded.join(", ")} — the exposure above is not the whole picture.
        </p>
      )}
    </article>
  );
}

export default function EventImpactPanel({ data, loading, error }) {
  const impacts = data?.impacts || [];
  const degraded = data?.degraded || [];

  return (
    <Panel
      title="Event impact"
      badges={<Badge variant="accent">weights calculated in code</Badge>}
    >
      {loading && <p className="text-[12px] text-muted">Loading event impact…</p>}
      {error && <EmptyState title="Event impact unavailable">{error}</EmptyState>}

      {degraded.length > 0 && (
        <p className="mb-3 text-[11.5px] text-warn">
          Degraded: {degraded.join(", ")}. What is shown may be incomplete — that is not evidence
          that nothing is coming.
        </p>
      )}

      {!loading && !error && impacts.length === 0 && (
        <EmptyState title="No scheduled events touch these holdings">
          Nothing in the stored calendar window bears on the companies in this portfolio. Missing
          coverage is shown as missing; the app does not invent a date to fill the gap.
        </EmptyState>
      )}

      <div className="flex flex-col gap-3">
        {impacts.map((impact) => (
          <Impact key={impact.eventId} impact={impact} />
        ))}
      </div>

      {impacts.length > 0 && (
        <p className="mt-3 text-[10.5px] leading-relaxed text-dim">
          Exposure counts each holding once, whatever number of relationships connect it to the
          event. This is a review surface: it does not suggest a size, an allocation or a trade.
        </p>
      )}
    </Panel>
  );
}
