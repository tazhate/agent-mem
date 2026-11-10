package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tazhate/agent-mem/internal/config"
	"github.com/tazhate/agent-mem/internal/event"
	"github.com/tazhate/agent-mem/internal/recall"
	"github.com/tazhate/agent-mem/internal/redact"
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
	got, err := ingestCommit(s, ctx, start, true)
	if err != nil || !strings.Contains(got, "#") || !strings.Contains(got, "Key memories") {
		t.Fatalf("session start: %v %q", err, got)
	}
	// Already injected at start: the prompt must not repeat it.
	p := ev(event.Prompt)
	p.Prompt = "почему скрипты берут ключи из файлов когда vault запечатанный?"
	if got, _ := ingestCommit(s, ctx, p, true); strings.Contains(got, "запечатан") {
		t.Fatalf("dedupe failed: %q", got)
	}

	// Fresh session: the prompt finds it.
	p2 := &event.Event{Runtime: "claude-code", NativeSession: "s2", Cwd: repo, Kind: event.Prompt, ID: event.NewID(), Prompt: p.Prompt}
	got, _ = ingestCommit(s, ctx, p2, true)
	if !strings.Contains(got, "запечатан") {
		t.Fatalf("prompt recall missed: %q", got)
	}

	// Reading the file in a third session surfaces the file memory.
	in, _ := json.Marshal(map[string]string{"file_path": filepath.Join(repo, "scripts/vault_env.py")})
	r := &event.Event{Runtime: "claude-code", NativeSession: "s3", Cwd: repo, Kind: event.ToolPre, ID: event.NewID(), Tool: &event.Tool{Name: "Read", Input: in}}
	if got, _ := ingestCommit(s, ctx, r, true); !strings.Contains(got, "vault_env.py") && !strings.Contains(got, "запечатан") {
		t.Fatalf("file recall missed: %q", got)
	}

	// Citing the id at turn end counts as use.
	end := &event.Event{Runtime: "claude-code", NativeSession: "s2", Cwd: repo, Kind: event.TurnEnd, ID: event.NewID(), LastAssistant: "see #" + itoa(id)}
	if _, err := ingestCommit(s, ctx, end, true); err != nil {
		t.Fatal(err)
	}
	m, _ := s.st.GetMemories(ctx, []int64{id})
	if m[0].UseCount != 1 || m[0].InjectCount < 2 {
		t.Fatalf("feedback: use=%d inject=%d", m[0].UseCount, m[0].InjectCount)
	}

	// A failing command finds the gotcha.
	fail := &event.Event{Runtime: "claude-code", NativeSession: "s4", Cwd: repo, Kind: event.ToolFail, ID: event.NewID(),
		Tool: &event.Tool{Name: "Bash", Input: json.RawMessage(`{"command":"python3 scripts/inventory.py"}`), Error: "401 Key not found: vault запечатан, ключи из файлов устарели"}}
	if got, _ := ingestCommit(s, ctx, fail, true); !strings.Contains(got, "seen before") {
		t.Fatalf("error recall missed: %q", got)
	}

	// Duplicate delivery of the same event id is ignored.
	if _, err := ingestCommit(s, ctx, end, true); err != nil {
		t.Fatal(err)
	}
	var n int
	s.st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = 'turn_end'`).Scan(&n)
	if n != 1 {
		t.Fatalf("duplicate event stored: %d", n)
	}
}

func TestProjectForAdoptsLegacyPlainDirectory(t *testing.T) {
	s, _ := newServer(t, "on")
	ctx := t.Context()
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.Mkdir(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	legacyID, err := s.st.EnsureProject(ctx, "legacy:plain", "plain", "")
	if err != nil {
		t.Fatal(err)
	}
	memoryID, _, err := s.st.InsertMemory(ctx, &store.Memory{ProjectID: legacyID, Type: "discovery", Title: "imported fact"})
	if err != nil {
		t.Fatal(err)
	}
	projectID, info, err := s.projectFor(ctx, plain)
	if err != nil {
		t.Fatal(err)
	}
	if info.Git || projectID == legacyID {
		t.Fatalf("plain project resolution: %+v, id=%d legacy=%d", info, projectID, legacyID)
	}
	var gotProject int64
	if err := s.st.DB().QueryRow(`SELECT project_id FROM memories WHERE id = ?`, memoryID).Scan(&gotProject); err != nil {
		t.Fatal(err)
	}
	if gotProject != projectID {
		t.Fatalf("memory project = %d, want %d", gotProject, projectID)
	}
	var legacyRows, aliasRows int
	s.st.DB().QueryRow(`SELECT count(*) FROM projects WHERE id = ?`, legacyID).Scan(&legacyRows)
	s.st.DB().QueryRow(`SELECT count(*) FROM project_aliases WHERE alias = 'legacy:plain' AND project_id = ?`, projectID).Scan(&aliasRows)
	if legacyRows != 0 || aliasRows != 1 {
		t.Fatalf("legacy merge left projects=%d aliases=%d", legacyRows, aliasRows)
	}
}

func TestIngestCwdChurnKeepsSessionProject(t *testing.T) {
	s, _ := newServer(t, "on")
	ctx := t.Context()
	root := filepath.Join(t.TempDir(), "plain-root")
	child := filepath.Join(root, "child")
	grandchild := filepath.Join(child, "grandchild")
	if err := os.MkdirAll(grandchild, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, cwd := range []string{root, child, grandchild} {
		ev := &event.Event{Runtime: "claude-code", NativeSession: "churn", Cwd: cwd, Kind: event.ToolPost, ID: event.NewID(),
			Tool: &event.Tool{Name: "Bash", Input: json.RawMessage(`{"command":"pwd"}`)}, TS: int64(i + 1)}
		if i == 0 {
			ev.Kind = event.SessionStart
			ev.Tool = nil
		}
		if _, err := ingestCommit(s, ctx, ev, false); err != nil {
			t.Fatal(err)
		}
	}
	var projects int
	if err := s.st.DB().QueryRow(`SELECT count(*) FROM projects`).Scan(&projects); err != nil || projects != 1 {
		t.Fatalf("projects after cwd changes = %d, %v", projects, err)
	}
	sess, err := s.st.GetSession(ctx, "claude-code:churn")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.st.ProjectByID(ctx, sess.ProjectID)
	if err != nil || p.Key != root {
		t.Fatalf("session project = %+v, %v; want %s", p, err, root)
	}
	other := &event.Event{Runtime: "claude-code", NativeSession: "other", Cwd: child, Kind: event.SessionStart, ID: event.NewID(), TS: 4}
	if _, err := ingestCommit(s, ctx, other, false); err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB().QueryRow(`SELECT count(*) FROM projects`).Scan(&projects); err != nil || projects != 2 {
		t.Fatalf("a distinct session did not get its plain-directory project: %d, %v", projects, err)
	}
}

func TestShadowMode(t *testing.T) {
	s, repo := newServer(t, "shadow")
	seed(t, s, repo)
	p := &event.Event{Runtime: "codex", NativeSession: "x", Cwd: repo, Kind: event.Prompt, ID: event.NewID(),
		Prompt: "vault запечатан и скрипты читают ключи из файлов"}
	if got, _ := ingestCommit(s, t.Context(), p, true); got != "" {
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
	ingestCommit(s, t.Context(), p, true)
	tool := &event.Event{Runtime: "claude-code", NativeSession: "p", Cwd: repo, Kind: event.ToolPost, ID: event.NewID(),
		Tool: &event.Tool{Name: "Bash", Input: json.RawMessage(`{"command":"export GITHUB_TOKEN=` + "ghp_" + `abcdefghijklmnopqrstuvwxyz0123456789"}`), Output: "ok"}}
	ingestCommit(s, t.Context(), tool, true)
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
	ingestCommit(s, ctx, pub, true)
	priv := mk(event.Prompt)
	priv.Prompt = "<private>customer data</private>"
	ingestCommit(s, ctx, priv, true)
	// Tool events arrive in separate deliveries, long after the prompt.
	for range 3 {
		tool := mk(event.ToolPost)
		tool.Tool = &event.Tool{Name: "Bash", Input: json.RawMessage(`{"command":"cat customers.csv"}`), Output: "s3cret-row"}
		ingestCommit(s, ctx, tool, true)
	}
	end := mk(event.TurnEnd)
	end.LastAssistant = "the customer owes 4200"
	ingestCommit(s, ctx, end, true)
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
	ingestCommit(s, ctx, pub2, true)
	tool := mk(event.ToolPost)
	tool.Tool = &event.Tool{Name: "Bash", Input: json.RawMessage(`{"command":"make"}`), Output: "ok-visible"}
	ingestCommit(s, ctx, tool, true)
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
	ingestCommit(s, t.Context(), e, true)
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

// ingestCommit runs ingest and commits injections as if the hook received the answer.
func ingestCommit(s *Server, ctx context.Context, ev *event.Event, live bool) (string, error) {
	text, _, commit, err := s.ingest(ctx, ev, live)
	if commit != nil {
		commit()
	}
	return text, err
}

func TestRestartOnlyForOwnBinary(t *testing.T) {
	s, _ := newServer(t, "on")
	if s.shouldRestart(&Request{Version: "other", Exe: "/somewhere/else/amem"}) {
		t.Fatal("another installed copy must not restart the daemon")
	}
	if !s.shouldRestart(&Request{Version: "other", Exe: selfExe(), Built: time.Now().UTC().Format(time.RFC3339)}) {
		t.Fatal("a newer build of the same binary should take over")
	}
	if s.shouldRestart(&Request{Version: "other", Exe: selfExe(), Built: "2020-01-01T00:00:00Z"}) && BuildTime != "" {
		t.Fatal("an older build still running in a long-lived process must not restart the daemon")
	}
	if s.shouldRestart(&Request{Version: "other", Exe: selfExe()}) {
		t.Fatal("a client without build time must not restart the daemon")
	}
	if s.shouldRestart(&Request{Version: Version, Exe: selfExe()}) || !s.shouldRestart(&Request{Op: OpRestart}) {
		t.Fatal("restart rules")
	}
}

func TestDaemonEnvKeepsReferencedSecrets(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-referenced")
	cfg := config.Default()
	cfg.Observer.Providers = []config.ProviderConfig{{Kind: "anthropic", Key: "env:ANTHROPIC_AUTH_TOKEN"}}
	if !strings.Contains(strings.Join(DaemonEnv(cfg), "\n"), "tok-referenced") {
		t.Fatal("a variable named by an env: reference was dropped")
	}
}

func TestDaemonEnvDropsSessionRedirects(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "https://api.z.ai/api/anthropic")
	t.Setenv("ANTHROPIC_DEFAULT_HAIKU_MODEL", "glm-5.3-flash")
	t.Setenv("ANTHROPIC_API_KEY", "kept-for-env-refs")
	env := strings.Join(DaemonEnv(nil), "\n")
	if strings.Contains(env, "z.ai") || strings.Contains(env, "glm-5.3") || !strings.Contains(env, "AMEM_DISABLE=1") || !strings.Contains(env, "kept-for-env-refs") {
		t.Fatalf("daemon env:\n%s", env)
	}
}

func TestSpooledPrivatePromptStaysPrivate(t *testing.T) {
	s, repo := newServer(t, "on")
	ctx := t.Context()
	cfg := config.Default()
	prompt := &event.Event{Runtime: "claude-code", NativeSession: "sp", Cwd: repo, Kind: event.Prompt, ID: event.NewID(), Prompt: "<private>secret plan</private>", TS: 1000}
	Sanitize(cfg, prompt) // what the hook does before writing the spool
	ingestCommit(s, ctx, prompt, false)
	tool := &event.Tool{Name: "Bash", Input: json.RawMessage(`{"command":"cat plan"}`), Output: "plan-content"}
	ingestCommit(s, ctx, &event.Event{Runtime: "claude-code", NativeSession: "sp", Cwd: repo, Kind: event.ToolPost, ID: event.NewID(), Tool: tool, TS: 2000}, false)
	// A late public prompt from before (TS 500) must not end the private turn.
	ingestCommit(s, ctx, &event.Event{Runtime: "claude-code", NativeSession: "sp", Cwd: repo, Kind: event.Prompt, ID: event.NewID(), Prompt: "earlier public question", TS: 500}, false)
	tool2 := &event.Tool{Name: "Bash", Input: json.RawMessage(`{"command":"cat plan2"}`), Output: "plan-content-2"}
	ingestCommit(s, ctx, &event.Event{Runtime: "claude-code", NativeSession: "sp", Cwd: repo, Kind: event.ToolPost, ID: event.NewID(), Tool: tool2, TS: 3000}, false)
	var dump string
	s.st.DB().QueryRow(`SELECT group_concat(coalesce(input,'') || coalesce(output,''), '|') FROM events`).Scan(&dump)
	if strings.Contains(dump, "plan-content") || strings.Contains(dump, "secret plan") {
		t.Fatalf("private turn leaked after spool replay: %s", dump)
	}
}

func TestPrivateBlockSurvivesElision(t *testing.T) {
	r := redact.New(true)
	text := strings.Repeat("public ", 3000) + "<private>" + strings.Repeat("confidential ", 2000) + "</private> tail"
	out := CleanText(r, true, text, 16000)
	if strings.Contains(out, "confidential") {
		t.Fatal("private content survived elision")
	}
}

func TestPrivateKeyCutByElision(t *testing.T) {
	r := redact.New(true)
	// The tail cut (last 30% of the limit) lands inside the key, past its BEGIN line.
	key := "-----BEGIN RSA " + "PRIVATE KEY-----\n" + strings.Repeat("MIIEowIBAAKCAQEAkeyMaterialLine0123456789abcdef\n", 200) + "-----END RSA " + "PRIVATE KEY-----\n"
	text := strings.Repeat("noise ", 4000) + key + strings.Repeat("x", 1000)
	out := CleanText(r, true, text, 16000)
	if strings.Contains(out, "keyMaterialLine") {
		t.Fatal("key body survived after elision cut its BEGIN line")
	}
}

func TestNestedWorkFieldsRedacted(t *testing.T) {
	s, repo := newServer(t, "on")
	var lists []store.WorkList
	err := s.CallLocal(t.Context(), OpWorkW, WorkParams{List: "ops", Cwd: repo,
		Fields: map[string]any{"task": "x", "details": map[string]any{"note": "password=hunter2hunter2", "items": []any{"token: abcdefgh12345"}}}}, &lists)
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	s.st.DB().QueryRow(`SELECT fields FROM work_entries`).Scan(&raw)
	if strings.Contains(raw, "hunter2") || strings.Contains(raw, "abcdefgh12345") {
		t.Fatalf("nested work fields: %s", raw)
	}
}

func TestStartSummaryDisplay(t *testing.T) {
	s, repo := newServer(t, "on")
	seed(t, s, repo)
	_, display, _, err := s.ingest(t.Context(), &event.Event{Runtime: "claude-code", NativeSession: "d1", Cwd: repo, Kind: event.SessionStart, Source: "startup", ID: event.NewID()}, true)
	if err != nil || !strings.Contains(display, "1 memories") || !strings.Contains(display, "запечатан") {
		t.Fatalf("display: %v %q", err, display)
	}
	if !strings.Contains(display, "\x1b[") {
		t.Fatalf("summary is not colored by default: %q", display)
	}
	plain := s.cfg.Recall
	plain.StartSummaryColor = false
	s.rec = recall.New(s.st, plain, nil)
	_, display, _, err = s.ingest(t.Context(), &event.Event{Runtime: "claude-code", NativeSession: "d2", Cwd: repo, Kind: event.SessionStart, Source: "startup", ID: event.NewID()}, true)
	if err != nil || display == "" || strings.Contains(display, "\x1b") {
		t.Fatalf("plain display: %v %q", err, display)
	}
}
