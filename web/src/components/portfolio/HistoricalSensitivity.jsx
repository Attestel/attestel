import { useCallback, useEffect, useState } from "react";
import { fetchSensitivity } from "../../lib/api.js";
import { Badge } from "../ui/index.js";

// HistoricalSensitivity — Phase 4's aggregate, rendered ONLY when the evidence gate passes.
//
// THE REFUSAL IS THE FEATURE. Below the server's sample floor this component renders the words
// "insufficient history" and the count it is short by, and NO number: no median, no frequency, no
// direction, not a greyed-out one and not one behind a tooltip. A percentage on a screen is acted
// on whatever is printed beside it, so there is deliberately no provisional mode.
//
// When the gate does pass it renders a DISTRIBUTION — sample count, median, dispersion, the
// positive/negative split, and the benchmark-relative figures — rather than one confident number,
// because one number is what invites the reader to treat history as a forecast.
//
// The component computes nothing. Every figure here was computed by `services/events` from stored,
// matured, non-synthetic reactions and arrives with its calculation version.

function pct(value) {
  if (typeof value !== "number" || !Number.isFinite(value)) return "—";
  const sign = value > 0 ? "+" : "";
  return `${sign}${(value * 100).toFixed(2)}%`;
}

function Distribution({ title, data }) {
  if (!data) return null;
  return (
    <div className="rounded-lg border border-line/70 bg-panel2/30 p-3">
      <div className="mb-1.5 label-mono text-muted">{title}</div>
      <dl className="grid grid-cols-2 gap-x-4 gap-y-1 text-[11.5px] sm:grid-cols-3">
        <div className="flex justify-between gap-2"><dt className="text-dim">median</dt><dd className="num text-fg">{pct(data.median)}</dd></div>
        <div className="flex justify-between gap-2"><dt className="text-dim">mean</dt><dd className="num text-muted">{pct(data.mean)}</dd></div>
        <div className="flex justify-between gap-2"><dt className="text-dim">std dev</dt><dd className="num text-muted">{pct(data.stdev)}</dd></div>
        <div className="flex justify-between gap-2"><dt className="text-dim">p25</dt><dd className="num text-muted">{pct(data.p25)}</dd></div>
        <div className="flex justify-between gap-2"><dt className="text-dim">p75</dt><dd className="num text-muted">{pct(data.p75)}</dd></div>
        <div className="flex justify-between gap-2"><dt className="text-dim">range</dt><dd className="num text-muted">{pct(data.min)} … {pct(data.max)}</dd></div>
        <div className="flex justify-between gap-2 sm:col-span-3">
          <dt className="text-dim">positive / negative</dt>
          <dd className="num text-muted">
            {data.positiveCount} / {data.negativeCount} ({(data.positiveFrequency * 100).toFixed(0)}% positive)
          </dd>
        </div>
      </dl>
    </div>
  );
}

export default function HistoricalSensitivity({ ticker, kind, horizon = "1d" }) {
  const [state, setState] = useState({ status: "idle", data: null, error: "" });

  const load = useCallback(async () => {
    if (!ticker && !kind) return;
    setState({ status: "loading", data: null, error: "" });
    try {
      const data = await fetchSensitivity({ ticker, kind, horizon });
      setState({ status: "ready", data, error: "" });
    } catch (err) {
      setState({ status: "error", data: null, error: err?.message || "unavailable" });
    }
  }, [ticker, kind, horizon]);

  useEffect(() => {
    load();
  }, [load]);

  if (state.status === "loading") {
    return <p className="text-[11px] text-dim">Checking stored history…</p>;
  }
  if (state.status === "error") {
    return <p className="text-[11px] text-warn">Historical sensitivity unavailable: {state.error}</p>;
  }

  const data = state.data;
  if (!data) return null;

  // THE GATE. Nothing numeric renders on this branch.
  if (!data.sufficient) {
    return (
      <div className="rounded-lg border border-dashed border-line2 bg-white/[.015] p-3">
        <div className="mb-1 flex flex-wrap items-center gap-2">
          <Badge variant="outline">insufficient history</Badge>
          <span className="text-[11.5px] text-muted">
            {data.sampleCount ?? 0} stored observation(s); {data.minimumSample ?? "the"} needed
            {typeof data.shortBy === "number" ? ` (short by ${data.shortBy})` : ""}
          </span>
        </div>
        <p className="m-0 text-[11px] leading-relaxed text-dim">
          {data.reason === "sensitivity unavailable"
            ? "The stored history could not be read, so nothing is shown. That is not evidence that history is neutral."
            : "No tendency is shown, because none can be supported by the evidence stored so far. Synthetic bars are excluded from this count."}
        </p>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2">
        <Badge variant="accent">{data.sampleCount} observations</Badge>
        <span className="num text-[11px] text-muted">{data.taxonomy?.horizon} window</span>
        {data.taxonomy?.kind && <span className="text-[11px] text-muted">{data.taxonomy.kind}</span>}
        <span className="text-[11px] text-dim">{data.calcVersion} · {data.sensitivityVersion}</span>
      </div>

      <Distribution title="Move" data={data.raw} />
      <Distribution title="Excess over benchmark" data={data.excess} />
      {data.excess === null && data.raw && (
        <p className="text-[11px] text-dim">
          Benchmark-relative figures are not shown: fewer than {data.minimumSample} of these
          observations have stored benchmark bars.
        </p>
      )}

      <p className="m-0 text-[10.5px] leading-relaxed text-dim">
        {data.interpretation} Synthetic bars are excluded
        {typeof data.benchmarkCoverage === "number"
          ? `; benchmark coverage ${(data.benchmarkCoverage * 100).toFixed(0)}%`
          : ""}
        .
      </p>
    </div>
  );
}
