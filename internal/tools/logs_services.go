package tools

import (
	"context"
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
	return &LogsServicesHandler{client: requireClient(client)}
}

// Tool returns the MCP tool definition.
func (h *LogsServicesHandler) Tool() mcp.Tool {
	return newToolWithTimeFilter("logs.services",
		mcp.WithDescription("List available services and resources in the project by scanning recent logs. "+
			"Discovers Kubernetes containers, Cloud Run, Cloud Functions, App Engine, and Compute Engine instances. "+
			"Useful as a first step to discover services before querying their logs. "+
			"Returns service names you can use as filters in logs.k8s or logs.query."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithString("project_id",
			mcp.Description("GCP project ID (uses default if not specified)"),
		),
	)
}

// Handle processes the logs.services tool request.
func (h *LogsServicesHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project, errResult := requireProject(request, h.client.Config().DefaultProject)
	if errResult != nil {
		return errResult, nil
	}

	timeFilter, err := buildTimeFilter(request)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	result, err := gcpdata.ListServices(ctx, h.client.LoggingClient(), project, timeFilter)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to list services: %v. Verify the project_id and that Cloud Logging API is enabled.", err)), nil
	}

	return jsonResult(result)
}
