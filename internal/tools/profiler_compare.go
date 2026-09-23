package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func RegisterProfilerCompare(s *mcp.Server, d Deps) {
	requireProfiler(d.Profiler)
	mcp.AddTool(s, &mcp.Tool{
		Name: "profiler_compare",
		Description: applyMode(d.Mode, "Compare two profiles and identify regressions and improvements. "+
			"Takes a current profile_id and a base_profile_id, computes the diff, and returns a summary. "+
			"Pass the same profile_id and base_profile_id to profiler_top, profiler_peek, or profiler_flamegraph "+
			"to recompute and navigate the diff in one stateless request. "+
			"Useful for before/after deploy comparisons and regression hunting."),
		Annotations: readOnlyAnnotations,
		InputSchema: projectInputSchema[ProfilerCompareInput](d.Project,
			nonEmptyProp("profile_id"),
			nonEmptyProp("base_profile_id"),
			nonNegativeValueIndex,
		),
		OutputSchema: outputSchemaFor[gcpdata.ProfileCompareResult](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ProfilerCompareInput) (*mcp.CallToolResult, *gcpdata.ProfileCompareResult, error) {
		project, err := d.Project.Resolve(in.ProjectID)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		// CompareProfiles fetches two profiles, each of which may scan the Export
		// API and run long on large projects; heartbeat progress keeps the client
		// request alive across both fetches.
		stopHeartbeat := startProgressHeartbeat(ctx, req, "Comparing profiles…")
		result, err := d.Profiler.CompareProfiles(ctx, project,
			in.ProfileID, in.BaseProfileID, in.ValueIndex, 10)
		stopHeartbeat()
		if err != nil {
			mcpLog(ctx, req, logLevelError, "profiler_compare", fmt.Sprintf("compare profiles failed: %v", err))
			return gcpErrorResult(fmt.Sprintf("Failed to compare profiles: %v", err), err, ""), nil, nil
		}

		if result.Warning != "" {
			mcpLog(ctx, req, logLevelWarning, "profiler_compare", result.Warning)
		}

		return nil, result, nil
	})
}
