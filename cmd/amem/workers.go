package main

import (
	"context"
	"log/slog"

	"agent-mem/internal/config"
	"agent-mem/internal/daemon"
	"agent-mem/internal/embed"
	"agent-mem/internal/observer"
)

// attachWorkers wires background workers into the daemon.
func attachWorkers(cfg *config.Config, srv *daemon.Server, log *slog.Logger) {
	if cfg.Observer.Enabled {
		srv.AddWorker(observer.New(srv.Store(), cfg.Observer, srv.Recall(), log.With("worker", "observer")))
	}
	emb, err := embed.NewEmbedder(context.Background(), cfg.Embed)
	if err != nil {
		log.Error("embedder disabled", "err", err)
		return
	}
	if emb != nil {
		idx := embed.NewIndex(srv.Store(), emb, cfg.Embed, log.With("worker", "embed"), srv.LastActivity)
		srv.Recall().SetVectors(idx)
		srv.AddWorker(idx)
	}
}
