package tools

import (
	"context"

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
		project, err := d.Project.Resolve(in.ProjectID)
		if err != nil {
			return ErrorResult(err.Error()), nil, nil
		}
		progress, stop := profileNavigationProgress(ctx, req)
		defer stop()
		result, err := d.Profiler.FlamegraphProfile(ctx, gcpdata.FlamegraphProfileParams{
			Project: project, ProfileID: in.ProfileID, BaseProfileID: in.BaseProfileID,
			RootFunction: in.RootFunction, ValueIndex: in.ValueIndex, MaxDepth: in.MaxDepth, MinPct: in.MinPct,
			OnProgress: progress,
		})
		if err != nil {
			return profileNavigationErrorResult(err, "Failed to build flamegraph:"), nil, nil
		}
		return nil, result, nil
	})
}
