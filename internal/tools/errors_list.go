package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

// ErrorsListHandler handles the errors.list tool.
type ErrorsListHandler struct {
	client *gcpclient.Client
}

// NewErrorsListHandler creates a new ErrorsListHandler.
func NewErrorsListHandler(client *gcpclient.Client) *ErrorsListHandler {
	return &ErrorsListHandler{client: client}
}

// Tool returns the MCP tool definition.
func (h *ErrorsListHandler) Tool() mcp.Tool {
	return mcp.NewTool("errors.list",
		mcp.WithDescription("List error groups from Google Cloud Error Reporting, sorted by occurrence count. Returns aggregated errors, not individual log entries."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("project_id",
			mcp.Description("GCP project ID (uses default if not specified)"),
		),
		mcp.WithNumber("time_range_hours",
			mcp.Description("Time range in hours to look back (default 24, max 720)"),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of error groups to return (default 50)"),
		),
		mcp.WithString("service_filter",
			mcp.Description("Filter by service name"),
		),
		mcp.WithString("version_filter",
			mcp.Description("Filter by service version"),
		),
	)
}

// Handle processes the errors.list tool request.
func (h *ErrorsListHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := request.GetString("project_id", h.client.Config.DefaultProject)
	timeRangeHours := request.GetInt("time_range_hours", 24)
	limit := clampLimit(request.GetInt("limit", 50), 50, h.client.Config.ErrorsMaxLimit)
	serviceFilter := request.GetString("service_filter", "")
	versionFilter := request.GetString("version_filter", "")

	result, err := gcpdata.ListErrors(ctx, h.client.Errors, project, timeRangeHours, limit, serviceFilter, versionFilter)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to list errors: %v", err)), nil
	}

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to marshal result: %v", err)), nil
	}

	return mcp.NewToolResultText(string(data)), nil
}
