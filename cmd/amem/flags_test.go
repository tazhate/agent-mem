package main

import (
	"flag"
	"slices"
	"testing"
)

func TestParseInterspersed(t *testing.T) {
	for _, c := range []struct {
		args   []string
		pos    []string
		n      int
		title  string
		global bool
	}{
		{[]string{"vault", "sealed", "-n", "5"}, []string{"vault", "sealed"}, 5, "", false},
		{[]string{"-n", "3", "q"}, []string{"q"}, 3, "", false},
		{[]string{"use", "-race", "in", "CI", "--global"}, []string{"use", "-race", "in", "CI"}, 10, "", true},
		{[]string{"fix", "the", "-1", "offset"}, []string{"fix", "the", "-1", "offset"}, 10, "", false},
		{[]string{"--title", "--", "hello", "--global"}, []string{"hello"}, 10, "--", true},
		{[]string{"x", "--", "-n", "lit"}, []string{"x", "-n", "lit"}, 10, "", false},
		{[]string{"a", "--title=t", "b"}, []string{"a", "b"}, 10, "t", false},
	} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		n := fs.Int("n", 10, "")
		title := fs.String("title", "", "")
		global := fs.Bool("global", false, "")
		got := parseInterspersed(fs, c.args)
		if !slices.Equal(got, c.pos) || *n != c.n || *title != c.title || *global != c.global {
			t.Errorf("%v: pos=%q n=%d title=%q global=%v", c.args, got, *n, *title, *global)
		}
	}
}
