package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tazhate/agent-mem/internal/config"
)

var schema = map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "required": []string{"ok"}}

func TestAnthropicFallback(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("x-api-key") != "k" || r.URL.Path != "/v1/messages" {
			w.WriteHeader(401)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "output_config") {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":{"message":"output_config: not supported"}}`))
			return
		}
		w.Write([]byte(`{"content":[{"type":"text","text":"Here:\n` + "```json\\n{\\\"ok\\\":true}\\n```" + `"}]}`))
	}))
	defer srv.Close()
	p, _ := New(t.Context(), config.ProviderConfig{Kind: "anthropic", BaseURL: srv.URL, Key: "k"})
	out, err := p.Complete(t.Context(), Request{System: "s", User: "u", Schema: schema})
	if err != nil || string(out) != `{"ok":true}` || calls.Load() != 2 {
		t.Fatalf("out=%s err=%v calls=%d", out, err, calls.Load())
	}
}

func TestAnthropicGatewayIgnoresOutputConfig(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			System any `json:"system"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(400)
			return
		}
		// A gateway (z.ai with GLM) accepts output_config with a 200 and silently ignores it:
		// the schema must reach the model through the system prompt in that first request.
		var system string
		switch s := req.System.(type) {
		case string:
			system = s
		case []any:
			for _, part := range s {
				if m, _ := part.(map[string]any); m != nil {
					system += fmt.Sprint(m["text"])
				}
			}
		}
		if !strings.Contains(system, `"ok"`) {
			t.Errorf("first request carried no prompt-side schema: %q", system)
		}
		w.Write([]byte(`{"content":[{"type":"text","text":"Here:\n` + "```json\\n{\\\"ok\\\":true}\\n```" + `"}]}`))
	}))
	defer srv.Close()
	p, _ := New(t.Context(), config.ProviderConfig{Kind: "anthropic", BaseURL: srv.URL, Key: "k"})
	out, err := p.Complete(t.Context(), Request{System: "s", User: "u", Schema: schema})
	if err != nil || string(out) != `{"ok":true}` || calls.Load() != 1 {
		t.Fatalf("out=%s err=%v calls=%d", out, err, calls.Load())
	}
}

func TestOpenAIAndChain(t *testing.T) {
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) }))
	defer fail.Close()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if rf, _ := req["response_format"].(map[string]any); rf["type"] != "json_schema" {
			t.Errorf("response_format: %v", rf)
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":true}"}}]}`))
	}))
	defer ok.Close()
	ch, err := NewChain(t.Context(), []config.ProviderConfig{
		{Name: "a", Kind: "openai", BaseURL: fail.URL},
		{Name: "b", Kind: "openai", BaseURL: ok.URL, Key: "env:AMEM_TEST_KEY_UNSET_OK"},
		{Name: "c", Kind: "openai", BaseURL: ok.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ch.Name() != "a>c" {
		t.Fatalf("misconfigured provider not skipped: %s", ch.Name())
	}
	out, err := ch.Complete(t.Context(), Request{Schema: schema})
	if err != nil || string(out) != `{"ok":true}` {
		t.Fatalf("%s %v", out, err)
	}
}

func TestExec(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-cli")
	// Echoes a claude -p style envelope and proves the schema placeholder and env were set.
	os.WriteFile(script, []byte(`#!/bin/sh
cat > /dev/null
[ "$AMEM_DISABLE" = 1 ] || exit 3
case "$2" in *'"ok"'*) ;; *) exit 4;; esac
echo '{"type":"result","is_error":false,"result":"x","structured_output":{"ok":true}}'
`), 0o755)
	p, _ := newProvider(t.Context(), config.ProviderConfig{Kind: "exec", Command: []string{script, "--json-schema", PhSchema, "--model", PhModel}}, dir)
	out, err := p.Complete(t.Context(), Request{System: "s", User: "u", Schema: schema})
	if err != nil || string(out) != `{"ok":true}` {
		t.Fatalf("%s %v", out, err)
	}
}

func TestExecUsesStableWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	scratch := filepath.Join(dir, "tmp")
	projects := filepath.Join(dir, "projects")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", scratch)
	t.Setenv("AMEM_TEST_CWD_LOG", filepath.Join(dir, "cwds"))
	t.Setenv("AMEM_TEST_PROJECTS", projects)
	script := filepath.Join(dir, "fake-cli")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
cat > /dev/null
printf '%s\n' "$PWD" >> "$AMEM_TEST_CWD_LOG"
mkdir -p "$AMEM_TEST_PROJECTS/$(pwd | tr '/' '-')"
echo '{"ok":true}'
`), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := newProvider(t.Context(), config.ProviderConfig{Kind: "exec", Command: []string{script}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := p.Complete(t.Context(), Request{Schema: schema}); err != nil {
			t.Fatal(err)
		}
	}
	body, err := os.ReadFile(filepath.Join(dir, "cwds"))
	if err != nil {
		t.Fatal(err)
	}
	cwds := strings.Split(strings.TrimSpace(string(body)), "\n")
	workDir := filepath.Join(dir, "exec-cwd")
	if len(cwds) != 2 || cwds[0] != workDir || cwds[1] != workDir {
		t.Fatalf("working directories = %q, want two calls in %q", cwds, workDir)
	}
	entries, err := os.ReadDir(projects)
	if err != nil || len(entries) != 1 {
		t.Fatalf("fake CLI project directories = %d, %v", len(entries), err)
	}
	leftovers, err := os.ReadDir(scratch)
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("per-call scratch directories left behind = %d, %v", len(leftovers), err)
	}
	if st, err := os.Stat(workDir); err != nil || !st.IsDir() {
		t.Fatalf("stable working directory removed: %v", err)
	}
}

func TestExtractJSONProseWithBraces(t *testing.T) {
	for in, want := range map[string]string{
		`Here is {the} answer: {"ok":true}`: `{"ok":true}`,
		"{} then {\"ok\":true}":             `{"ok":true}`,
		"```json\n{\"a\":{\"b\":1}}\n```":   `{"a":{"b":1}}`,
		`{}`:                                `{}`,
	} {
		got, err := extractJSON(in)
		if err != nil || string(got) != want {
			t.Errorf("extractJSON(%q) = %s, %v; want %s", in, got, err, want)
		}
	}
}

func TestOpenAIMaxCompletionTokens(t *testing.T) {
	var sawLegacy bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if _, ok := req["max_tokens"]; ok {
			sawLegacy = true
		}
		if _, ok := req["max_completion_tokens"]; !ok {
			t.Error("max_completion_tokens not sent")
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":true}"}}]}`))
	}))
	defer srv.Close()
	p, _ := New(t.Context(), config.ProviderConfig{Kind: "openai", BaseURL: srv.URL})
	if _, err := p.Complete(t.Context(), Request{Schema: schema, MaxTokens: 100}); err != nil || sawLegacy {
		t.Fatalf("err=%v legacy=%v", err, sawLegacy)
	}
}

func TestExecKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "hang")
	// A grandchild keeps stdout open; without killing the group Run would wait for it.
	os.WriteFile(script, []byte("#!/bin/sh\nsleep 30 &\nsleep 30\n"), 0o755)
	p, _ := newProvider(t.Context(), config.ProviderConfig{Kind: "exec", Command: []string{script}}, dir)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := p.Complete(ctx, Request{Schema: schema}); err == nil {
		t.Fatal("expected timeout error")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("exec timeout took %s", d)
	}
}

func TestChainPerProviderTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer slow.Close()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":true}"}}]}`))
	}))
	defer ok.Close()
	ch, _ := NewChain(t.Context(), []config.ProviderConfig{{Kind: "openai", BaseURL: slow.URL}, {Kind: "openai", BaseURL: ok.URL}})
	ch.Timeout = 300 * time.Millisecond
	if out, err := ch.Complete(t.Context(), Request{Schema: schema}); err != nil || string(out) != `{"ok":true}` {
		t.Fatalf("fallback after timeout: %s %v", out, err)
	}
}

