package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"agent-mem/internal/config"
	"agent-mem/internal/daemon"
	"agent-mem/internal/store"
)

func init() {
	register("daemon", "run amemd (started automatically by hooks)", cmdDaemon)
}

func cmdDaemon(args []string) int {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	foreground := fs.Bool("foreground", false, "never exit on idle (for systemd)")
	fs.Parse(args)

	cfg, err := config.Load()
	if err != nil {
		return fail("%v", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	lock, err := daemon.Lock(cfg)
	if errors.Is(err, daemon.ErrRunning) {
		return 0
	}
	if err != nil {
		return fail("lock: %v", err)
	}
	defer lock.Close()

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return fail("open db: %v", err)
	}
	defer st.Close()

	srv := daemon.New(cfg, st, log)
	attachWorkers(cfg, srv, log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx, *foreground); err != nil {
		return fail("%v", err)
	}
	return 0
}
