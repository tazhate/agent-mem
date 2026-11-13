// Package config loads amem settings from TOML and resolves secret references.
package config

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Duration is a time.Duration that decodes from TOML strings like "30m".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

type Config struct {
	DataDir  string         `toml:"data_dir,omitempty"`
	Daemon   DaemonConfig   `toml:"daemon"`
	Capture  CaptureConfig  `toml:"capture"`
	Recall   RecallConfig   `toml:"recall"`
	Observer ObserverConfig `toml:"observer"`
	Embed    EmbedConfig    `toml:"embed"`

	path string
	// Warnings lists invalid settings that Load replaced with safe values.
	Warnings []string `toml:"-"`
}

type DaemonConfig struct {
	IdleExit Duration `toml:"idle_exit"`
	// HTTPAddr enables streamable-HTTP MCP on this address, e.g. "127.0.0.1:37800".
	HTTPAddr string `toml:"http_addr"`
}

type CaptureConfig struct {
	SkipTools     []string `toml:"skip_tools"`
	MaxFieldChars int      `toml:"max_field_chars"`
	Redact        bool     `toml:"redact"`
	RedactGeneric bool     `toml:"redact_generic"`
	// RawRetention is how long raw events are kept after the observer processed them.
	RawRetention Duration `toml:"raw_retention"`
}

type RecallConfig struct {
	// Mode is "on", "shadow" (compute and log, never inject) or "off".
	Mode                string  `toml:"mode"`
	SessionBudgetTokens int     `toml:"session_budget_tokens"`
	StartTokens         int     `toml:"start_tokens"`
	CompactTokens       int     `toml:"compact_tokens"`
	PromptLimit         int     `toml:"prompt_limit"`
	FileLimit           int     `toml:"file_limit"`
	ErrorLimit          int     `toml:"error_limit"`
	MinPromptChars      int     `toml:"min_prompt_chars"`
	MinScore            float64 `toml:"min_score"`
	GlobalPreferences   bool    `toml:"global_preferences"`
	// VectorFloor is the cosine similarity that maps to zero relevance; it depends on the
	// embedding model (unrelated texts sit around 0.2-0.4 for paraphrase-multilingual).
	VectorFloor float64 `toml:"vector_floor"`
	// FatigueAfter stops automatic injection of a memory shown this many times without ever
	// being used; explicit search still finds it. 0 disables.
	FatigueAfter int `toml:"fatigue_after"`
	// ImportedWeight scales the score of memories imported from another system (amem import)
	// during automatic injection: 1 treats them like native ones, 0 never injects them.
	// Explicit search is not affected.
	ImportedWeight float64 `toml:"imported_weight"`
	// StartSummary shows the user a short summary of the project's memory at session start
	// (runtimes with a user-visible hook message: Claude Code, Codex, Gemini CLI).
	StartSummary bool `toml:"start_summary"`
	// StartSummaryItems is how many recent memories the summary lists.
	StartSummaryItems int `toml:"start_summary_items"`
	// StartSummaryColor styles the summary with ANSI colors where the runtime renders them
	// (Claude Code); other runtimes get plain text. NO_COLOR in the hook environment also
	// turns it off.
	StartSummaryColor bool `toml:"start_summary_color"`
}

type ObserverConfig struct {
	Enabled     bool             `toml:"enabled"`
	Language    string           `toml:"language"`
	BatchEvents int              `toml:"batch_events"`
	BatchChars  int              `toml:"batch_chars"`
	IdleFlush   Duration         `toml:"idle_flush"`
	Timeout     Duration         `toml:"timeout"`
	Providers   []ProviderConfig `toml:"providers"`
}

// ProviderConfig describes one LLM backend. Kind is "anthropic", "openai" or "exec".
type ProviderConfig struct {
	Name    string   `toml:"name"`
	Kind    string   `toml:"kind"`
	BaseURL string   `toml:"base_url"`
	Key     string   `toml:"key"`
	Model   string   `toml:"model"`
	Command []string `toml:"command"`
	// MaxTokens caps the response for HTTP providers.
	MaxTokens int `toml:"max_tokens"`
	// EnvUnset removes variables from an exec provider's environment. Unset for a `claude`
	// command, it defaults to the variables that would move the CLI off the user's
	// subscription login (API key, base URL, model overrides).
	EnvUnset []string `toml:"env_unset,omitempty"`
}

