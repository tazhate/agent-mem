package redact

import (
	"strings"
	"testing"
)

func TestClean(t *testing.T) {
	r := New(true)
	cases := []struct {
		in       string
		mustLose string
		mustKeep string
	}{
		{"key AKIAABCDEFGHIJKLMNOP here", "AKIAABCDEFGHIJKLMNOP", "here"},
		{"export ANTHROPIC_API_KEY=sk-ant-api03-abcdefghijklmnopqrstuvwxyz", "abcdefghijklmnopqrstuvwxyz", "ANTHROPIC_API_KEY="},
		{`{"password": "hunter2hunter2"}`, "hunter2hunter2", `"password"`},
		{"Authorization: Bearer abcdef123456789xyz", "abcdef123456789xyz", "Authorization"},
		{"max_tokens: 10240000", "", "10240000"},
		{"vault token hvs.CAESIabcdefghijklmnopqrstu", "CAESIabcdefghijklmnopqrstu", "vault token"},
		{"before <private>secret stuff</private> after", "secret stuff", "after"},
		{"x <system-reminder>\ninjected\n</system-reminder> y", "injected", "y"},
		{"gh token ghp_abcdefghijklmnopqrstuvwxyz0123456789", "ghp_abcdefghij", "gh token"},
		{"--token=abcdefgh12345 run", "abcdefgh12345", "run"},
	}
	for _, c := range cases {
		got := r.Clean(c.in)
		if c.mustLose != "" && strings.Contains(got, c.mustLose) {
			t.Errorf("Clean(%q) = %q, still contains %q", c.in, got, c.mustLose)
		}
		if !strings.Contains(got, c.mustKeep) {
			t.Errorf("Clean(%q) = %q, lost %q", c.in, got, c.mustKeep)
		}
	}
}

func TestFullyPrivate(t *testing.T) {
	if !IsFullyPrivate("  <private>all of it</private> ") {
		t.Fatal("expected fully private")
	}
	if IsFullyPrivate("<private>a</private> b") {
		t.Fatal("expected not fully private")
	}
}
