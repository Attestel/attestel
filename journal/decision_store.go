package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// decision_store.go — persistence for decision records and outcome reviews.
//
// Same discipline as trades.json / theses.json / evidence.json: plain JSON, one file per user,
// mutex-guarded, atomic (.tmp + rename), lazily loaded. Contract: PARALLEL_CONTRACTS.md §5.2/§5.4.
//
//	{TRADES_DIR}/{uid}/decisions.json -> [ DecisionRecord ]
//	{TRADES_DIR}/{uid}/outcomes.json  -> [ OutcomeReview ]
//
// Two files behind ONE mutex, for the same reason the evidence lane pairs records with links:
// DELETE /decisions/{id} cascades its outcome review (§5.7), and a cascade that removes the decision
// but leaves the review — or vice versa — would leave a review pointing at nothing. One lock makes
// that one atomic step.
//
// The refuse-to-clobber guard is inherited too: a file that EXISTS but does not parse latches an
// error, and every write then refuses with 503 rather than replacing a user's reasoning record with
// an empty array. Preserving unreadable research beats a clean start.

const (
	decisionsFile = "decisions.json"
	outcomesFile  = "outcomes.json"

	// Caps. A research log, not an archive — but far above any realistic hand-authored volume, since
	// every record here costs the user a written rationale.
	maxDecisionsPerUser = 5000
	maxOutcomesPerUser  = 5000
)

var (
	errDecisionUnreadable  = errors.New("stored decisions could not be read")
	errDecisionReservedUID = errors.New("reserved user partition")
)

type decisionStore struct {
	base      string
	mu        sync.Mutex
	decisions map[string][]DecisionRecord
	outcomes  map[string][]OutcomeReview
	loadErr   map[string]error
	docs      *documentRepository
}

func openDecisionStore(base string) (*decisionStore, error) {
	return openDecisionStoreWithRepository(base, nil)
}

func openDecisionStoreWithRepository(base string, docs *documentRepository) (*decisionStore, error) {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}
	return &decisionStore{
		base:      base,
		decisions: map[string][]DecisionRecord{},
		outcomes:  map[string][]OutcomeReview{},
		loadErr:   map[string]error{},
		docs:      docs,
	}, nil
}

func (s *decisionStore) path(uid, file string) string { return filepath.Join(s.base, uid, file) }

// loadDecisionsLocked / loadOutcomesLocked lazily read a user's collection. Caller must hold s.mu.
// loadJSON is the evidence lane's shared reader: a missing file is an empty list, an unparseable one
// latches an error.
func (s *decisionStore) loadDecisionsLocked(uid string) []DecisionRecord {
	if d, ok := s.decisions[uid]; ok {
		return d
	}
	list := []DecisionRecord{}
	var err error
	if s.docs != nil {
		var payload []byte
		payload, _, err = s.docs.load(uid, "decisions", s.path(uid, decisionsFile))
		if err == nil && len(payload) > 0 {
			err = json.Unmarshal(payload, &list)
		}
	} else {
		_, err = loadJSON(s.path(uid, decisionsFile), &list)
	}
	if err != nil {
		s.loadErr[uid+"/"+decisionsFile] = err
		list = []DecisionRecord{}
	}
	s.decisions[uid] = list
	return list
}

func (s *decisionStore) loadOutcomesLocked(uid string) []OutcomeReview {
	if o, ok := s.outcomes[uid]; ok {
		return o
	}
	list := []OutcomeReview{}
	var err error
	if s.docs != nil {
		var payload []byte
		payload, _, err = s.docs.load(uid, "outcomes", s.path(uid, outcomesFile))
		if err == nil && len(payload) > 0 {
			err = json.Unmarshal(payload, &list)
		}
	} else {
		_, err = loadJSON(s.path(uid, outcomesFile), &list)
	}
	if err != nil {
		s.loadErr[uid+"/"+outcomesFile] = err
		list = []OutcomeReview{}
	}
	s.outcomes[uid] = list
	return list
}

// writableLocked reports whether BOTH files are safe to overwrite — a cascade touches both, so a
// half-writable pair is not writable at all. Caller must hold s.mu.
func (s *decisionStore) writableLocked(uid string) bool {
	s.loadDecisionsLocked(uid)
	s.loadOutcomesLocked(uid)
	return s.loadErr[uid+"/"+decisionsFile] == nil && s.loadErr[uid+"/"+outcomesFile] == nil
}

