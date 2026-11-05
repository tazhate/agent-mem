package provider

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 3 * time.Minute}

func post(ctx context.Context, name, url string, headers map[string]string, body any) ([]byte, int, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, 0, permanent(name, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, 0, permanent(name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, retryable(name, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, resp.StatusCode, retryable(name, err)
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return out, resp.StatusCode, retryable(name, fmt.Errorf("HTTP %d: %.300s", resp.StatusCode, out))
	case resp.StatusCode >= 400:
		return out, resp.StatusCode, permanent(name, fmt.Errorf("HTTP %d: %.300s", resp.StatusCode, out))
	}
	return out, resp.StatusCode, nil
}

// --- Anthropic Messages -----------------------------------------------------

type anthropic struct {
	name, base, key, model string
	maxTokens              int
}

func (a *anthropic) Name() string { return a.name }

func (a *anthropic) Complete(ctx context.Context, req Request) (json.RawMessage, error) {
	url := cmp.Or(a.base, "https://api.anthropic.com") + "/v1/messages"
	headers := map[string]string{"x-api-key": a.key, "anthropic-version": "2023-06-01"}
	body := map[string]any{
		"model":      cmp.Or(a.model, "claude-haiku-4-5"),
		"max_tokens": cmp.Or(req.MaxTokens, a.maxTokens, 4096),
		"system":     req.System,
		"messages":   []map[string]any{{"role": "user", "content": req.User}},
		"output_config": map[string]any{
			"format": map[string]any{"type": "json_schema", "schema": req.Schema},
		},
	}
	out, status, err := post(ctx, a.name, url, headers, body)
	if err != nil && status == http.StatusBadRequest && strings.Contains(string(out), "output_config") {
		// Models or gateways without structured outputs: ask for JSON in the prompt instead.
		delete(body, "output_config")
		body["system"] = req.System + schemaPrompt(req.Schema)
		out, _, err = post(ctx, a.name, url, headers, body)
	}
	if err != nil {
		return nil, err
	}
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, retryable(a.name, err)
	}
	var text strings.Builder
	for _, c := range resp.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	raw, err := extractJSON(text.String())
	if err != nil {
		return nil, retryable(a.name, err)
	}
	return raw, nil
}

// --- OpenAI-compatible chat completions --------------------------------------

type openai struct {
	name, base, key, model string
	maxTokens              int
}

func (o *openai) Name() string { return o.name }

func (o *openai) Complete(ctx context.Context, req Request) (json.RawMessage, error) {
	url := cmp.Or(o.base, "https://api.openai.com/v1") + "/chat/completions"
	headers := map[string]string{}
	if o.key != "" {
		headers["Authorization"] = "Bearer " + o.key
	}
	body := map[string]any{
		"model": cmp.Or(o.model, "gpt-5-mini"),
		"messages": []map[string]string{
			{"role": "system", "content": req.System},
			{"role": "user", "content": req.User},
		},
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name": cmp.Or(req.SchemaName, "result"), "strict": true, "schema": req.Schema,
			},
		},
	}
	if n := cmp.Or(req.MaxTokens, o.maxTokens); n > 0 {
		body["max_tokens"] = n
	}
	out, status, err := post(ctx, o.name, url, headers, body)
	if err != nil && status == http.StatusBadRequest {
		// Many local servers only know json_object (or nothing): put the schema in the prompt.
		body["response_format"] = map[string]any{"type": "json_object"}
		body["messages"] = []map[string]string{
			{"role": "system", "content": req.System + schemaPrompt(req.Schema)},
			{"role": "user", "content": req.User},
		}
		out, status, err = post(ctx, o.name, url, headers, body)
		if err != nil && status == http.StatusBadRequest {
			delete(body, "response_format")
			out, _, err = post(ctx, o.name, url, headers, body)
		}
	}
	if err != nil {
		return nil, err
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, retryable(o.name, err)
	}
	if len(resp.Choices) == 0 {
		return nil, retryable(o.name, errors.New("empty choices"))
	}
	raw, err := extractJSON(resp.Choices[0].Message.Content)
	if err != nil {
		return nil, retryable(o.name, err)
	}
	return raw, nil
}

