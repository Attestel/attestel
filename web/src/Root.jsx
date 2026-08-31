import { useEffect, useState } from "react";
import App from "./App.jsx";
import SignIn from "./components/auth/SignIn.jsx";
import SignUp from "./components/auth/SignUp.jsx";
import { SettingsProvider } from "./settings/SettingsContext.jsx";
import { OnboardingProvider } from "./components/onboarding/OnboardingContext.jsx";

// Root — top-level router. #signin / #signup render the auth screens FULL-SCREEN (no TopBar/LeftRail);
// every other hash renders the normal app in guest mode by default. Keeping the auth screens outside
// <App/> means App's own hash-view logic never has to know about them.

const authRouteFromHash = () => {
  const h = typeof window !== "undefined" ? window.location.hash.replace("#", "") : "";
  return h === "signin" || h === "signup" ? h : null;
};

export default function Root() {
  const [route, setRoute] = useState(authRouteFromHash);
  useEffect(() => {
    const onHash = () => setRoute(authRouteFromHash());
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);

  if (route === "signin") return <SignIn />;
  if (route === "signup") return <SignUp />;
  // SettingsProvider wraps the app (inside AuthProvider from main.jsx) so the per-user directional-
  // signal prefs + the client-side auto-train loop have one source of truth. OnboardingProvider sits
  // inside it because the guided first review reports the user's experience level with its progress,
  // and outside App so the guidance state survives every navigation within the shell.
  return (
    <SettingsProvider>
      <OnboardingProvider>
        <App />
      </OnboardingProvider>
    </SettingsProvider>
  );
}