type EmbedConfig struct {
	// Kind is "none", "static" (built-in model2vec model, no service; Model names a directory
	// under <data_dir>/models or an absolute path) or "openai" (any OpenAI-compatible
	// /v1/embeddings: Ollama, LM Studio, OpenAI...).
	Kind      string   `toml:"kind"`
	BaseURL   string   `toml:"base_url"`
	Key       string   `toml:"key"`
	Model     string   `toml:"model"`
	BatchSize int      `toml:"batch_size"`
	IdleGap   Duration `toml:"idle_gap"`
	// MaxChars clips text before embedding; most small models see only ~128-512 tokens anyway.
	MaxChars int `toml:"max_chars"`
}

func Default() *Config {
	return &Config{
		Daemon: DaemonConfig{IdleExit: Duration{30 * time.Minute}},
		Capture: CaptureConfig{
			SkipTools:     []string{"TodoWrite", "AskUserQuestion", "Skill", "SlashCommand", "ListMcpResourcesTool", "ToolSearch"},
			MaxFieldChars: 16000,
			Redact:        true,
			RedactGeneric: true,
			RawRetention:  Duration{30 * 24 * time.Hour},
		},
		Recall: RecallConfig{
			Mode:                "on",
			SessionBudgetTokens: 6000,
			StartTokens:         600,
			CompactTokens:       800,
			PromptLimit:         5,
			FileLimit:           3,
			ErrorLimit:          2,
			MinPromptChars:      15,
			MinScore:            0.25,
			GlobalPreferences:   true,
			VectorFloor:         0.40,
			FatigueAfter:        10,
			ImportedWeight:      1,
			StartSummary:        true,
			StartSummaryItems:   5,
			StartSummaryColor:   true,
		},
		Observer: ObserverConfig{
			Enabled:     true,
			Language:    "English",
			BatchEvents: 40,
			BatchChars:  120000,
			IdleFlush:   Duration{60 * time.Second},
			Timeout:     Duration{3 * time.Minute},
		},
		Embed: EmbedConfig{Kind: "none", BatchSize: 32, IdleGap: Duration{5 * time.Second}, MaxChars: 600},
	}
}

// Home returns the data directory: $AMEM_HOME, else $XDG_DATA_HOME/amem, else ~/.local/share/amem.
func Home() string {
	if h := os.Getenv("AMEM_HOME"); h != "" {
		return h
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "amem")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "amem")
}

// Path returns the config file path: $AMEM_CONFIG, else $AMEM_HOME/config.toml when AMEM_HOME
// is set, else $XDG_CONFIG_HOME/amem/config.toml.
func Path() string {
	if p := os.Getenv("AMEM_CONFIG"); p != "" {
		return p
	}
	if h := os.Getenv("AMEM_HOME"); h != "" {
		return filepath.Join(h, "config.toml")
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "amem", "config.toml")
}

// Load reads the config file; a missing file yields defaults.
func Load() (*Config, error) {
	c := Default()
	c.path = Path()
	if _, err := toml.DecodeFile(c.path, c); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("config %s: %w", c.path, err)
	}
	c.DataDir = expand(cmp.Or(c.DataDir, Home()))
	c.validate()
	return c, nil
}

