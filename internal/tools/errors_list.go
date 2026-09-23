package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func RegisterErrorsList(s *mcp.Server, d Deps) {
	requireErrors(d.Errors)
	mcp.AddTool(s, &mcp.Tool{
		Name: "errors_list",
		Description: applyMode(d.Mode, "List error groups from Google Cloud Error Reporting, sorted by occurrence count. "+
			"Returns aggregated errors with group IDs, not individual log entries. "+
			"Use errors_get with a group_id from these results to see individual error events and stack traces. "+
			"The window is one exact Error Reporting period ending at now: 1h, 6h, 24h, 7d, or 30d."),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		InputSchema: projectInputSchema[ErrorsListInput](d.Project,
			enumProp("window", gcpdata.ErrorWindows()),
		),
		OutputSchema: outputSchemaFor[gcpdata.ErrorGroupList](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ErrorsListInput) (*mcp.CallToolResult, *gcpdata.ErrorGroupList, error) {
		project, err := d.Project.Resolve(in.ProjectID)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		window := errorsWindowOrDefault(in.Window)

		limit := clampLimit(in.Limit, 50, ErrorsHardLimit)

		sendProgress(ctx, req, 0, 1, "Listing error groups...")

		result, err := d.Errors.ListErrors(ctx, project, window, limit, in.ServiceFilter, in.VersionFilter)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "errors_list", fmt.Sprintf("list errors failed for project %s: %v", project, err))
			return errResult(fmt.Sprintf("Failed to list errors: %v. Verify the project_id and that Error Reporting API is enabled.", err)), nil, nil
		}

		return nil, result, nil
	})
}

// errorsWindowOrDefault applies the default window to the schema-validated
// window input.
func errorsWindowOrDefault(raw string) gcpdata.ErrorWindow {
	if raw == "" {
		return gcpdata.ErrorWindow24H
	}
	return gcpdata.ErrorWindow(raw)
}
