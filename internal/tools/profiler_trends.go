package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/codes"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

// trendsDeadlineGuidance replaces the shared retry advice for a timeout: the
// scan ran out of its own time budget, which a retry of the same request would
// hit again.
var trendsDeadlineGuidance = codeGuidance{codes.DeadlineExceeded,
	"The trend scan exceeded its time budget, so retrying the same request will not help; lower max_profiles or pass function_filter."}

func RegisterProfilerTrends(s *mcp.Server, d Deps) {
	requireProfiler(d.Profiler)
	mcp.AddTool(s, &mcp.Tool{
		Name: "profiler_trends",
		Description: applyMode(d.Mode, "Show how function costs change over time across multiple profiles (Profile history). "+
			"Analyzes multiple profiles of the same type and target to build a time series of "+
			"self and cumulative cost for top functions. Useful for detecting performance regressions "+
			"or improvements over time. Both profile_type and target are required. "+
			"Use function_filter to focus on specific functions from profiler_top results."),
		Annotations: readOnlyAnnotations,
		InputSchema: projectInputSchema[ProfilerTrendsInput](d.Project,
			nonEmptyProp("target"),
			nonNegativeValueIndex,
			enumProp("profile_type", gcpdata.ProfileTypes, ""),
		),
		OutputSchema: outputSchemaFor[gcpdata.ProfileTrendsResult](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ProfilerTrendsInput) (*mcp.CallToolResult, *gcpdata.ProfileTrendsResult, error) {
		project, err := d.Project.Resolve(in.ProjectID)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		maxProfiles := clampLimit(in.MaxProfiles, 30, 100)
		maxFunctions := clampLimit(in.MaxFunctions, 10, 20)

		progressFn := func(current, total int, msg string) {
			sendProgress(ctx, req, float64(current), float64(total), msg)
		}

		result, err := d.Profiler.ComputeTrends(ctx, gcpdata.ComputeTrendsParams{
			Project:        project,
			ProfileType:    in.ProfileType,
			Target:         in.Target,
			FunctionFilter: in.FunctionFilter,
			ValueIndex:     in.ValueIndex,
			MaxProfiles:    maxProfiles,
			MaxFunctions:   maxFunctions,
		}, progressFn)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "profiler_trends", fmt.Sprintf("compute trends failed: %v", err))
			return gcpErrorResult(fmt.Sprintf("Failed to compute trends: %v", err), err, "", trendsDeadlineGuidance), nil, nil
		}

		return nil, result, nil
	})
}
