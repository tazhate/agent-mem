package adapter

import (
	"github.com/tazhate/agent-mem/internal/event"
)

// claudeCode handles Claude Code hooks (and Qwen Code, which uses the same payloads).
type claudeCode struct{}

func init() { register(claudeCode{}) }

func (claudeCode) Name() string { return "claude-code" }

var claudeKinds = map[string]event.Kind{
	"SessionStart":       event.SessionStart,
	"UserPromptSubmit":   event.Prompt,
	"PreToolUse":         event.ToolPre,
	"PostToolUse":        event.ToolPost,
	"PostToolUseFailure": event.ToolFail,
	"Stop":               event.TurnEnd,
	"SubagentStart":      event.SubagentStart,
	"SubagentStop":       event.SubagentStop,
	"PreCompact":         event.Compact,
	"PostCompact":        event.Compact,
	"SessionEnd":         event.SessionEnd,
}

// claudeContextEvents accept hookSpecificOutput.additionalContext.
var claudeContextEvents = map[string]bool{
	"SessionStart": true, "UserPromptSubmit": true, "PreToolUse": true,
	"PostToolUse": true, "PostToolUseFailure": true, "SubagentStart": true,
}

func (claudeCode) Parse(hookArg string, raw []byte, env Env) (*event.Event, error) {
	return parseSnake("claude-code", claudeKinds, hookArg, raw, env, env("CLAUDE_PROJECT_DIR"))
}

func (claudeCode) Render(ev *event.Event, hookArg string, context string) []byte {
	name := firstNonEmpty(hookArg, hookNameFor(claudeKinds, ev.Kind))
	if context == "" || !claudeContextEvents[name] {
		return nil
	}
	return hookSpecific(name, context)
}

// parseSnake is shared by the runtimes whose payloads follow the Claude Code field naming.
func parseSnake(runtime string, kinds map[string]event.Kind, hookArg string, raw []byte, env Env, cwdOverride string) (*event.Event, error) {
	p, err := decode(raw)
	if err != nil {
		return nil, err
	}
	name := firstNonEmpty(p.HookEventName, hookArg)
	kind, ok := kinds[name]
	if !ok {
		return nil, ErrSkip
	}
	ev := &event.Event{
		Runtime:        runtime,
		NativeSession:  firstNonEmpty(p.SessionID, p.ConversationID),
		Cwd:            firstNonEmpty(cwdOverride, p.Cwd, firstOf(p.WorkspaceRoots)),
		Kind:           kind,
		Source:         firstNonEmpty(p.Source, p.Trigger),
		TurnID:         p.TurnID,
		Prompt:         text(p.Prompt),
		Tool:           toolFromPayload(p),
		AgentID:        p.AgentID,
		AgentType:      p.AgentType,
		TranscriptPath: p.TranscriptPath,
		LastAssistant:  p.LastAssistantMessage,
		Model:          p.Model,
		TS:             event.NowMS(),
	}
	if kind == event.ToolFail && ev.Tool != nil && ev.Tool.Error == "" {
		ev.Tool.Error = "tool failed"
	}
	if ev.NativeSession == "" {
		return nil, ErrSkip
	}
	return ev, nil
}

func hookNameFor(kinds map[string]event.Kind, k event.Kind) string {
	for name, kind := range kinds {
		if kind == k {
			return name
		}
	}
	return ""
}

func firstOf(s []string) string {
	if len(s) > 0 {
		return s[0]
	}
	return ""
}

func (claudeCode) CanInject(ev *event.Event, hookArg string) bool {
	return claudeContextEvents[firstNonEmpty(hookArg, hookNameFor(claudeKinds, ev.Kind))]
}
