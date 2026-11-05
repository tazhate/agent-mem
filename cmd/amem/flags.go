package main

import "flag"

// parseInterspersed parses flags anywhere in args, so `amem search "query" -n 5` works the same
// as `amem search -n 5 "query"`. The standard flag package stops at the first positional
// argument. A literal "--" ends flag parsing.
func parseInterspersed(fs *flag.FlagSet, args []string) []string {
	var positional []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return positional // ExitOnError sets exit before this
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		// fs.Parse consumed a "--" terminator if the remaining args are shorter than expected.
		consumed := len(args) - len(rest)
		if consumed > 0 && args[consumed-1] == "--" {
			positional = append(positional, rest...)
			break
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
	return positional
}
