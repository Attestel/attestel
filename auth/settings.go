package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// settings.go — per-user preferences store, mutex-guarded plain JSON (mirrors store.go). It lives
// beside users.json under the SAME UsersDir volume, so it needs no new env / volume. Each user gets a
// Settings record controlling the directional signal's client-side auto train+refresh cadence plus
// its allow-short / default-horizon defaults.
//
// These are PREFERENCES ONLY — nothing here executes a trade (invariant #2). "allow short" merely
// permits the walk-forward-validated model to emit a SHORT bias; it changes nothing about the
// no-execution rule. The auto-train LOOP itself runs client-side while the app is open (a server-side
// background scheduler is deliberately deferred — see GAPS.md).

// Settings is one user's saved preferences. Zero value ~ auto-train off (safe default).
type Settings struct {
	TrainIntervalMinutes int  `json:"trainIntervalMinutes"` // {0,15,30,60,240,1440}; 0 = off
	AllowShort           bool `json:"allowShort"`
	DefaultHorizon       int  `json:"defaultHorizon"` // {3,5,10}
	// BrowserNotifications is the user's PREFERENCE to receive desktop notifications for new alert
	// events. It is only the preference — the OS-level permission is a separate browser grant the
	// frontend must obtain (Notification.requestPermission). Any bool is valid.
	BrowserNotifications bool `json:"browserNotifications"`
	// Beginner-onboarding layer (docs/BEGINNER.md). PREFERENCES ONLY — none of these executes a trade
	// or moves money (invariant #2). ExperienceLevel/AckRealityCheck are stored here now but surfaced
	// later (beginner mode / reality-check first-run); AccountEquity + DefaultRiskPct feed the pure
	// client-side position sizer that caps risk per trade at the user's chosen %.
	ExperienceLevel string  `json:"experienceLevel"` // "beginner" | "standard"
	AckRealityCheck bool    `json:"ackRealityCheck"`
	AccountEquity   float64 `json:"accountEquity"`  // >= 0; 0 = unset (the sizer prompts for it)
	DefaultRiskPct  float64 `json:"defaultRiskPct"` // [0.1, 2] — the 1–2% convention; 2 is the hard cap
	ActiveTicker    string  `json:"activeTicker"`   // the company context restored on refresh / sign-in
}

// defaultSettings — what a new / never-saved user gets: auto-train off, long-only, horizon 5,
// desktop notifications off, beginner experience, reality-check not yet acknowledged, equity unset,
// default risk 1% per trade.
func defaultSettings() Settings {
	return Settings{
		TrainIntervalMinutes: 0, AllowShort: false, DefaultHorizon: 5, BrowserNotifications: false,
		ExperienceLevel: "beginner", AckRealityCheck: false, AccountEquity: 0, DefaultRiskPct: 1,
		ActiveTicker: "NVDA",
	}
}

// Allowed value sets, validated on every write so a bad client can't persist junk (or a punishing
// sub-15m cadence — training is a heavy walk-forward backtest, hence bounded presets not raw seconds).
var (
	allowedIntervals = map[int]bool{0: true, 15: true, 30: true, 60: true, 240: true, 1440: true}
	allowedHorizons  = map[int]bool{3: true, 5: true, 10: true}
	allowedLevels    = map[string]bool{"beginner": true, "standard": true}
)

var errInvalidSettings = errors.New("invalid settings values")

// withDefaults backfills the beginner-layer fields for records written before they existed (and for a
// partial PUT), so an old settings.json loads with the documented defaults instead of Go zero values:
// an empty experienceLevel becomes "beginner" and a zero defaultRiskPct becomes 1% (a validated write
// can never store 0 — validateSettings rejects it — so this only rescues pre-existing records).
func withDefaults(s Settings) Settings {
	if s.ExperienceLevel == "" {
		s.ExperienceLevel = "beginner"
	}
	if s.DefaultRiskPct == 0 {
		s.DefaultRiskPct = 1
	}
	if strings.TrimSpace(s.ActiveTicker) == "" {
		s.ActiveTicker = "NVDA"
	} else {
		s.ActiveTicker = strings.ToUpper(strings.TrimSpace(s.ActiveTicker))
	}
	return s
}

