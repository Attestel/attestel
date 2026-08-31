package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var postgresIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func openPostgresTradeStore(base, databaseURL, schema string) (*Store, error) {
	if !postgresIdentifier.MatchString(schema) {
		return nil, fmt.Errorf("JOURNAL_DATABASE_SCHEMA must be a PostgreSQL identifier")
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
	qualified := `"` + schema + `"`
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		db.Close()
		return nil, err
	}
	statements := []string{
		`CREATE SCHEMA IF NOT EXISTS ` + qualified,
		`CREATE TABLE IF NOT EXISTS ` + qualified + `.schema_migrations (
			version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS ` + qualified + `.trades (
			user_id TEXT NOT NULL, id TEXT NOT NULL, data JSONB NOT NULL,
			created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL,
			PRIMARY KEY (user_id, id))`,
		`CREATE INDEX IF NOT EXISTS trades_user_created_idx ON ` + qualified + `.trades(user_id, created_at, id)`,
		`CREATE TABLE IF NOT EXISTS ` + qualified + `.legacy_imports (
			name TEXT PRIMARY KEY, imported_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS ` + qualified + `.documents (
			user_id TEXT NOT NULL, collection TEXT NOT NULL, data JSONB NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(), PRIMARY KEY(user_id, collection))`,
		`CREATE TABLE IF NOT EXISTS ` + qualified + `.analytics_events (
			id BIGSERIAL PRIMARY KEY, event_date DATE NOT NULL, data JSONB NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE INDEX IF NOT EXISTS analytics_events_date_idx ON ` + qualified + `.analytics_events(event_date, id)`,
		`INSERT INTO ` + qualified + `.schema_migrations(version) VALUES ('001_trades') ON CONFLICT DO NOTHING`,
		`INSERT INTO ` + qualified + `.schema_migrations(version) VALUES ('002_research_documents') ON CONFLICT DO NOTHING`,
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
	s := &Store{base: base, db: db, table: qualified + `.trades`, schema: qualified}
	if err := s.importLegacyTradeFiles(ctx, qualified); err != nil {
		db.Close()
		return nil, fmt.Errorf("import legacy trade files: %w", err)
	}
	return s, nil
}

func (s *Store) importLegacyTradeFiles(ctx context.Context, schema string) error {
	var done bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM `+schema+`.legacy_imports WHERE name='trades-json-v1')`,
	).Scan(&done); err != nil || done {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	entries, err := os.ReadDir(s.base)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		uid := entry.Name()
		path := filepath.Join(s.base, uid, "trades.json")
		payload, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		var trades []Trade
		if err := json.Unmarshal(payload, &trades); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		for _, trade := range trades {
			data, err := json.Marshal(trade)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO `+s.table+`(user_id,id,data,created_at,updated_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`,
				uid, trade.ID, data, trade.CreatedAt, trade.UpdatedAt,
			); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO `+schema+`.legacy_imports(name) VALUES('trades-json-v1') ON CONFLICT DO NOTHING`,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) listPostgres(uid string) ([]Trade, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx,
		`SELECT data FROM `+s.table+` WHERE user_id=$1 ORDER BY created_at, id`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Trade{}
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var trade Trade
		if err := json.Unmarshal(payload, &trade); err != nil {
			return nil, err
		}
		out = append(out, trade)
	}
	return out, rows.Err()
}

func (s *Store) addPostgres(uid string, trade Trade) error {
	payload, err := json.Marshal(trade)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO `+s.table+`(user_id,id,data,created_at,updated_at) VALUES($1,$2,$3,$4,$5)`,
		uid, trade.ID, payload, trade.CreatedAt, trade.UpdatedAt)
	return err
}

func (s *Store) getPostgres(uid, id string) (Trade, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var payload []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT data FROM `+s.table+` WHERE user_id=$1 AND id=$2`, uid, id).Scan(&payload)
	if err == sql.ErrNoRows {
		return Trade{}, false, nil
	}
	if err != nil {
		return Trade{}, false, err
	}
	var trade Trade
	if err := json.Unmarshal(payload, &trade); err != nil {
		return Trade{}, false, err
	}
	return trade, true, nil
}

func (s *Store) replacePostgres(uid string, trade Trade) (Trade, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Trade{}, false, err
	}
	defer tx.Rollback()
	var createdAt int64
	err = tx.QueryRowContext(ctx,
		`SELECT created_at FROM `+s.table+` WHERE user_id=$1 AND id=$2 FOR UPDATE`, uid, trade.ID,
	).Scan(&createdAt)
	if err == sql.ErrNoRows {
		return Trade{}, false, nil
	}
	if err != nil {
		return Trade{}, false, err
	}
	trade.CreatedAt = createdAt
	trade.UpdatedAt = time.Now().Unix()
	payload, err := json.Marshal(trade)
	if err != nil {
		return Trade{}, false, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE `+s.table+` SET data=$3, updated_at=$4 WHERE user_id=$1 AND id=$2`,
		uid, trade.ID, payload, trade.UpdatedAt,
	); err != nil {
		return Trade{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Trade{}, false, err
	}
	return trade, true, nil
}

func (s *Store) deletePostgres(uid, id string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM `+s.table+` WHERE user_id=$1 AND id=$2`, uid, id)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}
