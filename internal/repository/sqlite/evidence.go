// Package sqlite is the Phase 1 evidence store (ADR 0004).
package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registers "sqlite"

	"github.com/davidsugianto/upsera-agent/internal/domain"
)

const schema = `CREATE TABLE IF NOT EXISTS evidence (
	investigation_id TEXT NOT NULL,
	id               TEXT NOT NULL,
	tool             TEXT NOT NULL,
	args             TEXT NOT NULL,
	excerpt          TEXT NOT NULL,
	ts               TEXT NOT NULL,
	sha256           TEXT NOT NULL,
	PRIMARY KEY (investigation_id, id)
)`

// Store implements investigation.EvidenceStore on a local SQLite file.
type Store struct {
	db *sql.DB
}

// Open creates the file with mode 0600 if needed and migrates the schema.
//
// SQLite creates files with the process umask, so the file is created here
// first to pin the mode. The journal stays in the default rollback mode, not
// WAL: Phase 1 is a single-process CLI, and WAL would add -wal and -shm
// sidecar files that hold evidence too and need the same protection.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create evidence dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create evidence file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("create evidence file: %w", err)
	}
	// O_CREATE only applies the mode to new files; tighten one that already
	// existed with looser permissions.
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("restrict evidence file: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open evidence db: %w", err)
	}
	// PRAGMAs are per connection; a single connection keeps them in force and
	// serialises writers.
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{"PRAGMA busy_timeout = 5000", schema} {
		if _, err := db.Exec(stmt); err != nil {
			return nil, errors.Join(fmt.Errorf("init evidence db: %w", err), db.Close())
		}
	}
	return &Store{db: db}, nil
}

// Add stores one evidence row. It refuses rows whose hash does not match the
// excerpt, so a stored row can always be re-verified.
func (s *Store) Add(ctx context.Context, e domain.Evidence) error {
	if e.ID == "" || e.InvestigationID == "" || e.Tool == "" {
		return errors.New("evidence requires id, investigation id and tool")
	}
	sum := sha256.Sum256([]byte(e.Excerpt))
	if e.SHA256 != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("evidence %s: sha256 does not match excerpt", e.ID)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO evidence (investigation_id, id, tool, args, excerpt, ts, sha256)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.InvestigationID, e.ID, e.Tool, e.Args, e.Excerpt,
		e.TS.UTC().Format(time.RFC3339Nano), e.SHA256)
	if err != nil {
		return fmt.Errorf("add evidence %s/%s: %w", e.InvestigationID, e.ID, err)
	}
	return nil
}

// List returns an investigation's evidence in insertion order.
func (s *Store) List(ctx context.Context, investigationID string) ([]domain.Evidence, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT investigation_id, id, tool, args, excerpt, ts, sha256
		 FROM evidence WHERE investigation_id = ? ORDER BY rowid`, investigationID)
	if err != nil {
		return nil, fmt.Errorf("list evidence: %w", err)
	}
	defer rows.Close()

	out := []domain.Evidence{}
	for rows.Next() {
		var e domain.Evidence
		var ts string
		if err := rows.Scan(&e.InvestigationID, &e.ID, &e.Tool, &e.Args, &e.Excerpt, &ts, &e.SHA256); err != nil {
			return nil, fmt.Errorf("scan evidence: %w", err)
		}
		if e.TS, err = time.Parse(time.RFC3339Nano, ts); err != nil {
			return nil, fmt.Errorf("evidence %s: bad timestamp: %w", e.ID, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list evidence: %w", err)
	}
	return out, nil
}

func (s *Store) Close() error { return s.db.Close() }
