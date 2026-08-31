import { useMemo, useState } from "react";
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
import { cx } from "../../lib/cx.js";

// The landing's `.field` recipe, shared with the other auth screen via AuthLayout.
const LABEL = AUTH_LABEL;
const FIELD = AUTH_FIELD;
const INPUT = AUTH_INPUT;

// scorePassword rates length/case/digit/symbol -> {score 0..4, label, tone}. Purely advisory; the
// backend enforces the real minimum (>= 8 chars).
function scorePassword(pw) {
  if (!pw) return { score: 0, label: "", tone: "muted" };
  let score = 0;
  if (pw.length >= 8) score++;
  if (/[a-z]/.test(pw) && /[A-Z]/.test(pw)) score++;
  if (/\d/.test(pw)) score++;
  if (/[^A-Za-z0-9]/.test(pw)) score++;
  const meta = [
    { label: "Weak", tone: "down" },
    { label: "Weak", tone: "down" },
    { label: "Fair", tone: "warn" },
    { label: "Good", tone: "llm" },
    { label: "Strong", tone: "accent" },
  ][score];
  return { score, ...meta };
}

const TONE_TEXT = { down: "text-down", warn: "text-warn", llm: "text-llm", accent: "text-accent", muted: "text-muted" };
const TONE_BAR = { down: "bg-down", warn: "bg-warn", llm: "bg-llm", accent: "bg-accent", muted: "bg-line" };

function StrengthMeter({ pw }) {
  const { score, label, tone } = scorePassword(pw);
  if (!pw) return null;
  return (
    <div className="mt-2.5 flex items-center gap-2.5">
      <div className="flex flex-1 gap-1">
        {[0, 1, 2, 3].map((i) => (
          <span key={i} className={cx("h-1 flex-1 rounded-full", i < score ? TONE_BAR[tone] : "bg-line")} />
        ))}
      </div>
      <span className={cx("font-mono text-[10.5px]", TONE_TEXT[tone])}>{label}</span>
    </div>
  );
}

export default function SignUp() {
  const { signup } = useAuth();
  const toast = useToast();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [showPw, setShowPw] = useState(false);
  const [agree, setAgree] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const canSubmit = useMemo(
    () => agree && email.trim() !== "" && password.length >= 8 && !busy,
    [agree, email, password, busy]
  );

  const submit = async (e) => {
    e.preventDefault();
    if (!canSubmit) {
      if (password.length < 8) setError("Password must be at least 8 characters.");
      else if (!agree) setError("Please acknowledge this is an analysis tool, not investment advice.");
      return;
    }
    setError("");
    setBusy(true);
    try {
      await signup({ email, password });
      window.location.hash = consumeReturnTo();
    } catch (err) {
      setError(err.message || "Could not create your account.");
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
    <AuthLayout mode="signup">
      <form onSubmit={submit} noValidate>
        <h1 className="text-[26px] font-[550] leading-[1.06] tracking-[-0.03em] text-fg">Create your account</h1>
        <p className="mt-1.5 text-[13.5px] text-muted">
          Already have one?{" "}
          <a href="#signin" className="font-medium text-accent hover:brightness-110">
            Sign in
          </a>
        </p>

        <div className="mt-6">
          <GoogleButton label="Sign up with Google" onClick={onGoogle} />
        </div>

        <OrDivider />

        <div className="mb-3.5">
          <label htmlFor="signup-email" className={`mb-1.5 block ${LABEL}`}>
            EMAIL
          </label>
          <div className={FIELD}>
            <input
              id="signup-email"
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
          <label htmlFor="signup-password" className={`mb-1.5 block ${LABEL}`}>
            PASSWORD
          </label>
          <div className={FIELD}>
            <input
              id="signup-password"
              type={showPw ? "text" : "password"}
              autoComplete="new-password"
              required
              minLength={8}
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="At least 8 characters"
              className={INPUT}
              aria-describedby="pw-strength"
            />
            <EyeToggle shown={showPw} onClick={() => setShowPw((v) => !v)} />
          </div>
          <div id="pw-strength">
            <StrengthMeter pw={password} />
          </div>
        </div>

        <label className="mt-3.5 flex cursor-pointer items-start gap-2.5 text-[12.5px] leading-[1.5] text-fg/85">
          <input
            type="checkbox"
            checked={agree}
            onChange={(e) => setAgree(e.target.checked)}
            className="mt-0.5 h-[17px] w-[17px] flex-none accent-[var(--color-accent)]"
          />
          <span>
            I understand this is an analysis tool and <strong className="text-llm">not investment advice</strong>.
          </span>
        </label>

        {error && (
          <p role="alert" className="mt-3.5 rounded-lg border border-down/40 bg-down/10 px-3 py-2 text-[12.5px] text-down">
            {error}
          </p>
        )}

        <button
          type="submit"
          disabled={!canSubmit}
          className={AUTH_SUBMIT}
        >
          {busy ? "Creating account…" : "Create account"}
        </button>

        <p className="mt-[18px] text-center text-[11px] leading-[1.6] text-muted/70">
          Informational tool — not investment advice.
        </p>
      </form>
    </AuthLayout>
  );
}
