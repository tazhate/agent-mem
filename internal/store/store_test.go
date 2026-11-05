package store

import (
	"path/filepath"
	"testing"

	"agent-mem/internal/stem"
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