func TestClaudePresetUsesSubscriptionEnv(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "claude") // the preset is recognized by the command name
	os.WriteFile(script, []byte(`#!/bin/sh
cat > /dev/null
[ -z "$ANTHROPIC_BASE_URL$ANTHROPIC_API_KEY$ANTHROPIC_DEFAULT_HAIKU_MODEL" ] || { echo "leaked: $ANTHROPIC_BASE_URL" >&2; exit 5; }
echo '{"type":"result","is_error":false,"structured_output":{"ok":true}}'
`), 0o755)
	t.Setenv("ANTHROPIC_BASE_URL", "https://api.z.ai/api/anthropic")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("ANTHROPIC_DEFAULT_HAIKU_MODEL", "glm-5.3-flash")
	p, _ := newProvider(t.Context(), config.ProviderConfig{Kind: "exec", Command: []string{script}}, dir)
	if out, err := p.Complete(t.Context(), Request{Schema: schema}); err != nil || string(out) != `{"ok":true}` {
		t.Fatalf("%s %v", out, err)
	}
}

func TestAccountTruncationAndMaxTokens(t *testing.T) {
	var status atomic.Int32
	var sentMax atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			MaxTokens int64 `json:"max_tokens"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		sentMax.Store(req.MaxTokens)
		if code := int(status.Load()); code != 200 {
			w.WriteHeader(code)
			w.Write([]byte(`{"error":{"message":"invalid x-api-key"}}`))
			return
		}
		w.Write([]byte(`{"content":[{"type":"text","text":"{\"ok\":"}],"stop_reason":"max_tokens"}`))
	}))
	defer srv.Close()
	p, _ := New(t.Context(), config.ProviderConfig{Kind: "anthropic", BaseURL: srv.URL, MaxTokens: 50})
	status.Store(401)
	_, err := p.Complete(t.Context(), Request{Schema: schema, MaxTokens: 4096})
	if pe, ok := errors.AsType[*Error](err); !ok || !pe.Retryable {
		t.Fatalf("401 must keep the batch (retryable): %v", err)
	}
	if sentMax.Load() != 50 {
		t.Fatalf("config max_tokens ignored: sent %d", sentMax.Load())
	}
	status.Store(200)
	_, err = p.Complete(t.Context(), Request{Schema: schema})
	if pe, ok := errors.AsType[*Error](err); !ok || pe.Retryable || !errors.Is(err, errTruncated) {
		t.Fatalf("truncated output must be permanent: %v", err)
	}
}

func TestExecLingeringChildKeepsAnswer(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "linger")
	// Answers and exits 0, but a background child keeps stdout open past WaitDelay.
	os.WriteFile(script, []byte("#!/bin/sh\necho '{\"ok\":true}'\nsleep 30 &\nexit 0\n"), 0o755)
	p, _ := newProvider(t.Context(), config.ProviderConfig{Kind: "exec", Command: []string{script}}, dir)
	out, err := p.Complete(t.Context(), Request{Schema: schema})
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("answer lost: %v %s", err, out)
	}
}
