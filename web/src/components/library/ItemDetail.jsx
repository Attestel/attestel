import { useEffect, useRef, useState } from "react";
import { Badge, Button, ProvenanceBadge, useToast } from "../ui/index.js";
import { cx } from "../../lib/cx.js";
import EvidenceCard from "../evidence/EvidenceCard.jsx";
import { AuthRequiredError } from "../../lib/api.js";
import {
  deleteEvidence,
  evidenceErrorMessage,
  unlinkEvidence,
} from "../../lib/evidenceApi.js";
import { OBJECT_TYPE_LABELS, fmtWhenExact } from "../../lib/libraryApi.js";

// ItemDetail — open one saved object.
//
// **Opening an item runs no model call and no refetch.** The record was already loaded by the sweep,
// so this component renders what is in memory. That is not an optimisation; it is the requirement.
// Library exists to revisit research cheaply, and a retrieval surface that re-ran a lens, a committee
// pipeline, or a transcript analysis every time you clicked a row would be a bill, not a library.
//
// Provenance and model metadata are shown for every type, including the awkward ones: a `stub` badge
// when the deterministic offline fallback produced the content, and the model id when a model did.
// A reader deciding whether to trust an old review needs to know which of those it is.

export default function ItemDetail({ item, thesisById, onClose, onChanged, onOpenResearch, onApplyLens }) {
  const toast = useToast();
  const panelRef = useRef(null);
  const restoreFocusRef = useRef(null);
  const [busy, setBusy] = useState("");
  const [confirmDelete, setConfirmDelete] = useState(false);

  useEffect(() => {
    if (!item) return undefined;
    restoreFocusRef.current = document.activeElement;
    const onKey = (e) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        onClose();
      }
    };
    document.addEventListener("keydown", onKey, true);
    const prev = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    const t = setTimeout(() => panelRef.current?.focus?.(), 40);
    return () => {
      document.removeEventListener("keydown", onKey, true);
      document.body.style.overflow = prev;
      clearTimeout(t);
      restoreFocusRef.current?.focus?.();
    };
  }, [item, onClose]);

  useEffect(() => setConfirmDelete(false), [item?.key]);

  if (!item) return null;

  const doUnlink = async (link) => {
    setBusy(link.id);
    try {
      await unlinkEvidence(link.id);
      toast({
        tone: "success",
        title: "Unlinked",
        message: "The capture is still saved — only its link to that thesis was removed.",
      });
      onChanged?.();
      onClose();
    } catch (e) {
      if (e instanceof AuthRequiredError) return;
      toast({ tone: "error", title: "Could not unlink", message: evidenceErrorMessage(e) });
    } finally {
      setBusy("");
    }
  };

  const doDelete = async (force) => {
    setBusy("delete");
    try {
      await deleteEvidence(item.id, { force });
      toast({ tone: "success", title: "Deleted", message: "The capture was removed from your evidence." });
      onChanged?.();
      onClose();
    } catch (e) {
      if (e instanceof AuthRequiredError) return;
      if (e.code === "evidence_linked") {
        // The delete was refused and NOTHING was written. Surfacing which theses are affected is the
        // point: the user is being told what a forced delete would break, not just that it failed.
        setConfirmDelete(true);
        toast({
          tone: "warn",
          title: "Still linked to a thesis",
          message: `${evidenceErrorMessage(e)} Deleting it will remove those links too.`,
        });
        return;
      }
      toast({ tone: "error", title: "Could not delete", message: evidenceErrorMessage(e) });
    } finally {
      setBusy("");
    }
  };

  const links = (item.links || []).filter((l) => !l.revoked);

  return (
    <div className="fixed inset-0 z-[90] flex items-start justify-center overflow-y-auto bg-black/60 px-4 py-8 backdrop-blur-sm">
      <button type="button" aria-label="Close" className="absolute inset-0 cursor-default" onClick={onClose} />
      <div
        ref={panelRef}
        tabIndex={-1}
        role="dialog"
        aria-modal="true"
        aria-labelledby="library-item-title"
        className="relative w-[min(720px,100%)] surface-card focus:outline-none"
      >
        <header className="flex items-start gap-3 border-b border-line px-5 py-3.5">
          <div className="min-w-0 flex-1">
            <div className="flex flex-wrap items-center gap-1.5">
              <Badge variant="outline">{OBJECT_TYPE_LABELS[item.type] || item.type}</Badge>
              {item.ticker && <Badge variant="outline">{item.ticker}</Badge>}
              {item.dataState && <ProvenanceBadge kind={item.dataState} label={item.dataState} />}
              {item.stub && <Badge variant="warn">offline stub</Badge>}
              {item.retried && <Badge variant="outline">retried</Badge>}
              {item.inference && <Badge variant="info">model-derived</Badge>}
            </div>
            <h2 id="library-item-title" className="mt-1.5 text-[14px] font-semibold leading-snug text-fg">
              {item.title}
            </h2>
          </div>
          <Button variant="ghost" size="xs" onClick={onClose}>
            Close
          </Button>
        </header>

        <div className="flex flex-col gap-3.5 px-5 py-4">
          <Body item={item} />

          <Meta item={item} />

          {links.length > 0 && (
            <section className="rounded-lg border border-line bg-panel2/40">
              <header className="border-b border-line px-3.5 py-2 text-[11px] font-semibold uppercase tracking-[0.08em] text-fg/70">
                Linked theses
              </header>
              <ul className="divide-y divide-line">
                {links.map((l) => (
                  <li key={l.id} className="flex flex-wrap items-center gap-2 px-3.5 py-2.5">
                    <Badge variant={l.bearing === "contradicts" ? "down" : l.bearing === "supports" ? "up" : "info"}>
                      {l.bearing}
                    </Badge>
                    <span className="min-w-0 flex-1 truncate text-[12px] text-fg/90">
                      {thesisById?.[l.thesisId]?.claim ||
                        thesisById?.[l.thesisId]?.text ||
                        `thesis ${l.thesisId.slice(0, 8)}…`}
                    </span>
                    <Button
                      variant="ghost"
                      size="xs"
                      disabled={busy === l.id}
                      title="Removes the link only — the capture stays in your evidence."
                      onClick={() => doUnlink(l)}
                    >
                      {busy === l.id ? "Unlinking…" : "Unlink"}
                    </Button>
                  </li>
                ))}
              </ul>
            </section>
          )}

          <Actions
            item={item}
            busy={busy}
            confirmDelete={confirmDelete}
            onDelete={doDelete}
            onCancelDelete={() => setConfirmDelete(false)}
            onOpenResearch={onOpenResearch}
            onApplyLens={onApplyLens}
            onClose={onClose}
          />
        </div>
      </div>
    </div>
  );
}

