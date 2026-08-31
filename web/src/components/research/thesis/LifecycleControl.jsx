import { cx } from "../../../lib/cx.js";
import { CLOSE_REASONS, LEGAL_TRANSITIONS, STATUS_META } from "../../../lib/thesisApi.js";

// LifecycleControl — moves a thesis through draft → active → closed → archived.
//
// It offers ONLY the transitions PARALLEL_CONTRACTS.md §1.3 declares legal, so the UI cannot ask the
// server for something it will refuse. Two rules worth naming, because they look like omissions:
//
//   * `invalidated` is a close REASON, never a status. There is no "Invalidated" button here.
//   * `closed → active` is absent by design. A reopen goes through `archived`, so reopening is always
//     visible in the record rather than silently reverting a conclusion.
//
// Closing requires a reason, which is why picking "Close" reveals the reason selector and the confirm
// button stays disabled until one is chosen.
export default function LifecycleControl({
  status,
  pending,
  closeReason,
  invalidationConditions = [],
  closedByConditionId,
  onPick,
  onCloseReason,
  onClosedByCondition,
  onConfirm,
  onCancel,
  busy = false,
  disabled = false,
  idPrefix,
}) {
  const legal = LEGAL_TRANSITIONS[status] || [];
  const current = STATUS_META[status] || STATUS_META.draft;
  const needsReason = pending === "closed";
  const canConfirm = Boolean(pending) && (!needsReason || Boolean(closeReason));

  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-[11px] font-semibold uppercase tracking-wide text-muted">Lifecycle</span>
        <span className={cx("flex items-center gap-1.5 text-[12px] font-semibold", current.tone)}>
          <span aria-hidden="true">{current.glyph}</span>
          {current.label}
        </span>
        <span className="text-[11.5px] text-muted/70">{current.help}</span>
      </div>

      {legal.length === 0 ? (
        <p className="text-[11.5px] text-muted/70">No further lifecycle changes from here.</p>
      ) : (
        <div className="flex flex-wrap items-center gap-1.5">
          {legal.map((next) => {
            const meta = STATUS_META[next];
            const active = pending === next;
            return (
              <button
                key={next}
                type="button"
                disabled={disabled || busy}
                aria-pressed={active}
                onClick={() => onPick(active ? null : next)}
                className={cx(
                  "rounded-full border px-2.5 py-1.5 text-[11.5px] font-semibold transition-colors disabled:opacity-40",
                  active
                    ? "border-accent/70 bg-accent/15 text-accent"
                    : "border-line bg-panel2 text-muted hover:border-line2 hover:text-fg"
                )}
              >
                <span aria-hidden="true" className="mr-1 opacity-70">
                  {meta.glyph}
                </span>
                {next === "active" && status === "archived" ? "Reopen as active" : `Mark ${meta.label.toLowerCase()}`}
              </button>
            );
          })}
        </div>
      )}

      {pending && (
        <div className="flex flex-col gap-2 rounded-lg border border-line2 bg-panel2/60 p-2.5">
          <p className="text-[11.5px] leading-relaxed text-muted">
            {pending === "active" && status === "draft" && (
              <>
                Activating counts this thesis against the limit of{" "}
                <span className="font-mono text-fg">3</span> active theses for this ticker.
              </>
            )}
            {pending === "active" && status === "archived" && (
              <>Reopening puts this thesis back into your active set. The archive step stays in history.</>
            )}
            {pending === "closed" && <>Closing records a conclusion. It needs a reason.</>}
            {pending === "archived" && <>Archiving keeps the thesis for the record and out of your active set.</>}
          </p>

          {needsReason && (
            <>
              <div className="flex min-w-0 flex-col gap-1">
                <label
                  htmlFor={`${idPrefix}-close-reason`}
                  className="text-[11px] font-semibold uppercase tracking-wide text-muted"
                >
                  Close reason
                </label>
                <select
                  id={`${idPrefix}-close-reason`}
                  value={closeReason || ""}
                  disabled={busy}
                  onChange={(e) => onCloseReason(e.target.value)}
                  className="w-full rounded-md border border-line bg-panel2 px-2.5 py-1.5 text-[12.5px] text-fg focus:outline-none focus-visible:border-accent"
                >
                  <option value="">Choose a reason…</option>
                  {CLOSE_REASONS.map((r) => (
                    <option key={r.value} value={r.value}>
                      {r.label}
                    </option>
                  ))}
                </select>
              </div>

              {/* Optional link back to the condition that fired. Only offered for `invalidated`, and
                  only when the thesis actually HAS saved invalidation conditions — a user can
                  conclude invalidation without a machine-evaluable condition (§1.3). */}
              {closeReason === "invalidated" && invalidationConditions.length > 0 && (
                <div className="flex min-w-0 flex-col gap-1">
                  <label
                    htmlFor={`${idPrefix}-closed-by`}
                    className="text-[11px] font-semibold uppercase tracking-wide text-muted"
                  >
                    Which condition fired? <span className="font-normal normal-case text-muted/60">(optional)</span>
                  </label>
                  <select
                    id={`${idPrefix}-closed-by`}
                    value={closedByConditionId || ""}
                    disabled={busy}
                    onChange={(e) => onClosedByCondition(e.target.value)}
                    className="w-full rounded-md border border-line bg-panel2 px-2.5 py-1.5 text-[12.5px] text-fg focus:outline-none focus-visible:border-accent"
                  >
                    <option value="">Not tied to one condition</option>
                    {invalidationConditions.map((c) => (
                      <option key={c.id} value={c.id}>
                        {c.text.length > 80 ? `${c.text.slice(0, 80)}…` : c.text}
                      </option>
                    ))}
                  </select>
                </div>
              )}
            </>
          )}

          <div className="flex flex-wrap gap-1.5">
            <button
              type="button"
              disabled={!canConfirm || busy}
              onClick={onConfirm}
              className="rounded-full bg-fg px-3 py-1.5 text-[11.5px] font-semibold text-bg transition-colors duration-200 ease-premium hover:bg-white disabled:opacity-40"
            >
              {busy ? "Saving…" : `Confirm ${STATUS_META[pending]?.label.toLowerCase()}`}
            </button>
            <button
              type="button"
              disabled={busy}
              onClick={onCancel}
              className="rounded-full border border-line px-2.5 py-1.5 text-[11.5px] font-semibold text-muted transition-colors hover:text-fg disabled:opacity-40"
            >
              Cancel
            </button>
          </div>
        </div>
      )}
    </div>
  );
}
