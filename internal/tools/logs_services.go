package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func RegisterLogsServices(s *mcp.Server, d Deps) {
	requireClient(d.Client)
	mcp.AddTool(s, &mcp.Tool{
		Name: "logs_services",
		Description: applyMode(d.Mode, "List available services and resources in the project by scanning recent logs. "+
			"Discovers Kubernetes containers, Cloud Run, Cloud Functions, App Engine, and Compute Engine instances. "+
			"Useful as a first step to discover services before querying their logs. "+
			"Returns service names you can use as filters in logs_k8s or logs_query."),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		OutputSchema: outputSchemaFor[gcpdata.ServiceList](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in LogsServicesInput) (*mcp.CallToolResult, *gcpdata.ServiceList, error) {
		project, err := resolveProject(in.ProjectID, d.Client.Config().DefaultProject)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		timeFilter, err := buildTimeFilter(in.TimeFilterInput)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		sendProgress(ctx, req, 0, 1, "Discovering services...")

		result, err := gcpdata.ListServices(ctx, d.Client.LoggingClient(), project, timeFilter)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "logs_services", fmt.Sprintf("list services failed for project %s: %v", project, err))
			return errResult(fmt.Sprintf("Failed to list services: %v. Verify the project_id and that Cloud Logging API is enabled.", err)), nil, nil
		}

		return nil, result, nil
	})
}
