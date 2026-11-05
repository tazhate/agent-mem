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
	db   *sql.DB
	path string
	// wmu serializes writers; SQLite allows one writer anyway and this avoids busy retries.
	wmu sync.Mutex
}

// Open opens or creates the database at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	q := url.Values{}
	for _, p := range []string{"busy_timeout(10000)", "journal_mode(WAL)", "synchronous(NORMAL)", "foreign_keys(OFF)", "temp_store(MEMORY)", "cache_size(-65536)", "mmap_size(2147483648)"} {
		q.Add("_pragma", p)
	}
	// IMMEDIATE: a deferred transaction that reads first and writes later cannot wait on
	// busy_timeout when another process (watch, import) holds the write lock.
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	s := &Store{db: db, path: path}
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
	// Columns added after the first release; CREATE TABLE IF NOT EXISTS does not add them.
	for _, c := range []struct{ table, column, decl string }{
		{"sessions", "private_turn", "INTEGER NOT NULL DEFAULT -1"},
		{"memory_files", "base", "TEXT NOT NULL DEFAULT ''"},
		{"watch_state", "pending", "TEXT NOT NULL DEFAULT '{}'"},
		{"watch_state", "session", "TEXT NOT NULL DEFAULT ''"},
		{"watch_state", "cwd", "TEXT NOT NULL DEFAULT ''"},
		{"watch_state", "subagent", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if err := s.ensureColumn(ctx, c.table, c.column, c.decl); err != nil {
			return err
		}
	}
	if err := s.backfillFileBases(ctx); err != nil {
		return fmt.Errorf("file name index: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS memory_files_base ON memory_files(base)`); err != nil {
		return err
	}
	for _, f := range []string{"", "-wal", "-shm"} {
		_ = os.Chmod(s.path+f, 0o600)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES ('schema_version', ?)
		ON CONFLICT(key) DO NOTHING`, schemaVersion)
	return err
}

func (s *Store) ensureColumn(ctx context.Context, table, column, decl string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+column+` `+decl)
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

// backfillFileBases fills memory_files.base for rows created before the column existed.
func (s *Store) backfillFileBases(ctx context.Context) error {
	for {
		rows, err := s.db.QueryContext(ctx, `SELECT memory_id, path FROM memory_files WHERE base = '' LIMIT 5000`)
		if err != nil {
			return err
		}
		type row struct {
			id   int64
			path string
		}
		var batch []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.path); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil || len(batch) == 0 {
			return err
		}
		if err := s.Write(ctx, func(tx *sql.Tx) error {
			for _, r := range batch {
				base := filepath.Base(r.path)
				if base == "" || base == "." || base == "/" {
					base = r.path
				}
				if _, err := tx.ExecContext(ctx, `UPDATE memory_files SET base = ? WHERE memory_id = ? AND path = ?`, base, r.id, r.path); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
}
