package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func RegisterLogsK8s(s *mcp.Server, client *gcpclient.Client) {
	requireClient(client)
	mcp.AddTool(s, &mcp.Tool{
		Name: "logs_k8s",
		Description: "Query Kubernetes container logs with convenient filters. " +
			"Automatically builds Cloud Logging filter for resource.type=\"k8s_container\". " +
			"Preferred over logs_query for K8s workloads. Results default to newest-first (use order parameter to change).",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		InputSchema: inputSchemaWithEnums[LogsK8sInput](
			enumPatch{"severity", enumSeverity},
			enumPatch{"order", enumSortOrder},
		),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in LogsK8sInput) (*mcp.CallToolResult, *gcpdata.LogQueryResult, error) {
		project, err := resolveProject(in.ProjectID, client.Config().DefaultProject)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		limit := clampLimit(in.Limit, 100, client.Config().LogsMaxLimit)

		// Build K8s-specific filter
		parts := []string{`resource.type="k8s_container"`}

		if in.Namespace != "" {
			parts = append(parts, fmt.Sprintf(`resource.labels.namespace_name="%s"`, gcpdata.EscapeFilterValue(in.Namespace)))
		}
		if in.PodName != "" {
			parts = append(parts, fmt.Sprintf(`resource.labels.pod_name:"%s"`, gcpdata.EscapeFilterValue(in.PodName)))
		}
		if in.ContainerName != "" {
			parts = append(parts, fmt.Sprintf(`resource.labels.container_name="%s"`, gcpdata.EscapeFilterValue(in.ContainerName)))
		}
		if in.Severity != "" {
			severity := strings.ToUpper(in.Severity)
			if !gcpdata.IsValidSeverity(severity) {
				return errResult(fmt.Sprintf("invalid severity %q: must be one of DEFAULT, DEBUG, INFO, NOTICE, WARNING, ERROR, CRITICAL, ALERT, EMERGENCY", in.Severity)), nil, nil
			}
			parts = append(parts, fmt.Sprintf(`severity>=%s`, severity))
		}
		if in.TextSearch != "" {
			escaped := gcpdata.EscapeFilterValue(in.TextSearch)
			parts = append(parts, fmt.Sprintf(`(textPayload:"%s" OR jsonPayload.message:"%s")`, escaped, escaped))
		}

		filter := strings.Join(parts, "\n")

		timeFilter, err := buildTimeFilter(in.TimeFilterInput)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		filter = gcpdata.AppendFilter(filter, timeFilter)

		order := in.Order
		if order == "" {
			order = "desc"
		}
		if order != "asc" && order != "desc" {
			return errResult(fmt.Sprintf("invalid order %q: must be \"asc\" or \"desc\"", order)), nil, nil
		}

		sendProgress(ctx, req, 0, 1, "Querying Kubernetes logs...")

		result, err := gcpdata.QueryLogs(ctx, client.LoggingClient(), project, filter, limit, order, in.PageToken)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "logs_k8s", fmt.Sprintf("query failed for project %s: %v", project, err))
			return errResult(fmt.Sprintf("Failed to query K8s logs: %v. Verify the project_id and that K8s logging is enabled.", err)), nil, nil
		}

		return nil, result, nil
	})
}
