import { useEffect, useRef, useState } from "react";
import { useSettings } from "../../settings/SettingsContext.jsx";
import { cx } from "../../lib/cx.js";

// RealityCheck — a one-time expectations RESET shown right after sign-in, before the terminal is usable
// (docs/BEGINNER.md Piece 1). It leads with the platform's own honest numbers instead of burying them:
// a trust moat, not a disclaimer wall. Purely informational — no buy/sell, no execution, no money
// (invariant #2). Every figure below traces to docs/PREDICTION_MODELS.md ("Read this first — honest
// expectations"); nothing is invented (invariant #3). Static content + a settings write, so it renders
// fully offline.
//
// firstRun mode is the fork into the beginner path: "Start beginner journey" (Beginner) or "Skip"
// (Standard). Either choice persists ackRealityCheck so the modal never returns; the App gate then
// stops rendering it. review mode (re-opened from Settings) only closes — it never changes the level.

// The honest points — sourced from docs/PREDICTION_MODELS.md ("Read this first — honest expectations").
const POINTS = [
  "Most people who trade actively lose money. Any real edge is small and perishable — that's the base rate you're up against.",
  "Realistic, validated edges are right about 53–56% of the time, with a Sharpe around 1–2 — and they decay over time.",
  "Getting consistent is roughly a 2–3 year arc of practice and journaling. Anyone promising faster is selling something.",
  "This app suggests and simulates — you execute. It never places an order, integrates a broker, or moves money — ever.",
  "A Sharpe above 3 in a backtest almost always means a bug (leakage), not a jackpot — so we flag those as suspect, never celebrate them.",
];

export default function RealityCheck({ mode = "firstRun", onClose }) {
  const { saveSettings } = useSettings();
  const panelRef = useRef(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  // Focus the primary action on mount + trap Tab within the panel. Escape closes review only —
  // first-run has no dismiss-without-choosing (the two buttons are the only exits; no dark pattern,
  // but also "no other exits" until they pick a path).
  useEffect(() => {
    panelRef.current?.querySelector("[data-autofocus]")?.focus();
  }, []);

  function onKeyDown(e) {
    if (e.key === "Escape" && mode === "review") {
      onClose?.();
      return;
    }
    if (e.key !== "Tab") return;
    const nodes = panelRef.current?.querySelectorAll('button, a[href], [tabindex]:not([tabindex="-1"])');
    if (!nodes || nodes.length === 0) return;
    const first = nodes[0];
    const last = nodes[nodes.length - 1];
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  }

  // choose persists the acknowledgement + the chosen experience level. On failure it keeps the modal
  // open with an inline error — never strand the user acknowledged-but-not-saved. On success the App
  // gate (ackRealityCheck) flips and unmounts this overlay, so there's nothing to close manually.
  async function choose(level) {
    if (busy) return;
    setBusy(true);
    setError("");
    try {
      await saveSettings({ ackRealityCheck: true, experienceLevel: level });
    } catch (e) {
      setError(e?.message || "Couldn't save that — check your connection and try again.");
      setBusy(false);
    }
  }

  return (
    <div
      ref={panelRef}
      onKeyDown={onKeyDown}
      role="dialog"
      aria-modal="true"
      aria-labelledby="reality-check-title"
      className="fixed inset-0 z-50 overflow-y-auto bg-bg"
    >
      <div className="mx-auto flex min-h-full max-w-[680px] flex-col justify-center px-5 py-12">
        <div className="mb-1 label-mono text-muted">
          {mode === "review" ? "Reality check" : "Before you start"}
        </div>
        <h1 id="reality-check-title" className="text-[24px] font-bold leading-tight text-fg sm:text-[27px]">
          The honest odds
        </h1>
        <p className="mt-2.5 text-[13.5px] leading-relaxed text-muted">
          We lead with this because most platforms bury it — it hurts sign-ups. None of it is marketing. If it changes
          your mind about active trading, that's the system working.
        </p>

        <ul className="mt-6 flex flex-col gap-3.5">
          {POINTS.map((text) => (
            <li key={text} className="flex items-start gap-3 rounded-lg border border-line2 bg-panel2/40 px-4 py-3">
              <span aria-hidden="true" className="mt-[5px] h-1.5 w-1.5 shrink-0 rounded-full bg-accent" />
              <span className="text-[13.5px] leading-relaxed text-fg/90">{text}</span>
            </li>
          ))}
        </ul>

        <div className="mt-5 rounded-lg border border-accent/30 bg-accent/[0.07] px-4 py-3.5">
          <div className="mb-1 text-[12.5px] font-semibold text-accent">What it does do</div>
          <p className="text-[13px] leading-relaxed text-fg/85">
            Enforce <strong className="text-fg">process</strong> — a thesis you could be proven wrong about, evidence
            attached to the claim, a lens that argues against you, and a journal that shows you your own decisions and
            results — instead of promising profit. Discipline is the edge it can actually give you.
          </p>
        </div>

        {error && (
          <p role="alert" className="mt-4 rounded-lg border border-down/40 bg-down/10 px-3.5 py-2.5 text-[12.5px] text-down">
            {error}
          </p>
        )}

        {mode === "review" ? (
          <div className="mt-8">
            <button
              type="button"
              data-autofocus
              onClick={onClose}
              className="pill-lift inline-flex min-h-[52px] items-center rounded-full bg-fg px-[26px] text-[14.5px] font-semibold text-bg transition-[transform,background-color,box-shadow] duration-200 ease-premium hover:-translate-y-0.5 hover:bg-white hover:shadow-glow"
            >
              Close
            </button>
          </div>
        ) : (
          <div className="mt-8 flex flex-col items-start gap-3">
            <button
              type="button"
              data-autofocus
              onClick={() => choose("beginner")}
              disabled={busy}
              className="pill-lift inline-flex min-h-[52px] items-center rounded-full bg-fg px-[26px] text-[14.5px] font-semibold text-bg transition-[transform,background-color,box-shadow] duration-200 ease-premium hover:-translate-y-0.5 hover:bg-white hover:shadow-glow disabled:pointer-events-none disabled:opacity-60"
            >
              {busy ? "Starting…" : "Start a guided research review"}
            </button>
            <button
              type="button"
              onClick={() => choose("standard")}
              disabled={busy}
              className={cx(
                "text-[12.5px] text-muted underline decoration-dotted underline-offset-2 transition-colors hover:text-fg",
                busy && "opacity-60"
              )}
            >
              Skip — I'm experienced
            </button>
          </div>
        )}

        <p className="mt-8 border-t border-line/60 pt-3 text-[11px] leading-relaxed text-muted/70">
          Figures are from this project's own research notes (docs/PREDICTION_MODELS.md). This app never places an order,
          integrates a broker, or moves money.
        </p>
      </div>
    </div>
  );
}
