// Package redact strips private blocks and secrets before anything reaches the database or an LLM.
package redact

import (
	"regexp"
	"strings"
)

// strippedTags are removed together with their content: user-marked private text and context
// that runtimes or memory tools injected (re-recording it would feed memory back into memory).
var strippedTags = []string{"private", "system-reminder", "amem-context", "claude-mem-context", "persisted-output", "system_instruction", "system-instruction"}

var tagPatterns = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(strippedTags))
	for _, t := range strippedTags {
		out = append(out, regexp.MustCompile(`(?s)<`+regexp.QuoteMeta(t)+`(?:\s[^>]*)?>.*?</`+regexp.QuoteMeta(t)+`>`))
	}
	return out
}()

// StripTags removes private and injected blocks.
func StripTags(s string) string {
	if !strings.Contains(s, "<") {
		return s
	}
	for _, re := range tagPatterns {
		s = re.ReplaceAllString(s, "")
	}
	return s
}

// IsFullyPrivate reports whether nothing is left once private blocks are removed.
func IsFullyPrivate(s string) bool {
	return strings.Contains(s, "<private") && strings.TrimSpace(StripTags(s)) == ""
}

type rule struct {
	name string
	re   *regexp.Regexp
	// group is the submatch holding the secret; 0 means the whole match. RE2 has no lookbehind,
	// so keyed patterns capture the key as group 1 and the value as group 2.
	group int
	// needLetter rejects values without a letter (counts, ports, timestamps).
	needLetter bool
}

const genericValue = `[A-Za-z0-9_.\-+/=~@#$%^*!?]{8,}`

var specific = []rule{
	{name: "private_key", re: regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY(?: BLOCK)?-----.*?-----END [A-Z ]*PRIVATE KEY(?: BLOCK)?-----`)},
	{name: "aws_access_key", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{name: "aws_secret_key", re: regexp.MustCompile(`(?i)((?:aws_secret_access_key|secretaccesskey)["']?\s{0,8}[=:]\s{0,8}["']?)([A-Za-z0-9/+=]{40})`), group: 2},
	{name: "github_token", re: regexp.MustCompile(`\bgh[oprsu]_[A-Za-z0-9]{36}\b|\bgithub_pat_[A-Za-z0-9_]{82}\b`)},
	{name: "anthropic_key", re: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)},
	{name: "openai_key", re: regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`)},
	{name: "slack_token", re: regexp.MustCompile(`\b(?:xox[abeprs]|xapp)-[A-Za-z0-9-]{10,}`)},
	{name: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)},
	{name: "stripe_key", re: regexp.MustCompile(`\b(?:sk|pk|rk)_(?:live|test)_[A-Za-z0-9]{24,}\b`)},
	{name: "google_api_key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`)},
	{name: "vault_token", re: regexp.MustCompile(`\bhv[sbr]\.[A-Za-z0-9_-]{20,}`)},
	{name: "bearer_token", re: regexp.MustCompile(`(?i)(authorization["']?\s{0,8}:\s{0,8}["']?bearer\s{1,8})(` + genericValue + `)`), group: 2},
}

var generic = []rule{
	{name: "secret", re: regexp.MustCompile(`(?i)(\b[\w-]*(?:password|passwd|pwd|secret|token|api[_-]?key|apikey|access[_-]?key|private[_-]?key)["']?\s{0,8}[=:]\s{0,8}["']?)(` + genericValue + `)`), group: 2, needLetter: true},
	{name: "secret_flag", re: regexp.MustCompile(`(?i)(--(?:token|password|passwd|api-key|apikey|secret)[= ]["']?)(` + genericValue + `)`), group: 2, needLetter: true},
}

type Redactor struct {
	rules []rule
}

func New(withGeneric bool) *Redactor {
	r := &Redactor{rules: append([]rule(nil), specific...)}
	if withGeneric {
		r.rules = append(r.rules, generic...)
	}
	return r
}

// Clean strips private blocks and masks secrets.
func (r *Redactor) Clean(s string) string {
	if s == "" {
		return s
	}
	s = StripTags(s)
	for _, ru := range r.rules {
		s = ru.apply(s)
	}
	return s
}

func (ru rule) apply(s string) string {
	idx := ru.re.FindAllStringSubmatchIndex(s, -1)
	if idx == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range idx {
		start, end := m[2*ru.group], m[2*ru.group+1]
		if start < 0 {
			continue
		}
		val := s[start:end]
		if strings.HasPrefix(val, "[REDACTED") || (ru.needLetter && !hasLetter(val)) {
			continue
		}
		b.WriteString(s[last:start])
		b.WriteString("[REDACTED:" + ru.name + "]")
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

func hasLetter(s string) bool {
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			return true
		}
	}
	return false
}
