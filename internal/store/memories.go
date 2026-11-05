package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/tazhate/agent-mem/internal/stem"
)

// Types is the closed set of memory types.
var Types = []string{"decision", "bugfix", "gotcha", "change", "discovery", "preference", "todo"}

func ValidType(t string) bool {
	for _, v := range Types {
		if v == t {
			return true
		}
	}
	return false
}

type Memory struct {
	ID           int64             `json:"id"`
	ProjectID    int64             `json:"project_id,omitzero"` // 0 = global
	Project      string            `json:"project,omitempty"`
	SessionID    string            `json:"session_id,omitempty"`
	Turn         int               `json:"turn,omitzero"`
	Type         string            `json:"type"`
	Title        string            `json:"title"`
	Body         string            `json:"body,omitempty"`
	Facts        []string          `json:"facts,omitempty"`
	Files        []string          `json:"files,omitempty"`
	Concepts     []string          `json:"concepts,omitempty"`
	Private      bool              `json:"private,omitzero"`
	CreatedAt    int64             `json:"created_at"`
	UpdatedAt    int64             `json:"updated_at"`
	LastUsedAt   int64             `json:"last_used_at,omitzero"`
	UseCount     int               `json:"use_count"`
	InjectCount  int               `json:"inject_count"`
	SupersededBy int64             `json:"superseded_by,omitzero"`
	Origin       string            `json:"origin,omitempty"`
	OriginID     string            `json:"origin_id,omitempty"`
	Meta         map[string]string `json:"meta,omitempty"`
}

// Tokens estimates how many tokens the full record costs when fetched.
func (m *Memory) Tokens() int {
	n := len(m.Title) + len(m.Body)
	for _, f := range m.Facts {
		n += len(f) + 3
	}
	return max(1, n/4)
}

// Hash is the dedupe key: same project, type, title and body means the same memory.
func (m *Memory) Hash() string {
	h := sha256.New()
	fmt.Fprintf(h, "%d\x00%s\x00%s\x00%s", m.ProjectID, m.Type, norm(m.Title), norm(m.Body))
	return hex.EncodeToString(h.Sum(nil))[:24]
}

func norm(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}

func jsonList(v []string) string {
	if len(v) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func parseList(s string) []string {
	var v []string
	_ = json.Unmarshal([]byte(s), &v)
	return v
}

// ftsText is what gets stemmed into the index body column.
func ftsBody(m *Memory) string {
	var b strings.Builder
	b.WriteString(m.Body)
	for _, f := range m.Facts {
		b.WriteString("\n")
		b.WriteString(f)
	}
	for _, f := range m.Files {
		b.WriteString(" ")
		b.WriteString(filepath.Base(f))
	}
	for _, c := range m.Concepts {
		b.WriteString(" ")
		b.WriteString(c)
	}
	b.WriteString(" ")
	b.WriteString(m.Type)
	return b.String()
}

// InsertMemoryTx stores a memory; an identical memory (same hash) or the same origin row
// returns the existing id with created=false.
func InsertMemoryTx(ctx context.Context, tx *sql.Tx, m *Memory) (id int64, created bool, err error) {
	now := time.Now().UnixMilli()
	if m.CreatedAt == 0 {
		m.CreatedAt = now
	}
	if m.UpdatedAt == 0 {
		m.UpdatedAt = m.CreatedAt
	}
	meta := "{}"
	if len(m.Meta) > 0 {
		b, _ := json.Marshal(m.Meta)
		meta = string(b)
	}
	var pid any
	if m.ProjectID != 0 {
		pid = m.ProjectID
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO memories(project_id, session_id, turn, type, title, body, facts, files, concepts,
		hash, private, created_at, updated_at, origin, origin_id, meta)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING RETURNING id`,
		pid, nullStr(m.SessionID), m.Turn, m.Type, m.Title, m.Body, jsonList(m.Facts), jsonList(m.Files), jsonList(m.Concepts),
		m.Hash(), m.Private, m.CreatedAt, m.UpdatedAt, nullStr(m.Origin), nullStr(m.OriginID), meta).Scan(&id)
	if err == sql.ErrNoRows {
		err = tx.QueryRowContext(ctx, `SELECT id FROM memories WHERE hash = ?
			UNION ALL SELECT id FROM memories WHERE origin = ? AND origin_id = ? LIMIT 1`,
			m.Hash(), m.Origin, m.OriginID).Scan(&id)
		if err != nil {
			return id, false, err
		}
		// Learned again after being replaced: the same knowledge is current again.
		if !m.Private && m.Origin != "claude-mem" {
			res, err := tx.ExecContext(ctx, `UPDATE memories SET superseded_by = NULL, updated_at = ? WHERE id = ? AND superseded_by IS NOT NULL`, now, id)
			if err != nil {
				return id, false, err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				return id, true, nil
			}
		}
		return id, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	m.ID = id
	title, body := stem.Text(m.Title), stem.Text(ftsBody(m))
	if _, err = tx.ExecContext(ctx, `INSERT INTO memories_fts(rowid, title, body) VALUES (?, ?, ?)`, id, title, body); err != nil {
		return 0, false, err
	}
	if err = bumpDF(ctx, tx, title+" "+body); err != nil {
		return 0, false, err
	}
	for _, f := range m.Files {
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO memory_files(memory_id, path, base) VALUES (?, ?, ?)`, id, f, filepath.Base(f)); err != nil {
			return 0, false, err
		}
	}
	return id, true, nil
}

