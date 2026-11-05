package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tazhate/agent-mem/internal/config"
	"github.com/tazhate/agent-mem/internal/daemon"
	"github.com/tazhate/agent-mem/internal/importer/claudemem"
	"github.com/tazhate/agent-mem/internal/redact"
	"github.com/tazhate/agent-mem/internal/store"
)

func init() {
	register("import", "import memory from another system: amem import claude-mem [flags]", cmdImport)
}

func cmdImport(args []string) int {
	if len(args) < 1 || args[0] != "claude-mem" {
		return fail("usage: amem import claude-mem [--db PATH] [--dry-run] [--raw 30d|all] [--project-map FILE] [--roots DIRS]")
	}
	home, _ := os.UserHomeDir()
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	src := fs.String("db", filepath.Join(home, ".claude-mem", "claude-mem.db"), "claude-mem database")
	dry := fs.Bool("dry-run", false, "read and report, write nothing")
	raw := fs.String("raw", "", "also import raw tool uses: a duration like 30d, or all")
	pmap := fs.String("project-map", "", `JSON file {"claude-mem project": "/path/to/repo"}`)
	roots := fs.String("roots", strings.Join([]string{filepath.Join(home, "repos"), home}, ","), "comma-separated dirs searched for <root>/<project>/.git")
	fs.Parse(args[1:])

	opt := claudemem.Options{Source: *src, DryRun: *dry, Log: os.Stderr}
	for r := range strings.SplitSeq(*roots, ",") {
		if r = strings.TrimSpace(r); r != "" {
			opt.SearchRoots = append(opt.SearchRoots, r)
		}
	}
	switch {
	case *raw == "all":
		opt.RawSince = -1
	case *raw != "":
		d, err := parseDays(*raw)
		if err != nil {
			return fail("--raw: %v", err)
		}
		opt.RawSince = time.Now().Add(-d).UnixMilli()
	}
	if *pmap != "" {
		b, err := os.ReadFile(*pmap)
		if err != nil {
			return fail("%v", err)
		}
		if err := json.Unmarshal(b, &opt.ProjectMap); err != nil {
			return fail("project map: %v", err)
		}
	}
	cfg, err := config.Load()
	if err != nil {
		return fail("%v", err)
	}
	red := redact.New(cfg.Capture.RedactGeneric)
	opt.Clean = func(v string) string { return daemon.CleanText(red, cfg.Capture.Redact, v, cfg.Capture.MaxFieldChars) }
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return fail("%v", err)
	}
	defer st.Close()
	fmt.Fprintf(os.Stderr, "importing %s -> %s%s\n", *src, cfg.DBPath(), map[bool]string{true: " (dry run)"}[*dry])
	rep, err := claudemem.Run(context.Background(), st, opt)
	fmt.Fprintln(os.Stderr)
	if rep != nil {
		rep.Print(os.Stdout)
	}
	if err != nil {
		return fail("%v", err)
	}
	return 0
}

// parseDays accepts Go durations plus a "d" suffix for days.
func parseDays(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		var days int
		if _, err := fmt.Sscanf(n, "%d", &days); err != nil {
			return 0, err
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}
