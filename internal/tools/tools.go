package tools

import (
	"context"
	"fmt"
	"time"

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

// newToolWithTimeFilter creates a tool with start_time and end_time parameters appended.
func newToolWithTimeFilter(name string, opts ...mcp.ToolOption) mcp.Tool {
	opts = append(opts,
		mcp.WithString("start_time",
			mcp.Description("Start of time range in RFC3339 format (e.g. '2025-01-15T00:00:00Z'). Defaults to 24 hours ago if neither start_time nor end_time is specified."),
		),
		mcp.WithString("end_time",
			mcp.Description("End of time range in RFC3339 format (e.g. '2025-01-15T23:59:59Z'). Defaults to now."),
		),
	)
	return mcp.NewTool(name, opts...)
}

// buildTimeFilter constructs a Cloud Logging timestamp filter from request params.
// Returns just the timestamp part (e.g. `timestamp>="..." \n timestamp<="..."`).
// Defaults to last 24 hours if neither start_time nor end_time is specified.
func buildTimeFilter(request mcp.CallToolRequest) (string, error) {
	startTime := request.GetString("start_time", "")
	endTime := request.GetString("end_time", "")

	if startTime == "" && endTime == "" {
		startTime = time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	}

	var filter string
	if startTime != "" {
		if _, err := time.Parse(time.RFC3339, startTime); err != nil {
			return "", fmt.Errorf("invalid start_time %q: must be RFC3339 format (e.g. 2025-01-15T00:00:00Z)", startTime)
		}
		filter = fmt.Sprintf(`timestamp>="%s"`, startTime)
	}
	if endTime != "" {
		if _, err := time.Parse(time.RFC3339, endTime); err != nil {
			return "", fmt.Errorf("invalid end_time %q: must be RFC3339 format (e.g. 2025-01-15T23:59:59Z)", endTime)
		}
		filter = appendFilter(filter, fmt.Sprintf(`timestamp<="%s"`, endTime))
	}

	return filter, nil
}

// appendFilter joins two filter parts with a newline, handling empty base.
func appendFilter(base, part string) string {
	if base == "" {
		return part
	}
	return base + "\n" + part
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
