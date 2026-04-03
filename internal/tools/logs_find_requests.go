package tools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

// LogsFindRequestsHandler handles the logs.find_requests tool.
type LogsFindRequestsHandler struct {
	client *gcpclient.Client
}

// NewLogsFindRequestsHandler creates a new LogsFindRequestsHandler.
func NewLogsFindRequestsHandler(client *gcpclient.Client) *LogsFindRequestsHandler {
	return &LogsFindRequestsHandler{client: requireClient(client)}
}

// Tool returns the MCP tool definition.
func (h *LogsFindRequestsHandler) Tool() mcp.Tool {
	return newToolWithTimeFilter("logs.find_requests",
		mcp.WithDescription("Find examples of HTTP requests by URL pattern. "+
			"Returns trace_id and request_id for each request, enabling deeper investigation with logs.by_trace, logs.by_request_id, or trace.get."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithString("url_pattern",
			mcp.Description("URL substring to match (e.g. '/api/profile', '/v1/connect')"),
			mcp.Required(),
		),
		mcp.WithString("method",
			mcp.Description("HTTP method filter"),
			mcp.Enum("GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"),
		),
		mcp.WithNumber("status_code",
			mcp.Description("HTTP status code filter (e.g. 500, 404)"),
			mcp.Min(100),
			mcp.Max(599),
		),
		mcp.WithBoolean("traced_only",
			mcp.Description("Only return requests that have a trace_id (default false)"),
		),
		mcp.WithString("project_id",
			mcp.Description("GCP project ID (uses default if not specified)"),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of requests to return (default 20, server max applies)"),
			mcp.Min(1),
		),
	)
}

// Handle processes the logs.find_requests tool request.
func (h *LogsFindRequestsHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	urlPattern, err := request.RequireString("url_pattern")
	if err != nil {
		return mcp.NewToolResultError("url_pattern is required"), nil
	}

	project, errResult := requireProject(request, h.client.Config().DefaultProject)
	if errResult != nil {
		return errResult, nil
	}
	method := request.GetString("method", "")
	statusCode := request.GetInt("status_code", 0)
	tracedOnly := request.GetBool("traced_only", false)
	limit := clampLimit(request.GetInt("limit", 20), 20, h.client.Config().LogsMaxLimit)

	timeFilter, err := buildTimeFilter(request)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	result, err := gcpdata.FindRequests(ctx, h.client.LoggingClient(), project, urlPattern, method, statusCode, tracedOnly, timeFilter, limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to find requests: %v. Verify the project_id and that the URL pattern is correct.", err)), nil
	}

	return jsonResult(result)
}
