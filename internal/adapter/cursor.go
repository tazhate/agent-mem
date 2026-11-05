package adapter

import (
	"encoding/json"

	"agent-mem/internal/event"
)

// cursor handles Cursor hooks from ~/.cursor/hooks.json. Context can only be injected on
// sessionStart, postToolUse and postToolUseFailure.
type cursor struct{}

func init() { register(cursor{}) }

func (cursor) Name() string { return "cursor" }

var cursorKinds = map[string]event.Kind{
	"sessionStart":        event.SessionStart,
	"beforeSubmitPrompt":  event.Prompt,
	"preToolUse":          event.ToolPre,
	"postToolUse":         event.ToolPost,
	"postToolUseFailure":  event.ToolFail,
	"afterShellExecution": event.ToolPost,
	"afterFileEdit":       event.ToolPost,
	"stop":                event.TurnEnd,
	"subagentStart":       event.SubagentStart,
	"subagentStop":        event.SubagentStop,
	"preCompact":          event.Compact,
	"sessionEnd":          event.SessionEnd,
}

var cursorContextEvents = map[string]bool{"sessionStart": true, "postToolUse": true, "postToolUseFailure": true}

func (cursor) Parse(hookArg string, raw []byte, env Env) (*event.Event, error) {
	ev, err := parseSnake("cursor", cursorKinds, hookArg, raw, env, "")
	if err != nil {
		return nil, err
	}
	p, _ := decode(raw)
	// Cursor keys sessions by conversation; sessionStart also carries session_id.
	if p.ConversationID != "" {
		ev.NativeSession = p.ConversationID
	}
	name := firstNonEmpty(p.HookEventName, hookArg)
	switch name {
	case "afterShellExecution":
		in, _ := json.Marshal(map[string]string{"command": p.Command})
		ev.Tool = &event.Tool{Name: "Bash", Input: in, Output: text(p.Output)}
	case "afterFileEdit":
		in, _ := json.Marshal(map[string]any{"file_path": p.FilePath, "edits": p.Edits})
		ev.Tool = &event.Tool{Name: "Edit", Input: in}
	}
	return ev, nil
}

func (cursor) Render(ev *event.Event, hookArg string, context string) []byte {
	name := firstNonEmpty(hookArg, hookNameFor(cursorKinds, ev.Kind))
	switch {
	case name == "beforeSubmitPrompt":
		return []byte(`{"continue":true}`)
	case context == "" || !cursorContextEvents[name]:
		return nil
	}
	b, _ := json.Marshal(map[string]string{"additional_context": truncateContext(context)})
	return b
}
