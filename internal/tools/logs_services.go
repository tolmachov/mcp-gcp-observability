package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func RegisterLogsServices(s *mcp.Server, client *gcpclient.Client) {
	requireClient(client)
	mcp.AddTool(s, &mcp.Tool{
		Name: "logs.services",
		Description: "List available services and resources in the project by scanning recent logs. " +
			"Discovers Kubernetes containers, Cloud Run, Cloud Functions, App Engine, and Compute Engine instances. " +
			"Useful as a first step to discover services before querying their logs. " +
			"Returns service names you can use as filters in logs.k8s or logs.query.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in LogsServicesInput) (*mcp.CallToolResult, *gcpdata.ServiceList, error) {
		project, err := resolveProject(in.ProjectID, client.Config().DefaultProject)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		timeFilter, err := buildTimeFilter(in.TimeFilterInput)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		sendProgress(ctx, req, 0, 1, "Discovering services...")

		result, err := gcpdata.ListServices(ctx, client.LoggingClient(), project, timeFilter)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "logs.services", fmt.Sprintf("list services failed for project %s: %v", project, err))
			return errResult(fmt.Sprintf("Failed to list services: %v. Verify the project_id and that Cloud Logging API is enabled.", err)), nil, nil
		}

		return nil, result, nil
	})
}
