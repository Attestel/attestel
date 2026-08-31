import { createContext, useCallback, useContext, useEffect, useState } from "react";
import { authLogin, authLogout, authMe, authSignup } from "../lib/api.js";
import { DEFAULT_VIEW, isAuthRoute } from "../lib/routes.js";

// AuthContext — the app's single source of truth for "who is signed in".
//
// On mount it hydrates from GET /auth/me (a guest is a valid state — {user:null}). The rest of the
// app runs in GUEST mode by default; only WRITES are gated. requireAuth(action) runs the action when
// signed in, otherwise remembers the current view and routes the user to the sign-in screen (so a
// guest who clicks "Log trade" lands on sign-in instead of a dead 401).

const AuthCtx = createContext(null);

export function useAuth() {
  return useContext(AuthCtx);
}

const RETURN_KEY = "auth.returnTo";

// rememberReturn stashes the current route (`view` or `view/subview`) so we can return to it after
// sign-in/up. A legacy id stashed before the seven-destination migration is still accepted on the way
// back: parseHash resolves it to a live destination (or the default), so a session in flight is never
// stranded (docs/research-os/MIGRATION_CONTRACT.md §2.1 rule 5).
function rememberReturn() {
  try {
    const cur = window.location.hash.replace("#", "") || DEFAULT_VIEW;
    if (!isAuthRoute(cur)) sessionStorage.setItem(RETURN_KEY, cur);
  } catch {
    /* ignore */
  }
}

// consumeReturnTo reads + clears the stashed return route (default: the default destination). Used by
// the sign-in/up screens.
export function consumeReturnTo() {
  try {
    const v = sessionStorage.getItem(RETURN_KEY);
    sessionStorage.removeItem(RETURN_KEY);
    return v || DEFAULT_VIEW;
  } catch {
    return DEFAULT_VIEW;
  }
}

export function AuthProvider({ children }) {
  const [user, setUser] = useState(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let alive = true;
    authMe()
      .then((d) => {
        if (alive) setUser(d?.user ?? null);
      })
      .finally(() => {
        if (alive) setLoading(false);
      });
    return () => {
      alive = false;
    };
  }, []);

  const login = useCallback(async (creds) => {
    const u = await authLogin(creds);
    setUser(u);
    return u;
  }, []);

  const signup = useCallback(async (creds) => {
    const u = await authSignup(creds);
    setUser(u);
    return u;
  }, []);

  const logout = useCallback(async () => {
    try {
      await authLogout();
    } catch {
      /* best-effort — clear local state regardless */
    }
    setUser(null);
    window.location.hash = "signin"; // after signing out, land on the sign-in screen
  }, []);

  // promptSignIn: remember where we are, then route to the sign-in screen.
  const promptSignIn = useCallback(() => {
    rememberReturn();
    window.location.hash = "signin";
  }, []);

  // requireAuth(action): run action when signed in; otherwise route to sign-in and return false.
  const requireAuth = useCallback(
    (action) => {
      if (user) {
        action?.();
        return true;
      }
      promptSignIn();
      return false;
    },
    [user, promptSignIn]
  );

  return (
    <AuthCtx.Provider value={{ user, loading, login, signup, logout, requireAuth, promptSignIn }}>
      {children}
    </AuthCtx.Provider>
  );
}
