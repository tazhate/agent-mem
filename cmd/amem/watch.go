package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tazhate/agent-mem/internal/daemon"
	"github.com/tazhate/agent-mem/internal/event"
	"github.com/tazhate/agent-mem/internal/store"
	"github.com/tazhate/agent-mem/internal/watch"
)

func init() {
	register("watch", "follow transcripts of runtimes without hooks: amem watch --preset claude|codex", cmdWatch)
}

func cmdWatch(args []string) int {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	preset := fs.String("preset", "codex", "transcript format: claude or codex")
	once := fs.Bool("once", false, "one pass, then exit")
	since := fs.Duration("since", 24*time.Hour, "on first sight, skip files not modified within this window")
	subagents := fs.Bool("subagents-only", false, "only subagent sessions (use when hooks already capture top-level sessions)")
	interval := fs.Duration("interval", 3*time.Second, "poll interval")
	fs.Parse(args)
	p, ok := watch.Presets[*preset]
	if !ok {
		return fail("unknown preset %q", *preset)
	}
	if *subagents && *preset != "codex" {
		return fail("--subagents-only works with --preset codex only (Claude transcripts do not mark subagents)")
	}
	cfg := loadCfg()
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return fail("%v", err)
	}
	defer st.Close()
	home, _ := os.UserHomeDir()
	sent := 0
	send := func(ev *event.Event) error {
		_, err := daemon.Call(cfg, &daemon.Request{Op: daemon.OpEvent, Event: ev}, 5*time.Second)
		if err != nil && daemon.IsUnavailable(err) {
			_ = daemon.Spawn(cfg)
			if daemon.WaitReady(cfg, 20*time.Second) {
				_, err = daemon.Call(cfg, &daemon.Request{Op: daemon.OpEvent, Event: ev}, 5*time.Second)
			}
		}
		if err != nil {
			return daemon.SpoolAppend(cfg.SpoolPath(), ev)
		}
		sent++
		return nil
	}
	w := watch.New(st, p, home, send)
	w.SubagentsOnly = *subagents
	w.Since = time.Now().Add(-*since)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := w.Run(ctx, *interval, *once); err != nil {
		return fail("%v", err)
	}
	fmt.Fprintf(os.Stderr, "watch: %d events sent\n", sent)
	return 0
}
