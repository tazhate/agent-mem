package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"agent-mem/internal/config"
	"agent-mem/internal/daemon"
	"agent-mem/internal/store"
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
	fs.Parse(args)
	q := strings.Join(fs.Args(), " ")
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
	if *typ != "" {
		p.Types = strings.Split(*typ, ",")
	}
	var hits []daemon.SearchHit
	if err := daemon.CallData(loadCfg(), daemon.OpSearch, p, &hits); err != nil {
		return fail("%v", err)
	}
	if *asJSON {
		return printJSON(hits)
	}
	for _, h := range hits {
		fmt.Printf("#%-7d %s %-10s %-18s %s (~%dt, %.2f)\n", h.ID, h.Date, h.Type, clipStr(h.Project, 18), h.Title, h.Tokens, h.Score)
	}
	return 0
}

func cmdShow(args []string) int {
	var ids []int64
	for _, a := range args {
		id, err := strconv.ParseInt(strings.TrimPrefix(a, "#"), 10, 64)
		if err != nil {
			return fail("bad id %q", a)
		}
		ids = append(ids, id)
	}
	var mems []*store.Memory
	if err := daemon.CallData(loadCfg(), daemon.OpGet, daemon.GetParams{IDs: ids}, &mems); err != nil {
		return fail("%v", err)
	}
	for _, m := range mems {
		fmt.Printf("#%d %s [%s] %s\n%s\n", m.ID, m.Type, m.Project, time.UnixMilli(m.CreatedAt).Format("2006-01-02 15:04"), m.Title)
		if m.Body != "" {
			fmt.Printf("\n%s\n", m.Body)
		}
		for _, f := range m.Facts {
			fmt.Printf("- %s\n", f)
		}
		if len(m.Files) > 0 {
			fmt.Printf("files: %s\n", strings.Join(m.Files, ", "))
		}
		fmt.Printf("used %d / injected %d\n\n", m.UseCount, m.InjectCount)
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
	fs.Parse(args)
	cwd, _ := os.Getwd()
	var out map[string]any
	if err := daemon.CallData(loadCfg(), daemon.OpSave, daemon.SaveParams{Text: strings.Join(fs.Args(), " "), Title: *title, Type: *typ, Global: *global, Cwd: cwd}, &out); err != nil {
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

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
