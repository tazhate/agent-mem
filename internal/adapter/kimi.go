package adapter

import "github.com/tazhate/agent-mem/internal/event"

// kimi handles Kimi Code hooks ([[hooks]] blocks in $KIMI_CODE_HOME/config.toml). Payloads follow
// the Claude Code field names; the prompt is a ContentPart[] list and Read uses "path". Context
// is returned as plain stdout text.
type kimi struct{}

func init() { register(kimi{}) }

func (kimi) Name() string { return "kimi" }

var kimiKinds = map[string]event.Kind{
	"SessionStart":       event.SessionStart,
	"UserPromptSubmit":   event.Prompt,
	"PreToolUse":         event.ToolPre,
	"PostToolUse":        event.ToolPost,
	"PostToolUseFailure": event.ToolFail,
	"Stop":               event.TurnEnd,
	"PreCompact":         event.Compact,
	"SessionEnd":         event.SessionEnd,
}

// Kimi ignores SessionStart output; prompt and pre-tool output reach the model.
var kimiContextEvents = map[string]bool{"UserPromptSubmit": true, "PreToolUse": true}

func (kimi) Parse(hookArg string, raw []byte, env Env) (*event.Event, error) {
	ev, err := parseSnake("kimi", kimiKinds, hookArg, raw, env, "")
	if err != nil {
		return nil, err
	}
	if ev.Tool != nil && ev.Tool.UseID == "" {
		p, _ := decode(raw)
		ev.Tool.UseID = p.ToolCallID
	}
	if ev.Kind == event.ToolFail && ev.Tool != nil && ev.Tool.Error == "tool failed" && ev.Tool.Output != "" {
		ev.Tool.Error = ev.Tool.Output
	}
	return ev, nil
}

func (kimi) Render(ev *event.Event, hookArg string, context string) []byte {
	name := firstNonEmpty(hookArg, hookNameFor(kimiKinds, ev.Kind))
	if context == "" || !kimiContextEvents[name] {
		return nil
	}
	return []byte(truncateContext(context))
}

func (kimi) CanInject(ev *event.Event, hookArg string) bool {
	return kimiContextEvents[firstNonEmpty(hookArg, hookNameFor(kimiKinds, ev.Kind))]
}
