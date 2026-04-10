package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func RegisterLogsByTrace(s *mcp.Server, client *gcpclient.Client) {
	requireClient(client)
	mcp.AddTool(s, &mcp.Tool{
		Name: "logs.by_trace",
		Description: "Find all log entries associated with a specific trace ID. " +
			"Returns logs sorted by timestamp ascending to show the request flow. " +
			"Get trace IDs from logs.find_requests results or from the trace field in logs.query output. " +
			"If you have a request_id instead of a trace_id, use logs.by_request_id.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  ptrTrue(),
			IdempotentHint: true,
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in LogsByTraceInput) (*mcp.CallToolResult, *gcpdata.LogQueryResult, error) {
		if in.TraceID == "" {
			return errResult("trace_id is required"), nil, nil
		}
		project, err := resolveProject(in.ProjectID, client.Config().DefaultProject)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		limit := clampLimit(in.Limit, 100, client.Config().LogsMaxLimit)

		timeFilter, err := buildTimeFilter(in.TimeFilterInput)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		result, err := gcpdata.QueryLogsByTrace(ctx, client.LoggingClient(), project, in.TraceID, timeFilter, limit, in.PageToken)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "logs.by_trace", fmt.Sprintf("trace query failed for %s: %v", in.TraceID, err))
			return errResult(fmt.Sprintf("Failed to query logs by trace: %v. Verify the trace_id format (hex string, not full resource path). Use logs.find_requests to discover valid trace IDs.", err)), nil, nil
		}

		return nil, result, nil
	})
}
