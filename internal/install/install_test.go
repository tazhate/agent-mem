package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setup(t *testing.T) Options {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{
  "model": "opus",
  "hooks": {
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "echo mine"}]}
    ]
  },
  "zeta": true
}
`), 0o644)
	os.MkdirAll(filepath.Join(home, ".codex"), 0o755)
	os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte("# my comment\nmodel = \"x\"\n"), 0o644)
	return Options{Home: home, Binary: "/opt/bin/amem", SkipMCP: false}
}

func TestClaudeInstallIdempotentAndReversible(t *testing.T) {
	o := setup(t)
	o.SkipMCP = true
	path := filepath.Join(o.Home, ".claude", "settings.json")
	orig, _ := os.ReadFile(path)
	if _, err := Run(o, "claude-code", false); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	s := string(after)
	for _, want := range []string{"echo mine", "/opt/bin/amem hook claude-code UserPromptSubmit", `"matcher": "Read|Edit|Write|MultiEdit|NotebookEdit"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in\n%s", want, s)
		}
	}
	if strings.Index(s, `"model"`) > strings.Index(s, `"zeta"`) {
		t.Fatal("key order not preserved")
	}
	ch, _ := Run(Options{Home: o.Home, Binary: o.Binary, SkipMCP: true, DryRun: true}, "claude-code", false)
	if ch[0].Before != ch[0].After {
		t.Fatalf("second install not idempotent:\n%s", Diff(ch[0]))
	}
	if !Installed(o)["claude-code"] {
		t.Fatal("Installed() does not see it")
	}
	if _, err := Run(o, "claude-code", true); err != nil {
		t.Fatal(err)
	}
	back, _ := os.ReadFile(path)
	if strings.Contains(string(back), "amem") || !strings.Contains(string(back), "echo mine") {
		t.Fatalf("uninstall:\n%s", back)
	}
	// Semantically the original file again (formatting normalized to two-space JSON).
	norm, _ := parseObject(string(orig))
	want, _ := render(norm)
	if string(back) != want {
		t.Fatalf("uninstall did not restore original:\n%s\nwant:\n%s", back, want)
	}
	baks, _ := filepath.Glob(path + ".bak-amem-*")
	if len(baks) == 0 {
		t.Fatal("no backup written")
	}
}

func TestCodexTomlBlock(t *testing.T) {
	o := setup(t)
	if _, err := Run(o, "codex", false); err != nil {
		t.Fatal(err)
	}
	toml, _ := os.ReadFile(filepath.Join(o.Home, ".codex", "config.toml"))
	if !strings.HasPrefix(string(toml), "# my comment\nmodel = \"x\"\n") || !strings.Contains(string(toml), "[mcp_servers.amem]") {
		t.Fatalf("toml:\n%s", toml)
	}
	hooks, _ := os.ReadFile(filepath.Join(o.Home, ".codex", "hooks.json"))
	if !strings.Contains(string(hooks), "hook codex SessionStart") {
		t.Fatalf("codex hooks:\n%s", hooks)
	}
	Run(o, "codex", false) // reinstall must not duplicate the block
	toml, _ = os.ReadFile(filepath.Join(o.Home, ".codex", "config.toml"))
	if strings.Count(string(toml), "[mcp_servers.amem]") != 1 {
		t.Fatalf("duplicated block:\n%s", toml)
	}
	Run(o, "codex", true)
	toml, _ = os.ReadFile(filepath.Join(o.Home, ".codex", "config.toml"))
	if strings.TrimSpace(string(toml)) != "# my comment\nmodel = \"x\"" {
		t.Fatalf("toml after uninstall:\n%q", toml)
	}
}

