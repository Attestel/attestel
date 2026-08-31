package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
)

// feedback.go — the Feedback record + validation.

// Feedback is one tester submission AS PERSISTED. Context is whatever compact snapshot the frontend
// captured at submit time (ticker, view, timeframe, data-source flags) — stored as-is, size-capped,
// never interpreted by the server.
//
// Owner is the opaque auth user id, stamped SERVER-SIDE from the verified session cookie (D-16). It
// is the authorization key for every read, so it never crosses the wire to a browser: HTTP responses
// are built from `publicFeedback` below. A record with an EMPTY Owner is legacy — written before
// ownership existed — and stays that way: it is admin-only, never claimed by whoever reads it next.
type Feedback struct {
	ID        string         `json:"id"`
	CreatedAt int64          `json:"createdAt"` // unix seconds
	UpdatedAt int64          `json:"updatedAt"`
	Category  string         `json:"category"`          // bug | idea | ux | other
	Rating    int            `json:"rating"`            // 1..5; 0 = not given
	Message   string         `json:"message"`           // required
	Name      string         `json:"name,omitempty"`    // optional tester name/contact
	Status    string         `json:"status"`            // new | reviewed | resolved
	Context   map[string]any `json:"context,omitempty"` // auto-captured app state at submit time
	Owner     string         `json:"owner,omitempty"`   // auth uid; "" = legacy, admin-only. NEVER served.
}

// publicFeedback is the wire shape: everything a client legitimately needs, minus Owner. Building
// responses from a distinct type (rather than blanking a field) means a future field added to the
// persisted record cannot leak by omission — it has to be added here deliberately.
type publicFeedback struct {
	ID        string         `json:"id"`
	CreatedAt int64          `json:"createdAt"`
	UpdatedAt int64          `json:"updatedAt"`
	Category  string         `json:"category"`
	Rating    int            `json:"rating"`
	Message   string         `json:"message"`
	Name      string         `json:"name,omitempty"`
	Status    string         `json:"status"`
	Context   map[string]any `json:"context,omitempty"`
}

func public(f Feedback) publicFeedback {
	return publicFeedback{
		ID: f.ID, CreatedAt: f.CreatedAt, UpdatedAt: f.UpdatedAt,
		Category: f.Category, Rating: f.Rating, Message: f.Message,
		Name: f.Name, Status: f.Status, Context: f.Context,
	}
}

var validCategory = map[string]bool{"bug": true, "idea": true, "ux": true, "other": true}
var validStatus = map[string]bool{"new": true, "reviewed": true, "resolved": true}

const (
	maxMessageLen = 4000
	maxNameLen    = 120
	maxContextKey = 40 // keep the snapshot compact — it's provenance, not a payload dump
)

func validateFeedback(f *Feedback) error {
	f.Message = strings.TrimSpace(f.Message)
	f.Name = strings.TrimSpace(f.Name)
	f.Category = strings.ToLower(strings.TrimSpace(f.Category))

	if f.Message == "" {
		return errors.New("message is required")
	}
	if len(f.Message) > maxMessageLen {
		f.Message = f.Message[:maxMessageLen]
	}
	if len(f.Name) > maxNameLen {
		f.Name = f.Name[:maxNameLen]
	}
	if f.Category == "" {
		f.Category = "other"
	}
	if !validCategory[f.Category] {
		return errors.New("category must be one of: bug, idea, ux, other")
	}
	if f.Rating < 0 || f.Rating > 5 {
		return errors.New("rating must be 1-5 (or omitted)")
	}
	if len(f.Context) > maxContextKey {
		return errors.New("context snapshot too large")
	}
	return nil
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
