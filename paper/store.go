package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// store.go — the engine's persisted state (mutex-guarded JSON under DATA_DIR, mirroring the other
// services). One record per configured (ticker, timeframe, horizon): the position it currently
// holds, the bar it last acted on, and the last decision it made — so the engine survives restarts
// without ever acting twice on one bar (docs/PAPER_EXECUTION_CONTRACT.md §3.1).
//
// The trades themselves live in the JOURNAL service (the source of truth). This state is just the
// engine's bookkeeping: which side it believes it is on, and why it last did or did not move.

// Decision is one bar's outcome, kept whether or not anything happened. A refusal is a decision, and
// a decision nobody can see is indistinguishable from a service that is quietly doing nothing.
type Decision struct {
	At              string       `json:"at"`      // RFC3339, when the decision was reached
	Bar             string       `json:"bar"`     // the bar timestamp it was decided on ("" if none was read)
	BarUnix         int64        `json:"barUnix"` // comparable form of Bar
	From            string       `json:"from"`    // position before: "long" | "short" | "flat"
	Target          string       `json:"target"`  // target position: "long" | "short" | "flat" | "unknown"
	Action          string       `json:"action"`  // "open" | "close" | "flip" | "hold" | "none"
	Gate            string       `json:"gate"`    // the gate that refused, "" when nothing refused
	Reason          string       `json:"reason"`  // why, in words, always populated
	Gates           []gateResult `json:"gates"`   // every gate's verdict, for the status payload
	ModelVersion    string       `json:"modelVersion,omitempty"`
	StrategyVersion string       `json:"strategyVersion,omitempty"`
}

// DecisionEvent is one SETTLED completed-bar decision in an experiment generation. Transient
// transport failures do not belong here because the bar is deliberately retried; counting every
// poll attempt would turn an outage into a fictitious run of independent decisions.
type DecisionEvent struct {
	Generation int64    `json:"generation"`
	Config     string   `json:"config"`
	Ticker     string   `json:"ticker"`
	Timeframe  string   `json:"timeframe"`
	Horizon    int      `json:"horizon"`
	Decision   Decision `json:"decision"`
}

func (e DecisionEvent) key() string {
	return fmt.Sprintf("%d:%s:%d", e.Generation, e.Config, e.Decision.BarUnix)
}

// ConfigState is everything the engine remembers about one config.
type ConfigState struct {
	ConfigKey string `json:"configKey"`
	Ticker    string `json:"ticker"`
	Timeframe string `json:"timeframe"`
	Horizon   int    `json:"horizon"`

	// Current position. Side is "" when flat; TradeID is the OPEN journal trade backing it.
	Side       string  `json:"side"`
	TradeID    string  `json:"tradeId"`
	EntryPrice float64 `json:"entryPrice"`
	EntryTime  int64   `json:"entryTime"` // unix
	EntryBar   string  `json:"entryBar"`  // the bar the entry was decided on
	// Exact sizing evidence captured before the journal write. Recovery may replay only these
	// values; it must never recompute an old fill from today's equity or config count.
	EntryQty      float64 `json:"entryQty,omitempty"`
	EntryNotional float64 `json:"entryNotional,omitempty"`
	EntryCostBps  float64 `json:"entryCostBps,omitempty"`
	EntryN        int     `json:"entryN,omitempty"`
	EntryFillKind string  `json:"entryFillKind,omitempty"`
	// Exact immutable model lineage that opened the position. A later promotion must never rewrite
	// the provenance of an already-open fake-money trade.
	EntryModelVersion    string `json:"entryModelVersion,omitempty"`
	EntryStrategyVersion string `json:"entryStrategyVersion,omitempty"`

	// LEGACY DISPLAY ONLY. Positions opened before the gates existed could be opened on synthetic
	// data and merely marked; new opens are REJECTED instead (contract §4.1), so this is false on
	// everything the current engine writes.
	EntrySynthetic bool `json:"entrySynthetic"`

	// The last bar this config acted on. Advancing this is what makes the engine act at most once
	// per bar and idempotent across a restart.
	LastBarActedOn string `json:"lastBarActedOn"`
	LastBarUnix    int64  `json:"lastBarUnix"`

	LastDecision *Decision `json:"lastDecision"`
}

