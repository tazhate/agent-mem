// Package install registers amem hooks and the MCP server in each runtime's config. It merges
// into existing files without touching foreign entries, keeps a timestamped backup, and can
// show a diff instead of writing.
package install

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

//go:embed opencode.ts
var opencodePlugin string

type Options struct {
	Home   string // base for ~ paths; tests point it at a temp dir
	Binary string // absolute path of the amem binary
	DryRun bool
	// SkipMCP leaves MCP registration out (hooks only).
	SkipMCP bool
	Out     func(format string, args ...any)
}

// Change is either a file rewrite (Path/Before/After) or a command to run (Cmd).
type Change struct {
	Path   string
	Before string
	After  string
	Cmd    []string
	Note   string
}

type hookSpec struct {
	event   string
	matcher string
}

type runtime struct {
	name     string
	install  func(o Options, uninstall bool) ([]Change, error)
	detected func(o Options) bool
}

var runtimes = []runtime{
	{"claude-code", claudeCode, func(o Options) bool { return exists(filepath.Join(o.Home, ".claude")) }},
	{"codex", codexInstall, func(o Options) bool { return exists(filepath.Join(o.Home, ".codex")) }},
	{"gemini", geminiInstall, func(o Options) bool { return exists(filepath.Join(o.Home, ".gemini")) }},
	{"cursor", cursorInstall, func(o Options) bool { return exists(filepath.Join(o.Home, ".cursor")) }},
	{"opencode", opencodeInstall, func(o Options) bool { return exists(filepath.Join(o.Home, ".config", "opencode")) }},
	{"kimi", kimiInstall, func(o Options) bool { return exists(kimiHome(o)) }},
}

func Runtimes() []string {
	out := make([]string, len(runtimes))
	for i, r := range runtimes {
		out[i] = r.name
	}
	return out
}

// Detected lists runtimes whose config directory exists.
func Detected(o Options) []string {
	var out []string
	for _, r := range runtimes {
		if r.detected(o) {
			out = append(out, r.name)
		}
	}
	return out
}

// Run installs (or removes) amem for one runtime and applies or previews the changes.
func Run(o Options, name string, uninstall bool) ([]Change, error) {
	i := slices.IndexFunc(runtimes, func(r runtime) bool { return r.name == name })
	if i < 0 {
		return nil, fmt.Errorf("unknown runtime %q (%s)", name, strings.Join(Runtimes(), ", "))
	}
	if o.Out == nil {
		o.Out = func(string, ...any) {}
	}
	changes, err := runtimes[i].install(o, uninstall)
	if err != nil {
		return nil, err
	}
	if uninstall {
		// Removing amem from a runtime that never had it must not create its config files.
		kept := changes[:0]
		for _, c := range changes {
			if len(c.Cmd) > 0 || c.Before != "" {
				kept = append(kept, c)
			}
		}
		changes = kept
	}
	if o.DryRun {
		return changes, nil
	}
	stamp := time.Now().Format("20060102-150405")
	for _, c := range changes {
		if len(c.Cmd) > 0 {
			out, err := exec.Command(c.Cmd[0], c.Cmd[1:]...).CombinedOutput()
			if err != nil && !strings.Contains(string(out), "already exists") && !strings.Contains(string(out), "not found") {
				o.Out("warning: %s: %v: %s\n", strings.Join(c.Cmd, " "), err, strings.TrimSpace(string(out)))
			}
			continue
		}
		if c.Before == c.After {
			continue
		}
		// The runtime or the user may have edited the file since it was read: do not silently
		// overwrite that.
		now, err := readFile(c.Path)
		if err != nil {
			return nil, err
		}
		if now != c.Before {
			return nil, fmt.Errorf("%s changed while installing; run the command again", c.Path)
		}
		if err := writeFile(c.Path, c.Before, c.After, stamp); err != nil {
			return nil, err
		}
	}
	return changes, nil
}