func (s *Store) InsertMemory(ctx context.Context, m *Memory) (int64, bool, error) {
	var id int64
	var created bool
	err := s.Write(ctx, func(tx *sql.Tx) error {
		var err error
		id, created, err = InsertMemoryTx(ctx, tx, m)
		return err
	})
	return id, created, err
}

// UpdateMemory rewrites body and facts of an existing memory and reindexes it.
func (s *Store) UpdateMemory(ctx context.Context, id int64, title, body string, facts []string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		m, err := getMemoryTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if title != "" {
			m.Title = title
		}
		if body != "" {
			m.Body = body
		}
		if facts != nil {
			m.Facts = facts
		}
		if _, err := tx.ExecContext(ctx, `UPDATE memories SET title = ?, body = ?, facts = ?, hash = ?, updated_at = ? WHERE id = ?`,
			m.Title, m.Body, jsonList(m.Facts), m.Hash(), time.Now().UnixMilli(), id); err != nil {
			return fmt.Errorf("update memory %d (an identical memory may exist): %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM memories_fts WHERE rowid = ?`, id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO memories_fts(rowid, title, body) VALUES (?, ?, ?)`,
			id, stem.Text(m.Title), stem.Text(ftsBody(m)))
		return err
	})
}

// Supersede marks old memories as replaced by newID; they leave recall but stay readable.
func (s *Store) Supersede(ctx context.Context, newID int64, old []int64) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		for _, id := range old {
			if id == newID {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE memories SET superseded_by = ? WHERE id = ? AND superseded_by IS NULL`, newID, id); err != nil {
				return err
			}
		}
		return nil
	})
}

const memoryCols = `m.id, coalesce(m.project_id, 0), coalesce(p.name, ''), coalesce(m.session_id, ''), coalesce(m.turn, 0),
	m.type, m.title, m.body, m.facts, m.files, m.concepts, m.private, m.created_at, m.updated_at,
	coalesce(m.last_used_at, 0), m.use_count, m.inject_count, coalesce(m.superseded_by, 0),
	coalesce(m.origin, ''), coalesce(m.origin_id, ''), m.meta`

const memoryFrom = ` FROM memories m LEFT JOIN projects p ON p.id = m.project_id `

type scanner interface{ Scan(...any) error }

func scanMemory(sc scanner) (*Memory, error) {
	var m Memory
	var facts, files, concepts, meta string
	if err := sc.Scan(&m.ID, &m.ProjectID, &m.Project, &m.SessionID, &m.Turn, &m.Type, &m.Title, &m.Body,
		&facts, &files, &concepts, &m.Private, &m.CreatedAt, &m.UpdatedAt, &m.LastUsedAt, &m.UseCount,
		&m.InjectCount, &m.SupersededBy, &m.Origin, &m.OriginID, &meta); err != nil {
		return nil, err
	}
	m.Facts, m.Files, m.Concepts = parseList(facts), parseList(files), parseList(concepts)
	if meta != "{}" {
		_ = json.Unmarshal([]byte(meta), &m.Meta)
	}
	return &m, nil
}

func getMemoryTx(ctx context.Context, tx *sql.Tx, id int64) (*Memory, error) {
	return scanMemory(tx.QueryRowContext(ctx, `SELECT `+memoryCols+memoryFrom+`WHERE m.id = ?`, id))
}

func (s *Store) queryMemories(ctx context.Context, q string, args ...any) ([]*Memory, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+memoryCols+memoryFrom+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetMemories returns non-private memories by id in the requested order. This is what reaches
// models (MCP memory_get, recall), so private records never leave the database this way.
func (s *Store) GetMemories(ctx context.Context, ids []int64) ([]*Memory, error) {
	return s.getMemories(ctx, ids, false)
}

// GetMemoriesAll includes private memories; for the local CLI only.
func (s *Store) GetMemoriesAll(ctx context.Context, ids []int64) ([]*Memory, error) {
	return s.getMemories(ctx, ids, true)
}

func (s *Store) getMemories(ctx context.Context, ids []int64, withPrivate bool) ([]*Memory, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph, args := placeholders(ids)
	q := `WHERE m.id IN (` + ph + `)`
	if !withPrivate {
		q += ` AND m.private = 0`
	}
	list, err := s.queryMemories(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]*Memory, len(list))
	for _, m := range list {
		byID[m.ID] = m
	}
	out := make([]*Memory, 0, len(list))
	for _, id := range ids {
		if m, ok := byID[id]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// Hit is an FTS candidate with its bm25 rank (lower is better, as SQLite reports it).
type Hit struct {
	ID   int64
	BM25 float64
}

// Scope limits a search: one project plus globals, or everything when ProjectID is 0 and All is set.
type Scope struct {
	ProjectID int64
	All       bool
	Types     []string
	// Since drops memories created before this unix ms time.
	Since int64
}

func (sc Scope) where(args *[]any) string {
	var b strings.Builder
	b.WriteString(` m.private = 0 AND m.superseded_by IS NULL`)
	if !sc.All {
		b.WriteString(` AND (m.project_id = ? OR m.project_id IS NULL)`)
		*args = append(*args, sc.ProjectID)
	}
	if len(sc.Types) > 0 {
		b.WriteString(` AND m.type IN (`)
		for i, t := range sc.Types {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString("?")
			*args = append(*args, t)
		}
		b.WriteString(")")
	}
	if sc.Since > 0 {
		b.WriteString(` AND m.created_at >= ?`)
		*args = append(*args, sc.Since)
	}
	return b.String()
}

// SearchFTS runs an OR query over stems, title weighted 3x.
func (s *Store) SearchFTS(ctx context.Context, stems []string, sc Scope, limit int) ([]Hit, error) {
	if len(stems) == 0 {
		return nil, nil
	}
	quoted := make([]string, len(stems))
	for i, st := range stems {
		quoted[i] = `"` + strings.ReplaceAll(st, `"`, `""`) + `"`
	}
	args := []any{strings.Join(quoted, " OR ")}
	q := `SELECT m.id, bm25(memories_fts, 3.0, 1.0) AS r FROM memories_fts JOIN memories m INDEXED BY memories_scope ON m.id = memories_fts.rowid
		WHERE memories_fts MATCH ? AND` + sc.where(&args) + ` ORDER BY r LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.ID, &h.BM25); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ByFiles returns live memories that mention any of the paths (exact or same basename suffix).
func (s *Store) ByFiles(ctx context.Context, paths []string, sc Scope, limit int) ([]*Memory, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	args := []any{}
	var cond []string
	for _, p := range paths {
		cond = append(cond, `f.path = ? OR f.base = ?`)
		args = append(args, p, filepath.Base(p))
	}
	where := sc.where(&args)
	args = append(args, limit)
	return s.queryMemories(ctx, `WHERE m.id IN (SELECT f.memory_id FROM memory_files f WHERE `+strings.Join(cond, " OR ")+`)
		AND`+where+` ORDER BY m.created_at DESC LIMIT ?`, args...)
}

// Durable returns the most useful long-lived memories of a project (decisions, gotchas, preferences).
func (s *Store) Durable(ctx context.Context, sc Scope, limit int) ([]*Memory, error) {
	sc.Types = []string{"decision", "gotcha", "preference"}
	args := []any{}
	where := sc.where(&args)
	args = append(args, limit)
	return s.queryMemories(ctx, `WHERE`+where+`
		ORDER BY (m.use_count * 3 - max(0, m.inject_count - m.use_count)) DESC, m.created_at DESC LIMIT ?`, args...)
}

// Recent returns the newest memories in scope.
func (s *Store) Recent(ctx context.Context, sc Scope, limit int) ([]*Memory, error) {
	args := []any{}
	where := sc.where(&args)
	args = append(args, limit)
	return s.queryMemories(ctx, `WHERE`+where+` ORDER BY m.created_at DESC LIMIT ?`, args...)
}

// SessionMemories returns memories produced in a session, newest first.
func (s *Store) SessionMemories(ctx context.Context, sessionID string, limit int) ([]*Memory, error) {
	return s.queryMemories(ctx, `WHERE m.session_id = ? AND m.superseded_by IS NULL AND m.private = 0 ORDER BY m.created_at DESC LIMIT ?`, sessionID, limit)
}

// Timeline returns memories of the same project around an anchor memory.
func (s *Store) Timeline(ctx context.Context, anchor int64, before, after int) ([]*Memory, error) {
	var pid sql.NullInt64
	var ts int64
	if err := s.db.QueryRowContext(ctx, `SELECT project_id, created_at FROM memories WHERE id = ?`, anchor).Scan(&pid, &ts); err != nil {
		return nil, err
	}
	older, err := s.queryMemories(ctx, `WHERE m.project_id IS ? AND m.created_at < ? AND m.private = 0 ORDER BY m.created_at DESC LIMIT ?`, pid, ts, before)
	if err != nil {
		return nil, err
	}
	newer, err := s.queryMemories(ctx, `WHERE m.project_id IS ? AND m.created_at >= ? AND m.private = 0 ORDER BY m.created_at ASC LIMIT ?`, pid, ts, after+1)
	if err != nil {
		return nil, err
	}
	out := make([]*Memory, 0, len(older)+len(newer))
	for i := len(older) - 1; i >= 0; i-- {
		out = append(out, older[i])
	}
	return append(out, newer...), nil
}

func placeholders(ids []int64) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

// DocFreq returns how many memories contain each stem, plus the total memory count.
func (s *Store) DocFreq(ctx context.Context, stems []string) (map[string]int, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT coalesce(max(id), 0) FROM memories`).Scan(&total); err != nil {
		return nil, 0, err
	}
	out := make(map[string]int, len(stems))
	if len(stems) == 0 {
		return out, total, nil
	}
	for _, t := range stems {
		var n int
		err := s.db.QueryRowContext(ctx, `SELECT doc FROM stem_df WHERE term = ?`, t).Scan(&n)
		if err != nil && err != sql.ErrNoRows {
			return nil, 0, err
		}
		out[t] = n
	}
	return out, total, nil
}

