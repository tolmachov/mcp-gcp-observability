package tools

import (
	"context"
	"fmt"

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
		limit := clampLimit(in.Limit, 20, 50)

		p, meta, errRes := loadProfile(ctx, req, d, "profiler_top", in.ProjectID, in.ProfileID, in.BaseProfileID)
		if errRes != nil {
			return errRes, nil, nil
		}

		sendProgress(ctx, req, 1, 2, "Analyzing profile...")

		topFuncs, total, truncated, err := gcpdata.TopFunctions(p, in.ValueIndex, limit, in.SortBy, in.Filter)
		if err != nil {
			mcpLog(ctx, req, logLevelWarning, "profiler_top", fmt.Sprintf("analysis failed: %v", err))
			return errResult(fmt.Sprintf("Failed to analyze profile: %v", err)), nil, nil
		}
		// TopFunctions validated in.ValueIndex against the profile's value types.
		vt := gcpdata.ProfileValueTypes(p)

		result := &gcpdata.ProfileTopResult{
			ProfileMeta:     meta,
			ValueType:       vt[in.ValueIndex],
			AvailableValues: vt,
			TotalValue:      total,
			TopFunctions:    topFuncs,
			Truncated:       truncated,
		}
		if truncated {
			result.TruncationHint = fmt.Sprintf("Showing top %d functions. Use filter to narrow results or increase limit (max 50).", limit)
		}

		return nil, result, nil
	})
}
