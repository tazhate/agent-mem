// Package watch follows JSONL transcripts of runtimes that have no hooks (or for sessions hooks
// do not see, like Codex subagents) and turns new lines into events.
package watch

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tazhate/agent-mem/internal/event"
	"github.com/tazhate/agent-mem/internal/redact"
	"github.com/tazhate/agent-mem/internal/store"
)

// Parser converts one transcript line into zero or more events. State is per file.
type Parser interface {
	Runtime() string
	Line(st *FileState, line []byte) []*event.Event
}

type FileState struct {
	Path     string
	Session  string
	Cwd      string
	SkipFile bool
	pending  map[string]pendingTool // tool call id -> call
	lastText string
}

type pendingTool struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

type Preset struct {
	Name   string
	Glob   func(home string) []string // directories to walk
	Parser Parser
}

var Presets = map[string]Preset{
	"claude": {Name: "claude", Glob: func(h string) []string { return []string{filepath.Join(h, ".claude", "projects")} }, Parser: claudeParser{}},
	"codex":  {Name: "codex", Glob: func(h string) []string { return []string{filepath.Join(h, ".codex", "sessions")} }, Parser: codexParser{}},
}

var pendingRedactor = redact.New(true)

type Watcher struct {
	st     *store.Store
	preset Preset
	home   string
	send   func(*event.Event) error
	// SubagentsOnly limits Codex to subagent rollouts (top-level sessions come through hooks).
	SubagentsOnly bool
	// Since skips files not modified after this time on first sight.
	Since time.Time
	files map[string]*FileState
}

func New(st *store.Store, p Preset, home string, send func(*event.Event) error) *Watcher {
	return &Watcher{st: st, preset: p, home: home, send: send, files: map[string]*FileState{}}
}