// ---- type-specific bodies -----------------------------------------------------------------------

function Body({ item }) {
  switch (item.type) {
    case "evidence":
      return <EvidenceCard record={item.raw} />;
    case "transcript":
      return <TranscriptBody rec={item.raw} />;
    case "read":
      return <ReadBody rec={item.raw} />;
    case "committee":
      return <CommitteeBody rec={item.raw} />;
    case "review":
      return <ReviewBody rec={item.raw} />;
    case "lens":
      return <LensBody rec={item.raw} />;
    default:
      return null;
  }
}

function List({ label, items, mono }) {
  const arr = (items || []).filter(Boolean);
  if (!arr.length) return null;
  return (
    <div>
      <h4 className="text-[11px] font-semibold uppercase tracking-[0.08em] text-muted">{label}</h4>
      <ul className={cx("mt-1 flex flex-col gap-1", mono && "font-mono")}>
        {arr.map((x, i) => (
          <li key={i} className="text-[12px] leading-relaxed text-fg/90">
            • {typeof x === "string" ? x : JSON.stringify(x)}
          </li>
        ))}
      </ul>
    </div>
  );
}

function TranscriptBody({ rec }) {
  const s = rec.structured || {};
  return (
    <div className="flex flex-col gap-3">
      {s.summary && <p className="text-[12.5px] leading-relaxed text-fg/90">{s.summary}</p>}
      <List label="Key points" items={s.keyPoints} />
      {(s.evasiveMoments || []).length > 0 && (
        <div>
          <h4 className="text-[11px] font-semibold uppercase tracking-[0.08em] text-muted">
            Evasive moments — quotations verified against the call text when this was analysed
          </h4>
          <ul className="mt-1 flex flex-col gap-2">
            {s.evasiveMoments.map((m, i) => (
              <li key={i} className="rounded-md border border-line bg-panel2/40 px-3 py-2">
                <p className="text-[11.5px] italic leading-relaxed text-muted">Q: “{m.question}”</p>
                <p className="mt-1 text-[12px] leading-relaxed text-fg/90">A: “{m.answer}”</p>
                {m.why && <p className="mt-1 text-[11px] text-muted">{m.why}</p>}
              </li>
            ))}
          </ul>
        </div>
      )}
      {s.tone && (
        <p className="text-[12px] text-fg/85">
          Tone — confidence <span className="font-semibold">{s.tone.confidence}</span>, hedging{" "}
          <span className="font-semibold">{s.tone.hedging}</span>
        </p>
      )}
      <List label="Notable language" items={s.tone?.notableLanguage} />
      {s.toneShift && (
        <p className="text-[12px] leading-relaxed text-fg/85">
          <span className="font-semibold">Shift vs the prior quarter: </span>
          {s.toneShift}
        </p>
      )}
      <List label="Risk flags" items={s.riskFlags} />
      <p className="text-[11px] leading-relaxed text-muted">
        The transcript text itself is not stored — only this derived structure and its verified
        quotations.
      </p>
    </div>
  );
}

