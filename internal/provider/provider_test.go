package provider

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

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
	p, _ := New(t.Context(), config.ProviderConfig{Kind: "exec", Command: []string{script, "--json-schema", PhSchema, "--model", PhModel}})
	out, err := p.Complete(t.Context(), Request{System: "s", User: "u", Schema: schema})
	if err != nil || string(out) != `{"ok":true}` {
		t.Fatalf("%s %v", out, err)
	}
}
