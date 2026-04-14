package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func RegisterLogsByRequestID(s *mcp.Server, client *gcpclient.Client) {
	requireClient(client)
	mcp.AddTool(s, &mcp.Tool{
		Name: "logs_by_request_id",
		Description: "Find all log entries associated with a specific request ID. " +
			"Matches common structured-log fields such as jsonPayload.request_id, jsonPayload.requestId, labels.request_id, and labels.requestId. " +
			"Returns logs sorted by timestamp ascending to show the full request lifecycle. " +
			"Get request IDs from logs_find_requests results. " +
			"If you have a trace ID instead, use logs_by_trace or trace_get.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in LogsByRequestIDInput) (*mcp.CallToolResult, *gcpdata.LogQueryResult, error) {
		if in.RequestID == "" {
			return errResult("request_id is required"), nil, nil
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

		sendProgress(ctx, req, 0, 1, "Querying logs by request ID...")

		result, err := gcpdata.QueryLogsByRequestID(ctx, client.LoggingClient(), project, in.RequestID, timeFilter, limit, in.PageToken)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "logs_by_request_id", fmt.Sprintf("request ID query failed for %s: %v", in.RequestID, err))
			return errResult(fmt.Sprintf("Failed to query logs by request ID: %v. Verify the request_id is correct. Use logs_find_requests to discover valid request IDs.", err)), nil, nil
		}

		return nil, result, nil
	})
}
