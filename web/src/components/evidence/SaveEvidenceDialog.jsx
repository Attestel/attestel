import { useEffect, useMemo, useRef, useState } from "react";
import { Badge, Button, Field, ProvenanceBadge, controlClass, useToast } from "../ui/index.js";
import { cx } from "../../lib/cx.js";
import { AuthRequiredError, listTheses } from "../../lib/api.js";
import { useAuth } from "../../auth/AuthContext.jsx";
import EvidenceCard from "./EvidenceCard.jsx";
import {
  BEARINGS,
  DATA_STATE_LABELS,
  EVIDENCE_KIND_LABELS,
  evidenceErrorMessage,
  saveFromFact,
} from "../../lib/evidenceApi.js";

// SaveEvidenceDialog — the one save flow, used by every "Save to thesis" action.
//
// It does five things the prompt requires before a single byte is written:
//   1. shows EXACTLY what will be stored, field by field;
//   2. shows the source and whether the underlying data is live, seed, or synthetic;
//   3. lets the user pick a thesis and a BEARING (supports / contradicts / context);
//   4. lets them add their own note;
//   5. requires a signed-in account, and routes a guest to sign-in instead of dropping their work.
//
// On success it swaps to the STORED record — the server's own account of the capture, with the
// provenance it actually stamped — and offers a way through to the thesis. That ordering matters: the
// preview is what the app has on screen, the confirmation is what the store now holds. If the two
// ever disagree, the user sees the second one.
//
// Two honest limits are stated in the UI rather than hidden:
//   · provenance comes from the server, so the preview marks itself "as shown here";
//   · the thesis link lands on the Thesis section of the ACTIVE company (D-19 is unresolved — a
//     company is not yet addressable in the URL), so the button says so.

const SUCCESS_HASH = "#research/thesis";

