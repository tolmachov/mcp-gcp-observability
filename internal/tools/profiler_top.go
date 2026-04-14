package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func RegisterProfilerTop(s *mcp.Server, client *gcpclient.Client, cache *gcpdata.ProfileCache) {
	requireClient(client)
	mcp.AddTool(s, &mcp.Tool{
		Name: "profiler_top",
		Description: "Show top functions from a profile ranked by resource consumption (like pprof top). " +
			"Returns a flat ranking of functions by self or cumulative cost. " +
			"Use profile_id from profiler_list results, or diff_id from profiler_compare. " +
			"Start here to identify hotspots, then use profiler_peek for caller/callee context. " +
			"For multi-value profiles (e.g. HEAP with alloc_space and alloc_objects), check available_values in the response.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		InputSchema: inputSchemaWithEnums[ProfilerTopInput](
			enumPatch{"sort_by", enumSortBy},
		),
		OutputSchema: outputSchemaFor[gcpdata.ProfileTopResult](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ProfilerTopInput) (*mcp.CallToolResult, *gcpdata.ProfileTopResult, error) {
		if in.ProfileID == "" {
			return errResult("profile_id is required"), nil, nil
		}
		if in.ValueIndex < 0 {
			return errResult("value_index must be non-negative"), nil, nil
		}
		project, err := resolveProject(in.ProjectID, client.Config().DefaultProject)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		limit := clampLimit(in.Limit, 20, 50)

		sendProgress(ctx, req, 0, 2, "Downloading profile...")

		p, meta, err := gcpdata.GetOrFetchProfile(ctx, client.ProfilerService(), cache, project, in.ProfileID)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "profiler_top", fmt.Sprintf("fetch profile failed: %v", err))
			return errResult(fmt.Sprintf("Failed to fetch profile: %v", err)), nil, nil
		}

		sendProgress(ctx, req, 1, 2, "Analyzing profile...")

		vt := gcpdata.ProfileValueTypes(p)
		if in.ValueIndex >= len(vt) {
			return errResult(fmt.Sprintf("value_index %d out of range (profile has %d value types)", in.ValueIndex, len(vt))), nil, nil
		}
		valueType := vt[in.ValueIndex]

		topFuncs, total, truncated, err := gcpdata.TopFunctions(p, in.ValueIndex, limit, in.SortBy, in.Filter)
		if err != nil {
			mcpLog(ctx, req, logLevelWarning, "profiler_top", fmt.Sprintf("analysis failed: %v", err))
			return errResult(fmt.Sprintf("Failed to analyze profile: %v", err)), nil, nil
		}

		result := &gcpdata.ProfileTopResult{
			ProfileMeta:     meta,
			ValueType:       valueType,
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
