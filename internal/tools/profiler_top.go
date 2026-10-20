package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func RegisterProfilerTop(s *mcp.Server, d Deps) {
	requireProfiler(d.Profiler)
	mcp.AddTool(s, &mcp.Tool{
		Name: "profiler_top",
		Description: applyMode(d.Mode, "Show top functions from a profile ranked by resource consumption (like pprof top). "+
			"Returns a flat ranking of functions by self or cumulative cost. "+
			"Use profile_id from profiler_list; add base_profile_id to analyze a request-local diff. "+
			"Start here to identify hotspots, then use profiler_peek for caller/callee context. "+
			"For multi-value profiles (e.g. HEAP with alloc_space and alloc_objects), check available_values in the response."),
		Annotations: readOnlyAnnotations,
		InputSchema: projectInputSchema[ProfilerTopInput](d.Project,
			nonEmptyProp("profile_id"),
			nonNegativeValueIndex,
			enumProp("sort_by", profileSortBys, defaultProfileSortBy),
		),
		OutputSchema: outputSchemaFor[gcpdata.ProfileTopResult](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ProfilerTopInput) (*mcp.CallToolResult, *gcpdata.ProfileTopResult, error) {
		project, err := d.Project.Resolve(in.ProjectID)
		if err != nil {
			return ErrorResult(err.Error()), nil, nil
		}
		progress, stop := profileNavigationProgress(ctx, req)
		defer stop()
		result, err := d.Profiler.TopProfile(ctx, gcpdata.TopProfileParams{
			Project: project, ProfileID: in.ProfileID, BaseProfileID: in.BaseProfileID,
			ValueIndex: in.ValueIndex, Limit: in.Limit, SortBy: in.SortBy, Filter: in.Filter,
			OnProgress: progress,
		})
		if err != nil {
			return profileNavigationErrorResult(err, "Failed to analyze profile:"), nil, nil
		}
		return nil, result, nil
	})
}
