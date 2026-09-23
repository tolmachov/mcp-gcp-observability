package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func RegisterLogsFindRequests(s *mcp.Server, d Deps) {
	requireLogs(d.Logs)
	mcp.AddTool(s, &mcp.Tool{
		Name: "logs_find_requests",
		Description: applyMode(d.Mode, "Find examples of HTTP requests by URL pattern. "+
			"Returns trace_id and request_id for each request, enabling deeper investigation with logs_by_trace, logs_by_request_id, or trace_get."),
		Annotations: readOnlyAnnotations,
		InputSchema: projectInputSchema[LogsFindRequestsInput](d.Project,
			nonEmptyProp("url_pattern"),
			enumProp("method", httpMethods, ""),
		),
		OutputSchema: outputSchemaFor[gcpdata.RequestList](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in LogsFindRequestsInput) (*mcp.CallToolResult, *gcpdata.RequestList, error) {
		if in.StatusCode != 0 && (in.StatusCode < 100 || in.StatusCode > 599) {
			return errResult(fmt.Sprintf("invalid status_code %d: must be in range [100, 599]", in.StatusCode)), nil, nil
		}
		project, err := d.Project.Resolve(in.ProjectID)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		limit := clampLimit(in.Limit, 20, LogsHardLimit)

		timeFilter, err := buildTimeFilter(in.TimeFilterInput)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		sendProgress(ctx, req, 0, 1, "Finding requests...")

		result, err := d.Logs.FindRequests(ctx, gcpdata.FindRequestsParams{
			Project:    project,
			URLPattern: in.URLPattern,
			Method:     in.Method,
			StatusCode: in.StatusCode,
			TracedOnly: in.TracedOnly,
			TimeFilter: timeFilter,
			Limit:      limit,
		})
		if err != nil {
			mcpLog(ctx, req, logLevelError, "logs_find_requests", fmt.Sprintf("find requests failed: %v", err))
			return gcpErrorResult(fmt.Sprintf("Failed to find requests: %v", err), err, "Verify the project_id and that the URL pattern is correct."), nil, nil
		}

		return nil, result, nil
	})
}
