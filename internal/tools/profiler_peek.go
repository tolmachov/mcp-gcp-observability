package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func RegisterProfilerPeek(s *mcp.Server, d Deps) {
	requireProfiler(d.Profiler)
	mcp.AddTool(s, &mcp.Tool{
		Name: "profiler_peek",
		Description: applyMode(d.Mode, "Show callers and callees of a specific function in a profile (like pprof peek). "+
			"Navigates the call graph: who calls this function, and what does it call? "+
			"Use function names from profiler_top results. Substring matching is used. "+
			"If the name is ambiguous, the error will list matching candidates — use a more specific name. "+
			"Add base_profile_id to inspect a request-local diff."),
		Annotations: readOnlyAnnotations,
		InputSchema: projectInputSchema[ProfilerPeekInput](d.Project,
			nonEmptyProp("profile_id"),
			nonEmptyProp("function_name"),
			nonNegativeValueIndex,
		),
		OutputSchema: outputSchemaFor[gcpdata.ProfilePeekResult](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ProfilerPeekInput) (*mcp.CallToolResult, *gcpdata.ProfilePeekResult, error) {
		project, err := d.Project.Resolve(in.ProjectID)
		if err != nil {
			return ErrorResult(err.Error()), nil, nil
		}
		progress, stop := profileNavigationProgress(ctx, req)
		defer stop()
		result, err := d.Profiler.PeekProfile(ctx, gcpdata.PeekProfileParams{
			Project: project, ProfileID: in.ProfileID, BaseProfileID: in.BaseProfileID,
			FunctionName: in.FunctionName, ValueIndex: in.ValueIndex, Limit: in.Limit,
			OnProgress: progress,
		})
		if err != nil {
			return profileNavigationErrorResult(err, "Failed to peek function:"), nil, nil
		}
		return nil, result, nil
	})
}