func (s *decisionStore) persistLocked(uid, file string, v any) error {
	if s.docs != nil {
		collection := "decisions"
		if file == outcomesFile {
			collection = "outcomes"
		}
		if err := s.docs.save(uid, collection, v); err != nil {
			if file == decisionsFile {
				delete(s.decisions, uid)
			} else {
				delete(s.outcomes, uid)
			}
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Join(s.base, uid), 0o755); err != nil {
		return err
	}
	return writeJSONFileAtomic(s.path(uid, file), v)
}

// writeJSONFileAtomic marshals to a .tmp sibling and renames, so a crash mid-write cannot truncate
// an existing record file.
func writeJSONFileAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---- reads ----

// listDecisions returns a copy of the caller's decisions, newest DECIDED first. `decidedAt` is
// user-settable, so ties are broken by id to keep the order (and therefore pagination) deterministic.
func (s *decisionStore) listDecisions(uid string) []DecisionRecord {
	if uid == "" || reservedUID(uid) {
		return []DecisionRecord{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.loadDecisionsLocked(uid)
	out := make([]DecisionRecord, len(src))
	copy(out, src)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].DecidedAt != out[j].DecidedAt {
			return out[i].DecidedAt > out[j].DecidedAt
		}
		return out[i].ID > out[j].ID
	})
	return out
}

