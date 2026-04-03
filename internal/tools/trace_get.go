package tools

import (
	"context"
	"encoding/json"
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
	return &TraceGetHandler{client: client}
}

// Tool returns the MCP tool definition.
func (h *TraceGetHandler) Tool() mcp.Tool {
	return mcp.NewTool("trace.get",
		mcp.WithDescription("Get trace details with all spans by trace ID. "+
			"Returns spans sorted by start time showing the full request execution tree. "+
			"Use trace IDs from logs.find_requests results or the trace field in logs.query output. "+
			"Requires Cloud Trace API to be enabled in the project."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithString("trace_id",
			mcp.Description("The trace ID (32-character hex string)"),
			mcp.Required(),
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

	project := request.GetString("project_id", h.client.Config.DefaultProject)

	result, err := gcpdata.GetTrace(ctx, h.client.Trace, project, traceID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to get trace: %v. Verify the trace_id is a valid 32-character hex string. Use logs.find_requests to discover valid trace IDs.", err)), nil
	}

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to marshal result: %v", err)), nil
	}

	return mcp.NewToolResultText(string(data)), nil
}
