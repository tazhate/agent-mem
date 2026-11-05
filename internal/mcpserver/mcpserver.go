// Package mcpserver exposes memory to agents as MCP tools. Every call is forwarded to amemd, so
// the per-session MCP process stays tiny and all state lives in one place.
package mcpserver

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tazhate/agent-mem/internal/config"
	"github.com/tazhate/agent-mem/internal/daemon"
	"github.com/tazhate/agent-mem/internal/store"
)

// Caller sends an API op to the daemon. The stdio server uses the socket; the daemon's own
// HTTP endpoint calls the API in process.
type Caller func(op string, params, out any) error

type SearchIn struct {
	Query   string   `json:"query" jsonschema:"what to look for, in any language"`
	Project string   `json:"project,omitempty" jsonschema:"project name; default is the current project, 'all' searches everything"`
	Types   []string `json:"types,omitempty" jsonschema:"filter by type: decision, bugfix, gotcha, change, discovery, preference, todo"`
	Limit   int      `json:"limit,omitempty" jsonschema:"max results, default 10"`
}

type GetIn struct {
	IDs []int64 `json:"ids" jsonschema:"memory ids from search results or injected context (#123 -> 123)"`
}

type TimelineIn struct {
	ID     int64 `json:"id" jsonschema:"anchor memory id"`
	Before int   `json:"before,omitempty" jsonschema:"memories before the anchor, default 5"`
	After  int   `json:"after,omitempty" jsonschema:"memories after the anchor, default 5"`
}

type SaveIn struct {
	Text   string   `json:"text" jsonschema:"what to remember: a decision, gotcha, preference or fact worth keeping"`
	Title  string   `json:"title,omitempty" jsonschema:"short title, default is the first line"`
	Type   string   `json:"type,omitempty" jsonschema:"decision, bugfix, gotcha, change, discovery, preference or todo"`
	Global bool     `json:"global,omitempty" jsonschema:"true for user preferences and rules that apply in every project"`
	Files  []string `json:"files,omitempty" jsonschema:"related file paths"`
}

type WorkIn struct {
	List          string         `json:"list,omitempty" jsonschema:"list name, e.g. 'release'; required when writing"`
	Fields        map[string]any `json:"fields,omitempty" jsonschema:"to write: {task, status: todo|doing|done|dropped, ...}; omit to read"`
	IncludeClosed bool           `json:"include_closed,omitempty" jsonschema:"when reading, also show done and dropped items"`
}

const instructions = `amem is persistent memory across sessions and agents.
Injected context lines look like "#123 type: title (~Nt)" where ~Nt is the cost of the full record.
Fetch full records with memory_get only for ids that matter to the current task.
Search with memory_search before re-investigating something that may have been solved before.
Save durable knowledge with memory_save: decisions with their reason, gotchas, user preferences (global).
Track multi-step work with work_state.`

