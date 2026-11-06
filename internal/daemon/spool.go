package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tazhate/agent-mem/internal/event"
)

// spoolLockWait bounds how long a hook waits for the spool lock; hooks must never hang.
const spoolLockWait = 300 * time.Millisecond

func marshalEvent(ev *event.Event) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ev); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// SpoolAppend stores an event for the daemon to pick up later. Writers hold an exclusive flock
// and re-check that the path still points at the file they opened, so a concurrent drain
// (which renames the spool away) never loses a line. If the lock stays busy, the event goes to
// its own side file instead of blocking.
func SpoolAppend(path string, ev *event.Event) error {
	line, err := marshalEvent(ev)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(spoolLockWait)
	for time.Now().Before(deadline) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		if !lockUntil(f, deadline) {
			f.Close()
			break
		}
		if same(f, path) {
			_, err := f.Write(line)
			f.Close()
			return err
		}
		f.Close() // drained between open and lock; retry on the fresh file
	}
	return spoolSide(path, line)
}

func lockUntil(f *os.File, deadline time.Time) bool {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return true
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// spoolSide writes one event to a unique file next to the spool, atomically via rename.
func spoolSide(path string, line []byte) error {
	dir, base := filepath.Split(path)
	name := fmt.Sprintf("%s.side-%d-%d.jsonl", base, os.Getpid(), time.Now().UnixNano())
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := os.WriteFile(tmp, line, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

func same(f *os.File, path string) bool {
	a, err1 := f.Stat()
	b, err2 := os.Stat(path)
	return err1 == nil && err2 == nil && os.SameFile(a, b)
}

// SpoolPending reports whether anything waits to be drained.
func SpoolPending(path string) bool {
	if _, err := os.Stat(path); err == nil {
		return true
	}
	return len(leftovers(path)) > 0
}

// leftovers lists side files and drain files abandoned by a crashed or interrupted drain.
// Only one daemon runs at a time (flock), so every such file is safe to take over.
func leftovers(path string) []string {
	dir, base := filepath.Split(path)
	entries, err := os.ReadDir(filepath.Clean(dir))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, base+".") && (strings.HasSuffix(n, ".draining") || (strings.Contains(n, ".side-") && strings.HasSuffix(n, ".jsonl"))) {
			out = append(out, filepath.Join(dir, n))
		}
	}
	return out
}

// SpoolDrain feeds every spooled event to fn: first files left by earlier drains and side
// files, then the current spool, which is moved aside under a unique name. Draining stops at
// the first event fn fails on: that event and everything after it stay on disk, in order, for
// the next pass. Order matters: a private prompt must be seen before the rest of its turn.
func SpoolDrain(path string, fn func(*event.Event) error) (int, error) {
	files := leftovers(path)
	tmp := fmt.Sprintf("%s.%d-%d.draining", path, os.Getpid(), time.Now().UnixNano())
	switch err := os.Rename(path, tmp); {
	case err == nil:
		files = append(files, tmp)
	case !os.IsNotExist(err):
		return 0, err
	}
	total := 0
	for _, f := range files {
		n, err := drainFile(f, fn)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func drainFile(file string, fn func(*event.Event) error) (int, error) {
	f, err := os.Open(file)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	// Wait for writers that opened the file before it was renamed.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return 0, err
	}
	n := 0
	var off int64 // start of the current line
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		start := off
		off += int64(len(line)) + 1
		var ev event.Event
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if err := fn(&ev); err != nil {
			// Keep the original bytes: fn may have changed the event (cleaning drops a
			// private prompt's text, which would turn it into a public one on replay).
			if kerr := keepTail(f, file, start); kerr != nil {
				return n, errors.Join(err, kerr)
			}
			return n, fmt.Errorf("spooled event %s: %w", ev.ID, err)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		// Unreadable remainder (e.g. a line over 64 MB): keep the file for inspection.
		return n, fmt.Errorf("drain %s: %w", file, err)
	}
	return n, os.Remove(file)
}

// keepTail replaces file with its content from off on, atomically, so events already ingested
// are not replayed and the rest keeps its order.
func keepTail(f *os.File, file string, off int64) error {
	if off == 0 {
		return nil
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return err
	}
	dir, base := filepath.Split(file)
	tmp, err := os.CreateTemp(filepath.Clean(dir), "."+base+".*.tmp")
	if err != nil {
		return err
	}
	if _, err := io.Copy(tmp, f); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), file)
}
