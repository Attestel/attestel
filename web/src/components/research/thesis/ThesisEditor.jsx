import { useState } from "react";
import { cx } from "../../../lib/cx.js";
import { Tag } from "../../terminal/bits.jsx";
import ConfidenceControl from "./ConfidenceControl.jsx";
import EvidenceSlot from "./EvidenceSlot.jsx";
import { ThesisEvidencePanel } from "../../evidence/index.js";
import { LensRunner } from "../../lens/index.js";
import ItemListEditor from "./ItemListEditor.jsx";
import LifecycleControl from "./LifecycleControl.jsx";
import ModelCheckCard from "./ModelCheckCard.jsx";
import VersionHistory from "./VersionHistory.jsx";
import {
  HORIZONS,
  ITEM_LISTS,
  LIMITS,
  STATUS_META,
  STRUCTURED_SLOTS,
  countStructured,
  fmtWhen,
  fmtWhenExact,
  isUnstructured,
} from "../../../lib/thesisApi.js";
import { Icon } from "../../shell/icons.jsx";

// ThesisEditor — structured authoring for ONE thesis.
//
// Save model: explicit. Nothing is written until the user presses Save, the Save/Discard bar only
// appears once something actually changed, and the parent keeps the in-progress draft so navigating
// away and back does not lose work.
//
// Ownership is the rule that shapes this file: every input here is user-authored. The model's output
// lives in exactly one read-only card (ModelCheckCard) with no path into these fields — there is no
// "accept suggestion" button anywhere, by design.

const TEXTAREA =
  "w-full min-w-0 rounded-md border border-line bg-panel2 px-2.5 py-2 text-[12.5px] leading-relaxed text-fg transition-colors placeholder:text-muted/40 hover:border-line2 focus:outline-none focus-visible:border-accent";

function FieldError({ id, message }) {
  if (!message) return null;
  return (
    <p id={id} role="alert" className="flex items-start gap-1.5 text-[11.5px] text-down">
      <Icon name="close" size={11} />
      <span>{message}</span>
    </p>
  );
}

// Block — a titled group inside the editor, so the form reads as one document rather than a wall.
function Block({ title, note, children }) {
  return (
    <section className="flex flex-col gap-2 border-t border-line px-[18px] py-3.5 first:border-t-0">
      {title && (
        <div className="flex flex-wrap items-baseline gap-x-2">
          <h3 className="text-[12px] font-semibold text-fg">{title}</h3>
          {note && <span className="text-[11.5px] text-muted/70">{note}</span>}
        </div>
      )}
      {children}
    </section>
  );
}

