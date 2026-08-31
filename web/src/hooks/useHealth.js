import { useState } from "react";
import { HEALTH_TARGETS, pingHealth } from "../lib/api.js";
import { usePolling } from "./usePolling.js";

// useHealth — polls the browser-reachable service /health endpoints (gateway, alerts, journal,
// paper). Returns a map of key -> boolean (undefined until first probe = "unknown"). Never throws.
export function useHealth(intervalMs = 15000) {
  const [status, setStatus] = useState({});

  usePolling(async () => {
    const results = await Promise.all(
      HEALTH_TARGETS.map((t) => pingHealth(t.url).then((ok) => [t.key, ok]))
    );
    setStatus(Object.fromEntries(results));
  }, intervalMs);

  return status;
}
