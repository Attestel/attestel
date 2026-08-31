package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// documentRepository is the PostgreSQL backend for the journal's existing per-user JSON
// collections. The owning stores still validate and interpret their own document shape; this layer
// only provides atomic bytes-in/bytes-out persistence and one-time lazy import of the old file.
type documentRepository struct {
	db     *sql.DB
	schema string
	base   string
}

func (r *documentRepository) importAnalyticsFiles() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var done bool
	if err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM `+r.schema+`.legacy_imports WHERE name='analytics-jsonl-v1')`,
	).Scan(&done); err != nil || done {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	dir := filepath.Join(r.base, analyticsDirName)
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		file, err := os.Open(filepath.Join(dir, entry.Name()))
		if err != nil {
			return err
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			payload := append([]byte(nil), scanner.Bytes()...)
			if !json.Valid(payload) {
				continue
			}
			var ev analyticsEvent
			if json.Unmarshal(payload, &ev) != nil {
				continue
			}
			date := time.Unix(ev.At, 0).UTC().Format("2006-01-02")
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO `+r.schema+`.analytics_events(event_date,data) VALUES($1,$2)`, date, payload); err != nil {
				file.Close()
				return err
			}
		}
		scanErr := scanner.Err()
		file.Close()
		if scanErr != nil {
			return scanErr
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO `+r.schema+`.legacy_imports(name) VALUES('analytics-jsonl-v1') ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *documentRepository) appendAnalytics(event analyticsEvent) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	date := time.Unix(event.At, 0).UTC().Format("2006-01-02")
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO `+r.schema+`.analytics_events(event_date,data) VALUES($1,$2)`, date, payload)
	return err
}

func (r *documentRepository) analyticsEvents() ([]analyticsEvent, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := r.db.QueryContext(ctx,
		`SELECT data FROM `+r.schema+`.analytics_events ORDER BY event_date,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []analyticsEvent
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var event analyticsEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

func newDocumentRepository(db *sql.DB, schema, base string) *documentRepository {
	if db == nil {
		return nil
	}
	return &documentRepository{db: db, schema: schema, base: base}
}

func (r *documentRepository) load(uid, collection, legacyPath string) ([]byte, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var payload []byte
	err := r.db.QueryRowContext(ctx,
		`SELECT data FROM `+r.schema+`.documents WHERE user_id=$1 AND collection=$2`, uid, collection,
	).Scan(&payload)
	if err == nil {
		return payload, true, nil
	}
	if err != sql.ErrNoRows {
		return nil, false, err
	}
	legacy, readErr := os.ReadFile(legacyPath)
	if os.IsNotExist(readErr) {
		return nil, false, nil
	}
	if readErr != nil {
		return nil, false, readErr
	}
	if !json.Valid(legacy) {
		return nil, true, errors.New("legacy journal document contains invalid JSON")
	}
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO `+r.schema+`.documents(user_id,collection,data) VALUES($1,$2,$3)
		 ON CONFLICT(user_id,collection) DO NOTHING`, uid, collection, legacy)
	if err != nil {
		return nil, false, err
	}
	err = r.db.QueryRowContext(ctx,
		`SELECT data FROM `+r.schema+`.documents WHERE user_id=$1 AND collection=$2`, uid, collection,
	).Scan(&payload)
	return payload, err == nil, err
}

// saveMany writes related collections in one transaction. Cascades use this so PostgreSQL can
// never retain one half of a delete (for example an evidence record without its link cleanup).
func (r *documentRepository) saveMany(uid string, values map[string]any) error {
	payloads := make(map[string][]byte, len(values))
	for collection, value := range values {
		payload, err := json.Marshal(value)
		if err != nil {
			return err
		}
		payloads[collection] = payload
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for collection, payload := range payloads {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO `+r.schema+`.documents(user_id,collection,data,updated_at) VALUES($1,$2,$3,now())
			 ON CONFLICT(user_id,collection) DO UPDATE SET data=excluded.data,updated_at=now()`,
			uid, collection, payload); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *documentRepository) save(uid, collection string, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO `+r.schema+`.documents(user_id,collection,data,updated_at) VALUES($1,$2,$3,now())
		 ON CONFLICT(user_id,collection) DO UPDATE SET data=excluded.data,updated_at=now()`,
		uid, collection, payload)
	return err
}

// userIDs imports legacy documents visible on disk before returning the PostgreSQL owner set. It
// exists for internal cross-user sweeps; normal browser routes remain strictly session-partitioned.
func (r *documentRepository) userIDs(collection, filename string) ([]string, error) {
	entries, err := os.ReadDir(r.base)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		uid := entry.Name()
		if reservedUID(uid) {
			continue
		}
		_, _, err := r.load(uid, collection, filepath.Join(r.base, uid, filename))
		if err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := r.db.QueryContext(ctx,
		`SELECT DISTINCT user_id FROM `+r.schema+`.documents WHERE collection=$1`, collection)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		if !reservedUID(uid) {
			out = append(out, uid)
		}
	}
	sort.Strings(out)
	return out, rows.Err()
}
