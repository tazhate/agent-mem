// Package redact strips private blocks and secrets before anything reaches the database or an LLM.
package redact

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

// strippedTags are removed together with their content: user-marked private text and context
// that runtimes or memory tools injected (re-recording it would feed memory back into memory).
var strippedTags = []string{"private", "system-reminder", "amem-context", "claude-mem-context", "persisted-output", "system_instruction", "system-instruction"}

var tagPatterns = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(strippedTags))
	for _, t := range strippedTags {
		out = append(out, regexp.MustCompile(`(?is)<`+regexp.QuoteMeta(t)+`(?:\s[^>]*)?>.*?</`+regexp.QuoteMeta(t)+`\s*>`))
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
	return strings.Contains(strings.ToLower(s), "<private") && strings.TrimSpace(StripTags(s)) == ""
}

type rule struct {
	name string
	re   *regexp.Regexp
	// group is the submatch holding the secret; 0 means the whole match. RE2 has no lookbehind,
	// so keyed patterns capture the key as group 1 and the value as group 2.
	group int
	// needLetter rejects values without a letter (counts, ports, timestamps).
	needLetter bool
	// needDigit rejects values without a digit (CSS class names like sk-spinner-fading-circle).
	needDigit bool
	// hints are lowercase substrings one of which must occur for the rule to match at all;
	// checking them first skips the regexp on almost all text.
	hints []string
}

const genericValue = `[A-Za-z0-9_.\-+/=~@#$%^*!?]{8,}`

var specific = []rule{
	// A key without its END line (e.g. `head id_rsa`) is masked to the end of the text.
	{name: "private_key", re: regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY(?: BLOCK)?-----.*?(?:-----END [A-Z ]*PRIVATE KEY(?: BLOCK)?-----|\z)`), hints: []string{"private key"}},
	{name: "url_password", re: regexp.MustCompile(`(?i)(\b[a-z][a-z0-9+.-]{1,20}://[^/\s:@'"]{1,128}:)([^@\s/'"]{3,256})@`), group: 2, hints: []string{"://"}},
	{name: "aws_access_key", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), hints: []string{"akia", "asia"}},
	{name: "aws_secret_key", re: regexp.MustCompile(`(?i)((?:aws_secret_access_key|secretaccesskey)["']?\s{0,8}[=:]\s{0,8}["']?)([A-Za-z0-9/+=]{40})`), group: 2, hints: []string{"secretaccesskey", "secret_access_key"}},
	{name: "azure_key", re: regexp.MustCompile(`(?i)((?:AccountKey|SharedAccessKey)=)([A-Za-z0-9+/=]{20,})`), group: 2, hints: []string{"accountkey", "sharedaccesskey"}},
	{name: "github_token", re: regexp.MustCompile(`\bgh[oprsu]_[A-Za-z0-9]{36}\b|\bgithub_pat_[A-Za-z0-9_]{82}\b`), hints: []string{"ghp_", "gho_", "ghr_", "ghs_", "ghu_", "github_pat_"}},
	{name: "gitlab_token", re: regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`), hints: []string{"glpat-"}},
	{name: "npm_token", re: regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`), hints: []string{"npm_"}},
	{name: "huggingface_token", re: regexp.MustCompile(`\bhf_[A-Za-z0-9]{30,}\b`), hints: []string{"hf_"}},
	{name: "telegram_bot_token", re: regexp.MustCompile(`\b\d{6,12}:AA[A-Za-z0-9_-]{30,}`), hints: []string{":aa"}},
	{name: "anthropic_key", re: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`), hints: []string{"sk-ant-"}},
	{name: "openai_key", re: regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_-]{20,}`), needDigit: true, hints: []string{"sk-"}},
	{name: "slack_token", re: regexp.MustCompile(`\b(?:xox[abeprs]|xapp)-[A-Za-z0-9-]{10,}`), hints: []string{"xox", "xapp-"}},
	{name: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`), hints: []string{"eyj"}},
	{name: "stripe_key", re: regexp.MustCompile(`\b(?:sk|pk|rk)_(?:live|test)_[A-Za-z0-9]{24,}\b`), hints: []string{"_live_", "_test_"}},
	{name: "google_api_key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`), hints: []string{"aiza"}},
	{name: "vault_token", re: regexp.MustCompile(`\bhv[sbr]\.[A-Za-z0-9_-]{20,}`), hints: []string{"hvs.", "hvb.", "hvr."}},
	{name: "bearer_token", re: regexp.MustCompile(`(?i)(authorization["']?\s{0,8}:\s{0,8}["']?(?:bearer|basic|token)\s{1,8})(` + genericValue + `)`), group: 2, hints: []string{"authorization"}},
}