// OptimizeFTS merges index segments; run after bulk imports.
func (s *Store) OptimizeFTS(ctx context.Context) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO memories_fts(memories_fts) VALUES ('optimize')`)
		return err
	})
}

// MergeFTS does a bounded amount of incremental segment merging.
func (s *Store) MergeFTS(ctx context.Context) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO memories_fts(memories_fts, rank) VALUES ('merge', 500)`)
		return err
	})
}

func bumpDF(ctx context.Context, tx *sql.Tx, stems string) error {
	seen := map[string]bool{}
	for t := range strings.FieldsSeq(stems) {
		if seen[t] {
			continue
		}
		seen[t] = true
		if _, err := tx.ExecContext(ctx, `INSERT INTO stem_df(term, doc) VALUES (?, 1)
			ON CONFLICT(term) DO UPDATE SET doc = doc + 1`, t); err != nil {
			return err
		}
	}
	return nil
}

// RebuildDF recomputes stem_df from the FTS vocabulary in one pass.
func (s *Store) RebuildDF(ctx context.Context) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM stem_df`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO stem_df(term, doc) SELECT term, doc FROM memories_vocab`)
		return err
	})
}

// ScrubReport counts rows changed by Scrub.
type ScrubReport struct {
	Memories, Summaries, Events, WorkEntries int
}