func (s *decisionStore) getDecision(uid, id string) (DecisionRecord, bool) {
	if uid == "" || reservedUID(uid) {
		return DecisionRecord{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.loadDecisionsLocked(uid) {
		if d.ID == id {
			return d, true
		}
	}
	return DecisionRecord{}, false
}

func (s *decisionStore) listOutcomes(uid string) []OutcomeReview {
	if uid == "" || reservedUID(uid) {
		return []OutcomeReview{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.loadOutcomesLocked(uid)
	out := make([]OutcomeReview, len(src))
	copy(out, src)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ReviewedAt != out[j].ReviewedAt {
			return out[i].ReviewedAt > out[j].ReviewedAt
		}
		return out[i].ID > out[j].ID
	})
	return out
}

func (s *decisionStore) getOutcome(uid, id string) (OutcomeReview, bool) {
	if uid == "" || reservedUID(uid) {
		return OutcomeReview{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.loadOutcomesLocked(uid) {
		if o.ID == id {
			return o, true
		}
	}
	return OutcomeReview{}, false
}

// ---- writes ----

func (s *decisionStore) addDecision(uid string, rec DecisionRecord) (DecisionRecord, error) {
	if reservedUID(uid) {
		return DecisionRecord{}, errDecisionReservedUID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writableLocked(uid) {
		return DecisionRecord{}, errDecisionUnreadable
	}
	list := append(s.loadDecisionsLocked(uid), rec)
	if len(list) > maxDecisionsPerUser {
		list = list[len(list)-maxDecisionsPerUser:]
	}
	s.decisions[uid] = list
	if err := s.persistLocked(uid, decisionsFile, list); err != nil {
		return DecisionRecord{}, err
	}
	return rec, nil
}

// linkTrade applies the ONLY mutable field on a decision record (§5.3: PATCH accepts tradeId only).
// `tradeMode` is copied at link time so a decision-level metric can group by mode without re-reading
// the trade — and so it stays correct even if the trade is later deleted.
func (s *decisionStore) linkTrade(uid, id string, tradeID *string, tradeMode string) (DecisionRecord, bool, error) {
	if reservedUID(uid) {
		return DecisionRecord{}, false, errDecisionReservedUID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writableLocked(uid) {
		return DecisionRecord{}, false, errDecisionUnreadable
	}
	list := s.loadDecisionsLocked(uid)
	for i := range list {
		if list[i].ID != id {
			continue
		}
		list[i].TradeID = tradeID
		if tradeID == nil {
			list[i].TradeMode = ""
		} else {
			list[i].TradeMode = tradeMode
		}
		s.decisions[uid] = list
		if err := s.persistLocked(uid, decisionsFile, list); err != nil {
			return DecisionRecord{}, false, err
		}
		return list[i], true, nil
	}
	return DecisionRecord{}, false, nil
}

// deleteDecision removes a decision AND its outcome review, atomically. A review of a decision that
// no longer exists is not history, it is a dangling reference (§5.7 "cascades its outcome review").
func (s *decisionStore) deleteDecision(uid, id string) (bool, int, error) {
	if reservedUID(uid) {
		return false, 0, errDecisionReservedUID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writableLocked(uid) {
		return false, 0, errDecisionUnreadable
	}
	list := s.loadDecisionsLocked(uid)
	idx := -1
	for i := range list {
		if list[i].ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		return false, 0, nil
	}
	nextDecisions := append(list[:idx:idx], list[idx+1:]...)
	kept := make([]OutcomeReview, 0, len(s.loadOutcomesLocked(uid)))
	cascaded := 0
	for _, o := range s.loadOutcomesLocked(uid) {
		if o.DecisionID == id {
			cascaded++
			continue
		}
		kept = append(kept, o)
	}
	if s.docs != nil {
		values := map[string]any{"decisions": nextDecisions}
		if cascaded > 0 {
			values["outcomes"] = kept
		}
		if err := s.docs.saveMany(uid, values); err != nil {
			delete(s.decisions, uid)
			delete(s.outcomes, uid)
			return false, 0, err
		}
		s.decisions[uid] = nextDecisions
		if cascaded > 0 {
			s.outcomes[uid] = kept
		}
		return true, cascaded, nil
	}
	s.decisions[uid] = nextDecisions
	if err := s.persistLocked(uid, decisionsFile, nextDecisions); err != nil {
		return false, 0, err
	}
	if cascaded > 0 {
		s.outcomes[uid] = kept
		if err := s.persistLocked(uid, outcomesFile, kept); err != nil {
			return false, 0, err
		}
	}
	return true, cascaded, nil
}

// addOutcome enforces "one outcome review per decision" inside the lock, so two concurrent reviews
// of the same decision cannot both win. Returns (review, conflict): conflict means one already
// exists and the caller answers 409 outcome_exists (§5.7).
//
// It never touches decisions.json. That is the structural half of §5.4's rule that an outcome review
// can never modify the decision it reviews — there is no code path from here into that file.
func (s *decisionStore) addOutcome(uid string, rec OutcomeReview) (OutcomeReview, bool, error) {
	if reservedUID(uid) {
		return OutcomeReview{}, false, errDecisionReservedUID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writableLocked(uid) {
		return OutcomeReview{}, false, errDecisionUnreadable
	}
	list := s.loadOutcomesLocked(uid)
	for _, o := range list {
		if o.DecisionID == rec.DecisionID {
			return o, true, nil
		}
	}
	list = append(list, rec)
	if len(list) > maxOutcomesPerUser {
		list = list[len(list)-maxOutcomesPerUser:]
	}
	s.outcomes[uid] = list
	if err := s.persistLocked(uid, outcomesFile, list); err != nil {
		return OutcomeReview{}, false, err
	}
	return rec, false, nil
}

// patchOutcome applies the four user-authored fields §5.4 allows a later revision to change. A nil
// pointer means "absent, leave untouched". `marketOutcome` is deliberately NOT patchable: it is
// derived server-side from the linked trade, never asserted by a client.
func (s *decisionStore) patchOutcome(uid, id string, p outcomePatchValues) (OutcomeReview, bool, error) {
	if reservedUID(uid) {
		return OutcomeReview{}, false, errDecisionReservedUID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writableLocked(uid) {
		return OutcomeReview{}, false, errDecisionUnreadable
	}
	list := s.loadOutcomesLocked(uid)
	for i := range list {
		if list[i].ID != id {
			continue
		}
		if p.ObservedOutcome != nil {
			list[i].ObservedOutcome = *p.ObservedOutcome
		}
		if p.ReasoningQuality != nil {
			list[i].ReasoningQuality = p.ReasoningQuality
		}
		if p.ProcessLessons != nil {
			list[i].ProcessLessons = *p.ProcessLessons
		}
		if p.Retrospective != nil {
			list[i].Retrospective = *p.Retrospective
		}
		list[i].UpdatedAt = p.Now
		s.outcomes[uid] = list
		if err := s.persistLocked(uid, outcomesFile, list); err != nil {
			return OutcomeReview{}, false, err
		}
		return list[i], true, nil
	}
	return OutcomeReview{}, false, nil
}

// outcomeFor returns the review of one decision, if any.
func (s *decisionStore) outcomeFor(uid, decisionID string) (OutcomeReview, bool) {
	if uid == "" || reservedUID(uid) {
		return OutcomeReview{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.loadOutcomesLocked(uid) {
		if o.DecisionID == decisionID {
			return o, true
		}
	}
	return OutcomeReview{}, false
}
