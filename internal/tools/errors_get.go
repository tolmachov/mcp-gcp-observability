package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func RegisterErrorsGet(s *mcp.Server, d Deps) {
	requireClient(d.Client)
	mcp.AddTool(s, &mcp.Tool{
		Name: "errors_get",
		Description: applyMode(d.Mode, "Get details for a specific error group, including individual error events, reported stack traces/messages, and structured context when available. "+
			"Requires a group_id from errors_list results. "+
			"Returns all recent events for the group (time filtering is not supported for individual error events)."),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		OutputSchema: outputSchemaFor[gcpdata.ErrorGroupDetail](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ErrorsGetInput) (*mcp.CallToolResult, *gcpdata.ErrorGroupDetail, error) {
		if in.GroupID == "" {
			return errResult("group_id is required"), nil, nil
		}
		project, err := resolveProject(in.ProjectID, d.Client.Config().DefaultProject)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		limit := clampLimit(in.Limit, 20, d.Client.Config().ErrorsMaxLimit)

		sendProgress(ctx, req, 0, 1, "Fetching error group details...")

		result, err := gcpdata.GetErrorGroup(ctx, d.Client.ErrorsClient(), project, in.GroupID, limit, in.PageToken)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "errors_get", fmt.Sprintf("get error group %s failed: %v", in.GroupID, err))
			return errResult(fmt.Sprintf("Failed to get error group: %v. Verify the group_id is valid — use errors_list to find available group IDs.", err)), nil, nil
		}

		return nil, result, nil
	})
}
