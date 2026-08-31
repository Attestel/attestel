import { useCallback, useEffect, useMemo, useState } from "react";
import { Badge, Button, EmptyState, SkeletonText, controlClass, useToast } from "../ui/index.js";
import { cx } from "../../lib/cx.js";
import { useAuth } from "../../auth/AuthContext.jsx";
import { AuthRequiredError } from "../../lib/api.js";
import EvidenceCard from "./EvidenceCard.jsx";
import SaveEvidenceDialog from "./SaveEvidenceDialog.jsx";
import {
  BEARINGS,
  evidenceErrorMessage,
  listEvidence,
  listEvidenceLinks,
  unlinkEvidence,
  updateEvidenceLink,
} from "../../lib/evidenceApi.js";

// ThesisEvidencePanel — the evidence attached to ONE thesis, grouped by what it does to the claim.
//
// This is the Evidence lane's component for the thesis workspace. It takes a thesis id as input and
// owns nothing about the thesis itself: it never reads, writes, or renders a thesis field. The Wave 2
// integrator places it beside the structured thesis editor.
//
// Design decisions worth keeping:
//
//   · CONTRADICTING EVIDENCE IS A PEER GROUP, not a footnote. It renders in the same type, at the same
//     level, with its own count. A research tool whose UI makes disconfirming evidence easy to skip is
//     working against the user.
//   · An EMPTY group says so plainly. It never shows a placeholder card or a "coming soon" state, and
//     the panel never implies a link that is not persisted (guide §11.4).
//   · UNLINK IS NOT DELETE, and the button says which. The record survives with its provenance and can
//     be attached elsewhere; destroying a capture is a separate, deliberate act.
//   · Every item shows source, age, capture time and live/seed/synthetic state, because a thesis built
//     on a six-month-old headline and one built on this morning's filing must not look the same.

const GROUPS = BEARINGS.map((b) => b.id); // supports · contradicts · context

const GROUP_META = {
  supports: {
    title: "Supporting",
    blurb: "Evidence you read as backing the claim.",
    accent: "text-up",
  },
  contradicts: {
    title: "Contradicting",
    blurb: "Evidence that cuts against the claim. Keeping this visible is the point of the file.",
    accent: "text-down",
  },
  context: {
    title: "Context",
    blurb: "Relevant background that neither confirms nor challenges the claim.",
    accent: "text-info",
  },
};

