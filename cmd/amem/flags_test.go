package main

import (
	"flag"
	"slices"
	"testing"
)

func TestParseInterspersed(t *testing.T) {
	for _, c := range []struct {
		args []string
		pos  []string
		n    int
		proj string
	}{
		{[]string{"vault", "sealed", "-n", "5"}, []string{"vault", "sealed"}, 5, ""},
		{[]string{"-n", "3", "q"}, []string{"q"}, 3, ""},
		{[]string{"a", "--project", "all", "b"}, []string{"a", "b"}, 10, "all"},
		{[]string{"x", "--", "-n", "lit"}, []string{"x", "-n", "lit"}, 10, ""},
	} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		n := fs.Int("n", 10, "")
		p := fs.String("project", "", "")
		got := parseInterspersed(fs, c.args)
		if !slices.Equal(got, c.pos) || *n != c.n || *p != c.proj {
			t.Errorf("%v: pos=%v n=%d project=%q", c.args, got, *n, *p)
		}
	}
}
