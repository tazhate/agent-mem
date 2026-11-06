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
