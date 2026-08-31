import { useEffect, useState } from "react";
import {
  AdminRequiredError,
  AuthRequiredError,
  deleteFeedback,
  listFeedback,
  submitFeedback,
  updateFeedback,
} from "../lib/api.js";
import { useAuth } from "../auth/AuthContext.jsx";
import { Icon } from "./shell/icons.jsx";

// FeedbackPanel — tester submissions, and (for an admin) triage, in one tab.
//
// Submit: rating (1-5), category, message, optional name. A compact context snapshot (ticker,
// timeframe, data-source flags, app hash) is captured AUTOMATICALLY so bug reports are
// reproducible without interrogating the tester.
//
// Access follows the feedback service's own policy (D-16), which this component only REFLECTS:
//   guest        -> cannot submit; routed to the existing sign-in flow
//   tester       -> submits, and sees only their OWN submissions, read-only
//   admin        -> sees every submission (including legacy ownerless ones), filters, triages, deletes
//
// `admin` comes from the server's response, computed from its FEEDBACK_ADMIN_UIDS allow-list — never
// from local state. Hiding a button is presentation, not authorization: every privileged route is
// re-checked server-side, so a user who forces this flag still gets 403.
//
// Talks to the standalone feedback service (:8098); fails soft if it's genuinely down.

const CATEGORIES = [
  { id: "bug", label: "Bug" },
  { id: "idea", label: "Idea" },
  { id: "ux", label: "UX" },
  { id: "other", label: "Other" },
];

const STATUS_TONE = {
  new: "text-warn border-warn/40 bg-warn/10",
  reviewed: "text-info border-info/40 bg-info/10",
  resolved: "text-up border-up/40 bg-up/10",
};

const fmtWhen = (unix) => new Date(unix * 1000).toLocaleString();

