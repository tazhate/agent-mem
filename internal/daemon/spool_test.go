package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/tazhate/agent-mem/internal/event"
)

func ev(id string) *event.Event {
	return &event.Event{ID: id, Runtime: "t", NativeSession: "s", Kind: event.ToolPost}
}

func TestSpoolRecoversAbandonedDrains(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spool.jsonl")
	for _, id := range []string{"a0", "a1"} {
		if err := SpoolAppend(path, ev(id)); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate a drain killed right after the rename.
	if err := os.Rename(path, path+".999-1.draining"); err != nil {
		t.Fatal(err)
	}
	SpoolAppend(path, ev("b0"))
	var got []string
	n, err := SpoolDrain(path, func(e *event.Event) error { got = append(got, e.ID); return nil })
	if err != nil || n != 3 || len(got) != 3 {
		t.Fatalf("drained %d %v err=%v", n, got, err)
	}
	if SpoolPending(path) {
		t.Fatal("files left behind")
	}
}

func TestSpoolRespoolsFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spool.jsonl")
	for _, id := range []string{"ok1", "bad", "ok2"} {
		SpoolAppend(path, ev(id))
	}
	n, err := SpoolDrain(path, func(e *event.Event) error {
		if e.ID == "bad" {
			return errors.New("busy")
		}
		return nil
	})
	if n != 2 || err == nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	var again []string
	SpoolDrain(path, func(e *event.Event) error { again = append(again, e.ID); return nil })
	if len(again) != 1 || again[0] != "bad" {
		t.Fatalf("failed event not retried: %v", again)
	}
}

func TestSpoolSideFileWhenLocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spool.jsonl")
	f, _ := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	if err := SpoolAppend(path, ev("side")); err != nil {
		t.Fatal(err)
	}
	f.Close()
	var got []string
	SpoolDrain(path, func(e *event.Event) error { got = append(got, e.ID); return nil })
	if len(got) != 1 || got[0] != "side" {
		t.Fatalf("side file not drained: %v", got)
	}
}