function ReadBody({ rec }) {
  const s = rec.structured || {};
  const r = rec.regimeSnapshot || {};
  return (
    <div className="flex flex-col gap-3">
      {s.headline && <p className="text-[13px] font-semibold text-fg">{s.headline}</p>}
      {s.summary && <p className="text-[12.5px] leading-relaxed text-fg/90">{s.summary}</p>}
      <List label="What happened" items={s.whatHappened} />
      <List label="What to watch" items={s.watch} />
      <List label="Risk flags" items={s.riskFlags} />
      {(r.trend || r.lastClose) && (
        <p className="font-mono text-[11.5px] text-muted">
          regime at capture — trend {r.trend || "?"} · momentum {r.momentum || "?"} · last {r.lastClose ?? "?"}
        </p>
      )}
    </div>
  );
}

function CommitteeBody({ rec }) {
  const c = rec.consensus || {};
  const analysts = rec.analysts || {};
  return (
    <div className="flex flex-col gap-3">
      <div className="rounded-md border border-line bg-panel2/40 px-3.5 py-2.5">
        <p className="text-[12.5px] text-fg/90">
          <span className="font-semibold">Consensus: {c.label || "unknown"}</span>
          {typeof c.agreement === "number" && ` · ${Math.round(c.agreement * 100)}% agreement across ${c.n || 0} analysts`}
        </p>
        <p className="mt-1 text-[11px] leading-relaxed text-muted">
          Computed deterministically in code from the analyst stances — never produced by the model. A
          descriptive lean, never a verdict or a recommendation.
        </p>
      </div>
      {Object.entries(analysts).map(([role, a]) => (
        <div key={role} className="border-l-2 border-line2 pl-2.5">
          <p className="text-[11px] font-semibold uppercase tracking-wide text-muted">
            {role} — {a?.stance || "no stance"}
          </p>
          {a?.summary && <p className="mt-0.5 text-[12px] leading-relaxed text-fg/85">{a.summary}</p>}
        </div>
      ))}
      {Object.entries(rec.debate || {}).map(([side, d]) => (
        <div key={side} className="border-l-2 border-line2 pl-2.5">
          <p className="text-[11px] font-semibold uppercase tracking-wide text-muted">{side} case</p>
          {d?.summary && <p className="mt-0.5 text-[12px] leading-relaxed text-fg/85">{d.summary}</p>}
        </div>
      ))}
      <List label="Points of agreement" items={rec.judge?.agreements} />
      <List label="Points of disagreement" items={rec.judge?.disagreements} />
    </div>
  );
}

