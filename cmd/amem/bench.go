package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/pprof"
	"slices"
	"strings"
	"time"

	"github.com/tazhate/agent-mem/internal/config"
	"github.com/tazhate/agent-mem/internal/daemon"
	"github.com/tazhate/agent-mem/internal/recall"
	"github.com/tazhate/agent-mem/internal/store"
)

func init() {
	register("bench", "measure latency: amem bench recall|hook [-n N]", cmdBench)
}

func cmdBench(args []string) int {
	if len(args) < 1 {
		return fail("usage: amem bench recall|hook [-n N]")
	}
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	n := fs.Int("n", 200, "iterations")
	prof := fs.String("cpuprofile", "", "write a CPU profile")
	fs.Parse(args[1:])
	if *prof != "" {
		f, err := os.Create(*prof)
		if err != nil {
			return fail("%v", err)
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			return fail("cpu profile: %v", err)
		}
		defer pprof.StopCPUProfile()
	}
	switch args[0] {
	case "recall":
		return benchRecall(*n)
	case "hook":
		return benchHook(*n)
	}
	return fail("unknown bench %q", args[0])
}

// benchRecall replays real user prompts from the database as recall queries, in process.
func benchRecall(n int) int {
	cfg := loadCfg()
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return fail("%v", err)
	}
	defer st.Close()
	ctx := context.Background()
	rows, err := st.DB().QueryContext(ctx, `SELECT e.input, coalesce(s.project_id, 0) FROM events e JOIN sessions s ON s.id = e.session_id
		WHERE e.kind = 'prompt' AND length(e.input) BETWEEN 20 AND 2000 ORDER BY random() LIMIT ?`, n)
	if err != nil {
		return fail("%v", err)
	}
	type q struct {
		text string
		pid  int64
	}
	var qs []q
	for rows.Next() {
		var x q
		if rows.Scan(&x.text, &x.pid) == nil {
			qs = append(qs, x)
		}
	}
	rows.Close()
	if len(qs) == 0 {
		return fail("no prompts in database to replay")
	}
	eng := recall.New(st, cfg.Recall, nil)
	var lat []time.Duration
	found := 0
	for _, x := range qs {
		t := time.Now()
		hits, err := eng.Search(ctx, x.text, store.Scope{ProjectID: x.pid}, cfg.Recall.PromptLimit)
		lat = append(lat, time.Since(t))
		if err != nil {
			return fail("%v", err)
		}
		if len(hits) > 0 && hits[0].Score >= cfg.Recall.MinScore {
			found++
		}
	}
	report("recall search", lat)
	fmt.Printf("  queries with an injectable hit (score >= %.2f): %d/%d\n", cfg.Recall.MinScore, found, len(qs))
	return 0
}

// benchHook runs the real binary as a PostToolUse hook against a scratch daemon in a temporary
// AMEM_HOME, so the measurement never writes into the real memory database.
func benchHook(n int) int {
	exe, _ := os.Executable()
	home, err := os.MkdirTemp("", "amem-bench-")
	if err != nil {
		return fail("%v", err)
	}
	defer os.RemoveAll(home)
	os.WriteFile(filepath.Join(home, "config.toml"), []byte("[observer]\nenabled = false\n"), 0o600)
	env := append(os.Environ(), "AMEM_HOME="+home)
	defer func() {
		// Stop the scratch daemon over its own socket (never by a PID read from a file) and
		// wait until it released its lock, so it cannot recreate the temp dir after removal.
		scratch := loadCfgAt(home)
		if daemon.WaitReady(scratch, 5*time.Second) {
			_, _ = daemon.Call(scratch, &daemon.Request{Op: daemon.OpRestart}, time.Second)
		}
		daemon.WaitStopped(scratch, 5*time.Second)
	}()
	payload := `{"session_id":"bench","cwd":"/tmp","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"true"},"tool_response":{"stdout":""}}`
	var lat []time.Duration
	for i := range n {
		cmd := exec.Command(exe, "hook", "claude-code")
		cmd.Env = env
		cmd.Stdin = strings.NewReader(payload)
		t := time.Now()
		if err := cmd.Run(); err != nil {
			return fail("run %d: %v", i, err)
		}
		lat = append(lat, time.Since(t))
	}
	report("capture hook (process start to exit)", lat)
	return 0
}

func report(name string, lat []time.Duration) {
	slices.Sort(lat)
	p := func(q float64) time.Duration { return lat[min(len(lat)-1, int(float64(len(lat))*q))] }
	fmt.Printf("%s: n=%d p50=%s p90=%s p99=%s max=%s\n", name, len(lat),
		p(0.5).Round(time.Microsecond), p(0.9).Round(time.Microsecond), p(0.99).Round(time.Microsecond), lat[len(lat)-1].Round(time.Microsecond))
}

// loadCfgAt returns defaults rooted at a scratch data directory.
func loadCfgAt(home string) *config.Config {
	c := config.Default()
	c.DataDir = home
	return c
}
