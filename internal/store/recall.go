package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

// Injected returns ids already shown in a session (real or shadow), for dedupe.
func (s *Store) Injected(ctx context.Context, sessionID string) (map[int64]bool, int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT memory_id, tokens FROM injections WHERE session_id = ?`, sessionID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	seen := map[int64]bool{}
	total := 0
	for rows.Next() {
		var id int64
		var tok int
		if err := rows.Scan(&id, &tok); err != nil {
			return nil, 0, err
		}
		seen[id] = true
		total += tok
	}
	return seen, total, rows.Err()
}

type Injection struct {
	MemoryID int64
	Tokens   int
}

// RecordInjections logs what recall returned; real injections also bump inject_count.
func (s *Store) RecordInjections(ctx context.Context, sessionID, trigger string, items []Injection, shadow bool) error {
	if len(items) == 0 {
		return nil
	}
	now := time.Now().UnixMilli()
	return s.Write(ctx, func(tx *sql.Tx) error {
		for _, it := range items {
			if _, err := tx.ExecContext(ctx, `INSERT INTO injections(session_id, memory_id, trigger, tokens, ts, shadow)
				VALUES (?, ?, ?, ?, ?, ?)`, sessionID, it.MemoryID, trigger, it.Tokens, now, shadow); err != nil {
				return err
			}
			if !shadow {
				if _, err := tx.ExecContext(ctx, `UPDATE memories SET inject_count = inject_count + 1 WHERE id = ?`, it.MemoryID); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// MarkUsed records that memories proved useful in a session: fetched, cited or acted on.
// Each memory counts once per session.
func (s *Store) MarkUsed(ctx context.Context, sessionID string, ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	now := time.Now().UnixMilli()
	n := 0
	err := s.Write(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			var already int
			err := tx.QueryRowContext(ctx, `SELECT count(*) FROM injections WHERE session_id = ? AND memory_id = ? AND used = 1`, sessionID, id).Scan(&already)
			if err != nil {
				return err
			}
			if already > 0 {
				continue
			}
			res, err := tx.ExecContext(ctx, `UPDATE injections SET used = 1 WHERE session_id = ? AND memory_id = ?`, sessionID, id)
			if err != nil {
				return err
			}
			if aff, _ := res.RowsAffected(); aff == 0 {
				// Fetched without having been injected (found via search): still a use.
				if _, err := tx.ExecContext(ctx, `INSERT INTO injections(session_id, memory_id, trigger, ts, used) VALUES (?, ?, 'fetch', ?, 1)`, sessionID, id, now); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE memories SET use_count = use_count + 1, last_used_at = ? WHERE id = ?`, now, id); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

// InjectedWithFiles returns injected-but-unused memories of a session that mention any of the paths.
func (s *Store) InjectedWithFiles(ctx context.Context, sessionID string, paths []string) ([]int64, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	args := []any{sessionID}
	cond := ""
	for i, p := range paths {
		if i > 0 {
			cond += " OR "
		}
		cond += "f.path = ?"
		args = append(args, p)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT i.memory_id FROM injections i JOIN memory_files f ON f.memory_id = i.memory_id
		WHERE i.session_id = ? AND i.used = 0 AND i.shadow = 0 AND (`+cond+`)`, args...)
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

type ShadowRow struct {
	SessionID string
	Trigger   string
	MemoryID  int64
	Title     string
	TS        int64
}

func (s *Store) ShadowLog(ctx context.Context, since int64, limit int) ([]ShadowRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT i.session_id, i.trigger, i.memory_id, coalesce(m.title, ''), i.ts
		FROM injections i LEFT JOIN memories m ON m.id = i.memory_id
		WHERE i.shadow = 1 AND i.ts >= ? ORDER BY i.ts DESC LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShadowRow
	for rows.Next() {
		var r ShadowRow
		if err := rows.Scan(&r.SessionID, &r.Trigger, &r.MemoryID, &r.Title, &r.TS); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type Summary struct {
	ID           int64  `json:"id"`
	SessionID    string `json:"session_id"`
	ProjectID    int64  `json:"project_id,omitzero"`
	Request      string `json:"request,omitempty"`
	Investigated string `json:"investigated,omitempty"`
	Learned      string `json:"learned,omitempty"`
	Completed    string `json:"completed,omitempty"`
	NextSteps    string `json:"next_steps,omitempty"`
	Notes        string `json:"notes,omitempty"`
	CreatedAt    int64  `json:"created_at"`
	Origin       string `json:"-"`
	OriginID     string `json:"-"`
}

func InsertSummaryTx(ctx context.Context, tx *sql.Tx, su *Summary) (bool, error) {
	if su.CreatedAt == 0 {
		su.CreatedAt = time.Now().UnixMilli()
	}
	var pid any
	if su.ProjectID != 0 {
		pid = su.ProjectID
	}
	return InsertReturning(ctx, tx, `INSERT INTO summaries(session_id, project_id, request, investigated, learned, completed,
		next_steps, notes, created_at, origin, origin_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		su.SessionID, pid, su.Request, su.Investigated, su.Learned, su.Completed, su.NextSteps, su.Notes, su.CreatedAt,
		nullStr(su.Origin), nullStr(su.OriginID))
}

func (s *Store) InsertSummary(ctx context.Context, su *Summary) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		if _, err := InsertSummaryTx(ctx, tx, su); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET summarized_at = ? WHERE id = ?`, su.CreatedAt, su.SessionID)
		return err
	})
}

// LatestSummary returns the newest summary of a project, excluding one session.
func (s *Store) LatestSummary(ctx context.Context, projectID int64, excludeSession string) (*Summary, error) {
	var su Summary
	err := s.db.QueryRowContext(ctx, `SELECT id, session_id, request, investigated, learned, completed, next_steps, notes, created_at
		FROM summaries WHERE project_id = ? AND session_id != ? ORDER BY created_at DESC LIMIT 1`, projectID, excludeSession).
		Scan(&su.ID, &su.SessionID, &su.Request, &su.Investigated, &su.Learned, &su.Completed, &su.NextSteps, &su.Notes, &su.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &su, err
}

// UnsummarizedSessions returns ended or idle sessions of a project that have memories but no summary.
func (s *Store) UnsummarizedSessions(ctx context.Context, idleBefore int64, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.id FROM sessions s
		WHERE (s.summarized_at IS NULL OR s.summarized_at < s.last_event_at) AND s.origin IS NULL
		AND (s.ended_at IS NOT NULL OR s.last_event_at < ?)
		AND EXISTS (SELECT 1 FROM events e WHERE e.session_id = s.id AND e.kind = 'prompt')
		ORDER BY s.last_event_at DESC LIMIT ?`, idleBefore, limit)
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

