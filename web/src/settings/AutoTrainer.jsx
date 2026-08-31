import { useEffect } from "react";
import { useSettings } from "./SettingsContext.jsx";

// Legacy name, smaller responsibility: publish the company in view for explicit model lifecycle
// actions. It intentionally runs no timer and never trains from a page lifecycle.
export default function AutoTrainer({ ticker, timeframe }) {
  const { setTarget } = useSettings();

  // Publish the in-view symbol so the manual "Train now" button (trainNow) targets what's on screen.
  useEffect(() => {
    setTarget(ticker, timeframe);
  }, [ticker, timeframe, setTarget]);

  return null;
}
