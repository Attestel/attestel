import { useState } from "react";
import {
  AUTH_FIELD,
  AUTH_INPUT,
  AUTH_LABEL,
  AUTH_SUBMIT,
  AuthLayout,
  EyeToggle,
  GoogleButton,
  OrDivider,
} from "./AuthLayout.jsx";
import { useAuth, consumeReturnTo } from "../../auth/AuthContext.jsx";
import { useToast } from "../ui/Toast.jsx";
import { authGoogle, googleStartURL } from "../../lib/api.js";
import { baseViewOf } from "../../lib/routes.js";

// The landing's `.field` recipe, shared with the other auth screen via AuthLayout.
const LABEL = AUTH_LABEL;
const FIELD = AUTH_FIELD;
const INPUT = AUTH_INPUT;

export default function SignIn() {
  const { login } = useAuth();
  const toast = useToast();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [remember, setRemember] = useState(true);
  const [showPw, setShowPw] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const submit = async (e) => {
    e.preventDefault();
    if (busy) return;
    setError("");
    setBusy(true);
    try {
      await login({ email, password, remember });
      window.location.hash = consumeReturnTo();
    } catch (err) {
      setError(err.message || "Could not sign in.");
    } finally {
      setBusy(false);
    }
  };

  const onGoogle = async () => {
    const { configured } = await authGoogle();
    if (!configured) {
      toast({ title: "Google sign-in coming soon", message: "Use email & password for now.", tone: "info" });
      return;
    }
    // OAuth needs a top-level navigation (not fetch). Carry the intended return view; the auth service
    // round-trips it back as the post-login hash. Only the DESTINATION segment can travel: the auth
    // service's sanitizeReturnTo rejects anything containing a slash, so a `view/subview` route would be
    // silently discarded — baseViewOf sends `view` and the user lands on its default subview.
    window.location.href = googleStartURL(baseViewOf(consumeReturnTo()));
  };

  return (
    <AuthLayout mode="signin">
      <form onSubmit={submit} noValidate>
        <h1 className="text-[26px] font-[550] leading-[1.06] tracking-[-0.03em] text-fg">Sign in</h1>
        <p className="mt-1.5 text-[13.5px] text-muted">
          New here?{" "}
          <a href="#signup" className="font-medium text-accent hover:brightness-110">
            Create an account
          </a>
        </p>

        <div className="mt-6">
          <GoogleButton label="Continue with Google" onClick={onGoogle} />
        </div>

        <OrDivider />

        <div className="mb-3.5">
          <label htmlFor="signin-email" className={`mb-1.5 block ${LABEL}`}>
            EMAIL
          </label>
          <div className={FIELD}>
            <input
              id="signin-email"
              type="email"
              autoComplete="email"
              required
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              placeholder="you@desk.com"
              className={INPUT}
            />
          </div>
        </div>

        <div>
          <div className="mb-1.5 flex items-center justify-between">
            <label htmlFor="signin-password" className={LABEL}>
              PASSWORD
            </label>
            <button
              type="button"
              onClick={() =>
                toast({ title: "Password reset coming soon", message: "Contact the owner to reset for now.", tone: "info" })
              }
              className="text-[12px] text-accent hover:brightness-110"
            >
              Forgot?
            </button>
          </div>
          <div className={FIELD}>
            <input
              id="signin-password"
              type={showPw ? "text" : "password"}
              autoComplete="current-password"
              required
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="••••••••"
              className={INPUT}
            />
            <EyeToggle shown={showPw} onClick={() => setShowPw((v) => !v)} />
          </div>
        </div>

        <label className="mt-4 flex cursor-pointer items-center gap-2.5 text-[13px] text-fg/85">
          <input
            type="checkbox"
            checked={remember}
            onChange={(e) => setRemember(e.target.checked)}
            className="h-[17px] w-[17px] accent-[var(--color-accent)]"
          />
          Keep me signed in
        </label>

        {error && (
          <p role="alert" className="mt-3.5 rounded-lg border border-down/40 bg-down/10 px-3 py-2 text-[12.5px] text-down">
            {error}
          </p>
        )}

        <button
          type="submit"
          disabled={busy}
          className={AUTH_SUBMIT}
        >
          {busy ? "Signing in…" : "Sign in"}
        </button>

        <p className="mt-[18px] text-center text-[11px] leading-[1.6] text-muted/70">
          By continuing you agree to the Terms &amp; Privacy Policy. Informational tool — not investment advice.
        </p>
      </form>
    </AuthLayout>
  );
}
