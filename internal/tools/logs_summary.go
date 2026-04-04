package tools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

// LogsSummaryHandler handles the logs.summary tool.
type LogsSummaryHandler struct {
	client *gcpclient.Client
}

// NewLogsSummaryHandler creates a new LogsSummaryHandler.
func NewLogsSummaryHandler(client *gcpclient.Client) *LogsSummaryHandler {
	return &LogsSummaryHandler{client: requireClient(client)}
}

// Tool returns the MCP tool definition.
func (h *LogsSummaryHandler) Tool() mcp.Tool {
	return newToolWithTimeFilter("logs.summary",
		mcp.WithDescription("Get an aggregated summary of logs (based on up to 1000 sampled entries): severity distribution, top services, top errors, and sample entries. "+
			"Useful for initial triage before drilling down with logs.query or logs.k8s. "+
			"Does NOT return full log entries — use logs.query for that."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithString("project_id",
			mcp.Description("GCP project ID (uses default if not specified)"),
		),
		mcp.WithString("filter",
			mcp.Description("Additional Cloud Logging filter to narrow the scope"),
		),
	)
}

// Handle processes the logs.summary tool request.
func (h *LogsSummaryHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project, errResult := requireProject(request, h.client.Config().DefaultProject)
	if errResult != nil {
		return errResult, nil
	}
	filter := request.GetString("filter", "")

	timeFilter, err := buildTimeFilter(request)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	filter = gcpdata.AppendFilter(filter, timeFilter)

	result, err := gcpdata.SummarizeLogs(ctx, h.client.LoggingClient(), project, filter)
	if err != nil {
		mcpLog(ctx, mcp.LoggingLevelError, "logs.summary", fmt.Sprintf("summarize failed for project %s: %v", project, err))
		return mcp.NewToolResultError(fmt.Sprintf("Failed to summarize logs: %v. Verify the project_id and filter syntax.", err)), nil
	}

	return jsonResult(result)
}