export default function SaveEvidenceDialog({ open, onClose, item, ticker, timeframe = "1D", onSaved }) {
  const { user, promptSignIn } = useAuth();
  const toast = useToast();
  const panelRef = useRef(null);
  const restoreFocusRef = useRef(null);

  const [theses, setTheses] = useState(null); // null = loading
  const [thesisId, setThesisId] = useState("");
  const [bearing, setBearing] = useState("supports");
  const [note, setNote] = useState("");
  const [speaker, setSpeaker] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState(null);
  const [saved, setSaved] = useState(null); // { evidence, deduplicated, link }

  const needsSpeaker = item?.kind === "management_statement";
  const isUserNote = item?.kind === "user_note";

  // A quotation becomes a MANAGEMENT STATEMENT the moment a speaker is named, and stays a plain
  // transcript excerpt while it is not. That is the honest split: the quotation's verification is the
  // same either way, but "who said it" is only claimable when someone actually claims it.
  const speakerOptional = !!item?.speakerOptional;
  const effectiveKind =
    needsSpeaker || (speakerOptional && speaker.trim()) ? "management_statement" : item?.kind;

  // Reset per open so a previous save's confirmation never bleeds into the next capture.
  useEffect(() => {
    if (!open) return;
    setErr(null);
    setSaved(null);
    setBusy(false);
    setNote("");
    setSpeaker("");
    setBearing("supports");
  }, [open, item?.key]);

  // The thesis list is the caller's own; a guest has none, so we do not even ask.
  useEffect(() => {
    if (!open || !user) return;
    let alive = true;
    setTheses(null);
    listTheses(ticker)
      .then((list) => {
        if (!alive) return;
        setTheses(list);
        // Default to the first ACTIVE thesis. Never auto-select an archived or closed one — attaching
        // fresh evidence to a thesis the user has put down is almost never what they meant.
        const active = list.find((t) => (t.status || "active") === "active");
        setThesisId(active?.id || "");
      })
      .catch(() => alive && setTheses([]));
    return () => {
      alive = false;
    };
  }, [open, user, ticker]);

  // Esc to close, scroll lock, focus in and back out.
  useEffect(() => {
    if (!open) return;
    restoreFocusRef.current = document.activeElement;
    const onKey = (e) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        onClose();
      }
    };
    document.addEventListener("keydown", onKey, true);
    const prevOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    const t = setTimeout(() => panelRef.current?.focus?.(), 40);
    return () => {
      document.removeEventListener("keydown", onKey, true);
      document.body.style.overflow = prevOverflow;
      clearTimeout(t);
      restoreFocusRef.current?.focus?.();
    };
  }, [open, onClose]);

  const activeTheses = useMemo(
    () => (theses || []).filter((t) => (t.status || "active") === "active"),
    [theses]
  );
  const otherTheses = useMemo(
    () => (theses || []).filter((t) => (t.status || "active") !== "active"),
    [theses]
  );

  if (!open || !item) return null;

  const submit = async () => {
    setErr(null);
    if (isUserNote && !note.trim()) {
      setErr("Write your observation first — a note is the whole content of this capture.");
      return;
    }
    if (needsSpeaker && !speaker.trim()) {
      setErr("Name the speaker and their role. The stored transcript analysis does not record speakers, so this attribution has to come from you.");
      return;
    }
    setBusy(true);
    try {
      const selector = { ...(item.selector || {}) };
      if (speaker.trim()) selector.speaker = speaker.trim();
      const res = await saveFromFact({
        ticker,
        kind: effectiveKind,
        selector,
        timeframe,
        note: note.trim(),
        thesisId,
        bearing: thesisId ? bearing : "",
      });
      setSaved(res);
      onSaved?.(res);
      toast({
        tone: "success",
        title: res.deduplicated ? "Already captured" : "Saved as evidence",
        message: res.deduplicated
          ? "This source was already in your evidence; the existing capture was used."
          : thesisId
            ? "Linked to your thesis."
            : "Saved without a thesis link.",
      });
    } catch (e) {
      if (e instanceof AuthRequiredError) {
        promptSignIn();
        onClose();
        return;
      }
      setErr(evidenceErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="fixed inset-0 z-[90] flex items-start justify-center overflow-y-auto bg-black/60 px-4 py-8 backdrop-blur-sm">
      <button type="button" aria-label="Close" className="absolute inset-0 cursor-default" onClick={onClose} />
      <div
        ref={panelRef}
        tabIndex={-1}
        role="dialog"
        aria-modal="true"
        aria-labelledby="save-evidence-title"
        className="relative w-[min(620px,100%)] surface-card focus:outline-none"
      >
        <header className="flex items-start gap-3 border-b border-line px-5 py-3.5">
          <div className="min-w-0 flex-1">
            <h2 id="save-evidence-title" className="text-[14px] font-semibold text-fg">
              {saved ? "Saved to your evidence" : "Save as evidence"}
            </h2>
            <p className="mt-0.5 text-[11.5px] text-muted">
              {saved
                ? "This is the record as stored, with the provenance the server attached."
                : `${EVIDENCE_KIND_LABELS[item.kind] || item.kind} · ${ticker}`}
            </p>
          </div>
          <Button variant="ghost" size="xs" onClick={onClose}>
            Close
          </Button>
        </header>

        <div className="flex flex-col gap-4 px-5 py-4">
          {saved ? (
            <SavedState result={saved} onClose={onClose} />
          ) : !user ? (
            <GuestState onSignIn={promptSignIn} />
          ) : (
            <>
              <WillBeStored item={item} kind={effectiveKind} isUserNote={isUserNote} note={note} />

              {(needsSpeaker || speakerOptional) && (
                <Field label={needsSpeaker ? "Speaker and role (required)" : "Speaker and role (optional)"}>
                  <input
                    className={controlClass}
                    value={speaker}
                    onChange={(e) => setSpeaker(e.target.value)}
                    placeholder="e.g. Colette Kress, CFO"
                  />
                  <span className="mt-1 block text-[11px] leading-relaxed text-muted">
                    The quotation itself was verified against the transcript when it was analysed. The
                    speaker is <span className="text-fg/70">not</span> recorded in the stored analysis, so
                    naming one records it as your attribution
                    {speakerOptional ? " and files this as a management statement rather than a plain quotation" : ""}.
                  </span>
                </Field>
              )}

              <ThesisPicker
                theses={theses}
                activeTheses={activeTheses}
                otherTheses={otherTheses}
                thesisId={thesisId}
                onThesis={setThesisId}
              />

              {thesisId && <BearingPicker bearing={bearing} onBearing={setBearing} />}

              <Field label={isUserNote ? "Your observation (required)" : "Your note (optional)"}>
                <textarea
                  className={cx(controlClass, "min-h-[76px] resize-y")}
                  value={note}
                  onChange={(e) => setNote(e.target.value)}
                  placeholder={
                    isUserNote
                      ? "What did you notice, and why does it matter to your thesis?"
                      : "Why you are keeping this — the part the source does not say."
                  }
                />
              </Field>

              {err && (
                <p role="alert" className="rounded-md border border-down/40 bg-down/10 px-3 py-2 text-[12px] leading-relaxed text-down">
                  {err}
                </p>
              )}

              <div className="flex items-center justify-end gap-2 border-t border-line pt-3.5">
                <Button variant="ghost" size="sm" onClick={onClose} disabled={busy}>
                  Cancel
                </Button>
                <Button variant="primary" size="sm" onClick={submit} disabled={busy}>
                  {busy ? "Saving…" : thesisId ? "Save and link" : "Save without a thesis"}
                </Button>
              </div>
            </>
          )}
        </div>
      </div>
    </div>
  );
}

// WillBeStored is the "show exactly what will be stored" panel. Every row is a field that ends up in
// the record. The provenance rows are marked "as shown here" because the SERVER stamps the stored
// values — claiming otherwise would be the same overreach this whole flow exists to prevent.
function WillBeStored({ item, kind, isUserNote, note }) {
  const rows = [
    ...(isUserNote ? [] : [{ label: "Title", value: item.title }]),
    ...(item.preview || []),
  ].filter((r) => r.value !== undefined && r.value !== null && r.value !== "");

  return (
    <section className="rounded-lg border border-line bg-panel2/40">
      <header className="flex flex-wrap items-center gap-2 border-b border-line px-3.5 py-2.5">
        <span className="text-[11px] font-semibold uppercase tracking-[0.08em] text-fg/70">
          What will be stored
        </span>
        <Badge variant="outline">{EVIDENCE_KIND_LABELS[kind] || kind}</Badge>
        {item.dataState && (
          <ProvenanceBadge kind={item.dataState} label={DATA_STATE_LABELS[item.dataState] || item.dataState} />
        )}
      </header>

      <dl className="divide-y divide-line">
        {rows.map((r) => (
          <div key={r.label} className="flex gap-3 px-3.5 py-2">
            <dt className="w-[104px] shrink-0 text-[11px] uppercase tracking-wide text-muted">{r.label}</dt>
            <dd className={cx("min-w-0 flex-1 text-[12px] leading-relaxed text-fg/90", r.mono && "font-mono")}>
              {r.value}
            </dd>
          </div>
        ))}
        {isUserNote && (
          <div className="flex gap-3 px-3.5 py-2">
            <dt className="w-[104px] shrink-0 text-[11px] uppercase tracking-wide text-muted">Your words</dt>
            <dd className="min-w-0 flex-1 text-[12px] leading-relaxed text-fg/90">
              {note.trim() || <span className="text-muted/70">(nothing written yet)</span>}
            </dd>
          </div>
        )}
      </dl>

      <p className="border-t border-line px-3.5 py-2.5 text-[11px] leading-relaxed text-muted">
        {item.excerptNote ||
          "Source, provider, publication time and live/seed/synthetic state are stamped by the server from the data it resolved — the values above are as shown here. No page text is fetched or stored."}
      </p>
    </section>
  );
}

function ThesisPicker({ theses, activeTheses, otherTheses, thesisId, onThesis }) {
  if (theses === null) {
    return <p className="text-[12px] text-muted">Loading your theses…</p>;
  }
  if (theses.length === 0) {
    return (
      <div className="rounded-md border border-dashed border-line px-3.5 py-3 text-[12px] leading-relaxed text-muted">
        You have no thesis for this company yet. You can still save this capture — it will sit in your
        evidence, unlinked, until you write a thesis to attach it to.
      </div>
    );
  }
  return (
    <Field label="Attach to thesis">
      <select className={controlClass} value={thesisId} onChange={(e) => onThesis(e.target.value)}>
        <option value="">Save without linking to a thesis</option>
        {activeTheses.length > 0 && (
          <optgroup label="Active">
            {activeTheses.map((t) => (
              <option key={t.id} value={t.id}>
                {truncate(t.claim || t.text || t.id, 90)}
              </option>
            ))}
          </optgroup>
        )}
        {otherTheses.length > 0 && (
          <optgroup label="Not active">
            {otherTheses.map((t) => (
              <option key={t.id} value={t.id}>
                {truncate(t.claim || t.text || t.id, 90)} ({t.status})
              </option>
            ))}
          </optgroup>
        )}
      </select>
    </Field>
  );
}

// BearingPicker makes `contradicts` a first-class, equally-weighted choice. Evidence against your own
// claim is the most valuable kind, so it is not tucked behind an "advanced" affordance.
function BearingPicker({ bearing, onBearing }) {
  return (
    <fieldset>
      <legend className="mb-1.5 text-[11px] font-medium uppercase tracking-wide text-muted">
        How does it bear on the claim?
      </legend>
      <div className="grid grid-cols-1 gap-1.5 sm:grid-cols-3">
        {BEARINGS.map((b) => {
          const selected = bearing === b.id;
          return (
            <label
              key={b.id}
              className={cx(
                "cursor-pointer rounded-md border px-3 py-2.5 transition-colors",
                selected ? "border-accent bg-accent/10" : "border-line bg-panel2/40 hover:border-line2"
              )}
            >
              <span className="flex items-center gap-2">
                <input
                  type="radio"
                  name="bearing"
                  className="accent-accent"
                  checked={selected}
                  onChange={() => onBearing(b.id)}
                />
                <span className="text-[12.5px] font-semibold text-fg">{b.label}</span>
              </span>
              <span className="mt-1 block text-[11px] leading-relaxed text-muted">{b.blurb}</span>
            </label>
          );
        })}
      </div>
    </fieldset>
  );
}

function GuestState({ onSignIn }) {
  return (
    <div className="flex flex-col gap-3 rounded-lg border border-dashed border-line px-4 py-5 text-center">
      <p className="text-[13px] font-semibold text-fg">Evidence is saved to your account</p>
      <p className="mx-auto max-w-[46ch] text-[12px] leading-relaxed text-muted">
        Captures and thesis links are per-user records, so they need a signed-in account. Nothing has
        been saved yet.
      </p>
      <div>
        <Button variant="primary" size="sm" onClick={onSignIn}>
          Sign in to save
        </Button>
      </div>
    </div>
  );
}

// SavedState shows the STORED record — not the preview — so what the user takes away is the store's
// own account of the capture, including a dedupe hit if that is what happened.
function SavedState({ result, onClose }) {
  const { evidence, deduplicated, link, linkUpdated } = result;
  return (
    <div className="flex flex-col gap-3">
      {deduplicated && (
        <p className="rounded-md border border-info/40 bg-info/10 px-3 py-2 text-[12px] leading-relaxed text-info">
          You had already captured this source, so your existing record was used rather than a second
          copy. Its original capture time is unchanged.
        </p>
      )}
      <EvidenceCard record={evidence} />

      {link ? (
        <div className="flex flex-col gap-2 rounded-md border border-line bg-panel2/40 px-3.5 py-3">
          <p className="text-[12px] leading-relaxed text-fg/85">
            Linked as <span className="font-semibold">{link.bearing}</span>
            {linkUpdated ? " (the existing link's bearing was updated)" : ""}.
          </p>
          <div>
            <Button
              variant="secondary"
              size="xs"
              onClick={() => {
                // D-19 is unresolved: a company is not addressable in the URL, so this opens the
                // Thesis section of the ACTIVE company rather than a per-thesis link. The label says so.
                window.location.hash = SUCCESS_HASH;
                onClose();
              }}
            >
              Open the Thesis section
            </Button>
          </div>
        </div>
      ) : (
        <p className="text-[12px] leading-relaxed text-muted">
          Saved to your evidence without a thesis link. You can attach it to a thesis later from the
          thesis workspace.
        </p>
      )}

      <div className="flex justify-end border-t border-line pt-3.5">
        <Button variant="ghost" size="sm" onClick={onClose}>
          Done
        </Button>
      </div>
    </div>
  );
}

function truncate(s, n) {
  const str = String(s || "");
  return str.length > n ? `${str.slice(0, n - 1)}…` : str;
}