// Flat reports whether the config currently holds nothing.
func (s *ConfigState) Flat() bool { return s == nil || s.Side == "" }

// sideOf maps a persisted side to a target word ("" -> "flat").
func sideOf(side string) string {
	if side == "" {
		return "flat"
	}
	return side
}

// legacyOpen is the PRE-per-bar state shape (one open position per config, with a wall-clock
// `dueAt`). It is read once, migrated, and never written again — the position carries over and the
// next bar re-decides it, which is exactly what should happen to a position whose exit rule no
// longer exists.
type legacyOpen struct {
	TradeID        string  `json:"tradeId"`
	Ticker         string  `json:"ticker"`
	Timeframe      string  `json:"timeframe"`
	Horizon        int     `json:"horizon"`
	Side           string  `json:"side"`
	EntryPrice     float64 `json:"entryPrice"`
	EntryTime      int64   `json:"entryTime"`
	EntrySynthetic bool    `json:"entrySynthetic"`
}

type persisted struct {
	Configs []PaperCfg              `json:"configs"`
	States  map[string]*ConfigState `json:"states"`
	Open    map[string]*legacyOpen  `json:"open,omitempty"` // legacy, migration input only
	// File-fallback history. PostgreSQL stores the same events in paper.decision_events so the
	// engine-state document stays small in production.
	Decisions []DecisionEvent `json:"decisionEvents,omitempty"`
	// Fills the journal accepted and the ledger refused, kept until the ledger takes them. See
	// PendingBooking at the bottom of this file.
	Pending []PendingBooking `json:"pendingBookings,omitempty"`
	// File-fallback shadow evidence. PostgreSQL keeps these in dedicated append-only tables; the
	// fallback uses the same atomically-renamed document so zero-database development remains usable.
	ShadowObservations []ShadowObservation `json:"shadowObservations,omitempty"`
	ShadowBars         []ShadowBar         `json:"shadowBars,omitempty"`
	ShadowOutcomes     []ShadowOutcome     `json:"shadowOutcomes,omitempty"`
}

type Store struct {
	dir       string
	mu        sync.Mutex
	data      persisted
	lastErr   error
	shadowErr error
	db        *paperDatabase
}

func openStore(dir string, defaults []PaperCfg) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return initializeStore(&Store{dir: dir, data: persisted{States: map[string]*ConfigState{}}}, defaults)
}

func openStoreWithDatabase(dir string, defaults []PaperCfg, db *paperDatabase) (*Store, error) {
	return initializeStore(&Store{dir: dir, db: db, data: persisted{States: map[string]*ConfigState{}}}, defaults)
}

