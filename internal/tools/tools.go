package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

// sendProgress sends a progress notification if the request includes a progress token.
func sendProgress(ctx context.Context, request mcp.CallToolRequest, progress, total float64, message string) {
	token := progressToken(request)
	if token == nil {
		return
	}
	srv := server.ServerFromContext(ctx)
	if srv == nil {
		return
	}
	_ = srv.SendNotificationToClient(ctx, "notifications/progress", map[string]any{
		"progressToken": token,
		"progress":      progress,
		"total":         total,
		"message":       message,
	})
}

// progressToken extracts the progress token from a request, or nil if not present.
func progressToken(request mcp.CallToolRequest) mcp.ProgressToken {
	if request.Params.Meta == nil {
		return nil
	}
	return request.Params.Meta.ProgressToken
}

// mcpLog sends a structured log message via MCP logging notification.
func mcpLog(ctx context.Context, level mcp.LoggingLevel, logger string, data any) {
	srv := server.ServerFromContext(ctx)
	if srv == nil {
		return
	}
	_ = srv.SendLogMessageToClient(ctx, mcp.NewLoggingMessageNotification(level, logger, data))
}

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
			mcp.Description("Start of time range in RFC3339 format (e.g. '2025-01-15T00:00:00Z'). Defaults to 24 hours ago, or 24 hours before end_time if only end_time is provided."),
		),
		mcp.WithString("end_time",
			mcp.Description("End of time range in RFC3339 format (e.g. '2025-01-15T23:59:59Z'). If omitted, only the start bound is applied (open-ended towards now)."),
		),
	)
	return mcp.NewTool(name, opts...)
}

// buildTimeFilter constructs a Cloud Logging timestamp filter from request params.
// Returns the timestamp portion of a Cloud Logging filter, e.g. `timestamp>="..." timestamp<="..."`
// joined by newline (implicit AND).
// When neither start_time nor end_time is specified, defaults start_time to 24h ago (open-ended towards now).
// When only end_time is given, start_time defaults to 24h before end_time.
// Returns an error if end_time is not after start_time.
func buildTimeFilter(request mcp.CallToolRequest) (string, error) {
	startTime := request.GetString("start_time", "")
	endTime := request.GetString("end_time", "")

	if startTime == "" && endTime == "" {
		startTime = time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	}

	var parsedStart, parsedEnd time.Time
	var filter string
	if startTime != "" {
		var err error
		parsedStart, err = time.Parse(time.RFC3339, startTime)
		if err != nil {
			return "", fmt.Errorf("invalid start_time %q: must be RFC3339 format (e.g. 2025-01-15T00:00:00Z)", startTime)
		}
		filter = fmt.Sprintf(`timestamp>="%s"`, startTime)
	}
	if endTime != "" {
		var err error
		parsedEnd, err = time.Parse(time.RFC3339, endTime)
		if err != nil {
			return "", fmt.Errorf("invalid end_time %q: must be RFC3339 format (e.g. 2025-01-15T23:59:59Z)", endTime)
		}
		// Default start to 24h before end to avoid unbounded scans
		if startTime == "" {
			parsedStart = parsedEnd.Add(-24 * time.Hour)
			filter = fmt.Sprintf(`timestamp>="%s"`, parsedStart.Format(time.RFC3339))
		}
		filter = gcpdata.AppendFilter(filter, fmt.Sprintf(`timestamp<="%s"`, endTime))
	}

	if !parsedStart.IsZero() && !parsedEnd.IsZero() && !parsedEnd.After(parsedStart) {
		return "", fmt.Errorf("end_time must be after start_time (got start=%s, end=%s)", startTime, endTime)
	}

	return filter, nil
}

// requireClient validates that client is non-nil, panicking on programming errors.
func requireClient(client *gcpclient.Client) *gcpclient.Client {
	if client == nil {
		panic("nil client")
	}
	return client
}

// requireProject returns the project ID from the request or default, returning an error result if empty.
func requireProject(request mcp.CallToolRequest, defaultProject string) (string, *mcp.CallToolResult) {
	project := request.GetString("project_id", defaultProject)
	if project == "" {
		return "", mcp.NewToolResultError("project_id must not be empty: either omit it to use the default project, or provide a valid project ID")
	}
	return project, nil
}

// jsonResult marshals v as indented JSON text and also sets structuredContent for typed output.
func jsonResult(v any) (*mcp.CallToolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to marshal result: %v", err)), nil
	}
	result := mcp.NewToolResultText(string(data))
	result.StructuredContent = v
	return result, nil
}

// clampLimit returns limit clamped to [1, maxLimit], falling back to defaultVal when limit is non-positive.
func clampLimit(limit, defaultVal, maxLimit int) int {
	if limit <= 0 {
		return defaultVal
	}
	if limit > maxLimit {
		return maxLimit
	}
	return limit
}
