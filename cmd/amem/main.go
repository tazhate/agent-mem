// Command amem is persistent memory for AI coding agents: hooks, daemon, MCP server and tools
// in one binary.
package main

import (
	"fmt"
	"os"
	"slices"
)

type command struct {
	name    string
	summary string
	run     func(args []string) int
}

var commands []command

func register(name, summary string, run func([]string) int) {
	commands = append(commands, command{name, summary, run})
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	name := os.Args[1]
	i := slices.IndexFunc(commands, func(c command) bool { return c.name == name })
	if i < 0 {
		if name == "-h" || name == "--help" || name == "help" {
			usage()
			return
		}
		fmt.Fprintf(os.Stderr, "amem: unknown command %q\n\n", name)
		usage()
		os.Exit(2)
	}
	os.Exit(commands[i].run(os.Args[2:]))
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: amem <command> [flags]\n\ncommands:")
	for _, c := range commands {
		fmt.Fprintf(os.Stderr, "  %-14s %s\n", c.name, c.summary)
	}
}

func fail(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "amem: "+format+"\n", args...)
	return 1
}
