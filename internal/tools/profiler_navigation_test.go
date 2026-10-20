package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func TestProfileNavigationProgressStopsOnce(t *testing.T) {
	params := &mcp.CallToolParamsRaw{}
	params.SetProgressToken("navigation")
	progress, stop := profileNavigationProgress(context.Background(), &mcp.CallToolRequest{Params: params})
	progress(1, 2, "Analyzing profile...")
	assert.NotPanics(t, stop, "the deferred cleanup also runs after fetch-to-analysis transition")
}

func TestProfileNavigationHandlerErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"fetch", &gcpdata.ProfileFetchError{Err: status.Error(codes.Unauthenticated, "expired")}, "Failed to fetch profile:"},
		{"analysis", errors.New("value_index 2 out of range"), "Failed to analyze profile:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			ts := newTestToolServer(t)
			RegisterProfilerTop(ts.server, Deps{Project: MustProjectPolicy("test-project"), Profiler: fakeProfiler{
				top: func(_ context.Context, params gcpdata.TopProfileParams) (*gcpdata.ProfileTopResult, error) {
					assert.Equal(t, "test-project", params.Project)
					assert.Equal(t, "cur", params.ProfileID)
					assert.Equal(t, "base", params.BaseProfileID)
					return nil, tc.err
				},
			}})
			ts.connect(ctx)
			defer ts.close()
			res, err := ts.callTool(ctx, "profiler_top", map[string]any{"profile_id": "cur", "base_profile_id": "base"})
			require.NoError(t, err)
			assert.True(t, res.IsError)
			require.Len(t, res.Content, 1)
			text, ok := res.Content[0].(*mcp.TextContent)
			require.True(t, ok)
			assert.Contains(t, text.Text, tc.want)
			if tc.name == "fetch" {
				assert.Contains(t, text.Text, "gcloud auth application-default login")
			} else {
				assert.NotContains(t, text.Text, "gcloud auth application-default login")
			}
		})
	}
}

// Handlers pass navigation inputs to the module and serialize the assembled
// result directly; no parsed graph or result construction is part of this seam.
func TestProfileNavigationHandlersDelegate(t *testing.T) {
	meta := gcpdata.ProfileMeta{ProfileID: "assembled", IsDiff: true}
	topResult := &gcpdata.ProfileTopResult{ProfileMeta: meta, TotalValue: 123, Truncated: true, TruncationHint: "bounded"}
	peekResult := &gcpdata.ProfilePeekResult{ProfileMeta: meta, Function: gcpdata.PeekFunctionInfo{Name: "assembled-function"}, CallersTruncated: true, TruncationHint: "bounded"}
	flameResult := &gcpdata.ProfileFlamegraphResult{ProfileMeta: meta, Root: gcpdata.FlamegraphNode{Name: "assembled-root"}, MaxDepth: 5, MinPct: 0.25, PrunedNodes: 7, Warning: "warning"}
	cases := []struct {
		name     string
		register func(*mcp.Server, Deps)
		profiler fakeProfiler
		args     map[string]any
		expected any
		actual   any
	}{
		{"profiler_top", RegisterProfilerTop, fakeProfiler{top: func(_ context.Context, params gcpdata.TopProfileParams) (*gcpdata.ProfileTopResult, error) {
			assert.NotNil(t, params.OnProgress)
			if params.OnProgress != nil {
				params.OnProgress(1, 2, "Analyzing profile...")
			}
			params.OnProgress = nil
			assert.Equal(t, gcpdata.TopProfileParams{Project: "test-project", ProfileID: "cur", BaseProfileID: "base", ValueIndex: 1, Limit: 11, SortBy: "self", Filter: "handler"}, params)
			return topResult, nil
		}}, map[string]any{"profile_id": "cur", "base_profile_id": "base", "value_index": 1, "limit": 11, "sort_by": "self", "filter": "handler"}, topResult, &gcpdata.ProfileTopResult{}},
		{"profiler_peek", RegisterProfilerPeek, fakeProfiler{peek: func(_ context.Context, params gcpdata.PeekProfileParams) (*gcpdata.ProfilePeekResult, error) {
			assert.NotNil(t, params.OnProgress)
			if params.OnProgress != nil {
				params.OnProgress(1, 2, "Analyzing function...")
			}
			params.OnProgress = nil
			assert.Equal(t, gcpdata.PeekProfileParams{Project: "test-project", ProfileID: "cur", BaseProfileID: "base", ValueIndex: 1, Limit: 11, FunctionName: "handler"}, params)
			return peekResult, nil
		}}, map[string]any{"profile_id": "cur", "base_profile_id": "base", "value_index": 1, "limit": 11, "function_name": "handler"}, peekResult, &gcpdata.ProfilePeekResult{}},
		{"profiler_flamegraph", RegisterProfilerFlamegraph, fakeProfiler{flamegraph: func(_ context.Context, params gcpdata.FlamegraphProfileParams) (*gcpdata.ProfileFlamegraphResult, error) {
			assert.NotNil(t, params.OnProgress)
			if params.OnProgress != nil {
				params.OnProgress(1, 2, "Building flamegraph...")
			}
			params.OnProgress = nil
			assert.Equal(t, gcpdata.FlamegraphProfileParams{Project: "test-project", ProfileID: "cur", BaseProfileID: "base", ValueIndex: 1, MaxDepth: 5, MinPct: 0.25, RootFunction: "handler"}, params)
			return flameResult, nil
		}}, map[string]any{"profile_id": "cur", "base_profile_id": "base", "value_index": 1, "max_depth": 5, "min_pct": 0.25, "root_function": "handler"}, flameResult, &gcpdata.ProfileFlamegraphResult{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestToolServer(t)
			tc.register(ts.server, Deps{Project: MustProjectPolicy("test-project"), Profiler: tc.profiler})
			ts.connect(context.Background())
			defer ts.close()
			params := &mcp.CallToolParams{Name: tc.name, Arguments: tc.args}
			params.SetProgressToken("navigation")
			result, err := ts.session.CallTool(context.Background(), params)
			require.NoError(t, err)
			require.False(t, result.IsError)
			unmarshalResult(t, result, tc.actual)
			assert.Equal(t, tc.expected, tc.actual)
		})
	}
}
