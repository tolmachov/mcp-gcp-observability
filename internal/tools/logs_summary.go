package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func RegisterLogsSummary(s *mcp.Server, client *gcpclient.Client, mode RegistrationMode) {
	requireClient(client)
	mcp.AddTool(s, &mcp.Tool{
		Name: "logs_summary",
		Description: applyMode(mode, "Get an aggregated summary of logs (based on up to 1000 sampled entries): severity distribution, top services, top errors, and sample entries. "+
			"Useful for initial triage before drilling down with logs_query or logs_k8s. "+
			"Does NOT return full log entries — use logs_query for that."),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		OutputSchema: outputSchemaFor[gcpdata.LogsSummary](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in LogsSummaryInput) (*mcp.CallToolResult, *gcpdata.LogsSummary, error) {
		project, err := resolveProject(in.ProjectID, client.Config().DefaultProject)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		filter := in.Filter

		timeFilter, err := buildTimeFilter(in.TimeFilterInput)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		filter = gcpdata.AppendFilter(filter, timeFilter)

		sendProgress(ctx, req, 0, 1000, "Scanning log entries")

		result, err := gcpdata.SummarizeLogs(ctx, client.LoggingClient(), project, filter,
			func(scanned, total int) {
				sendProgress(ctx, req, float64(scanned), float64(total),
					fmt.Sprintf("Scanned %d/%d entries", scanned, total))
			})
		if err != nil {
			mcpLog(ctx, req, logLevelError, "logs_summary", fmt.Sprintf("summarize failed for project %s: %v", project, err))
			return errResult(fmt.Sprintf("Failed to summarize logs: %v. Verify the project_id and filter syntax.", err)), nil, nil
		}

		sendProgress(ctx, req, 1000, 1000, "Aggregating results")

		return nil, result, nil
	})
}
