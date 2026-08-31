package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// evidence_store.go — persistence for evidence records and thesis↔evidence links.
//
// Same pattern as trades.json / theses.json: plain JSON, one file per user, mutex-guarded, written
// atomically (.tmp + rename), lazily loaded per user. Contract: PARALLEL_CONTRACTS.md §2.1.
//
//	{TRADES_DIR}/{uid}/evidence.json        -> [ EvidenceRecord ]
//	{TRADES_DIR}/{uid}/evidence_links.json  -> [ EvidenceLink ]
//
// Two files, and ONE mutex covering both. Two files because unlinking must never touch the evidence
// record, a link is queryable from both ends, and — during Wave 2 — only the thesis lane may write
// theses.json, so a link stored on the thesis would put two lanes in one file. One mutex because a
// forced delete has to remove a record AND its links or neither: with a single lock that is one
// atomic step, no cross-store ordering to get wrong.
//
// Retention (D-06): a record lives until the user deletes it or the account is deleted. Account
// deletion is `rm -rf {TRADES_DIR}/{uid}/` — which is the whole reason evidence is co-located with
// the user's other records rather than in a store of its own. No account-deletion endpoint exists in
// the tree yet; this layout is what makes one satisfiable in a single operation when it is built.

const (
	evidenceFile = "evidence.json"
	linksFile    = "evidence_links.json"
)

// evidenceStore holds both per-user collections behind one mutex.
//
// loadErr is the refuse-to-clobber guard: if a user's file EXISTS but does not parse, we remember
// that and refuse to write over it (503 store_unreadable) instead of silently replacing hand-edited
// or half-written research with an empty array. Preserving unreadable user data beats a clean start.
type evidenceStore struct {
	base    string
	mu      sync.Mutex
	records map[string][]EvidenceRecord
	links   map[string][]EvidenceLink
	loadErr map[string]error
	docs    *documentRepository
}

func openEvidenceStore(base string) (*evidenceStore, error) {
	return openEvidenceStoreWithRepository(base, nil)
}

func openEvidenceStoreWithRepository(base string, docs *documentRepository) (*evidenceStore, error) {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}
	return &evidenceStore{
		base:    base,
		records: map[string][]EvidenceRecord{},
		links:   map[string][]EvidenceLink{},
		loadErr: map[string]error{},
		docs:    docs,
	}, nil
}

func (s *evidenceStore) path(uid, file string) string { return filepath.Join(s.base, uid, file) }

// reservedUID rejects the `_`-prefixed buckets (`_legacy`, `_personas`, `_analytics`). A session uid
// can never be one of these, so this only ever fires on a bug — but D-23 makes `_legacy` read-never
// and write-never, and the cheapest way to keep that true is to make it unreachable by construction.
func reservedUID(uid string) bool { return strings.HasPrefix(uid, "_") }

// loadJSON reads one file into out. Returns (found, err): a missing file is (false, nil) — a user
// with no evidence yet is normal. A present-but-unparseable file is (true, err) and latches.
func loadJSON(path string, out any) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return true, err
	}
	return true, nil
}

// loadRecordsLocked / loadLinksLocked lazily read a user's collection. Caller must hold s.mu.
func (s *evidenceStore) loadRecordsLocked(uid string) []EvidenceRecord {
	if r, ok := s.records[uid]; ok {
		return r
	}
	recs := []EvidenceRecord{}
	var err error
	if s.docs != nil {
		var payload []byte
		payload, _, err = s.docs.load(uid, "evidence", s.path(uid, evidenceFile))
		if err == nil && len(payload) > 0 {
			err = json.Unmarshal(payload, &recs)
		}
	} else {
		_, err = loadJSON(s.path(uid, evidenceFile), &recs)
	}
	if err != nil {
		s.loadErr[uid+"/"+evidenceFile] = err
		recs = []EvidenceRecord{}
	}
	s.records[uid] = recs
	return recs
}

func (s *evidenceStore) loadLinksLocked(uid string) []EvidenceLink {
	if l, ok := s.links[uid]; ok {
		return l
	}
	links := []EvidenceLink{}
	var err error
	if s.docs != nil {
		var payload []byte
		payload, _, err = s.docs.load(uid, "evidence_links", s.path(uid, linksFile))
		if err == nil && len(payload) > 0 {
			err = json.Unmarshal(payload, &links)
		}
	} else {
		_, err = loadJSON(s.path(uid, linksFile), &links)
	}
	if err != nil {
		s.loadErr[uid+"/"+linksFile] = err
		links = []EvidenceLink{}
	}
	s.links[uid] = links
	return links
}

// writable reports whether both of a user's files are safe to overwrite. Caller must hold s.mu.
func (s *evidenceStore) writableLocked(uid string) bool {
	s.loadRecordsLocked(uid)
	s.loadLinksLocked(uid)
	return s.loadErr[uid+"/"+evidenceFile] == nil && s.loadErr[uid+"/"+linksFile] == nil
}

