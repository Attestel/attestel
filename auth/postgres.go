package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

var authIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func openPostgresStores(dir, databaseURL, schema string) (*Store, *SettingsStore, error) {
	if !authIdentifier.MatchString(schema) {
		return nil, nil, fmt.Errorf("AUTH_DATABASE_SCHEMA must be a PostgreSQL identifier")
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, nil, err
	}
	q := `"` + schema + `"`
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	statements := []string{
		`CREATE SCHEMA IF NOT EXISTS ` + q,
		`CREATE TABLE IF NOT EXISTS ` + q + `.schema_migrations (
			version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS ` + q + `.users (
			id TEXT PRIMARY KEY, email TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL,
			google_id TEXT NOT NULL DEFAULT '', created_at BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS ` + q + `.settings (
			user_id TEXT PRIMARY KEY, data JSONB NOT NULL, updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS ` + q + `.legacy_imports (
			name TEXT PRIMARY KEY, imported_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`INSERT INTO ` + q + `.schema_migrations(version) VALUES ('001_users_settings') ON CONFLICT DO NOTHING`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			tx.Rollback()
			db.Close()
			return nil, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		db.Close()
		return nil, nil, err
	}
	users := &Store{dir: dir, db: db, table: q + `.users`}
	settings := &SettingsStore{dir: dir, db: db, table: q + `.settings`}
	if err := importAuthFiles(ctx, db, q, users.table, settings.table, dir); err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("import legacy auth files: %w", err)
	}
	return users, settings, nil
}

func importAuthFiles(ctx context.Context, db *sql.DB, schema, usersTable, settingsTable, dir string) error {
	var done bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM `+schema+`.legacy_imports WHERE name='auth-json-v1')`,
	).Scan(&done); err != nil || done {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if payload, err := os.ReadFile(filepath.Join(dir, "users.json")); err == nil {
		var users []User
		if err := json.Unmarshal(payload, &users); err != nil {
			return err
		}
		for _, user := range users {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO `+usersTable+`(id,email,password_hash,google_id,created_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`,
				user.ID, normalizeEmail(user.Email), user.PasswordHash, user.GoogleID, user.CreatedAt,
			); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if payload, err := os.ReadFile(filepath.Join(dir, "settings.json")); err == nil {
		var settings map[string]Settings
		if err := json.Unmarshal(payload, &settings); err != nil {
			return err
		}
		for uid, value := range settings {
			data, err := json.Marshal(value)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO `+settingsTable+`(user_id,data) VALUES($1,$2) ON CONFLICT DO NOTHING`, uid, data,
			); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO `+schema+`.legacy_imports(name) VALUES('auth-json-v1') ON CONFLICT DO NOTHING`,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (s *Store) createUserPostgres(user User) (User, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO `+s.table+`(id,email,password_hash,google_id,created_at) VALUES($1,$2,$3,$4,$5)`,
		user.ID, user.Email, user.PasswordHash, user.GoogleID, user.CreatedAt)
	if isUniqueViolation(err) {
		return User{}, errDuplicateEmail
	}
	return user, err
}

func scanUser(row *sql.Row) (User, bool, error) {
	var user User
	err := row.Scan(&user.ID, &user.Email, &user.PasswordHash, &user.GoogleID, &user.CreatedAt)
	if err == sql.ErrNoRows {
		return User{}, false, nil
	}
	return user, err == nil, err
}

func (s *Store) findByEmailPostgres(email string) (User, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return scanUser(s.db.QueryRowContext(ctx,
		`SELECT id,email,password_hash,google_id,created_at FROM `+s.table+` WHERE email=$1`, email))
}

func (s *Store) getByIDPostgres(id string) (User, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return scanUser(s.db.QueryRowContext(ctx,
		`SELECT id,email,password_hash,google_id,created_at FROM `+s.table+` WHERE id=$1`, id))
}

func (s *Store) findOrLinkGooglePostgres(email, sub string) (User, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	created := User{ID: newID(), Email: email, GoogleID: sub, CreatedAt: time.Now().Unix()}
	row := s.db.QueryRowContext(ctx,
		`INSERT INTO `+s.table+` AS existing (id,email,password_hash,google_id,created_at) VALUES($1,$2,'',$3,$4)
		 ON CONFLICT (email) DO UPDATE SET google_id=CASE
		 WHEN existing.google_id='' THEN excluded.google_id ELSE existing.google_id END
		 RETURNING id,email,password_hash,google_id,created_at`,
		created.ID, email, sub, created.CreatedAt)
	user, _, err := scanUser(row)
	return user, err
}

func (s *SettingsStore) readSettingsPostgres(uid string) (Settings, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var payload []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT data FROM `+s.table+` WHERE user_id=$1`, uid).Scan(&payload)
	if err == sql.ErrNoRows {
		return defaultSettings(), nil
	}
	if err != nil {
		return Settings{}, err
	}
	var value Settings
	if err := json.Unmarshal(payload, &value); err != nil {
		return Settings{}, err
	}
	return withDefaults(value), nil
}

func (s *SettingsStore) putSettingsPostgres(uid string, value Settings) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO `+s.table+`(user_id,data,updated_at) VALUES($1,$2,now())
		 ON CONFLICT (user_id) DO UPDATE SET data=excluded.data,updated_at=now()`, uid, payload)
	return err
}
