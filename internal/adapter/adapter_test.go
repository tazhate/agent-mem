package adapter

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tazhate/agent-mem/internal/event"
)

func env(m map[string]string) Env { return func(k string) string { return m[k] } }

func TestClaudeCodeParse(t *testing.T) {
	a, _ := Get("claude-code")
	cases := []struct {
		raw  string
		kind event.Kind
		tool string
		fail bool
	}{
		{`{"session_id":"s","cwd":"/r","hook_event_name":"SessionStart","source":"compact"}`, event.SessionStart, "", false},
		{`{"session_id":"s","cwd":"/r","hook_event_name":"UserPromptSubmit","prompt":"fix it"}`, event.Prompt, "", false},
		{`{"session_id":"s","cwd":"/r","hook_event_name":"PreToolUse","tool_name":"Read","tool_input":{"file_path":"/r/a.go"}}`, event.ToolPre, "Read", false},
		{`{"session_id":"s","cwd":"/r","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"ls"},"tool_response":{"stdout":"x","stderr":"warn"}}`, event.ToolPost, "Bash", false},
		{`{"session_id":"s","cwd":"/r","hook_event_name":"PostToolUseFailure","tool_name":"Bash","tool_input":{"command":"false"},"error":"exit 1"}`, event.ToolFail, "Bash", true},
		{`{"session_id":"s","cwd":"/r","hook_event_name":"Stop","transcript_path":"/t.jsonl"}`, event.TurnEnd, "", false},
	}
	for _, c := range cases {
		ev, err := a.Parse("", []byte(c.raw), env(nil))
		if err != nil {
			t.Fatalf("%s: %v", c.raw, err)
		}
		if ev.Kind != c.kind || ev.SessionID() != "claude-code:s" || ev.Cwd != "/r" {
			t.Fatalf("%s: got %+v", c.raw, ev)
		}
		if c.tool != "" && (ev.Tool == nil || ev.Tool.Name != c.tool || ev.Tool.Failed() != c.fail) {
			t.Fatalf("%s: tool %+v", c.raw, ev.Tool)
		}
	}
	ev, _ := a.Parse("", []byte(`{"session_id":"s","cwd":"/x","hook_event_name":"PostToolUse","tool_name":"Bash","tool_response":{"stdout":"out","stderr":"err"}}`), env(map[string]string{"CLAUDE_PROJECT_DIR": "/proj"}))
	if ev.Cwd != "/proj" || !strings.Contains(ev.Tool.Output, "out") || !strings.Contains(ev.Tool.Output, "err") {
		t.Fatalf("cwd override / output: %+v %+v", ev, ev.Tool)
	}
}

