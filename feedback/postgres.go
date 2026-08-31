package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var feedbackIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func openPostgresStore(dir, databaseURL, schema string) (*Store, error) {
	if !feedbackIdentifier.MatchString(schema) {
		return nil, fmt.Errorf("FEEDBACK_DATABASE_SCHEMA must be a PostgreSQL identifier")
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	q := `"` + schema + `"`
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		db.Close()
		return nil, err
	}
	statements := []string{
		`CREATE SCHEMA IF NOT EXISTS ` + q,
		`CREATE TABLE IF NOT EXISTS ` + q + `.schema_migrations(version TEXT PRIMARY KEY,applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS ` + q + `.feedback(id TEXT PRIMARY KEY,owner TEXT NOT NULL DEFAULT '',created_at BIGINT NOT NULL,data JSONB NOT NULL,updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE INDEX IF NOT EXISTS feedback_owner_created_idx ON ` + q + `.feedback(owner,created_at DESC,id)`,
		`CREATE TABLE IF NOT EXISTS ` + q + `.legacy_imports(name TEXT PRIMARY KEY,imported_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`INSERT INTO ` + q + `.schema_migrations(version) VALUES('001_feedback') ON CONFLICT DO NOTHING`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			tx.Rollback()
			db.Close()
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{dir: dir, db: db, table: q + `.feedback`}
	if err := s.importLegacyPostgres(ctx, q); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.loadPostgres(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) importLegacyPostgres(ctx context.Context, schema string) error {
	var done bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+schema+`.legacy_imports WHERE name='feedback-json-v1')`).Scan(&done); err != nil || done {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	payload, err := os.ReadFile(s.path())
	if err == nil {
		var items []Feedback
		if err := json.Unmarshal(payload, &items); err != nil {
			return err
		}
		for _, item := range items {
			data, _ := json.Marshal(item)
			if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.table+`(id,owner,created_at,data) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, item.ID, item.Owner, item.CreatedAt, data); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+schema+`.legacy_imports(name) VALUES('feedback-json-v1') ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) loadPostgres(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM `+s.table+` ORDER BY created_at,id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return err
		}
		var item Feedback
		if err := json.Unmarshal(payload, &item); err != nil {
			return err
		}
		s.items = append(s.items, item)
	}
	return rows.Err()
}

func (s *Store) persistPostgresLocked() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+s.table); err != nil {
		return err
	}
	for _, item := range s.items {
		payload, err := json.Marshal(item)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.table+`(id,owner,created_at,data) VALUES($1,$2,$3,$4)`, item.ID, item.Owner, item.CreatedAt, payload); err != nil {
			return err
		}
	}
	return tx.Commit()
}
