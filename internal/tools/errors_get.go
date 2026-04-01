package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

// ErrorsGetHandler handles the errors.get tool.
type ErrorsGetHandler struct {
	client *gcpclient.Client
}

// NewErrorsGetHandler creates a new ErrorsGetHandler.
func NewErrorsGetHandler(client *gcpclient.Client) *ErrorsGetHandler {
	return &ErrorsGetHandler{client: client}
}

// Tool returns the MCP tool definition.
func (h *ErrorsGetHandler) Tool() mcp.Tool {
	return mcp.NewTool("errors.get",
		mcp.WithDescription("Get details for a specific error group, including individual error events with stack traces and context."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("group_id",
			mcp.Description("Error group ID (from errors.list results)"),
			mcp.Required(),
		),
		mcp.WithString("project_id",
			mcp.Description("GCP project ID (uses default if not specified)"),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of error events to return (default 20)"),
		),
	)
}

// Handle processes the errors.get tool request.
func (h *ErrorsGetHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	groupID, err := request.RequireString("group_id")
	if err != nil {
		return mcp.NewToolResultError("group_id is required"), nil
	}

	project := request.GetString("project_id", h.client.Config.DefaultProject)
	limit := clampLimit(request.GetInt("limit", 20), 20, h.client.Config.ErrorsMaxLimit)

	result, err := gcpdata.GetErrorGroup(ctx, h.client.Errors, project, groupID, limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to get error group: %v", err)), nil
	}

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to marshal result: %v", err)), nil
	}

	return mcp.NewToolResultText(string(data)), nil
}
