package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func RegisterProfilerPeek(s *mcp.Server, d Deps) {
	requireClient(d.Client)
	mcp.AddTool(s, &mcp.Tool{
		Name: "profiler_peek",
		Description: applyMode(d.Mode, "Show callers and callees of a specific function in a profile (like pprof peek). "+
			"Navigates the call graph: who calls this function, and what does it call? "+
			"Use function names from profiler_top results. Substring matching is used. "+
			"If the name is ambiguous, the error will list matching candidates — use a more specific name. "+
			"Works with both regular profile_id and diff_id from profiler_compare."),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		OutputSchema: outputSchemaFor[gcpdata.ProfilePeekResult](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ProfilerPeekInput) (*mcp.CallToolResult, *gcpdata.ProfilePeekResult, error) {
		if in.ProfileID == "" {
			return errResult("profile_id is required"), nil, nil
		}
		if in.ValueIndex < 0 {
			return errResult("value_index must be non-negative"), nil, nil
		}
		if in.FunctionName == "" {
			return errResult("function_name is required"), nil, nil
		}
		project, err := resolveProject(in.ProjectID, d.Client.Config().DefaultProject)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		limit := clampLimit(in.Limit, 10, 30)

		sendProgress(ctx, req, 0, 2, "Downloading profile...")

		p, meta, err := gcpdata.GetOrFetchProfile(ctx, d.Client.ProfilerService(), d.ProfileCache, project, in.ProfileID)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "profiler_peek", fmt.Sprintf("fetch profile failed: %v", err))
			return errResult(fmt.Sprintf("Failed to fetch profile: %v", err)), nil, nil
		}

		sendProgress(ctx, req, 1, 2, "Analyzing function...")

		vt := gcpdata.ProfileValueTypes(p)
		if in.ValueIndex >= len(vt) {
			return errResult(fmt.Sprintf("value_index %d out of range (profile has %d value types)", in.ValueIndex, len(vt))), nil, nil
		}
		valueType := vt[in.ValueIndex]

		funcInfo, callers, callees, err := gcpdata.PeekFunction(p, in.FunctionName, in.ValueIndex, limit)
		if err != nil {
			mcpLog(ctx, req, logLevelWarning, "profiler_peek", fmt.Sprintf("analysis failed: %v", err))
			return errResult(fmt.Sprintf("Failed to peek function: %v", err)), nil, nil
		}

		callersTrunc := len(callers) >= limit
		calleesTrunc := len(callees) >= limit
		result := &gcpdata.ProfilePeekResult{
			ProfileMeta:      meta,
			ValueType:        valueType,
			Function:         *funcInfo,
			Callers:          callers,
			Callees:          callees,
			CallersTruncated: callersTrunc,
			CalleesTruncated: calleesTrunc,
		}
		if callersTrunc || calleesTrunc {
			result.TruncationHint = fmt.Sprintf("Showing up to %d callers/callees. Increase limit (max 30) for more.", limit)
		}

		return nil, result, nil
	})
}
