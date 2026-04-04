package tools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

// TraceGetHandler handles the trace.get tool.
type TraceGetHandler struct {
	client *gcpclient.Client
}

// NewTraceGetHandler creates a new TraceGetHandler.
func NewTraceGetHandler(client *gcpclient.Client) *TraceGetHandler {
	return &TraceGetHandler{client: requireClient(client)}
}

// Tool returns the MCP tool definition.
func (h *TraceGetHandler) Tool() mcp.Tool {
	return mcp.NewTool("trace.get",
		mcp.WithDescription("Get trace details with all spans by trace ID. "+
			"Returns a span tree (parent-child hierarchy) sorted by start time, showing the full request execution flow. "+
			"Use trace IDs from logs.find_requests results or the trace field in logs.query output. "+
			"Requires Cloud Trace API to be enabled in the project."),
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
	)
}

// Handle processes the trace.get tool request.
func (h *TraceGetHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	traceID, err := request.RequireString("trace_id")
	if err != nil {
		return mcp.NewToolResultError("trace_id is required"), nil
	}

	project, errResult := requireProject(request, h.client.Config().DefaultProject)
	if errResult != nil {
		return errResult, nil
	}

	result, err := gcpdata.GetTrace(ctx, h.client.TraceClient(), project, traceID)
	if err != nil {
		mcpLog(ctx, mcp.LoggingLevelError, "trace.get", fmt.Sprintf("get trace %s failed: %v", traceID, err))
		return mcp.NewToolResultError(fmt.Sprintf("Failed to get trace: %v. Verify the trace_id is a valid 32-character hex string. Use logs.find_requests to discover valid trace IDs.", err)), nil
	}

	return jsonResult(result)
}
