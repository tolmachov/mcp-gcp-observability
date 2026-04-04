package tools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

// LogsByRequestIDHandler handles the logs.by_request_id tool.
type LogsByRequestIDHandler struct {
	client *gcpclient.Client
}

// NewLogsByRequestIDHandler creates a new LogsByRequestIDHandler.
func NewLogsByRequestIDHandler(client *gcpclient.Client) *LogsByRequestIDHandler {
	return &LogsByRequestIDHandler{client: requireClient(client)}
}

// Tool returns the MCP tool definition.
func (h *LogsByRequestIDHandler) Tool() mcp.Tool {
	return newToolWithTimeFilter("logs.by_request_id",
		mcp.WithDescription("Find all log entries associated with a specific request ID (jsonPayload.request_id). "+
			"Returns logs sorted by timestamp ascending to show the full request lifecycle. "+
			"Get request IDs from logs.find_requests results. "+
			"If you have a trace ID instead, use logs.by_trace or trace.get."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithString("request_id",
			mcp.Description("The request ID to search for"),
			mcp.Required(),
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

// Handle processes the logs.by_request_id tool request.
func (h *LogsByRequestIDHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	requestID, err := request.RequireString("request_id")
	if err != nil {
		return mcp.NewToolResultError("request_id is required"), nil
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

	result, err := gcpdata.QueryLogsByRequestID(ctx, h.client.LoggingClient(), project, requestID, timeFilter, limit, pageToken)
	if err != nil {
		mcpLog(ctx, mcp.LoggingLevelError, "logs.by_request_id", fmt.Sprintf("request ID query failed for %s: %v", requestID, err))
		return mcp.NewToolResultError(fmt.Sprintf("Failed to query logs by request ID: %v. Verify the request_id is correct. Use logs.find_requests to discover valid request IDs.", err)), nil
	}

	return jsonResult(result)
}