export default function ThesisEvidencePanel({ thesisId, ticker, thesisLabel, timeframe = "1D", className }) {
  const { user, promptSignIn } = useAuth();
  const toast = useToast();

  const [state, setState] = useState({ status: "idle", links: [], records: [] });
  const [err, setErr] = useState(null);
  const [pending, setPending] = useState(null); // link id being mutated
  const [confirmUnlink, setConfirmUnlink] = useState(null); // link id awaiting confirmation
  const [noteOpen, setNoteOpen] = useState(false);

  const load = useCallback(async () => {
    if (!thesisId) {
      setState({ status: "ready", links: [], records: [] });
      return;
    }
    setState((s) => ({ ...s, status: s.status === "ready" ? "reloading" : "loading" }));
    setErr(null);
    try {
      // Two reads, joined here by id. Both sides are durable stored records — this is a join over
      // persisted data, not a client-side graph invented for display.
      const [links, { evidence }] = await Promise.all([
        listEvidenceLinks({ thesisId }),
        listEvidence({ thesisId, limit: 200 }),
      ]);
      setState({ status: "ready", links, records: evidence });
    } catch (e) {
      setErr(evidenceErrorMessage(e));
      setState({ status: "error", links: [], records: [] });
    }
  }, [thesisId]);

  useEffect(() => {
    load();
  }, [load]);

  const byId = useMemo(() => {
    const m = new Map();
    state.records.forEach((r) => m.set(r.id, r));
    return m;
  }, [state.records]);

  // grouped pairs each link with its record. A link whose record is missing is DROPPED rather than
  // rendered as a stub: showing "evidence unavailable" would present a broken reference as content.
  const grouped = useMemo(() => {
    const out = { supports: [], contradicts: [], context: [] };
    state.links.forEach((l) => {
      const record = byId.get(l.evidenceId);
      if (!record) return;
      (out[l.bearing] || out.context).push({ link: l, record });
    });
    return out;
  }, [state.links, byId]);

  const total = grouped.supports.length + grouped.contradicts.length + grouped.context.length;
  const orphanCount = state.links.length - total;

  const changeBearing = async (link, bearing) => {
    if (bearing === link.bearing) return;
    setPending(link.id);
    try {
      await updateEvidenceLink(link.id, { bearing });
      await load();
    } catch (e) {
      if (e instanceof AuthRequiredError) {
        promptSignIn();
        return;
      }
      toast({ tone: "error", title: "Could not change the bearing", message: evidenceErrorMessage(e) });
    } finally {
      setPending(null);
    }
  };

  const doUnlink = async (link) => {
    setPending(link.id);
    try {
      await unlinkEvidence(link.id);
      setConfirmUnlink(null);
      await load();
      toast({
        tone: "success",
        title: "Unlinked",
        message: "The capture is still in your evidence — only the link to this thesis was removed.",
      });
    } catch (e) {
      if (e instanceof AuthRequiredError) {
        promptSignIn();
        return;
      }
      toast({ tone: "error", title: "Could not unlink", message: evidenceErrorMessage(e) });
    } finally {
      setPending(null);
    }
  };

  // ---- shells ----

  if (!thesisId) {
    return (
      <PanelShell ticker={ticker} className={className}>
        <div className="px-[18px] py-4">
          <EmptyState title="No thesis selected">
            Evidence attaches to a specific claim. Write or select a thesis for {ticker}, then save facts,
            filings, quotations and news against it.
          </EmptyState>
        </div>
      </PanelShell>
    );
  }

  if (!user) {
    return (
      <PanelShell ticker={ticker} className={className}>
        <div className="px-[18px] py-4">
          <EmptyState
            title="Evidence is saved to your account"
            action={
              <Button variant="primary" size="sm" onClick={promptSignIn}>
                Sign in
              </Button>
            }
          >
            Captures and their thesis links are per-user records, so they need a signed-in account.
          </EmptyState>
        </div>
      </PanelShell>
    );
  }

  if (state.status === "loading" || state.status === "idle") {
    return (
      <PanelShell ticker={ticker} className={className}>
        <div className="px-[18px] py-4">
          <SkeletonText lines={5} />
        </div>
      </PanelShell>
    );
  }

  if (state.status === "error") {
    return (
      <PanelShell ticker={ticker} className={className}>
        <div className="px-[18px] py-4">
          <EmptyState
            title="Evidence could not be loaded"
            action={
              <Button variant="secondary" size="sm" onClick={load}>
                Try again
              </Button>
            }
          >
            {err}
          </EmptyState>
        </div>
      </PanelShell>
    );
  }

  return (
    <PanelShell
      ticker={ticker}
      className={className}
      counts={{
        total,
        contradicting: grouped.contradicts.length,
      }}
      right={
        <Button variant="secondary" size="xs" onClick={() => setNoteOpen(true)}>
          Add your own observation
        </Button>
      }
    >
      {total === 0 ? (
        <div className="px-[18px] py-4">
          <EmptyState title="Nothing attached to this claim yet">
            Nothing has been attached to this thesis. Use <span className="text-fg/80">Save to thesis</span>{" "}
            in the Evidence section to attach a computed fact, a filing, a headline, a transcript quotation
            or a catalyst — and attach what argues against you as well as what argues for you.
          </EmptyState>
        </div>
      ) : (
        <div className="flex flex-col divide-y divide-line">
          {GROUPS.map((g) => (
            <Group
              key={g}
              meta={GROUP_META[g]}
              items={grouped[g]}
              pending={pending}
              confirmUnlink={confirmUnlink}
              onAskUnlink={setConfirmUnlink}
              onUnlink={doUnlink}
              onBearing={changeBearing}
            />
          ))}
        </div>
      )}

      {orphanCount > 0 && (
        // Honest rather than silent: a link whose record is gone is reported as a count, never
        // rendered as a card. It can only happen if a record was deleted outside this view.
        <p className="border-t border-line px-[18px] py-2.5 text-[11px] leading-relaxed text-warn">
          {orphanCount} link{orphanCount === 1 ? "" : "s"} on this thesis point at a capture that is no
          longer stored, so {orphanCount === 1 ? "it is" : "they are"} not shown.
        </p>
      )}

      <SaveEvidenceDialog
        open={noteOpen}
        ticker={ticker}
        timeframe={timeframe}
        item={{
          key: "user-note",
          kind: "user_note",
          selector: { title: thesisLabel ? `Observation on: ${thesisLabel}` : "" },
          title: `${ticker} — your observation`,
          preview: [
            {
              label: "Stored as",
              value: "Your own observation, captured against this company at this moment.",
            },
          ],
          excerptNote:
            "Your words are stored as written. The server records the company, the capture time, and whether the prices on screen were real or synthetic.",
        }}
        onClose={() => setNoteOpen(false)}
        onSaved={() => {
          setNoteOpen(false);
          load();
        }}
      />
    </PanelShell>
  );
}

