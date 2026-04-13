package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func RegisterLogsQuery(s *mcp.Server, client *gcpclient.Client) {
	requireClient(client)
	mcp.AddTool(s, &mcp.Tool{
		Name: "logs.query",
		Description: "Execute an arbitrary Cloud Logging query with full filter syntax. " +
			"Use Cloud Logging filter language (e.g. severity>=ERROR, resource.type=\"k8s_container\"). " +
			"For Kubernetes logs, prefer logs.k8s which builds filters automatically. " +
			"For initial triage, use logs.summary instead.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		InputSchema: inputSchemaWithEnums[LogsQueryInput](
			enumPatch{"order", enumSortOrder},
		),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in LogsQueryInput) (*mcp.CallToolResult, *gcpdata.LogQueryResult, error) {
		project, err := resolveProject(in.ProjectID, client.Config().DefaultProject)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		if in.Filter == "" {
			return errResult("filter is required"), nil, nil
		}
		limit := clampLimit(in.Limit, 100, client.Config().LogsMaxLimit)
		order := in.Order
		if order == "" {
			order = "desc"
		}
		if order != "asc" && order != "desc" {
			return errResult(fmt.Sprintf("invalid order %q: must be \"asc\" or \"desc\"", order)), nil, nil
		}

		timeFilter, err := buildTimeFilter(in.TimeFilterInput)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		filter := gcpdata.AppendFilter(in.Filter, timeFilter)

		sendProgress(ctx, req, 0, 1, "Querying logs...")

		result, err := gcpdata.QueryLogs(ctx, client.LoggingClient(), project, filter, limit, order, in.PageToken)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "logs.query", fmt.Sprintf("query failed for project %s: %v", project, err))
			return errResult(fmt.Sprintf("Failed to query logs: %v. Verify the project_id and filter syntax.", err)), nil, nil
		}

		return nil, result, nil
	})
}
