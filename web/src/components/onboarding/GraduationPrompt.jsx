import { useEffect, useState } from "react";
import { fetchJournalStats } from "../../lib/api.js";
import { useSettings } from "../../settings/SettingsContext.jsx";
import { Icon } from "../shell/icons.jsx";

// GraduationPrompt — an EARNED unlock to the full cockpit (docs/BEGINNER.md Piece 4). Graduation is
// not just a toggle: once the user has logged enough of their OWN checklist-complete closed trades, a
// one-time dismissible prompt offers to switch experienceLevel to "standard". The readiness number is
// the user's record (journal /stats checklistSplit.withAllPass.count = closed trades with an all-pass
// checklist) — no new endpoint. Dismissal is remembered for the session so it doesn't nag. Unlocking
// changes only which views/density they see — it grants no trading action (invariant #2).

const GRADUATION_N = 20; // checklist-complete closed trades that earn the full cockpit
const DISMISS_KEY = "nvda:graduation-dismissed";

export default function GraduationPrompt() {
  const { settings, saveSettings } = useSettings();
  const beginner = settings.experienceLevel !== "standard";
  const [count, setCount] = useState(0);
  const [saving, setSaving] = useState(false);
  const [dismissed, setDismissed] = useState(() => {
    try {
      return sessionStorage.getItem(DISMISS_KEY) === "1";
    } catch {
      return false;
    }
  });

  useEffect(() => {
    if (!beginner || dismissed) return;
    let alive = true;
    const load = () =>
      fetchJournalStats("all")
        .then((s) => {
          if (alive) setCount(s?.checklistSplit?.withAllPass?.count || 0);
        })
        .catch(() => {});
    load();
    const id = setInterval(load, 60000);
    return () => {
      alive = false;
      clearInterval(id);
    };
  }, [beginner, dismissed]);

  if (!beginner || dismissed || count < GRADUATION_N) return null;

  const unlock = async () => {
    setSaving(true);
    try {
      await saveSettings({ experienceLevel: "standard" });
    } catch {
      /* guest / offline: keep beginner mode, no error surfaced here */
    } finally {
      setSaving(false);
    }
  };
  const dismiss = () => {
    try {
      sessionStorage.setItem(DISMISS_KEY, "1");
    } catch {
      /* ignore */
    }
    setDismissed(true);
  };

  return (
    <div className="mb-4 flex flex-wrap items-center gap-3 rounded-lg border border-accent/40 bg-accent/10 px-4 py-3 text-[13px]">
      <span className="text-fg">
        <Icon name="graduation" size={15} className="mb-0.5 inline-block align-middle" /> You've logged <strong className="num">{count}</strong> disciplined, checklist-complete trades. Ready for the full
        cockpit — committee, confluence, transcripts, alerts and more?
      </span>
      <div className="ml-auto flex items-center gap-2">
        <button
          type="button"
          onClick={unlock}
          disabled={saving}
          className="rounded-full border border-accent/50 bg-accent/15 px-3.5 py-1.5 text-[12.5px] font-semibold text-accent transition-colors hover:bg-accent/25 disabled:opacity-50"
        >
          {saving ? "Unlocking…" : "Unlock full cockpit →"}
        </button>
        <button
          type="button"
          onClick={dismiss}
          className="rounded-full border border-line2 px-3 py-1.5 text-[12.5px] text-muted transition-colors hover:text-fg"
        >
          Not yet
        </button>
      </div>
    </div>
  );
}
