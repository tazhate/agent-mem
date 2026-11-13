package recall

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/tazhate/agent-mem/internal/config"
	"github.com/tazhate/agent-mem/internal/event"
	"github.com/tazhate/agent-mem/internal/store"
)

func TestFatigue(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pid, _ := st.EnsureProject(ctx, "k", "p", "")
	tired, _, _ := st.InsertMemory(ctx, &store.Memory{ProjectID: pid, Type: "gotcha", Title: "vault sealed scripts read stale keys"})
	fresh, _, _ := st.InsertMemory(ctx, &store.Memory{ProjectID: pid, Type: "gotcha", Title: "vault sealed: scripts cannot read keys until unseal"})
	st.DB().Exec(`UPDATE memories SET inject_count = 12 WHERE id = ?`, tired)
	cfg := config.Default().Recall
	e := New(st, cfg, nil)
	ev := &event.Event{Kind: event.Prompt, Prompt: "why is vault sealed and what about the scripts keys"}
	res, err := e.Recall(ctx, Request{Event: ev, SessionID: "s", ProjectID: pid, ProjectName: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, "stale keys") || !strings.Contains(res.Text, "unseal") {
		t.Fatalf("fatigue not applied:\n%s", res.Text)
	}
	// Explicit search still finds it.
	hits, _ := e.Search(ctx, "vault sealed scripts stale keys", store.Scope{ProjectID: pid}, 5)
	found := false
	for _, h := range hits {
		found = found || h.Mem.ID == tired
	}
	if !found {
		t.Fatal("explicit search must still return tired memories")
	}
	// A single use lifts it.
	st.DB().Exec(`UPDATE memories SET use_count = 1 WHERE id = ?`, tired)
	res, _ = e.Recall(ctx, Request{Event: ev, SessionID: "s2", ProjectID: pid, ProjectName: "p"})
	if !strings.Contains(res.Text, "stale keys") {
		t.Fatalf("used memory should come back:\n%s", res.Text)
	}
	_ = fresh
}

func TestOneLineDropsControls(t *testing.T) {
	if got := oneLine("fix \x1b[31mred\x1b[0m\n  build\x07"); got != "fix [31mred[0m build" {
		t.Fatalf("oneLine: %q", got)
	}
}

func TestFatigueSparesRules(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pid, _ := st.EnsureProject(ctx, "k", "p", "")
	rule, _, _ := st.InsertMemory(ctx, &store.Memory{Type: "preference", Title: "answer in short sentences"})
	st.DB().Exec(`UPDATE memories SET inject_count = 50 WHERE id = ?`, rule)
	mems, err := st.Durable(ctx, store.Scope{ProjectID: pid, Fatigue: 10}, 30)
	if err != nil || len(mems) != 1 || mems[0].ID != rule {
		t.Fatalf("global preference hidden by fatigue: %v %v", err, mems)
	}
}

func TestCompactionResetsSeen(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pid, _ := st.EnsureProject(ctx, "k", "p", "")
	id, _, _ := st.InsertMemory(ctx, &store.Memory{ProjectID: pid, Type: "gotcha", Title: "vault sealed: scripts cannot read keys until unseal"})
	st.RecordInjections(ctx, "s", "prompt", []store.Injection{{MemoryID: id, Tokens: 10}}, false)
	if seen, _, _ := st.Injected(ctx, "s"); !seen[id] {
		t.Fatal("injection not seen")
	}
	e := New(st, config.Default().Recall, nil)
	res, err := e.Recall(ctx, Request{Event: &event.Event{Kind: event.SessionStart, Source: "compact"}, SessionID: "s", ProjectID: pid, ProjectName: "p"})
	if err != nil {
		t.Fatal(err)
	}
	st.RecordInjections(ctx, "s", res.Trigger, res.Items, false)
	if seen, spent, _ := st.Injected(ctx, "s"); seen[id] || spent != 0 {
		t.Fatalf("compaction did not reset seen: %v %d", seen, spent)
	}
}

func TestImportedWeight(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pid, _ := st.EnsureProject(ctx, "k", "p", "")
	// The imported memory proved useful once, so it ranks first unless it is scaled down.
	old, _, _ := st.InsertMemory(ctx, &store.Memory{ProjectID: pid, Type: "gotcha", Origin: "claude-mem", OriginID: "1",
		Title: "vault sealed: scripts read stale keys after restart", Files: []string{"/srv/vault.hcl"}})
	native, _, _ := st.InsertMemory(ctx, &store.Memory{ProjectID: pid, Type: "gotcha",
		Title: "vault sealed: scripts read keys only after unseal", Files: []string{"/srv/vault.hcl"}})
	st.DB().Exec(`UPDATE memories SET use_count = 1 WHERE id = ?`, old)
	prompt := &event.Event{Kind: event.Prompt, Prompt: "vault sealed and the scripts read keys"}
	order := func(w float64, ev *event.Event, session string) []int64 {
		cfg := config.Default().Recall
		cfg.ImportedWeight = w
		res, err := New(st, cfg, nil).Recall(ctx, Request{Event: ev, SessionID: session, ProjectID: pid, ProjectName: "p"})
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, it := range res.Items {
			ids = append(ids, it.MemoryID)
		}
		return ids
	}
	if got := order(1, prompt, "a"); len(got) != 2 || got[0] != old {
		t.Fatalf("weight 1: imported memory should lead, got %v", got)
	}
	if got := order(0.3, prompt, "b"); len(got) == 0 || got[0] != native {
		t.Fatalf("weight 0.3: native memory should lead, got %v", got)
	}
	if got := order(0, prompt, "c"); len(got) != 1 || got[0] != native {
		t.Fatalf("weight 0: imported memory must not be injected, got %v", got)
	}
	start := &event.Event{Kind: event.SessionStart}
	if got := order(0.3, start, "d"); len(got) != 2 || got[0] != native {
		t.Fatalf("session start, weight 0.3: native memory should lead, got %v", got)
	}
	if got := order(0, start, "e"); len(got) != 1 || got[0] != native {
		t.Fatalf("session start, weight 0: imported memory must not be listed, got %v", got)
	}
	read := &event.Event{Kind: event.ToolPre, Tool: &event.Tool{Name: "Read", Input: []byte(`{"file_path":"/srv/vault.hcl"}`)}}
	if got := order(0, read, "f"); len(got) != 1 || got[0] != native {
		t.Fatalf("file recall, weight 0: imported memory must not be injected, got %v", got)
	}
	// Explicit search ignores the weight.
	cfg := config.Default().Recall
	cfg.ImportedWeight = 0
	hits, _ := New(st, cfg, nil).Search(ctx, prompt.Prompt, store.Scope{ProjectID: pid}, 5)
	if len(hits) == 0 || hits[0].Mem.ID != old {
		t.Fatalf("explicit search must rank the imported memory as usual, got %v", hits)
	}
}