// --- exec: any CLI on a subscription ------------------------------------------

// Placeholders in exec command arguments.
const (
	PhSchema     = "{schema}"      // schema JSON inline
	PhSchemaFile = "{schema_file}" // path of a temp file holding the schema
	PhOutFile    = "{out_file}"    // the CLI writes its final message here; read instead of stdout
	PhModel      = "{model}"
)

type execProvider struct {
	name    string
	command []string
	model   string
}

func (e *execProvider) Name() string { return e.name }

func (e *execProvider) Complete(ctx context.Context, req Request) (json.RawMessage, error) {
	dir, err := os.MkdirTemp("", "amem-exec-")
	if err != nil {
		return nil, permanent(e.name, err)
	}
	defer os.RemoveAll(dir)
	schema, _ := json.Marshal(req.Schema)
	schemaFile := filepath.Join(dir, "schema.json")
	outFile := filepath.Join(dir, "out.txt")
	usesSchema, usesOut := false, false
	args := make([]string, 0, len(e.command))
	for i, a := range e.command {
		if a == PhModel && e.model == "" {
			// No model configured: drop "--model {model}" and let the CLI use its default.
			if i > 0 && strings.HasPrefix(e.command[i-1], "-") && len(args) > 0 {
				args = args[:len(args)-1]
			}
			continue
		}
		if strings.Contains(a, PhSchema) || strings.Contains(a, PhSchemaFile) {
			usesSchema = true
		}
		if strings.Contains(a, PhOutFile) {
			usesOut = true
		}
		a = strings.NewReplacer(PhSchema, string(schema), PhSchemaFile, schemaFile, PhOutFile, outFile, PhModel, e.model).Replace(a)
		args = append(args, a)
	}
	if err := os.WriteFile(schemaFile, schema, 0o600); err != nil {
		return nil, permanent(e.name, err)
	}
	prompt := req.System + "\n\n" + req.User
	if !usesSchema {
		prompt += schemaPrompt(req.Schema)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(prompt)
	// The observer's own CLI session must not trigger amem hooks again.
	cmd.Env = append(os.Environ(), "AMEM_DISABLE=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, permanent(e.name, err)
		}
		return nil, retryable(e.name, fmt.Errorf("%v: %.300s", err, strings.TrimSpace(stderr.String())))
	}
	text := stdout.String()
	if usesOut {
		if b, err := os.ReadFile(outFile); err == nil {
			text = string(b)
		}
	}
	// claude -p --output-format json wraps the answer: prefer structured_output, then result.
	var wrapped struct {
		StructuredOutput json.RawMessage `json:"structured_output"`
		Result           *string         `json:"result"`
		IsError          bool            `json:"is_error"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(text)), &wrapped) == nil {
		switch {
		case wrapped.IsError:
			return nil, retryable(e.name, fmt.Errorf("cli reported error: %.300s", text))
		case len(wrapped.StructuredOutput) > 0 && string(wrapped.StructuredOutput) != "null":
			return wrapped.StructuredOutput, nil
		case wrapped.Result != nil:
			text = *wrapped.Result
		}
	}
	raw, err := extractJSON(text)
	if err != nil {
		return nil, retryable(e.name, err)
	}
	return raw, nil
}

// Presets are ready-made exec commands for subscription CLIs.
var Presets = map[string][]string{
	"claude": {"claude", "-p", "--output-format", "json", "--model", PhModel, "--no-session-persistence",
		"--strict-mcp-config", "--settings", `{"disableAllHooks":true}`, "--tools", "", "--json-schema", PhSchema},
	"codex": {"codex", "exec", "--skip-git-repo-check", "--ephemeral", "--sandbox", "read-only",
		"--model", PhModel, "--output-schema", PhSchemaFile, "-o", PhOutFile, "-"},
	"gemini": {"gemini", "-m", PhModel, "-p", "Answer with the JSON object only."},
}

// PresetModels are cheap, fast defaults per CLI.
var PresetModels = map[string]string{"claude": "haiku", "codex": "", "gemini": "gemini-2.5-flash"}
