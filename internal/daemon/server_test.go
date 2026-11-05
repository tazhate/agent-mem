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
		Tool: &event.Tool{Name: "Bash", Input: json.RawMessage(`{"command":"export GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123456789"}`), Output: "ok"}}
	s.ingest(t.Context(), tool, true)
	var dump string
	s.st.DB().QueryRow(`SELECT group_concat(coalesce(input,'') || coalesce(output,'')) FROM events`).Scan(&dump)
	if strings.Contains(dump, "hunter2") || strings.Contains(dump, "ghp_abcdef") {
		t.Fatalf("secret stored: %s", dump)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
