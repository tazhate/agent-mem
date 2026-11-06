package observer

import (
	"context"
	"encoding/json"
	"errors"
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

type flaky struct {
	calls int
}

func (f *flaky) Name() string { return "flaky" }
func (f *flaky) Complete(ctx context.Context, r provider.Request) (json.RawMessage, error) {
	f.calls++
	if f.calls == 2 {
		return nil, context.DeadlineExceeded
	}
	return json.RawMessage(`{"memories":[{"type":"change","title":"chunk ` + itoa(int64(f.calls)) + `","body":"b","facts":[],"files":[],"global":false,"replaces":[]}]}`), nil
}

func TestChunkFailureDoesNotResendStoredChunks(t *testing.T) {
	ctx := t.Context()
	st, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()
	pid, _ := st.EnsureProject(ctx, "/r/.git", "r", "/r")
	sid := "claude-code:c"
	now := time.Now().UnixMilli()
	st.Write(ctx, func(tx *sql.Tx) error {
		st.TouchSession(ctx, tx, sid, "claude-code", "c", pid, "/r", "", true, now)
		for turn := 1; turn <= 2; turn++ {
			st.InsertEvent(ctx, tx, store.EventRow{SessionID: sid, Turn: turn, Kind: "prompt", Input: strings.Repeat("x", 300), TS: now})
			st.InsertEvent(ctx, tx, store.EventRow{SessionID: sid, Turn: turn, Kind: "tool_post", Tool: "Bash", Input: `{"command":"make"}`, Output: strings.Repeat("y", 300), TS: now})
			st.InsertEvent(ctx, tx, store.EventRow{SessionID: sid, Turn: turn, Kind: "turn_end", Output: "done", TS: now})
		}
		return nil
	})
	cfg := config.Default().Observer
	cfg.BatchChars = 500 // one turn per chunk
	f := &flaky{}
	o := New(st, cfg, recall.New(st, config.Default().Recall, nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	o.SetProvider(f)
	o.process(ctx, sid, false) // chunk 1 ok, chunk 2 fails
	pending, _ := st.PendingEvents(ctx, sid, 100)
	if len(pending) != 3 || pending[0].Turn != 2 {
		t.Fatalf("after partial failure want only turn 2 pending, got %d", len(pending))
	}
	delete(o.backoff, sid)
	o.process(ctx, sid, false) // retry sends only turn 2
	mems, _ := st.SessionMemories(ctx, sid, 10)
	if len(mems) != 2 || f.calls != 3 {
		t.Fatalf("memories=%d calls=%d (turn 1 must not be resent)", len(mems), f.calls)
	}
}

type broken struct{ calls int }

func (b *broken) Name() string { return "broken" }
func (b *broken) Complete(context.Context, provider.Request) (json.RawMessage, error) {
	b.calls++
	return json.RawMessage(`not json`), nil
}

func TestSummaryGivesUp(t *testing.T) {
	ctx := t.Context()
	st, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()
	pid, _ := st.EnsureProject(ctx, "/r/.git", "r", "/r")
	sid := "claude-code:z"
	st.Write(ctx, func(tx *sql.Tx) error {
		st.TouchSession(ctx, tx, sid, "claude-code", "z", pid, "/r", "", true, 1)
		st.InsertEvent(ctx, tx, store.EventRow{SessionID: sid, Turn: 1, Kind: "prompt", Input: "do it", TS: 1})
		return st.EndSession(ctx, tx, sid, 2)
	})
	b := &broken{}
	o := New(st, config.Default().Observer, recall.New(st, config.Default().Recall, nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	o.SetProvider(b)
	for range 20 {
		o.scan(ctx)                       // the periodic path, which only picks unsummarized sessions
		delete(o.backoff, "summary:"+sid) // simulate time passing
	}
	if b.calls != maxFailures {
		t.Fatalf("summary attempts = %d, want %d then stop", b.calls, maxFailures)
	}
}

func TestEmptyObjectIsNotAnAnswer(t *testing.T) {
	ctx := t.Context()
	st, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()
	pid, _ := st.EnsureProject(ctx, "/r/.git", "r", "/r")
	sid := "claude-code:e"
	now := time.Now().UnixMilli()
	st.Write(ctx, func(tx *sql.Tx) error {
		st.TouchSession(ctx, tx, sid, "claude-code", "e", pid, "/r", "", true, now)
		st.InsertEvent(ctx, tx, store.EventRow{SessionID: sid, Turn: 1, Kind: "prompt", Input: "q", TS: now})
		st.InsertEvent(ctx, tx, store.EventRow{SessionID: sid, Turn: 1, Kind: "tool_post", Tool: "Bash", Input: `{"command":"make"}`, Output: "ok", TS: now})
		st.InsertEvent(ctx, tx, store.EventRow{SessionID: sid, Turn: 1, Kind: "turn_end", Output: "done", TS: now})
		return nil
	})
	o := New(st, config.Default().Observer, recall.New(st, config.Default().Recall, nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	o.SetProvider(&fake{resp: `{}`})
	o.process(ctx, sid, false)
	if p, _ := st.PendingEvents(ctx, sid, 10); len(p) != 3 {
		t.Fatalf("{} acknowledged the batch: %d pending", len(p))
	}
	// Outages never give up.
	for range 10 {
		if o.fail("k", &provider.Error{Retryable: true, Err: context.DeadlineExceeded}) {
			t.Fatal("gave up on a retryable outage")
		}
	}
}

func TestRetryableChain(t *testing.T) {
	misconfigured := &provider.Error{Provider: "a", Err: io.EOF}
	down := &provider.Error{Provider: "b", Retryable: true, Err: context.DeadlineExceeded}
	if !retryable(errors.Join(misconfigured, down)) {
		t.Fatal("a chain with one provider merely down must keep the batch")
	}
	if retryable(errors.Join(misconfigured, &provider.Error{Provider: "c", Err: io.EOF})) {
		t.Fatal("all-permanent chain must be able to give up")
	}
}
