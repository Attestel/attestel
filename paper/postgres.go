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
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var paperIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type paperDatabase struct {
	db     *sql.DB
	schema string
}

func openPaperDatabase(databaseURL, schema, legacyDir string) (*paperDatabase, error) {
	if !paperIdentifier.MatchString(schema) {
		return nil, fmt.Errorf("PAPER_DATABASE_SCHEMA must be a PostgreSQL identifier")
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
		`CREATE TABLE IF NOT EXISTS ` + q + `.schema_migrations (
			version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS ` + q + `.meta (
			id SMALLINT PRIMARY KEY CHECK(id=1), current_generation BIGINT NOT NULL)`,
		`INSERT INTO ` + q + `.meta(id,current_generation) VALUES(1,1) ON CONFLICT DO NOTHING`,
		`CREATE TABLE IF NOT EXISTS ` + q + `.documents (
			name TEXT PRIMARY KEY, generation BIGINT NOT NULL DEFAULT 0, data JSONB NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS ` + q + `.fills (
			generation BIGINT NOT NULL, seq BIGINT NOT NULL, data JSONB NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(), PRIMARY KEY(generation,seq))`,
		`CREATE TABLE IF NOT EXISTS ` + q + `.snapshots (
			generation BIGINT NOT NULL, date TEXT NOT NULL, data JSONB NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(), PRIMARY KEY(generation,date))`,
		`CREATE TABLE IF NOT EXISTS ` + q + `.decision_events (
			generation BIGINT NOT NULL, config TEXT NOT NULL, bar_unix BIGINT NOT NULL,
			decided_at TIMESTAMPTZ NOT NULL, data JSONB NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY(generation,config,bar_unix))`,
		`CREATE TABLE IF NOT EXISTS ` + q + `.shadow_observations (
			contract_version TEXT NOT NULL, config TEXT NOT NULL, bar_unix BIGINT NOT NULL,
			observed_at TIMESTAMPTZ NOT NULL, data JSONB NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY(contract_version,config,bar_unix))`,
		`CREATE TABLE IF NOT EXISTS ` + q + `.shadow_bars (
			ticker TEXT NOT NULL, timeframe TEXT NOT NULL, bar_unix BIGINT NOT NULL,
			data JSONB NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY(ticker,timeframe,bar_unix))`,
		`CREATE TABLE IF NOT EXISTS ` + q + `.shadow_outcomes (
			contract_version TEXT NOT NULL, config TEXT NOT NULL, signal_bar_unix BIGINT NOT NULL,
			horizon INTEGER NOT NULL, data JSONB NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY(contract_version,config,signal_bar_unix,horizon))`,
		`CREATE TABLE IF NOT EXISTS ` + q + `.resets (
			previous_generation BIGINT PRIMARY KEY, next_generation BIGINT NOT NULL,
			reset_at TIMESTAMPTZ NOT NULL, previous_state JSONB NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS ` + q + `.legacy_imports (
			name TEXT PRIMARY KEY, imported_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`INSERT INTO ` + q + `.schema_migrations(version) VALUES ('001_paper_state') ON CONFLICT DO NOTHING`,
		`INSERT INTO ` + q + `.schema_migrations(version) VALUES ('002_decision_events') ON CONFLICT DO NOTHING`,
		`INSERT INTO ` + q + `.schema_migrations(version) VALUES ('003_shadow_evidence') ON CONFLICT DO NOTHING`,
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
	repo := &paperDatabase{db: db, schema: q}
	if err := repo.importLegacy(ctx, legacyDir); err != nil {
		db.Close()
		return nil, fmt.Errorf("import legacy paper files: %w", err)
	}
	return repo, nil
}

func (p *paperDatabase) insertShadowObservation(observation ShadowObservation) error {
	payload, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = p.db.ExecContext(ctx,
		`INSERT INTO `+p.schema+`.shadow_observations
		 (contract_version,config,bar_unix,observed_at,data) VALUES($1,$2,$3,$4,$5)
		 ON CONFLICT(contract_version,config,bar_unix) DO NOTHING`,
		observation.ContractVersion, observation.Config, observation.SignalBarUnix,
		observation.ObservedAt, payload)
	return err
}

func (p *paperDatabase) insertShadowBars(bars []ShadowBar) error {
	if len(bars) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, bar := range bars {
		payload, err := json.Marshal(bar)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO `+p.schema+`.shadow_bars(ticker,timeframe,bar_unix,data)
			 VALUES($1,$2,$3,$4) ON CONFLICT(ticker,timeframe,bar_unix) DO NOTHING`,
			bar.Ticker, bar.Timeframe, bar.BarUnix, payload); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (p *paperDatabase) insertShadowOutcomes(outcomes []ShadowOutcome) error {
	if len(outcomes) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, outcome := range outcomes {
		payload, err := json.Marshal(outcome)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO `+p.schema+`.shadow_outcomes
			 (contract_version,config,signal_bar_unix,horizon,data) VALUES($1,$2,$3,$4,$5)
			 ON CONFLICT(contract_version,config,signal_bar_unix,horizon) DO NOTHING`,
			outcome.ContractVersion, outcome.Config, outcome.SignalBarUnix, outcome.Horizon, payload); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (p *paperDatabase) shadowDataset() (shadowDataset, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data := shadowDataset{Observations: []ShadowObservation{}, Bars: []ShadowBar{}, Outcomes: []ShadowOutcome{}}
	read := func(query string, consume func([]byte) error) error {
		rows, err := p.db.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var payload []byte
			if err := rows.Scan(&payload); err != nil {
				return err
			}
			if err := consume(payload); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	if err := read(`SELECT data FROM `+p.schema+`.shadow_observations ORDER BY bar_unix`, func(payload []byte) error {
		var item ShadowObservation
		if err := json.Unmarshal(payload, &item); err != nil {
			return err
		}
		data.Observations = append(data.Observations, item)
		return nil
	}); err != nil {
		return data, err
	}
	if err := read(`SELECT data FROM `+p.schema+`.shadow_bars ORDER BY ticker,timeframe,bar_unix`, func(payload []byte) error {
		var item ShadowBar
		if err := json.Unmarshal(payload, &item); err != nil {
			return err
		}
		data.Bars = append(data.Bars, item)
		return nil
	}); err != nil {
		return data, err
	}
	if err := read(`SELECT data FROM `+p.schema+`.shadow_outcomes ORDER BY signal_bar_unix,horizon`, func(payload []byte) error {
		var item ShadowOutcome
		if err := json.Unmarshal(payload, &item); err != nil {
			return err
		}
		data.Outcomes = append(data.Outcomes, item)
		return nil
	}); err != nil {
		return data, err
	}
	return data, nil
}

func (p *paperDatabase) generation(ctx context.Context) (int64, error) {
	var generation int64
	err := p.db.QueryRowContext(ctx,
		`SELECT current_generation FROM `+p.schema+`.meta WHERE id=1`).Scan(&generation)
	return generation, err
}

func (p *paperDatabase) importLegacy(ctx context.Context, dir string) error {
	var done bool
	if err := p.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM `+p.schema+`.legacy_imports WHERE name='paper-files-v1')`,
	).Scan(&done); err != nil || done {
		return err
	}
	generation, err := p.generation(ctx)
	if err != nil {
		return err
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	importDocument := func(name, filename string, target any) error {
		payload, err := os.ReadFile(filepath.Join(dir, filename))
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := json.Unmarshal(payload, target); err != nil {
			return fmt.Errorf("%s: %w", filename, err)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO `+p.schema+`.documents(name,generation,data) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`,
			name, generation, payload)
		return err
	}
	if err := importDocument("engine_state", "state.json", &persisted{}); err != nil {
		return err
	}
	if err := importDocument("ledger_state", "ledger.json", &ledgerState{}); err != nil {
		return err
	}
	if err := importJSONL(filepath.Join(dir, "fills.jsonl"), func(payload []byte) error {
		var fill Fill
		if json.Unmarshal(payload, &fill) != nil {
			return nil // preserve the legacy torn-final-line recovery rule
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO `+p.schema+`.fills(generation,seq,data) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`,
			generation, fill.Seq, payload)
		return err
	}); err != nil {
		return err
	}
	if err := importJSONL(filepath.Join(dir, "snapshots.jsonl"), func(payload []byte) error {
		var snapshot Snapshot
		if json.Unmarshal(payload, &snapshot) != nil || strings.TrimSpace(snapshot.Date) == "" {
			return nil
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO `+p.schema+`.snapshots(generation,date,data) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`,
			generation, snapshot.Date, payload)
		return err
	}); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO `+p.schema+`.legacy_imports(name) VALUES('paper-files-v1') ON CONFLICT DO NOTHING`,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func importJSONL(path string, consume func([]byte) error) error {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := []byte(strings.TrimSpace(scanner.Text()))
		if len(line) > 0 {
			if err := consume(line); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

func (p *paperDatabase) loadDocument(name string) ([]byte, bool, int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var payload []byte
	var generation int64
	err := p.db.QueryRowContext(ctx,
		`SELECT data,generation FROM `+p.schema+`.documents WHERE name=$1`, name,
	).Scan(&payload, &generation)
	if err == sql.ErrNoRows {
		return nil, false, 0, nil
	}
	return payload, err == nil, generation, err
}

func (p *paperDatabase) saveDocument(name string, generation int64, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = p.db.ExecContext(ctx,
		`INSERT INTO `+p.schema+`.documents(name,generation,data,updated_at) VALUES($1,$2,$3,now())
		 ON CONFLICT(name) DO UPDATE SET generation=excluded.generation,data=excluded.data,updated_at=now()`,
		name, generation, payload)
	return err
}

// saveStateAndDecision is the evidence commit point: the advanced bar cursor and the settled
// decision that explains it either both become durable or neither does.
func (p *paperDatabase) saveStateAndDecision(state persisted, event DecisionEvent) error {
	statePayload, err := json.Marshal(state)
	if err != nil {
		return err
	}
	eventPayload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO `+p.schema+`.documents(name,generation,data,updated_at) VALUES('engine_state',0,$1,now())
		 ON CONFLICT(name) DO UPDATE SET generation=0,data=excluded.data,updated_at=now()`,
		statePayload); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO `+p.schema+`.decision_events(generation,config,bar_unix,decided_at,data)
		 VALUES($1,$2,$3,$4,$5) ON CONFLICT(generation,config,bar_unix) DO NOTHING`,
		event.Generation, event.Config, event.Decision.BarUnix, event.Decision.At, eventPayload); err != nil {
		return err
	}
	return tx.Commit()
}

func (p *paperDatabase) decisionEvents(generation int64, limit int) ([]DecisionEvent, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	query := `SELECT data FROM ` + p.schema +
		`.decision_events WHERE generation=$1 ORDER BY bar_unix DESC,decided_at DESC`
	args := []any{generation}
	if limit > 0 {
		query += ` LIMIT $2`
		args = append(args, limit)
	}
	rows, err := p.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DecisionEvent{}
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var event DecisionEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

func (p *paperDatabase) archivedExperiments(parent context.Context) ([]experimentGenerationSummary, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	rows, err := p.db.QueryContext(ctx,
		`SELECT r.previous_generation,r.reset_at,r.previous_state,
			(SELECT count(*) FROM `+p.schema+`.fills f WHERE f.generation=r.previous_generation),
			(SELECT count(*) FROM `+p.schema+`.decision_events d WHERE d.generation=r.previous_generation)
		 FROM `+p.schema+`.resets r ORDER BY r.previous_generation DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []experimentGenerationSummary{}
	for rows.Next() {
		var item experimentGenerationSummary
		var resetAt time.Time
		var payload []byte
		if err := rows.Scan(&item.Generation, &resetAt, &payload, &item.NFills, &item.NDecisions); err != nil {
			return nil, err
		}
		var state ledgerState
		if err := json.Unmarshal(payload, &state); err != nil {
			return nil, err
		}
		item.ResetAt = resetAt.UTC().Format(time.RFC3339)
		item.OfficialStartedAt = state.OfficialStartedAt
		item.OfficialConfigs = append([]string{}, state.OfficialConfigs...)
		item.NSnapshots = len(state.Snapshots)
		item.NGapDates = len(state.GapDates)
		out = append(out, item)
	}
	return out, rows.Err()
}

func (p *paperDatabase) appendFill(generation int64, fill Fill) error {
	payload, err := json.Marshal(fill)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = p.db.ExecContext(ctx,
		`INSERT INTO `+p.schema+`.fills(generation,seq,data) VALUES($1,$2,$3)`, generation, fill.Seq, payload)
	return err
}

func (p *paperDatabase) fills(generation int64) ([]Fill, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := p.db.QueryContext(ctx,
		`SELECT data FROM `+p.schema+`.fills WHERE generation=$1 ORDER BY seq`, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Fill{}
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var fill Fill
		if err := json.Unmarshal(payload, &fill); err != nil {
			return nil, err
		}
		out = append(out, fill)
	}
	return out, rows.Err()
}

func (p *paperDatabase) appendSnapshot(generation int64, snapshot Snapshot) error {
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = p.db.ExecContext(ctx,
		`INSERT INTO `+p.schema+`.snapshots(generation,date,data) VALUES($1,$2,$3)`,
		generation, snapshot.Date, payload)
	return err
}

func (p *paperDatabase) snapshots(generation int64) ([]Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := p.db.QueryContext(ctx,
		`SELECT data FROM `+p.schema+`.snapshots WHERE generation=$1 ORDER BY date`, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Snapshot{}
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var snapshot Snapshot
		if err := json.Unmarshal(payload, &snapshot); err != nil {
			return nil, err
		}
		out = append(out, snapshot)
	}
	return out, rows.Err()
}

func (p *paperDatabase) resetLedger(previousGeneration int64, previous, fresh ledgerState, now time.Time) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	previousPayload, err := json.Marshal(previous)
	if err != nil {
		return previousGeneration, err
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return previousGeneration, err
	}
	defer tx.Rollback()
	var current int64
	if err := tx.QueryRowContext(ctx,
		`SELECT current_generation FROM `+p.schema+`.meta WHERE id=1 FOR UPDATE`).Scan(&current); err != nil {
		return previousGeneration, err
	}
	if current != previousGeneration {
		return previousGeneration, fmt.Errorf("paper ledger generation changed from %d to %d", previousGeneration, current)
	}
	next := current + 1
	fresh.Generation = next
	freshPayload, err := json.Marshal(fresh)
	if err != nil {
		return previousGeneration, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO `+p.schema+`.resets(previous_generation,next_generation,reset_at,previous_state)
		 VALUES($1,$2,$3,$4)`, current, next, now.UTC(), previousPayload); err != nil {
		return previousGeneration, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE `+p.schema+`.meta SET current_generation=$1 WHERE id=1`, next); err != nil {
		return previousGeneration, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO `+p.schema+`.documents(name,generation,data,updated_at) VALUES('ledger_state',$1,$2,now())
		 ON CONFLICT(name) DO UPDATE SET generation=excluded.generation,data=excluded.data,updated_at=now()`,
		next, freshPayload); err != nil {
		return previousGeneration, err
	}
	if err := tx.Commit(); err != nil {
		return previousGeneration, err
	}
	return next, nil
}
