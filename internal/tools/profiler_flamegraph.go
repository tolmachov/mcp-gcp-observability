package tools

import (
	"context"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

// flamegraphMaxNodes bounds the nodes profiler_flamegraph returns, root included.
const flamegraphMaxNodes = 1000

// flamegraphSchema is a hand-written JSON schema for ProfileFlamegraphResult.
// FlamegraphNode is recursive (children[] contains FlamegraphNode), so we use
// $ref/$defs to express the recursion — same pattern as trace_get.go.
var flamegraphSchema = &jsonschema.Schema{
	Type: "object",
	Properties: map[string]*jsonschema.Schema{
		"profile_meta": {Type: "object"},
		"value_type":   {Type: "object"},
		"total_value":  {Type: "integer"},
		"max_depth":    {Type: "integer"},
		"min_pct":      {Type: "number"},
		"pruned_nodes": {Type: "integer", Description: "Number of subtrees cut by max_depth, min_pct or the node budget: one per omitted child of a returned node, however many descendants it has"},
		"root":         {Ref: "#/$defs/FlamegraphNode"},
	},
	Required: []string{"profile_meta", "value_type", "total_value", "root", "max_depth", "min_pct"},
	Defs: map[string]*jsonschema.Schema{
		"FlamegraphNode": {
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"name":       {Type: "string"},
				"file":       {Type: "string"},
				"self":       {Type: "integer"},
				"cumulative": {Type: "integer"},
				"pct":        {Type: "number"},
				"children": {
					Types: []string{"null", "array"},
					Items: &jsonschema.Schema{Ref: "#/$defs/FlamegraphNode"},
				},
			},
			Required: []string{"name", "self", "cumulative", "pct"},
		},
	},
}

func RegisterProfilerFlamegraph(s *mcp.Server, d Deps) {
	requireProfiler(d.Profiler)
	mcp.AddTool(s, &mcp.Tool{
		Name: "profiler_flamegraph",
		Description: applyMode(d.Mode, "Get a bounded subtree of the profile call tree (like a flamegraph view). "+
			"Returns a tree of function calls pruned by max_depth and min_pct. "+
			"Use root_function to focus on a specific subtree (omit for full profile). "+
			"Use profiler_top first to identify interesting functions, then drill down here. "+
			"Add base_profile_id to render a request-local diff."),
		Annotations: readOnlyAnnotations,
		InputSchema: projectInputSchema[ProfilerFlamegraphInput](d.Project,
			nonEmptyProp("profile_id"),
			nonNegativeValueIndex,
		),
		OutputSchema: flamegraphSchema,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ProfilerFlamegraphInput) (*mcp.CallToolResult, *gcpdata.ProfileFlamegraphResult, error) {
		maxDepth := clampLimit(in.MaxDepth, 3, 6)

		minPct := in.MinPct
		if minPct <= 0 {
			minPct = 1.0
		}

		p, meta, errRes := loadProfile(ctx, req, d, "profiler_flamegraph", in.ProjectID, in.ProfileID, in.BaseProfileID)
		if errRes != nil {
			return errRes, nil, nil
		}

		sendProgress(ctx, req, 1, 2, "Building flamegraph...")

		root, total, pruned, err := gcpdata.Flamegraph(p, in.RootFunction, in.ValueIndex, maxDepth, flamegraphMaxNodes, minPct)
		if err != nil {
			mcpLog(ctx, req, logLevelWarning, "profiler_flamegraph", fmt.Sprintf("analysis failed: %v", err))
			return ErrorResult(fmt.Sprintf("Failed to build flamegraph: %v", err)), nil, nil
		}

		result := &gcpdata.ProfileFlamegraphResult{
			ProfileMeta: meta,
			ValueType:   gcpdata.ProfileValueTypes(p)[in.ValueIndex], // validated by Flamegraph
			TotalValue:  total,
			Root:        *root,
			MaxDepth:    maxDepth,
			MinPct:      minPct,
			PrunedNodes: pruned,
		}
		if total == 0 {
			result.Warning = "Total profile value is zero (positive and negative values cancel out in diff profiles). All percentage values will be 0%."
		}

		return nil, result, nil
	})
}
