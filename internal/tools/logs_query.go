package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func RegisterLogsQuery(s *mcp.Server, d Deps) {
	requireLogs(d.Logs)
	mcp.AddTool(s, &mcp.Tool{
		Name: "logs_query",
		Description: applyMode(d.Mode, "Execute an arbitrary Cloud Logging query with full filter syntax. "+
			"Use Cloud Logging filter language (e.g. severity>=ERROR, resource.type=\"k8s_container\"). "+
			"For Kubernetes logs, prefer logs_k8s which builds filters automatically. "+
			"For initial triage, use logs_summary instead."),
		Annotations: readOnlyAnnotations,
		InputSchema: projectInputSchema[LogsQueryInput](d.Project,
			nonEmptyProp("filter"),
			enumProp("order", sortOrders),
		),
		OutputSchema: outputSchemaFor[gcpdata.LogQueryResult](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in LogsQueryInput) (*mcp.CallToolResult, *gcpdata.LogQueryResult, error) {
		project, err := d.Project.Resolve(in.ProjectID)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		limit := clampLimit(in.Limit, 100, LogsHardLimit)
		order := in.Order
		if order == "" {
			order = "desc"
		}

		timeFilter, err := buildTimeFilter(in.TimeFilterInput)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		filter := gcpdata.AppendFilter(in.Filter, timeFilter)

		sendProgress(ctx, req, 0, 1, "Querying logs...")

		result, err := d.Logs.QueryLogs(ctx, project, filter, limit, order, in.PageToken)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "logs_query", fmt.Sprintf("query failed for project %s: %v", project, err))
			return gcpErrorResult(fmt.Sprintf("Failed to query logs: %v", err), err, "Verify the project_id and filter syntax."), nil, nil
		}

		return nil, result, nil
	})
}