// Scrub re-applies a cleaning function to stored text (after redaction rules improve) and
// reindexes changed memories. dryRun only counts.
func (s *Store) Scrub(ctx context.Context, clean func(string) string, dryRun bool) (ScrubReport, error) {
	var rep ScrubReport
	var last int64
	for {
		mems, err := s.queryMemories(ctx, `WHERE m.id > ? ORDER BY m.id LIMIT 2000`, last)
		if err != nil {
			return rep, err
		}
		if len(mems) == 0 {
			break
		}
		last = mems[len(mems)-1].ID
		var changed []*Memory
		for _, m := range mems {
			t, b := clean(m.Title), clean(m.Body)
			facts := make([]string, len(m.Facts))
			diff := t != m.Title || b != m.Body
			for i, f := range m.Facts {
				facts[i] = clean(f)
				diff = diff || facts[i] != f
			}
			if diff {
				m.Title, m.Body, m.Facts = t, b, facts
				changed = append(changed, m)
			}
		}
		rep.Memories += len(changed)
		if dryRun || len(changed) == 0 {
			continue
		}
		if err := s.Write(ctx, func(tx *sql.Tx) error {
			for _, m := range changed {
				if _, err := tx.ExecContext(ctx, `UPDATE memories SET title = ?, body = ?, facts = ? WHERE id = ?`,
					m.Title, m.Body, jsonList(m.Facts), m.ID); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM memories_fts WHERE rowid = ?`, m.ID); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO memories_fts(rowid, title, body) VALUES (?, ?, ?)`,
					m.ID, stem.Text(m.Title), stem.Text(ftsBody(m))); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return rep, err
		}
	}
	for _, t := range []struct {
		table string
		cols  []string
		count *int
	}{
		{"summaries", []string{"request", "investigated", "learned", "completed", "next_steps", "notes"}, &rep.Summaries},
		{"events", []string{"input", "output", "error"}, &rep.Events},
		{"work_entries", []string{"fields"}, &rep.WorkEntries},
	} {
		n, err := s.scrubTable(ctx, t.table, t.cols, clean, dryRun)
		if err != nil {
			return rep, err
		}
		*t.count = n
	}
	return rep, nil
}

func (s *Store) scrubTable(ctx context.Context, table string, cols []string, clean func(string) string, dryRun bool) (int, error) {
	sel := make([]string, len(cols))
	for i, c := range cols {
		sel[i] = "coalesce(" + c + ", '')"
	}
	total := 0
	var last int64
	for {
		rows, err := s.db.QueryContext(ctx, `SELECT id, `+strings.Join(sel, ", ")+` FROM `+table+` WHERE id > ? ORDER BY id LIMIT 5000`, last)
		if err != nil {
			return total, err
		}
		type upd struct {
			id   int64
			vals []string
		}
		var batch []upd
		n := 0
		for rows.Next() {
			vals := make([]string, len(cols))
			dst := []any{&last}
			for i := range vals {
				dst = append(dst, &vals[i])
			}
			if err := rows.Scan(dst...); err != nil {
				rows.Close()
				return total, err
			}
			n++
			diff := false
			for i, v := range vals {
				c := clean(v)
				// Never turn a valid JSON value (work_entries.fields, tool input) into invalid JSON.
				if c != v && (!json.Valid([]byte(v)) || json.Valid([]byte(c))) {
					vals[i], diff = c, true
				}
			}
			if diff {
				batch = append(batch, upd{last, vals})
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return total, err
		}
		total += len(batch)
		if n == 0 {
			return total, nil
		}
		if dryRun || len(batch) == 0 {
			continue
		}
		set := make([]string, len(cols))
		for i, c := range cols {
			set[i] = c + " = nullif(?, '')"
		}
		if err := s.Write(ctx, func(tx *sql.Tx) error {
			for _, u := range batch {
				args := make([]any, 0, len(cols)+1)
				for _, v := range u.vals {
					args = append(args, v)
				}
				args = append(args, u.id)
				if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET `+strings.Join(set, ", ")+` WHERE id = ?`, args...); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return total, err
		}
	}
}

// ftsVersion changes whenever stemming or indexed text changes; a mismatch triggers Reindex.
const ftsVersion = "2" // 2: ё folded to е

func (s *Store) NeedsReindex(ctx context.Context) (bool, error) {
	v, err := s.Meta(ctx, "fts_version")
	if err != nil {
		return false, err
	}
	if v == "" {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM memories`).Scan(&n); err != nil {
			return false, err
		}
		if n == 0 { // fresh database: indexed with current rules from the start
			return false, s.SetMeta(ctx, "fts_version", ftsVersion)
		}
	}
	return v != ftsVersion, nil
}

// Reindex rebuilds the FTS index and stem frequencies from the memories table.
func (s *Store) Reindex(ctx context.Context) (int, error) {
	if err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM memories_fts`)
		return err
	}); err != nil {
		return 0, err
	}
	n := 0
	var last int64
	for {
		mems, err := s.queryMemories(ctx, `WHERE m.id > ? ORDER BY m.id LIMIT 2000`, last)
		if err != nil {
			return n, err
		}
		if len(mems) == 0 {
			break
		}
		last = mems[len(mems)-1].ID
		if err := s.Write(ctx, func(tx *sql.Tx) error {
			for _, m := range mems {
				if _, err := tx.ExecContext(ctx, `INSERT INTO memories_fts(rowid, title, body) VALUES (?, ?, ?)`,
					m.ID, stem.Text(m.Title), stem.Text(ftsBody(m))); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return n, err
		}
		n += len(mems)
	}
	if err := s.OptimizeFTS(ctx); err != nil {
		return n, err
	}
	if err := s.RebuildDF(ctx); err != nil {
		return n, err
	}
	return n, s.SetMeta(ctx, "fts_version", ftsVersion)
}
