package tools

import (
	"context"
	"fmt"

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
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		InputSchema: projectInputSchema[ProfilerPeekInput](d.Project,
			nonEmptyProp("profile_id"),
			nonEmptyProp("function_name"),
			nonNegativeValueIndex,
		),
		OutputSchema: outputSchemaFor[gcpdata.ProfilePeekResult](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ProfilerPeekInput) (*mcp.CallToolResult, *gcpdata.ProfilePeekResult, error) {
		limit := clampLimit(in.Limit, 10, 30)

		p, meta, errRes := loadProfile(ctx, req, d, "profiler_peek", in.ProjectID, in.ProfileID, in.BaseProfileID)
		if errRes != nil {
			return errRes, nil, nil
		}

		sendProgress(ctx, req, 1, 2, "Analyzing function...")

		funcInfo, callers, callees, err := gcpdata.PeekFunction(p, in.FunctionName, in.ValueIndex, limit)
		if err != nil {
			mcpLog(ctx, req, logLevelWarning, "profiler_peek", fmt.Sprintf("analysis failed: %v", err))
			return errResult(fmt.Sprintf("Failed to peek function: %v", err)), nil, nil
		}

		callersTrunc := len(callers) >= limit
		calleesTrunc := len(callees) >= limit
		result := &gcpdata.ProfilePeekResult{
			ProfileMeta:      meta,
			ValueType:        gcpdata.ProfileValueTypes(p)[in.ValueIndex], // validated by PeekFunction
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
