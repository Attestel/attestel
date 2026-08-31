package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// onboarding.go — the durable state behind the thesis-first first session (Step 11).
//
// The guided sequence is one company research review, not a feature tour: choose a company, orient on
// the snapshot, write one falsifiable claim, name what would break it, attach one source-backed
// evidence item, run a challenge lens, then commit to the next question. The user may leave at any
// point and come back; nothing here blocks the rest of the product, and `dismissed` turns the guidance
// off permanently without touching what they have already built.
//
// Two rules make this record trustworthy:
//
//  1. PROGRESS IS A HINT; COMPLETION IS A FACT. `step`/`completedSteps` are what the client reported,
//     so the guidance can resume where the user left off. `status: "completed"` is NEVER accepted from
//     a client — verifyActivation re-reads the caller's own theses, evidence links, and saved reviews
//     and only then does the record complete. Activation means DURABLE records exist, not that a
//     wizard reached its last screen.
//  2. THE ACTIVATION EVIDENCE MUST BE SOURCE-BACKED. A model-derived record (`inference: true`,
//     §2.7) can never satisfy the evidence condition — otherwise "attach evidence" would be
//     satisfiable by asking the model a question, and activation would measure nothing.
//
// Storage: {TRADES_DIR}/{uid}/onboarding.json, same lazy-load + atomic-write discipline as every
// other per-user record here. A guest has no partition, so a guest has no durable onboarding record:
// the client holds guest progress and the two guest-visible events (§6.3) are emitted without a write.

const (
	onboardingSchemaVersion = 1
	onboardingFile          = "onboarding.json"

	// onboardingResumeGapSec is how long a first session may pause before the next state write counts
	// as a RESUME rather than continued progress (§6.2 "state write after a gap"). Half an hour: long
	// enough that stepping through the guide in one sitting is one session, short enough that coming
	// back after lunch is honestly a resume.
	onboardingResumeGapSec = 30 * 60
)

// The seven steps, in order. Ids are stable — they are persisted and they key the resume.
const (
	stepChooseCompany   = "choose_company"
	stepOrientSnapshot  = "orient_snapshot"
	stepWriteClaim      = "write_claim"
	stepNameCondition   = "name_condition"
	stepAttachEvidence  = "attach_evidence"
	stepChallengeReview = "challenge_review"
	stepCommitNext      = "commit_next"
)

var onboardingSteps = []string{
	stepChooseCompany, stepOrientSnapshot, stepWriteClaim, stepNameCondition,
	stepAttachEvidence, stepChallengeReview, stepCommitNext,
}

var validOnboardingStep = func() map[string]bool {
	m := make(map[string]bool, len(onboardingSteps))
	for _, s := range onboardingSteps {
		m[s] = true
	}
	return m
}()

const (
	onboardingNotStarted = "not_started"
	onboardingInProgress = "in_progress"
	onboardingCompleted  = "completed"
)

// OnboardingActivation is the SERVER'S proof that activation happened: the exact durable records that
// satisfied it. Stored so the completion is auditable later, and so a re-check is idempotent.
type OnboardingActivation struct {
	ThesisID   string `json:"thesisId"`
	EvidenceID string `json:"evidenceId"`
	LinkID     string `json:"linkId"`
	ReviewID   string `json:"reviewId"`
	Task       string `json:"task"`
	DataState  string `json:"dataState"` // of the evidence that satisfied it — feeds synthetic exclusion
	VerifiedAt int64  `json:"verifiedAt"`
}

