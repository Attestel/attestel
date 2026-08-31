package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/smtp"
	"strings"
	"time"
)

// notify.go — best-effort out-of-app delivery. Everything here is optional and MUST NOT crash the
// evaluation loop: every failure is logged and swallowed. The in-app events feed is the source of
// truth; webhook/email are conveniences.
//
// Messages are neutral, descriptive statements of a condition ("NVDA price crossed above 130.00")
// — never advice, never an order. That property is guaranteed upstream in eval.go, where every
// message is composed from fixed templates.

type Notifier struct {
	cfg  Config
	http *http.Client
}

func newNotifier(cfg Config) *Notifier {
	return &Notifier{cfg: cfg, http: &http.Client{Timeout: 10 * time.Second}}
}

// Notify delivers an event to any configured channels. Safe to call for every fired event.
func (n *Notifier) Notify(ev Event) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("notify panic recovered: %v", rec)
		}
	}()
	if n.cfg.WebhookURL != "" {
		if err := n.postWebhook(ev); err != nil {
			log.Printf("webhook notify failed: %v", err)
		}
	}
	if n.cfg.SMTPHost != "" && len(n.cfg.EmailTo) > 0 {
		if err := n.sendEmail(ev); err != nil {
			log.Printf("email notify failed: %v", err)
		}
	}
}

// notificationBody composes the out-of-app text: the event message, then the research context.
//
// It NAMES THE COMPANY IN TEXT deliberately. D-19 is unresolved — the company is not addressable in
// the URL — so an out-of-app link can only carry a hash and lands on the right section of whatever
// company is active. Until that is fixed, the words are what make the notification unambiguous, and
// removing them would make the link quietly wrong rather than merely imprecise (§4.5).
//
// Synthetic data is called out in the body for the same reason it is in the UI: a notification is
// often read away from the app, where the banner is not there to qualify it.
func notificationBody(ev Event) string {
	var b strings.Builder
	b.WriteString(ev.Message)
	if ev.DataState == DataStateSynthetic {
		b.WriteString("\n\n[SYNTHETIC DEMO DATA — this did not come from a live market feed.]")
	} else if ev.DataState == DataStateSeed {
		b.WriteString("\n\n[Seed data — this did not come from a live market feed.]")
	}
	if ev.ThesisID != "" {
		b.WriteString("\n\nThis relates to your " + ev.Ticker + " thesis.")
		if ev.Bearing != nil {
			b.WriteString(" Bearing: " + *ev.Bearing + ".")
		}
		switch ev.ResearchAction {
		case ActionReviewThesis:
			b.WriteString("\nSuggested next step: review the thesis.")
		case ActionAnswerQuestion:
			b.WriteString("\nSuggested next step: answer the open question.")
		default:
			b.WriteString("\nSuggested next step: review the evidence.")
		}
		if ev.ResearchLink != nil {
			b.WriteString("\nOpen " + ev.Ticker + " in Attestel, then: " + ev.ResearchLink.Hash)
		}
	}
	return b.String()
}

// postWebhook POSTs a JSON payload. It carries the documented fields {ticker, type, message, ts}
// plus `text` (Slack) and `content` (Discord) aliases, so the same URL works for a generic
// receiver, a Slack incoming webhook, or a Discord webhook without per-provider config.
func (n *Notifier) postWebhook(ev Event) error {
	text := notificationBody(ev)
	payload := map[string]any{
		"ticker":    ev.Ticker,
		"type":      ev.Type,
		"timeframe": ev.Timeframe,
		"message":   ev.Message,
		"ts":        ev.TS,
		"text":      text, // Slack
		"content":   text, // Discord
	}
	// Additive thesis context. Existing receivers ignore unknown keys; the documented four fields
	// above are unchanged, so no configured webhook breaks.
	if ev.ThesisID != "" {
		payload["thesisId"] = ev.ThesisID
		payload["researchAction"] = ev.ResearchAction
		if ev.ThesisItemID != "" {
			payload["thesisItemId"] = ev.ThesisItemID
		}
		if ev.Bearing != nil {
			payload["bearing"] = *ev.Bearing
		}
		if ev.ResearchLink != nil {
			payload["researchLink"] = ev.ResearchLink
		}
	}
	if ev.DataState != "" {
		payload["dataState"] = ev.DataState
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook http %d", resp.StatusCode)
	}
	return nil
}

// sendEmail sends a plain-text email via SMTP (STARTTLS on the standard submission port handled by
// the server). Auth is used only when SMTP_USER is set (some relays are unauthenticated).
func (n *Notifier) sendEmail(ev Event) error {
	addr := n.cfg.SMTPHost + ":" + n.cfg.SMTPPort
	from := n.cfg.SMTPUser
	if from == "" {
		from = "alerts@nvda-platform.local"
	}
	subject := fmt.Sprintf("[NVDA Platform alert] %s %s", ev.Ticker, ev.Type)
	msg := "From: " + from + "\r\n" +
		"To: " + strings.Join(n.cfg.EmailTo, ", ") + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"\r\n" +
		notificationBody(ev) + "\r\n\r\n" +
		"This is a descriptive market-condition notification. It is not financial advice.\r\n"

	var auth smtp.Auth
	if n.cfg.SMTPUser != "" {
		auth = smtp.PlainAuth("", n.cfg.SMTPUser, n.cfg.SMTPPass, n.cfg.SMTPHost)
	}
	return smtp.SendMail(addr, auth, from, n.cfg.EmailTo, []byte(msg))
}