// persistLocked writes one collection via temp file + rename. Caller must hold s.mu.
func (s *evidenceStore) persistLocked(uid, file string, v any) error {
	if s.docs != nil {
		collection := "evidence"
		if file == linksFile {
			collection = "evidence_links"
		}
		if err := s.docs.save(uid, collection, v); err != nil {
			if file == evidenceFile {
				delete(s.records, uid)
			} else {
				delete(s.links, uid)
			}
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Join(s.base, uid), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(uid, file) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(uid, file))
}

// ---- reads ----

// listRecords returns a copy of the user's records, newest capture first. `capturedAt` ties are
// broken by id so the order — and therefore cursor pagination — is deterministic.
func (s *evidenceStore) listRecords(uid string) []EvidenceRecord {
	if reservedUID(uid) {
		return []EvidenceRecord{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.loadRecordsLocked(uid)
	out := make([]EvidenceRecord, len(src))
	copy(out, src)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CapturedAt != out[j].CapturedAt {
			return out[i].CapturedAt > out[j].CapturedAt
		}
		return out[i].ID > out[j].ID
	})
	return out
}

func (s *evidenceStore) getRecord(uid, id string) (EvidenceRecord, bool) {
	if reservedUID(uid) {
		return EvidenceRecord{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.loadRecordsLocked(uid) {
		if r.ID == id {
			return r, true
		}
	}
	return EvidenceRecord{}, false
}

// listLinks returns a copy of the user's links, newest link first.
func (s *evidenceStore) listLinks(uid string) []EvidenceLink {
	if reservedUID(uid) {
		return []EvidenceLink{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.loadLinksLocked(uid)
	out := make([]EvidenceLink, len(src))
	copy(out, src)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].LinkedAt != out[j].LinkedAt {
			return out[i].LinkedAt > out[j].LinkedAt
		}
		return out[i].ID > out[j].ID
	})
	return out
}

func (s *evidenceStore) getLink(uid, id string) (EvidenceLink, bool) {
	if reservedUID(uid) {
		return EvidenceLink{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.loadLinksLocked(uid) {
		if l.ID == id {
			return l, true
		}
	}
	return EvidenceLink{}, false
}

// ---- writes ----

// addRecord appends a record, or returns the existing one when its dedupeKey already exists for
// this user (§2.5: a duplicate save is a 200 with the record we already have, never a second copy
// and never an error). The dedupe check and the append happen under one lock hold, so two
// simultaneous saves of the same headline cannot both win.
func (s *evidenceStore) addRecord(uid string, rec EvidenceRecord) (EvidenceRecord, bool, error) {
	if reservedUID(uid) {
		return EvidenceRecord{}, false, errEvidenceReservedUID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writableLocked(uid) {
		return EvidenceRecord{}, false, errEvidenceUnreadable
	}
	existing := s.loadRecordsLocked(uid)
	for _, r := range existing {
		if r.DedupeKey == rec.DedupeKey {
			return r, true, nil
		}
	}
	s.records[uid] = append(existing, rec)
	if err := s.persistLocked(uid, evidenceFile, s.records[uid]); err != nil {
		return EvidenceRecord{}, false, err
	}
	return rec, false, nil
}

// patchNote updates the ONE mutable field on a record (§2.2). Everything else is capture state: a
// changed upstream value is a new capture, not an edit to an old one.
func (s *evidenceStore) patchNote(uid, id, note string) (EvidenceRecord, bool, error) {
	if reservedUID(uid) {
		return EvidenceRecord{}, false, errEvidenceReservedUID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writableLocked(uid) {
		return EvidenceRecord{}, false, errEvidenceUnreadable
	}
	recs := s.loadRecordsLocked(uid)
	for i := range recs {
		if recs[i].ID == id {
			recs[i].Note = note
			recs[i].UpdatedAt = time.Now().Unix()
			s.records[uid] = recs
			if err := s.persistLocked(uid, evidenceFile, recs); err != nil {
				return EvidenceRecord{}, false, err
			}
			return recs[i], true, nil
		}
	}
	return EvidenceRecord{}, false, nil
}

// liveLinksFor returns the user's non-revoked links pointing at one evidence record. A revoked
// system-authored link is history, not an attachment, so it does not block a delete.
func (s *evidenceStore) liveLinksFor(uid, evidenceID string) []EvidenceLink {
	if reservedUID(uid) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []EvidenceLink
	for _, l := range s.loadLinksLocked(uid) {
		if l.EvidenceID == evidenceID && !l.Revoked {
			out = append(out, l)
		}
	}
	return out
}

// deleteRecord removes a record and, when force is set, its links too — atomically, because both
// collections are behind the same mutex. Returns (found, blockedBy): a non-empty blockedBy means
// live links exist and force was not set, so nothing was written.
func (s *evidenceStore) deleteRecord(uid, id string, force bool) (bool, []EvidenceLink, error) {
	if reservedUID(uid) {
		return false, nil, errEvidenceReservedUID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writableLocked(uid) {
		return false, nil, errEvidenceUnreadable
	}
	recs := s.loadRecordsLocked(uid)
	idx := -1
	for i := range recs {
		if recs[i].ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		return false, nil, nil
	}

	var live []EvidenceLink
	for _, l := range s.loadLinksLocked(uid) {
		if l.EvidenceID == id && !l.Revoked {
			live = append(live, l)
		}
	}
	if len(live) > 0 && !force {
		return true, live, nil
	}

	nextRecords := append(recs[:idx:idx], recs[idx+1:]...)
	nextLinks := s.loadLinksLocked(uid)
	if force {
		kept := make([]EvidenceLink, 0, len(s.links[uid]))
		for _, l := range s.loadLinksLocked(uid) {
			if l.EvidenceID != id {
				kept = append(kept, l)
			}
		}
		nextLinks = kept
	}
	if s.docs != nil {
		values := map[string]any{"evidence": nextRecords}
		if force {
			values["evidence_links"] = nextLinks
		}
		if err := s.docs.saveMany(uid, values); err != nil {
			delete(s.records, uid)
			delete(s.links, uid)
			return false, nil, err
		}
		s.records[uid] = nextRecords
		if force {
			s.links[uid] = nextLinks
		}
		return true, nil, nil
	}
	s.records[uid] = nextRecords
	if err := s.persistLocked(uid, evidenceFile, nextRecords); err != nil {
		return false, nil, err
	}
	if force {
		s.links[uid] = nextLinks
		if err := s.persistLocked(uid, linksFile, nextLinks); err != nil {
			return false, nil, err
		}
	}
	return true, nil, nil
}

// upsertLink creates a link, or updates the bearing/note of the existing (thesisId, evidenceId)
// pair (§2.6: that pair is unique per user; re-linking with a different bearing is a change of
// bearing, not a second link). Re-linking a revoked link un-revokes it — the user is re-asserting a
// connection the system had withdrawn, and refusing that would leave them unable to restore it.
// Returns (link, updated).
func (s *evidenceStore) upsertLink(uid string, in EvidenceLink) (EvidenceLink, bool, error) {
	if reservedUID(uid) {
		return EvidenceLink{}, false, errEvidenceReservedUID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writableLocked(uid) {
		return EvidenceLink{}, false, errEvidenceUnreadable
	}
	links := s.loadLinksLocked(uid)
	for i := range links {
		if links[i].ThesisID == in.ThesisID && links[i].EvidenceID == in.EvidenceID {
			links[i].Bearing = in.Bearing
			links[i].Note = in.Note
			links[i].ThesisItemID = in.ThesisItemID
			links[i].LinkedAt = in.LinkedAt
			links[i].Revoked = false
			links[i].RevokedAt = nil
			s.links[uid] = links
			if err := s.persistLocked(uid, linksFile, links); err != nil {
				return EvidenceLink{}, false, err
			}
			return links[i], true, nil
		}
	}
	s.links[uid] = append(links, in)
	if err := s.persistLocked(uid, linksFile, s.links[uid]); err != nil {
		return EvidenceLink{}, false, err
	}
	return in, false, nil
}

// patchLink applies the three mutable link fields. Setting revoked stamps revokedAt; clearing it
// wipes the stamp, so a revoked/restored link never carries a stale revocation time.
func (s *evidenceStore) patchLink(uid, id string, bearing, note *string, revoked *bool) (EvidenceLink, bool, error) {
	if reservedUID(uid) {
		return EvidenceLink{}, false, errEvidenceReservedUID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writableLocked(uid) {
		return EvidenceLink{}, false, errEvidenceUnreadable
	}
	links := s.loadLinksLocked(uid)
	for i := range links {
		if links[i].ID != id {
			continue
		}
		if bearing != nil {
			links[i].Bearing = *bearing
			links[i].LinkedAt = time.Now().Unix()
		}
		if note != nil {
			links[i].Note = *note
		}
		if revoked != nil {
			links[i].Revoked = *revoked
			if *revoked {
				now := time.Now().Unix()
				links[i].RevokedAt = &now
			} else {
				links[i].RevokedAt = nil
			}
		}
		s.links[uid] = links
		if err := s.persistLocked(uid, linksFile, links); err != nil {
			return EvidenceLink{}, false, err
		}
		return links[i], true, nil
	}
	return EvidenceLink{}, false, nil
}

// deleteLink removes a link and NOTHING else. The evidence record it pointed at is untouched —
// that separation is the whole reason links live in their own file (§2.6).
func (s *evidenceStore) deleteLink(uid, id string) (bool, error) {
	if reservedUID(uid) {
		return false, errEvidenceReservedUID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writableLocked(uid) {
		return false, errEvidenceUnreadable
	}
	links := s.loadLinksLocked(uid)
	for i := range links {
		if links[i].ID == id {
			s.links[uid] = append(links[:i:i], links[i+1:]...)
			if err := s.persistLocked(uid, linksFile, s.links[uid]); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}