// OnboardingState is the per-user record.
type OnboardingState struct {
	SchemaVersion  int      `json:"schemaVersion"`
	Status         string   `json:"status"` // not_started | in_progress | completed
	Step           string   `json:"step"`
	CompletedSteps []string `json:"completedSteps"`

	Ticker   string `json:"ticker"`
	ThesisID string `json:"thesisId"`

	// Level is the experience level the client reported (beginner|standard). Stored so server-side
	// events can carry §6.3's `level` property without this service calling the auth service on a
	// write path. It is a coarse enum, never personal content.
	Level string `json:"level"`

	// Dismissed records that the user turned the guidance off. The record survives — dismissing the
	// guide is not abandoning the research.
	Dismissed bool `json:"dismissed"`

	StartedAt    int64 `json:"startedAt"`
	LastActiveAt int64 `json:"lastActiveAt"`
	CompletedAt  int64 `json:"completedAt"`
	ResumeCount  int   `json:"resumeCount"`

	Activation *OnboardingActivation `json:"activation"`
}

// onboardingProgress is what the client needs to render the guide: the state plus which activation
// conditions are already durable, so the UI can show honest progress instead of its own guess.
type onboardingProgress struct {
	State           OnboardingState `json:"state"`
	Steps           []string        `json:"steps"`
	HasThesis       bool            `json:"hasThesis"`
	HasEvidenceLink bool            `json:"hasEvidenceLink"`
	HasChallenge    bool            `json:"hasChallenge"`
	Guest           bool            `json:"guest"`
}

func newOnboardingState() OnboardingState {
	return OnboardingState{
		SchemaVersion:  onboardingSchemaVersion,
		Status:         onboardingNotStarted,
		Step:           stepChooseCompany,
		CompletedSteps: []string{},
	}
}

// ---- store ----

type onboardingStore struct {
	base  string
	mu    sync.Mutex
	cache map[string]*OnboardingState
	// broken marks a user whose onboarding.json exists but did not parse; we serve a fresh state and
	// refuse to overwrite the file, same refuse-to-clobber rule as every other store here.
	broken map[string]bool
	docs   *documentRepository
}

func openOnboardingStore(base string) (*onboardingStore, error) {
	return openOnboardingStoreWithRepository(base, nil)
}

func openOnboardingStoreWithRepository(base string, docs *documentRepository) (*onboardingStore, error) {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}
	return &onboardingStore{base: base, cache: map[string]*OnboardingState{}, broken: map[string]bool{}, docs: docs}, nil
}

func (s *onboardingStore) path(uid string) string {
	return filepath.Join(s.base, uid, onboardingFile)
}

// loadLocked returns the user's state, reading from disk on first access. Caller holds s.mu.
func (s *onboardingStore) loadLocked(uid string) *OnboardingState {
	if st, ok := s.cache[uid]; ok {
		return st
	}
	st := newOnboardingState()
	var b []byte
	var found bool
	var err error
	if s.docs != nil {
		b, found, err = s.docs.load(uid, "onboarding", s.path(uid))
	} else {
		b, err = os.ReadFile(s.path(uid))
		found = err == nil
	}
	if err != nil && !os.IsNotExist(err) {
		s.broken[uid] = true
	} else if found {
		if err := json.Unmarshal(b, &st); err != nil {
			log.Printf("journal: %s did not parse (%v) — serving a fresh guide and refusing to overwrite it", s.path(uid), err)
			s.broken[uid] = true
			st = newOnboardingState()
		}
	}
	if st.CompletedSteps == nil {
		st.CompletedSteps = []string{}
	}
	s.cache[uid] = &st
	return &st
}