export default function FeedbackPanel({ ticker, timeframe, data }) {
  const { user, requireAuth, promptSignIn } = useAuth();

  // ---- submit form state ----
  const [category, setCategory] = useState("bug");
  const [rating, setRating] = useState(0);
  const [message, setMessage] = useState("");
  const [name, setName] = useState("");
  const [sending, setSending] = useState(false);
  const [sent, setSent] = useState(false);
  const [sendErr, setSendErr] = useState(null);

  // ---- review list state ----
  const [items, setItems] = useState(null);
  const [isAdmin, setIsAdmin] = useState(false);
  const [statusFilter, setStatusFilter] = useState("");
  const [listErr, setListErr] = useState(null); // set ONLY for a genuine service failure
  const [signInRequired, setSignInRequired] = useState(false);

  const load = () => {
    if (!user) {
      // A guest has nothing to list, and asking would just 401. Reset to the signed-out view.
      setItems(null);
      setIsAdmin(false);
      setListErr(null);
      setSignInRequired(false);
      return;
    }
    listFeedback(statusFilter)
      .then((r) => {
        setItems(r.feedback || []);
        setIsAdmin(Boolean(r.admin));
        setListErr(null);
        setSignInRequired(false);
      })
      .catch((e) => {
        setItems(null);
        setIsAdmin(false);
        if (e instanceof AuthRequiredError) {
          // Session expired between mount and fetch — sign-in required, not a dead service.
          setSignInRequired(true);
          setListErr(null);
        } else if (e instanceof AdminRequiredError) {
          setSignInRequired(false);
          setListErr(null);
        } else {
          setSignInRequired(false);
          setListErr(e.message);
        }
      });
  };
  useEffect(load, [statusFilter, user]);

  const contextSnapshot = () => ({
    ticker,
    timeframe,
    view: window.location.hash || "#settings/help",
    syntheticPrice: Boolean(data?.priceSourceIsSynthetic),
    priceSource: data?.priceSource || null,
    newsSource: data?.newsSource || null,
    userAgent: navigator.userAgent.slice(0, 120),
  });

  const send = (e) => {
    e.preventDefault();
    if (!message.trim()) return;
    if (!requireAuth()) return; // guest -> remembered + routed to sign-in, exactly like every other write
    setSending(true);
    setSendErr(null);
    submitFeedback({
      category,
      rating: rating || 0,
      message: message.trim(),
      name: name.trim(),
      context: contextSnapshot(),
    })
      .then(() => {
        setSent(true);
        setMessage("");
        setRating(0);
        load();
        setTimeout(() => setSent(false), 4000);
      })
      .catch((err) => {
        if (err instanceof AuthRequiredError) {
          promptSignIn();
          return;
        }
        setSendErr(err.message);
      })
      .finally(() => setSending(false));
  };

  // Triage and delete are admin-only server-side; a 403 here means the allow-list says no, which is
  // not the same thing as "the service is unreachable".
  const onAdminError = (err) => {
    if (err instanceof AuthRequiredError) promptSignIn();
    else if (err instanceof AdminRequiredError) setListErr(null);
    load();
  };
  const triage = (id, status) => {
    updateFeedback(id, status).then(load).catch(onAdminError);
  };
  const remove = (id) => {
    deleteFeedback(id).then(load).catch(onAdminError);
  };

  const unread = (items || []).filter((f) => f.status === "new").length;

  return (
    <div className="space-y-5">
      {/* ---- Submit ---- */}
      <section className="overflow-hidden surface-card">
        <div className="border-b border-line px-[22px] py-3.5">
          <p className="text-[13px] leading-[1.6] text-muted">
            <strong className="font-semibold text-info">Send feedback.</strong> Found a bug, have an idea, or
            something felt off? A snapshot of your current view ({ticker} · {timeframe}
            {data?.priceSourceIsSynthetic ? " · synthetic data" : ""}) is attached automatically so it's
            reproducible.
          </p>
        </div>

        {!user ? (
          <div className="space-y-3 px-[22px] py-5">
            <p className="text-[13px] leading-relaxed text-muted">
              Sign in to send feedback. Submissions are tied to your account so we can follow up, and so only
              you (and the team triaging them) can read yours.
            </p>
            <button
              type="button"
              onClick={promptSignIn}
              className="rounded-full bg-accent/15 px-4 py-2 text-[13px] font-semibold text-accent transition-colors hover:bg-accent/25"
            >
              Sign in to send feedback
            </button>
          </div>
        ) : (
          <form onSubmit={send} className="space-y-4 px-[22px] py-4">
            <div className="flex flex-wrap items-center gap-4">
              <div className="flex gap-1.5">
                {CATEGORIES.map((c) => (
                  <button
                    key={c.id}
                    type="button"
                    onClick={() => setCategory(c.id)}
                    className={`rounded-full border px-3 py-1.5 text-[12px] font-semibold transition-colors ${
                      category === c.id
                        ? "border-accent/50 bg-accent/15 text-accent"
                        : "border-line text-muted hover:text-fg"
                    }`}
                  >
                    {c.label}
                  </button>
                ))}
              </div>
              <div className="flex items-center gap-1" role="radiogroup" aria-label="Rating">
                {[1, 2, 3, 4, 5].map((n) => (
                  <button
                    key={n}
                    type="button"
                    onClick={() => setRating(n === rating ? 0 : n)}
                    aria-label={`${n} star${n > 1 ? "s" : ""}`}
                    className={`leading-none transition-colors ${
                      n <= rating ? "text-warn [&_svg]:fill-current" : "text-muted/40 hover:text-muted"
                    }`}
                  >
                    <Icon name="star" size={18} />
                  </button>
                ))}
                <span className="ml-1 text-[11px] text-muted">{rating ? `${rating}/5` : "optional"}</span>
              </div>
            </div>

            <textarea
              value={message}
              onChange={(e) => setMessage(e.target.value)}
              rows={3}
              required
              placeholder="What happened / what would make this better?"
              className="w-full rounded-lg border border-line bg-panel2 px-3 py-2.5 text-[13px] text-fg placeholder:text-muted/50 focus:border-accent/60 focus:outline-none"
            />

            <div className="flex flex-wrap items-center gap-3">
              <input
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="Name / contact (optional)"
                className="w-56 rounded-lg border border-line bg-panel2 px-3 py-2 text-[12px] text-fg placeholder:text-muted/50 focus:border-accent/60 focus:outline-none"
              />
              <button
                type="submit"
                disabled={sending || !message.trim()}
                className="rounded-full bg-accent/15 px-4 py-2 text-[13px] font-semibold text-accent transition-colors hover:bg-accent/25 disabled:opacity-50"
              >
                {sending ? "Sending…" : "Submit feedback"}
              </button>
              {sent && <span className="text-[12px] text-up">Thanks — received.</span>}
              {sendErr && <span className="text-[12px] text-down">Failed: {sendErr}</span>}
            </div>
          </form>
        )}
      </section>

      {/* ---- Submissions (own, or all for an admin) ---- */}
      {user && (
        <section className="overflow-hidden surface-card">
          <div className="flex flex-wrap items-center gap-3 border-b border-line px-[22px] py-3.5">
            <h2 className="text-[13px] font-bold text-fg">
              {isAdmin ? "All submissions" : "Your submissions"}
              {items ? ` (${items.length})` : ""}
            </h2>
            {isAdmin && unread > 0 && (
              <span className="rounded-full bg-warn/15 px-2 py-0.5 text-[11px] font-bold text-warn">
                {unread} new
              </span>
            )}
            <div className="ml-auto flex items-center gap-1.5">
              {isAdmin &&
                ["", "new", "reviewed", "resolved"].map((s) => (
                  <button
                    key={s || "all"}
                    type="button"
                    onClick={() => setStatusFilter(s)}
                    className={`rounded-full px-2.5 py-1 text-[11px] font-semibold transition-colors ${
                      statusFilter === s ? "bg-accent/15 text-accent" : "text-muted hover:text-fg"
                    }`}
                  >
                    {s || "all"}
                  </button>
                ))}
              <button type="button" onClick={load} title="Reload" className="ml-1 text-muted hover:text-fg">
                <Icon name="reload" size={13} />
              </button>
            </div>
          </div>

          {!isAdmin && !listErr && !signInRequired && (
            <p className="border-b border-line px-[22px] py-2.5 text-[12px] leading-relaxed text-muted">
              You're seeing only the feedback you sent. Other testers' submissions aren't visible to you.
            </p>
          )}

          {signInRequired && (
            <p className="px-[22px] py-6 text-[13px] text-muted">
              Your session ended.{" "}
              <button type="button" onClick={promptSignIn} className="text-accent hover:underline">
                Sign in again
              </button>{" "}
              to see your submissions.
            </p>
          )}
          {listErr && (
            <p className="px-[22px] py-6 text-[13px] text-down">
              Feedback service unreachable ({listErr}) — is it running on :8098?
            </p>
          )}
          {items && items.length === 0 && (
            <p className="px-[22px] py-8 text-center text-[13px] text-muted">
              {isAdmin ? "No submissions yet." : "You haven't sent any feedback yet."}
            </p>
          )}

          {items && items.length > 0 && (
            <ul className="divide-y divide-line">
              {items.map((f) => (
                <li key={f.id} className="px-[22px] py-4">
                  <div className="mb-1.5 flex flex-wrap items-center gap-2.5">
                    <span className={`rounded-md border px-2 py-0.5 text-[10px] font-bold uppercase ${STATUS_TONE[f.status] || ""}`}>
                      {f.status}
                    </span>
                    <span className="rounded-md bg-panel2 px-2 py-0.5 text-[10px] font-semibold uppercase text-muted">
                      {f.category}
                    </span>
                    {f.rating > 0 && (
                      <span className="inline-flex items-center gap-0.5" aria-label={`${f.rating} of 5`}>
                        {[1, 2, 3, 4, 5].map((n) => (
                          <Icon
                            key={n}
                            name="star"
                            size={10}
                            className={n <= f.rating ? "text-warn fill-current" : "text-muted/30"}
                          />
                        ))}
                      </span>
                    )}
                    <span className="text-[11px] text-muted/70">
                      {fmtWhen(f.createdAt)}
                      {f.name ? ` · ${f.name}` : " · anonymous"}
                      {f.context?.ticker ? ` · ${f.context.ticker}/${f.context.timeframe || "?"}` : ""}
                      {f.context?.syntheticPrice ? " · synthetic data" : ""}
                    </span>
                    {isAdmin && (
                      <span className="ml-auto flex gap-2">
                        {f.status !== "reviewed" && (
                          <button type="button" onClick={() => triage(f.id, "reviewed")} className="text-[11px] text-info hover:underline">
                            mark reviewed
                          </button>
                        )}
                        {f.status !== "resolved" && (
                          <button type="button" onClick={() => triage(f.id, "resolved")} className="text-[11px] text-up hover:underline">
                            resolve
                          </button>
                        )}
                        <button type="button" onClick={() => remove(f.id)} className="text-[11px] text-down/80 hover:underline">
                          delete
                        </button>
                      </span>
                    )}
                  </div>
                  <p className="whitespace-pre-wrap text-[13px] leading-relaxed text-fg/90">{f.message}</p>
                </li>
              ))}
            </ul>
          )}
        </section>
      )}
    </div>
  );
}