// validate replaces invalid values with safe ones and records why.
func (c *Config) validate() {
	warn := func(format string, a ...any) { c.Warnings = append(c.Warnings, fmt.Sprintf(format, a...)) }
	d := Default()
	if !filepath.IsAbs(c.DataDir) {
		warn("data_dir %q is not absolute; using %s", c.DataDir, Home())
		c.DataDir = Home()
	}
	switch c.Recall.Mode {
	case "on", "shadow", "off":
	default:
		// Unknown means the user wanted something other than "on"; do not keep injecting.
		warn("recall.mode %q is not on/shadow/off; recall is off", c.Recall.Mode)
		c.Recall.Mode = "off"
	}
	if f := c.Recall.VectorFloor; f < 0 || f >= 0.95 {
		warn("recall.vector_floor %v outside [0, 0.95); using %v", f, d.Recall.VectorFloor)
		c.Recall.VectorFloor = d.Recall.VectorFloor
	}
	if w := c.Recall.ImportedWeight; w < 0 || w > 1 {
		warn("recall.imported_weight %v outside [0, 1]; using %v", w, d.Recall.ImportedWeight)
		c.Recall.ImportedWeight = d.Recall.ImportedWeight
	}
	if c.Embed.MaxChars <= 0 {
		if c.Embed.MaxChars < 0 {
			warn("embed.max_chars %d must be positive; using %d", c.Embed.MaxChars, d.Embed.MaxChars)
		}
		c.Embed.MaxChars = d.Embed.MaxChars
	}
	if c.Embed.BatchSize <= 0 {
		c.Embed.BatchSize = d.Embed.BatchSize
	}
	if c.Capture.MaxFieldChars <= 0 {
		c.Capture.MaxFieldChars = d.Capture.MaxFieldChars
	}
	for _, v := range []*int{&c.Recall.PromptLimit, &c.Recall.FileLimit, &c.Recall.ErrorLimit, &c.Recall.StartTokens, &c.Recall.CompactTokens, &c.Recall.SessionBudgetTokens} {
		if *v < 0 {
			*v = 0
		}
	}
	if c.Observer.BatchEvents <= 0 {
		c.Observer.BatchEvents = d.Observer.BatchEvents
	}
	if c.Observer.BatchChars <= 0 {
		c.Observer.BatchChars = d.Observer.BatchChars
	}
	if c.Observer.Timeout.Duration <= 0 {
		c.Observer.Timeout = d.Observer.Timeout
	}
	if c.Observer.IdleFlush.Duration <= 0 {
		c.Observer.IdleFlush = d.Observer.IdleFlush
	}
	if c.Embed.Kind == "ollama" { // what users call it; it speaks the OpenAI-compatible API
		c.Embed.Kind = "openai"
	}
	switch c.Embed.Kind {
	case "", "none", "openai", "static":
	default:
		warn("embed.kind %q is not none/static/openai; embeddings are off", c.Embed.Kind)
		c.Embed.Kind = "none"
	}
}

// Fallback returns defaults for when the config file cannot be parsed, so hooks and the
// daemon agree on paths and keep working; the error is reported by `amem doctor`.
func Fallback(err error) *Config {
	c := Default()
	c.DataDir = Home()
	c.path = Path()
	c.Warnings = []string{"config not loaded, running on defaults: " + err.Error()}
	return c
}

func (c *Config) File() string { return c.path }

// Save writes the config with 0600 permissions. The default data directory is left out, so
// AMEM_HOME and XDG_DATA_HOME keep working.
func (c *Config) Save(path string) error {
	out := *c
	if out.DataDir == Home() {
		out.DataDir = ""
	}
	c = &out
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := toml.NewEncoder(f).Encode(c); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (c *Config) DBPath() string     { return filepath.Join(c.DataDir, "amem.db") }
func (c *Config) SocketPath() string { return filepath.Join(c.DataDir, "amemd.sock") }
func (c *Config) SpoolPath() string  { return filepath.Join(c.DataDir, "spool.jsonl") }
func (c *Config) LockPath() string   { return filepath.Join(c.DataDir, "amemd.lock") }
func (c *Config) LogPath() string    { return filepath.Join(c.DataDir, "amemd.log") }
func (c *Config) ModelsDir() string  { return filepath.Join(c.DataDir, "models") }

// Resolve expands a secret reference: "env:NAME", "cmd:shell command", "file:path" or a literal.
func Resolve(ctx context.Context, ref string) (string, error) {
	switch {
	case ref == "":
		return "", nil
	case strings.HasPrefix(ref, "env:"):
		name := strings.TrimPrefix(ref, "env:")
		v := os.Getenv(name)
		if v == "" {
			return "", fmt.Errorf("env %s is empty", name)
		}
		return v, nil
	case strings.HasPrefix(ref, "cmd:"):
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", strings.TrimPrefix(ref, "cmd:"))
		// A helper that leaves a child holding stdout must not hang the caller past the timeout.
		// No own process group: helpers may prompt on the terminal (pass, op).
		cmd.WaitDelay = 2 * time.Second
		out, err := cmd.Output()
		if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
			err = nil
		}
		if err != nil {
			return "", fmt.Errorf("secret command: %w", err)
		}
		return strings.TrimSpace(string(out)), nil
	case strings.HasPrefix(ref, "file:"):
		b, err := os.ReadFile(expand(strings.TrimPrefix(ref, "file:")))
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	default:
		return ref, nil
	}
}

func expand(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, rest)
	}
	return p
}
