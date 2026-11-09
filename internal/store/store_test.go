package store

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tazhate/agent-mem/internal/redact"
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

func TestScrubJSONField(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	// A key without END used to swallow the rest of the JSON, the edit was rejected as invalid
	// JSON and the other secret in the same field stayed.
	in := `{"command":"cat id_rsa","files":["-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNz","token glpat-` + `abcdefghij1234567890"]}`
	s.Write(ctx, func(tx *sql.Tx) error {
		s.InsertEvent(ctx, tx, EventRow{SessionID: "x", Kind: "tool_post", Tool: "Bash", Input: in, TS: 1})
		return nil
	})
	r := redact.New(true)
	if _, err := s.Scrub(ctx, r.Clean, false); err != nil {
		t.Fatal(err)
	}
	var got string
	s.DB().QueryRow(`SELECT input FROM events`).Scan(&got)
	if !json.Valid([]byte(got)) || strings.Contains(got, "b3BlbnNz") || strings.Contains(got, "glpat-abc") || !strings.Contains(got, "cat id_rsa") {
		t.Fatalf("scrubbed JSON: %s", got)
	}
}

func TestScrubRebuildsStemDF(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	pid, _ := s.EnsureProject(ctx, "k", "n", "")
	s.InsertMemory(ctx, &Memory{ProjectID: pid, Type: "gotcha", Title: "db password zqxsecretword"})
	var n int
	s.DB().QueryRow(`SELECT count(*) FROM stem_df WHERE term LIKE 'zqxsecret%'`).Scan(&n)
	if n == 0 {
		t.Fatal("precondition: stem indexed")
	}
	s.Scrub(ctx, func(v string) string { return strings.ReplaceAll(v, "zqxsecretword", "[REDACTED]") }, false)
	s.DB().QueryRow(`SELECT count(*) FROM stem_df WHERE term LIKE 'zqxsecret%'`).Scan(&n)
	if n != 0 {
		t.Fatal("secret stem still in stem_df after scrub")
	}
}

func TestClosedListHidesOpenTasks(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	pid, _ := s.EnsureProject(ctx, "k", "n", "")
	s.AppendWork(ctx, pid, "release", map[string]any{"task": "tag", "status": "todo"})
	s.AppendWork(ctx, pid, "release", map[string]any{"status": "dropped"})
	if lists, _ := s.WorkLists(ctx, pid, "", false); len(lists) != 0 {
		t.Fatalf("dropped list still shown: %+v", lists)
	}
}

func TestScopedQueries(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	pid, _ := s.EnsureProject(ctx, "k", "p", "")
	other, _ := s.EnsureProject(ctx, "o", "o", "")
	same, _, _ := s.InsertMemory(ctx, &Memory{ProjectID: pid, Type: "gotcha", Title: "same file elsewhere", Files: []string{"/old/checkout/cmd/x/main.go"}})
	s.InsertMemory(ctx, &Memory{ProjectID: pid, Type: "gotcha", Title: "another main.go", Files: []string{"cmd/y/main.go"}})
	global, _, _ := s.InsertMemory(ctx, &Memory{Type: "preference", Title: "global rule", Files: []string{"x/main.go"}})
	top, _, _ := s.InsertMemory(ctx, &Memory{ProjectID: pid, Type: "gotcha", Title: "makefile target", Files: []string{"Makefile"}})
	uni, _, _ := s.InsertMemory(ctx, &Memory{ProjectID: pid, Type: "gotcha", Title: "cyrillic dir", Files: []string{"/a/проект/main.go"}})
	s.InsertMemory(ctx, &Memory{ProjectID: other, Type: "decision", Title: "other project", Files: []string{"x/main.go"}})
	mems, err := s.ByFiles(ctx, []string{"/repo/cmd/x/main.go"}, Scope{ProjectID: pid}, 10)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, m := range mems {
		ids = append(ids, m.ID)
	}
	if len(ids) != 2 || !slices.Contains(ids, same) || !slices.Contains(ids, global) {
		t.Fatalf("ByFiles: %v", ids)
	}
	for path, want := range map[string]int64{"/home/u/repo/Makefile": top, "/b/проект/main.go": uni} {
		mems, err := s.ByFiles(ctx, []string{path}, Scope{ProjectID: pid}, 10)
		if err != nil || len(mems) != 1 || mems[0].ID != want {
			t.Fatalf("ByFiles(%s): %v %v", path, err, mems)
		}
	}
	for _, q := range []func() ([]*Memory, error){
		func() ([]*Memory, error) { return s.Durable(ctx, Scope{ProjectID: pid}, 10) },
		func() ([]*Memory, error) { return s.Recent(ctx, Scope{ProjectID: pid}, 10) },
	} {
		mems, err := q()
		if err != nil || len(mems) != 5 {
			t.Fatalf("scoped query: %v %d", err, len(mems))
		}
		for _, m := range mems {
			if m.ProjectID == other {
				t.Fatal("other project leaked into scope")
			}
		}
	}
}

func TestResumedSessionNotResummarized(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	now := time.Now().UnixMilli()
	// Ended a day ago, summarized then, resumed a minute ago and still active.
	for _, q := range []string{
		`INSERT INTO sessions(id, runtime, native_id, started_at, last_event_at, ended_at, summarized_at) VALUES ('r', 'x', 'r', ?1 - 86400000, ?1 - 60000, ?1 - 86000000, ?1 - 86000000)`,
		`INSERT INTO sessions(id, runtime, native_id, started_at, last_event_at, ended_at) VALUES ('e', 'x', 'e', ?1 - 3600000, ?1 - 60000, ?1 - 60000)`,
		`INSERT INTO events(session_id, kind, ts) VALUES ('r', 'prompt', ?1), ('e', 'prompt', ?1)`,
	} {
		if _, err := s.DB().ExecContext(ctx, q, now); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := s.UnsummarizedSessions(ctx, now-15*60_000, 10)
	if err != nil || !slices.Equal(ids, []string{"e"}) {
		t.Fatalf("unsummarized: %v %v", err, ids)
	}
}
