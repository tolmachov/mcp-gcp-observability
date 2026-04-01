package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

// LogsServicesHandler handles the logs.services tool.
type LogsServicesHandler struct {
	client *gcpclient.Client
}

// NewLogsServicesHandler creates a new LogsServicesHandler.
func NewLogsServicesHandler(client *gcpclient.Client) *LogsServicesHandler {
	return &LogsServicesHandler{client: client}
}

// Tool returns the MCP tool definition.
func (h *LogsServicesHandler) Tool() mcp.Tool {
	return mcp.NewTool("logs.services",
		mcp.WithDescription("List available services and resources in the project. Useful as a first step to understand what services exist before querying their logs."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("project_id",
			mcp.Description("GCP project ID (uses default if not specified)"),
		),
	)
}

// Handle processes the logs.services tool request.
func (h *LogsServicesHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := request.GetString("project_id", h.client.Config.DefaultProject)

	result, err := gcpdata.ListServices(ctx, h.client.Logging, project)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to list services: %v", err)), nil
	}

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to marshal result: %v", err)), nil
	}

	return mcp.NewToolResultText(string(data)), nil
}
