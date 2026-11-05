package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type Project struct {
	ID   int64
	Key  string
	Name string
	Root string
}

// EnsureProject returns the project id for key, creating it or following an alias.
func (s *Store) EnsureProject(ctx context.Context, key, name, root string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM projects WHERE key = ?
		UNION ALL SELECT project_id FROM project_aliases WHERE alias = ? LIMIT 1`, key, key).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	err = s.Write(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = ensureProjectTx(ctx, tx, key, name, root)
		return err
	})
	return id, err
}

func ensureProjectTx(ctx context.Context, tx *sql.Tx, key, name, root string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `INSERT INTO projects(key, name, root, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET root = coalesce(projects.root, excluded.root)
		RETURNING id`, key, name, nullStr(root), time.Now().UnixMilli()).Scan(&id)
	return id, err
}

func (s *Store) ProjectByID(ctx context.Context, id int64) (Project, error) {
	var p Project
	var root sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id, key, name, root FROM projects WHERE id = ?`, id).
		Scan(&p.ID, &p.Key, &p.Name, &root)
	p.Root = root.String
	return p, err
}

// AdoptLegacy merges an imported "legacy:<name>" project into a live git project with the same
// name, when exactly one legacy project matches. Called when a session from a git repo starts.
func (s *Store) AdoptLegacy(ctx context.Context, projectID int64, name string) (bool, error) {
	var legacyID int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM projects WHERE key = ?`, "legacy:"+name).Scan(&legacyID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil || legacyID == projectID {
		return false, err
	}
	return true, s.Write(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{
			`UPDATE memories SET project_id = ?1 WHERE project_id = ?2`,
			`UPDATE sessions SET project_id = ?1 WHERE project_id = ?2`,
			`UPDATE summaries SET project_id = ?1 WHERE project_id = ?2`,
			`UPDATE work_entries SET project_id = ?1 WHERE project_id = ?2`,
			`UPDATE project_aliases SET project_id = ?1 WHERE project_id = ?2`,
		} {
			if _, err := tx.ExecContext(ctx, q, projectID, legacyID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO project_aliases(alias, project_id)
			SELECT key, ? FROM projects WHERE id = ?`, projectID, legacyID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, legacyID)
		return err
	})
}

type Session struct {
	ID         string
	Runtime    string
	ProjectID  int64
	Turn       int
	Started    int64
	Ended      int64
	Summarized int64
}

// HasEvent reports whether an event with this hook id was already stored.
func (s *Store) HasEvent(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE origin = 'hook' AND origin_id = ?`, id).Scan(&n)
	return n > 0, err
}

// TouchSession creates the session on first sight and returns its current turn number.
// A prompt event opens a new turn.
func (s *Store) TouchSession(ctx context.Context, tx *sql.Tx, id, runtime, native string, projectID int64, cwd, model string, newTurn bool, ts int64) (int, error) {
	inc := 0
	if newTurn {
		inc = 1
	}
	var turn int
	err := tx.QueryRowContext(ctx, `INSERT INTO sessions(id, runtime, native_id, project_id, cwd, model, turn, started_at, last_event_at)
		VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?8)
		ON CONFLICT(id) DO UPDATE SET
			turn = sessions.turn + ?7,
			last_event_at = ?8,
			project_id = coalesce(sessions.project_id, excluded.project_id),
			cwd = coalesce(sessions.cwd, excluded.cwd),
			model = coalesce(excluded.model, sessions.model)
		RETURNING turn`, id, runtime, native, projectID, nullStr(cwd), nullStr(model), inc, ts).Scan(&turn)
	return turn, err
}

func (s *Store) EndSession(ctx context.Context, tx *sql.Tx, id string, ts int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE sessions SET ended_at = ? WHERE id = ?`, ts, id)
	return err
}

func (s *Store) GetSession(ctx context.Context, id string) (Session, error) {
	var se Session
	var ended, summ sql.NullInt64
	var pid sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT id, runtime, project_id, turn, started_at, ended_at, summarized_at
		FROM sessions WHERE id = ?`, id).Scan(&se.ID, &se.Runtime, &pid, &se.Turn, &se.Started, &ended, &summ)
	se.ProjectID, se.Ended, se.Summarized = pid.Int64, ended.Int64, summ.Int64
	return se, err
}

type EventRow struct {
	ID        int64
	SessionID string
	Turn      int
	Kind      string
	Tool      string
	Input     string
	Output    string
	Error     string
	ExitCode  *int
	TS        int64
	Origin    string
	OriginID  string
}

func (s *Store) InsertEvent(ctx context.Context, tx *sql.Tx, e EventRow) (int64, error) {
	var exit any
	if e.ExitCode != nil {
		exit = *e.ExitCode
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO events(session_id, turn, kind, tool, input, output, error, exit_code, ts, origin, origin_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		e.SessionID, e.Turn, e.Kind, nullStr(e.Tool), nullStr(e.Input), nullStr(e.Output), nullStr(e.Error), exit, e.TS,
		nullStr(e.Origin), nullStr(e.OriginID))
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, nil
	}
	return res.LastInsertId()
}

// PendingEvents returns unprocessed events of a session in order.
func (s *Store) PendingEvents(ctx context.Context, sessionID string, limit int) ([]EventRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, session_id, turn, kind, coalesce(tool,''), coalesce(input,''),
		coalesce(output,''), coalesce(error,''), exit_code, ts
		FROM events WHERE session_id = ? AND processed = 0 ORDER BY id LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var e EventRow
		var exit sql.NullInt64
		if err := rows.Scan(&e.ID, &e.SessionID, &e.Turn, &e.Kind, &e.Tool, &e.Input, &e.Output, &e.Error, &exit, &e.TS); err != nil {
			return nil, err
		}
		if exit.Valid {
			n := int(exit.Int64)
			e.ExitCode = &n
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SessionsWithPending lists sessions that have unprocessed events, oldest activity first.
func (s *Store) SessionsWithPending(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id FROM events WHERE processed = 0
		GROUP BY session_id ORDER BY min(id)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) MarkProcessed(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		st, err := tx.PrepareContext(ctx, `UPDATE events SET processed = 1 WHERE id = ?`)
		if err != nil {
			return err
		}
		defer st.Close()
		for _, id := range ids {
			if _, err := st.ExecContext(ctx, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// PruneEvents drops processed raw events older than the cutoff.
func (s *Store) PruneEvents(ctx context.Context, before int64) (int64, error) {
	var n int64
	err := s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM events WHERE processed = 1 AND ts < ?`, before)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ProjectsByName finds projects by human name (several repos may share one).
func (s *Store) ProjectsByName(ctx context.Context, name string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM projects WHERE name = ? OR key = ?
		UNION SELECT project_id FROM project_aliases WHERE alias = ? OR alias = 'legacy:' || ?`, name, name, name, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
