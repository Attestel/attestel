import { useEffect, useRef, useState } from "react";

// useFlash — watches a numeric value and reports a flash direction when it changes. The returned
// `key` increments every change so a consumer can remount the flashing node (`key={flash.key}`),
// which reliably restarts the CSS animation even on same-direction changes. Reduced-motion is
// handled in CSS (the flash-* classes are disabled), so this stays direction-only.
export function useFlash(value) {
  const prev = useRef(value);
  const [state, setState] = useState({ cls: "", key: 0 });

  useEffect(() => {
    const p = prev.current;
    prev.current = value;
    if (p == null || value == null) return;
    const a = Number(value);
    const b = Number(p);
    if (!Number.isFinite(a) || !Number.isFinite(b) || a === b) return;
    setState((s) => ({ cls: a > b ? "flash-up" : "flash-down", key: s.key + 1 }));
  }, [value]);

  return state;
}
