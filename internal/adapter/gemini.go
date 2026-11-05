package adapter

import "github.com/tazhate/agent-mem/internal/event"

// gemini handles Gemini CLI hooks configured in ~/.gemini/settings.json.
type gemini struct{}

func init() { register(gemini{}) }

func (gemini) Name() string { return "gemini" }

var geminiKinds = map[string]event.Kind{
	"SessionStart": event.SessionStart,
	"BeforeAgent":  event.Prompt,
	"BeforeTool":   event.ToolPre,
	"AfterTool":    event.ToolPost,
	"AfterAgent":   event.TurnEnd,
	"PreCompress":  event.Compact,
	"SessionEnd":   event.SessionEnd,
}

var geminiContextEvents = map[string]bool{"SessionStart": true, "BeforeAgent": true, "AfterTool": true}

func (gemini) Parse(hookArg string, raw []byte, env Env) (*event.Event, error) {
	ev, err := parseSnake("gemini", geminiKinds, hookArg, raw, env, "")
	if err != nil {
		return nil, err
	}
	if ev.Cwd == "" {
		ev.Cwd = firstNonEmpty(env("GEMINI_CWD"), env("GEMINI_PROJECT_DIR"))
	}
	if ev.Kind == event.ToolPost && ev.Tool.Failed() {
		ev.Kind = event.ToolFail
	}
	return ev, nil
}

func (gemini) Render(ev *event.Event, hookArg string, context string) []byte {
	name := firstNonEmpty(hookArg, hookNameFor(geminiKinds, ev.Kind))
	if context == "" || !geminiContextEvents[name] {
		return nil
	}
	return hookSpecific(name, context)
}

func (gemini) CanInject(ev *event.Event, hookArg string) bool {
	return geminiContextEvents[firstNonEmpty(hookArg, hookNameFor(geminiKinds, ev.Kind))]
}