function PanelShell({ ticker, counts, right, children, className }) {
  return (
    <section
      className={cx(
        "overflow-hidden surface-card",
        className
      )}
    >
      <div className="flex flex-wrap items-center gap-x-2.5 gap-y-2 border-b border-line px-[22px] py-3">
        <span className="text-[14px] font-semibold text-fg">{ticker} · Evidence on this thesis</span>
        {counts && (
          <>
            <Badge variant="outline">
              {counts.total} item{counts.total === 1 ? "" : "s"}
            </Badge>
            <Badge variant={counts.contradicting > 0 ? "down" : "outline"}>
              {counts.contradicting} contradicting
            </Badge>
          </>
        )}
        {right && <span className="ml-auto flex shrink-0 items-center gap-2">{right}</span>}
      </div>
      <p className="border-b border-line bg-panel2/30 px-[22px] py-2.5 text-[11.5px] leading-relaxed text-muted">
        Each item shows where it came from, when the source is from, when you captured it, and whether the
        underlying data was live, seed, or synthetic. Unlinking removes the connection to this thesis and
        keeps the capture.
      </p>
      {children}
    </section>
  );
}

function Group({ meta, items, pending, confirmUnlink, onAskUnlink, onUnlink, onBearing }) {
  return (
    <div className="px-[18px] py-3.5">
      <div className="mb-2 flex flex-wrap items-baseline gap-x-2.5 gap-y-1">
        <h3 className={cx("text-[12.5px] font-semibold uppercase tracking-[0.07em]", meta.accent)}>
          {meta.title}
        </h3>
        <span className="text-[11px] text-muted">
          {items.length} item{items.length === 1 ? "" : "s"}
        </span>
        <span className="w-full text-[11px] leading-relaxed text-muted/85 sm:w-auto sm:flex-1">{meta.blurb}</span>
      </div>

      {items.length === 0 ? (
        <p className="rounded-md border border-dashed border-line px-3 py-2.5 text-[11.5px] text-muted">
          Nothing filed here.
        </p>
      ) : (
        <ul className="flex flex-col gap-2">
          {items.map(({ link, record }) => (
            <li key={link.id}>
              <EvidenceCard
                record={record}
                right={
                  <>
                    {link.authoredBy === "system" && (
                      <Badge variant="info" title="Created automatically from an alert you set on this thesis.">
                        auto
                      </Badge>
                    )}
                    <select
                      aria-label="Bearing on the claim"
                      className={cx(controlClass, "w-[124px] px-2 py-1 text-[11px]")}
                      value={link.bearing}
                      disabled={pending === link.id}
                      onChange={(e) => onBearing(link, e.target.value)}
                    >
                      {BEARINGS.map((b) => (
                        <option key={b.id} value={b.id}>
                          {b.label}
                        </option>
                      ))}
                    </select>
                  </>
                }
                footer={
                  <div className="flex flex-wrap items-center gap-2 border-t border-line pt-2">
                    {link.note && (
                      <span className="min-w-0 flex-1 text-[11px] leading-relaxed text-muted">
                        Link note: {link.note}
                      </span>
                    )}
                    {confirmUnlink === link.id ? (
                      <span className="ml-auto flex items-center gap-1.5">
                        <span className="text-[11px] text-muted">Remove from this thesis?</span>
                        <Button
                          variant="danger"
                          size="xs"
                          disabled={pending === link.id}
                          onClick={() => onUnlink(link)}
                        >
                          {pending === link.id ? "Unlinking…" : "Unlink"}
                        </Button>
                        <Button variant="ghost" size="xs" onClick={() => onAskUnlink(null)}>
                          Keep
                        </Button>
                      </span>
                    ) : (
                      <Button
                        variant="ghost"
                        size="xs"
                        className="ml-auto"
                        title="Removes the link only — the capture stays in your evidence."
                        onClick={() => onAskUnlink(link.id)}
                      >
                        Unlink
                      </Button>
                    )}
                  </div>
                }
              />
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
