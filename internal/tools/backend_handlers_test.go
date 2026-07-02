package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

// These tests exercise the logs/errors/trace/profiler handlers end-to-end
// through the in-memory MCP session using the backend fakes — coverage that was
// impossible while the handlers bound the concrete *gcpclient.Client.

func TestLogsQueryHandler(t *testing.T) {
	ctx := context.Background()

	t.Run("happy path forwards project, filter, limit and returns result", func(t *testing.T) {
		var gotProject, gotFilter, gotOrder string
		var gotLimit int
		deps := Deps{
			Logs: fakeLogs{queryLogs: func(_ context.Context, project, filter string, limit int, order, _ string) (*gcpdata.LogQueryResult, error) {
				gotProject, gotFilter, gotLimit, gotOrder = project, filter, limit, order
				return &gcpdata.LogQueryResult{Count: 1, Entries: []gcpdata.LogEntry{{}}}, nil
			}},
			DefaultProject: "default-proj",
			LogsMaxLimit:   1000,
		}
		ts := newTestToolServer(t)
		RegisterLogsQuery(ts.server, deps)
		ts.connect(ctx)
		defer ts.close()

		res, err := ts.callTool(ctx, "logs_query", map[string]any{"filter": "severity>=ERROR", "limit": 5})
		require.NoError(t, err)
		assert.False(t, res.IsError)
		assert.Equal(t, "default-proj", gotProject)
		assert.Contains(t, gotFilter, "severity>=ERROR")
		assert.Equal(t, 5, gotLimit)
		assert.Equal(t, "desc", gotOrder)
	})

	t.Run("backend error becomes a tool error result", func(t *testing.T) {
		deps := Deps{
			Logs: fakeLogs{queryLogs: func(context.Context, string, string, int, string, string) (*gcpdata.LogQueryResult, error) {
				return nil, errors.New("boom")
			}},
			DefaultProject: "p",
			LogsMaxLimit:   1000,
		}
		ts := newTestToolServer(t)
		RegisterLogsQuery(ts.server, deps)
		ts.connect(ctx)
		defer ts.close()

		res, err := ts.callTool(ctx, "logs_query", map[string]any{"filter": "x"})
		require.NoError(t, err)
		assert.True(t, res.IsError)
	})

	t.Run("missing filter is rejected before the backend is called", func(t *testing.T) {
		called := false
		deps := Deps{
			Logs: fakeLogs{queryLogs: func(context.Context, string, string, int, string, string) (*gcpdata.LogQueryResult, error) {
				called = true
				return nil, nil
			}},
			DefaultProject: "p",
			LogsMaxLimit:   1000,
		}
		ts := newTestToolServer(t)
		RegisterLogsQuery(ts.server, deps)
		ts.connect(ctx)
		defer ts.close()

		res, err := ts.callTool(ctx, "logs_query", map[string]any{})
		require.NoError(t, err)
		assert.True(t, res.IsError)
		assert.False(t, called, "backend must not be called when validation fails")
	})
}

func TestErrorsListHandler(t *testing.T) {
	ctx := context.Background()

	t.Run("explicit project overrides default and clamps limit", func(t *testing.T) {
		var gotProject string
		var gotLimit int
		deps := Deps{
			Errors: fakeErrors{listErrors: func(_ context.Context, project string, _ int, limit int, _, _ string) (*gcpdata.ErrorGroupList, error) {
				gotProject, gotLimit = project, limit
				return &gcpdata.ErrorGroupList{Count: 0}, nil
			}},
			DefaultProject: "default-proj",
			ErrorsMaxLimit: 30,
		}
		ts := newTestToolServer(t)
		RegisterErrorsList(ts.server, deps)
		ts.connect(ctx)
		defer ts.close()

		res, err := ts.callTool(ctx, "errors_list", map[string]any{"project_id": "explicit", "limit": 999})
		require.NoError(t, err)
		assert.False(t, res.IsError)
		assert.Equal(t, "explicit", gotProject)
		assert.Equal(t, 30, gotLimit, "limit must be clamped to ErrorsMaxLimit")
	})
}

