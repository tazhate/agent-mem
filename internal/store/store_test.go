package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tazhate/agent-mem/internal/stem"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMemoryRoundTrip(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	pid, err := s.EnsureProject(ctx, "/repo/.git", "repo", "/repo")
	if err != nil {
		t.Fatal(err)
	}
	m := &Memory{ProjectID: pid, Type: "gotcha", Title: "Vault запечатан - скрипты молча берут файл",
		Body: "Без APP_VAULT=1 скрипты читают ключи из файлов", Files: []string{"/repo/scripts/vault_env.py"}}
	id, created, err := s.InsertMemory(ctx, m)
	if err != nil || !created {
		t.Fatalf("insert: %v %v", err, created)
	}
	if _, created, _ := s.InsertMemory(ctx, &Memory{ProjectID: pid, Type: "gotcha", Title: m.Title, Body: m.Body}); created {
		t.Fatal("duplicate created")
	}
	hits, err := s.SearchFTS(ctx, stem.Query("почему запечатанный vault"), Scope{ProjectID: pid}, 10)
	if err != nil || len(hits) != 1 || hits[0].ID != id {
		t.Fatalf("search: %v %+v", err, hits)
	}
	byFile, err := s.ByFiles(ctx, []string{"/other/checkout/scripts/vault_env.py"}, Scope{ProjectID: pid}, 5)
	if err != nil || len(byFile) != 1 {
		t.Fatalf("by file: %v %d", err, len(byFile))
	}
	if err := s.UpdateMemory(ctx, id, "", "новое тело про токен", nil); err != nil {
		t.Fatal(err)
	}
	hits, _ = s.SearchFTS(ctx, stem.Query("токены"), Scope{ProjectID: pid}, 10)
	if len(hits) != 1 {
		t.Fatalf("reindex failed: %+v", hits)
	}
	other, _ := s.EnsureProject(ctx, "/other/.git", "other", "")
	if hits, _ := s.SearchFTS(ctx, stem.Query("vault"), Scope{ProjectID: other}, 10); len(hits) != 0 {
		t.Fatal("project scope leaked")
	}
}

func TestWorkFold(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	pid, _ := s.EnsureProject(ctx, "k", "n", "")
	for _, f := range []map[string]any{
		{"task": "a", "status": "todo"},
		{"task": "b", "status": "todo", "note": "x"},
		{"task": "a", "status": "done"},
		{"task": "b", "note": nil},
	} {
		if err := s.AppendWork(ctx, pid, "l", f); err != nil {
			t.Fatal(err)
		}
	}
	lists, err := s.WorkLists(ctx, pid, "", false)
	if err != nil || len(lists) != 1 || len(lists[0].Items) != 1 || lists[0].Items[0].Task != "b" {
		t.Fatalf("%v %+v", err, lists)
	}
	if _, ok := lists[0].Items[0].Fields["note"]; ok {
		t.Fatal("null did not clear")
	}
}

func TestPruneKeepsPrompts(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	s.Write(ctx, func(tx *sql.Tx) error {
		s.InsertEvent(ctx, tx, EventRow{SessionID: "x", Kind: "prompt", Input: "hi", TS: 1})
		s.InsertEvent(ctx, tx, EventRow{SessionID: "x", Kind: "tool_post", Tool: "Bash", TS: 1})
		return nil
	})
	s.DB().Exec(`UPDATE events SET processed = 1`)
	n, err := s.PruneEvents(ctx, 100)
	var left int
	s.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = 'prompt'`).Scan(&left)
	if err != nil || n != 1 || left != 1 {
		t.Fatalf("pruned %d, prompts left %d, err %v", n, left, err)
	}
}

func TestImmediateTxWaitsInsteadOfBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.db")
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx := t.Context()
	release := make(chan struct{})
	held := make(chan struct{})
	go a.Write(ctx, func(tx *sql.Tx) error {
		tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES ('a', '1')`)
		close(held)
		<-release
		return nil
	})
	<-held
	go func() { time.Sleep(300 * time.Millisecond); close(release) }()
	// A read-then-write transaction in a second connection must wait for the lock, not fail.
	err = b.Write(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM events`).Scan(&n); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES ('b', '1')`)
		return err
	})
	if err != nil {
		t.Fatalf("second writer failed instead of waiting: %v", err)
	}
}

func TestScrub(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	pid, _ := s.EnsureProject(ctx, "k", "n", "")
	s.InsertMemory(ctx, &Memory{ProjectID: pid, Type: "gotcha", Title: "db url postgres://u:hunter2pw@h/db", Body: "plain"})
	s.Write(ctx, func(tx *sql.Tx) error {
		s.InsertEvent(ctx, tx, EventRow{SessionID: "x", Kind: "prompt", Input: "use postgres://u:hunter2pw@h/db", TS: 1})
		return nil
	})
	// A summary whose only content is a secret becomes '' and must not violate NOT NULL.
	s.InsertSummary(ctx, &Summary{SessionID: "x", ProjectID: pid, Request: "hunter2pw", Completed: "hunter2pw"})
	clean := func(v string) string {
		if v == "hunter2pw" {
			return ""
		}
		return strings.ReplaceAll(v, "hunter2pw", "[REDACTED]")
	}
	rep, err := s.Scrub(ctx, clean, true)
	if err != nil || rep.Memories != 1 || rep.Events != 1 || rep.Summaries != 1 {
		t.Fatalf("dry run: %+v %v", rep, err)
	}
	if _, err := s.Scrub(ctx, clean, false); err != nil {
		t.Fatal(err)
	}
	var dump string
	s.DB().QueryRow(`SELECT (SELECT group_concat(title) FROM memories) || (SELECT group_concat(input) FROM events)`).Scan(&dump)
	if strings.Contains(dump, "hunter2pw") {
		t.Fatalf("not scrubbed: %s", dump)
	}
	if hits, _ := s.SearchFTS(ctx, []string{"redact"}, Scope{ProjectID: pid}, 5); len(hits) != 1 {
		t.Fatal("FTS not reindexed after scrub")
	}
}
