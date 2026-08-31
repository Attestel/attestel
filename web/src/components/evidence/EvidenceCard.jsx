import { Badge, ProvenanceBadge } from "../ui/index.js";
import { cx } from "../../lib/cx.js";
import {
  DATA_STATE_LABELS,
  EVIDENCE_KIND_LABELS,
  ageLabel,
  formatCapturedAt,
  formatSourceTime,
} from "../../lib/evidenceApi.js";

// EvidenceCard + Provenance — how one capture is rendered everywhere it appears.
//
// The rule this component exists to enforce (prompt 06: "never present missing provenance as
// complete"): an unknown publication time renders as "source time unknown", NOT as the capture date
// and never as today. A record that cannot say when its content is from says so, in the same place
// and the same type size as one that can. The absence is information.
//
// Three states are deliberately loud rather than tasteful:
//   · dataState "synthetic" — amber, always. It is invariant #1's visible half and is never softened.
//   · inference: true — a model-derived record can never be mistaken for something observed.
//   · excerptTruncated — the user must know the stored quotation is shorter than the source.

// Provenance — the one-line source trail: where it came from, when the source is from, when we took
// it, and whether the underlying data was real.
export function Provenance({ record, className }) {
  const {
    kind,
    provider,
    attribution,
    dataState,
    sourceTime,
    capturedAt,
    sourceUrl,
    sourceRef,
    inference,
    modelUsed,
    excerptTruncated,
    fact,
  } = record;

  return (
    <div className={cx("flex flex-col gap-1.5", className)}>
      <div className="flex flex-wrap items-center gap-1.5">
        <Badge variant="outline">{EVIDENCE_KIND_LABELS[kind] || kind}</Badge>
        <ProvenanceBadge kind={dataState} label={DATA_STATE_LABELS[dataState] || dataState} />
        {inference && (
          <Badge variant="info" title={modelUsed ? `Model: ${modelUsed}` : undefined}>
            model-derived
          </Badge>
        )}
        {excerptTruncated && (
          <Badge variant="warn" title="The stored quotation was shortened to the policy limit for this source type.">
            excerpt shortened
          </Badge>
        )}
      </div>

      <p className="text-[11px] leading-relaxed text-muted">
        <span className="text-fg/70">{attribution || provider || "source not recorded"}</span>
        {" · "}
        <span className={cx(!sourceTime && "text-warn/80")}>{formatSourceTime(sourceTime)}</span>
        {" · captured "}
        <span title={formatCapturedAt(capturedAt)}>{ageLabel(capturedAt)}</span>
      </p>

      {fact?.asOf && (
        // A computed value is not a timeless truth. Its computation date and timeframe travel with it
        // so a stale number can never read as current.
        <p className="text-[11px] text-muted/85">
          Computed for <span className="text-fg/70">{fact.asOf}</span>
          {fact.timeframe ? ` on the ${fact.timeframe} chart` : ""}
        </p>
      )}

      {sourceUrl && (
        <a
          href={sourceUrl}
          target="_blank"
          rel="noreferrer noopener"
          className="w-fit break-all text-[11px] text-accent underline decoration-accent/40 underline-offset-2 hover:decoration-accent"
        >
          {sourceUrl}
        </a>
      )}
      {!sourceUrl && sourceRef?.id && (
        <p className="break-all font-mono text-[10.5px] text-muted/70">
          {sourceRef.store}: {sourceRef.id}
          {sourceRef.locator ? ` · ${sourceRef.locator}` : ""}
        </p>
      )}
    </div>
  );
}

// FactValue renders a normalized fact as "key = value unit" without inventing formatting for a value
// the server stored as-is.
export function FactValue({ fact }) {
  if (!fact?.key) return null;
  const value = Array.isArray(fact.value) ? fact.value.join("; ") : String(fact.value ?? "—");
  return (
    <p className="font-mono text-[12px] text-fg">
      {fact.key} = {value}
      {fact.unit ? ` ${fact.unit}` : ""}
    </p>
  );
}

// EvidenceCard — one stored capture. `right` takes the caller's actions (unlink, change bearing) so
// this component stays presentational and the same card serves the save preview and the thesis list.
export default function EvidenceCard({ record, right, footer, className }) {
  return (
    <article className={cx("flex flex-col gap-2 rounded-lg border border-line bg-panel2/40 px-3.5 py-3", className)}>
      <div className="flex items-start gap-3">
        <div className="min-w-0 flex-1">
          <h4 className="text-[13px] font-semibold leading-snug text-fg">{record.title || "(untitled capture)"}</h4>
          <FactValue fact={record.fact} />
        </div>
        {right && <div className="flex shrink-0 items-center gap-1.5">{right}</div>}
      </div>

      {record.excerpt && (
        <blockquote className="border-l-2 border-line2 pl-2.5 text-[12px] leading-relaxed text-muted">
          {record.excerpt}
        </blockquote>
      )}

      {record.note && (
        <p className="rounded-md bg-panel/60 px-2.5 py-2 text-[12px] leading-relaxed text-fg/85">
          <span className="font-semibold uppercase tracking-wide text-[10px] text-muted">Your note · </span>
          {record.note}
        </p>
      )}

      <Provenance record={record} />
      {footer}
    </article>
  );
}