// Run polls until ctx ends; once=true does a single pass.
func (w *Watcher) Run(ctx context.Context, interval time.Duration, once bool) error {
	for {
		if err := w.pass(ctx); err != nil {
			return err
		}
		if once {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

func (w *Watcher) pass(ctx context.Context) error {
	for _, root := range w.preset.Glob(w.home) {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") || ctx.Err() != nil {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			return w.follow(ctx, path, info)
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (w *Watcher) follow(ctx context.Context, path string, info os.FileInfo) error {
	var offset, size int64
	var session, cwd, pending string
	var sub bool
	err := w.st.DB().QueryRowContext(ctx, `SELECT offset, size, session, cwd, subagent, pending FROM watch_state WHERE path = ?`, path).
		Scan(&offset, &size, &session, &cwd, &sub, &pending)
	if errors.Is(err, sql.ErrNoRows) {
		if info.ModTime().Before(w.Since) {
			// Skip the history but learn the session header, so a resumed session that only
			// appends new lines is still attributed.
			fs := &FileState{Path: path, pending: map[string]pendingTool{}}
			w.readHeader(path, fs)
			return w.save(ctx, fs, info.Size(), info.Size())
		}
		offset = 0
	} else if err != nil {
		return err
	}
	if info.Size() < offset { // truncated or replaced
		offset = 0
	}
	if info.Size() == offset {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	fs := w.files[path]
	if fs == nil {
		// Restore what earlier lines established (session id, cwd) so a restart resumes mid-file.
		fs = &FileState{Path: path, Session: session, Cwd: cwd, pending: map[string]pendingTool{}}
		_ = json.Unmarshal([]byte(pending), &fs.pending)
		if sub {
			fs.pending["__subagent"] = pendingTool{}
		}
		w.files[path] = fs
	}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] != '\n' {
			break // partial line still being written; read it next pass
		}
		lineAt := offset
		offset += int64(len(line))
		if !fs.SkipFile {
			for i, ev := range w.preset.Parser.Line(fs, line) {
				// Stable identity from position: replaying after a crash dedupes in the daemon.
				ev.ID = stableID(path, lineAt, i)
				if w.SubagentsOnly && !fs.SkipFile && ev.AgentType == "" {
					continue
				}
				if err := w.send(ev); err != nil {
					return err
				}
			}
		}
		if err != nil {
			break
		}
	}
	return w.save(ctx, fs, offset, info.Size())
}

func (w *Watcher) save(ctx context.Context, fs *FileState, offset, size int64) error {
	_, sub := fs.pending["__subagent"]
	// Calls whose results are still to come; their arguments are cleaned like any capture.
	clean := make(map[string]pendingTool, len(fs.pending))
	for k, v := range fs.pending {
		v.Input = redact.CleanJSON(v.Input, pendingRedactor.Clean)
		clean[k] = v
	}
	pending, _ := json.Marshal(clean)
	return w.st.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO watch_state(path, offset, size, session, cwd, subagent, pending) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(path) DO UPDATE SET offset = excluded.offset, size = excluded.size, session = excluded.session,
			cwd = excluded.cwd, subagent = excluded.subagent, pending = excluded.pending`, fs.Path, offset, size, fs.Session, fs.Cwd, sub, string(pending))
		return err
	})
}

// readHeader parses the first lines of a file for session metadata only.
func (w *Watcher) readHeader(path string, fs *FileState) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	for range 50 {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			w.preset.Parser.Line(fs, line) // events are discarded; only fs fields matter here
			for k := range fs.pending {
				if k != "__subagent" {
					delete(fs.pending, k)
				}
			}
		}
		if err != nil || fs.Session != "" {
			return
		}
	}
}

func stableID(path string, offset int64, i int) string {
	h := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%d", path, offset, i))
	return "w" + hex.EncodeToString(h[:12])
}

func ev(fs *FileState, runtime string, kind event.Kind, ts int64) *event.Event {
	return &event.Event{Runtime: runtime, NativeSession: fs.Session, Cwd: fs.Cwd, Kind: kind, TS: ts}
}

func parseTS(s string) int64 {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UnixMilli()
	}
	return event.NowMS()
}

// --- Claude Code transcripts (~/.claude/projects/<dir>/<session>.jsonl) ---------

type claudeParser struct{}

func (claudeParser) Runtime() string { return "claude-code" }

// claudeSystemText reports user-role records the harness wrote, not the user: task
// notifications, slash command echoes and their output.
func claudeSystemText(t string) bool {
	t = strings.TrimSpace(t)
	for _, p := range []string{"<task-notification>", "<command-name>", "<command-message>", "<local-command-stdout>", "<local-command-stderr>", "<local-command-caveat>"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

func (claudeParser) Line(fs *FileState, line []byte) []*event.Event {
	var rec struct {
		Type      string `json:"type"`
		SessionID string `json:"sessionId"`
		Cwd       string `json:"cwd"`
		Timestamp string `json:"timestamp"`
		IsMeta    bool   `json:"isMeta"`
		// Subagent records (subagents/agent-*.jsonl) carry the parent's sessionId; compact
		// summaries are written as user messages.
		IsSidechain      bool `json:"isSidechain"`
		IsCompactSummary bool `json:"isCompactSummary"`
		Message          struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			StopReason string          `json:"stop_reason"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.IsMeta || rec.IsSidechain || rec.IsCompactSummary {
		return nil
	}
	if fs.Session == "" && rec.SessionID != "" {
		fs.Session = rec.SessionID
	}
	if rec.Cwd != "" {
		fs.Cwd = rec.Cwd
	}
	if fs.Session == "" {
		return nil
	}
	ts := parseTS(rec.Timestamp)
	var blocks []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
	}
	var text string
	if err := json.Unmarshal(rec.Message.Content, &blocks); err != nil {
		_ = json.Unmarshal(rec.Message.Content, &text)
	}
	var out []*event.Event
	switch rec.Type {
	case "user":
		if claudeSystemText(text) {
			return nil
		}
		if text != "" {
			e := ev(fs, "claude-code", event.Prompt, ts)
			e.Prompt = text
			return []*event.Event{e}
		}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if claudeSystemText(b.Text) {
					continue
				}
				e := ev(fs, "claude-code", event.Prompt, ts)
				e.Prompt = b.Text
				out = append(out, e)
			case "tool_result":
				call, ok := fs.pending[b.ToolUseID]
				if !ok {
					continue
				}
				delete(fs.pending, b.ToolUseID)
				kind := event.ToolPost
				t := &event.Tool{Name: call.Name, UseID: b.ToolUseID, Input: call.Input, Output: contentText(b.Content)}
				if b.IsError {
					kind, t.Error = event.ToolFail, t.Output
				}
				e := ev(fs, "claude-code", kind, ts)
				e.Tool = t
				out = append(out, e)
			}
		}
	case "assistant":
		for _, b := range blocks {
			switch b.Type {
			case "text":
				fs.lastText = b.Text
			case "tool_use":
				fs.pending[b.ID] = pendingTool{Name: b.Name, Input: b.Input}
			}
		}
		if rec.Message.StopReason == "end_turn" {
			e := ev(fs, "claude-code", event.TurnEnd, ts)
			e.LastAssistant = fs.lastText
			out = append(out, e)
		}
	}
	return out
}

func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return string(raw)
}

