package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func RegisterProfilerTrends(s *mcp.Server, client *gcpclient.Client, cache *gcpdata.ProfileCache) {
	requireClient(client)
	mcp.AddTool(s, &mcp.Tool{
		Name: "profiler_trends",
		Description: "Show how function costs change over time across multiple profiles (Profile history). " +
			"Analyzes multiple profiles of the same type and target to build a time series of " +
			"self and cumulative cost for top functions. Useful for detecting performance regressions " +
			"or improvements over time. Both profile_type and target are required. " +
			"Use function_filter to focus on specific functions from profiler_top results.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		InputSchema: inputSchemaWithEnums[ProfilerTrendsInput](
			enumPatch{"profile_type", enumProfileType},
		),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ProfilerTrendsInput) (*mcp.CallToolResult, *gcpdata.ProfileTrendsResult, error) {
		if in.ProfileType == "" {
			return errResult("profile_type is required (e.g. CPU, HEAP, WALL)"), nil, nil
		}
		if in.ValueIndex < 0 {
			return errResult("value_index must be non-negative"), nil, nil
		}
		if in.Target == "" {
			return errResult("target is required (service name from profiler_list results)"), nil, nil
		}
		project, err := resolveProject(in.ProjectID, client.Config().DefaultProject)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		if err := gcpdata.ValidateProfileType(in.ProfileType); err != nil {
			return errResult(err.Error()), nil, nil
		}

		maxProfiles := clampLimit(in.MaxProfiles, 30, 100)
		maxFunctions := clampLimit(in.MaxFunctions, 10, 20)

		progressFn := func(current, total int, msg string) {
			sendProgress(ctx, req, float64(current), float64(total), msg)
		}

		result, err := gcpdata.ComputeTrends(ctx, client.ProfilerService(), cache, project,
			in.ProfileType, in.Target, in.FunctionFilter,
			in.ValueIndex, maxProfiles, maxFunctions, progressFn)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "profiler_trends", fmt.Sprintf("compute trends failed: %v", err))
			return errResult(fmt.Sprintf("Failed to compute trends: %v", err)), nil, nil
		}

		return nil, result, nil
	})
}