// validateSettings enforces the bounded presets. Returns errInvalidSettings on any out-of-set value.
// An empty experienceLevel is tolerated (normalized to "beginner" on read) but any other non-allowed
// string is rejected; accountEquity must be >= 0 and defaultRiskPct within the [0.1, 2] risk cap.
func validateSettings(s Settings) error {
	if !allowedIntervals[s.TrainIntervalMinutes] || !allowedHorizons[s.DefaultHorizon] {
		return errInvalidSettings
	}
	if s.ExperienceLevel != "" && !allowedLevels[s.ExperienceLevel] {
		return errInvalidSettings
	}
	if s.AccountEquity < 0 {
		return errInvalidSettings
	}
	if s.DefaultRiskPct < 0.1 || s.DefaultRiskPct > 2 {
		return errInvalidSettings
	}
	if !validTickerSymbol(s.ActiveTicker) {
		return errInvalidSettings
	}
	return nil
}

func validTickerSymbol(symbol string) bool {
	if len(symbol) < 1 || len(symbol) > 12 {
		return false
	}
	for _, r := range symbol {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '.' && r != '-' {
			return false
		}
	}
	return true
}

type SettingsStore struct {
	dir   string
	mu    sync.Mutex
	byID  map[string]Settings
	db    *sql.DB
	table string
}

func openSettingsStore(dir string) (*SettingsStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &SettingsStore{dir: dir, byID: map[string]Settings{}}
	s.load()
	return s, nil
}

func (s *SettingsStore) path() string { return filepath.Join(s.dir, "settings.json") }

func (s *SettingsStore) load() {
	b, err := os.ReadFile(s.path())
	if err != nil {
		return // no file yet -> empty map, everyone gets defaults
	}
	var m map[string]Settings
	if json.Unmarshal(b, &m) == nil && m != nil {
		s.byID = m
	}
}

// persistLocked writes settings.json via temp file + rename (atomic). Caller must hold s.mu.
func (s *SettingsStore) persistLocked() error {
	b, err := json.MarshalIndent(s.byID, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path())
}

// GetSettings returns the user's saved settings, or defaults if they have never saved. Stored records
// are run through withDefaults so ones written before the beginner-layer fields existed come back with
// the documented defaults rather than Go zero values.
func (s *SettingsStore) GetSettings(uid string) Settings {
	value, _ := s.ReadSettings(uid)
	return value
}

// ReadSettings is the request-path form: unlike GetSettings it surfaces database/read failures.
func (s *SettingsStore) ReadSettings(uid string) (Settings, error) {
	if s.db != nil {
		return s.readSettingsPostgres(uid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.byID[uid]; ok {
		return withDefaults(v), nil
	}
	return defaultSettings(), nil
}

// PutSettings validates then persists the user's settings. Rejects out-of-set values; normalizes an
// empty experienceLevel to the default before persisting so stored records stay complete.
func (s *SettingsStore) PutSettings(uid string, in Settings) error {
	// A stale/rolling frontend does not know activeTicker yet. Preserve the user's current company
	// instead of silently resetting it to NVDA when that client saves an unrelated preference.
	if strings.TrimSpace(in.ActiveTicker) == "" {
		current, err := s.ReadSettings(uid)
		if err != nil {
			return err
		}
		in.ActiveTicker = current.ActiveTicker
	}
	in = withDefaults(in)
	if err := validateSettings(in); err != nil {
		return err
	}
	if s.db != nil {
		return s.putSettingsPostgres(uid, in)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	before, existed := s.byID[uid]
	s.byID[uid] = in
	if err := s.persistLocked(); err != nil {
		if existed {
			s.byID[uid] = before
		} else {
			delete(s.byID, uid)
		}
		return err
	}
	return nil
}
