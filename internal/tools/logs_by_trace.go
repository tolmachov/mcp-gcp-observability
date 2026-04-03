package tools

import (
	"context"
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
	return &LogsByTraceHandler{client: requireClient(client)}
}

// Tool returns the MCP tool definition.
func (h *LogsByTraceHandler) Tool() mcp.Tool {
	return newToolWithTimeFilter("logs.by_trace",
		mcp.WithDescription("Find all log entries associated with a specific trace ID. "+
			"Returns logs sorted by timestamp ascending to show the request flow. "+
			"Get trace IDs from logs.find_requests results or from the trace field in logs.query output. "+
			"If you have a request_id instead of a trace_id, use logs.by_request_id."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithString("trace_id",
			mcp.Description("The trace ID (32-character hex string, not the full resource path)"),
			mcp.Required(),
			mcp.Pattern(`^[a-fA-F0-9]{32}$`),
		),
		mcp.WithString("project_id",
			mcp.Description("GCP project ID (uses default if not specified)"),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of log entries to return (default 100, server max applies)"),
			mcp.Min(1),
		),
		mcp.WithString("page_token",
			mcp.Description("Page token for pagination (from previous response's next_page_token)"),
		),
	)
}

// Handle processes the logs.by_trace tool request.
func (h *LogsByTraceHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	traceID, err := request.RequireString("trace_id")
	if err != nil {
		return mcp.NewToolResultError("trace_id is required"), nil
	}

	project, errResult := requireProject(request, h.client.Config().DefaultProject)
	if errResult != nil {
		return errResult, nil
	}
	limit := clampLimit(request.GetInt("limit", 100), 100, h.client.Config().LogsMaxLimit)

	timeFilter, err := buildTimeFilter(request)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	pageToken := request.GetString("page_token", "")

	result, err := gcpdata.QueryLogsByTrace(ctx, h.client.LoggingClient(), project, traceID, timeFilter, limit, pageToken)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to query logs by trace: %v. Verify the trace_id format (hex string, not full resource path). Use logs.find_requests to discover valid trace IDs.", err)), nil
	}

	return jsonResult(result)
}
