package tools

import (
	"context"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

// flamegraphSchema is a hand-written JSON schema for ProfileFlamegraphResult.
// FlamegraphNode is recursive (children[] contains FlamegraphNode), so we use
// $ref/$defs to express the recursion — same pattern as trace_get.go.
var flamegraphSchema = &jsonschema.Schema{
	Type: "object",
	Properties: map[string]*jsonschema.Schema{
		"profile_meta":  {Type: "object"},
		"value_type":    {Type: "object"},
		"total_value":   {Type: "integer"},
		"max_depth":     {Type: "integer"},
		"min_pct":       {Type: "number"},
		"pruned_nodes":  {Type: "integer"},
		"truncated":     {Type: "boolean"},
		"omitted_nodes": {Type: "integer"},
		"root":          {Ref: "#/$defs/FlamegraphNode"},
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
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		InputSchema: projectInputSchema[ProfilerFlamegraphInput](d.Project,
			nonEmptyProp("profile_id"),
			nonNegativeValueIndex,
		),
		OutputSchema: flamegraphSchema,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ProfilerFlamegraphInput) (*mcp.CallToolResult, *gcpdata.ProfileFlamegraphResult, error) {
		project, err := d.Project.Resolve(in.ProjectID)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		maxDepth := in.MaxDepth
		if maxDepth <= 0 {
			maxDepth = 3
		}
		if maxDepth > 6 {
			maxDepth = 6
		}

		minPct := in.MinPct
		if minPct <= 0 {
			minPct = 1.0
		}

		// Fetching an uncached profile scans the Export API and can run long on
		// large projects; heartbeat progress keeps the client request alive.
		stopHeartbeat := startProgressHeartbeat(ctx, req, "Downloading profile…")
		p, meta, err := d.Profiler.GetProfileOrDiff(ctx, project, in.ProfileID, in.BaseProfileID)
		stopHeartbeat()
		if err != nil {
			mcpLog(ctx, req, logLevelError, "profiler_flamegraph", fmt.Sprintf("fetch profile failed: %v", err))
			return errResult(fmt.Sprintf("Failed to fetch profile: %v", err)), nil, nil
		}

		sendProgress(ctx, req, 1, 2, "Building flamegraph...")

		vt := gcpdata.ProfileValueTypes(p)
		if in.ValueIndex >= len(vt) {
			return errResult(fmt.Sprintf("value_index %d out of range (profile has %d value types)", in.ValueIndex, len(vt))), nil, nil
		}
		valueType := vt[in.ValueIndex]

		root, total, pruned, err := gcpdata.Flamegraph(p, in.RootFunction, in.ValueIndex, maxDepth, minPct)
		if err != nil {
			mcpLog(ctx, req, logLevelWarning, "profiler_flamegraph", fmt.Sprintf("analysis failed: %v", err))
			return errResult(fmt.Sprintf("Failed to build flamegraph: %v", err)), nil, nil
		}
		omitted := limitFlamegraphNodes(root, 1000)

		result := &gcpdata.ProfileFlamegraphResult{
			ProfileMeta:  meta,
			ValueType:    valueType,
			TotalValue:   total,
			Root:         *root,
			MaxDepth:     maxDepth,
			MinPct:       minPct,
			PrunedNodes:  pruned,
			Truncated:    omitted > 0,
			OmittedNodes: omitted,
		}
		if total == 0 {
			result.Warning = "Total profile value is zero (positive and negative values cancel out in diff profiles). All percentage values will be 0%."
		}

		return nil, result, nil
	})
}

func limitFlamegraphNodes(root *gcpdata.FlamegraphNode, limit int) int {
	if root == nil || limit < 1 {
		return 0
	}
	remaining := limit - 1
	omitted := 0
	var trim func(*gcpdata.FlamegraphNode)
	trim = func(node *gcpdata.FlamegraphNode) {
		kept := node.Children[:0]
		for i := range node.Children {
			child := &node.Children[i]
			if remaining == 0 {
				omitted += flamegraphNodeCount(child)
				continue
			}
			remaining--
			trim(child)
			kept = append(kept, *child)
		}
		node.Children = kept
	}
	trim(root)
	return omitted
}

func flamegraphNodeCount(node *gcpdata.FlamegraphNode) int {
	count := 1
	for i := range node.Children {
		count += flamegraphNodeCount(&node.Children[i])
	}
	return count
}