// ReviewBody renders the frozen lens-review shape (PARALLEL_CONTRACTS §3.4). A `kind:"fact"` finding
// must cite evidence; an `inference` finding is marked as one. Library renders that distinction rather
// than flattening the two, because it is the whole basis for trusting a saved review.
function ReviewBody({ rec }) {
  return (
    <div className="flex flex-col gap-3">
      {(rec.findings || []).length > 0 && (
        <div>
          <h4 className="text-[11px] font-semibold uppercase tracking-[0.08em] text-muted">Findings</h4>
          <ul className="mt-1 flex flex-col gap-1.5">
            {rec.findings.map((f) => (
              <li key={f.id} className="flex gap-2 text-[12px] leading-relaxed text-fg/90">
                <Badge variant={f.kind === "inference" ? "info" : "outline"}>{f.kind}</Badge>
                <span className="min-w-0 flex-1">
                  {f.statement}
                  {(f.citedEvidenceIds || []).length > 0 && (
                    <span className="ml-1 font-mono text-[10.5px] text-muted">
                      [{f.citedEvidenceIds.join(", ")}]
                    </span>
                  )}
                </span>
              </li>
            ))}
          </ul>
        </div>
      )}
      <List label="Agreements" items={rec.agreements} />
      <List label="Disagreements" items={rec.disagreements} />
      <List label="Missing evidence" items={rec.missingEvidence} />
      <List label="Open questions" items={rec.openQuestions} />
      {rec.disclaimer && <p className="text-[11px] leading-relaxed text-muted">{rec.disclaimer}</p>}
    </div>
  );
}

function LensBody({ rec }) {
  return (
    <div className="flex flex-col gap-2">
      <p className="whitespace-pre-wrap rounded-md border border-line bg-panel2/40 px-3.5 py-3 text-[12px] leading-relaxed text-fg/90">
        {rec.instructions}
      </p>
      <p className="text-[11px] leading-relaxed text-muted">
        A lens shapes how a research task is approached. It never loosens the grounding rules: no lens
        can produce a price target, a buy/sell call, or an invented number.
      </p>
    </div>
  );
}

// ---- provenance + model metadata ----------------------------------------------------------------

// SCOPE_LABEL states who an object belongs to. This matters because Library puts personal captures and
// company-level model artefacts in one list: a transcript analysis or a daily read is stored per
// COMPANY, not per user, so anyone signed in sees the same one. Presenting those as "your saved
// research" without saying so would overstate ownership.
const SCOPE_LABEL = {
  user: "Yours — stored in your account",
  company: "Company-level — stored per company, shared by everyone using this instance",
  global: "Built-in — ships with the app, identical for everyone",
};

