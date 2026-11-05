package adapter

import "github.com/tazhate/agent-mem/internal/event"

// codex handles Codex CLI hooks (also used by Qoder). Payloads mirror Claude Code plus turn_id,
// last_assistant_message and model.
type codex struct{}

func init() { register(codex{}) }

func (codex) Name() string { return "codex" }

var codexKinds = map[string]event.Kind{
	"SessionStart":     event.SessionStart,
	"UserPromptSubmit": event.Prompt,
	"PreToolUse":       event.ToolPre,
	"PostToolUse":      event.ToolPost,
	"Stop":             event.TurnEnd,
	"SubagentStart":    event.SubagentStart,
	"SubagentStop":     event.SubagentStop,
	"PreCompact":       event.Compact,
	"PostCompact":      event.Compact,
	"SessionEnd":       event.SessionEnd,
}

var codexContextEvents = map[string]bool{
	"SessionStart": true, "UserPromptSubmit": true, "PreToolUse": true,
	"PostToolUse": true, "SubagentStart": true,
}

func (codex) Parse(hookArg string, raw []byte, env Env) (*event.Event, error) {
	ev, err := parseSnake("codex", codexKinds, hookArg, raw, env, "")
	if err != nil {
		return nil, err
	}
	if ev.Kind == event.ToolPost && ev.Tool.Failed() {
		ev.Kind = event.ToolFail
	}
	return ev, nil
}

func (codex) Render(ev *event.Event, hookArg string, context string) []byte {
	name := firstNonEmpty(hookArg, hookNameFor(codexKinds, ev.Kind))
	if name == "SessionStart" {
		// Codex rejects a SessionStart output without hookSpecificOutput, even when empty.
		return hookSpecific(name, context)
	}
	if context == "" || !codexContextEvents[name] {
		return nil
	}
	return hookSpecific(name, context)
}
