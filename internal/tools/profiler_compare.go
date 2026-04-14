package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func RegisterProfilerCompare(s *mcp.Server, client *gcpclient.Client, cache *gcpdata.ProfileCache) {
	requireClient(client)
	mcp.AddTool(s, &mcp.Tool{
		Name: "profiler_compare",
		Description: "Compare two profiles and identify regressions and improvements. " +
			"Takes a current profile_id and a base_profile_id, computes the diff, and returns a summary. " +
			"The returned diff_id can be used with profiler_top, profiler_peek, and profiler_flamegraph " +
			"to navigate the diff — positive values mean regression, negative mean improvement. " +
			"Useful for before/after deploy comparisons and regression hunting.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		OutputSchema: outputSchemaFor[gcpdata.ProfileCompareResult](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ProfilerCompareInput) (*mcp.CallToolResult, *gcpdata.ProfileCompareResult, error) {
		if in.ProfileID == "" {
			return errResult("profile_id is required (current profile)"), nil, nil
		}
		if in.ValueIndex < 0 {
			return errResult("value_index must be non-negative"), nil, nil
		}
		if in.BaseProfileID == "" {
			return errResult("base_profile_id is required (base profile to compare against)"), nil, nil
		}
		if strings.HasPrefix(in.ProfileID, "diff:") || strings.HasPrefix(in.BaseProfileID, "diff:") {
			return errResult("profile_id and base_profile_id must be real profile IDs from profiler_list, not diff_ids from profiler_compare"), nil, nil
		}
		project, err := resolveProject(in.ProjectID, client.Config().DefaultProject)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		sendProgress(ctx, req, 0, 2, "Comparing profiles...")

		result, diffProfile, err := gcpdata.CompareProfiles(ctx, client.ProfilerService(), cache, project,
			in.ProfileID, in.BaseProfileID, in.ValueIndex, 10)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "profiler_compare", fmt.Sprintf("compare profiles failed: %v", err))
			return errResult(fmt.Sprintf("Failed to compare profiles: %v", err)), nil, nil
		}

		sendProgress(ctx, req, 1, 2, "Building diff profile...")

		if result.Warning != "" {
			mcpLog(ctx, req, logLevelWarning, "profiler_compare", result.Warning)
		}

		// Cache the diff profile so top/peek/flamegraph can use it via diff_id.
		if diffProfile != nil {
			diffMeta := gcpdata.ProfileMeta{
				ProfileID:   result.DiffID,
				ProfileType: result.CurrentMeta.ProfileType,
				Target:      result.CurrentMeta.Target,
				IsDiff:      true,
			}
			cache.Put(project+"/"+result.DiffID, diffProfile, diffMeta)
		}

		return nil, result, nil
	})
}
