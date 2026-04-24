package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func RegisterLogsByTrace(s *mcp.Server, d Deps) {
	requireClient(d.Client)
	mcp.AddTool(s, &mcp.Tool{
		Name: "logs_by_trace",
		Description: applyMode(d.Mode, "Find all log entries associated with a specific trace ID. "+
			"Returns logs sorted by timestamp ascending to show the request flow. "+
			"Get trace IDs from logs_find_requests results or from the trace field in logs_query output. "+
			"If you have a request_id instead of a trace_id, use logs_by_request_id."),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		OutputSchema: outputSchemaFor[gcpdata.LogQueryResult](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in LogsByTraceInput) (*mcp.CallToolResult, *gcpdata.LogQueryResult, error) {
		if in.TraceID == "" {
			return errResult("trace_id is required"), nil, nil
		}
		project, err := resolveProject(in.ProjectID, d.Client.Config().DefaultProject)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		limit := clampLimit(in.Limit, 100, d.Client.Config().LogsMaxLimit)

		timeFilter, err := buildTimeFilter(in.TimeFilterInput)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		sendProgress(ctx, req, 0, 1, "Querying logs by trace...")

		result, err := gcpdata.QueryLogsByTrace(ctx, d.Client.LoggingClient(), project, in.TraceID, timeFilter, limit, in.PageToken)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "logs_by_trace", fmt.Sprintf("trace query failed for %s: %v", in.TraceID, err))
			return errResult(fmt.Sprintf("Failed to query logs by trace: %v. Verify the trace_id format (hex string, not full resource path). Use logs_find_requests to discover valid trace IDs.", err)), nil, nil
		}

		return nil, result, nil
	})
}
