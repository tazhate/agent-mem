// Package store owns the single SQLite database: data, FTS5 index and vectors in one file.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

const schemaVersion = "1"

type Store struct {
	db *sql.DB
	// wmu serializes writers; SQLite allows one writer anyway and this avoids busy retries.
	wmu sync.Mutex
}

// Open opens or creates the database at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	q := url.Values{}
	for _, p := range []string{"busy_timeout(10000)", "journal_mode(WAL)", "synchronous(NORMAL)", "foreign_keys(OFF)", "temp_store(MEMORY)", "cache_size(-32000)"} {
		q.Add("_pragma", p)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// OpenReadOnly opens an existing database without write access (used for inspection tools).
func OpenReadOnly(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(10000)")
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES ('schema_version', ?)
		ON CONFLICT(key) DO NOTHING`, schemaVersion)
	return err
}

// Write runs fn in an IMMEDIATE transaction under the writer lock.
func (s *Store) Write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}

func (s *Store) Meta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
		return err
	})
}

// Stats is a quick inventory for `amem doctor`.
type Stats struct {
	Projects, Sessions, Events, PendingEvents, Memories, Summaries, Vectors int64
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM projects), (SELECT count(*) FROM sessions), (SELECT count(*) FROM events),
		(SELECT count(*) FROM events WHERE processed = 0), (SELECT count(*) FROM memories),
		(SELECT count(*) FROM summaries), (SELECT count(*) FROM vectors)`).
		Scan(&st.Projects, &st.Sessions, &st.Events, &st.PendingEvents, &st.Memories, &st.Summaries, &st.Vectors)
	return st, err
}
