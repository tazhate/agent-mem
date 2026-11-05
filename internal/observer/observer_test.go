package observer

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"database/sql"

	"github.com/tazhate/agent-mem/internal/config"
	"github.com/tazhate/agent-mem/internal/provider"
	"github.com/tazhate/agent-mem/internal/recall"
	"github.com/tazhate/agent-mem/internal/store"
)

type fake struct {
	got  []provider.Request
	resp string
}

func (f *fake) Name() string { return "fake" }
func (f *fake) Complete(ctx context.Context, r provider.Request) (json.RawMessage, error) {
	f.got = append(f.got, r)
	return json.RawMessage(f.resp), nil
}

func TestObserveTurn(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pid, _ := st.EnsureProject(ctx, "/r/.git", "r", "/r")
	old, _, _ := st.InsertMemory(ctx, &store.Memory{ProjectID: pid, Type: "gotcha", Title: "hook timeout 30s is enough", Files: []string{"/r/hooks.json"}})
	sid := "claude-code:s"
	exit1 := 1
	now := time.Now().UnixMilli()
	st.Write(ctx, func(tx *sql.Tx) error {
		st.TouchSession(ctx, tx, sid, "claude-code", "s", pid, "/r", "", true, 1)
		for _, e := range []store.EventRow{
			{SessionID: sid, Turn: 1, Kind: "prompt", Input: "why does the hook time out", TS: now + 1},
			{SessionID: sid, Turn: 1, Kind: "tool_post", Tool: "Read", Input: `{"file_path":"/r/hooks.json"}`, Output: "{...}", TS: now + 2},
			{SessionID: sid, Turn: 1, Kind: "tool_post", Tool: "Read", Input: `{"file_path":"/r/hooks.json"}`, Output: "{...}", TS: now + 3},
			{SessionID: sid, Turn: 1, Kind: "tool_fail", Tool: "Bash", Input: `{"command":"npm install"}`, Error: "timeout", ExitCode: &exit1, TS: now + 4},
			{SessionID: sid, Turn: 1, Kind: "turn_end", Output: "Raised the timeout to 60s.", TS: now + 5},
			{SessionID: sid, Turn: 2, Kind: "prompt", Input: "<private>", Error: "private", TS: now + 6},
			{SessionID: sid, Turn: 2, Kind: "tool_post", Tool: "Bash", Input: `{"command":"cat secrets"}`, Output: "s3cret", TS: now + 7},
		} {
			st.InsertEvent(ctx, tx, e)
		}
		return nil
	})
	f := &fake{resp: `{"memories":[{"type":"gotcha","title":"SessionStart hook needs >= 60s: npm install takes ~40s","body":"30s timed out.","facts":["timeout=60"],"files":["/r/hooks.json"],"global":false,"replaces":[` + itoa(old) + `,99999]},{"type":"weird","title":"","body":"","facts":[],"files":[],"global":false,"replaces":[]}]}`}
	cfg := config.Default().Observer
	o := New(st, cfg, recall.New(st, config.Default().Recall, nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	o.SetProvider(f)
	o.process(ctx, sid, false)

	if len(f.got) != 1 {
		t.Fatalf("provider calls: %d", len(f.got))
	}
	u := f.got[0].User
	if strings.Count(u, "Read /r/hooks.json") != 1 || !strings.Contains(u, "FAILED (exit 1)") || !strings.Contains(u, "#"+itoa(old)) {
		t.Fatalf("batch render:\n%s", u)
	}
	if strings.Contains(u, "s3cret") {
		t.Fatal("private turn leaked into the batch (it is not finished either)")
	}
	mems, _ := st.SessionMemories(ctx, sid, 10)
	if len(mems) != 1 || mems[0].Type != "gotcha" {
		t.Fatalf("stored: %+v", mems)
	}
	got, _ := st.GetMemories(ctx, []int64{old})
	if got[0].SupersededBy != mems[0].ID {
		t.Fatalf("old memory not superseded: %+v", got[0])
	}
	pending, _ := st.PendingEvents(ctx, sid, 100)
	if len(pending) != 2 {
		t.Fatalf("turn 2 should stay pending until it ends, got %d pending", len(pending))
	}
	o.process(ctx, sid, true)
	if len(f.got) != 1 {
		t.Fatal("private turn was sent to the provider")
	}
	if pending, _ := st.PendingEvents(ctx, sid, 100); len(pending) != 0 {
		t.Fatalf("forced flush left %d pending", len(pending))
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
