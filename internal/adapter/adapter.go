// Package adapter translates runtime-specific hook payloads into event.Event and renders
// injected context back into the format each runtime expects.
package adapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/tazhate/agent-mem/internal/event"
)

// MaxContextChars stays under the 10 000 character cap Claude Code and Codex apply to additionalContext.
const MaxContextChars = 9500

// Env looks up environment variables; tests pass a map-backed function.
type Env func(string) string

type Adapter interface {
	Name() string
	// Parse converts a raw stdin payload. hookArg is the event name passed on the command line,
	// used when the payload does not name the event itself.
	Parse(hookArg string, raw []byte, env Env) (*event.Event, error)
	// Render returns what the hook prints to stdout; nil means print nothing.
	Render(ev *event.Event, hookArg string, context string) []byte
	// CanInject reports whether this hook can deliver context to the model at all. Recall is
	// skipped otherwise, so nothing is marked as shown when the runtime would drop it.
	CanInject(ev *event.Event, hookArg string) bool
}

var registry = map[string]Adapter{}

func register(a Adapter) { registry[a.Name()] = a }

func Get(name string) (Adapter, error) {
	a, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown runtime %q", name)
	}
	return a, nil
}

func Names() []string {
	return []string{"claude-code", "codex", "gemini", "cursor", "opencode", "kimi"}
}

// ErrSkip marks payloads that carry nothing worth recording (unknown events and the like).
var ErrSkip = fmt.Errorf("skip")

// payload is the union of the snake_case fields used by Claude Code, Codex, Gemini CLI and Cursor.
type payload struct {
	SessionID            string          `json:"session_id"`
	ConversationID       string          `json:"conversation_id"`
	Cwd                  string          `json:"cwd"`
	WorkspaceRoots       []string        `json:"workspace_roots"`
	HookEventName        string          `json:"hook_event_name"`
	Source               string          `json:"source"`
	Trigger              string          `json:"trigger"`
	Prompt               json.RawMessage `json:"prompt"`
	ToolName             string          `json:"tool_name"`
	ToolInput            json.RawMessage `json:"tool_input"`
	ToolResponse         json.RawMessage `json:"tool_response"`
	ToolOutput           json.RawMessage `json:"tool_output"`
	ToolUseID            string          `json:"tool_use_id"`
	ToolCallID           string          `json:"tool_call_id"`
	Error                json.RawMessage `json:"error"`
	AgentID              string          `json:"agent_id"`
	AgentType            string          `json:"agent_type"`
	TranscriptPath       string          `json:"transcript_path"`
	LastAssistantMessage string          `json:"last_assistant_message"`
	TurnID               string          `json:"turn_id"`
	Model                string          `json:"model"`
	Command              string          `json:"command"`
	Output               json.RawMessage `json:"output"`
	FilePath             string          `json:"file_path"`
	Edits                json.RawMessage `json:"edits"`
	Duration             json.RawMessage `json:"duration"`
}

func decode(raw []byte) (*payload, error) {
	var p payload
	if len(bytes.TrimSpace(raw)) == 0 {
		return &p, nil
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decode hook payload: %w", err)
	}
	return &p, nil
}

// text turns a JSON value into readable text: strings are unquoted, Bash-style objects are
// flattened to stdout+stderr, prompt part arrays are joined, everything else stays compact JSON.
func text(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	case '{':
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) == nil {
			if out, ok := shellOutput(m); ok {
				return out
			}
			for _, k := range []string{"output", "content", "result", "text", "message"} {
				if v, ok := m[k]; ok {
					if s := text(v); s != "" {
						return s
					}
				}
			}
		}
	case '[':
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &parts) == nil {
			var b strings.Builder
			for _, p := range parts {
				if p.Text != "" {
					if b.Len() > 0 {
						b.WriteByte('\n')
					}
					b.WriteString(p.Text)
				}
			}
			if b.Len() > 0 {
				return b.String()
			}
		}
	}
	var buf bytes.Buffer
	if json.Compact(&buf, raw) == nil {
		return buf.String()
	}
	return string(raw)
}

func shellOutput(m map[string]json.RawMessage) (string, bool) {
	_, hasOut := m["stdout"]
	_, hasErr := m["stderr"]
	if !hasOut && !hasErr {
		return "", false
	}
	out, errOut := text(m["stdout"]), text(m["stderr"])
	switch {
	case out != "" && errOut != "":
		return out + "\n[stderr]\n" + errOut, true
	case errOut != "":
		return "[stderr]\n" + errOut, true
	}
	return out, true
}

// exitCode digs a process exit code out of a tool response object, if present.
func exitCode(raw json.RawMessage) *int {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	for _, k := range []string{"exit_code", "exitCode", "returnCode", "return_code"} {
		if v, ok := m[k]; ok {
			var n int
			if json.Unmarshal(v, &n) == nil {
				return &n
			}
		}
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// truncateContext caps injected text on a rune boundary.
func truncateContext(s string) string {
	if len(s) <= MaxContextChars {
		return s
	}
	cut := MaxContextChars
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n…"
}

// hookSpecific renders the hookSpecificOutput envelope shared by Claude Code, Codex and Gemini CLI.
func hookSpecific(eventName, context string) []byte {
	b, _ := json.Marshal(map[string]any{
		"hookSpecificOutput": map[string]string{
			"hookEventName":     eventName,
			"additionalContext": truncateContext(context),
		},
	})
	return b
}

// toolFromPayload builds the Tool part shared by snake_case runtimes.
func toolFromPayload(p *payload) *event.Tool {
	if p.ToolName == "" {
		return nil
	}
	resp := p.ToolResponse
	if len(resp) == 0 {
		resp = p.ToolOutput
	}
	t := &event.Tool{
		Name:     p.ToolName,
		UseID:    p.ToolUseID,
		Input:    p.ToolInput,
		Output:   text(resp),
		ExitCode: exitCode(resp),
		Error:    text(p.Error),
	}
	return t
}
