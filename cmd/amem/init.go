package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	"agent-mem/internal/config"
	"agent-mem/internal/install"
	"agent-mem/internal/provider"
)

func init() {
	register("init", "interactive setup: observer model, embedder, language, runtimes", cmdInit)
}

type initAnswers struct {
	observer, observerModel, observerURL, observerKey string
	embed, embedURL, embedModel, embedKey             string
	language, recallMode                              string
	runtimes                                          []string
}

func cmdInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	var a initAnswers
	var runtimes string
	fs.StringVar(&a.observer, "observer", "", "claude | codex | gemini (subscription CLI) | anthropic | openai (API key) | none")
	fs.StringVar(&a.observerModel, "observer-model", "", "model name for the observer")
	fs.StringVar(&a.observerURL, "observer-url", "", "base URL for openai-compatible or anthropic gateways")
	fs.StringVar(&a.observerKey, "observer-key", "", "API key reference: env:NAME, cmd:..., file:...")
	fs.StringVar(&a.embed, "embed", "", "none | ollama | openai")
	fs.StringVar(&a.embedURL, "embed-url", "", "embeddings base URL (default Ollama http://127.0.0.1:11434/v1)")
	fs.StringVar(&a.embedModel, "embed-model", "", "embedding model, e.g. bge-m3")
	fs.StringVar(&a.embedKey, "embed-key", "", "embeddings API key reference")
	fs.StringVar(&a.language, "language", "", "language of written memories, e.g. Russian")
	fs.StringVar(&a.recallMode, "recall", "", "on | shadow | off")
	fs.StringVar(&runtimes, "runtimes", "", "comma-separated runtimes to install, or 'detected', or 'none'")
	yes := fs.Bool("yes", false, "non-interactive: take flags and defaults, ask nothing")
	fs.Parse(args)

	cfg, err := config.Load()
	if err != nil {
		return fail("%v", err)
	}
	in := bufio.NewReader(os.Stdin)
	ask := func(q, def string, options ...string) string {
		if *yes {
			return def
		}
		hint := def
		if len(options) > 0 {
			hint = strings.Join(options, "/") + ", default " + def
		}
		fmt.Printf("%s [%s]: ", q, hint)
		line, _ := in.ReadString('\n')
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
		return def
	}

	// Observer: subscription CLI or API key, the user's choice.
	if a.observer == "" {
		def := "none"
		for _, c := range []string{"claude", "codex", "gemini"} {
			if _, err := exec.LookPath(c); err == nil {
				def = c
				break
			}
		}
		fmt.Println("Observer turns captured activity into memories. Subscription CLIs (claude, codex, gemini) use your")
		fmt.Println("existing login and share its limits; anthropic/openai use an API key (openai covers OpenRouter, Ollama, vLLM...).")
		a.observer = ask("Observer", def, "claude", "codex", "gemini", "anthropic", "openai", "none")
	}
	cfg.Observer.Providers = nil
	cfg.Observer.Enabled = a.observer != "none"
	switch a.observer {
	case "claude", "codex", "gemini":
		model := a.observerModel
		if model == "" {
			model = ask("Observer model", provider.PresetModels[a.observer])
		}
		cfg.Observer.Providers = []config.ProviderConfig{{Name: a.observer + "-cli", Kind: "exec", Model: model, Command: provider.Presets[a.observer]}}
	case "anthropic", "openai":
		p := config.ProviderConfig{Name: a.observer, Kind: a.observer, Model: a.observerModel, BaseURL: a.observerURL, Key: a.observerKey}
		if a.observer == "openai" && p.BaseURL == "" {
			p.BaseURL = ask("Base URL", "https://api.openai.com/v1")
		}
		if p.Model == "" {
			def := "claude-haiku-4-5"
			if a.observer == "openai" {
				def = "gpt-5-mini"
			}
			p.Model = ask("Model", def)
		}
		if p.Key == "" {
			def := "env:ANTHROPIC_API_KEY"
			if a.observer == "openai" {
				def = "env:OPENAI_API_KEY"
			}
			p.Key = ask("API key reference (env:NAME, cmd:vault kv get ..., file:path)", def)
		}
		cfg.Observer.Providers = []config.ProviderConfig{p}
	case "none":
	default:
		return fail("unknown observer %q", a.observer)
	}

	// Embedder: optional semantic search, the user's choice.
	if a.embed == "" {
		fmt.Println("Embeddings add search by meaning (helps across languages). Without them amem uses full-text search only.")
		a.embed = ask("Embedder", "none", "none", "ollama", "openai")
	}
	switch a.embed {
	case "none":
		cfg.Embed.Kind = "none"
	case "ollama", "openai":
		cfg.Embed.Kind = "openai"
		cfg.Embed.BaseURL = a.embedURL
		if cfg.Embed.BaseURL == "" {
			def := "http://127.0.0.1:11434/v1"
			if a.embed == "openai" {
				def = "https://api.openai.com/v1"
			}
			cfg.Embed.BaseURL = ask("Embeddings base URL", def)
		}
		cfg.Embed.Model = a.embedModel
		if cfg.Embed.Model == "" {
			def := "bge-m3"
			if a.embed == "openai" {
				def = "text-embedding-3-small"
			}
			cfg.Embed.Model = ask("Embedding model", def)
		}
		cfg.Embed.Key = a.embedKey
		if a.embed == "openai" && cfg.Embed.Key == "" {
			cfg.Embed.Key = ask("API key reference", "env:OPENAI_API_KEY")
		}
	default:
		return fail("unknown embedder %q", a.embed)
	}

	if a.language == "" {
		a.language = ask("Language for written memories", cfg.Observer.Language)
	}
	cfg.Observer.Language = a.language
	if a.recallMode == "" {
		a.recallMode = ask("Recall mode (shadow = compute but never inject, to compare first)", cfg.Recall.Mode, "on", "shadow", "off")
	}
	if !slices.Contains([]string{"on", "shadow", "off"}, a.recallMode) {
		return fail("recall mode must be on, shadow or off")
	}
	cfg.Recall.Mode = a.recallMode

	path := config.Path()
	if err := cfg.Save(path); err != nil {
		return fail("save config: %v", err)
	}
	fmt.Printf("wrote %s\n", path)

	o, err := installOptions(false, false)
	if err != nil {
		return fail("%v", err)
	}
	switch runtimes {
	case "":
		det := install.Detected(o)
		if len(det) == 0 {
			return 0
		}
		runtimes = ask("Install hooks into", strings.Join(det, ","))
		if *yes {
			runtimes = "none"
		}
	case "detected":
		runtimes = strings.Join(install.Detected(o), ",")
	}
	if runtimes == "none" || runtimes == "" {
		fmt.Println("no runtimes installed; run `amem install <runtime>` later")
		return 0
	}
	return applyInstall(o, strings.Split(runtimes, ","), false)
}
