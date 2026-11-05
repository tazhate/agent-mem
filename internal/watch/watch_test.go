package watch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tazhate/agent-mem/internal/event"
	"github.com/tazhate/agent-mem/internal/store"
)

func TestCodexRollout(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex", "sessions", "2026", "11", "05")
	os.MkdirAll(dir, 0o755)
	lines := `{"timestamp":"2026-11-05T10:00:00Z","type":"session_meta","payload":{"id":"c1","cwd":"/r","source":"cli"}}
{"timestamp":"2026-11-05T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>x</environment_context>"}]}}
{"timestamp":"2026-11-05T10:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the build"}]}}
{"timestamp":"2026-11-05T10:00:03Z","type":"response_item","payload":{"type":"function_call","call_id":"k1","name":"shell","arguments":"{\"command\":[\"go\",\"build\"]}"}}
{"timestamp":"2026-11-05T10:00:04Z","type":"response_item","payload":{"type":"function_call_output","call_id":"k1","output":"ok"}}
{"timestamp":"2026-11-05T10:00:05Z","type":"event_msg","payload":{"type":"task_complete","last_agent_message":"built"}}
{"timestamp":"2026-11-05T10:00:06Z","type":"event_msg","payload":{"type":"token_count"`
	path := filepath.Join(dir, "rollout-1.jsonl")
	os.WriteFile(path, []byte(lines), 0o644)
	st, _ := store.Open(filepath.Join(t.TempDir(), "w.db"))
	defer st.Close()
	var got []*event.Event
	w := New(st, Presets["codex"], home, func(e *event.Event) error { got = append(got, e); return nil })
	if err := w.Run(t.Context(), 0, true); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Kind != event.Prompt || got[0].Prompt != "fix the build" ||
		got[1].Tool == nil || got[1].Tool.Name != "shell" || got[2].LastAssistant != "built" || got[0].SessionID() != "codex:c1" {
		t.Fatalf("events: %+v", got)
	}
	// The partial last line is completed later; only new lines are read.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`}}` + "\n" + `{"timestamp":"2026-11-05T10:01:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}}` + "\n")
	f.Close()
	got = nil
	w2 := New(st, Presets["codex"], home, func(e *event.Event) error { got = append(got, e); return nil })
	w2.Run(t.Context(), 0, true)
	if len(got) != 1 || got[0].Prompt != "next" || got[0].SessionID() != "codex:c1" {
		// A restarted watcher must resume mid-file with the session it saw before.
		t.Fatalf("restart lost session: %+v", got)
	}
	got = nil
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"timestamp":"2026-11-05T10:02:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"again"}]}}` + "\n")
	f.Close()
	w2.Run(t.Context(), 0, true)
	if len(got) != 1 || got[0].Prompt != "again" {
		t.Fatalf("incremental read: %+v", got)
	}
}
