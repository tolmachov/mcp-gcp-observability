package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

// LogsByTraceHandler handles the logs.by_trace tool.
type LogsByTraceHandler struct {
	client *gcpclient.Client
}

// NewLogsByTraceHandler creates a new LogsByTraceHandler.
func NewLogsByTraceHandler(client *gcpclient.Client) *LogsByTraceHandler {
	return &LogsByTraceHandler{client: client}
}

// Tool returns the MCP tool definition.
func (h *LogsByTraceHandler) Tool() mcp.Tool {
	return mcp.NewTool("logs.by_trace",
		mcp.WithDescription("Find all log entries associated with a specific trace ID. "+
			"Returns logs sorted by timestamp ascending to show the request flow. "+
			"Get trace IDs from logs.find_requests results or from the trace field in logs.query output. "+
			"If you have a request_id instead of a trace_id, use logs.by_request_id."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithString("trace_id",
			mcp.Description("The trace ID (just the hex ID, not the full resource path)"),
			mcp.Required(),
		),
		mcp.WithString("project_id",
			mcp.Description("GCP project ID (uses default if not specified)"),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of entries to return (default 100)"),
		),
	)
}

// Handle processes the logs.by_trace tool request.
func (h *LogsByTraceHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	traceID, err := request.RequireString("trace_id")
	if err != nil {
		return mcp.NewToolResultError("trace_id is required"), nil
	}

	project := request.GetString("project_id", h.client.Config.DefaultProject)
	limit := clampLimit(request.GetInt("limit", 100), 100, h.client.Config.LogsMaxLimit)

	result, err := gcpdata.QueryLogsByTrace(ctx, h.client.Logging, project, traceID, limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to query logs by trace: %v. Verify the trace_id format (hex string, not full resource path). Use logs.find_requests to discover valid trace IDs.", err)), nil
	}

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to marshal result: %v", err)), nil
	}

	return mcp.NewToolResultText(string(data)), nil
}