func TestTraceGetHandler(t *testing.T) {
	ctx := context.Background()

	t.Run("forwards trace id and returns detail", func(t *testing.T) {
		var gotTraceID string
		deps := Deps{
			Traces: fakeTraces{getTrace: func(_ context.Context, _, traceID string) (*gcpdata.TraceDetail, error) {
				gotTraceID = traceID
				return &gcpdata.TraceDetail{TraceID: traceID, Count: 2}, nil
			}},
			DefaultProject: "p",
		}
		ts := newTestToolServer(t)
		RegisterTraceGet(ts.server, deps)
		ts.connect(ctx)
		defer ts.close()

		res, err := ts.callTool(ctx, "trace_get", map[string]any{"trace_id": "abc123"})
		require.NoError(t, err)
		assert.False(t, res.IsError)
		assert.Equal(t, "abc123", gotTraceID)
	})
}

// TestErrorPathsSurviveOutputSchema pins the omitempty contract on map-typed
// output fields: the SDK serializes the zero value of the output struct when a
// handler returns errResult, and the generated schema rejects null for maps
// (unlike slices). Without omitempty these calls fail with a protocol-level
// validation error instead of an IsError tool result.
func TestErrorPathsSurviveOutputSchema(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		tool     string
		register func(s *mcp.Server, d Deps)
		deps     Deps
		args     map[string]any
	}{
		{
			tool:     "profiler_list",
			register: RegisterProfilerList,
			deps: Deps{
				Profiler: fakeProfiler{listProfiles: func(context.Context, string, string, string, string, string, int, string) (*gcpdata.ProfileListResult, error) {
					return nil, errors.New("boom")
				}},
				DefaultProject: "p",
			},
			args: map[string]any{},
		},
		{
			tool:     "errors_trends",
			register: RegisterErrorsTrends,
			deps: Deps{
				Errors: fakeErrors{analyzeTrends: func(context.Context, string, int, int, string, string) (*gcpdata.ErrorTrendList, error) {
					return nil, errors.New("boom")
				}},
				DefaultProject: "p",
				ErrorsMaxLimit: 30,
			},
			args: map[string]any{},
		},
		{
			tool:     "logs_summary",
			register: RegisterLogsSummary,
			deps: Deps{
				Logs: fakeLogs{summarizeLogs: func(context.Context, string, string, gcpdata.ProgressFunc) (*gcpdata.LogsSummary, error) {
					return nil, errors.New("boom")
				}},
				DefaultProject: "p",
				LogsMaxLimit:   1000,
			},
			args: map[string]any{"filter": "severity>=ERROR"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.tool+" backend error becomes a tool error result", func(t *testing.T) {
			ts := newTestToolServer(t)
			tc.register(ts.server, tc.deps)
			ts.connect(ctx)
			defer ts.close()

			res, err := ts.callTool(ctx, tc.tool, tc.args)
			require.NoError(t, err, "error path must not trip output-schema validation")
			assert.True(t, res.IsError)
		})
	}
}

func TestProfilerListHandler(t *testing.T) {
	ctx := context.Background()

	t.Run("happy path returns profiles", func(t *testing.T) {
		deps := Deps{
			Profiler: fakeProfiler{listProfiles: func(context.Context, string, string, string, string, string, int, string) (*gcpdata.ProfileListResult, error) {
				return &gcpdata.ProfileListResult{
					Count:    1,
					Profiles: []gcpdata.ProfileMeta{{ProfileType: "CPU"}},
					Summary: gcpdata.ProfileSummary{
						CountByType:   map[string]int{"CPU": 1},
						CountByTarget: map[string]int{},
					},
				}, nil
			}},
			DefaultProject: "p",
		}
		ts := newTestToolServer(t)
		RegisterProfilerList(ts.server, deps)
		ts.connect(ctx)
		defer ts.close()

		res, err := ts.callTool(ctx, "profiler_list", map[string]any{})
		require.NoError(t, err)
		assert.False(t, res.IsError)
	})
}
