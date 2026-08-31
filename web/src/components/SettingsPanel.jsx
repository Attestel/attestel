import { useEffect, useState } from "react";
import { AdminRequiredError, AuthRequiredError } from "../lib/api.js";
import { useAuth } from "../auth/AuthContext.jsx";
import { useSettings } from "../settings/SettingsContext.jsx";
import { useToast } from "./ui/index.js";
import { Tag } from "./terminal/bits.jsx";
import { cx } from "../lib/cx.js";

// SettingsPanel — signal preferences plus the explicit champion/challenger lifecycle. Saved server-side (they follow the user across
// devices). A guest sees a sign-in prompt, not the form (writes are gated everywhere in this app).
//
// These are PREFERENCES ONLY. "allow short" merely lets the walk-forward-validated model emit a SHORT
// bias; nothing here executes a trade (invariant #2). Training creates an immutable candidate;
// promotion is separate, evidence-gated, and admin-only.

const HORIZONS = [3, 5, 10];

function relTime(ms) {
  if (!ms) return "never";
  const s = Math.max(0, Math.floor((Date.now() - ms) / 1000));
  if (s < 60) return "just now";
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

const microLabel = "mb-2 label-mono text-muted";

// Segmented control — one row of pill buttons, active = accent fill. Matches the terminal aesthetic.
function Segmented({ items, value, onChange, disabled, ariaLabel }) {
  return (
    <div role="group" aria-label={ariaLabel} className="inline-flex flex-wrap gap-[3px] rounded-md border border-line p-[3px]">
      {items.map((it) => {
        const active = it.v === value;
        return (
          <button
            key={it.v}
            type="button"
            aria-pressed={active}
            disabled={disabled}
            onClick={() => onChange(it.v)}
            className={cx(
              "num rounded-full px-3.5 py-1.5 text-[12.5px] font-semibold transition-colors disabled:opacity-50",
              active ? "bg-accent/15 text-accent" : "text-muted hover:bg-white/10 hover:text-fg"
            )}
          >
            {it.label}
          </button>
        );
      })}
    </div>
  );
}

function Row({ label, hint, children }) {
  return (
    <div className="flex flex-col gap-3 border-b border-line/60 px-[22px] py-5 sm:flex-row sm:items-center sm:justify-between">
      <div className="max-w-[380px]">
        <div className="text-[13.5px] font-semibold text-fg">{label}</div>
        {hint && <p className="mt-1 text-[12px] leading-relaxed text-muted">{hint}</p>}
      </div>
      <div className="shrink-0">{children}</div>
    </div>
  );
}

// GuestPrompt — what a signed-out visitor sees instead of the form.
function GuestPrompt({ onSignIn }) {
  return (
    <div className="px-[22px] py-12 text-center">
      <div className="mx-auto mb-4 flex h-11 w-11 items-center justify-center rounded-full border border-line2 text-muted">
        <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <rect x="3" y="11" width="18" height="10" rx="2" />
          <path d="M7 11V7a5 5 0 0 1 10 0v4" />
        </svg>
      </div>
      <div className="mb-1.5 text-[15px] font-semibold text-fg">Sign in to change settings</div>
      <p className="mx-auto mb-5 max-w-[420px] text-[12.5px] leading-relaxed text-muted">
        Settings are saved to your account so they sync across devices — model defaults, risk controls,
        and your default horizon. Browsing stays open to guests; only saving is
        gated.
      </p>
      <button
        onClick={onSignIn}
        className="rounded-full border border-accent/50 bg-accent/10 px-5 py-2.5 text-[13px] font-semibold text-accent transition-colors hover:bg-accent/16"
      >
        Sign in
      </button>
    </div>
  );
}

export default function SettingsPanel({ onReviewRealityCheck }) {
  const { user, promptSignIn } = useAuth();
  const {
    settings, saveSettings, lastTrainedAt, autoTraining, trainNow,
    modelRegistry, target, promoteNow, rollbackNow, refreshModelRegistry,
  } = useSettings();
  const toast = useToast();
  const [saving, setSaving] = useState(false);
  const [deploying, setDeploying] = useState("");

  const lifecycle = modelRegistry.filter(
    (r) => r.ticker === target.ticker && r.timeframe === target.timeframe
  );
  const candidate = lifecycle.find(
    (r) => {
      if (r.deploymentState !== "candidate") return false;
      const parent = lifecycle.find((a) => a.active && a.horizon === r.horizon);
      return !parent || r.parentModelVersion === parent.modelVersion;
    }
  );
  const activeModel = lifecycle.find(
    (r) => r.active && r.horizon === (candidate?.horizon ?? settings.defaultHorizon)
  );
  const rollbackTarget = lifecycle.find(
    (r) => r.deploymentState === "archived" && r.horizon === activeModel?.horizon
  );
  const firstFailedGate = candidate?.promotionGates?.find((g) => !g.passed);

  // Account equity is a free number (unlike the bounded presets), so it's edited locally and persisted
  // on blur. Seeded from the saved value; "" shows as an empty field (0 = unset).
  const [equity, setEquity] = useState("");
  useEffect(() => {
    setEquity(settings.accountEquity ? String(settings.accountEquity) : "");
  }, [settings.accountEquity]);

  // Desktop-notification capability + live permission state. Notifications need a Notification API AND
  // a secure context (HTTPS or localhost) — a plain-IP dev box can't request permission.
  const notifSupported = typeof window !== "undefined" && "Notification" in window;
  const secureCtx = typeof window !== "undefined" && window.isSecureContext;
  const canNotify = notifSupported && secureCtx;
  const [permState, setPermState] = useState(() =>
    notifSupported ? window.Notification.permission : "unsupported"
  );

  // Re-render every 30s so the "last trained … ago" line ages without a state change.
  const [, setNow] = useState(0);
  useEffect(() => {
    const id = setInterval(() => setNow((n) => n + 1), 30000);
    return () => clearInterval(id);
  }, []);

  async function change(patch) {
    setSaving(true);
    try {
      await saveSettings(patch);
      toast({ tone: "success", title: "Settings saved" });
    } catch (err) {
      if (err instanceof AuthRequiredError) {
        promptSignIn();
        return;
      }
      toast({ tone: "error", title: "Could not save settings", message: err.message });
    } finally {
      setSaving(false);
    }
  }

  // The permission request MUST run inside the click handler (browsers require a user gesture). Only
  // persist `true` when the OS actually grants; denied/dismissed/unsupported keeps it off (saves false).
  async function onToggleNotifications(e) {
    const wantOn = e.target.checked;
    if (!wantOn) {
      await change({ browserNotifications: false });
      return;
    }
    let perm = window.Notification.permission;
    if (perm === "default") {
      try {
        perm = await window.Notification.requestPermission();
      } catch {
        perm = window.Notification.permission;
      }
    }
    setPermState(perm);
    await change({ browserNotifications: perm === "granted" });
  }

  // Persist the equity on blur, only when it actually changed (clamped to >= 0).
  async function saveEquity() {
    const v = Math.max(0, Number(equity) || 0);
    if (v === settings.accountEquity) return;
    await change({ accountEquity: v });
  }

  async function onTrainNow() {
    try {
      const trained = await trainNow();
      toast(trained
        ? { tone: "success", title: "Candidate trained", message: "Serving did not change. Review its gates before promotion." }
        : { tone: "info", title: "Training already running", message: "A candidate run is in flight — try again shortly." });
    } catch (err) {
      if (err instanceof AuthRequiredError) return promptSignIn();
      toast({
        tone: err instanceof AdminRequiredError ? "info" : "error",
        title: err instanceof AdminRequiredError ? "Admin access required" : "Candidate training failed",
        message: err.message,
      });
    }
  }

  async function onPromote() {
    if (!candidate) return;
    setDeploying("promote");
    try {
      await promoteNow(candidate, "Promoted from the Settings champion/challenger review");
      toast({ tone: "success", title: "Candidate promoted", message: `${candidate.modelVersion} is now active.` });
    } catch (err) {
      toast({
        tone: err instanceof AdminRequiredError ? "info" : "error",
        title: err instanceof AdminRequiredError ? "Admin access required" : "Promotion refused",
        message: err.message,
      });
    } finally {
      setDeploying("");
    }
  }

  async function onRollback() {
    if (!rollbackTarget) return;
    setDeploying("rollback");
    try {
      await rollbackNow(rollbackTarget, "Rolled back from the Settings champion/challenger review");
      toast({ tone: "success", title: "Model rolled back", message: `${rollbackTarget.modelVersion} is active again.` });
    } catch (err) {
      toast({
        tone: err instanceof AdminRequiredError ? "info" : "error",
        title: err instanceof AdminRequiredError ? "Admin access required" : "Rollback failed",
        message: err.message,
      });
    } finally {
      setDeploying("");
    }
  }

  return (
    <section className="mx-auto max-w-[860px] overflow-hidden surface-card">
      <div className="flex h-11 items-center gap-2.5 border-b border-line px-[22px]">
        <span className="text-[14px] font-semibold text-fg">Settings</span>
        <Tag tone="outline">Directional signal · Suggestion only</Tag>
      </div>

      {!user ? (
        <GuestPrompt onSignIn={promptSignIn} />
      ) : (
        <>
          {/* Experience level — the strong default that gates surface + density (docs/BEGINNER.md P4) */}
          <div className="flex h-9 items-center gap-2.5 border-b border-t border-line bg-panel2/30 px-[22px]">
            <span className="label-mono text-muted">Experience</span>
            <Tag tone="outline">Progressive disclosure</Tag>
          </div>
          <Row
            label="Experience level"
            hint="Beginner keeps a short, guided path (Today · Research · Calendar · Journal · Settings) with a simpler company Snapshot and a blocking pre-trade checklist. Standard adds Watchlist and Library, opens the full Research file — Evidence, Perspectives, Scenarios, History, Decision log — and makes the checklist advisory. Switch any time; it changes only what you see, never any trading action."
          >
            <Segmented
              ariaLabel="Experience level"
              items={[
                { v: "beginner", label: "Beginner" },
                { v: "standard", label: "Standard" },
              ]}
              value={settings.experienceLevel === "standard" ? "standard" : "beginner"}
              disabled={saving}
              onChange={(v) => change({ experienceLevel: v })}
            />
          </Row>

          <Row
            label="Reality check"
            hint="The one-time expectations reset from your first run — the honest odds, sourced from this project's research notes. Re-reading it won't change your experience level."
          >
            <button
              type="button"
              onClick={() => onReviewRealityCheck?.()}
              className="rounded-full border border-line2 px-4 py-2 text-[12.5px] font-semibold text-fg/90 transition-colors hover:border-accent/60"
            >
              Review the reality check
            </button>
          </Row>

          <Row
            label="Champion / challenger"
            hint="Training creates an immutable challenger. Only a current pooled EDGE verdict and every safety gate can make the Promote button available."
          >
            <div className="min-w-[270px] space-y-2 text-right text-[11.5px] text-muted">
              <div>
                Active H{candidate?.horizon ?? settings.defaultHorizon}: <span className="num text-fg/90">{activeModel?.modelVersion || "none"}</span>
              </div>
              <div>
                Challenger: <span className="num text-fg/90">{candidate ? `H${candidate.horizon} · ${candidate.modelVersion}` : "none"}</span>
              </div>
              {candidate && (
                <div className={candidate.promotionEligible ? "text-accent" : "max-w-[340px] text-warn"}>
                  {candidate.promotionEligible ? "Every promotion gate passes." : firstFailedGate?.detail || "Not eligible for promotion."}
                </div>
              )}
              <div className="flex justify-end gap-2">
                <button
                  type="button"
                  onClick={() => refreshModelRegistry().catch((err) => toast({ tone: "error", title: "Could not refresh model gates", message: err.message }))}
                  className="num rounded-full border border-line2 px-3.5 py-1.5 text-[11.5px] font-semibold text-muted transition-colors hover:border-accent/60 hover:text-fg"
                >
                  Review gates
                </button>
                <button
                  type="button"
                  onClick={onPromote}
                  disabled={!candidate?.promotionEligible || deploying}
                  className="num rounded-full border border-line2 px-3.5 py-1.5 text-[11.5px] font-semibold text-fg/90 transition-colors hover:border-accent/60 disabled:opacity-40"
                >
                  {deploying === "promote" ? "Promoting…" : "Promote challenger"}
                </button>
                {rollbackTarget && (
                  <button
                    type="button"
                    onClick={onRollback}
                    disabled={deploying}
                    className="num rounded-full border border-warn/40 px-3.5 py-1.5 text-[11.5px] font-semibold text-warn transition-colors hover:border-warn disabled:opacity-40"
                  >
                    {deploying === "rollback" ? "Rolling back…" : "Roll back"}
                  </button>
                )}
              </div>
            </div>
          </Row>

          <Row
            label="Allow short"
            hint="Permit the validated model to emit a SHORT bias, not long-only. It changes the suggestion only — the app still executes nothing."
          >
            <label className="flex cursor-pointer items-center gap-2.5 text-[13px] text-fg/90">
              <input
                type="checkbox"
                checked={settings.allowShort}
                disabled={saving}
                onChange={(e) => change({ allowShort: e.target.checked })}
                className="h-4 w-4 accent-accent"
              />
              {settings.allowShort ? "Long & short" : "Long only"}
            </label>
          </Row>

          <Row
            label="Default horizon"
            hint="The forecast horizon (bars ahead) the signal uses by default across the app."
          >
            <Segmented
              ariaLabel="Default horizon"
              items={HORIZONS.map((h) => ({ v: h, label: `H${h}` }))}
              value={settings.defaultHorizon}
              disabled={saving}
              onChange={(v) => change({ defaultHorizon: v })}
            />
          </Row>

          <Row
            label="Browser notifications"
            hint={
              !notifSupported
                ? "Your browser doesn't support desktop notifications."
                : !secureCtx
                ? "Desktop notifications need a secure context — HTTPS or localhost."
                : "Pop a desktop notification when a new alert fires. Descriptive events only — never advice. Requires granting your browser's permission."
            }
          >
            <div className="flex flex-col items-start gap-1.5 sm:items-end">
              <label
                className={cx(
                  "flex items-center gap-2.5 text-[13px]",
                  canNotify ? "cursor-pointer text-fg/90" : "cursor-not-allowed text-muted/50"
                )}
              >
                <input
                  type="checkbox"
                  checked={settings.browserNotifications}
                  disabled={saving || !canNotify}
                  onChange={onToggleNotifications}
                  className="h-4 w-4 accent-accent"
                />
                {settings.browserNotifications ? "On" : "Off"}
              </label>
              {canNotify && (
                <span className="label-mono text-muted">
                  permission: {permState}
                </span>
              )}
              {canNotify && permState === "denied" && (
                <span className="max-w-[230px] text-[11px] leading-snug text-warn/90 sm:text-right">
                  Notifications are blocked in your browser — enable them in site settings first.
                </span>
              )}
            </div>
          </Row>

          {/* Account & risk — feeds the client-side position sizer (docs/BEGINNER.md Piece 2) */}
          <div className="flex h-9 items-center gap-2.5 border-b border-t border-line bg-panel2/30 px-[22px]">
            <span className="label-mono text-muted">Account &amp; risk</span>
            <Tag tone="outline">Position sizer</Tag>
          </div>

          <Row
            label="Account equity"
            hint="Used by the position sizer to cap risk per trade at your chosen %. 0 leaves it unset — the sizer will prompt for it. Never used to place an order."
          >
            <div className="relative w-[170px]">
              <span className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-[13px] text-muted/60">$</span>
              <input
                type="number"
                step="any"
                min="0"
                inputMode="decimal"
                value={equity}
                disabled={saving}
                onChange={(e) => setEquity(e.target.value)}
                onBlur={saveEquity}
                placeholder="25000"
                className="num h-[38px] w-full rounded-lg border border-line2 bg-panel2 pl-6 pr-3 text-[13px] text-fg focus:border-accent/60 focus:outline-none disabled:opacity-50"
                aria-label="Account equity"
              />
            </div>
          </Row>

          <Row
            label="Default risk per trade"
            hint="The 1–2% convention is what keeps a losing streak survivable. 2% is the hard cap the sizer warns above."
          >
            <Segmented
              ariaLabel="Default risk percent"
              items={[
                { v: 0.5, label: "0.5%" },
                { v: 1, label: "1%" },
                { v: 2, label: "2%" },
              ]}
              value={settings.defaultRiskPct}
              disabled={saving}
              onChange={(v) => change({ defaultRiskPct: v })}
            />
          </Row>

          {/* Status + manual override */}
          <div className="flex flex-col gap-3 px-[22px] py-5 sm:flex-row sm:items-center sm:justify-between">
            <div className="text-[12.5px] text-muted">
              <span className="label-mono text-muted">Candidate training</span>{" · "}
              {autoTraining ? (
                <span className="text-accent">training now…</span>
              ) : (
                <>active changed <span className="text-fg/90">{relTime(lastTrainedAt)}</span></>
              )}
            </div>
            <button
              type="button"
              onClick={onTrainNow}
              disabled={autoTraining}
              className="num shrink-0 rounded-full border border-line2 px-4 py-2 text-[12.5px] font-semibold text-fg/90 transition-colors hover:border-accent/60 disabled:opacity-50"
            >
              {autoTraining ? "Training…" : "Train challenger"}
            </button>
          </div>

          {/* Notes */}
          <div className="border-t border-line bg-panel2/40 px-[22px] py-4">
            <p className="mb-2 text-[11.5px] leading-relaxed text-muted">
              Training runs a heavy walk-forward backtest and creates a candidate only. It never replaces
              the active model, and no page timer retrains it repeatedly.
            </p>
            <p className="text-[11.5px] leading-relaxed text-muted">
              <strong className="text-llm">Suggestion only — you place every trade; this app never executes orders.</strong>{" "}
              The signal is still shown only with its walk-forward track record.
            </p>
          </div>
        </>
      )}
    </section>
  );
}
