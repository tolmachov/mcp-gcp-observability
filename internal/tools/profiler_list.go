package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func RegisterProfilerList(s *mcp.Server, d Deps) {
	requireClient(d.Client)
	mcp.AddTool(s, &mcp.Tool{
		Name: "profiler_list",
		Description: applyMode(d.Mode, "List available Cloud Profiler profiles with metadata. "+
			"Returns profile IDs, types, deployment targets, and timestamps — no profile data. "+
			"Use the returned profile_id with profiler_top to start analyzing a profile. "+
			"Supports filtering by profile_type (CPU, WALL, HEAP, THREADS, CONTENTION, PEAK_HEAP, HEAP_ALLOC) and target (service name). "+
			"Requires Cloud Profiler API to be enabled."),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		InputSchema: inputSchemaWithEnums[ProfilerListInput](
			enumPatch{"profile_type", enumProfileType},
		),
		OutputSchema: outputSchemaFor[gcpdata.ProfileListResult](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ProfilerListInput) (*mcp.CallToolResult, *gcpdata.ProfileListResult, error) {
		project, err := resolveProject(in.ProjectID, d.Client.Config().DefaultProject)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		if err := gcpdata.ValidateProfileType(in.ProfileType); err != nil {
			return errResult(err.Error()), nil, nil
		}

		pageSize := clampLimit(in.Limit, 20, 100)

		sendProgress(ctx, req, 0, 1, "Listing profiles...")

		result, err := gcpdata.ListProfiles(ctx, d.Client.ProfilerService(), project,
			in.ProfileType, in.Target, in.StartTime, in.EndTime, pageSize, in.PageToken)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "profiler_list", fmt.Sprintf("list profiles failed: %v", err))
			return errResult(fmt.Sprintf("Failed to list profiles: %v. Verify the project_id and that Cloud Profiler API is enabled.", err)), nil, nil
		}

		return nil, result, nil
	})
}
