import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useAuth } from "../../../auth/AuthContext.jsx";
import { EmptyState, useToast } from "../../ui/index.js";
import { cx } from "../../../lib/cx.js";
import ThesisEditor from "./ThesisEditor.jsx";
import ThesisList from "./ThesisList.jsx";
import WhatChangedPanel from "../../monitoring/WhatChangedPanel.jsx";
import {
  AuthRequiredError,
  ITEM_LISTS,
  ThesisApiError,
  buildPatch,
  createThesisV2,
  deleteThesisV2,
  listThesesV2,
  markThesisReviewed,
  patchThesis,
  runThesisCheck,
  toForm,
  validateForm,
} from "../../../lib/thesisApi.js";

// ThesisWorkspace — the Thesis section of Research: the primary USER-OWNED object in the app.
//
// It owns four things the child components deliberately do not:
//
//   1. Load + selection, persisted per ticker so a refresh or a trip to another section returns you
//      to the thesis you were working on.
//   2. Draft safety. Unsaved edits are mirrored into sessionStorage keyed by thesis id, so switching
//      ticker or section and coming back restores them; switching thesis in-place asks first; and a
//      refresh/close while dirty triggers the browser's own warning. (A hard route-level veto would
//      mean editing App.jsx, a shared file this lane must not broaden — see the Step 06 handoff.)
//   3. Writes, and the mapping from the journal's error envelope onto a specific input.
//   4. The guest boundary: a guest can read everything and is routed to sign-in at the first write.
//
// What it must NOT do, and does not: put model output into a user field. `lastCheck` is rendered in
// its own read-only card and never merged into `form`.

const SEL_KEY = (ticker) => `attestel.thesis.sel.${ticker}`;
const DRAFT_KEY = (ticker, id) => `attestel.thesis.draft.${ticker}.${id || "new"}`;

const readJSON = (key) => {
  try {
    const raw = sessionStorage.getItem(key);
    return raw ? JSON.parse(raw) : null;
  } catch {
    return null;
  }
};
const writeJSON = (key, value) => {
  try {
    sessionStorage.setItem(key, JSON.stringify(value));
  } catch {
    /* private mode / quota — draft safety is best-effort, never load-bearing */
  }
};
const dropKey = (key) => {
  try {
    sessionStorage.removeItem(key);
  } catch {
    /* ignore */
  }
};

// sameForm compares edited state against the record's baseline. Item rows are compared on the fields
// the user can actually change, so re-ordering or retyping counts and nothing else does.
function sameForm(a, b) {
  if (a.claim.trim() !== b.claim.trim()) return false;
  if (a.notes !== b.notes) return false;
  if (a.horizon !== b.horizon) return false;
  if ((a.confidence ?? null) !== (b.confidence ?? null)) return false;
  for (const l of ITEM_LISTS) {
    const x = (a[l.field] || []).filter((r) => r.text.trim() !== "");
    const y = (b[l.field] || []).filter((r) => r.text.trim() !== "");
    if (x.length !== y.length) return false;
    for (let i = 0; i < x.length; i++) {
      if (x[i].text.trim() !== y[i].text.trim()) return false;
      if ((x[i].resolvedAt ?? null) !== (y[i].resolvedAt ?? null)) return false;
      if ((x[i].dueAt ?? null) !== (y[i].dueAt ?? null)) return false;
    }
  }
  return true;
}

