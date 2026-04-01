package tools

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Handler defines the interface for MCP tool handlers.
type Handler interface {
	Tool() mcp.Tool
	Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error)
}

// RegisterTools registers all handlers with the MCP server.
func RegisterTools(s *server.MCPServer, handlers []Handler) {
	for _, h := range handlers {
		s.AddTool(h.Tool(), h.Handle)
	}
}

// clampLimit ensures limit is within [1, maxLimit].
func clampLimit(limit, defaultVal, maxLimit int) int {
	if limit <= 0 {
		return defaultVal
	}
	if limit > maxLimit {
		return maxLimit
	}
	return limit
}
