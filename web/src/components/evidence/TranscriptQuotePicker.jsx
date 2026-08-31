import { useEffect, useState } from "react";
import { Field, controlClass } from "../ui/index.js";
import { fetchTranscriptHistory } from "../../lib/api.js";
import CaptureStrip from "./CaptureStrip.jsx";

// TranscriptQuotePicker — turns a STORED transcript analysis into savable quotations.
//
// This is the only door into transcript evidence, and it is narrow on purpose. The transcript source
// text is never persisted (D-08 (a)); what IS persisted is the analysis snapshot, whose quotations the
// analysis pipeline already checked verbatim against the source and dropped if it could not find them.
// So the addressable strings here — key points, the two halves of an evasive moment, tone phrases, risk
// flags, the summary — are exactly the strings that survived that check.
//
// Each row sends a LOCATOR (`evasiveMoments:0:answer`), not the text. The server re-reads the snapshot
// and stores its own bytes, so a quotation cannot be edited on the way into the evidence store.
//
// A pasted transcript that has never been analysed produces nothing here, which is correct: there is
// no stored, verified text to quote from yet.

// LOCATOR_GROUPS defines what is quotable and how each is labelled. `management_statement` is offered
// only for an evasive-moment ANSWER — the one place a specific person is answering a specific question,
// which is what makes naming the speaker meaningful rather than a guess about the whole call.
function quotableFrom(analysis) {
  const s = analysis?.structured || {};
  const out = [];

  (s.keyPoints || []).forEach((text, i) => {
    if (!text) return;
    out.push({ locator: `keyPoints:${i}`, group: "Key point", text, kind: "transcript_excerpt" });
  });

  (s.evasiveMoments || []).forEach((m, i) => {
    if (m?.question) {
      out.push({
        locator: `evasiveMoments:${i}:question`,
        group: "Analyst question",
        text: m.question,
        kind: "transcript_excerpt",
      });
    }
    if (m?.answer) {
      out.push({
        locator: `evasiveMoments:${i}:answer`,
        group: `Evasive answer${m.topic ? ` · ${m.topic}` : ""}`,
        text: m.answer,
        kind: "transcript_excerpt",
        // An answer is the one place a specific person replies to a specific question, so naming the
        // speaker is meaningful here and a guess anywhere else. Naming one upgrades the capture to a
        // management statement; leaving it blank keeps it a plain, still-verified quotation.
        speakerOptional: true,
      });
    }
  });

  (s.tone?.notableLanguage || []).forEach((text, i) => {
    if (!text) return;
    out.push({ locator: `notableLanguage:${i}`, group: "Tone phrase", text, kind: "transcript_excerpt" });
  });

  (s.riskFlags || []).forEach((text, i) => {
    if (!text) return;
    out.push({ locator: `riskFlags:${i}`, group: "Risk raised by the language", text, kind: "transcript_excerpt" });
  });

  if (s.summary) {
    out.push({ locator: "summary", group: "Call summary", text: s.summary, kind: "transcript_excerpt" });
  }
  return out;
}

export default function TranscriptQuotePicker({ ticker, timeframe = "1D" }) {
  const [analyses, setAnalyses] = useState(null); // null = loading, [] = none stored
  const [selectedKey, setSelectedKey] = useState("");
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let alive = true;
    setAnalyses(null);
    setFailed(false);
    fetchTranscriptHistory(ticker)
      .then((r) => {
        if (!alive) return;
        const list = r?.analyses || [];
        setAnalyses(list);
        setSelectedKey(list[0]?.key || "");
      })
      .catch(() => {
        if (!alive) return;
        setAnalyses([]);
        setFailed(true);
      });
    return () => {
      alive = false;
    };
  }, [ticker]);

  if (analyses === null) {
    return (
      <p className="rounded-lg border border-line bg-panel2/30 px-3.5 py-3 text-[12px] text-muted">
        Looking for stored transcript analyses…
      </p>
    );
  }

  if (analyses.length === 0) {
    return (
      <p className="rounded-lg border border-line bg-panel2/30 px-3.5 py-3 text-[12px] leading-relaxed text-muted">
        {failed
          ? "The stored transcript analyses could not be read, so no quotation can be captured with a verified source right now."
          : `No transcript has been analysed for ${ticker} yet. Quotations can only be captured from a stored analysis — the call text itself is never kept, so an un-analysed transcript has nothing verifiable to quote.`}
      </p>
    );
  }

  const selected = analyses.find((a) => a.key === selectedKey) || analyses[0];
  const quotes = quotableFrom(selected);
  const label = selected?.label || selected?.key || "";
  const stub = String(selected?.modelUsed || "").startsWith("stub:");

  const items = quotes.map((q) => ({
    key: `${selected.key}:${q.locator}`,
    kind: q.kind,
    speakerOptional: !!q.speakerOptional,
    selector: { transcriptKey: selected.key, locator: q.locator },
    title: q.text,
    subtitle: `${q.group} · ${label}`,
    dataState: "live",
    preview: [
      { label: "Quotation", value: q.text },
      { label: "From", value: `${label} earnings call · ${q.group}` },
      { label: "Locator", value: q.locator, mono: true },
    ],
    excerptNote:
      "The quotation is re-read from the stored analysis by the server, so it is stored exactly as it was verified against the call text. The source text itself is not retained.",
  }));

  return (
    <div className="flex flex-col gap-2.5">
      {analyses.length > 1 && (
        <Field label="Transcript">
          <select className={controlClass} value={selected.key} onChange={(e) => setSelectedKey(e.target.value)}>
            {analyses.map((a) => (
              <option key={a.key} value={a.key}>
                {a.label || a.key}
              </option>
            ))}
          </select>
        </Field>
      )}

      {stub && (
        <p className="rounded-md border border-warn/40 bg-warn/10 px-3 py-2 text-[11.5px] leading-relaxed text-warn">
          This analysis came from the offline deterministic fallback, not the model. Its lines are keyword
          extractions from the text you supplied — capture them as context, not as verified quotations.
        </p>
      )}

      <CaptureStrip
        ticker={ticker}
        timeframe={timeframe}
        items={items}
        title={`Quotations from ${label}`}
        hint="Every line below was verified verbatim against the call text when the transcript was analysed."
        emptyNote="This analysis contains no quotable lines."
      />
    </div>
  );
}
