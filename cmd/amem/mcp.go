package main

import (
	"context"
	"os/signal"
	"syscall"

	"github.com/tazhate/agent-mem/internal/mcpserver"
)

func init() {
	register("mcp", "MCP server on stdio (memory_search, memory_get, memory_timeline, memory_save, work_state)", cmdMCP)
}

func cmdMCP(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := mcpserver.RunStdio(ctx, loadCfg()); err != nil && ctx.Err() == nil {
		return fail("mcp: %v", err)
	}
	return 0
}