func TestRender(t *testing.T) {
	cc, _ := Get("claude-code")
	ev := &event.Event{Kind: event.Prompt}
	var out struct {
		H struct {
			Name string `json:"hookEventName"`
			Ctx  string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(cc.Render(ev, "UserPromptSubmit", "mem"), &out); err != nil || out.H.Name != "UserPromptSubmit" || out.H.Ctx != "mem" {
		t.Fatalf("claude render: %v %+v", err, out)
	}
	if cc.Render(&event.Event{Kind: event.TurnEnd}, "Stop", "x") != nil {
		t.Fatal("Stop must not carry context")
	}
	if cc.Render(ev, "UserPromptSubmit", "") != nil {
		t.Fatal("empty context must print nothing")
	}
	cx, _ := Get("codex")
	if b := cx.Render(&event.Event{}, "SessionStart", ""); !strings.Contains(string(b), `"hookEventName":"SessionStart"`) {
		t.Fatalf("codex SessionStart needs envelope: %s", b)
	}
	cu, _ := Get("cursor")
	if b := cu.Render(&event.Event{}, "sessionStart", "m"); string(b) != `{"additional_context":"m"}` {
		t.Fatalf("cursor: %s", b)
	}
	if b := cu.Render(&event.Event{}, "beforeSubmitPrompt", "m"); string(b) != `{"continue":true}` {
		t.Fatalf("cursor prompt: %s", b)
	}
	long := strings.Repeat("я", MaxContextChars)
	if b := cc.Render(ev, "UserPromptSubmit", long); len(b) > MaxContextChars+200 {
		t.Fatalf("not truncated: %d", len(b))
	}
}

func TestOtherRuntimes(t *testing.T) {
	g, _ := Get("gemini")
	ev, err := g.Parse("", []byte(`{"session_id":"g","hook_event_name":"AfterTool","tool_name":"run_shell_command","tool_input":{"command":"go test"},"tool_response":{"output":"FAIL","exit_code":1}}`), env(map[string]string{"GEMINI_CWD": "/g"}))
	if err != nil || ev.Kind != event.ToolFail || ev.Cwd != "/g" {
		t.Fatalf("gemini: %v %+v", err, ev)
	}
	cu, _ := Get("cursor")
	ev, err = cu.Parse("", []byte(`{"conversation_id":"c1","workspace_roots":["/w"],"hook_event_name":"afterShellExecution","command":"make","output":"ok"}`), env(nil))
	if err != nil || ev.Tool.Name != "Bash" || ev.Cwd != "/w" || ev.NativeSession != "c1" {
		t.Fatalf("cursor: %v %+v", err, ev)
	}
	cx, _ := Get("codex")
	ev, err = cx.Parse("", []byte(`{"session_id":"x","cwd":"/c","hook_event_name":"Stop","turn_id":"t1","last_assistant_message":"done #12"}`), env(nil))
	if err != nil || ev.Kind != event.TurnEnd || ev.LastAssistant != "done #12" {
		t.Fatalf("codex: %v %+v", err, ev)
	}
	oc, _ := Get("opencode")
	ev, err = oc.Parse("", []byte(`{"kind":"tool_post","session_id":"o","cwd":"/o","tool":"bash","args":{"command":"x"},"output":"boom","error":"exit 2"}`), env(nil))
	if err != nil || ev.Kind != event.ToolFail {
		t.Fatalf("opencode: %v %+v", err, ev)
	}
	if _, err := g.Parse("", []byte(`{"session_id":"g","hook_event_name":"Notification"}`), env(nil)); err != ErrSkip {
		t.Fatalf("unknown events must skip, got %v", err)
	}
}

func TestKimi(t *testing.T) {
	k, _ := Get("kimi")
	ev, err := k.Parse("", []byte(`{"session_id":"k1","cwd":"/k","hook_event_name":"UserPromptSubmit","prompt":[{"type":"text","text":"почини"},{"type":"image_url"}]}`), env(nil))
	if err != nil || ev.Kind != event.Prompt || ev.Prompt != "почини" {
		t.Fatalf("kimi prompt: %v %+v", err, ev)
	}
	ev, _ = k.Parse("", []byte(`{"session_id":"k1","cwd":"/k","hook_event_name":"PostToolUseFailure","tool_name":"Shell","tool_input":{"command":"make"},"tool_output":"boom","tool_call_id":"c9"}`), env(nil))
	if ev.Kind != event.ToolFail || ev.Tool.UseID != "c9" || ev.Tool.Error != "boom" {
		t.Fatalf("kimi failure: %+v %+v", ev, ev.Tool)
	}
	if string(k.Render(ev, "UserPromptSubmit", "ctx")) != "ctx" || k.Render(ev, "Stop", "ctx") != nil {
		t.Fatal("kimi render")
	}
}

func TestCanInject(t *testing.T) {
	cu, _ := Get("cursor")
	if cu.CanInject(&event.Event{Kind: event.Prompt}, "beforeSubmitPrompt") {
		t.Fatal("cursor beforeSubmitPrompt cannot carry context")
	}
	if !cu.CanInject(&event.Event{Kind: event.SessionStart}, "sessionStart") {
		t.Fatal("cursor sessionStart can")
	}
	k, _ := Get("kimi")
	if k.CanInject(&event.Event{Kind: event.SessionStart}, "SessionStart") {
		t.Fatal("kimi ignores SessionStart output")
	}
	cc, _ := Get("claude-code")
	if cc.CanInject(&event.Event{Kind: event.TurnEnd}, "Stop") || !cc.CanInject(&event.Event{Kind: event.Prompt}, "UserPromptSubmit") {
		t.Fatal("claude-code capability")
	}
}

func TestGeminiDocumentedFields(t *testing.T) {
	g, _ := Get("gemini")
	ev, err := g.Parse("", []byte(`{"session_id":"g","cwd":"/g","hook_event_name":"AfterAgent","prompt":"q","prompt_response":"final answer"}`), env(nil))
	if err != nil || ev.Kind != event.TurnEnd || ev.LastAssistant != "final answer" {
		t.Fatalf("AfterAgent: %v %+v", err, ev)
	}
	ev, err = g.Parse("", []byte(`{"session_id":"g","cwd":"/g","hook_event_name":"AfterTool","tool_name":"run_shell_command","tool_input":{"command":"x"},"tool_response":{"llmContent":"boom text","returnDisplay":"boom","error":{"message":"exit 1"}}}`), env(nil))
	if err != nil || ev.Kind != event.ToolFail || ev.Tool.Output != "boom text" || !strings.Contains(ev.Tool.Error, "exit 1") {
		t.Fatalf("AfterTool failure: %v %+v %+v", err, ev, ev.Tool)
	}
}
