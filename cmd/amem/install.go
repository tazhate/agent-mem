package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"agent-mem/internal/install"
)

func init() {
	register("install", "register hooks and MCP: amem install <runtime|detected> [--uninstall] [--dry-run]", cmdInstall)
}

func installOptions(dry, skipMCP bool) (install.Options, error) {
	bin, err := install.Binary()
	if err != nil {
		return install.Options{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return install.Options{}, err
	}
	return install.Options{Home: home, Binary: bin, DryRun: dry, SkipMCP: skipMCP,
		Out: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f, a...) }}, nil
}

func cmdInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	uninstall := fs.Bool("uninstall", false, "remove amem from the runtime")
	dry := fs.Bool("dry-run", false, "show the changes without writing")
	noMCP := fs.Bool("no-mcp", false, "hooks only, skip MCP registration")
	if len(args) == 0 {
		return fail("usage: amem install <%s|detected> [--uninstall] [--dry-run] [--no-mcp]", strings.Join(install.Runtimes(), "|"))
	}
	target := args[0]
	fs.Parse(args[1:])
	o, err := installOptions(*dry, *noMCP)
	if err != nil {
		return fail("%v", err)
	}
	targets := []string{target}
	if target == "detected" {
		targets = install.Detected(o)
	}
	return applyInstall(o, targets, *uninstall)
}

func applyInstall(o install.Options, targets []string, uninstall bool) int {
	code := 0
	for _, rt := range targets {
		changes, err := install.Run(o, rt, uninstall)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", rt, err)
			code = 1
			continue
		}
		for _, c := range changes {
			if len(c.Cmd) == 0 && c.Before == c.After {
				continue
			}
			if o.DryRun {
				fmt.Print(install.Diff(c))
			} else if len(c.Cmd) > 0 {
				fmt.Printf("%s: ran %s\n", rt, strings.Join(c.Cmd, " "))
			} else {
				fmt.Printf("%s: updated %s (backup next to it)\n", rt, c.Path)
			}
		}
		if rt == "codex" && !uninstall && !o.DryRun {
			fmt.Println("codex: start codex once and trust the new hooks when it asks")
		}
	}
	return code
}
