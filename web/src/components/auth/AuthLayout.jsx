import { motion, useReducedMotion } from "framer-motion";
import { IconChip, Label, AttestelWordmark } from "../ui/index.js";
import { Icon } from "../shell/icons.jsx";

// AuthLayout — the split-panel sign-in / sign-up shell. LEFT is the brand panel (wordmark, eyebrow,
// headline, proof points, disclaimer footer); RIGHT is the form (`children`). Full-screen, no
// TopBar/LeftRail. On small screens the brand panel is hidden and the form takes the full width.
//
// This is the screen immediately after the landing page's "Request Access", so it is deliberately
// the landing's twin: the brand panel is the landing `.hero-canvas` (inset canvas, drifting glow,
// star field, 28px radius inside a 12px margin) and the form uses the landing `.field` recipe with
// a white 52px pill submit.
//
// There is NO second mark in this brand — the wordmark stands alone, exactly as in the landing nav.

// Shared form recipes, exported so SignIn and SignUp cannot drift apart.
export const AUTH_LABEL = "label-mono block";
export const AUTH_FIELD =
  "flex h-[52px] items-center gap-2 rounded-[14px] border border-line2 bg-[rgba(20,22,27,.65)] px-[18px] text-[15.5px] text-fg backdrop-blur-[10px] transition-colors focus-within:border-accent/70";
export const AUTH_INPUT =
  "min-w-0 flex-1 bg-transparent text-fg placeholder:text-dim focus:outline-none";
export const AUTH_SUBMIT =
  "pill-lift mt-5 flex min-h-[52px] w-full items-center justify-center rounded-full bg-fg px-[26px] text-[14.5px] font-semibold text-bg transition-[transform,background-color,box-shadow] duration-200 ease-premium hover:-translate-y-0.5 hover:bg-white hover:shadow-glow disabled:pointer-events-none disabled:opacity-50";

// PROOF_POINTS — the approved proof set (PARALLEL_CONTRACTS.md §6.5, guide §6.3): connected thesis
// context, multiple research perspectives, permanent decision history. Deliberately NOT a feature
// inventory: the promise is decision quality, not a list of panels.
//
// `kind` picks the provenance tint of the mark beside each point — `user` for what stays yours,
// `filing` for what is sourced.
const PROOF_POINTS = [
  {
    title: "Everything connected to the thesis",
    sub: "Evidence, challenges and decisions live on the claim — not scattered across tabs.",
    kind: "filing",
  },
  {
    title: "Multiple research perspectives",
    sub: "Lenses that challenge, explain and compare the evidence you attached.",
    kind: "user",
  },
  {
    title: "A permanent decision record",
    sub: "What you believed, what changed, and what you learned from it.",
    kind: "user",
  },
];

function BrandPanel({ mode }) {
  const isSignup = mode === "signup";
  return (
    <aside className="hero-canvas m-3 hidden w-[440px] flex-none flex-col px-9 py-9 lg:flex">
      <div className="hero-glow" aria-hidden="true" />
      <div className="hero-stars" aria-hidden="true" />

      <div className="relative z-[1]">
        <AttestelWordmark height={20} className="text-fg" />
      </div>

      <div className="relative z-[1] flex flex-1 flex-col justify-center">
        <Label className="mb-3.5 flex items-center gap-2.5">
          <span
            className="h-1.5 w-1.5 flex-none rounded-full bg-accent shadow-[0_0_10px_var(--color-accent)]"
            aria-hidden="true"
          />
          {isSignup ? "Start your first research review" : "Pick up where you left off"}
        </Label>
        {/* Approved hero + supporting line, verbatim (§6.5). */}
        <h2 className="text-[30px] font-[550] leading-[1.06] tracking-[-0.03em] text-fg">
          The operating system for investment research.
        </h2>
        <p className="mt-3.5 text-[14px] leading-[1.6] text-muted">
          Build your thesis. Challenge your assumptions. Track what changes. Learn from every decision.
        </p>

        <div className="mt-7 flex flex-col gap-4">
          {PROOF_POINTS.map((f) => (
            <div key={f.title} className="flex items-start gap-3.5">
              <IconChip kind={f.kind} size="sm">
                <Icon name="check" size={14} />
              </IconChip>
              <div className="min-w-0">
                <div className="text-[13.5px] font-medium text-fg">{f.title}</div>
                <div className="mt-1 text-[12px] leading-[1.55] text-muted">{f.sub}</div>
              </div>
            </div>
          ))}
        </div>

        {/* Retained disclosure. It no longer leads, but it is not removed or softened: what the
            product does not do stays stated next to what it does. */}
        <p className="mt-7 text-[12px] leading-[1.6] text-muted">Analysis only; you place every trade.</p>
      </div>

      <div className="label-mono relative z-[1] leading-[1.5] tracking-[.1em]">
        Analysis tool — no buy/sell verdicts, no order execution.
      </div>
    </aside>
  );
}

