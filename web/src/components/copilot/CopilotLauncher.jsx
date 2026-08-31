import { Icon } from "../shell/icons.jsx";

// CopilotLauncher — a floating round button, bottom-right, that opens the Copilot drawer. Hidden
// while the drawer is open (it sits under the scrim anyway). Keyboard-reachable; title hints the
// shortcut.
//
// `hasContext` is Wave 4 Lane 4C's ONLY addition here, and it is deliberately the smallest one
// available: a 6px dot, no badge, no count, no colour change to the button itself, no animation.
// Doc §16.12 is explicit that AI must be available everywhere WITHOUT DOMINATING any page, and a
// launcher that grows a label or pulses to advertise itself is the failure that rule describes.
// The dot exists for one reason — so that a user who attached a context, navigated, and forgot can
// tell that the drawer will open with something attached rather than empty.
//
// It defaults to `false`, so the launcher renders exactly as it did before this lane until the
// integrator wires the prop (recorded in the Lane 4C handoff; `App.jsx` is reserved).
export function CopilotLauncher({ open, onClick, hasContext = false }) {
  if (open) return null;
  return (
    <button
      type="button"
      onClick={onClick}
      aria-label={hasContext ? "Open Copilot (context attached)" : "Open Copilot"}
      title="Copilot (press c)"
      className="pill-lift fixed bottom-5 right-5 z-40 flex h-[38px] w-[38px] items-center justify-center rounded-full border border-white/[.14] bg-[rgba(22,24,29,.7)] text-fg shadow-lift backdrop-blur-[10px] transition-[transform,border-color,box-shadow] duration-200 ease-premium hover:-translate-y-0.5 hover:border-white/34"
    >
      <Icon name="copilot" size={18} />
      {hasContext && (
        <span
          className="absolute right-[7px] top-[7px] h-1.5 w-1.5 rounded-full bg-accent"
          aria-hidden="true"
        />
      )}
    </button>
  );
}
