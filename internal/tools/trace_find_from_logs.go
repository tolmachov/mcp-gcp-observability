package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

// maxTraceFindResults caps the number of distinct traces trace_find_from_logs returns.
const maxTraceFindResults = 100

func RegisterTraceFindFromLogs(s *mcp.Server, d Deps) {
	requireLogs(d.Logs)
	mcp.AddTool(s, &mcp.Tool{
		Name: "trace_find_from_logs",
		Description: applyMode(d.Mode, "Discover traces by scanning logs that match a Cloud Logging filter. "+
			"Groups the matching entries by trace ID and returns distinct traces with log volume, severity, service, and a sample message. "+
			"Bridges the 'found an error in logs -> inspect its trace' workflow: filter for the logs you care about, then pivot to trace_get or logs_by_trace on the returned IDs. "+
			"Unlike logs_find_requests (HTTP requests only), this works with any log filter."),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		OutputSchema: outputSchemaFor[gcpdata.TraceFromLogsList](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in TraceFindFromLogsInput) (*mcp.CallToolResult, *gcpdata.TraceFromLogsList, error) {
		if in.Filter == "" {
			return errResult("filter is required"), nil, nil
		}

		project, err := resolveProject(in.ProjectID, d.DefaultProject)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		scanLimit := clampLimit(in.ScanLimit, 500, d.LogsMaxLimit)
		resultLimit := clampLimit(in.Limit, 20, maxTraceFindResults)

		timeFilter, err := buildTimeFilter(in.TimeFilterInput)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		sendProgress(ctx, req, 0, 1, "Scanning logs for traces...")

		result, err := d.Logs.FindTracesFromLogs(ctx, project, in.Filter, timeFilter, scanLimit, resultLimit)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "trace_find_from_logs", fmt.Sprintf("find traces from logs failed: %v", err))
			return errResult(fmt.Sprintf("Failed to find traces from logs: %v. Verify the project_id and filter syntax.", err)), nil, nil
		}
		if result.Truncated && result.TruncationHint != "" {
			mcpLog(ctx, req, logLevelWarning, "trace_find_from_logs", result.TruncationHint)
		}

		return nil, result, nil
	})
}