func initializeStore(s *Store, defaults []PaperCfg) (*Store, error) {
	loaded, err := s.load()
	if err != nil {
		return nil, err
	}
	if len(s.data.Configs) == 0 {
		s.data.Configs = defaults // seed from env on first run
		if !loaded || len(defaults) > 0 {
			if err := s.persistLocked(); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}

func (s *Store) path() string { return filepath.Join(s.dir, "state.json") }

func (s *Store) load() (bool, error) {
	var b []byte
	var err error
	if s.db != nil {
		var found bool
		b, found, _, err = s.db.loadDocument("engine_state")
		if err != nil {
			return false, fmt.Errorf("read paper state from PostgreSQL: %w", err)
		}
		if !found {
			return false, nil
		}
	} else {
		b, err = os.ReadFile(s.path())
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read paper state: %w", err)
	}
	var p persisted
	if err := json.Unmarshal(b, &p); err != nil {
		return false, fmt.Errorf("decode paper state: %w", err)
	}
	if p.States == nil {
		p.States = map[string]*ConfigState{}
	}
	// Migrate any pre-per-bar open positions. LastBarActedOn is left empty on purpose: the next
	// observed bar decides the position afresh, under the current rule.
	for key, o := range p.Open {
		if o == nil || p.States[key] != nil {
			continue
		}
		p.States[key] = &ConfigState{
			ConfigKey: key, Ticker: o.Ticker, Timeframe: o.Timeframe, Horizon: o.Horizon,
			Side: o.Side, TradeID: o.TradeID, EntryPrice: o.EntryPrice, EntryTime: o.EntryTime,
			EntrySynthetic: o.EntrySynthetic,
		}
	}
	p.Open = nil
	s.data = p
	return true, nil
}

// writeJSONAtomic makes the rename the commit point and fsyncs both the file and its directory.
// Returning the error is part of the execution contract: an in-memory decision is not durable.
type atomicWriteError struct {
	err       error
	committed bool
}

func (e *atomicWriteError) Error() string { return e.err.Error() }
func (e *atomicWriteError) Unwrap() error { return e.err }

func writeCommitted(err error) bool {
	var atomicErr *atomicWriteError
	return errors.As(err, &atomicErr) && atomicErr.committed
}

func writeJSONAtomic(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary %s: %w", filepath.Base(path), err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temporary %s: %w", filepath.Base(path), err)
	}
	if _, err := tmp.ReadFrom(bytes.NewReader(b)); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary %s: %w", filepath.Base(path), err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fsync temporary %s: %w", filepath.Base(path), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("commit %s: %w", filepath.Base(path), err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return &atomicWriteError{err: fmt.Errorf("open state directory for fsync: %w", err), committed: true}
	}
	err = d.Sync()
	closeErr := d.Close()
	if err != nil {
		return &atomicWriteError{err: fmt.Errorf("fsync state directory: %w", err), committed: true}
	}
	if closeErr != nil {
		return &atomicWriteError{err: fmt.Errorf("close state directory: %w", closeErr), committed: true}
	}
	return nil
}

func (s *Store) persistLocked() error {
	if s.db != nil {
		s.lastErr = s.db.saveDocument("engine_state", 0, s.data)
	} else {
		s.lastErr = writeJSONAtomic(s.path(), s.data)
	}
	return s.lastErr
}

// PersistenceError is the most recent commit failure. Reconciliation treats it as a blocking
// mismatch until a later successful commit proves the durable store writable again.
func (s *Store) PersistenceError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// ShadowError is separate from PersistenceError: losing experimental evidence must be visible, but
// it must never unlock, block, or otherwise change the official paper account's gate semantics.
func (s *Store) ShadowError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shadowErr
}

func (s *Store) Check(ctx context.Context) error {
	if s.db == nil {
		return nil
	}
	return s.db.db.PingContext(ctx)
}

func (s *Store) Configs() []PaperCfg {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PaperCfg, len(s.data.Configs))
	copy(out, s.data.Configs)
	return out
}

func (s *Store) SetConfigs(cfgs []PaperCfg) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.data.Configs
	s.data.Configs = cfgs
	if err := s.persistLocked(); err != nil {
		if !writeCommitted(err) {
			s.data.Configs = old
		}
		return err
	}
	return nil
}

// StateFor returns a COPY of a config's state, creating an empty (flat) one if it has never been
// seen. A copy so callers can't mutate persisted state without going through SaveState.
func (s *Store) StateFor(cfg PaperCfg) *ConfigState {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := cfg.Key()
	if st := s.data.States[key]; st != nil {
		cp := *st
		return &cp
	}
	return &ConfigState{ConfigKey: key, Ticker: cfg.Ticker, Timeframe: cfg.Timeframe, Horizon: cfg.Horizon}
}

// SaveState persists a config's state.
func (s *Store) SaveState(st *ConfigState) error {
	if st == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *st
	old, existed := s.data.States[st.ConfigKey]
	s.data.States[st.ConfigKey] = &cp
	if err := s.persistLocked(); err != nil {
		if !writeCommitted(err) {
			if existed {
				s.data.States[st.ConfigKey] = old
			} else {
				delete(s.data.States, st.ConfigKey)
			}
		}
		return err
	}
	return nil
}

