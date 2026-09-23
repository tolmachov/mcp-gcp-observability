package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func RegisterLogsSummary(s *mcp.Server, d Deps) {
	requireLogs(d.Logs)
	mcp.AddTool(s, &mcp.Tool{
		Name: "logs_summary",
		Description: applyMode(d.Mode, "Get an aggregated summary of logs (based on up to 200 sampled entries): severity distribution, top services, top errors, and sample entries. "+
			"Useful for initial triage before drilling down with logs_query or logs_k8s. "+
			"Does NOT return full log entries — use logs_query for that."),
		Annotations:  readOnlyAnnotations,
		InputSchema:  projectInputSchema[LogsSummaryInput](d.Project),
		OutputSchema: outputSchemaFor[gcpdata.LogsSummary](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in LogsSummaryInput) (*mcp.CallToolResult, *gcpdata.LogsSummary, error) {
		project, err := d.Project.Resolve(in.ProjectID)
		if err != nil {
			return ErrorResult(err.Error()), nil, nil
		}
		filter := in.Filter

		timeFilter, err := buildTimeFilter(in.TimeFilterInput)
		if err != nil {
			return ErrorResult(err.Error()), nil, nil
		}
		filter = gcpdata.AppendFilter(filter, timeFilter)

		sendProgress(ctx, req, 0, LogsHardLimit, "Scanning log entries")

		result, err := d.Logs.SummarizeLogs(ctx, project, filter,
			func(scanned, total int) {
				sendProgress(ctx, req, float64(scanned), float64(total),
					fmt.Sprintf("Scanned %d/%d entries", scanned, total))
			})
		if err != nil {
			mcpLog(ctx, req, logLevelError, "logs_summary", fmt.Sprintf("summarize failed for project %s: %v", project, err))
			return gcpErrorResult(fmt.Sprintf("Failed to summarize logs: %v", err), err, "Verify the project_id and filter syntax."), nil, nil
		}

		sendProgress(ctx, req, LogsHardLimit, LogsHardLimit, "Aggregating results")

		return nil, result, nil
	})
}
