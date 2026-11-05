// Package provider talks to LLMs for the observer. Three backends cover the field: Anthropic
// Messages, OpenAI-compatible chat completions (OpenAI, OpenRouter, Ollama, vLLM, LM Studio,
// DeepSeek, Gemini's OpenAI endpoint...) and exec, which runs a CLI on a subscription
// (claude -p, codex exec, gemini -p).
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agent-mem/internal/config"
)

type Request struct {
	System     string
	User       string
	Schema     map[string]any // JSON schema of the expected object
	SchemaName string
	MaxTokens  int
}

type Provider interface {
	Name() string
	// Complete returns the JSON object produced by the model.
	Complete(ctx context.Context, req Request) (json.RawMessage, error)
}

// Error carries whether trying the next provider (or later) makes sense.
type Error struct {
	Provider  string
	Retryable bool
	Err       error
}

func (e *Error) Error() string { return e.Provider + ": " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

func retryable(name string, err error) error {
	return &Error{Provider: name, Retryable: true, Err: err}
}
func permanent(name string, err error) error { return &Error{Provider: name, Err: err} }

// New builds one provider from config. Secrets are resolved here, once.
func New(ctx context.Context, c config.ProviderConfig) (Provider, error) {
	key, err := config.Resolve(ctx, c.Key)
	if err != nil {
		return nil, fmt.Errorf("provider %s key: %w", c.Name, err)
	}
	name := c.Name
	if name == "" {
		name = c.Kind
	}
	switch c.Kind {
	case "anthropic":
		return &anthropic{name: name, base: strings.TrimRight(c.BaseURL, "/"), key: key, model: c.Model, maxTokens: c.MaxTokens}, nil
	case "openai":
		return &openai{name: name, base: strings.TrimRight(c.BaseURL, "/"), key: key, model: c.Model, maxTokens: c.MaxTokens}, nil
	case "exec":
		if len(c.Command) == 0 {
			return nil, fmt.Errorf("provider %s: exec needs command", name)
		}
		return &execProvider{name: name, command: c.Command, model: c.Model}, nil
	}
	return nil, fmt.Errorf("provider %s: unknown kind %q (anthropic, openai, exec)", name, c.Kind)
}

// Chain tries providers in order and moves on when one fails in a retryable way
// (rate limit, 5xx, timeout, CLI failure).
type Chain struct{ list []Provider }

func NewChain(ctx context.Context, cfgs []config.ProviderConfig) (*Chain, error) {
	var ch Chain
	var errs []error
	for _, c := range cfgs {
		p, err := New(ctx, c)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		ch.list = append(ch.list, p)
	}
	if len(ch.list) == 0 {
		if len(errs) == 0 {
			return nil, errors.New("no observer providers configured (run `amem init`)")
		}
		return nil, errors.Join(errs...)
	}
	return &ch, nil
}

func (c *Chain) Name() string {
	names := make([]string, len(c.list))
	for i, p := range c.list {
		names[i] = p.Name()
	}
	return strings.Join(names, ">")
}

func (c *Chain) Complete(ctx context.Context, req Request) (json.RawMessage, error) {
	var errs []error
	for _, p := range c.list {
		out, err := p.Complete(ctx, req)
		if err == nil {
			return out, nil
		}
		errs = append(errs, err)
		if pe, ok := errors.AsType[*Error](err); ok && !pe.Retryable {
			continue // misconfigured provider: still try the next one
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, errors.Join(errs...)
}

// extractJSON pulls the first JSON object out of model text (code fences, prose around it).
func extractJSON(s string) (json.RawMessage, error) {
	s = strings.TrimSpace(s)
	if rest, ok := strings.CutPrefix(s, "```"); ok {
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			rest = rest[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(rest), "```")
	}
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end < start {
		return nil, fmt.Errorf("no JSON object in model output: %.200q", s)
	}
	raw := json.RawMessage(s[start : end+1])
	if !json.Valid(raw) {
		// Walk forward for the first balanced object.
		dec := json.NewDecoder(bytes.NewReader([]byte(s[start:])))
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("invalid JSON in model output: %w", err)
		}
		raw = v
	}
	return raw, nil
}

// schemaPrompt is appended when a backend cannot enforce the schema itself.
func schemaPrompt(schema map[string]any) string {
	b, _ := json.Marshal(schema)
	return "\n\nRespond with one JSON object only, no prose, matching this JSON schema:\n" + string(b)
}