export default function ThesisEditor({
  thesis, // stored record; null while creating
  form,
  onForm,
  errors,
  dirty,
  busy,
  saving,
  readOnly,
  onSave,
  onDiscard,
  onDelete,
  onStatusChange,
  onMarkReviewed,
  onRunCheck,
  checking,
  reason,
  onReason,
  historyKey,
}) {
  const [pendingStatus, setPendingStatus] = useState(null);
  const [pendingReason, setPendingReason] = useState("");
  const [pendingCondition, setPendingCondition] = useState("");
  const [confirmDelete, setConfirmDelete] = useState(false);

  const isNew = !thesis;
  const idPrefix = `thesis-${thesis?.id || "new"}`;
  const set = (patch) => onForm({ ...form, ...patch });

  const filled = thesis ? countStructured(thesis) : 0;
  const unstructured = thesis ? isUnstructured(thesis) : false;

  // acceptAsQuestion — the ONLY path from a model review into a user-owned field, and it is
  // deliberately not a write. It STAGES the finding's text as a new Next question in the form, which
  // marks the thesis dirty and surfaces the Save bar: the user reads it, can edit it, and must press
  // Save themselves. Nothing is persisted here, so model text can never land in a thesis field
  // without the user having seen and accepted it (PARALLEL_CONTRACTS.md §3.7).
  const acceptAsQuestion = (text) => {
    const trimmed = String(text || "").trim().slice(0, LIMITS.itemText);
    if (!trimmed) return;
    if (form.nextQuestions.some((q) => q.text.trim() === trimmed)) return;
    set({
      nextQuestions: [
        ...form.nextQuestions,
        { id: "", text: trimmed, createdAt: null, resolvedAt: null, dueAt: null },
      ],
    });
  };

  const resetLifecycle = () => {
    setPendingStatus(null);
    setPendingReason("");
    setPendingCondition("");
  };

  const confirmLifecycle = () => {
    onStatusChange(pendingStatus, {
      closeReason: pendingStatus === "closed" ? pendingReason : undefined,
      closedByConditionId: pendingStatus === "closed" ? pendingCondition || undefined : undefined,
    }).then((ok) => ok && resetLifecycle());
  };

  return (
    <div className="flex min-w-0 flex-col">
      {/* ---- header: identity + provenance of the record itself ---- */}
      <div className="flex flex-wrap items-center gap-2 border-b border-line px-[18px] py-2.5">
        <span className="text-[13px] font-semibold text-fg">
          {isNew ? "New thesis" : "Thesis"}
        </span>
        {!isNew && (
          <span className={cx("flex items-center gap-1 text-[11px] font-bold uppercase tracking-wide", STATUS_META[form.status]?.tone)}>
            <span aria-hidden="true">{STATUS_META[form.status]?.glyph}</span>
            {STATUS_META[form.status]?.label}
          </span>
        )}
        <Tag tone="outline">you author this</Tag>
        {!isNew && (
          <span className="ml-auto flex flex-wrap items-center gap-x-2.5 gap-y-1 text-[10.5px] text-muted/70">
            <span>Created {fmtWhen(thesis.createdAt)}</span>
            <span>Updated {fmtWhen(thesis.updatedAt)}</span>
            <span className="font-mono">
              {filled}/{STRUCTURED_SLOTS} fields
            </span>
          </span>
        )}
      </div>

      {/* A claim-only record says WHY it looks sparse, so empty structure reads as "not written yet"
          rather than as data this app lost. Nothing is auto-filled to cover the gap.
          The copy states only what is observable: normalization stamps every record as v2 on read, so
          the frontend cannot tell an old free-text thesis from a new claim-only one — and must not
          claim it can. Either way the guidance is the same. */}
      {unstructured && (
        <p className="border-b border-line bg-panel2/40 px-[18px] py-2 text-[11.5px] leading-relaxed text-muted">
          Only the claim is filled in on this thesis — it shows below exactly as you wrote it. The
          structured fields are empty because they were never captured, not because anything was lost.
          Add whatever is useful and leave the rest.
        </p>
      )}

      {readOnly && (
        <p className="border-b border-line bg-info/[0.07] px-[18px] py-2 text-[11.5px] leading-relaxed text-info">
          You are viewing this workspace as a guest. Reading is fine — saving asks you to sign in first.
        </p>
      )}

      {/* ---- the claim: the one required field ---- */}
      <Block title="Your claim" note="Required. What you believe, and why.">
        <label htmlFor={`${idPrefix}-claim`} className="sr-only">
          Thesis claim
        </label>
        <textarea
          id={`${idPrefix}-claim`}
          value={form.claim}
          rows={3}
          disabled={busy}
          maxLength={LIMITS.claim}
          aria-invalid={errors.claim ? "true" : undefined}
          aria-describedby={cx(`${idPrefix}-claim-help`, errors.claim && `${idPrefix}-claim-error`)}
          onChange={(e) => set({ claim: e.target.value })}
          placeholder="e.g. Data-center demand outpaces supply through 2027, so margins hold above 70%"
          className={cx(TEXTAREA, errors.claim && "border-down/60")}
        />
        <div className="flex flex-wrap items-baseline justify-between gap-2">
          <p id={`${idPrefix}-claim-help`} className="text-[11.5px] text-muted/80">
            Write it so it could turn out to be wrong — that is what makes it testable.
          </p>
          <span className="font-mono text-[10.5px] text-muted/50">
            {form.claim.length}/{LIMITS.claim}
          </span>
        </div>
        <FieldError id={`${idPrefix}-claim-error`} message={errors.claim} />
      </Block>

      {/* ---- horizon + confidence ---- */}
      <Block title="Horizon and conviction">
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <div className="flex min-w-0 flex-col gap-1">
            <label
              htmlFor={`${idPrefix}-horizon`}
              className="text-[11px] font-semibold uppercase tracking-wide text-muted"
            >
              Time horizon
            </label>
            <select
              id={`${idPrefix}-horizon`}
              value={form.horizon}
              disabled={busy}
              aria-invalid={errors.horizon ? "true" : undefined}
              aria-describedby={errors.horizon ? `${idPrefix}-horizon-error` : undefined}
              onChange={(e) => set({ horizon: e.target.value })}
              className="w-full rounded-md border border-line bg-panel2 px-2.5 py-1.5 text-[12.5px] text-fg focus:outline-none focus-visible:border-accent"
            >
              <option value="">Not set</option>
              {HORIZONS.map((h) => (
                <option key={h.value} value={h.value}>
                  {h.label}
                </option>
              ))}
            </select>
            <p className="text-[11.5px] text-muted/80">Over what period should this play out?</p>
            <FieldError id={`${idPrefix}-horizon-error`} message={errors.horizon} />
          </div>

          <div className="min-w-0">
            <ConfidenceControl
              value={form.confidence}
              onChange={(v) => set({ confidence: v })}
              disabled={busy}
              idPrefix={idPrefix}
            />
            <FieldError id={`${idPrefix}-confidence-error`} message={errors.confidence} />
          </div>
        </div>
      </Block>

      {/* ---- the five repeatable lists ---- */}
      <Block
        title="Structure"
        note="Each entry is its own field, so evidence and monitoring can attach to one line later."
      >
        <div className="flex flex-col gap-4">
          {ITEM_LISTS.map((spec) => (
            <ItemListEditor
              key={spec.field}
              spec={spec}
              rows={form[spec.field]}
              onChange={(rows) => set({ [spec.field]: rows })}
              error={errors[spec.field]}
              disabled={busy}
              idPrefix={idPrefix}
              ticker={thesis?.ticker}
              thesisId={thesis?.id}
            />
          ))}
        </div>
      </Block>

      {/* ---- notes ---- */}
      <Block title="Notes" note="Anything that does not fit above.">
        <label htmlFor={`${idPrefix}-notes`} className="sr-only">
          Notes
        </label>
        <textarea
          id={`${idPrefix}-notes`}
          value={form.notes}
          rows={3}
          disabled={busy}
          maxLength={LIMITS.notes}
          aria-invalid={errors.notes ? "true" : undefined}
          aria-describedby={errors.notes ? `${idPrefix}-notes-error` : undefined}
          onChange={(e) => set({ notes: e.target.value })}
          className={cx(TEXTAREA, errors.notes && "border-down/60")}
        />
        <div className="flex justify-end">
          <span className="font-mono text-[10.5px] text-muted/50">
            {form.notes.length}/{LIMITS.notes}
          </span>
        </div>
        <FieldError id={`${idPrefix}-notes-error`} message={errors.notes} />
      </Block>

      {/* WAVE 2 INTEGRATION SEAM. Step 05 shipped EvidenceSlot as an honest "not built yet"
          boundary while Step 06 built evidence concurrently. Both lanes are now integrated, so the
          real panel takes that place — Step 06's own instruction was "place beside the structured
          thesis editor" (web/src/components/evidence/index.js).

          A thesis being CREATED has no id yet, so there is nothing to attach evidence to; the slot's
          boundary copy still serves there and no fetch is issued. */}
      <Block>
        {isNew ? (
          <EvidenceSlot />
        ) : (
          <ThesisEvidencePanel
            thesisId={thesis.id}
            ticker={thesis.ticker}
            thesisLabel={form.claim}
          />
        )}
      </Block>

      {/* ---- save bar. Appears only when there is something to save. ---- */}
      {(dirty || isNew) && (
        <div className="sticky bottom-0 z-10 flex flex-col gap-2 border-t border-accent/30 bg-panel/95 px-[18px] py-3 backdrop-blur">
          {!isNew && (
            <div className="flex min-w-0 flex-col gap-1">
              <label
                htmlFor={`${idPrefix}-reason`}
                className="text-[11px] font-semibold uppercase tracking-wide text-muted"
              >
                Why the change? <span className="font-normal normal-case text-muted/60">(optional, kept in history)</span>
              </label>
              <input
                id={`${idPrefix}-reason`}
                value={reason}
                disabled={busy}
                maxLength={LIMITS.reason}
                onChange={(e) => onReason(e.target.value)}
                placeholder="e.g. Cut conviction after the guidance miss"
                className="w-full rounded-md border border-line bg-panel2 px-2.5 py-1.5 text-[12px] text-fg placeholder:text-muted/40 focus:outline-none focus-visible:border-accent"
              />
            </div>
          )}

          <div className="flex flex-wrap items-center gap-2">
            <span className="flex items-center gap-1.5 text-[11.5px] font-semibold text-warn">
              <span aria-hidden="true">●</span>
              {isNew ? "Not saved yet" : "Unsaved changes"}
            </span>
            <span className="ml-auto flex flex-wrap gap-1.5">
              <button
                type="button"
                onClick={onSave}
                disabled={saving}
                className="rounded-full bg-fg px-3.5 py-1.5 text-[12px] font-semibold text-bg transition-colors duration-200 ease-premium hover:bg-white disabled:opacity-50"
              >
                {saving ? "Saving…" : isNew ? "Create thesis" : "Save changes"}
              </button>
              <button
                type="button"
                onClick={onDiscard}
                disabled={saving}
                className="rounded-full border border-line px-3 py-1.5 text-[12px] font-semibold text-muted transition-colors hover:text-fg disabled:opacity-50"
              >
                {isNew ? "Cancel" : "Discard changes"}
              </button>
            </span>
          </div>

          {errors._form && (
            <p role="alert" className="flex items-start gap-1.5 text-[11.5px] text-down">
              <Icon name="close" size={11} />
              <span>{errors._form}</span>
            </p>
          )}
        </div>
      )}

      {/* ---- everything below exists only for a SAVED thesis ---- */}
      {!isNew && (
        <>
          <Block title="Model review">
            <ModelCheckCard
              check={thesis.lastCheck}
              onRun={onRunCheck}
              busy={checking}
              canRun={!readOnly}
            />
          </Block>

          {/* STEP 07. Contextual research lenses, on the object the user is already editing — this is
              the primary "Challenge this thesis" entry point, not a chat box somewhere else.
              Reviews attach to the thesis and are never merged into the fields above: accepting a
              finding opens the Next questions editor with the text, and the user saves it themselves. */}
          <Block
            title="Research lenses"
            note="Apply a named method to this thesis. Each run saves a cited review; none of it edits your fields."
          >
            <LensRunner
              target={{ type: "thesis", id: thesis.id }}
              ticker={thesis.ticker}
              thesisId={thesis.id}
              title="Challenge, explain, or review this thesis"
              onAcceptAsQuestion={readOnly ? null : acceptAsQuestion}
            />
          </Block>

          <Block title="Lifecycle">
            <LifecycleControl
              status={form.status}
              pending={pendingStatus}
              closeReason={pendingReason}
              invalidationConditions={thesis.invalidationConditions || []}
              closedByConditionId={pendingCondition}
              onPick={(s) => {
                setPendingStatus(s);
                setPendingReason("");
                setPendingCondition("");
              }}
              onCloseReason={setPendingReason}
              onClosedByCondition={setPendingCondition}
              onConfirm={confirmLifecycle}
              onCancel={resetLifecycle}
              busy={busy}
              disabled={dirty}
              idPrefix={idPrefix}
            />
            {dirty && (
              <p className="text-[11.5px] text-muted/70">
                Save or discard your edits first — a lifecycle change is saved on its own.
              </p>
            )}
            {form.closeReason && (
              <p className="text-[11.5px] text-muted">
                Closed as <span className="font-semibold text-fg/80">{form.closeReason.replace(/_/g, " ")}</span>
                {thesis.closedAt ? <> on {fmtWhen(thesis.closedAt)}</> : null}.
              </p>
            )}
          </Block>

          <Block
            title="Your review checkpoint"
            note="Marks how far you have reviewed. Only you move it."
          >
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-[11.5px] text-muted">
                {thesis.lastReview ? (
                  <>Last reviewed {fmtWhenExact(thesis.lastReview.at)}</>
                ) : (
                  <>Never marked reviewed.</>
                )}
              </span>
              <button
                type="button"
                onClick={onMarkReviewed}
                disabled={busy}
                className="ml-auto rounded-full border border-line bg-panel2 px-2.5 py-1.5 text-[11.5px] font-semibold text-fg transition-colors hover:border-accent/60 hover:text-accent disabled:opacity-40"
              >
                Mark reviewed now
              </button>
            </div>
          </Block>

          <Block title="History" note="Every saved revision you have made.">
            <VersionHistory thesisId={thesis.id} refreshKey={historyKey} />
          </Block>

          <Block>
            {!confirmDelete ? (
              <div className="flex flex-wrap items-center gap-2">
                <span className="text-[11.5px] text-muted/70">
                  Archiving keeps the record. Deleting removes it permanently.
                </span>
                <button
                  type="button"
                  onClick={() => setConfirmDelete(true)}
                  disabled={busy}
                  className="ml-auto rounded-full border border-line px-2.5 py-1.5 text-[11.5px] font-semibold text-muted transition-colors hover:border-down/60 hover:text-down disabled:opacity-40"
                >
                  Delete thesis
                </button>
              </div>
            ) : (
              <div className="flex flex-wrap items-center gap-2 rounded-lg border border-down/40 bg-down/[0.06] px-2.5 py-2">
                <span className="text-[11.5px] font-semibold text-down">
                  Delete this thesis and its history permanently?
                </span>
                <span className="ml-auto flex gap-1.5">
                  <button
                    type="button"
                    onClick={() => onDelete().then((ok) => !ok && setConfirmDelete(false))}
                    disabled={busy}
                    className="rounded-full bg-down px-2.5 py-1.5 text-[11.5px] font-bold text-bg transition-[filter] hover:brightness-110 disabled:opacity-50"
                  >
                    Delete
                  </button>
                  <button
                    type="button"
                    onClick={() => setConfirmDelete(false)}
                    disabled={busy}
                    className="rounded-full border border-line px-2.5 py-1.5 text-[11.5px] font-semibold text-muted hover:text-fg disabled:opacity-50"
                  >
                    Keep
                  </button>
                </span>
              </div>
            )}
          </Block>
        </>
      )}
    </div>
  );
}
