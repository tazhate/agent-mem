package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/tazhate/agent-mem/internal/adapter"
	"github.com/tazhate/agent-mem/internal/config"
	"github.com/tazhate/agent-mem/internal/daemon"
	"github.com/tazhate/agent-mem/internal/event"
)

func init() {
	register("hook", "handle a runtime hook: amem hook <runtime> [event] < payload", cmdHook)
}

const maxStdin = 4 << 20

// cmdHook never blocks or fails the agent: every error path exits 0 with a valid (possibly
// empty) answer for the runtime.
func cmdHook(args []string) (code int) {
	defer func() {
		if r := recover(); r != nil {
			debugf("panic: %v", r)
			code = 0
		}
	}()
	if len(args) < 1 {
		return 0
	}
	ad, err := adapter.Get(args[0])
	if err != nil {
		debugf("%v", err)
		return 0
	}
	hookArg := ""
	if len(args) > 1 {
		hookArg = args[1]
	}
	empty := func() int {
		out(ad.Render(&event.Event{}, hookArg, ""))
		return 0
	}
	if os.Getenv("AMEM_DISABLE") == "1" {
		return empty()
	}
	raw := readStdin(2 * time.Second)
	ev, err := ad.Parse(hookArg, raw, os.Getenv)
	if err != nil {
		debugf("parse: %v", err)
		return empty()
	}
	ev.ID = event.NewID()
	cfg, err := config.Load()
	if err != nil {
		debugf("%v", err)
		cfg = config.Default()
		cfg.DataDir = config.Home()
	}
	timeout := 400 * time.Millisecond
	if ev.Kind.WantsContext() {
		timeout = 1500 * time.Millisecond
	}
	// Ask the adapter whether this hook can carry context at all; recall is skipped otherwise.
	req := &daemon.Request{Op: daemon.OpEvent, Event: ev, NoContext: ad.Render(ev, hookArg, "probe") == nil}
	resp, err := daemon.Call(cfg, req, timeout)
	if err != nil && daemon.IsUnavailable(err) {
		if serr := daemon.Spawn(cfg); serr != nil {
			debugf("spawn: %v", serr)
		}
		// Session start is worth a short wait: it is the one injection a new session gets.
		if ev.Kind == event.SessionStart && daemon.WaitReady(cfg, 800*time.Millisecond) {
			resp, err = daemon.Call(cfg, req, timeout)
		}
	}
	if err != nil {
		// Whatever went wrong (no daemon, crash mid-request, timeout), keep the event: the
		// daemon drains the spool on start and drops duplicates by event id.
		debugf("call: %v", err)
		if serr := daemon.SpoolAppend(cfg.SpoolPath(), ev); serr != nil {
			debugf("spool: %v", serr)
		}
		return empty()
	}
	out(ad.Render(ev, hookArg, resp.Context))
	return 0
}

func out(b []byte) {
	if len(b) > 0 {
		os.Stdout.Write(append(b, '\n'))
	}
}

// readStdin reads the payload with a size cap and a timeout, so a runtime that never closes
// stdin cannot hang the hook.
func readStdin(timeout time.Duration) []byte {
	done := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(os.Stdin, maxStdin))
		done <- b
	}()
	select {
	case b := <-done:
		return bytes.TrimSpace(b)
	case <-time.After(timeout):
		return nil
	}
}

func debugf(format string, args ...any) {
	if os.Getenv("AMEM_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "amem hook: "+format+"\n", args...)
	}
}