func TestOtherRuntimes(t *testing.T) {
	o := setup(t)
	for _, rt := range []string{"gemini", "cursor", "opencode"} {
		if _, err := Run(o, rt, false); err != nil {
			t.Fatalf("%s: %v", rt, err)
		}
	}
	g, _ := os.ReadFile(filepath.Join(o.Home, ".gemini", "settings.json"))
	if !strings.Contains(string(g), "hook gemini BeforeAgent") || !strings.Contains(string(g), `"timeout": 10000`) || !strings.Contains(string(g), "mcpServers") {
		t.Fatalf("gemini:\n%s", g)
	}
	c, _ := os.ReadFile(filepath.Join(o.Home, ".cursor", "hooks.json"))
	if !strings.Contains(string(c), `"version": 1`) || !strings.Contains(string(c), "hook cursor sessionStart") {
		t.Fatalf("cursor:\n%s", c)
	}
	p, _ := os.ReadFile(filepath.Join(o.Home, ".config", "opencode", "plugins", "amem.ts"))
	if !strings.Contains(string(p), `const BIN = "/opt/bin/amem"`) {
		t.Fatal("opencode plugin binary not substituted")
	}
	inst := Installed(o)
	for _, rt := range []string{"gemini", "cursor", "opencode"} {
		if !inst[rt] {
			t.Fatalf("%s not detected as installed: %v", rt, inst)
		}
	}
}

func TestKimiBlock(t *testing.T) {
	o := setup(t)
	os.MkdirAll(filepath.Join(o.Home, ".kimi-code"), 0o755)
	os.WriteFile(filepath.Join(o.Home, ".kimi-code", "config.toml"), []byte("[[hooks]]\nevent = \"SessionStart\"\ncommand = \"mine\"\n"), 0o644)
	if _, err := Run(o, "kimi", false); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(o.Home, ".kimi-code", "config.toml"))
	if !strings.Contains(string(b), `command = "mine"`) || strings.Count(string(b), "hook kimi") != 7 {
		t.Fatalf("kimi:\n%s", b)
	}
	Run(o, "kimi", false)
	b, _ = os.ReadFile(filepath.Join(o.Home, ".kimi-code", "config.toml"))
	if strings.Count(string(b), "hook kimi") != 7 {
		t.Fatal("reinstall duplicated kimi hooks")
	}
	Run(o, "kimi", true)
	b, _ = os.ReadFile(filepath.Join(o.Home, ".kimi-code", "config.toml"))
	if strings.Contains(string(b), "amem") {
		t.Fatalf("kimi uninstall:\n%s", b)
	}
}

func TestInstallerSafety(t *testing.T) {
	o := setup(t)
	o.SkipMCP = true
	// Symlinked settings (stow, chezmoi) stay a symlink; the target gets the hooks.
	dots := filepath.Join(o.Home, "dotfiles")
	os.MkdirAll(dots, 0o755)
	target := filepath.Join(dots, "settings.json")
	link := filepath.Join(o.Home, ".claude", "settings.json")
	os.Rename(link, target)
	os.Symlink(target, link)
	if _, err := Run(o, "claude-code", false); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced by a regular file")
	}
	if b, _ := os.ReadFile(target); !strings.Contains(string(b), "hook claude-code") {
		t.Fatal("target not updated")
	}
	// Uninstalling a runtime that was never set up creates nothing.
	if _, err := Run(o, "cursor", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(o.Home, ".cursor")); err == nil {
		t.Fatal("uninstall created ~/.cursor")
	}
	// A user's own MCP server named amem is not clobbered.
	os.MkdirAll(filepath.Join(o.Home, ".gemini"), 0o755)
	os.WriteFile(filepath.Join(o.Home, ".gemini", "settings.json"), []byte(`{"mcpServers":{"amem":{"httpUrl":"https://example.com/mcp"}}}`), 0o644)
	if _, err := Run(Options{Home: o.Home, Binary: o.Binary}, "gemini", false); err == nil {
		t.Fatal("foreign amem MCP entry overwritten")
	}
	// Paths with shell metacharacters are quoted.
	if q := shellQuote("/opt/a&b;x/amem"); q != "'/opt/a&b;x/amem'" {
		t.Fatalf("shellQuote: %s", q)
	}
	if tomlString("a\"b\\c\x01") != `"a\"b\\c\u0001"` {
		t.Fatalf("tomlString: %s", tomlString("a\"b\\c\x01"))
	}
}