export function AuthLayout({ mode, children }) {
  const reduce = useReducedMotion();
  return (
    <div className="flex min-h-screen bg-bg text-fg">
      <BrandPanel mode={mode} />
      <main className="flex flex-1 flex-col justify-center px-6 py-10 sm:px-14">
        <motion.div
          key={mode}
          initial={reduce ? false : { opacity: 0, y: 8 }}
          animate={{ opacity: 1, y: 0 }}
          transition={{ duration: 0.2, ease: [0.16, 0.8, 0.24, 1] }}
          className="mx-auto w-full max-w-[400px]"
        >
          {/* Compact brand row for small screens where the brand panel is hidden. */}
          <div className="mb-8 lg:hidden">
            <AttestelWordmark height={20} className="text-fg" />
          </div>
          {children}
        </motion.div>
      </main>
    </div>
  );
}

// Shared bits reused by SignIn / SignUp -----------------------------------------------------------

// GoogleButton — deferred OAuth. Calls the 501 stub; parent shows a "coming soon" toast.
// The landing's glass pill; the Google mark keeps its brand colours by design.
export function GoogleButton({ label, onClick }) {
  return (
    <button
      type="button"
      onClick={onClick}
      className="pill-lift flex min-h-[52px] w-full items-center justify-center gap-2.5 rounded-full border border-line2 bg-[rgba(24,26,32,.65)] px-[26px] text-[14.5px] font-semibold text-fg backdrop-blur-[12px] transition-[transform,border-color,box-shadow] duration-200 ease-premium hover:-translate-y-0.5 hover:border-white/34 hover:shadow-lift"
    >
      <svg width="17" height="17" viewBox="0 0 24 24" aria-hidden="true">
        <path fill="#4285F4" d="M22.56 12.25c0-.78-.07-1.53-.2-2.25H12v4.26h5.92a5.06 5.06 0 01-2.2 3.32v2.77h3.57c2.08-1.92 3.27-4.74 3.27-8.1z" />
        <path fill="#34A853" d="M12 23c2.97 0 5.46-.98 7.28-2.66l-3.57-2.77c-.98.66-2.23 1.06-3.71 1.06-2.86 0-5.29-1.93-6.16-4.53H2.18v2.84A11 11 0 0012 23z" />
        <path fill="#FBBC05" d="M5.84 14.1a6.6 6.6 0 010-4.2V7.06H2.18a11 11 0 000 9.88l3.66-2.84z" />
        <path fill="#EA4335" d="M12 5.38c1.62 0 3.06.56 4.21 1.64l3.15-3.15C17.45 2.09 14.97 1 12 1a11 11 0 00-9.82 6.06l3.66 2.84C6.71 7.3 9.14 5.38 12 5.38z" />
      </svg>
      {label}
    </button>
  );
}

// OrDivider — the "OR WITH EMAIL" rule.
export function OrDivider() {
  return (
    <div className="my-5 flex items-center gap-3.5">
      <span className="h-px flex-1 bg-line" />
      <Label className="flex-none">Or with email</Label>
      <span className="h-px flex-1 bg-line" />
    </div>
  );
}

// EyeToggle — show/hide password button.
export function EyeToggle({ shown, onClick }) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-label={shown ? "Hide password" : "Show password"}
      aria-pressed={shown}
      className="flex items-center rounded-full p-1 text-muted transition-colors hover:text-fg"
    >
      {shown ? (
        <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" aria-hidden="true">
          <path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7S2 12 2 12z" />
          <path d="M4 4l16 16" strokeLinecap="round" />
        </svg>
      ) : (
        <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" aria-hidden="true">
          <path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7S2 12 2 12z" />
          <circle cx="12" cy="12" r="2.5" />
        </svg>
      )}
    </button>
  );
}
