package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/tazhate/agent-mem/internal/embed"
)

func init() {
	register("embed", "embedding models: amem embed pull [--model repo]", cmdEmbed)
}

func cmdEmbed(args []string) int {
	if len(args) < 1 || args[0] != "pull" {
		return fail("usage: amem embed pull [--model %s]", embed.DefaultStatic)
	}
	fs := flag.NewFlagSet("embed pull", flag.ExitOnError)
	model := fs.String("model", "", "Hugging Face repo of a model2vec model (default "+embed.DefaultStatic+")")
	fs.Parse(args[1:])
	cfg := loadCfg()
	repo := *model
	if repo == "" && cfg.Embed.Kind == "static" && cfg.Embed.Model != "" {
		repo = cfg.Embed.Model
	}
	dir, err := embed.Pull(context.Background(), repo, cfg.ModelsDir(), func(s string) { fmt.Fprintln(os.Stderr, s) })
	if err != nil {
		return fail("%v", err)
	}
	fmt.Println(dir)
	return 0
}
