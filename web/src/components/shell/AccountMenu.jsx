import { useEffect, useRef, useState } from "react";
import { cx } from "../../lib/cx.js";
import { useAuth } from "../../auth/AuthContext.jsx";

// AccountMenu — the TopBar's right-side account control. Logged out: a "Sign in" button that routes
// to #signin (remembering the current view). Logged in: an avatar button that opens a small menu with
// the email + "Sign out". Signing in/out only scopes data — it unlocks no trading action.

export function AccountMenu() {
  const { user, loading, logout, promptSignIn } = useAuth();
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const rootRef = useRef(null);

  useEffect(() => {
    if (!open) return;
    const onDoc = (e) => {
      if (rootRef.current && !rootRef.current.contains(e.target)) setOpen(false);
    };
    const onKey = (e) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onDoc);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDoc);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  // While hydrating /auth/me, render a neutral placeholder so the control doesn't flicker.
  if (loading) {
    return <span className="h-9 w-9 rounded-full border border-line bg-panel2" aria-hidden="true" />;
  }

  if (!user) {
    return (
      <button
        onClick={promptSignIn}
        className="pill-lift flex h-9 items-center gap-1.5 rounded-full border border-accent/50 bg-accent/10 px-4 text-[12.5px] font-semibold text-accent transition-[transform,background-color,border-color] duration-200 ease-premium hover:-translate-y-px hover:bg-accent/16"
      >
        Sign in
      </button>
    );
  }

  const initial = (user.email || "?").trim().charAt(0).toUpperCase();

  const onSignOut = async () => {
    setBusy(true);
    try {
      await logout();
    } finally {
      setBusy(false);
      setOpen(false);
    }
  };

  return (
    <div ref={rootRef} className="relative">
      <button
        onClick={() => setOpen((v) => !v)}
        aria-haspopup="menu"
        aria-expanded={open}
        title={user.email}
        className={cx(
          "flex h-9 w-9 items-center justify-center rounded-full border text-[13px] font-bold uppercase transition-colors",
          open ? "border-accent/60 bg-accent/16 text-accent" : "border-line2 bg-panel2 text-fg hover:border-accent/60"
        )}
      >
        {initial}
      </button>

      {open && (
        <div
          role="menu"
          className="surface-deep absolute right-0 z-50 mt-2 w-[240px] overflow-hidden !rounded-xl"
        >
          <div className="border-b border-line px-3.5 py-3">
            <div className="label-mono text-muted">Signed in as</div>
            <div className="mt-1 truncate text-[13px] font-medium text-fg" title={user.email}>
              {user.email}
            </div>
          </div>
          <button
            role="menuitem"
            onClick={() => {
              window.location.hash = "settings/preferences";
              setOpen(false);
            }}
            className="flex w-full items-center gap-2 border-b border-line px-3.5 py-2.5 text-left text-[13px] text-fg/85 transition-colors hover:bg-white/10 hover:text-fg"
          >
            <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
              <circle cx="12" cy="12" r="3" />
              <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06A1.65 1.65 0 0 0 4.6 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06A1.65 1.65 0 0 0 9 4.6a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06A1.65 1.65 0 0 0 19.4 9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z" />
            </svg>
            Settings
          </button>
          {/* Help & feedback — the former top-level Feedback destination, now reached from the account
              surface (and from Settings). */}
          <button
            role="menuitem"
            onClick={() => {
              window.location.hash = "settings/help";
              setOpen(false);
            }}
            className="flex w-full items-center gap-2 border-b border-line px-3.5 py-2.5 text-left text-[13px] text-fg/85 transition-colors hover:bg-white/10 hover:text-fg"
          >
            <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
              <path d="M4 4h16v12H9l-5 4z" />
              <path d="M8 9h8M8 12h5" />
            </svg>
            Help &amp; feedback
          </button>
          <button
            role="menuitem"
            onClick={onSignOut}
            disabled={busy}
            className="flex w-full items-center gap-2 px-3.5 py-2.5 text-left text-[13px] text-fg/85 transition-colors hover:bg-white/10 hover:text-fg disabled:opacity-50"
          >
            <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
              <path d="M9 21H5a2 2 0 01-2-2V5a2 2 0 012-2h4M16 17l5-5-5-5M21 12H9" />
            </svg>
            {busy ? "Signing out…" : "Sign out"}
          </button>
        </div>
      )}
    </div>
  );
}
