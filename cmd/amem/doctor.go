package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/tazhate/agent-mem/internal/daemon"
	"github.com/tazhate/agent-mem/internal/embed"
	"github.com/tazhate/agent-mem/internal/install"
	"github.com/tazhate/agent-mem/internal/provider"
)

func init() {
	register("doctor", "check daemon, hooks, providers and the database", cmdDoctor)
}

func cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	probe := fs.Bool("probe", false, "also send a tiny request to the observer provider and embedder")
	fs.Parse(args)
	cfg := loadCfg()
	ok := func(good bool, format string, a ...any) {
		mark := "ok  "
		if !good {
			mark = "FAIL"
		}
		fmt.Printf("%s %s\n", mark, fmt.Sprintf(format, a...))
	}
	fmt.Printf("config   %s\ndata     %s\n", cfg.File(), cfg.DataDir)

	var ping map[string]any
	resp, err := daemon.Call(cfg, &daemon.Request{Op: daemon.OpPing}, time.Second)
	if err == nil {
		_ = jsonUnmarshal(resp.Data, &ping)
		pid, _ := ping["pid"].(float64)
		ok(ping["version"] == daemon.Version, "daemon running, pid %d, version %v (binary %s)", int(pid), ping["version"], daemon.Version)
	} else {
		ok(true, "daemon not running (starts on the next hook): %v", err)
	}
	var stats map[string]any
	if err := daemon.CallData(cfg, daemon.OpStats, struct{}{}, &stats); err == nil {
		fmt.Printf("     db stats: %v\n", stats["stats"])
	} else {
		ok(false, "stats: %v", err)
	}
	if st, err := os.Stat(cfg.SpoolPath()); err == nil {
		ok(st.Size() == 0, "spool has %d bytes waiting", st.Size())
	}

	o, err := installOptions(true, false)
	if err == nil {
		inst := install.Installed(o)
		for _, rt := range install.Runtimes() {
			if inst[rt] {
				ok(true, "%s hooks installed", rt)
			}
		}
		for _, rt := range install.Detected(o) {
			if !inst[rt] {
				fmt.Printf("     %s detected but not installed (amem install %s)\n", rt, rt)
			}
		}
	}

	ctx := context.Background()
	if cfg.Observer.Enabled {
		ch, err := provider.NewChain(ctx, cfg.Observer.Providers)
		if err != nil {
			ok(false, "observer: %v", err)
		} else {
			ok(true, "observer providers: %s (language %s, recall mode %s)", ch.Name(), cfg.Observer.Language, cfg.Recall.Mode)
			if *probe {
				t := time.Now()
				out, err := ch.Complete(ctx, provider.Request{System: "Reply with JSON.", User: `Return {"ok": true}.`,
					Schema: map[string]any{"type": "object", "additionalProperties": false, "required": []string{"ok"},
						"properties": map[string]any{"ok": map[string]any{"type": "boolean"}}}})
				ok(err == nil, "observer probe %s: %s %v", time.Since(t).Round(time.Millisecond), out, err)
			}
		}
	} else {
		fmt.Println("     observer disabled: events are captured, no new memories are written")
	}
	emb, err := embed.NewEmbedder(ctx, cfg.Embed)
	switch {
	case err != nil:
		ok(false, "embedder: %v", err)
	case emb == nil:
		fmt.Println("     embedder: none (full-text search only)")
	default:
		ok(true, "embedder: %s at %s", emb.Model(), cfg.Embed.BaseURL)
		if *probe {
			t := time.Now()
			v, err := emb.Embed(ctx, []string{"probe"})
			dim := 0
			if err == nil && len(v) > 0 {
				dim = len(v[0])
			}
			ok(err == nil, "embed probe %s: dim %d %v", time.Since(t).Round(time.Millisecond), dim, err)
		}
	}
	return 0
}
