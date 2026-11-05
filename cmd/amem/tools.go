package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tazhate/agent-mem/internal/config"
	"github.com/tazhate/agent-mem/internal/daemon"
	"github.com/tazhate/agent-mem/internal/mcpserver"
	"github.com/tazhate/agent-mem/internal/store"
)

func init() {
	register("search", "search memory: amem search [--project P] [--type T] query", cmdSearch)
	register("show", "print full memories: amem show <id>...", cmdShow)
	register("stats", "database and daemon status", cmdStats)
	register("shadow-report", "what shadow-mode recall would have injected (last 7 days)", cmdShadow)
	register("save", "save a manual memory: amem save [--global] [--type T] text", cmdSave)
}

func loadCfg() *config.Config {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "amem:", err)
		os.Exit(1)
	}
	return cfg
}

func cmdSearch(args []string) int {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	proj := fs.String("project", "", "project name (default: project of the current directory; 'all' for every project)")
	typ := fs.String("type", "", "comma-separated types")
	limit := fs.Int("n", 10, "results")
	asJSON := fs.Bool("json", false, "JSON output")
	q := strings.Join(parseInterspersed(fs, args), " ")
	if q == "" {
		return fail("empty query")
	}
	p := daemon.SearchParams{Query: q, Limit: *limit}
	switch *proj {
	case "all":
	case "":
		p.Cwd, _ = os.Getwd()
	default:
		p.Project = *proj
	}
	for t := range strings.SplitSeq(*typ, ",") {
		if t = strings.TrimSpace(t); t != "" {
			p.Types = append(p.Types, t)
		}
	}
	var hits []daemon.SearchHit
	if err := daemon.CallData(loadCfg(), daemon.OpSearch, p, &hits); err != nil {
		return fail("%v", err)
	}
	if *asJSON {
		return printJSON(hits)
	}
	if len(hits) == 0 {
		fmt.Fprintln(os.Stderr, "no matches")
		return 1
	}
	for _, h := range hits {
		fmt.Printf("#%-7d %s %-10s %-18s %s (~%dt, %.2f)\n", h.ID, h.Date, h.Type, clipStr(h.Project, 18), h.Title, h.Tokens, h.Score)
	}
	return 0
}

func cmdShow(args []string) int {
	if len(args) == 0 {
		return fail("usage: amem show <id>...")
	}
	var ids []int64
	for _, a := range args {
		id, err := strconv.ParseInt(strings.TrimPrefix(a, "#"), 10, 64)
		if err != nil {
			return fail("bad id %q", a)
		}
		ids = append(ids, id)
	}
	var mems []*store.Memory
	if err := daemon.CallData(loadCfg(), daemon.OpGet, daemon.GetParams{IDs: ids, IncludePrivate: true}, &mems); err != nil {
		return fail("%v", err)
	}
	found := map[int64]bool{}
	for _, m := range mems {
		found[m.ID] = true
	}
	for _, id := range ids {
		if !found[id] {
			fmt.Fprintf(os.Stderr, "not found: #%d\n", id)
		}
	}
	for _, m := range mems {
		fmt.Print(strings.TrimSuffix(mcpserver.FormatMemories([]*store.Memory{m}), "\n"))
		priv := ""
		if m.Private {
			priv = ", private (never shown to agents)"
		}
		fmt.Printf("used %d / injected %d%s\n\n", m.UseCount, m.InjectCount, priv)
	}
	if len(mems) < len(ids) {
		return 1
	}
	return 0
}

func cmdStats(args []string) int {
	var out map[string]any
	if err := daemon.CallData(loadCfg(), daemon.OpStats, struct{}{}, &out); err != nil {
		return fail("%v", err)
	}
	return printJSON(out)
}

func cmdShadow(args []string) int {
	var rows []store.ShadowRow
	if err := daemon.CallData(loadCfg(), daemon.OpShadow, struct{}{}, &rows); err != nil {
		return fail("%v", err)
	}
	if len(rows) == 0 {
		fmt.Println("no shadow injections in the last 7 days")
		return 0
	}
	for _, r := range rows {
		fmt.Printf("%s %-14s %-40s #%d %s\n", time.UnixMilli(r.TS).Format("01-02 15:04"), r.Trigger, clipStr(r.SessionID, 40), r.MemoryID, r.Title)
	}
	return 0
}

func cmdSave(args []string) int {
	fs := flag.NewFlagSet("save", flag.ExitOnError)
	global := fs.Bool("global", false, "visible in every project")
	typ := fs.String("type", "", "memory type")
	title := fs.String("title", "", "title (default: first line)")
	text := strings.Join(parseInterspersed(fs, args), " ")
	cwd, _ := os.Getwd()
	var out map[string]any
	if err := daemon.CallData(loadCfg(), daemon.OpSave, daemon.SaveParams{Text: text, Title: *title, Type: *typ, Global: *global, Cwd: cwd}, &out); err != nil {
		return fail("%v", err)
	}
	return printJSON(out)
}

func printJSON(v any) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fail("%v", err)
	}
	return 0
}

func clipStr(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