// New builds the MCP server. cwd scopes project-relative calls.
func New(call Caller, cwd string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "amem", Version: daemon.Version}, &mcp.ServerOptions{Instructions: instructions})

	mcp.AddTool(s, &mcp.Tool{Name: "memory_search", Description: "Search memory of past sessions. Returns an index: id, date, type, title, token cost. Fetch details with memory_get."},
		func(ctx context.Context, req *mcp.CallToolRequest, in SearchIn) (*mcp.CallToolResult, any, error) {
			p := daemon.SearchParams{Query: in.Query, Types: in.Types, Limit: in.Limit}
			switch in.Project {
			case "all":
			case "":
				p.Cwd = cwd
			default:
				p.Project = in.Project
			}
			var hits []daemon.SearchHit
			if err := call(daemon.OpSearch, p, &hits); err != nil {
				return nil, nil, err
			}
			if len(hits) == 0 {
				return text("no matches"), nil, nil
			}
			var b strings.Builder
			for _, h := range hits {
				fmt.Fprintf(&b, "#%d %s %s [%s] %s (~%dt)\n", h.ID, h.Date, h.Type, h.Project, h.Title, h.Tokens)
			}
			return text(b.String()), nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "memory_get", Description: "Fetch full memory records by id."},
		func(ctx context.Context, req *mcp.CallToolRequest, in GetIn) (*mcp.CallToolResult, any, error) {
			var mems []*store.Memory
			if err := call(daemon.OpGet, daemon.GetParams{IDs: in.IDs}, &mems); err != nil {
				return nil, nil, err
			}
			return text(FormatMemories(mems)), nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "memory_timeline", Description: "Show what happened around a memory in the same project, in time order."},
		func(ctx context.Context, req *mcp.CallToolRequest, in TimelineIn) (*mcp.CallToolResult, any, error) {
			var mems []*store.Memory
			if err := call(daemon.OpTimeline, daemon.TimelineParams{Anchor: in.ID, Before: in.Before, After: in.After}, &mems); err != nil {
				return nil, nil, err
			}
			var b strings.Builder
			for _, m := range mems {
				mark := " "
				if m.ID == in.ID {
					mark = ">"
				}
				fmt.Fprintf(&b, "%s #%d %s %s: %s (~%dt)\n", mark, m.ID, time.UnixMilli(m.CreatedAt).Format("2006-01-02 15:04"), m.Type, m.Title, m.Tokens())
			}
			return text(b.String()), nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "memory_save", Description: "Save something worth remembering in future sessions."},
		func(ctx context.Context, req *mcp.CallToolRequest, in SaveIn) (*mcp.CallToolResult, any, error) {
			var out map[string]any
			err := call(daemon.OpSave, daemon.SaveParams{Text: in.Text, Title: in.Title, Type: in.Type, Global: in.Global, Files: in.Files, Cwd: cwd}, &out)
			if err != nil {
				return nil, nil, err
			}
			return text(fmt.Sprintf("saved #%v", out["id"])), nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "work_state", Description: "Read or update to-do lists and work state that persist across sessions. With fields: append an entry (latest value per key wins, null clears, status done/dropped closes). Without fields: read."},
		func(ctx context.Context, req *mcp.CallToolRequest, in WorkIn) (*mcp.CallToolResult, any, error) {
			var lists []store.WorkList
			op := daemon.OpWorkR
			if len(in.Fields) > 0 {
				op = daemon.OpWorkW
			}
			if err := call(op, daemon.WorkParams{List: in.List, Fields: in.Fields, Cwd: cwd, IncludeClosed: in.IncludeClosed}, &lists); err != nil {
				return nil, nil, err
			}
			return text(formatWork(lists)), nil, nil
		})
	return s
}

// RunStdio serves MCP over stdio, forwarding to the daemon socket.
func RunStdio(ctx context.Context, cfg *config.Config) error {
	cwd, _ := os.Getwd()
	call := func(op string, params, out any) error { return daemon.CallData(cfg, op, params, out) }
	return New(call, cwd).Run(ctx, &mcp.StdioTransport{})
}

// HTTPHandler serves streamable-HTTP MCP from inside the daemon. Every request must carry
// "Authorization: Bearer <token>": the unix socket is protected by file permissions, a TCP
// port is reachable by every local user.
func HTTPHandler(call Caller, token string) http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return New(call, r.URL.Query().Get("cwd"))
	}, nil)
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// HTTPToken returns the bearer token stored at path, creating it (0600) on first use.
func HTTPToken(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
		return strings.TrimSpace(string(b)), nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

// CheckLoopback refuses HTTP listen addresses other than loopback.
func CheckLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("daemon.http_addr %q: only loopback addresses are allowed", addr)
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// FormatMemories renders full records for agents and the CLI.
func FormatMemories(mems []*store.Memory) string {
	if len(mems) == 0 {
		return "not found"
	}
	var b strings.Builder
	for _, m := range mems {
		scope := m.Project
		if m.ProjectID == 0 {
			scope = "global"
		}
		fmt.Fprintf(&b, "#%d %s [%s] %s\n%s\n", m.ID, m.Type, scope, time.UnixMilli(m.CreatedAt).Format("2006-01-02"), m.Title)
		if m.Body != "" && strings.TrimSpace(m.Body) != strings.TrimSpace(strings.TrimSuffix(m.Title, "…")) {
			b.WriteString(m.Body + "\n")
		}
		for _, f := range m.Facts {
			b.WriteString("- " + f + "\n")
		}
		if len(m.Files) > 0 {
			b.WriteString("files: " + strings.Join(m.Files, ", ") + "\n")
		}
		if m.SupersededBy != 0 {
			fmt.Fprintf(&b, "(superseded by #%d)\n", m.SupersededBy)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func formatWork(lists []store.WorkList) string {
	if len(lists) == 0 {
		return "no open work"
	}
	var b strings.Builder
	for _, l := range lists {
		b.WriteString(l.List)
		for k, v := range l.State {
			fmt.Fprintf(&b, " %s=%v", k, v)
		}
		b.WriteString("\n")
		for _, it := range l.Items {
			fmt.Fprintf(&b, "  - %s", it.Task)
			for k, v := range it.Fields {
				if k != "task" {
					fmt.Fprintf(&b, " %s=%v", k, v)
				}
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}
