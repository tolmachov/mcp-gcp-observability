package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

// LogsQueryHandler handles the logs.query tool.
type LogsQueryHandler struct {
	client *gcpclient.Client
}

// NewLogsQueryHandler creates a new LogsQueryHandler.
func NewLogsQueryHandler(client *gcpclient.Client) *LogsQueryHandler {
	return &LogsQueryHandler{client: client}
}

// Tool returns the MCP tool definition.
func (h *LogsQueryHandler) Tool() mcp.Tool {
	return mcp.NewTool("logs.query",
		mcp.WithDescription("Execute an arbitrary Cloud Logging query with filter syntax. Use Cloud Logging filter language (e.g. severity>=ERROR, resource.type=\"k8s_container\")."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("project_id",
			mcp.Description("GCP project ID (uses default if not specified)"),
		),
		mcp.WithString("filter",
			mcp.Description("Cloud Logging filter expression (e.g. 'severity>=ERROR', 'resource.type=\"k8s_container\"')"),
			mcp.Required(),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of entries to return (default 100)"),
		),
		mcp.WithString("order",
			mcp.Description("Sort order: 'asc' or 'desc' (default 'desc')"),
		),
		mcp.WithString("page_token",
			mcp.Description("Page token for pagination"),
		),
	)
}

// Handle processes the logs.query tool request.
func (h *LogsQueryHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	filter, err := request.RequireString("filter")
	if err != nil {
		return mcp.NewToolResultError("filter is required"), nil
	}

	project := request.GetString("project_id", h.client.Config.DefaultProject)
	limit := clampLimit(request.GetInt("limit", 100), 100, h.client.Config.LogsMaxLimit)
	order := request.GetString("order", "desc")
	pageToken := request.GetString("page_token", "")

	result, err := gcpdata.QueryLogs(ctx, h.client.Logging, project, filter, limit, order, pageToken)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to query logs: %v", err)), nil
	}

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to marshal result: %v", err)), nil
	}

	return mcp.NewToolResultText(string(data)), nil
}
