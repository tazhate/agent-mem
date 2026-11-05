package main

import (
	"log/slog"

	"agent-mem/internal/config"
	"agent-mem/internal/daemon"
)

// attachWorkers wires background workers into the daemon.
func attachWorkers(cfg *config.Config, srv *daemon.Server, log *slog.Logger) {}