// SaveDecision atomically persists a settled decision and the state containing its advanced bar
// cursor. PostgreSQL writes both records in one transaction. The file fallback keeps both in the
// same atomically-renamed state document; it is deliberately simpler because production evidence
// is PostgreSQL-owned.
func (s *Store) SaveDecision(st *ConfigState, event DecisionEvent) error {
	if st == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	cp := *st
	old, existed := s.data.States[st.ConfigKey]
	s.data.States[st.ConfigKey] = &cp
	oldDecisionLen := len(s.data.Decisions)

	if s.db != nil {
		s.lastErr = s.db.saveStateAndDecision(s.data, event)
	} else {
		duplicate := false
		for _, existing := range s.data.Decisions {
			if existing.key() == event.key() {
				duplicate = true
				break
			}
		}
		if !duplicate {
			s.data.Decisions = append(s.data.Decisions, event)
		}
		s.lastErr = writeJSONAtomic(s.path(), s.data)
	}
	if s.lastErr != nil && !writeCommitted(s.lastErr) {
		if existed {
			s.data.States[st.ConfigKey] = old
		} else {
			delete(s.data.States, st.ConfigKey)
		}
		s.data.Decisions = s.data.Decisions[:oldDecisionLen]
	}
	return s.lastErr
}

// DecisionEvents returns newest-first settled decisions for one experiment generation. A zero
// limit means all events; handlers cap what they return to the browser.
func (s *Store) DecisionEvents(generation int64, limit int) ([]DecisionEvent, error) {
	if s.db != nil {
		return s.db.decisionEvents(generation, limit)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]DecisionEvent, 0)
	for _, event := range s.data.Decisions {
		if event.Generation == generation {
			out = append(out, event)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Decision.BarUnix == out[j].Decision.BarUnix {
			return out[i].Decision.At > out[j].Decision.At
		}
		return out[i].Decision.BarUnix > out[j].Decision.BarUnix
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SaveShadowObservation preserves the first signal/quote for a config+bar. Duplicate retries are a
// successful no-op; replacing the first quote with a later one would introduce hindsight.
func (s *Store) SaveShadowObservation(observation ShadowObservation) error {
	if observation.ContractVersion == "" || observation.Config == "" || observation.SignalBarUnix <= 0 {
		return fmt.Errorf("invalid shadow observation key")
	}
	if s.db != nil {
		err := s.db.insertShadowObservation(observation)
		s.mu.Lock()
		s.shadowErr = err
		s.mu.Unlock()
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.data.ShadowObservations {
		if existing.key() == observation.key() {
			s.shadowErr = nil
			return nil
		}
	}
	s.data.ShadowObservations = append(s.data.ShadowObservations, observation)
	if err := writeJSONAtomic(s.path(), s.data); err != nil {
		s.data.ShadowObservations = s.data.ShadowObservations[:len(s.data.ShadowObservations)-1]
		s.shadowErr = err
		return err
	}
	s.shadowErr = nil
	return nil
}

// SaveShadowBars stores completed bars and settles every newly due H1/H3/H5/H10 outcome. Neither
// operation touches official paper state, journal trades, or ledger generations.
func (s *Store) SaveShadowBars(bars []ShadowBar, now time.Time) error {
	if len(bars) == 0 {
		return nil
	}
	if s.db != nil {
		barErr := s.db.insertShadowBars(bars)
		data, readErr := s.db.shadowDataset()
		var outcomeErr error
		if readErr == nil {
			outcomeErr = s.db.insertShadowOutcomes(settleShadowOutcomes(data, now))
		}
		err := joinShadowErrors(barErr, readErr, outcomeErr)
		s.mu.Lock()
		s.shadowErr = err
		s.mu.Unlock()
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	oldBars, oldOutcomes := len(s.data.ShadowBars), len(s.data.ShadowOutcomes)
	seen := map[string]bool{}
	for _, existing := range s.data.ShadowBars {
		seen[existing.key()] = true
	}
	for _, bar := range bars {
		if !seen[bar.key()] {
			s.data.ShadowBars = append(s.data.ShadowBars, bar)
			seen[bar.key()] = true
		}
	}
	data := shadowDataset{
		Observations: s.data.ShadowObservations, Bars: s.data.ShadowBars, Outcomes: s.data.ShadowOutcomes,
	}
	created := settleShadowOutcomes(data, now)
	if len(s.data.ShadowBars) == oldBars && len(created) == 0 {
		s.shadowErr = nil
		return nil
	}
	s.data.ShadowOutcomes = append(s.data.ShadowOutcomes, created...)
	if err := writeJSONAtomic(s.path(), s.data); err != nil {
		s.data.ShadowBars = s.data.ShadowBars[:oldBars]
		s.data.ShadowOutcomes = s.data.ShadowOutcomes[:oldOutcomes]
		s.shadowErr = err
		return err
	}
	s.shadowErr = nil
	return nil
}

// ShadowReport composes the independent, all-generations experimental evidence view.
func (s *Store) ShadowReport(recentLimit int) (shadowReport, error) {
	if s.db != nil {
		data, err := s.db.shadowDataset()
		storageErr := joinShadowErrors(err, s.ShadowError())
		return buildShadowReport(data, recentLimit, storageErr), err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data := shadowDataset{
		Observations: append([]ShadowObservation{}, s.data.ShadowObservations...),
		Bars:         append([]ShadowBar{}, s.data.ShadowBars...),
		Outcomes:     append([]ShadowOutcome{}, s.data.ShadowOutcomes...),
	}
	return buildShadowReport(data, recentLimit, s.shadowErr), nil
}

// States returns every remembered config state (copies), for the status/positions payloads.
func (s *Store) States() []ConfigState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ConfigState, 0, len(s.data.States))
	for _, st := range s.data.States {
		if st != nil {
			out = append(out, *st)
		}
	}
	return out
}

// OpenPositions returns the states that currently hold a nonzero position.
func (s *Store) OpenPositions() []ConfigState {
	out := make([]ConfigState, 0)
	for _, st := range s.States() {
		if st.Side != "" {
			out = append(out, st)
		}
	}
	return out
}

// Reset clears ALL engine bookkeeping — positions, bar cursors and decisions (used by /paper/reset,
// which also deletes the journal's paper trades).
func (s *Store) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	oldStates, oldPending := s.data.States, s.data.Pending
	s.data.States = map[string]*ConfigState{}
	// Pending bookings go too. They are owed against trades that no longer exist after a reset, and
	// retrying them into a fresh book would fabricate positions nobody took.
	s.data.Pending = nil
	if err := s.persistLocked(); err != nil {
		if !writeCommitted(err) {
			s.data.States, s.data.Pending = oldStates, oldPending
		}
		return err
	}
	return nil
}

// --- pending bookings (docs/PAPER_EXECUTION_CONTRACT.md §5.9) ------------------------------------
//
// A fill the JOURNAL accepted and the LEDGER refused is not a fill that did not happen. It happened:
// the position moved, the journal says so, and the book is the record that is behind. Before this
// existed the failure was logged, the log line claimed "/paper/status says so" — nothing compared
// the stores, so it did not — and the fill was simply lost, forever, with no retry.
//
// A PendingBooking is that fill, written down durably, so the next tick can try to book it again.
// It carries everything `Ledger.open`/`Ledger.close` needs, because a retry that had to re-derive
// its own inputs (the equity at the time, the config count, the validated cost) would book a
// DIFFERENT fill from the one that was intended.
//
// Retrying is safe because booking is IDEMPOTENT BY (trade id, kind): the ledger refuses a second
// fill for a pair it already holds (`errDuplicateFill`), so a crash between the ledger booking the
// fill and this record being cleared cannot double-book on the retry.
type PendingBooking struct {
	ConfigKey string  `json:"configKey"`
	Ticker    string  `json:"ticker"`
	Timeframe string  `json:"timeframe"`
	Horizon   int     `json:"horizon"`
	Kind      string  `json:"kind"`    // fillOpen | fillFlipOpen | fillClose | fillFlipClose
	Side      string  `json:"side"`    // the position this leg opened ("" on a close leg)
	TradeID   string  `json:"tradeId"` // the journal trade the fill belongs to
	Price     float64 `json:"price"`
	Bar       string  `json:"bar"`
	N         int     `json:"n"`       // the enabled-config count the 1/N slice was taken against
	CostBps   float64 `json:"costBps"` // the cost the model was VALIDATED under, never a new one
	Qty       float64 `json:"qty,omitempty"`
	Notional  float64 `json:"notional,omitempty"`
	Note      string  `json:"note,omitempty"`

	At        string `json:"at"`        // when the fill was attempted
	Attempts  int    `json:"attempts"`  // how many times booking has been tried
	LastError string `json:"lastError"` // why the most recent attempt failed
}

// cfg reconstructs the PaperCfg the booking belongs to, so a retry books against the same key even
// if the config has since been removed from the enabled list.
func (p PendingBooking) cfg() PaperCfg {
	return PaperCfg{Ticker: p.Ticker, Timeframe: p.Timeframe, Horizon: p.Horizon}
}

// ident is the idempotency key the ledger refuses duplicates on.
func (p PendingBooking) ident() string { return p.TradeID + "|" + p.Kind }

// AddPendingBooking records a fill the ledger would not accept. Idempotent on (trade id, kind): a
// second failure of the same fill updates the existing record rather than queuing it twice.
func (s *Store) AddPendingBooking(p PendingBooking) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.data.Pending {
		if existing.ident() == p.ident() {
			p.Attempts = existing.Attempts
			s.data.Pending[i] = p
			if err := s.persistLocked(); err != nil {
				if !writeCommitted(err) {
					s.data.Pending[i] = existing
				}
				return err
			}
			return nil
		}
	}
	s.data.Pending = append(s.data.Pending, p)
	if err := s.persistLocked(); err != nil {
		if !writeCommitted(err) {
			s.data.Pending = s.data.Pending[:len(s.data.Pending)-1]
		}
		return err
	}
	return nil
}

// PendingBookings returns copies of every unbooked fill still owed to the ledger.
func (s *Store) PendingBookings() []PendingBooking {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]PendingBooking(nil), s.data.Pending...)
}

