package main

import (
	"flag"
	"strings"
)

// parseInterspersed lets flags follow positional arguments (`amem search "q" -n 5`). Only
// flags the command defines are taken out; once text has started, anything else that starts
// with a dash ("-race", "-1") stays part of the text. Before the first positional, unknown
// flags are still errors. A value-taking flag consumes the next token even if it is "--";
// a bare "--" ends flag parsing.
func parseInterspersed(fs *flag.FlagSet, args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		name, hasValue := flagName(a)
		f := fs.Lookup(name)
		switch {
		case name == "" || (f == nil && len(positional) > 0):
			positional = append(positional, a)
		case f == nil:
			flags = append(flags, a) // let fs report the unknown flag
		default:
			flags = append(flags, a)
			if !hasValue && !isBool(f) && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		}
	}
	_ = fs.Parse(flags) // ExitOnError exits on bad input
	return append(positional, fs.Args()...)
}

// flagName returns the name of a "-x", "--x" or "--x=v" token, or "" for anything else.
func flagName(a string) (string, bool) {
	if len(a) < 2 || a[0] != '-' {
		return "", false
	}
	n := strings.TrimLeft(a, "-")
	if n == "" || len(a)-len(n) > 2 {
		return "", false
	}
	if k, _, ok := strings.Cut(n, "="); ok {
		return k, true
	}
	return n, false
}

func isBool(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}
