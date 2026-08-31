import { createContext, useCallback, useContext, useEffect, useRef, useState } from "react";
import { useAuth } from "../../auth/AuthContext.jsx";
import { useSettings } from "../../settings/SettingsContext.jsx";
import {
  STEP_IDS,
  clearGuestProgress,
  getOnboarding,
  guestSessionId,
  loadGuestProgress,
  saveGuestProgress,
  saveOnboarding,
  stepIndex,
} from "../../lib/onboardingApi.js";

// OnboardingContext — the state behind the guided first research review.
//
// It holds two very different things behind one interface:
//
//   - SIGNED IN: the server's record. Progress is a durable write, and completion is the server's own
//     verdict after re-reading the user's thesis, evidence, and challenge review. The client never
//     decides that someone is activated.
//   - GUEST: localStorage. A visitor with no account has no research records to verify, so there is
//     nothing to complete — the guide simply walks them to the point where an account is needed. The
//     server is told only that a session started or resumed (the two guest-visible events).
//
// On sign-in, guest progress is used ONCE to seed the account's first server write and then dropped.
// Guest *activity* is never attributed to the account: the two identities hash differently by
// construction, so the funnel shows an anonymous start and an account start, which is the truth.

const OnboardingCtx = createContext(null);

export function useOnboarding() {
  return useContext(OnboardingCtx);
}

const emptyProgress = {
  state: { status: "not_started", step: STEP_IDS[0], completedSteps: [], ticker: "", thesisId: "", dismissed: false },
  hasThesis: false,
  hasEvidenceLink: false,
  hasChallenge: false,
  guest: true,
};

export function OnboardingProvider({ children }) {
  const { user, loading: authLoading, promptSignIn } = useAuth();
  const settings = useSettings();
  const level = settings?.settings?.experienceLevel === "standard" ? "standard" : "beginner";

  const [progress, setProgress] = useState(emptyProgress);
  const [ready, setReady] = useState(false);
  // seeded guards the one-time guest -> account handoff, so a re-render never replays it.
  const seeded = useRef(false);
  const inFlight = useRef(false);

  // Hydrate. A guest reads localStorage; a signed-in user reads the server, which is also where an
  // activation completed elsewhere in the product (Research, Perspectives) is noticed.
  useEffect(() => {
    if (authLoading) return;
    let alive = true;

    if (!user) {
      const local = loadGuestProgress();
      setProgress(local ? { ...emptyProgress, state: { ...emptyProgress.state, ...local } } : emptyProgress);
      setReady(true);
      return () => {
        alive = false;
      };
    }

    getOnboarding()
      .then((p) => {
        if (!alive || !p) return;
        setProgress(p);
        // One-time handoff: a visitor who got partway through before signing up resumes where they
        // were instead of starting over.
        const local = loadGuestProgress();
        if (!seeded.current && local && p.state?.status === "not_started") {
          seeded.current = true;
          saveOnboarding({ step: local.step, ticker: local.ticker, level })
            .then((next) => alive && next && setProgress(next))
            .catch(() => {})
            .finally(() => clearGuestProgress());
        } else if (local) {
          clearGuestProgress();
        }
      })
      .catch(() => {
        /* journal unreachable — the guide hides rather than inventing progress */
      })
      .finally(() => alive && setReady(true));

    return () => {
      alive = false;
    };
  }, [user, authLoading, level]);

  // report advances the guide. For a guest it is a local write plus the one server event a guest may
  // produce; for a signed-in user it is a durable write and the server's answer replaces local state.
  const report = useCallback(
    async (patch = {}) => {
      if (inFlight.current) return;
      inFlight.current = true;
      try {
        if (!user) {
          const prev = loadGuestProgress();
          const next = {
            ...emptyProgress.state,
            ...prev,
            ...patch,
            status: "in_progress",
            completedSteps: Array.from(new Set([...(prev?.completedSteps || []), patch.step].filter(Boolean))),
          };
          saveGuestProgress(next);
          setProgress((p) => ({ ...p, state: next, guest: true }));

          const sessionId = guestSessionId();
          if (sessionId) {
            await saveOnboarding({
              sessionId,
              step: patch.step,
              ticker: patch.ticker,
              resumed: Boolean(prev),
            }).catch(() => {});
          }
          return;
        }
        const next = await saveOnboarding({ ...patch, level });
        if (next) setProgress(next);
      } catch {
        /* never block research on the guide failing to record itself */
      } finally {
        inFlight.current = false;
      }
    },
    [user, level]
  );

  const dismiss = useCallback(async () => {
    if (!user) {
      const prev = loadGuestProgress() || {};
      const next = { ...emptyProgress.state, ...prev, dismissed: true };
      saveGuestProgress(next);
      setProgress((p) => ({ ...p, state: next }));
      return;
    }
    await report({ dismissed: true });
  }, [user, report]);

  const state = progress?.state || emptyProgress.state;
  const value = {
    ready,
    guest: !user,
    progress,
    state,
    level,
    stepIndex: stepIndex(state.step),
    completed: state.status === "completed",
    dismissed: Boolean(state.dismissed),
    // visible: the guidance shows until the user activates or turns it off. It is contextual help,
    // never a gate — every destination stays reachable while it is on screen.
    visible: ready && state.status !== "completed" && !state.dismissed,
    report,
    dismiss,
    promptSignIn,
  };

  return <OnboardingCtx.Provider value={value}>{children}</OnboardingCtx.Provider>;
}