// --- Codex rollouts (~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl) ----------------

type codexParser struct{}

func (codexParser) Runtime() string { return "codex" }

func (codexParser) Line(fs *FileState, line []byte) []*event.Event {
	var rec struct {
		Timestamp string `json:"timestamp"`
		Type      string `json:"type"`
		Payload   struct {
			Type    string          `json:"type"`
			ID      string          `json:"id"`
			Cwd     string          `json:"cwd"`
			Source  json.RawMessage `json:"source"`
			Role    string          `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			CallID           string          `json:"call_id"`
			Name             string          `json:"name"`
			Arguments        string          `json:"arguments"`
			Input            json.RawMessage `json:"input"`
			Output           json.RawMessage `json:"output"`
			LastAgentMessage string          `json:"last_agent_message"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return nil
	}
	p := rec.Payload
	ts := parseTS(rec.Timestamp)
	switch rec.Type {
	case "session_meta":
		fs.Session = p.ID
		fs.Cwd = p.Cwd
		// Top-level sessions have a string source ("cli", "vscode"); subagents an object.
		if len(p.Source) > 0 && p.Source[0] == '{' {
			fs.pending["__subagent"] = pendingTool{}
		}
		return nil
	case "turn_context":
		if p.Cwd != "" {
			fs.Cwd = p.Cwd
		}
		return nil
	}
	if fs.Session == "" {
		return nil
	}
	_, sub := fs.pending["__subagent"]
	mark := func(e *event.Event) *event.Event {
		if sub {
			e.AgentType = "subagent"
		}
		return e
	}
	switch {
	case rec.Type == "response_item" && p.Type == "message" && p.Role == "user":
		var b strings.Builder
		for _, c := range p.Content {
			b.WriteString(c.Text)
		}
		t := b.String()
		if strings.HasPrefix(t, "<environment_context>") || strings.HasPrefix(t, "<user_instructions>") ||
			strings.HasPrefix(t, "<recommended_plugins>") || strings.HasPrefix(t, "<skills_instructions>") ||
			strings.HasPrefix(t, "# AGENTS.md instructions") || strings.HasPrefix(t, "<user_action>") {
			return nil
		}
		e := ev(fs, "codex", event.Prompt, ts)
		e.Prompt = t
		return []*event.Event{mark(e)}
	case rec.Type == "response_item" && (p.Type == "function_call" || p.Type == "custom_tool_call"):
		in := json.RawMessage(p.Arguments)
		if len(in) == 0 || !json.Valid(in) {
			in = p.Input
		}
		fs.pending[p.CallID] = pendingTool{Name: p.Name, Input: in}
	case rec.Type == "response_item" && (p.Type == "function_call_output" || p.Type == "custom_tool_call_output"):
		call, ok := fs.pending[p.CallID]
		if !ok {
			return nil
		}
		delete(fs.pending, p.CallID)
		e := ev(fs, "codex", event.ToolPost, ts)
		e.Tool = &event.Tool{Name: call.Name, UseID: p.CallID, Input: call.Input, Output: contentText(p.Output)}
		return []*event.Event{mark(e)}
	case rec.Type == "event_msg" && p.Type == "task_complete":
		e := ev(fs, "codex", event.TurnEnd, ts)
		e.LastAssistant = p.LastAgentMessage
		return []*event.Event{mark(e)}
	}
	return nil
}
