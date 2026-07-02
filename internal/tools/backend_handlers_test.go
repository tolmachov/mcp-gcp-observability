package tools

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/pprof/profile"
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
				Profiler: fakeProfiler{listProfiles: func(context.Context, gcpdata.ListProfilesParams) (*gcpdata.ProfileListResult, error) {
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

func TestTraceListHandler(t *testing.T) {
	ctx := context.Background()

	t.Run("stopped-on-error partial result round-trips without a resume token", func(t *testing.T) {
		deps := Deps{
			Traces: fakeTraces{listTrace: func(context.Context, string, string, string, string, time.Time, time.Time, int, string) (*gcpdata.TraceListResult, error) {
				return &gcpdata.TraceListResult{
					Count:          2,
					Traces:         []gcpdata.TraceSummary{{TraceID: "a"}, {TraceID: "b"}},
					Truncated:      true,
					StoppedOnError: true,
					TruncationHint: "Listing stopped after 2 traces due to an error and cannot be resumed: rpc deadline",
				}, nil
			}},
			DefaultProject: "p",
		}
		ts := newTestToolServer(t)
		RegisterTraceList(ts.server, deps)
		ts.connect(ctx)
		defer ts.close()

		res, err := ts.callTool(ctx, "trace_list", map[string]any{})
		require.NoError(t, err)
		assert.False(t, res.IsError)

		var got gcpdata.TraceListResult
		parseResult(t, res, &got)
		assert.True(t, got.StoppedOnError, "error-stop must be machine-distinguishable")
		assert.Empty(t, got.NextPageToken, "an error-stop cannot be resumed")
		assert.NotContains(t, got.TruncationHint, "next_page_token",
			"error-stop hint must not invite pagination")
	})
}

func TestProfilerCompareHandler(t *testing.T) {
	ctx := context.Background()

	t.Run("forwards current/base ID order and caches diff under project/diff_id", func(t *testing.T) {
		var gotCurrent, gotBase string
		cached := map[string]*profile.Profile{}
		deps := Deps{
			Profiler: fakeProfiler{
				compare: func(_ context.Context, _, currentID, baseID string, _, _ int) (*gcpdata.ProfileCompareResult, *profile.Profile, error) {
					gotCurrent, gotBase = currentID, baseID
					return &gcpdata.ProfileCompareResult{
						DiffID:      "diffXYZ",
						CurrentMeta: gcpdata.ProfileMeta{ProfileType: "CPU", Target: "svc"},
					}, &profile.Profile{}, nil
				},
				cached: cached,
			},
			DefaultProject: "proj",
		}
		ts := newTestToolServer(t)
		RegisterProfilerCompare(ts.server, deps)
		ts.connect(ctx)
		defer ts.close()

		res, err := ts.callTool(ctx, "profiler_compare", map[string]any{
			"profile_id":      "current-1",
			"base_profile_id": "base-2",
		})
		require.NoError(t, err)
		assert.False(t, res.IsError)
		// A swap here would silently invert every regression/improvement.
		assert.Equal(t, "current-1", gotCurrent, "current profile_id must not be swapped with base")
		assert.Equal(t, "base-2", gotBase)
		// top/peek/flamegraph resolve the diff via GetOrFetchProfile, which
		// derives its key as project+"/"+id — the cache key must match.
		_, ok := cached["proj/diffXYZ"]
		assert.True(t, ok, "diff must be cached under project+\"/\"+DiffID")
	})
}

// TestProfilerNavigationHandlersValidateBeforeFetch pins that the diff-navigation
// handlers reject bad input before spending a profile fetch. The internal
// max_depth/limit clamps are exercised indirectly (they need a real parsed
// profile), but the required-field and non-negative guards are the meaningful
// user-facing contract.
func TestProfilerNavigationHandlersValidateBeforeFetch(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		tool     string
		register func(*mcp.Server, Deps)
		args     map[string]any
	}{
		{"flamegraph missing profile_id", "profiler_flamegraph", RegisterProfilerFlamegraph, map[string]any{}},
		{"flamegraph negative value_index", "profiler_flamegraph", RegisterProfilerFlamegraph, map[string]any{"profile_id": "p", "value_index": -1}},
		{"top missing profile_id", "profiler_top", RegisterProfilerTop, map[string]any{}},
		{"top negative value_index", "profiler_top", RegisterProfilerTop, map[string]any{"profile_id": "p", "value_index": -1}},
		{"peek missing profile_id", "profiler_peek", RegisterProfilerPeek, map[string]any{"function_name": "f"}},
		{"peek missing function_name", "profiler_peek", RegisterProfilerPeek, map[string]any{"profile_id": "p"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fetched := false
			deps := Deps{
				Profiler: fakeProfiler{getOrFetch: func(context.Context, string, string) (*profile.Profile, gcpdata.ProfileMeta, error) {
					fetched = true
					return nil, gcpdata.ProfileMeta{}, nil
				}},
				DefaultProject: "p",
			}
			ts := newTestToolServer(t)
			tc.register(ts.server, deps)
			ts.connect(ctx)
			defer ts.close()

			res, err := ts.callTool(ctx, tc.tool, tc.args)
			require.NoError(t, err)
			assert.True(t, res.IsError)
			assert.False(t, fetched, "validation must reject before fetching the profile")
		})
	}
}

func TestProfilerListHandler(t *testing.T) {
	ctx := context.Background()

	t.Run("happy path returns profiles", func(t *testing.T) {
		deps := Deps{
			Profiler: fakeProfiler{listProfiles: func(context.Context, gcpdata.ListProfilesParams) (*gcpdata.ProfileListResult, error) {
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
