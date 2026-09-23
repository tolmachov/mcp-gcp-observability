package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var allProjectToolRegistrations = []func(*mcp.Server, Deps){
	RegisterLogsQuery, RegisterLogsByTrace, RegisterLogsByRequestID,
	RegisterLogsFindRequests, RegisterLogsK8s, RegisterLogsServices, RegisterLogsSummary,
	RegisterErrorsList, RegisterErrorsGet, RegisterErrorsTrends,
	RegisterTraceGet, RegisterTraceList, RegisterTraceFindFromLogs,
	RegisterMetricsList, RegisterMetricsSnapshot, RegisterMetricsTop, RegisterMetricsRelated, RegisterMetricsCompare,
	RegisterProfilerList, RegisterProfilerTop, RegisterProfilerPeek, RegisterProfilerFlamegraph, RegisterProfilerCompare, RegisterProfilerTrends,
}

func listedProjectTools(t *testing.T, policy ProjectPolicy) []*mcp.Tool {
	t.Helper()
	deps := allFakeBackends()
	deps.Project = policy
	ts := newTestToolServer(t)
	for _, register := range allProjectToolRegistrations {
		register(ts.server, deps)
	}
	ts.connect(context.Background())
	t.Cleanup(ts.close)
	result, err := ts.session.ListTools(context.Background(), nil)
	require.NoError(t, err)
	return result.Tools
}

func schemaObject(t *testing.T, schema any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(schema)
	require.NoError(t, err)
	var object map[string]any
	require.NoError(t, json.Unmarshal(encoded, &object))
	return object
}

func TestEveryProjectToolHasPinnedOrUnpinnedSchema(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy ProjectPolicy
		pinned bool
	}{
		{name: "pinned", policy: MustProjectPolicy("pinned-project"), pinned: true},
		{name: "unpinned", policy: MustProjectPolicy("")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listed := listedProjectTools(t, tc.policy)
			require.Len(t, listed, len(allProjectToolRegistrations))
			for _, tool := range listed {
				schema := schemaObject(t, tool.InputSchema)
				properties, ok := schema["properties"].(map[string]any)
				require.True(t, ok, "tool %s", tool.Name)
				_, hasProject := properties["project_id"]
				required := map[string]bool{}
				if items, ok := schema["required"].([]any); ok {
					for _, item := range items {
						required[item.(string)] = true
					}
				}
				if tc.pinned {
					assert.False(t, hasProject, "tool %s must hide project_id", tool.Name)
					assert.False(t, required["project_id"], "tool %s", tool.Name)
				} else {
					assert.True(t, hasProject, "tool %s must expose project_id", tool.Name)
					assert.True(t, required["project_id"], "tool %s", tool.Name)
				}
				switch additional := schema["additionalProperties"].(type) {
				case bool:
					assert.False(t, additional, "tool %s must reject unknown fields", tool.Name)
				case map[string]any:
					_, hasNot := additional["not"]
					assert.True(t, hasNot, "tool %s must reject unknown fields", tool.Name)
				default:
					require.Failf(t, "missing additionalProperties=false", "tool %s", tool.Name)
				}
			}
		})
	}
}

// TestRequiredStringPropertiesRejectEmpty pins that every required string
// input is non-empty at the schema level, so the SDK rejects "" before the
// handler runs and handlers need no "X is required" checks of their own.
func TestRequiredStringPropertiesRejectEmpty(t *testing.T) {
	for _, tool := range listedProjectTools(t, MustProjectPolicy("pinned-project")) {
		schema := schemaObject(t, tool.InputSchema)
		properties := schema["properties"].(map[string]any)
		required, _ := schema["required"].([]any)
		for _, name := range required {
			prop := properties[name.(string)].(map[string]any)
			if prop["type"] != "string" || prop["enum"] != nil {
				continue
			}
			assert.Equal(t, 1.0, prop["minLength"], "tool %s property %s", tool.Name, name)
		}
	}
}
