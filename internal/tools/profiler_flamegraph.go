package tools

import (
	"context"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
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
		"pruned_nodes": {Type: "integer"},
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

func RegisterProfilerFlamegraph(s *mcp.Server, client *gcpclient.Client, cache *gcpdata.ProfileCache, mode RegistrationMode) {
	requireClient(client)
	mcp.AddTool(s, &mcp.Tool{
		Name: "profiler_flamegraph",
		Description: applyMode(mode, "Get a bounded subtree of the profile call tree (like a flamegraph view). "+
			"Returns a tree of function calls pruned by max_depth and min_pct. "+
			"Use root_function to focus on a specific subtree (omit for full profile). "+
			"Use profiler_top first to identify interesting functions, then drill down here. "+
			"Works with both regular profile_id and diff_id from profiler_compare."),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		OutputSchema: flamegraphSchema,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ProfilerFlamegraphInput) (*mcp.CallToolResult, *gcpdata.ProfileFlamegraphResult, error) {
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

		sendProgress(ctx, req, 0, 2, "Downloading profile...")

		p, meta, err := gcpdata.GetOrFetchProfile(ctx, client.ProfilerService(), cache, project, in.ProfileID)
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

		result := &gcpdata.ProfileFlamegraphResult{
			ProfileMeta: meta,
			ValueType:   valueType,
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