func (s *Store) MarkSummarized(ctx context.Context, sessionID string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET summarized_at = ? WHERE id = ?`, time.Now().UnixMilli(), sessionID)
		return err
	})
}

// SessionPrompts returns the user prompts of a session, oldest first.
func (s *Store) SessionPrompts(ctx context.Context, sessionID string, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT input FROM events WHERE session_id = ? AND kind = 'prompt' AND input IS NOT NULL
		ORDER BY id LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// --- work state -----------------------------------------------------------

type WorkItem struct {
	Task   string         `json:"task,omitempty"`
	Fields map[string]any `json:"fields"`
}

type WorkList struct {
	List  string         `json:"list"`
	State map[string]any `json:"state,omitempty"`
	Items []WorkItem     `json:"items,omitempty"`
	Open  bool           `json:"open"`
}

func AppendWorkTx(ctx context.Context, tx *sql.Tx, projectID int64, list string, fields map[string]any, ts int64, origin, originID string) (bool, error) {
	b, err := json.Marshal(fields)
	if err != nil {
		return false, err
	}
	var pid any
	if projectID != 0 {
		pid = projectID
	}
	return InsertReturning(ctx, tx, `INSERT INTO work_entries(project_id, list, fields, created_at, origin, origin_id)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, pid, list, string(b), ts, nullStr(origin), nullStr(originID))
}

func (s *Store) AppendWork(ctx context.Context, projectID int64, list string, fields map[string]any) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := AppendWorkTx(ctx, tx, projectID, list, fields, time.Now().UnixMilli(), "", "")
		return err
	})
}

// WorkLists folds the append-only log: later keys win, null clears a key, status done/dropped
// closes a task (or the whole list when set on list state).
func (s *Store) WorkLists(ctx context.Context, projectID int64, list string, includeClosed bool) ([]WorkList, error) {
	q := `SELECT list, fields FROM work_entries WHERE project_id IS ?`
	args := []any{nullInt(projectID)}
	if list != "" {
		q += ` AND list = ?`
		args = append(args, list)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type acc struct {
		state map[string]any
		tasks map[string]map[string]any
		order []string
	}
	lists := map[string]*acc{}
	var order []string
	for rows.Next() {
		var name, raw string
		if err := rows.Scan(&name, &raw); err != nil {
			return nil, err
		}
		var f map[string]any
		if json.Unmarshal([]byte(raw), &f) != nil {
			continue
		}
		a := lists[name]
		if a == nil {
			a = &acc{state: map[string]any{}, tasks: map[string]map[string]any{}}
			lists[name] = a
			order = append(order, name)
		}
		target := a.state
		if t, ok := f["task"].(string); ok && t != "" {
			if a.tasks[t] == nil {
				a.tasks[t] = map[string]any{}
				a.order = append(a.order, t)
			}
			target = a.tasks[t]
		}
		for k, v := range f {
			if v == nil {
				delete(target, k)
			} else {
				target[k] = v
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	closed := func(m map[string]any) bool {
		st, _ := m["status"].(string)
		return st == "done" || st == "dropped"
	}
	var out []WorkList
	for _, name := range order {
		a := lists[name]
		wl := WorkList{List: name, State: a.state}
		openTasks := 0
		for _, t := range a.order {
			f := a.tasks[t]
			if closed(f) {
				if !includeClosed {
					continue
				}
			} else {
				openTasks++
			}
			wl.Items = append(wl.Items, WorkItem{Task: t, Fields: f})
		}
		wl.Open = !closed(a.state) || openTasks > 0
		if !wl.Open && !includeClosed {
			continue
		}
		out = append(out, wl)
	}
	return out, nil
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// LastAssistant returns the newest final assistant message recorded for a session.
func (s *Store) LastAssistant(ctx context.Context, sessionID string) (string, error) {
	var out string
	err := s.db.QueryRowContext(ctx, `SELECT coalesce(output, '') FROM events WHERE session_id = ? AND kind = 'turn_end'
		AND output IS NOT NULL ORDER BY id DESC LIMIT 1`, sessionID).Scan(&out)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return out, err
}
