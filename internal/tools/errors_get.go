package tools

import (
	"context"
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
	return &ErrorsGetHandler{client: requireClient(client)}
}

// Tool returns the MCP tool definition.
func (h *ErrorsGetHandler) Tool() mcp.Tool {
	return mcp.NewTool("errors.get",
		mcp.WithDescription("Get details for a specific error group, including individual error events with stack traces and context. "+
			"Requires a group_id from errors.list results. "+
			"Returns all recent events for the group (time filtering is not supported for individual error events)."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithString("group_id",
			mcp.Description("Error group ID (from errors.list results)"),
			mcp.Required(),
		),
		mcp.WithString("project_id",
			mcp.Description("GCP project ID (uses default if not specified)"),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of error events to return (default 20, server max applies)"),
			mcp.Min(1),
		),
	)
}

// Handle processes the errors.get tool request.
func (h *ErrorsGetHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	groupID, err := request.RequireString("group_id")
	if err != nil {
		return mcp.NewToolResultError("group_id is required"), nil
	}

	project, errResult := requireProject(request, h.client.Config().DefaultProject)
	if errResult != nil {
		return errResult, nil
	}
	limit := clampLimit(request.GetInt("limit", 20), 20, h.client.Config().ErrorsMaxLimit)

	result, err := gcpdata.GetErrorGroup(ctx, h.client.ErrorsClient(), project, groupID, limit)
	if err != nil {
		mcpLog(ctx, mcp.LoggingLevelError, "errors.get", fmt.Sprintf("get error group %s failed: %v", groupID, err))
		return mcp.NewToolResultError(fmt.Sprintf("Failed to get error group: %v. Verify the group_id is valid — use errors.list to find available group IDs.", err)), nil
	}

	return jsonResult(result)
}
