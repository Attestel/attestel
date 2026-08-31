import { useEffect, useState } from "react";
import { Tag } from "../../terminal/bits.jsx";
import {
  AuthRequiredError,
  ITEM_LIST_LABELS,
  ThesisApiError,
  fmtWhenExact,
  listThesisVersions,
} from "../../../lib/thesisApi.js";

// VersionHistory — the thesis's append-only change log, from GET /theses/{id}/versions.
//
// Compact by design: one line per event with the fields that moved, expandable to the prior scalar
// values. It shows what the server actually stored and nothing more:
//
//   * an empty list means this record has no recorded history. Step 04 deliberately writes NO
//     synthetic "created" event for a legacy thesis, because inventing history for a record whose
//     history was never captured would make this list a lie (§1.5). So an old thesis reads
//     "no recorded history yet" rather than a fabricated origin entry.
//   * `itemDelta` carries item IDs only, never item text (§1.4 keeps the record bounded), so a list
//     change is reported as counts — "2 added, 1 edited" — not as a text diff we cannot reconstruct.
//   * every entry is user-authored: system writes (`lastCheck`, `lastReview`) produce no version
//     event at all. The `actor` field is still shown so that stays visible rather than assumed.

const FIELD_LABELS = {
  claim: "Claim",
  status: "Lifecycle",
  closeReason: "Close reason",
  horizon: "Time horizon",
  confidence: "Your confidence",
  notes: "Notes",
  ...ITEM_LIST_LABELS,
};

const labelFor = (f) => FIELD_LABELS[f] || f;

function DeltaSummary({ delta }) {
  const parts = [];
  if (delta.added?.length) parts.push(`${delta.added.length} added`);
  if (delta.removed?.length) parts.push(`${delta.removed.length} removed`);
  if (delta.edited?.length) parts.push(`${delta.edited.length} edited`);
  if (!parts.length) return null;
  return <span className="text-muted/70"> ({parts.join(", ")})</span>;
}

export default function VersionHistory({ thesisId, refreshKey }) {
  const [state, setState] = useState({ loading: true, versions: [], truncated: false, error: null });
  const [open, setOpen] = useState(null);

  useEffect(() => {
    if (!thesisId) return;
    let alive = true;
    setState((s) => ({ ...s, loading: true, error: null }));
    listThesisVersions(thesisId)
      .then((r) => {
        if (alive) setState({ loading: false, versions: r.versions, truncated: r.truncated, error: null });
      })
      .catch((e) => {
        if (!alive) return;
        // A guest reading history is not an error worth shouting about; neither is a service that is
        // down. Both degrade to an explanatory line, because history is context, not the main task.
        const msg =
          e instanceof AuthRequiredError
            ? "Sign in to see this thesis's history."
            : e instanceof ThesisApiError && e.isUnavailable
              ? "History is unavailable — the journal service is not responding."
              : e.message;
        setState({ loading: false, versions: [], truncated: false, error: msg });
      });
    return () => {
      alive = false;
    };
  }, [thesisId, refreshKey]);

  if (state.loading) {
    return <p className="text-[11.5px] text-muted/70">Loading history…</p>;
  }
  if (state.error) {
    return <p className="text-[11.5px] text-muted">{state.error}</p>;
  }
  if (!state.versions.length) {
    return (
      <p className="text-[11.5px] leading-relaxed text-muted">
        No recorded history yet. Revisions you make from here on are logged — earlier edits were not
        tracked, and this app will not invent entries it never captured.
      </p>
    );
  }

  return (
    <div className="flex flex-col gap-1.5">
      <ol className="flex flex-col divide-y divide-line">
        {state.versions.map((v) => {
          const isOpen = open === v.id;
          const hasDetail = Boolean(v.reason) || Object.keys(v.before || {}).length > 0;
          return (
            <li key={v.id} className="py-1.5">
              <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
                <span className="font-mono text-[10.5px] text-muted/60">{fmtWhenExact(v.at)}</span>
                <Tag tone={v.actor === "system" ? "llm" : "outline"}>
                  {v.actor === "system" ? v.actorDetail || "system" : "you"}
                </Tag>
                <span className="text-[12px] text-fg/85">
                  {(v.changedFields || []).map((f, i) => (
                    <span key={f}>
                      {i > 0 && <span className="text-muted/50"> · </span>}
                      {labelFor(f)}
                      {v.itemDelta?.[f] && <DeltaSummary delta={v.itemDelta[f]} />}
                    </span>
                  ))}
                </span>
                {hasDetail && (
                  <button
                    type="button"
                    onClick={() => setOpen(isOpen ? null : v.id)}
                    aria-expanded={isOpen}
                    className="ml-auto text-[11px] text-info underline decoration-dotted hover:text-fg"
                  >
                    {isOpen ? "hide" : "what changed"}
                  </button>
                )}
              </div>

              {isOpen && (
                <div className="mt-1.5 flex flex-col gap-1 rounded-md border border-line bg-panel2/50 px-2.5 py-2">
                  {v.reason && (
                    <p className="break-words text-[11.5px] text-muted">
                      <span className="font-semibold text-fg/80">Reason: </span>
                      {v.reason}
                    </p>
                  )}
                  {Object.entries(v.before || {}).map(([field, prior]) => (
                    <p key={field} className="break-words text-[11.5px] leading-relaxed text-muted">
                      <span className="font-semibold text-fg/80">{labelFor(field)} was: </span>
                      {prior === "" || prior == null ? (
                        <span className="italic text-muted/60">empty</span>
                      ) : (
                        String(prior)
                      )}
                    </p>
                  ))}
                </div>
              )}
            </li>
          );
        })}
      </ol>

      {state.truncated && (
        <p className="text-[11px] text-muted/60">
          Older entries are not shown — history is capped so one thesis record stays bounded.
        </p>
      )}
    </div>
  );
}
