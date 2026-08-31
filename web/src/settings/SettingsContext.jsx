import { createContext, useCallback, useContext, useEffect, useRef, useState } from "react";
import { useAuth } from "../auth/AuthContext.jsx";
import {
  DEFAULT_SETTINGS,
  fetchModelRegistry,
  getSettings,
  promoteModel,
  rollbackModel,
  trainModel,
  updateSettings,
} from "../lib/api.js";

// SettingsContext — signed-in signal preferences plus the immutable model registry lifecycle. A
// training request creates a candidate; only an explicit promotion changes the active model. A guest / while-loading falls back to
// DEFAULT_SETTINGS (settings persist server-side so they follow the user across devices).
//
// The training and deployment actions share one in-flight guard. There is deliberately no timer:
// repeated retraining on the same daily bar produces candidate churn, not evidence.

const SettingsCtx = createContext(null);

export function useSettings() {
  return useContext(SettingsCtx);
}

export function SettingsProvider({ children }) {
  const { user, loading: authLoading } = useAuth();
  const [settings, setSettings] = useState(DEFAULT_SETTINGS);
  // hydrated tracks whether the CURRENT identity's settings have resolved — a guest is settled at once,
  // a signed-in user only after the fetch returns. The reality-check gate reads it so a returning,
  // already-acknowledged user never sees a flash of the first-run screen while settings hydrate.
  const [hydrated, setHydrated] = useState(false);
  const [lastTrainedAt, setLastTrainedAt] = useState(0);
  const [autoTraining, setAutoTraining] = useState(false);
  const [modelRegistry, setModelRegistry] = useState([]);
  const [target, setTargetState] = useState({ ticker: "NVDA", timeframe: "1D" });
  const trainingRef = useRef(false);
  const targetRef = useRef(target);

  // Hydrate from the server when a user is present; reset to defaults for a guest / on error.
  useEffect(() => {
    if (authLoading) {
      setHydrated(false);
      return;
    }
    if (!user) {
      setSettings(DEFAULT_SETTINGS);
      setHydrated(true); // a guest has no server settings — settled immediately (and never gated)
      return;
    }
    setHydrated(false); // re-gate on a fresh identity until its settings resolve
    let alive = true;
    getSettings()
      .then((s) => alive && setSettings(s))
      .catch(() => alive && setSettings(DEFAULT_SETTINGS))
      .finally(() => alive && setHydrated(true)); // true in BOTH then and catch — never wedge the modal
    return () => {
      alive = false;
    };
  }, [user, authLoading]);

  // saveSettings merges a patch into the full record and PUTs it (the server validates the whole
  // object, so every field is always sent). Updates local state to the server's echoed value.
  const saveSettings = useCallback(
    async (patch) => {
      const next = { ...settings, ...patch };
      const saved = await updateSettings(next);
      setSettings(saved || next);
      return saved || next;
    },
    [settings]
  );

  // The null-render target publisher keeps lifecycle actions pointed at the company in view.
  const setTarget = useCallback((ticker, timeframe) => {
    const next = { ticker, timeframe };
    targetRef.current = next;
    setTargetState((old) => old.ticker === ticker && old.timeframe === timeframe ? old : next);
  }, []);

  const refreshModelRegistry = useCallback(async () => {
    const { ticker, timeframe } = targetRef.current;
    const body = await fetchModelRegistry(ticker, { timeframe });
    setModelRegistry(body?.models || []);
    return body?.models || [];
  }, []);

  useEffect(() => {
    if (!user) {
      setModelRegistry([]);
      return;
    }
    refreshModelRegistry().catch(() => setModelRegistry([]));
  }, [user, target, refreshModelRegistry]);

  // runTrain creates one candidate. It does not bump lastTrainedAt because serving did not change.
  const runTrain = useCallback(async () => {
    if (trainingRef.current) return null;
    const { ticker, timeframe } = targetRef.current;
    trainingRef.current = true;
    setAutoTraining(true);
    try {
      const candidate = await trainModel(ticker, {
        timeframe,
        horizon: settings.defaultHorizon,
        allowShort: settings.allowShort,
      });
      await refreshModelRegistry();
      return candidate;
    } finally {
      trainingRef.current = false;
      setAutoTraining(false);
    }
  }, [settings.defaultHorizon, settings.allowShort, refreshModelRegistry]);

  const promoteNow = useCallback(async (candidate, reason = "") => {
    const result = await promoteModel(candidate.ticker, candidate.modelVersion, {
      timeframe: candidate.timeframe,
      horizon: candidate.horizon,
      reason,
    });
    setLastTrainedAt(Date.now());
    await refreshModelRegistry();
    return result;
  }, [refreshModelRegistry]);

  const rollbackNow = useCallback(async (record, reason = "") => {
    const result = await rollbackModel(record.ticker, record.modelVersion, {
      timeframe: record.timeframe,
      horizon: record.horizon,
      reason,
    });
    setLastTrainedAt(Date.now());
    await refreshModelRegistry();
    return result;
  }, [refreshModelRegistry]);

  const value = {
    settings,
    hydrated,
    saveSettings,
    lastTrainedAt,
    autoTraining,
    modelRegistry,
    target,
    trainNow: runTrain,
    promoteNow,
    rollbackNow,
    refreshModelRegistry,
    setTarget,
  };

  // Do not mount App until the current identity is known. Otherwise App starts its dashboard and
  // quote requests with NVDA before a returning user's saved active company has hydrated.
  if (!hydrated) {
    return (
      <div className="flex min-h-screen items-center justify-center bg-bg text-[13px] text-muted" role="status">
        Loading your workspace…
      </div>
    );
  }

  return <SettingsCtx.Provider value={value}>{children}</SettingsCtx.Provider>;
}
