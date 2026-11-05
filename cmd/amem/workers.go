package main

import (
	"log/slog"

	"agent-mem/internal/config"
	"agent-mem/internal/daemon"
	"agent-mem/internal/observer"
)

// attachWorkers wires background workers into the daemon.
func attachWorkers(cfg *config.Config, srv *daemon.Server, log *slog.Logger) {
	if cfg.Observer.Enabled {
		srv.AddWorker(observer.New(srv.Store(), cfg.Observer, srv.Recall(), log.With("worker", "observer")))
	}
}