export default function ThesisWorkspace({ ticker }) {
  const { user, requireAuth, promptSignIn } = useAuth();
  const toast = useToast();
  const isGuest = !user;

  const [theses, setTheses] = useState(null); // null = still loading
  const [loadError, setLoadError] = useState(null);
  const [selectedId, setSelectedId] = useState(null);
  const [creating, setCreating] = useState(false);
  const [form, setForm] = useState(() => toForm(null));
  const [reason, setReason] = useState("");
  const [errors, setErrors] = useState({});
  const [saving, setSaving] = useState(false);
  const [checking, setChecking] = useState(false);
  const [historyKey, setHistoryKey] = useState(0);
  const [pendingSwitch, setPendingSwitch] = useState(null); // thesis id the user tried to switch to

  const selected = useMemo(
    () => (creating ? null : (theses || []).find((t) => t.id === selectedId) || null),
    [theses, selectedId, creating]
  );

  const baseline = useMemo(() => toForm(selected), [selected]);
  const dirty = creating ? Boolean(form.claim.trim()) : !sameForm(form, baseline);

  // ---- load -------------------------------------------------------------------------------------

  const load = useCallback(
    (opts = {}) => {
      let alive = true;
      listThesesV2(ticker)
        .then((list) => {
          if (!alive) return;
          setTheses(list);
          setLoadError(null);
          if (opts.selectId) {
            setSelectedId(opts.selectId);
            return;
          }
          // Restore the previous selection when it still exists; otherwise prefer an active thesis.
          setSelectedId((cur) => {
            const stored = cur || readJSON(SEL_KEY(ticker));
            if (stored && list.some((t) => t.id === stored)) return stored;
            return (list.find((t) => t.status === "active") || list[0])?.id || null;
          });
        })
        .catch((e) => {
          if (!alive) return;
          setTheses([]);
          setLoadError(
            e instanceof ThesisApiError && e.isUnavailable
              ? "The journal service is not responding, so your theses could not be loaded. Nothing has been lost — try again shortly."
              : e.message
          );
        });
      return () => {
        alive = false;
      };
    },
    [ticker]
  );

  // Reload on ticker change. Selection and creating-state reset; drafts survive in sessionStorage.
  useEffect(() => {
    setTheses(null);
    setSelectedId(null);
    setCreating(false);
    setErrors({});
    return load();
  }, [ticker, user?.id, load]);

  useEffect(() => {
    if (selectedId) writeJSON(SEL_KEY(ticker), selectedId);
  }, [ticker, selectedId]);

  // ---- form hydration: stored record, overlaid with any unsaved draft --------------------------

  const draftKey = DRAFT_KEY(ticker, creating ? "new" : selectedId);

  useEffect(() => {
    if (!creating && !selectedId) {
      setForm(toForm(null));
      setReason("");
      return;
    }
    const base = toForm(creating ? null : selected);
    const draft = readJSON(DRAFT_KEY(ticker, creating ? "new" : selectedId));
    setForm(draft?.form ? { ...base, ...draft.form } : base);
    setReason(draft?.reason || "");
    setErrors({});
  }, [ticker, selectedId, creating, selected]); // eslint-disable-line react-hooks/exhaustive-deps

  // Mirror unsaved work into sessionStorage; clear the entry the moment it matches the record again.
  useEffect(() => {
    if (!creating && !selectedId) return;
    if (dirty) writeJSON(draftKey, { form, reason });
    else dropKey(draftKey);
  }, [draftKey, form, reason, dirty, creating, selectedId]);

  // The browser's own guard for refresh / tab close while dirty.
  useEffect(() => {
    if (!dirty) return;
    const onBeforeUnload = (e) => {
      e.preventDefault();
      e.returnValue = "";
    };
    window.addEventListener("beforeunload", onBeforeUnload);
    return () => window.removeEventListener("beforeunload", onBeforeUnload);
  }, [dirty]);

  // ---- error mapping ---------------------------------------------------------------------------

  // applyError turns one thrown error into either a field-attached message or a form-level one. A
  // guest write routes to sign-in instead of showing an error at all.
  const applyError = useCallback(
    (e) => {
      if (e instanceof AuthRequiredError) {
        promptSignIn();
        return false;
      }
      if (e instanceof ThesisApiError) {
        if (e.code === "active_thesis_cap") {
          setErrors({
            _form: `You already have ${e.activeCount ?? e.cap} active theses for ${ticker} (the limit is ${e.cap ?? 3}). Close or archive one first — drafts are unlimited.`,
          });
          return false;
        }
        if (e.isUnavailable) {
          setErrors({
            _form:
              "The journal service is not responding, so nothing was saved. Your edits are still here — try again shortly.",
          });
          return false;
        }
        if (e.field) {
          setErrors({ [e.field]: e.message });
          return false;
        }
      }
      setErrors({ _form: e.message });
      return false;
    },
    [promptSignIn, ticker]
  );

  // ---- writes ----------------------------------------------------------------------------------

  const save = () => {
    const clientErrors = validateForm(form);
    if (Object.keys(clientErrors).length) {
      setErrors(clientErrors);
      return;
    }
    if (!requireAuth()) return; // guest -> routed to sign-in before any write is attempted
    setErrors({});
    setSaving(true);

    const done = (t, msg) => {
      dropKey(draftKey);
      setCreating(false);
      setReason("");
      setHistoryKey((k) => k + 1);
      load({ selectId: t?.id });
      toast({ title: msg, tone: "success" });
    };

    if (creating) {
      const fields = {
        claim: form.claim.trim(),
        horizon: form.horizon || null,
        confidence: form.confidence ?? null,
        notes: form.notes,
      };
      for (const l of ITEM_LISTS) {
        fields[l.field] = (form[l.field] || [])
          .filter((r) => r.text.trim() !== "")
          .map((r) => ({ text: r.text.trim(), resolvedAt: r.resolvedAt ?? null, dueAt: r.dueAt ?? null }));
      }
      createThesisV2(ticker, fields)
        .then((t) => done(t, "Thesis created as a draft"))
        .catch(applyError)
        .finally(() => setSaving(false));
      return;
    }

    const patch = buildPatch(form, selected);
    if (!Object.keys(patch).length) {
      setSaving(false);
      return;
    }
    if (reason.trim()) patch.reason = reason.trim();
    patchThesis(selected.id, patch)
      .then((t) => done(t, "Thesis saved"))
      .catch(applyError)
      .finally(() => setSaving(false));
  };

  const discard = () => {
    dropKey(draftKey);
    setErrors({});
    setReason("");
    if (creating) {
      setCreating(false);
      setForm(toForm(selected));
    } else {
      setForm(toForm(selected));
    }
  };

  // changeStatus is a standalone write: a lifecycle move is never bundled with unsaved field edits,
  // which is why the editor disables it while dirty.
  const changeStatus = (status, extra = {}) => {
    if (!requireAuth()) return Promise.resolve(false);
    setErrors({});
    setSaving(true);
    const patch = { status };
    if (extra.closeReason) patch.closeReason = extra.closeReason;
    if (extra.closedByConditionId) patch.closedByConditionId = extra.closedByConditionId;
    // Leaving `closed` must clear the reason, which §1.2 requires to be null off that status.
    if (status !== "closed" && selected?.closeReason) patch.closeReason = null;

    return patchThesis(selected.id, patch)
      .then((t) => {
        setHistoryKey((k) => k + 1);
        load({ selectId: t?.id });
        toast({ title: `Thesis marked ${status}`, tone: "success" });
        return true;
      })
      .catch(applyError)
      .finally(() => setSaving(false));
  };

  const remove = () => {
    if (!requireAuth()) return Promise.resolve(false);
    setSaving(true);
    return deleteThesisV2(selected.id)
      .then(() => {
        dropKey(draftKey);
        dropKey(SEL_KEY(ticker));
        setSelectedId(null);
        load();
        toast({ title: "Thesis deleted", tone: "success" });
        return true;
      })
      .catch(applyError)
      .finally(() => setSaving(false));
  };

  const markReviewed = () => {
    if (!requireAuth()) return;
    setSaving(true);
    markThesisReviewed(selected.id, 0)
      .then((t) => {
        load({ selectId: t?.id });
        toast({ title: "Marked reviewed", tone: "success" });
      })
      .catch(applyError)
      .finally(() => setSaving(false));
  };

  const check = () => {
    if (!requireAuth()) return;
    setChecking(true);
    runThesisCheck(selected.id)
      .then(() => load({ selectId: selected.id }))
      .catch((e) => {
        if (e instanceof AuthRequiredError) promptSignIn();
        else
          toast({
            title: "Check unavailable",
            message: e.message,
            tone: "error",
          });
      })
      .finally(() => setChecking(false));
  };

  // ---- selection, with the unsaved-changes gate -------------------------------------------------

  const trySelect = (id) => {
    if (id === selectedId && !creating) return;
    if (dirty) {
      setPendingSwitch(id);
      return;
    }
    setCreating(false);
    setSelectedId(id);
  };

  const tryNew = () => {
    if (creating) return;
    if (dirty) {
      setPendingSwitch("__new__");
      return;
    }
    setCreating(true);
    setForm(toForm(null));
    setErrors({});
  };

  const resolveSwitch = (keepEditing) => {
    const target = pendingSwitch;
    setPendingSwitch(null);
    if (keepEditing || !target) return;
    // Discarding here is explicit: the user chose "leave without saving".
    dropKey(draftKey);
    setReason("");
    setErrors({});
    if (target === "__new__") {
      setCreating(true);
      setForm(toForm(null));
    } else {
      setCreating(false);
      setSelectedId(target);
    }
  };

  // ---- render -----------------------------------------------------------------------------------

  const list = theses || [];
  const showEditor = creating || Boolean(selected);

  return (
    <section className="overflow-hidden surface-card">
      {loadError && (
        <p className="border-b border-line bg-down/[0.07] px-[18px] py-2.5 text-[12px] leading-relaxed text-down">
          {loadError}
        </p>
      )}

      {theses === null ? (
        <p className="px-[18px] py-6 text-center text-[12.5px] text-muted">Loading your theses…</p>
      ) : list.length === 0 && !creating ? (
        // Empty state: one instruction, not a feature tour. Research language, never a trade call.
        <div className="px-[18px] py-6">
          <EmptyState
            title={`No thesis for ${ticker} yet`}
            action={
              <button
                type="button"
                onClick={tryNew}
                className="rounded-full bg-fg px-3.5 py-1.5 text-[12px] font-semibold text-bg transition-colors duration-200 ease-premium hover:bg-white"
              >
                Write your claim
              </button>
            }
          >
            Start with one sentence you could be proven wrong about — what you think is true about{" "}
            {ticker}, and why. You can add assumptions, catalysts, risks, and the conditions that
            would change your mind once the claim is down.
          </EmptyState>
        </div>
      ) : (
        <div className="grid grid-cols-1 lg:grid-cols-[280px_minmax(0,1fr)]">
          <div className="min-w-0 border-b border-line lg:border-b-0 lg:border-r lg:border-r-line">
            <ThesisList
              theses={list}
              selectedId={creating ? null : selectedId}
              dirtyId={dirty && !creating ? selectedId : null}
              onSelect={trySelect}
              onNew={tryNew}
              canCreate={!creating}
            />
          </div>

          <div className="min-w-0">
            {showEditor ? (
              <ThesisEditor
                thesis={creating ? null : selected}
                form={form}
                onForm={setForm}
                errors={errors}
                dirty={dirty}
                busy={saving}
                saving={saving}
                readOnly={isGuest}
                onSave={save}
                onDiscard={discard}
                onDelete={remove}
                onStatusChange={changeStatus}
                onMarkReviewed={markReviewed}
                onRunCheck={check}
                checking={checking}
                reason={reason}
                onReason={setReason}
                historyKey={historyKey}
              />
            ) : (
              <p className="px-[18px] py-6 text-center text-[12.5px] text-muted">
                Pick a thesis on the left, or start a new one.
              </p>
            )}

            {/* Step 08's "What changed since your last review?" (Contracts §4.4), mounted by the
                Wave 4 integrator — the panel is Lane A's, this file is Lane B-adjacent (Step 05's),
                so neither lane could wire it itself.

                It sits below the editor and only for a SAVED thesis: the change set is computed
                against that thesis's stored review checkpoint, so there is nothing to compare while
                a new one is still being drafted. The panel fetches on its own and renders an honest
                empty state, and its only write is advancing the checkpoint. */}
            {!creating && selectedId && (
              <div className="border-t border-line">
                <WhatChangedPanel thesisId={selectedId} />
              </div>
            )}
          </div>
        </div>
      )}

      {/* Unsaved-changes gate. A confirm step rather than a silent discard — the claim is the user's
          own writing and losing it to a stray click is the worst failure this screen can have. */}
      {pendingSwitch && (
        <div
          role="alertdialog"
          aria-modal="true"
          aria-labelledby="thesis-switch-title"
          className="fixed inset-0 z-50 flex items-center justify-center bg-bg/70 p-4 backdrop-blur-sm"
        >
          <div className="w-full max-w-[400px] rounded-xl border border-line2 bg-panel p-4 shadow-2xl">
            <h2 id="thesis-switch-title" className="text-[13px] font-semibold text-fg">
              Leave without saving?
            </h2>
            <p className="mt-1.5 text-[12px] leading-relaxed text-muted">
              You have unsaved changes to this thesis. Leaving now discards them.
            </p>
            <div className="mt-3 flex flex-wrap justify-end gap-1.5">
              <button
                type="button"
                autoFocus
                onClick={() => resolveSwitch(true)}
                className="rounded-full bg-fg px-3.5 py-1.5 text-[12px] font-semibold text-bg transition-colors duration-200 ease-premium hover:bg-white"
              >
                Keep editing
              </button>
              <button
                type="button"
                onClick={() => resolveSwitch(false)}
                className={cx(
                  "rounded-full border border-line px-3 py-1.5 text-[12px] font-semibold text-muted",
                  "transition-colors hover:border-down/60 hover:text-down"
                )}
              >
                Discard and leave
              </button>
            </div>
          </div>
        </div>
      )}
    </section>
  );
}
