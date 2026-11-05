package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/tazhate/agent-mem/internal/config"
	"github.com/tazhate/agent-mem/internal/daemon"
	"github.com/tazhate/agent-mem/internal/mcpserver"
	"github.com/tazhate/agent-mem/internal/store"
)

func init() {
	register("daemon", "run amemd (started automatically by hooks)", cmdDaemon)
}

func cmdDaemon(args []string) int {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	foreground := fs.Bool("foreground", false, "never exit on idle (for systemd)")
	fs.Parse(args)

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load()
	if err != nil {
		// Same fallback as the hooks: refusing to start would only grow the spool forever.
		cfg = config.Fallback(err)
	}
	for _, w := range cfg.Warnings {
		log.Warn("config", "problem", w)
	}
	lock, err := daemon.Lock(cfg)
	if errors.Is(err, daemon.ErrRunning) {
		return 0
	}
	if err != nil {
		return fail("lock: %v", err)
	}
	defer func() {
		_ = lock.Truncate(0) // a stale PID must not point at whatever process reuses it
		lock.Close()
	}()

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return fail("open db: %v", err)
	}
	defer st.Close()

	srv := daemon.New(cfg, st, log)
	attachWorkers(cfg, srv, log)
	if cfg.Daemon.HTTPAddr != "" {
		if err := mcpserver.CheckLoopback(cfg.Daemon.HTTPAddr); err != nil {
			return fail("%v", err)
		}
		token, err := mcpserver.HTTPToken(filepath.Join(cfg.DataDir, "http.token"))
		if err != nil {
			return fail("http token: %v", err)
		}
		srv.HTTP = mcpserver.HTTPHandler(func(op string, params, out any) error {
			return srv.CallLocal(context.Background(), op, params, out)
		}, token)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx, *foreground); err != nil {
		return fail("%v", err)
	}
	return 0
}
