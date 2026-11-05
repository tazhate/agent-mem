package adapter

import (
	"encoding/json"

	"github.com/tazhate/agent-mem/internal/event"
)

// opencode receives payloads from the amem OpenCode plugin shim, which already sends
// amem kind names and field names; the reply is {"context": "..."} for the shim to inject.
type opencode struct{}

func init() { register(opencode{}) }

func (opencode) Name() string { return "opencode" }

type opencodePayload struct {
	Kind      string          `json:"kind"`
	SessionID string          `json:"session_id"`
	Cwd       string          `json:"cwd"`
	Prompt    string          `json:"prompt"`
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args"`
	Output    json.RawMessage `json:"output"`
	Error     string          `json:"error"`
	Assistant string          `json:"assistant"`
}

func (opencode) Parse(hookArg string, raw []byte, env Env) (*event.Event, error) {
	var p opencodePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	kind := event.Kind(firstNonEmpty(p.Kind, hookArg))
	if p.SessionID == "" || kind == "" {
		return nil, ErrSkip
	}
	ev := &event.Event{
		Runtime:       "opencode",
		NativeSession: p.SessionID,
		Cwd:           p.Cwd,
		Kind:          kind,
		Prompt:        p.Prompt,
		LastAssistant: p.Assistant,
		TS:            event.NowMS(),
	}
	if p.Tool != "" {
		ev.Tool = &event.Tool{Name: p.Tool, Input: p.Args, Output: text(p.Output), Error: p.Error}
		if ev.Tool.Error != "" && kind == event.ToolPost {
			ev.Kind = event.ToolFail
		}
	}
	return ev, nil
}

func (opencode) Render(ev *event.Event, hookArg string, context string) []byte {
	if context == "" {
		return nil
	}
	b, _ := json.Marshal(map[string]string{"context": truncateContext(context)})
	return b
}