// PendingBookingsFor returns the unbooked fills owed for one config.
func (s *Store) PendingBookingsFor(configKey string) []PendingBooking {
	out := make([]PendingBooking, 0)
	for _, p := range s.PendingBookings() {
		if p.ConfigKey == configKey {
			out = append(out, p)
		}
	}
	return out
}

// ResolvePendingBooking drops a booking that has been accepted (or that the ledger already holds).
func (s *Store) ResolvePendingBooking(ident string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := append([]PendingBooking(nil), s.data.Pending...)
	out := s.data.Pending[:0]
	for _, p := range s.data.Pending {
		if p.ident() != ident {
			out = append(out, p)
		}
	}
	s.data.Pending = append([]PendingBooking(nil), out...)
	if err := s.persistLocked(); err != nil {
		if !writeCommitted(err) {
			s.data.Pending = old
		}
		return err
	}
	return nil
}

// FailPendingBooking records that a retry was attempted and did not succeed. The record stays: a
// fill the book still owes is not resolved by giving up on it.
func (s *Store) FailPendingBooking(ident, failure string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Pending {
		if s.data.Pending[i].ident() == ident {
			old := s.data.Pending[i]
			s.data.Pending[i].Attempts++
			s.data.Pending[i].LastError = failure
			if err := s.persistLocked(); err != nil {
				if !writeCommitted(err) {
					s.data.Pending[i] = old
				}
				return err
			}
			return nil
		}
	}
	return nil
}
