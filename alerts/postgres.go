package main

import (
	"bufio"
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

var alertsIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func openPostgresStore(dir, databaseURL, schema string) (*Store, error) {
	if !alertsIdentifier.MatchString(schema) {
		return nil, fmt.Errorf("ALERTS_DATABASE_SCHEMA must be a PostgreSQL identifier")
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
		`CREATE TABLE IF NOT EXISTS ` + q + `.schema_migrations(version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS ` + q + `.rules(id TEXT PRIMARY KEY, data JSONB NOT NULL, updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS ` + q + `.events(id TEXT PRIMARY KEY, user_id TEXT NOT NULL, ts BIGINT NOT NULL, data JSONB NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE INDEX IF NOT EXISTS alert_events_user_ts_idx ON ` + q + `.events(user_id,ts DESC,id)`,
		`CREATE TABLE IF NOT EXISTS ` + q + `.read_state(user_id TEXT PRIMARY KEY, watermark BIGINT NOT NULL DEFAULT 0, read_ids JSONB NOT NULL DEFAULT '{}'::jsonb, updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS ` + q + `.monitor_state(name TEXT PRIMARY KEY, data JSONB NOT NULL, updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS ` + q + `.legacy_imports(name TEXT PRIMARY KEY, imported_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`INSERT INTO ` + q + `.schema_migrations(version) VALUES('001_alert_state') ON CONFLICT DO NOTHING`,
		`INSERT INTO ` + q + `.schema_migrations(version) VALUES('002_thesis_monitor_state') ON CONFLICT DO NOTHING`,
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
	s := &Store{dir: dir, db: db, schema: q, readWatermarks: map[string]int64{}, readIDs: map[string]map[string]bool{}}
	if err := s.importLegacyPostgres(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.loadPostgres(ctx); err != nil {
		db.Close()
		return nil, err
	}
	s.migrateLegacyRules()
	return s, nil
}

func (s *Store) importLegacyPostgres(ctx context.Context) error {
	var done bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.schema+`.legacy_imports WHERE name='alerts-files-v1')`).Scan(&done); err != nil || done {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if payload, err := os.ReadFile(s.rulesPath()); err == nil {
		var rules []Rule
		if err := json.Unmarshal(payload, &rules); err != nil {
			return err
		}
		for _, rule := range rules {
			if rule.UserID == "" {
				rule.UserID = "_legacy"
			}
			data, _ := json.Marshal(rule)
			if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.schema+`.rules(id,data) VALUES($1,$2) ON CONFLICT DO NOTHING`, rule.ID, data); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if file, err := os.Open(s.eventsPath()); err == nil {
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			var event Event
			if json.Unmarshal(scanner.Bytes(), &event) != nil {
				continue
			}
			data, _ := json.Marshal(event)
			if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.schema+`.events(id,user_id,ts,data) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, event.ID, event.UserID, event.TS, data); err != nil {
				file.Close()
				return err
			}
		}
		scanErr := scanner.Err()
		file.Close()
		if scanErr != nil {
			return scanErr
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if payload, err := os.ReadFile(s.statePath()); err == nil {
		var state persistedState
		if err := json.Unmarshal(payload, &state); err != nil {
			return err
		}
		users := map[string]bool{}
		for uid := range state.ReadWatermarks {
			users[uid] = true
		}
		for uid := range state.ReadIDs {
			users[uid] = true
		}
		for uid := range users {
			ids, _ := json.Marshal(state.ReadIDs[uid])
			if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.schema+`.read_state(user_id,watermark,read_ids) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, uid, state.ReadWatermarks[uid], ids); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	monitor := monitorState{Markers: map[string]ThesisMarker{}}
	monitorFound := false
	if payload, err := os.ReadFile(filepath.Join(s.dir, "thesis_markers.json")); err == nil {
		if err := json.Unmarshal(payload, &monitor.Markers); err != nil {
			return err
		}
		monitorFound = true
	} else if !os.IsNotExist(err) {
		return err
	}
	if payload, err := os.ReadFile(filepath.Join(s.dir, "resynth_queue.json")); err == nil {
		var queue monitorState
		if err := json.Unmarshal(payload, &queue); err != nil {
			return err
		}
		monitor.Queue, monitor.Dropped = queue.Queue, queue.Dropped
		monitorFound = true
	} else if !os.IsNotExist(err) {
		return err
	}
	if monitorFound {
		payload, _ := json.Marshal(monitor)
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.schema+`.monitor_state(name,data) VALUES('thesis-monitor',$1) ON CONFLICT DO NOTHING`, payload); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.schema+`.legacy_imports(name) VALUES('alerts-files-v1') ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) loadMonitorPostgres() (monitorState, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var payload []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM `+s.schema+`.monitor_state WHERE name='thesis-monitor'`).Scan(&payload)
	if err == sql.ErrNoRows {
		return monitorState{}, false, nil
	}
	if err != nil {
		return monitorState{}, false, err
	}
	var state monitorState
	if err := json.Unmarshal(payload, &state); err != nil {
		return monitorState{}, false, err
	}
	return state, true, nil
}

func (s *Store) saveMonitorPostgres(state monitorState) error {
	payload, err := json.Marshal(state)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = s.db.ExecContext(ctx, `INSERT INTO `+s.schema+`.monitor_state(name,data,updated_at) VALUES('thesis-monitor',$1,now()) ON CONFLICT(name) DO UPDATE SET data=excluded.data,updated_at=now()`, payload)
	return err
}

func (s *Store) loadPostgres(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM `+s.schema+`.rules ORDER BY id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			rows.Close()
			return err
		}
		var rule Rule
		if err := json.Unmarshal(payload, &rule); err != nil {
			rows.Close()
			return err
		}
		s.rules = append(s.rules, rule)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT user_id,watermark,read_ids FROM `+s.schema+`.read_state`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var uid string
		var watermark int64
		var payload []byte
		if err := rows.Scan(&uid, &watermark, &payload); err != nil {
			return err
		}
		ids := map[string]bool{}
		if err := json.Unmarshal(payload, &ids); err != nil {
			return err
		}
		s.readWatermarks[uid] = watermark
		s.readIDs[uid] = ids
	}
	return rows.Err()
}

func (s *Store) persistRulesPostgresLocked() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+s.schema+`.rules`); err != nil {
		return err
	}
	for _, rule := range s.rules {
		payload, err := json.Marshal(rule)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.schema+`.rules(id,data) VALUES($1,$2)`, rule.ID, payload); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) persistStatePostgresLocked(uid string) error {
	payload, err := json.Marshal(s.readIDs[uid])
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = s.db.ExecContext(ctx, `INSERT INTO `+s.schema+`.read_state(user_id,watermark,read_ids,updated_at) VALUES($1,$2,$3,now()) ON CONFLICT(user_id) DO UPDATE SET watermark=excluded.watermark,read_ids=excluded.read_ids,updated_at=now()`, uid, s.readWatermarks[uid], payload)
	return err
}

func (s *Store) appendEventPostgres(event Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = s.db.ExecContext(ctx, `INSERT INTO `+s.schema+`.events(id,user_id,ts,data) VALUES($1,$2,$3,$4) ON CONFLICT(id) DO NOTHING`, event.ID, event.UserID, event.TS, payload)
	return err
}

func (s *Store) listEventsPostgres(uid string) ([]Event, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM `+s.schema+`.events WHERE user_id=$1 ORDER BY ts DESC,id`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var event Event
		if err := json.Unmarshal(payload, &event); err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, rows.Err()
}
