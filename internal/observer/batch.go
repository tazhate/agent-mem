package observer

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tazhate/agent-mem/internal/event"
	"github.com/tazhate/agent-mem/internal/recall"
	"github.com/tazhate/agent-mem/internal/store"
)

type turn struct {
	n         int
	prompt    string
	steps     []string
	assistant string
	files     []string
	chars     int
}

func lastTurnEnd(evs []store.EventRow) int {
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind == string(event.TurnEnd) {
			return i + 1
		}
	}
	return 0
}

func size(evs []store.EventRow) int {
	n := 0
	for _, e := range evs {
		n += len(e.Input) + len(e.Output) + len(e.Error)
	}
	return n
}

// buildTurns groups events by turn, drops private turns and noise, and renders tool calls compactly.
func buildTurns(evs []store.EventRow) []turn {
	var out []turn
	private := map[int]bool{}
	for _, e := range evs {
		if e.Kind == string(event.Prompt) && e.Error == "private" {
			private[e.Turn] = true
		}
	}
	idx := map[int]int{}
	seenRead := map[string]bool{}
	for _, e := range evs {
		if private[e.Turn] {
			continue
		}
		i, ok := idx[e.Turn]
		if !ok {
			out = append(out, turn{n: e.Turn})
			i = len(out) - 1
			idx[e.Turn] = i
		}
		t := &out[i]
		switch event.Kind(e.Kind) {
		case event.Prompt:
			t.prompt = e.Input
		case event.TurnEnd, event.SubagentStop:
			if e.Output != "" {
				t.assistant = e.Output
			}
		case event.ToolPost, event.ToolFail:
			if e.Tool == "Read" || e.Tool == "read_file" {
				if seenRead[e.Input] {
					continue
				}
				seenRead[e.Input] = true
			}
			step := renderStep(e)
			if step == "" {
				continue
			}
			t.steps = append(t.steps, step)
			t.files = append(t.files, recall.FilePaths(&event.Tool{Input: json.RawMessage(e.Input)})...)
		}
	}
	kept := out[:0]
	for _, t := range out {
		if len(t.steps) == 0 && t.assistant == "" {
			continue
		}
		t.chars = len(t.prompt) + len(t.assistant)
		for _, s := range t.steps {
			t.chars += len(s)
		}
		kept = append(kept, t)
	}
	return kept
}

func renderStep(e store.EventRow) string {
	var in map[string]any
	_ = json.Unmarshal([]byte(e.Input), &in)
	str := func(k string) string { s, _ := in[k].(string); return s }
	failed := e.Error != "" || (e.ExitCode != nil && *e.ExitCode != 0)
	var b strings.Builder
	switch e.Tool {
	case "Bash", "run_shell_command", "shell", "exec_command":
		cmd := str("command")
		if cmd == "" {
			cmd = e.Input
		}
		fmt.Fprintf(&b, "%s `%s`", e.Tool, clip(oneLine(cmd), 400))
		writeOutcome(&b, e, failed, 1500)
	case "Read", "read_file", "view":
		fmt.Fprintf(&b, "Read %s", firstNonEmpty(str("file_path"), str("path"), str("absolute_path")))
		if failed {
			writeOutcome(&b, e, true, 300)
		}
	case "Edit", "MultiEdit", "replace", "edit":
		fmt.Fprintf(&b, "Edit %s", firstNonEmpty(str("file_path"), str("path")))
		if o, n := str("old_string"), str("new_string"); o != "" || n != "" {
			fmt.Fprintf(&b, "\n  - %s\n  + %s", clip(oneLine(o), 300), clip(oneLine(n), 500))
		}
		if failed {
			writeOutcome(&b, e, true, 300)
		}
	case "Write", "write_file":
		fmt.Fprintf(&b, "Write %s (%d chars): %s", firstNonEmpty(str("file_path"), str("path")), len(str("content")), clip(oneLine(str("content")), 300))
	case "Grep", "Glob", "search_file_content", "glob":
		fmt.Fprintf(&b, "%s %s", e.Tool, clip(oneLine(firstNonEmpty(str("pattern"), str("query"), e.Input)), 200))
		writeOutcome(&b, e, failed, 400)
	default:
		fmt.Fprintf(&b, "%s %s", e.Tool, clip(oneLine(e.Input), 500))
		writeOutcome(&b, e, failed, 800)
	}
	return b.String()
}

func writeOutcome(b *strings.Builder, e store.EventRow, failed bool, limit int) {
	if failed {
		b.WriteString(" -> FAILED")
		if e.ExitCode != nil {
			fmt.Fprintf(b, " (exit %d)", *e.ExitCode)
		}
		if e.Error != "" {
			b.WriteString(": " + clip(oneLine(e.Error), 300))
		}
	}
	if out := strings.TrimSpace(e.Output); out != "" {
		b.WriteString("\n  " + strings.ReplaceAll(headTail(out, limit), "\n", "\n  "))
	}
}

// chunks splits turns into batches that fit the character budget.
func chunks(turns []turn, budget int) [][]turn {
	var out [][]turn
	var cur []turn
	n := 0
	for _, t := range turns {
		if len(cur) > 0 && n+t.chars > budget {
			out = append(out, cur)
			cur, n = nil, 0
		}
		cur = append(cur, t)
		n += t.chars
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

func renderBatch(projectName string, turns []turn, existing []*store.Memory) string {
	var b strings.Builder
	fmt.Fprintf(&b, "PROJECT: %s\nDATE: %s\n\n", projectName, time.Now().Format("2006-01-02"))
	if len(existing) > 0 {
		b.WriteString("EXISTING MEMORIES (do not repeat; reference by id in replaces when outdated):\n")
		for _, m := range existing {
			fmt.Fprintf(&b, "#%d %s: %s - %s\n", m.ID, m.Type, m.Title, clip(oneLine(m.Body), 240))
		}
		b.WriteString("\n")
	}
	b.WriteString("SESSION ACTIVITY:\n")
	for _, t := range turns {
		fmt.Fprintf(&b, "<turn %d>\n", t.n)
		if t.prompt != "" {
			b.WriteString("USER: " + headTail(t.prompt, 3000) + "\n")
		}
		if len(t.steps) > 0 {
			b.WriteString("ACTIONS:\n")
			for _, s := range t.steps {
				b.WriteString("- " + s + "\n")
			}
		}
		if t.assistant != "" {
			b.WriteString("ASSISTANT (final message): " + headTail(t.assistant, 4000) + "\n")
		}
		b.WriteString("</turn>\n")
	}
	return b.String()
}

func headTail(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	h, t := limit*6/10, limit*3/10
	return string(r[:h]) + fmt.Sprintf("\n…[%d chars elided]…\n", len(r)-h-t) + string(r[len(r)-t:])
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
