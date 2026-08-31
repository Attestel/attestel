import { useEffect, useState } from "react";
import { fetchReads, fetchTranscriptHistory, fetchCommitteeHistory } from "../../lib/api.js";
import { EmptyState, SkeletonText } from "../ui/index.js";
import { Micro, Tag } from "../terminal/bits.jsx";
import Placeholder, { SectionShell } from "./Placeholder.jsx";
import { relFromISO } from "../../lib/format.js";

// HistorySection — how the record of this company has moved over time.
//
// Three stores genuinely keep dated snapshots today, and each is read as itself:
//   * daily analytical reads          llm.snapshots -> GET /api/reads/{t}
//   * transcript analyses            llm.snapshots -> GET /api/transcript/{t}
//   * committee snapshots            llm.snapshots -> GET /api/committee/{t}/history
//
// They are shown as THREE SEPARATE lists, not merged. There is no join key between them and no stored
// thesis/confidence/decision history to weave them into, so merging them into one timeline in the client
// would manufacture a relationship that does not exist (guide §11.4 — "do not fake graph semantics in the
// UI"). The unified thesis history is Step 04/08 work and is named as missing below.
//
// All three are plain directory reads of already-stored snapshots — no model call.
export default function HistorySection({ ticker }) {
  const [reads, setReads] = useState(null);
  const [transcripts, setTranscripts] = useState(null);
  const [committee, setCommittee] = useState(null);

  useEffect(() => {
    let alive = true;
    setReads(null);
    setTranscripts(null);
    setCommittee(null);
    fetchReads(ticker, 10)
      .then((r) => alive && setReads(r.reads || []))
      .catch(() => alive && setReads("error"));
    fetchTranscriptHistory(ticker)
      .then((r) => alive && setTranscripts(r.analyses || []))
      .catch(() => alive && setTranscripts("error"));
    fetchCommitteeHistory(ticker)
      .then((r) => alive && setCommittee(r.snapshots || []))
      .catch(() => alive && setCommittee("error"));
    return () => {
      alive = false;
    };
  }, [ticker]);

  return (
    <div className="flex flex-col gap-3">
      <SectionShell
        title={`${ticker} · History`}
        tags={<Tag tone="outline">stored snapshots</Tag>}
        note="What was already saved, by source. These are separate records with no stored link between them — a single timeline of facts, thesis edits, confidence, and decisions arrives with the versioned thesis."
      >
        <Group label="Daily analytical read" state={reads} empty="No stored daily reads yet. One snapshot is kept per calendar day on the 1D timeframe.">
          {(rows) =>
            rows.map((r) => (
              <li key={r.date} className="flex flex-wrap items-start gap-x-2.5 gap-y-1 px-[22px] py-2.5">
                <span className="num w-[86px] shrink-0 text-[11.5px] font-semibold text-fg/85">{r.date}</span>
                <Tag tone={String(r.modelUsed || "").includes("stub") ? "warn" : "llm"}>
                  {r.modelUsed || "model"}
                </Tag>
                <span className="min-w-0 flex-1 text-[12px] leading-snug text-muted">
                  {r.structured?.summary || r.structured?.headline || "—"}
                </span>
                {r.regimeSnapshot?.trend && <Tag tone="outline">{r.regimeSnapshot.trend}</Tag>}
              </li>
            ))
          }
        </Group>

        <Group
          label="Transcript analyses"
          state={transcripts}
          empty="No transcripts analysed for this company yet. Paste one in Evidence → Transcripts."
        >
          {(rows) =>
            rows.map((r) => (
              <li key={r.key || r.label} className="flex flex-wrap items-start gap-x-2.5 gap-y-1 px-[22px] py-2.5">
                <span className="num w-[86px] shrink-0 text-[11.5px] font-semibold text-fg/85">
                  {r.label || r.key}
                </span>
                {r.structured?.tone?.confidence && (
                  <Tag tone="outline">confidence {r.structured.tone.confidence}</Tag>
                )}
                {r.structured?.toneShift && <Tag tone="warn">tone shift</Tag>}
                <span className="min-w-0 flex-1 text-[12px] leading-snug text-muted">
                  {r.structured?.summary || "—"}
                </span>
                <span className="num text-[10.5px] text-muted/70">{relFromISO(r.timestamp)}</span>
              </li>
            ))
          }
        </Group>

        <Group
          label="Committee snapshots"
          state={committee}
          empty="No committee runs stored yet. Start one in Perspectives → Review with perspectives."
        >
          {(rows) =>
            rows.map((r) => (
              <li key={r.date} className="flex flex-wrap items-start gap-x-2.5 gap-y-1 px-[22px] py-2.5">
                <span className="num w-[86px] shrink-0 text-[11.5px] font-semibold text-fg/85">{r.date}</span>
                {r.consensus?.stance && <Tag tone="outline">{r.consensus.stance}</Tag>}
                <span className="min-w-0 flex-1 text-[12px] leading-snug text-muted">
                  {r.judge?.summary || "—"}
                </span>
              </li>
            ))
          }
        </Group>
      </SectionShell>

      <SectionShell title="Thesis & confidence history" tags={<Tag tone="outline">not built</Tag>}>
        <Placeholder title="No versioned thesis history yet" step="Steps 04 & 08">
          A thesis is currently a single free-text claim with one latest evidence check — there are no
          field-level versions, no timestamped confidence changes, and no record of which assumption moved
          when. Once the thesis becomes a versioned object, that timeline appears here alongside the stored
          snapshots above.
        </Placeholder>
      </SectionShell>
    </div>
  );
}

// Group renders one source's list, or its own loading / error / empty state. `state` is null (loading),
// "error", or an array.
function Group({ label, state, empty, children }) {
  return (
    <>
      <div className="flex h-8 items-center border-b border-t border-line bg-panel2/30 px-[22px] first:border-t-0">
        <Micro>{label}</Micro>
        {Array.isArray(state) && state.length > 0 && (
          <span className="num ml-2 text-[10.5px] text-muted/60">{state.length}</span>
        )}
      </div>
      {state === null ? (
        <div className="px-[22px] py-3">
          <SkeletonText lines={2} />
        </div>
      ) : state === "error" ? (
        <p className="px-[22px] py-3 text-[11.5px] text-muted">
          Unavailable — that service may be unreachable. The other sources are unaffected.
        </p>
      ) : state.length === 0 ? (
        <div className="px-[22px] py-3">
          <EmptyState title="Nothing stored yet">{empty}</EmptyState>
        </div>
      ) : (
        <ul className="divide-y divide-line">{children(state)}</ul>
      )}
    </>
  );
}
