package tools

import (
	"context"
	"encoding/json"
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
	return &LogsByRequestIDHandler{client: client}
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
			mcp.Description("Maximum number of entries to return (default 100)"),
		),
	)
}

// Handle processes the logs.by_request_id tool request.
func (h *LogsByRequestIDHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	requestID, err := request.RequireString("request_id")
	if err != nil {
		return mcp.NewToolResultError("request_id is required"), nil
	}

	project := request.GetString("project_id", h.client.Config.DefaultProject)
	limit := clampLimit(request.GetInt("limit", 100), 100, h.client.Config.LogsMaxLimit)

	timeFilter, err := buildTimeFilter(request)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	result, err := gcpdata.QueryLogsByRequestID(ctx, h.client.Logging, project, requestID, timeFilter, limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to query logs by request ID: %v. Verify the request_id is correct. Use logs.find_requests to discover valid request IDs.", err)), nil
	}

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to marshal result: %v", err)), nil
	}

	return mcp.NewToolResultText(string(data)), nil
}
