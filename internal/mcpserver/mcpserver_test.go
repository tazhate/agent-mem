package mcpserver

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"agent-mem/internal/daemon"
)

func TestTools(t *testing.T) {
	saved := map[string]any{}
	call := func(op string, params, out any) error {
		switch op {
		case daemon.OpSave:
			saved["text"] = params.(daemon.SaveParams).Text
			if m, ok := out.(*map[string]any); ok {
				*m = map[string]any{"id": 7}
			}
		case daemon.OpSearch:
			*(out.(*[]daemon.SearchHit)) = []daemon.SearchHit{{ID: 7, Type: "gotcha", Title: "vault sealed", Date: "2026-11-05", Tokens: 40}}
		}
		return nil
	}
	ctx := t.Context()
	ct, st := mcp.NewInMemoryTransports()
	srv := New(call, "/repo")
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 5 {
		t.Fatalf("tools: %v %d", err, len(tools.Tools))
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "memory_search", Arguments: map[string]any{"query": "vault"}})
	if err != nil || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "#7") {
		t.Fatalf("search: %v %+v", err, res)
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "memory_save", Arguments: map[string]any{"text": "remember this"}})
	if err != nil || saved["text"] != "remember this" || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "#7") {
		t.Fatalf("save: %v %+v %v", err, res, saved)
	}
}
