package daemon

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tazhate/agent-mem/internal/config"
	"github.com/tazhate/agent-mem/internal/event"
	"github.com/tazhate/agent-mem/internal/store"
)

func newServer(t *testing.T, mode string) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.Recall.Mode = mode
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil))), repo
}

func seed(t *testing.T, s *Server, repo string) int64 {
	t.Helper()
	pid, _, err := s.projectFor(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := s.st.InsertMemory(t.Context(), &store.Memory{ProjectID: pid, Type: "gotcha",
		Title: "Vault запечатан: скрипты молча читают ключи из файлов", Body: "Без APP_VAULT=1 скрипт берет устаревшие ключи",
		Files: []string{filepath.Join(repo, "scripts/vault_env.py")}})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRecallFlow(t *testing.T) {
	s, repo := newServer(t, "on")
	ctx := t.Context()
	id := seed(t, s, repo)
	ev := func(kind event.Kind) *event.Event {
		return &event.Event{Runtime: "claude-code", NativeSession: "s1", Cwd: repo, Kind: kind, ID: event.NewID()}
	}

	start := ev(event.SessionStart)
	start.Source = "startup"
	got, err := s.ingest(ctx, start, true)
	if err != nil || !strings.Contains(got, "#") || !strings.Contains(got, "Key memories") {
		t.Fatalf("session start: %v %q", err, got)
	}
	// Already injected at start: the prompt must not repeat it.
	p := ev(event.Prompt)
	p.Prompt = "почему скрипты берут ключи из файлов когда vault запечатанный?"
	if got, _ := s.ingest(ctx, p, true); strings.Contains(got, "запечатан") {
		t.Fatalf("dedupe failed: %q", got)
	}

	// Fresh session: the prompt finds it.
	p2 := &event.Event{Runtime: "claude-code", NativeSession: "s2", Cwd: repo, Kind: event.Prompt, ID: event.NewID(), Prompt: p.Prompt}
	got, _ = s.ingest(ctx, p2, true)
	if !strings.Contains(got, "запечатан") {
		t.Fatalf("prompt recall missed: %q", got)
	}

	// Reading the file in a third session surfaces the file memory.
	in, _ := json.Marshal(map[string]string{"file_path": filepath.Join(repo, "scripts/vault_env.py")})
	r := &event.Event{Runtime: "claude-code", NativeSession: "s3", Cwd: repo, Kind: event.ToolPre, ID: event.NewID(), Tool: &event.Tool{Name: "Read", Input: in}}
	if got, _ := s.ingest(ctx, r, true); !strings.Contains(got, "vault_env.py") && !strings.Contains(got, "запечатан") {
		t.Fatalf("file recall missed: %q", got)
	}

	// Citing the id at turn end counts as use.
	end := &event.Event{Runtime: "claude-code", NativeSession: "s2", Cwd: repo, Kind: event.TurnEnd, ID: event.NewID(), LastAssistant: "see #" + itoa(id)}
	if _, err := s.ingest(ctx, end, true); err != nil {
		t.Fatal(err)
	}
	m, _ := s.st.GetMemories(ctx, []int64{id})
	if m[0].UseCount != 1 || m[0].InjectCount < 2 {
		t.Fatalf("feedback: use=%d inject=%d", m[0].UseCount, m[0].InjectCount)
	}

	// A failing command finds the gotcha.
	fail := &event.Event{Runtime: "claude-code", NativeSession: "s4", Cwd: repo, Kind: event.ToolFail, ID: event.NewID(),
		Tool: &event.Tool{Name: "Bash", Input: json.RawMessage(`{"command":"python3 scripts/inventory.py"}`), Error: "401 Key not found: vault запечатан, ключи из файлов устарели"}}
	if got, _ := s.ingest(ctx, fail, true); !strings.Contains(got, "seen before") {
		t.Fatalf("error recall missed: %q", got)
	}

	// Duplicate delivery of the same event id is ignored.
	if _, err := s.ingest(ctx, end, true); err != nil {
		t.Fatal(err)
	}
	var n int
	s.st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = 'turn_end'`).Scan(&n)
	if n != 1 {
		t.Fatalf("duplicate event stored: %d", n)
	}
}

func TestShadowMode(t *testing.T) {
	s, repo := newServer(t, "shadow")
	seed(t, s, repo)
	p := &event.Event{Runtime: "codex", NativeSession: "x", Cwd: repo, Kind: event.Prompt, ID: event.NewID(),
		Prompt: "vault запечатан и скрипты читают ключи из файлов"}
	if got, _ := s.ingest(t.Context(), p, true); got != "" {
		t.Fatalf("shadow mode injected: %q", got)
	}
	rows, _ := s.st.ShadowLog(t.Context(), 0, 10)
	if len(rows) == 0 {
		t.Fatal("shadow injection not logged")
	}
}

func TestPrivacy(t *testing.T) {
	s, repo := newServer(t, "on")
	p := &event.Event{Runtime: "claude-code", NativeSession: "p", Cwd: repo, Kind: event.Prompt, ID: event.NewID(),
		Prompt: "<private>my password is hunter2hunter2</private>"}
	s.ingest(t.Context(), p, true)
	tool := &event.Event{Runtime: "claude-code", NativeSession: "p", Cwd: repo, Kind: event.ToolPost, ID: event.NewID(),
		Tool: &event.Tool{Name: "Bash", Input: json.RawMessage(`{"command":"export GITHUB_TOKEN=` + "ghp_" + `abcdefghijklmnopqrstuvwxyz0123456789"}`), Output: "ok"}}
	s.ingest(t.Context(), tool, true)
	var dump string
	s.st.DB().QueryRow(`SELECT group_concat(coalesce(input,'') || coalesce(output,'')) FROM events`).Scan(&dump)
	if strings.Contains(dump, "hunter2") || strings.Contains(dump, "ghp_abcdef") {
		t.Fatalf("secret stored: %s", dump)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestPrivateTurnAcrossBatches(t *testing.T) {
	s, repo := newServer(t, "on")
	ctx := t.Context()
	mk := func(kind event.Kind) *event.Event {
		return &event.Event{Runtime: "claude-code", NativeSession: "pv", Cwd: repo, Kind: kind, ID: event.NewID()}
	}
	pub := mk(event.Prompt)
	pub.Prompt = "public question about the build"
	s.ingest(ctx, pub, true)
	priv := mk(event.Prompt)
	priv.Prompt = "<private>customer data</private>"
	s.ingest(ctx, priv, true)
	// Tool events arrive in separate deliveries, long after the prompt.
	for range 3 {
		tool := mk(event.ToolPost)
		tool.Tool = &event.Tool{Name: "Bash", Input: json.RawMessage(`{"command":"cat customers.csv"}`), Output: "s3cret-row"}
		s.ingest(ctx, tool, true)
	}
	end := mk(event.TurnEnd)
	end.LastAssistant = "the customer owes 4200"
	s.ingest(ctx, end, true)
	var dump string
	s.st.DB().QueryRow(`SELECT group_concat(coalesce(input,'') || coalesce(output,''), '|') FROM events`).Scan(&dump)
	for _, leak := range []string{"customer data", "s3cret-row", "owes 4200", "customers.csv"} {
		if strings.Contains(dump, leak) {
			t.Fatalf("private turn content stored: %q in %s", leak, dump)
		}
	}
	last, _ := s.st.LastAssistant(ctx, "claude-code:pv")
	if strings.Contains(last, "4200") {
		t.Fatal("summary input would contain the private answer")
	}
	// The next public prompt ends the private state.
	pub2 := mk(event.Prompt)
	pub2.Prompt = "back to the build"
	s.ingest(ctx, pub2, true)
	tool := mk(event.ToolPost)
	tool.Tool = &event.Tool{Name: "Bash", Input: json.RawMessage(`{"command":"make"}`), Output: "ok-visible"}
	s.ingest(ctx, tool, true)
	s.st.DB().QueryRow(`SELECT group_concat(coalesce(output,''), '|') FROM events`).Scan(&dump)
	if !strings.Contains(dump, "ok-visible") {
		t.Fatal("public turn after a private one was not stored")
	}
}

func TestEscapedPrivateInToolInput(t *testing.T) {
	s, repo := newServer(t, "on")
	// What json.Marshal makes of <private> inside a RawMessage.
	in := json.RawMessage(`{"command":"echo <private>top-secret</private> done"}`)
	e := &event.Event{Runtime: "claude-code", NativeSession: "esc", Cwd: repo, Kind: event.ToolPost, ID: event.NewID(),
		Tool: &event.Tool{Name: "Bash", Input: in, Output: "x"}}
	s.ingest(t.Context(), e, true)
	var stored string
	s.st.DB().QueryRow(`SELECT input FROM events WHERE tool = 'Bash'`).Scan(&stored)
	if strings.Contains(stored, "top-secret") || !strings.Contains(stored, "done") {
		t.Fatalf("stored input: %s", stored)
	}
}

func TestPrivateMemoriesNotServed(t *testing.T) {
	s, repo := newServer(t, "on")
	ctx := t.Context()
	pid, _, _ := s.projectFor(ctx, repo)
	id, _, _ := s.st.InsertMemory(ctx, &store.Memory{ProjectID: pid, SessionID: "claude-code:x", Type: "discovery", Title: "sensitive thing", Private: true})
	var got []*store.Memory
	if err := s.CallLocal(ctx, OpGet, GetParams{IDs: []int64{id}}, &got); err != nil || len(got) != 0 {
		t.Fatalf("memory_get served a private memory: %v %d", err, len(got))
	}
	if err := s.CallLocal(ctx, OpGet, GetParams{IDs: []int64{id}, IncludePrivate: true}, &got); err != nil || len(got) != 1 {
		t.Fatalf("CLI path lost private memory: %v %d", err, len(got))
	}
	if m, _ := s.st.SessionMemories(ctx, "claude-code:x", 10); len(m) != 0 {
		t.Fatal("SessionMemories returned a private memory")
	}
}

func TestSaveAndWorkStateRedacted(t *testing.T) {
	s, repo := newServer(t, "on")
	ctx := t.Context()
	var out map[string]any
	if err := s.CallLocal(ctx, OpSave, SaveParams{Text: "deploy notes", Title: "token ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789", Cwd: repo}, &out); err != nil {
		t.Fatal(err)
	}
	var lists []store.WorkList
	s.CallLocal(ctx, OpWorkW, WorkParams{List: "ops", Fields: map[string]any{"task": "rotate", "note": "password=hunter2hunter2 <private>x</private>"}, Cwd: repo}, &lists)
	var dump string
	s.st.DB().QueryRow(`SELECT (SELECT group_concat(title) FROM memories) || (SELECT group_concat(fields) FROM work_entries)`).Scan(&dump)
	if strings.Contains(dump, "ghp_abcdef") || strings.Contains(dump, "hunter2") || strings.Contains(dump, "<private>") {
		t.Fatalf("unredacted: %s", dump)
	}
}
