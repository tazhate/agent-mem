package main

import (
	"context"
	"log/slog"

	"github.com/tazhate/agent-mem/internal/config"
	"github.com/tazhate/agent-mem/internal/daemon"
	"github.com/tazhate/agent-mem/internal/embed"
	"github.com/tazhate/agent-mem/internal/observer"
	"github.com/tazhate/agent-mem/internal/redact"
)

// attachWorkers wires background workers into the daemon.
func attachWorkers(cfg *config.Config, srv *daemon.Server, log *slog.Logger) {
	if cfg.Observer.Enabled {
		obs := observer.New(srv.Store(), cfg.Observer, srv.Recall(), log.With("worker", "observer"))
		if cfg.Capture.Redact {
			obs.SetRedactor(redact.New(cfg.Capture.RedactGeneric))
		}
		srv.AddWorker(obs)
	}
	emb, err := embed.NewEmbedder(context.Background(), cfg.Embed, cfg.ModelsDir())
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
