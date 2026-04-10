package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func RegisterErrorsList(s *mcp.Server, client *gcpclient.Client) {
	requireClient(client)
	mcp.AddTool(s, &mcp.Tool{
		Name: "errors.list",
		Description: "List error groups from Google Cloud Error Reporting, sorted by occurrence count. " +
			"Returns aggregated errors with group IDs, not individual log entries. " +
			"Use errors.get with a group_id from these results to see individual error events and stack traces. " +
			"Time range is specified via time_range_hours only. " +
			"Note: Error Reporting supports only lookback periods ending at now (1h, 6h, 1d, 1w, 30d), not arbitrary historical start/end timestamps.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  ptrTrue(),
			IdempotentHint: true,
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ErrorsListInput) (*mcp.CallToolResult, *gcpdata.ErrorGroupList, error) {
		project, err := resolveProject(in.ProjectID, client.Config().DefaultProject)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		timeRangeHours, err := resolveErrorsTimeRange(in)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		limit := clampLimit(in.Limit, 50, client.Config().ErrorsMaxLimit)

		result, err := gcpdata.ListErrors(ctx, client.ErrorsClient(), project, timeRangeHours, limit, in.ServiceFilter, in.VersionFilter)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "errors.list", fmt.Sprintf("list errors failed for project %s: %v", project, err))
			return errResult(fmt.Sprintf("Failed to list errors: %v. Verify the project_id and that Error Reporting API is enabled.", err)), nil, nil
		}

		return nil, result, nil
	})
}

// resolveErrorsTimeRange returns the lookback range in hours for Error Reporting.
func resolveErrorsTimeRange(in ErrorsListInput) (int, error) {
	hours := in.TimeRangeHours
	if hours == 0 {
		hours = 24
	}
	if hours < 1 || hours > 720 {
		return 0, fmt.Errorf("time_range_hours must be between 1 and 720, got %d", hours)
	}
	return hours, nil
}
