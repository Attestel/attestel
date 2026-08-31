package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// User is the persisted record. PasswordHash is an encoded PBKDF2 value; plaintext never rests.
type User struct {
	ID           string `json:"id"`
	Email        string `json:"email"`
	PasswordHash string `json:"passwordHash"`
	GoogleID     string `json:"googleId,omitempty"`
	CreatedAt    int64  `json:"createdAt"`
}

type publicUser struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

func (u User) public() publicUser { return publicUser{ID: u.ID, Email: u.Email} }

const minPasswordLen = 8

var emailRE = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

var (
	errDuplicateEmail = errors.New("an account with this email already exists")
	errInvalidEmail   = errors.New("please enter a valid email address")
	errShortPassword  = errors.New("password must be at least 8 characters")
)

type Store struct {
	dir   string
	mu    sync.Mutex
	users []User
	db    *sql.DB
	table string
}

// openStore is the explicit file fallback used by local tests.
func openStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) path() string { return filepath.Join(s.dir, "users.json") }

func (s *Store) load() error {
	b, err := os.ReadFile(s.path())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, &s.users)
}

func (s *Store) persistLocked() error {
	b, err := json.MarshalIndent(s.users, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path())
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "u_" + hex.EncodeToString(b)
}

func normalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

func (s *Store) CreateUser(email, password string) (User, error) {
	email = normalizeEmail(email)
	if !emailRE.MatchString(email) {
		return User{}, errInvalidEmail
	}
	if len(password) < minPasswordLen {
		return User{}, errShortPassword
	}
	hash, err := hashPassword(password)
	if err != nil {
		return User{}, err
	}
	u := User{ID: newID(), Email: email, PasswordHash: hash, CreatedAt: time.Now().Unix()}
	if s.db != nil {
		return s.createUserPostgres(u)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.users {
		if s.users[i].Email == email {
			return User{}, errDuplicateEmail
		}
	}
	before := append([]User(nil), s.users...)
	s.users = append(s.users, u)
	if err := s.persistLocked(); err != nil {
		s.users = before
		return User{}, err
	}
	return u, nil
}

// LookupByEmail exposes storage failures; login must distinguish "not found" from "database down".
func (s *Store) LookupByEmail(email string) (User, bool, error) {
	email = normalizeEmail(email)
	if s.db != nil {
		return s.findByEmailPostgres(email)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.users {
		if s.users[i].Email == email {
			return s.users[i], true, nil
		}
	}
	return User{}, false, nil
}

// FindByEmail remains for package compatibility; request handlers use LookupByEmail so errors are
// never swallowed on the production path.
func (s *Store) FindByEmail(email string) (User, bool) {
	u, ok, _ := s.LookupByEmail(email)
	return u, ok
}

func (s *Store) FindOrLinkGoogleUser(email, sub, name string) (User, error) {
	_ = name
	email = normalizeEmail(email)
	if !emailRE.MatchString(email) {
		return User{}, errInvalidEmail
	}
	if s.db != nil {
		return s.findOrLinkGooglePostgres(email, sub)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.users {
		if s.users[i].Email == email {
			if s.users[i].GoogleID == "" {
				before := s.users[i]
				s.users[i].GoogleID = sub
				if err := s.persistLocked(); err != nil {
					s.users[i] = before
					return User{}, err
				}
			}
			return s.users[i], nil
		}
	}
	u := User{ID: newID(), Email: email, GoogleID: sub, CreatedAt: time.Now().Unix()}
	s.users = append(s.users, u)
	if err := s.persistLocked(); err != nil {
		s.users = s.users[:len(s.users)-1]
		return User{}, err
	}
	return u, nil
}

func (s *Store) LookupByID(id string) (User, bool, error) {
	if s.db != nil {
		return s.getByIDPostgres(id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.users {
		if s.users[i].ID == id {
			return s.users[i], true, nil
		}
	}
	return User{}, false, nil
}

func (s *Store) GetByID(id string) (User, bool) {
	u, ok, _ := s.LookupByID(id)
	return u, ok
}
