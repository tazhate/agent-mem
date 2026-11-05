package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"syscall"

	"github.com/tazhate/agent-mem/internal/event"
)

// SpoolAppend stores an event for the daemon to pick up later. Writers hold an exclusive flock
// and re-check that the path still points at the file they opened, so a concurrent drain
// (which renames the spool away) never loses a line.
func SpoolAppend(path string, ev *event.Event) error {
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	for range 5 {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			f.Close()
			return err
		}
		if same(f, path) {
			_, err := f.Write(line)
			f.Close()
			return err
		}
		f.Close() // drained between open and lock; retry on the fresh file
	}
	return fmt.Errorf("spool: could not append")
}

func same(f *os.File, path string) bool {
	a, err1 := f.Stat()
	b, err2 := os.Stat(path)
	return err1 == nil && err2 == nil && os.SameFile(a, b)
}

// SpoolDrain moves the spool aside and feeds every event to fn. Lines that fail to decode are skipped.
func SpoolDrain(path string, fn func(*event.Event) error) (int, error) {
	tmp := fmt.Sprintf("%s.%d.draining", path, os.Getpid())
	if err := os.Rename(path, tmp); err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	f, err := os.Open(tmp)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	// Wait for writers that opened the file before the rename.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return 0, err
	}
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var ev event.Event
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if err := fn(&ev); err != nil {
			return n, err
		}
		n++
	}
	if err := sc.Err(); err != nil {
		return n, err
	}
	return n, os.Remove(tmp)
}