var generic = []rule{
	{name: "secret", re: regexp.MustCompile(`(?i)(\b[\w-]*(?:password|passwd|pwd|secret|token|api[_-]?key|apikey|access[_-]?key|private[_-]?key)["']?\s{0,8}[=:]\s{0,8}["']?)(` + genericValue + `)`), group: 2, needLetter: true, hints: []string{"password", "passwd", "pwd", "secret", "token", "key"}},
	{name: "secret_flag", re: regexp.MustCompile(`(?i)(--(?:token|password|passwd|api-key|apikey|secret)[= ]["']?)(` + genericValue + `)`), group: 2, needLetter: true, hints: []string{"--token", "--password", "--passwd", "--api-key", "--apikey", "--secret"}},
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
	lower := strings.ToLower(s)
	for _, ru := range r.rules {
		if out := ru.apply(s, lower); out != s {
			s, lower = out, strings.ToLower(out)
		}
	}
	return s
}

func (ru rule) apply(s, lower string) string {
	if len(ru.hints) > 0 {
		found := false
		for _, h := range ru.hints {
			if strings.Contains(lower, h) {
				found = true
				break
			}
		}
		if !found {
			return s
		}
	}
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
		if strings.HasPrefix(val, "[REDACTED") || (ru.needLetter && !hasLetter(val)) || (ru.needDigit && !hasDigit(val)) {
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

func hasDigit(s string) bool {
	for _, c := range s {
		if c >= '0' && c <= '9' {
			return true
		}
	}
	return false
}

// secretKey matches JSON field names whose string values are secrets.
var secretKey = regexp.MustCompile(`(?i)(password|passwd|pwd|secret|token|api[_-]?key|apikey|access[_-]?key|private[_-]?key|authorization|credential)`)

// CleanJSON applies fix to every string inside a JSON value. Strings are decoded first, so
// escaped forms like \u003cprivate\u003e are seen as <private>, and a rule that runs to the
// end of its text (a private key without END) stops at the end of that string instead of
// eating the rest of the document. Output keeps < and > literal. Invalid JSON is cleaned as
// plain text and re-encoded as a JSON string.
func CleanJSON(raw []byte, fix func(string) string) []byte { return cleanJSON(raw, fix, true) }

// NormalizeJSON re-encodes a JSON value the way CleanJSON does, without changing any value;
// compare against it to tell whether cleaning changed anything.
func NormalizeJSON(raw []byte) []byte {
	return cleanJSON(raw, func(s string) string { return s }, false)
}

func cleanJSON(raw []byte, fix func(string) string, keyed bool) []byte {
	if len(raw) == 0 {
		return raw
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		b, _ := json.Marshal(fix(string(raw)))
		return b
	}
	var walk func(any) any
	walk = func(x any) any {
		switch t := x.(type) {
		case string:
			return fix(t)
		case map[string]any:
			for k, val := range t {
				// Keyed rules need the field name: {"password": "x"} carries no "password=".
				if str, ok := val.(string); ok && keyed && secretKey.MatchString(k) && len(str) >= 8 && hasLetter(str) && !strings.HasPrefix(str, "[REDACTED") {
					t[k] = "[REDACTED:secret]"
					continue
				}
				t[k] = walk(val)
			}
		case []any:
			for i, val := range t {
				t[i] = walk(val)
			}
		}
		return x
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(walk(v)); err != nil {
		return raw
	}
	return bytes.TrimRight(out.Bytes(), "\n")
}
