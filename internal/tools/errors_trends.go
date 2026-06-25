package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func RegisterErrorsTrends(s *mcp.Server, d Deps) {
	requireClient(d.Client)
	mcp.AddTool(s, &mcp.Tool{
		Name: "errors_trends",
		Description: applyMode(d.Mode, "Analyze how error groups changed over a lookback window from Google Cloud Error Reporting. "+
			"Splits the window into an older and a recent half and classifies each group as new, growing, shrinking, disappeared, or flat. "+
			"Returns groups sorted by delta (most worsened first) plus a summary count per category, so you can spot regressions and newly appearing errors at a glance. "+
			"Use errors_list for a point-in-time snapshot and errors_get with a group_id to drill into individual events. "+
			"Note: Error Reporting supports only lookback periods ending now, so the comparison is intra-window (first half vs second half), not against an arbitrary historical baseline."),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		OutputSchema: outputSchemaFor[gcpdata.ErrorTrendList](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ErrorsListInput) (*mcp.CallToolResult, *gcpdata.ErrorTrendList, error) {
		project, err := resolveProject(in.ProjectID, d.Client.Config().DefaultProject)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		timeRangeHours, err := resolveErrorsTimeRange(in)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		limit := clampLimit(in.Limit, 50, d.Client.Config().ErrorsMaxLimit)

		sendProgress(ctx, req, 0, 1, "Analyzing error trends...")

		result, err := gcpdata.AnalyzeErrorTrends(ctx, d.Client.ErrorsClient(), project, timeRangeHours, limit, in.ServiceFilter, in.VersionFilter)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "errors_trends", fmt.Sprintf("analyze error trends failed for project %s: %v", project, err))
			return errResult(fmt.Sprintf("Failed to analyze error trends: %v. Verify the project_id and that Error Reporting API is enabled.", err)), nil, nil
		}

		return nil, result, nil
	})
}
