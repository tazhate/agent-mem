package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/tazhate/agent-mem/internal/redact"
	"github.com/tazhate/agent-mem/internal/store"
)

func init() {
	register("scrub", "re-apply redaction to everything already stored: amem scrub [--dry-run]", cmdScrub)
}

func cmdScrub(args []string) int {
	fs := flag.NewFlagSet("scrub", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "count rows that would change, write nothing")
	fs.Parse(args)
	cfg := loadCfg()
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return fail("%v", err)
	}
	defer st.Close()
	r := redact.New(cfg.Capture.RedactGeneric)
	rep, err := st.Scrub(context.Background(), r.Clean, *dry)
	if err != nil {
		return fail("%v", err)
	}
	verb := "changed"
	if *dry {
		verb = "would change"
	}
	fmt.Printf("%s: %d memories, %d summaries, %d events, %d work entries\n", verb, rep.Memories, rep.Summaries, rep.Events, rep.WorkEntries)
	return 0
}