func (s *onboardingStore) persistLocked(uid string) error {
	if s.broken[uid] {
		return errors.New("onboarding store is unreadable")
	}
	st := s.cache[uid]
	if st == nil {
		return errors.New("onboarding state is missing")
	}
	if s.docs != nil {
		if err := s.docs.save(uid, "onboarding", st); err != nil {
			s.broken[uid] = true
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Join(s.base, uid), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(uid) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(uid))
}

// Get returns a copy of the caller's state.
func (s *onboardingStore) Get(uid string) OnboardingState {
	if uid == "" || reservedUID(uid) {
		return newOnboardingState()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.loadLocked(uid)
	out := *st
	out.CompletedSteps = append([]string{}, st.CompletedSteps...)
	if st.Activation != nil {
		a := *st.Activation
		out.Activation = &a
	}
	return out
}

// levelOf feeds the analytics sink §6.3's `level` property from the cached record — an in-memory read
// on the write path, never a cross-service call.
func (s *onboardingStore) levelOf(uid string) string {
	if uid == "" || reservedUID(uid) {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(uid).Level
}

// ---- activation verification ----

// activationCheck is the result of re-reading the caller's durable records.
type activationCheck struct {
	HasThesis       bool
	HasEvidenceLink bool
	HasChallenge    bool
	Activation      *OnboardingActivation
}

// verifyActivation re-reads the caller's OWN theses, evidence, evidence links, and saved reviews and
// reports which activation conditions hold. Nothing is inferred and nothing is taken from the client.
//
// `preferThesis` scopes the check to the thesis the guide is about when there is one; otherwise ANY
// of the user's theses may satisfy activation, because a user who did the work outside the guide is
// activated just the same.
//
// Evidence and review files are read fresh rather than through the lanes' cached stores: those caches
// belong to the evidence and review route handlers, and a second cached instance here would go stale
// the moment the user saves something (the same reasoning as outcomes.go's readers).
func (s *Server) verifyActivation(uid, preferThesis string) activationCheck {
	var out activationCheck
	if uid == "" || reservedUID(uid) {
		return out
	}

	theses := s.theses.List(uid)
	if len(theses) == 0 {
		return out
	}
	owned := make(map[string]bool, len(theses))
	for _, t := range theses {
		owned[t.ID] = true
	}
	out.HasThesis = true

	records, err := readUserEvidence(s.cfg.TradesDir, uid, s.documents)
	if err != nil {
		return out
	}
	byID := make(map[string]EvidenceRecord, len(records))
	for _, r := range records {
		byID[r.ID] = r
	}

	links, err := readUserEvidenceLinks(s.cfg.TradesDir, uid, s.documents)
	if err != nil {
		return out
	}
	reviews, err := readUserChallengeReviews(s.cfg.TradesDir, uid, s.documents)
	if err != nil {
		return out
	}

	// candidate holds the per-thesis evidence + review that satisfy activation together.
	type candidate struct {
		evidenceID, linkID, dataState string
		reviewID, task                string
	}
	cands := map[string]*candidate{}
	get := func(id string) *candidate {
		if c, ok := cands[id]; ok {
			return c
		}
		c := &candidate{}
		cands[id] = c
		return c
	}

	for _, l := range links {
		if l.Revoked || !owned[l.ThesisID] {
			continue
		}
		rec, ok := byID[l.EvidenceID]
		if !ok || rec.Inference {
			continue // §2.7: model-derived content is not source-backed evidence
		}
		out.HasEvidenceLink = true
		c := get(l.ThesisID)
		if c.evidenceID == "" {
			c.evidenceID, c.linkID, c.dataState = rec.ID, l.ID, rec.DataState
		}
	}
	for _, rv := range reviews {
		if !owned[rv.thesisID] {
			continue
		}
		out.HasChallenge = true
		c := get(rv.thesisID)
		if c.reviewID == "" {
			c.reviewID, c.task = rv.id, rv.task
		}
	}

	// A thesis activates only when the SAME thesis carries both. Preferring the guide's thesis keeps
	// the recorded activation about the research the user was actually guided through.
	pick := func(id string) bool {
		c := cands[id]
		if c == nil || c.evidenceID == "" || c.reviewID == "" {
			return false
		}
		out.Activation = &OnboardingActivation{
			ThesisID: id, EvidenceID: c.evidenceID, LinkID: c.linkID,
			ReviewID: c.reviewID, Task: c.task, DataState: c.dataState,
		}
		return true
	}
	if preferThesis != "" && pick(preferThesis) {
		return out
	}
	for _, t := range theses {
		if pick(t.ID) {
			return out
		}
	}
	return out
}

// readUserEvidenceLinks reads {uid}/evidence_links.json read-only. Same rationale as outcomes.go's
// readUserEvidence: fresh, never cached here, never written.
func readUserEvidenceLinks(base, uid string, docs *documentRepository) ([]EvidenceLink, error) {
	if uid == "" || reservedUID(uid) {
		return nil, nil
	}
	var b []byte
	var err error
	var found bool
	if docs != nil {
		b, found, err = docs.load(uid, "evidence_links", filepath.Join(base, uid, linksFile))
	} else {
		b, err = os.ReadFile(filepath.Join(base, uid, linksFile))
		found = err == nil
	}
	if !found && err == nil {
		return nil, nil
	}
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []EvidenceLink
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// challengeReviewRef is the only part of a saved review this file needs: which thesis it challenged.
// The findings themselves are model text and are on the forbidden list — they are never read here.
type challengeReviewRef struct {
	id       string
	thesisID string
	task     string
	ticker   string
	lensID   string
}

func readUserChallengeReviews(base, uid string, docs *documentRepository) ([]challengeReviewRef, error) {
	if uid == "" || reservedUID(uid) {
		return nil, nil
	}
	var b []byte
	var err error
	var found bool
	if docs != nil {
		b, found, err = docs.load(uid, "reviews", filepath.Join(base, uid, "reviews.json"))
	} else {
		b, err = os.ReadFile(filepath.Join(base, uid, "reviews.json"))
		found = err == nil
	}
	if !found && err == nil {
		return nil, nil
	}
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list []Review
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	out := make([]challengeReviewRef, 0, len(list))
	for _, rev := range list {
		task := reviewStr(rev, "task")
		if !challengeTasks[task] {
			continue
		}
		ref := challengeReviewRef{
			id:       reviewStr(rev, "id"),
			thesisID: reviewStr(rev, "thesisId"),
			task:     task,
			ticker:   reviewStr(rev, "ticker"),
		}
		if lens, ok := rev["lens"].(map[string]any); ok {
			ref.lensID, _ = lens["id"].(string)
		}
		if ref.id != "" && ref.thesisID != "" {
			out = append(out, ref)
		}
	}
	return out, nil
}

// ---- routes ----

// registerOnboardingRoutes mounts the guided-first-session surface. One line in handlers.go, the same
// seam every lane after Step 04 used.
//
// GET is optionalAuth because a guest can see the guide (and start it) without an account; POST is
// optionalAuth too, and that is deliberate: a guest CAN start onboarding, which emits the two events
// §6.3 permits a guest to emit, but a guest has no partition so nothing durable is written and no
// activation event is reachable — every activation condition requires an authenticated write.
func (s *Server) registerOnboardingRoutes(mux *http.ServeMux) {
	store, err := openOnboardingStoreWithRepository(s.cfg.TradesDir, s.documents)
	if err != nil {
		// Additive, like every lane before it: if the store cannot open, the rest of the journal serves.
		log.Printf("journal: onboarding store unavailable in %q: %v (onboarding routes not mounted)", s.cfg.TradesDir, err)
		return
	}
	api := &onboardingAPI{srv: s, store: store}
	// The sink asks the onboarding record for the caller's experience level, which is how §6.3's
	// `level` property is populated without this service calling the auth service on a write path.
	analytics.useLevelSource(store.levelOf)

	mux.HandleFunc("GET /onboarding", s.optionalAuth(api.handleGet))
	mux.HandleFunc("POST /onboarding", s.optionalAuth(api.handleProgress))
}

// onboardingAPI owns the onboarding routes, constructed inside registerOnboardingRoutes exactly as
// the evidence, review, and decision lanes do — so this lane's footprint in shared files is one line.
type onboardingAPI struct {
	srv   *Server
	store *onboardingStore
}

func (a *onboardingAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	if uid == "" {
		writeJSON(w, http.StatusOK, map[string]any{"onboarding": onboardingProgress{
			State: newOnboardingState(), Steps: onboardingSteps, Guest: true,
		}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"onboarding": a.progressFor(uid)})
}

// progressFor reads the state and re-verifies activation. Verification runs on READ as well as on
// write because the three activation conditions are satisfied in other parts of the product — a user
// can finish their challenge review in Perspectives and never touch the guide again, and that user is
// activated.
func (a *onboardingAPI) progressFor(uid string) onboardingProgress {
	st := a.store.Get(uid)
	check := a.srv.verifyActivation(uid, st.ThesisID)
	if completed, next := a.completeIfActivated(uid, st, check); completed {
		st = next
	}
	return onboardingProgress{
		State:           st,
		Steps:           onboardingSteps,
		HasThesis:       check.HasThesis,
		HasEvidenceLink: check.HasEvidenceLink,
		HasChallenge:    check.HasChallenge,
	}
}

// completeIfActivated flips the record to completed exactly once, and only on the server's own proof.
// The state write is the durable transition that authorises `onboarding_completed` (§6.2).
func (a *onboardingAPI) completeIfActivated(uid string, st OnboardingState, check activationCheck) (bool, OnboardingState) {
	if check.Activation == nil || st.Status == onboardingCompleted {
		return false, st
	}
	now := time.Now().Unix()

	a.store.mu.Lock()
	cur := a.store.loadLocked(uid)
	if cur.Status == onboardingCompleted { // another request got there first
		out := *cur
		a.store.mu.Unlock()
		return true, out
	}
	prior := *cur
	prior.CompletedSteps = append([]string{}, cur.CompletedSteps...)
	if cur.Activation != nil {
		activation := *cur.Activation
		prior.Activation = &activation
	}
	act := *check.Activation
	act.VerifiedAt = now
	cur.Status = onboardingCompleted
	cur.CompletedAt = now
	cur.LastActiveAt = now
	cur.Activation = &act
	if cur.StartedAt == 0 {
		cur.StartedAt = now
	}
	if cur.ThesisID == "" {
		cur.ThesisID = act.ThesisID
	}
	cur.CompletedSteps = mergeStep(cur.CompletedSteps, stepChallengeReview)
	if err := a.store.persistLocked(uid); err != nil {
		*cur = prior
		a.store.mu.Unlock()
		return false, st
	}
	out := *cur
	out.CompletedSteps = append([]string{}, cur.CompletedSteps...)
	a.store.mu.Unlock()

	analytics.emit(eventInput{
		Event:         evOnboardingCompleted,
		UID:           uid,
		TransitionKey: onboardingCompleted,
		ObjectID:      act.ThesisID,
		ThesisID:      act.ThesisID,
		Ticker:        out.Ticker,
		Task:          act.Task,
		DataState:     act.DataState,
	})
	return true, out
}

// onboardingReq is the ENTIRE accepted write surface. `status` is absent by design: a client cannot
// declare itself activated.
type onboardingReq struct {
	Step      string `json:"step"`
	Ticker    string `json:"ticker"`
	ThesisID  string `json:"thesisId"`
	Level     string `json:"level"`
	Dismissed *bool  `json:"dismissed"`

	// SessionID identifies a GUEST browser session so the two guest-visible events can be attributed
	// to a stable hash. It is hashed with the analytics salt and never stored anywhere.
	SessionID string `json:"sessionId"`

	// Resumed lets a guest client report that it is continuing an earlier session. Only a guest may
	// set it — for a signed-in user the server derives resume from the stored record's own timestamps,
	// which is the authoritative source.
	Resumed bool `json:"resumed"`
}

func (a *onboardingAPI) handleProgress(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	var req onboardingReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil && err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}
	step := strings.TrimSpace(req.Step)
	if step != "" && !validOnboardingStep[step] {
		writeAPIError(w, badField("step", "step must be one of the guided research steps"))
		return
	}

	// ---- guest: events only, no durable record (there is no guest partition) ----
	if uid == "" {
		session := opaqueID(strings.TrimSpace(req.SessionID))
		if session == "" {
			writeAPIError(w, badField("sessionId", "sessionId is required while signed out"))
			return
		}
		event := evOnboardingStarted
		if req.Resumed {
			event = evOnboardingResumed
		}
		analytics.emit(eventInput{
			Event:         event,
			GuestSession:  session,
			TransitionKey: firstNonEmpty(step, stepChooseCompany),
			ObjectID:      analytics.hashIdentity(session),
			Ticker:        req.Ticker,
		})
		st := newOnboardingState()
		st.Status = onboardingInProgress
		st.Step = firstNonEmpty(step, stepChooseCompany)
		st.Ticker = strings.ToUpper(strings.TrimSpace(req.Ticker))
		writeJSON(w, http.StatusOK, map[string]any{"onboarding": onboardingProgress{
			State: st, Steps: onboardingSteps, Guest: true,
		}})
		return
	}

	now := time.Now().Unix()
	var emitEvent string
	var transitionKey string

	a.store.mu.Lock()
	if a.store.broken[uid] {
		a.store.mu.Unlock()
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "stored onboarding progress could not be read; refusing to overwrite it",
			"code":  "store_unreadable",
		})
		return
	}
	st := a.store.loadLocked(uid)
	prior := *st
	prior.CompletedSteps = append([]string{}, st.CompletedSteps...)
	if st.Activation != nil {
		activation := *st.Activation
		prior.Activation = &activation
	}

	switch {
	case st.Status == onboardingNotStarted:
		// §6.2: onboarding_started fires on the FIRST onboarding-state write, for this identity.
		st.Status = onboardingInProgress
		st.StartedAt = now
		emitEvent, transitionKey = evOnboardingStarted, onboardingInProgress
	case st.Status == onboardingInProgress && st.LastActiveAt > 0 && now-st.LastActiveAt >= onboardingResumeGapSec:
		st.ResumeCount++
		emitEvent = evOnboardingResumed
		// The resume count is part of the key so a second genuine resume is a second event, while a
		// replayed request inside the same gap window de-duplicates.
		transitionKey = "resume:" + itoa(st.ResumeCount)
	}

	if step != "" {
		st.Step = step
		st.CompletedSteps = mergeStep(st.CompletedSteps, step)
	}
	if t := analyticsTicker(req.Ticker); t != "" {
		st.Ticker = t
	}
	if id := opaqueID(strings.TrimSpace(req.ThesisID)); id != "" {
		st.ThesisID = id
	}
	if lvl := enumOrEmpty(strings.TrimSpace(req.Level), validAnalyticsLevel); lvl != "" {
		st.Level = lvl
	}
	if req.Dismissed != nil {
		st.Dismissed = *req.Dismissed
	}
	st.LastActiveAt = now
	if err := a.store.persistLocked(uid); err != nil {
		*st = prior
		a.store.mu.Unlock()
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "onboarding storage is unavailable", "code": "store_unavailable"})
		return
	}
	ticker := st.Ticker
	thesisID := st.ThesisID
	a.store.mu.Unlock()

	if emitEvent != "" {
		analytics.emit(eventInput{
			Event:         emitEvent,
			UID:           uid,
			TransitionKey: transitionKey,
			ObjectID:      analytics.hashIdentity(uid),
			Ticker:        ticker,
			ThesisID:      thesisID,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"onboarding": a.progressFor(uid)})
}

// ---- small helpers ----

func mergeStep(done []string, step string) []string {
	for _, s := range done {
		if s == step {
			return done
		}
	}
	return append(done, step)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// itoa avoids importing strconv for one call in this file.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}