function Meta({ item }) {
  const rows = [
    ["Source", item.attribution || item.sourceLabel || "not recorded"],
    item.scope ? ["Belongs to", SCOPE_LABEL[item.scope] || item.scope] : null,
    ["Date", fmtWhenExact(item.at)],
    item.modelUsed ? ["Model", item.modelUsed] : null,
    item.stub ? ["Model status", "Deterministic offline fallback — not a model result"] : null,
    item.lensVersion ? ["Lens version", item.lensVersion] : null,
    item.raw?.sourceRef ? ["Reference", `${item.raw.sourceRef.store}: ${item.raw.sourceRef.id}`] : null,
    item.type === "lens" && item.raw?.updatedAt ? ["Last updated", fmtWhenExact(item.at)] : null,
  ].filter(Boolean);

  return (
    <dl className="divide-y divide-line rounded-lg border border-line">
      {rows.map(([k, v]) => (
        <div key={k} className="flex gap-3 px-3.5 py-2">
          <dt className="w-[108px] shrink-0 text-[11px] uppercase tracking-wide text-muted">{k}</dt>
          <dd className="min-w-0 flex-1 break-words text-[12px] leading-relaxed text-fg/90">{v}</dd>
        </div>
      ))}
    </dl>
  );
}

// ---- actions ------------------------------------------------------------------------------------

// Actions offers only what the shipped contracts actually support. Deletion exists for the two object
// types whose stores define a deletion policy (evidence, and your own lenses). Model artefacts —
// transcript analyses, daily reads, committee snapshots — have no deletion endpoint and no approved
// retention policy, so no button is drawn and the reason is stated instead of implied.
function Actions({ item, busy, confirmDelete, onDelete, onCancelDelete, onOpenResearch, onApplyLens, onClose }) {
  const canOpenThesis = (item.thesisIds || []).length > 0;
  const deletable = item.type === "evidence" || (item.type === "lens" && !item.builtin);

  const openResearch = (subview) => {
    if (onOpenResearch) {
      onOpenResearch(item.ticker, subview);
    } else {
      // Fallback while App.jsx has not been wired to pass a navigator (see the handoff): the hash
      // still moves to the right section, but the active company does not change. Saying so beats
      // pretending the jump was company-exact.
      window.location.hash = `#research/${subview}`;
    }
    onClose();
  };

  return (
    <div className="flex flex-col gap-2 border-t border-line pt-3.5">
      <div className="flex flex-wrap items-center gap-2">
        {item.ticker && (
          <>
            <Button variant="secondary" size="sm" onClick={() => openResearch("investigations")}>
              Open in Research
            </Button>
            {canOpenThesis && (
              <Button variant="secondary" size="sm" onClick={() => openResearch("thesis")}>
                Open the thesis
              </Button>
            )}
          </>
        )}
        {item.type === "lens" && onApplyLens && (
          <Button
            variant="primary"
            size="sm"
            onClick={() => {
              onApplyLens(item.id);
              onClose();
            }}
          >
            Apply this lens
          </Button>
        )}

        {deletable && !confirmDelete && (
          <Button
            variant="danger"
            size="sm"
            className="ml-auto"
            disabled={busy === "delete"}
            onClick={() => onDelete(false)}
          >
            {busy === "delete" ? "Deleting…" : "Delete"}
          </Button>
        )}
        {deletable && confirmDelete && (
          <span className="ml-auto flex items-center gap-1.5">
            <span className="text-[11px] text-muted">Delete it and its thesis links?</span>
            <Button variant="danger" size="xs" disabled={busy === "delete"} onClick={() => onDelete(true)}>
              Delete anyway
            </Button>
            <Button variant="ghost" size="xs" onClick={onCancelDelete}>
              Keep
            </Button>
          </span>
        )}
      </div>

      {!deletable && item.type !== "lens" && (
        <p className="text-[11px] leading-relaxed text-muted">
          Stored model artefacts have no deletion endpoint and no approved retention policy yet, so
          Library does not offer one. They are removed only with the account.
        </p>
      )}
      {item.type === "lens" && item.builtin && (
        <p className="text-[11px] leading-relaxed text-muted">
          Built-in lenses are read-only. Duplicate one in the Copilot to make a version you can edit.
        </p>
      )}
      {!onOpenResearch && item.ticker && (
        <p className="text-[11px] leading-relaxed text-muted">
          Opening moves to the Research section for the company that is currently active — a company is
          not yet addressable in the URL.
        </p>
      )}
    </div>
  );
}
