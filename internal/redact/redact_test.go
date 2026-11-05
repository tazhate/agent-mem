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
		{"key AKIA" + "ABCDEFGHIJKLMNOP here", "AKIA" + "ABCDEFGHIJKLMNOP", "here"},
		{"export ANTHROPIC_API_KEY=sk-ant-" + "api03-abcdefghijklmnopqrstuvwxyz", "abcdefghijklmnopqrstuvwxyz", "ANTHROPIC_API_KEY="},
		{`{"password": "hunter2hunter2"}`, "hunter2hunter2", `"password"`},
		{"Authorization: Bearer abcdef123456789xyz", "abcdef123456789xyz", "Authorization"},
		{"max_tokens: 10240000", "", "10240000"},
		{"vault token hvs." + "CAESIabcdefghijklmnopqrstu", "CAESIabcdefghijklmnopqrstu", "vault token"},
		{"before <private>secret stuff</private> after", "secret stuff", "after"},
		{"x <system-reminder>\ninjected\n</system-reminder> y", "injected", "y"},
		{"gh token ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789", "ghp_" + "abcdefghij", "gh token"},
		{"--token=abcdefgh12345 run", "abcdefgh12345", "run"},
		{"DATABASE_URL=postgres://admin:Sup3rS3cret@db:5432/app", "Sup3rS3cret", "postgres://admin:"},
		{"remote glpat-" + "abcdefghijklmnopqrstu done", "glpat-" + "abcdefghij", "done"},
		{"//registry/:_authToken=npm_" + "abcdefghijklmnopqrstuvwxyz0123456789", "npm_" + "abcdefghij", "registry"},
		{"HF hf_" + "abcdefghijklmnopqrstuvwxyzABCDEFGH ok", "hf_" + "abcdefghij", "ok"},
		{"bot 123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw x", "AAHdqTcvCH1vGWJx", "bot"},
		{"Authorization: Basic dXNlcjpwYXNzd29yZA==", "dXNlcjpwYXNzd29yZA", "Authorization"},
		{"DefaultEndpointsProtocol=https;AccountKey=abcDEF123456789+/abcdefghij==;", "abcDEF123456789", "AccountKey="},
		{"head id_rsa\n-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAA", "b3BlbnNzaC1rZXkt", "head id_rsa"},
		{"x <PRIVATE>secret upper</PRIVATE> y", "secret upper", "y"},
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

func TestNoFalsePositive(t *testing.T) {
	r := New(true)
	for _, in := range []string{
		`<div class="sk-spinner-fading-circle-wrapper">`,
		"see https://example.com/docs:intro@section",
		"max_tokens: 4096",
	} {
		if got := r.Clean(in); got != in {
			t.Errorf("Clean(%q) = %q, want unchanged", in, got)
		}
	}
}

func BenchmarkCleanTypical(b *testing.B) {
	r := New(true)
	text := strings.Repeat("The daemon drains the spool every second and stores events in SQLite WAL mode. ", 20)
	for b.Loop() {
		r.Clean(text)
	}
}
