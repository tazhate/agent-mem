package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/tazhate/agent-mem/internal/daemon"
	"github.com/tazhate/agent-mem/internal/embed"
	"github.com/tazhate/agent-mem/internal/install"
	"github.com/tazhate/agent-mem/internal/provider"
	"github.com/tazhate/agent-mem/internal/store"
)

func init() {
	register("doctor", "check daemon, hooks, providers and the database", cmdDoctor)
}

func cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	probe := fs.Bool("probe", false, "also send a tiny request to the observer provider and embedder")
	fs.Parse(args)
	cfg := loadCfg()
	failures := 0
	ok := func(good bool, format string, a ...any) {
		mark := "ok  "
		if !good {
			mark = "FAIL"
			failures++
		}
		fmt.Printf("%s %s\n", mark, fmt.Sprintf(format, a...))
	}
	fmt.Printf("config   %s\ndata     %s\n", cfg.File(), cfg.DataDir)
	for _, w := range cfg.Warnings {
		ok(false, "config: %s", w)
	}

	var ping struct {
		Version  string   `json:"version"`
		PID      int      `json:"pid"`
		Warnings []string `json:"warnings"`
	}
	resp, err := daemon.Call(cfg, &daemon.Request{Op: daemon.OpPing}, time.Second)
	if err != nil && daemon.IsUnavailable(err) {
		// Not running is normal; not being able to start is not.
		if serr := daemon.StartAndWait(cfg, 15*time.Second); serr != nil {
			ok(false, "daemon cannot start: %v", serr)
		} else {
			resp, err = daemon.Call(cfg, &daemon.Request{Op: daemon.OpPing}, time.Second)
		}
	}
	if err == nil {
		_ = json.Unmarshal(resp.Data, &ping)
		ok(ping.Version == daemon.Version, "daemon running, pid %d, version %s (binary %s)", ping.PID, ping.Version, daemon.Version)
		for _, w := range ping.Warnings {
			if !slices.Contains(cfg.Warnings, w) {
				ok(false, "daemon config: %s", w)
			}
		}
		var stats struct {
			Stats store.Stats `json:"stats"`
		}
		if err := daemon.CallData(cfg, daemon.OpStats, struct{}{}, &stats); err == nil {
			st := stats.Stats
			fmt.Printf("     memories %d, summaries %d, sessions %d, projects %d, events %d (pending %d), vectors %d\n",
				st.Memories, st.Summaries, st.Sessions, st.Projects, st.Events, st.PendingEvents, st.Vectors)
		} else {
			ok(false, "stats: %v", err)
		}
	} else if !daemon.IsUnavailable(err) {
		ok(false, "daemon does not answer: %v", err)
	}
	if st, err := os.Stat(cfg.SpoolPath()); err == nil {
		ok(st.Size() == 0, "spool has %d bytes waiting", st.Size())
	}

	o, err := installOptions(true, false)
	if err == nil {
		inst := install.Installed(o)
		for _, rt := range install.Runtimes() {
			switch inst[rt] {
			case install.HooksCurrent:
				ok(true, "%s hooks installed", rt)
			case install.HooksStale:
				ok(false, "%s hooks run another amem binary, not %s (amem install %s)", rt, o.Binary, rt)
			}
		}
		for _, rt := range install.Detected(o) {
			if inst[rt] == "" {
				fmt.Printf("     %s detected but not installed (amem install %s)\n", rt, rt)
			}
		}
	}

	ctx := context.Background()
	if cfg.Observer.Enabled {
		ch, err := provider.NewChainWithDataDir(ctx, cfg.Observer.Providers, cfg.DataDir)
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
	emb, err := embed.NewEmbedder(ctx, cfg.Embed, cfg.ModelsDir())
	switch {
	case err != nil:
		ok(false, "embedder: %v", err)
	case emb == nil:
		fmt.Println("     embedder: none (full-text search only)")
	default:
		where := cfg.Embed.BaseURL
		if cfg.Embed.Kind == "static" {
			where = embed.StaticDir(cfg.Embed.Model, cfg.ModelsDir())
		}
		ok(true, "embedder: %s (%s) at %s", emb.Model(), cfg.Embed.Kind, where)
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
	if failures > 0 {
		return 1
	}
	return 0
}