// Installed reports which runtimes currently reference the amem binary in their hook config.
func Installed(o Options) map[string]bool {
	out := map[string]bool{}
	for path, name := range map[string]string{
		filepath.Join(o.Home, ".claude", "settings.json"):                  "claude-code",
		filepath.Join(o.Home, ".codex", "hooks.json"):                      "codex",
		filepath.Join(o.Home, ".gemini", "settings.json"):                  "gemini",
		filepath.Join(o.Home, ".cursor", "hooks.json"):                     "cursor",
		filepath.Join(o.Home, ".config", "opencode", "plugins", "amem.ts"): "opencode",
		filepath.Join(kimiHome(o), "config.toml"):                          "kimi",
	} {
		b, err := os.ReadFile(path)
		out[name] = err == nil && strings.Contains(string(b), "hook "+name)
	}
	return out
}

func writeFile(path, before, after, stamp string) error {
	// Write through symlinks (stow, chezmoi): replace the target, not the link.
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	mode := fs.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
		if err := os.WriteFile(path+".bak-amem-"+stamp, []byte(before), mode); err != nil {
			return fmt.Errorf("backup %s: %w", path, err)
		}
	}
	if after == "" {
		return os.Remove(path)
	}
	tmp := path + ".amem-tmp"
	if err := os.WriteFile(tmp, []byte(after), mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return string(b), err
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// --- JSON hook files (Claude Code, Codex, Gemini CLI) ---------------------------

func command(o Options, runtime, event string) string {
	return shellQuote(o.Binary) + " hook " + runtime + " " + event
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./+:@%-]+$`)

func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// isOurs recognizes amem hook commands for a runtime regardless of where the binary lives.
func isOurs(cmd, runtime string) bool {
	return strings.Contains(cmd, " hook "+runtime) && strings.Contains(cmd, "amem")
}

// mergeHooks edits a {"hooks": {Event: [{matcher, hooks: [{type, command, timeout}]}]}} document.
func mergeHooks(raw, runtime string, specs []hookSpec, o Options, timeout int, uninstall bool) (string, error) {
	doc, err := parseObject(raw)
	if err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	hooks, _ := doc.get("hooks").(*object)
	if hooks == nil {
		hooks = newObject()
	}
	// Remove previous amem entries first: install is idempotent and picks up a moved binary.
	for _, ev := range append([]string(nil), hooks.keys...) {
		groups, _ := hooks.get(ev).([]any)
		var kept []any
		for _, g := range groups {
			gm, _ := g.(*object)
			if gm == nil {
				kept = append(kept, g)
				continue
			}
			inner, _ := gm.get("hooks").([]any)
			var keepInner []any
			for _, h := range inner {
				if hm, _ := h.(*object); hm != nil {
					if c, _ := hm.get("command").(string); isOurs(c, runtime) {
						continue
					}
				}
				keepInner = append(keepInner, h)
			}
			if len(keepInner) == 0 && len(inner) > 0 {
				continue
			}
			gm.set("hooks", keepInner)
			kept = append(kept, g)
		}
		if len(kept) == 0 {
			hooks.del(ev)
		} else {
			hooks.set(ev, kept)
		}
	}
	if !uninstall {
		for _, s := range specs {
			entry := newObject()
			entry.set("type", "command")
			entry.set("command", command(o, runtime, s.event))
			entry.set("timeout", timeout)
			group := newObject()
			if s.matcher != "" {
				group.set("matcher", s.matcher)
			}
			group.set("hooks", []any{entry})
			list, _ := hooks.get(s.event).([]any)
			hooks.set(s.event, append(list, group))
		}
	}
	if len(hooks.keys) > 0 {
		doc.set("hooks", hooks)
	} else {
		doc.del("hooks")
	}
	return render(doc)
}

// mergeMCP sets or removes <key>.amem in a JSON settings document.
func mergeMCP(raw string, key string, entry map[string]any, uninstall bool) (string, error) {
	doc, err := parseObject(raw)
	if err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	servers, _ := doc.get(key).(*object)
	if servers == nil {
		servers = newObject()
	}
	if cur := servers.get("amem"); cur != nil && !isOurEntry(cur) {
		if uninstall {
			return render(doc) // someone else's "amem" server: leave it alone
		}
		return "", fmt.Errorf("an MCP server named \"amem\" exists that amem did not install; rename or remove it first")
	}
	if uninstall {
		servers.del("amem")
	} else {
		servers.set("amem", entry)
	}
	if len(servers.keys) == 0 {
		doc.del(key)
	} else {
		doc.set(key, servers)
	}
	return render(doc)
}

func jsonFile(path string, edit func(string) (string, error)) (Change, error) {
	before, err := readFile(path)
	if err != nil {
		return Change{}, err
	}
	after, err := edit(before)
	if err != nil {
		return Change{}, fmt.Errorf("%s: %w", path, err)
	}
	return Change{Path: path, Before: before, After: after}, nil
}

// isOurEntry recognizes MCP entries written by amem install: an amem binary run with "mcp".
func isOurEntry(v any) bool {
	o, _ := v.(*object)
	if o == nil {
		return false
	}
	words := []string{}
	switch c := o.get("command").(type) {
	case string:
		words = append(words, c)
	case []any:
		for _, w := range c {
			if s, ok := w.(string); ok {
				words = append(words, s)
			}
		}
	}
	if args, ok := o.get("args").([]any); ok {
		for _, w := range args {
			if s, ok := w.(string); ok {
				words = append(words, s)
			}
		}
	}
	// Release binaries are named amem-linux-amd64 and the like.
	return len(words) >= 2 && strings.HasPrefix(filepath.Base(words[0]), "amem") && words[len(words)-1] == "mcp"
}

// --- runtimes -------------------------------------------------------------------

func claudeCode(o Options, uninstall bool) ([]Change, error) {
	specs := []hookSpec{
		{"SessionStart", "startup|resume|clear|compact"},
		{"UserPromptSubmit", ""},
		{"PreToolUse", "Read|Edit|Write|MultiEdit|NotebookEdit"},
		{"PostToolUse", "*"},
		{"PostToolUseFailure", "*"},
		{"Stop", ""},
		{"SubagentStart", ""},
		{"SubagentStop", ""},
		{"SessionEnd", ""},
	}
	c, err := jsonFile(filepath.Join(o.Home, ".claude", "settings.json"), func(raw string) (string, error) {
		return mergeHooks(raw, "claude-code", specs, o, 10, uninstall)
	})
	if err != nil {
		return nil, err
	}
	changes := []Change{c}
	if !o.SkipMCP {
		// ~/.claude.json is Claude Code's live state file; let the CLI edit it.
		// ~/.claude.json is only read here, to make sure an existing "amem" server is ours.
		if raw, err := readFile(filepath.Join(o.Home, ".claude.json")); err == nil && raw != "" {
			if doc, err := parseObject(raw); err == nil {
				if servers, _ := doc.get("mcpServers").(*object); servers != nil {
					if cur := servers.get("amem"); cur != nil && !isOurEntry(cur) {
						if uninstall {
							return changes, nil // someone else's server: leave it alone
						}
						return nil, fmt.Errorf(`a user-scope MCP server named "amem" exists that amem did not install; rename or remove it first`)
					}
				}
			}
		}
		// Remove first: `add` refuses an existing name, which would keep a stale binary path.
		changes = append(changes, Change{Cmd: []string{"claude", "mcp", "remove", "--scope", "user", "amem"}, Note: "unregister MCP server"})
		if !uninstall {
			changes = append(changes, Change{Cmd: []string{"claude", "mcp", "add", "--scope", "user", "amem", "--", o.Binary, "mcp"}, Note: "register MCP server"})
		}
	}
	return changes, nil
}

func codexInstall(o Options, uninstall bool) ([]Change, error) {
	specs := []hookSpec{
		{"SessionStart", "startup|resume|clear|compact"},
		{"UserPromptSubmit", ""},
		{"PreToolUse", ""},
		{"PostToolUse", ""},
		{"Stop", ""},
		{"SubagentStart", ""},
		{"SubagentStop", ""},
		{"PostCompact", ""},
		{"SessionEnd", ""},
	}
	c, err := jsonFile(filepath.Join(o.Home, ".codex", "hooks.json"), func(raw string) (string, error) {
		return mergeHooks(raw, "codex", specs, o, 10, uninstall)
	})
	if err != nil {
		return nil, err
	}
	changes := []Change{c}
	if !o.SkipMCP {
		m, err := tomlBlock(filepath.Join(o.Home, ".codex", "config.toml"),
			"[mcp_servers.amem]\ncommand = "+tomlString(o.Binary)+"\nargs = [\"mcp\"]\n", uninstall)
		if err != nil {
			return nil, err
		}
		changes = append(changes, m)
	}
	return changes, nil
}

func geminiInstall(o Options, uninstall bool) ([]Change, error) {
	specs := []hookSpec{
		{"SessionStart", ""}, {"BeforeAgent", ""}, {"BeforeTool", ""}, {"AfterTool", ""},
		{"AfterAgent", ""}, {"PreCompress", ""}, {"SessionEnd", ""},
	}
	path := filepath.Join(o.Home, ".gemini", "settings.json")
	c, err := jsonFile(path, func(raw string) (string, error) {
		out, err := mergeHooks(raw, "gemini", specs, o, 10000, uninstall) // Gemini timeouts are in ms
		if err != nil || o.SkipMCP {
			return out, err
		}
		return mergeMCP(out, "mcpServers", map[string]any{"command": o.Binary, "args": []string{"mcp"}}, uninstall)
	})
	if err != nil {
		return nil, err
	}
	return []Change{c}, nil
}

func cursorInstall(o Options, uninstall bool) ([]Change, error) {
	events := []string{"sessionStart", "beforeSubmitPrompt", "preToolUse", "postToolUse", "postToolUseFailure",
		"stop", "subagentStart", "subagentStop", "sessionEnd"}
	c, err := jsonFile(filepath.Join(o.Home, ".cursor", "hooks.json"), func(raw string) (string, error) {
		doc, err := parseObject(raw)
		if err != nil {
			return "", err
		}
		if doc.get("version") == nil {
			doc.set("version", 1)
		}
		hooks, _ := doc.get("hooks").(*object)
		if hooks == nil {
			hooks = newObject()
		}
		for _, ev := range append([]string(nil), hooks.keys...) {
			list, _ := hooks.get(ev).([]any)
			var kept []any
			for _, h := range list {
				if hm, _ := h.(*object); hm != nil {
					if cmd, _ := hm.get("command").(string); isOurs(cmd, "cursor") {
						continue
					}
				}
				kept = append(kept, h)
			}
			if len(kept) == 0 {
				hooks.del(ev)
			} else {
				hooks.set(ev, kept)
			}
		}
		if !uninstall {
			for _, ev := range events {
				entry := newObject()
				entry.set("type", "command")
				entry.set("command", command(o, "cursor", ev))
				entry.set("timeout", 10)
				list, _ := hooks.get(ev).([]any)
				hooks.set(ev, append(list, entry))
			}
		}
		doc.set("hooks", hooks)
		return render(doc)
	})
	if err != nil {
		return nil, err
	}
	changes := []Change{c}
	if !o.SkipMCP {
		m, err := jsonFile(filepath.Join(o.Home, ".cursor", "mcp.json"), func(raw string) (string, error) {
			return mergeMCP(raw, "mcpServers", map[string]any{"command": o.Binary, "args": []string{"mcp"}}, uninstall)
		})
		if err != nil {
			return nil, err
		}
		changes = append(changes, m)
	}
	return changes, nil
}

func opencodeInstall(o Options, uninstall bool) ([]Change, error) {
	dir := filepath.Join(o.Home, ".config", "opencode")
	plugin := filepath.Join(dir, "plugins", "amem.ts")
	before, err := readFile(plugin)
	if err != nil {
		return nil, err
	}
	after := ""
	if !uninstall {
		bin, _ := json.Marshal(o.Binary)
		after = strings.ReplaceAll(opencodePlugin, "__AMEM_BIN__", string(bin))
	}
	changes := []Change{{Path: plugin, Before: before, After: after}}
	if !o.SkipMCP {
		m, err := jsonFile(filepath.Join(dir, "opencode.json"), func(raw string) (string, error) {
			return mergeMCP(raw, "mcp", map[string]any{"type": "local", "command": []string{o.Binary, "mcp"}, "enabled": true}, uninstall)
		})
		if err != nil {
			return nil, err
		}
		changes = append(changes, m)
	}
	return changes, nil
}

const (
	tomlBegin = "# >>> amem (managed by `amem install`)"
	tomlEnd   = "# <<< amem"
)

// tomlBlock adds or removes a marked block of TOML text, leaving the rest of the file (and its
// comments) byte-for-byte intact.
func tomlBlock(path, block string, uninstall bool) (Change, error) {
	return markedBlock(path, tomlBegin, tomlEnd, "[mcp_servers.amem]", block, uninstall)
}

// markedBlock adds or removes text between begin/end marker lines; conflict is a string whose
// presence outside the block means the user configured it by hand.
func markedBlock(path, tomlBegin, tomlEnd, conflict, block string, uninstall bool) (Change, error) {
	before, err := readFile(path)
	if err != nil {
		return Change{}, err
	}
	after := before
	if i := strings.Index(after, tomlBegin); i >= 0 {
		if j := strings.Index(after[i:], tomlEnd); j >= 0 {
			end := i + j + len(tomlEnd)
			if end < len(after) && after[end] == '\n' {
				end++
			}
			// Drop the blank separator line in front of the block too, so install, reinstall and
			// uninstall do not pile up empty lines.
			after = strings.TrimRight(after[:i], "\n") + "\n" + after[end:]
			if strings.TrimSpace(after) == "" {
				after = ""
			}
		}
	}
	if !uninstall {
		if conflict != "" && strings.Contains(after, conflict) {
			return Change{}, fmt.Errorf("%s already has an unmanaged %s entry", path, conflict)
		}
		if after != "" && !strings.HasSuffix(after, "\n") {
			after += "\n"
		}
		after += "\n" + tomlBegin + "\n" + block + tomlEnd + "\n"
	}
	return Change{Path: path, Before: before, After: after}, nil
}

// Binary returns the absolute, symlink-resolved path of the running executable.
func Binary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	// Prefer a stable copy on PATH when the running binary is the same file.
	if p, err := exec.LookPath("amem"); err == nil {
		if a, err := filepath.EvalSymlinks(p); err == nil && sameFile(a, exe) {
			return p, nil
		}
	}
	return exe, nil
}

func sameFile(a, b string) bool {
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(sa, sb)
}

// Diff renders a compact line diff of a change.
func Diff(c Change) string {
	if len(c.Cmd) > 0 {
		return "$ " + strings.Join(c.Cmd, " ") + "\n"
	}
	a, b := splitLines(c.Before), splitLines(c.After)
	// LCS table; config files are small.
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", c.Path, c.Path)
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i] == b[j]:
			i++
			j++
		case j < m && (i >= n || lcs[i][j+1] >= lcs[i+1][j]):
			out.WriteString("+ " + b[j] + "\n")
			j++
		default:
			out.WriteString("- " + a[i] + "\n")
			i++
		}
	}
	return out.String()
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// kimiHome honors $KIMI_CODE_HOME like Kimi Code itself.
func kimiHome(o Options) string {
	if h := os.Getenv("KIMI_CODE_HOME"); h != "" && o.Home == mustHome() {
		return h
	}
	return filepath.Join(o.Home, ".kimi-code")
}

func mustHome() string { h, _ := os.UserHomeDir(); return h }

func kimiInstall(o Options, uninstall bool) ([]Change, error) {
	rules := []struct{ event, matcher string }{
		{"SessionStart", "startup|resume"},
		{"UserPromptSubmit", ""},
		{"PreToolUse", ""},
		{"PostToolUse", ""},
		{"PostToolUseFailure", ""},
		{"Stop", ""},
		{"PreCompact", ""},
	}
	var b strings.Builder
	for i, r := range rules {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("[[hooks]]\n")
		b.WriteString("event = " + tomlString(r.event) + "\n")
		if r.matcher != "" {
			b.WriteString("matcher = " + tomlString(r.matcher) + "\n")
		}
		b.WriteString("command = " + tomlString(command(o, "kimi", r.event)) + "\ntimeout = 10\n")
	}
	home := kimiHome(o)
	c, err := markedBlock(filepath.Join(home, "config.toml"), "# >>> amem kimi hooks (managed by `amem install`)", "# <<< amem kimi hooks", "", b.String(), uninstall)
	if err != nil {
		return nil, err
	}
	changes := []Change{c}
	if !o.SkipMCP {
		m, err := jsonFile(filepath.Join(home, "mcp.json"), func(raw string) (string, error) {
			return mergeMCP(raw, "mcpServers", map[string]any{"command": o.Binary, "args": []string{"mcp"}}, uninstall)
		})
		if err != nil {
			return nil, err
		}
		changes = append(changes, m)
	}
	return changes, nil
}

// tomlString renders a TOML basic string. Go's %q is not TOML: it emits \x.. escapes and
// accepts invalid UTF-8.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f || r == utf8.RuneError:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
