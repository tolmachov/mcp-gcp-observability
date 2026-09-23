package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/pprof/profile"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
	"github.com/tolmachov/mcp-gcp-observability/internal/tools"
)

// failingBackends implements every gcpdata backend interface with methods
// that all fail with err.
type failingBackends struct{ err error }

var (
	_ gcpdata.LogsQuerier     = failingBackends{}
	_ gcpdata.ErrorsQuerier   = failingBackends{}
	_ gcpdata.TraceQuerier    = failingBackends{}
	_ gcpdata.ProfilerQuerier = failingBackends{}
	_ gcpdata.MetricsQuerier  = failingBackends{}
)

func (f failingBackends) QueryLogs(context.Context, string, string, int, string, string) (*gcpdata.LogQueryResult, error) {
	return nil, f.err
}

func (f failingBackends) QueryLogsByTrace(context.Context, string, string, string, int, string) (*gcpdata.LogQueryResult, error) {
	return nil, f.err
}

func (f failingBackends) QueryLogsByRequestID(context.Context, string, string, string, int, string) (*gcpdata.LogQueryResult, error) {
	return nil, f.err
}

func (f failingBackends) FindRequests(context.Context, gcpdata.FindRequestsParams) (*gcpdata.RequestList, error) {
	return nil, f.err
}

func (f failingBackends) ListServices(context.Context, string, string) (*gcpdata.ServiceList, error) {
	return nil, f.err
}

func (f failingBackends) SummarizeLogs(context.Context, string, string, gcpdata.ProgressFunc) (*gcpdata.LogsSummary, error) {
	return nil, f.err
}

func (f failingBackends) FindTracesFromLogs(context.Context, string, string, string, int, int) (*gcpdata.TraceFromLogsList, error) {
	return nil, f.err
}

func (f failingBackends) ListErrors(context.Context, string, gcpdata.ErrorWindow, int, string, string) (*gcpdata.ErrorGroupList, error) {
	return nil, f.err
}

func (f failingBackends) GetErrorGroup(context.Context, string, string, int, string) (*gcpdata.ErrorGroupDetail, error) {
	return nil, f.err
}

func (f failingBackends) AnalyzeErrorTrends(context.Context, string, gcpdata.ErrorWindow, int, string, string) (*gcpdata.ErrorTrendList, error) {
	return nil, f.err
}

func (f failingBackends) GetTrace(context.Context, string, string) (*gcpdata.TraceDetail, error) {
	return nil, f.err
}

func (f failingBackends) ListTraces(context.Context, string, string, string, string, time.Time, time.Time, int, string) (*gcpdata.TraceListResult, error) {
	return nil, f.err
}

func (f failingBackends) ListProfiles(context.Context, gcpdata.ListProfilesParams) (*gcpdata.ProfileListResult, error) {
	return nil, f.err
}

func (f failingBackends) GetOrFetchProfile(context.Context, string, string) (*profile.Profile, gcpdata.ProfileMeta, error) {
	return nil, gcpdata.ProfileMeta{}, f.err
}

func (f failingBackends) GetProfileOrDiff(context.Context, string, string, string) (*profile.Profile, gcpdata.ProfileMeta, error) {
	return nil, gcpdata.ProfileMeta{}, f.err
}

func (f failingBackends) CompareProfiles(context.Context, string, string, string, int, int) (*gcpdata.ProfileCompareResult, error) {
	return nil, f.err
}

func (f failingBackends) ComputeTrends(context.Context, gcpdata.ComputeTrendsParams, func(int, int, string)) (*gcpdata.ProfileTrendsResult, error) {
	return nil, f.err
}

func (failingBackends) Close() error { return nil }

func (f failingBackends) GetMetricDescriptor(context.Context, string, string) (gcpdata.MetricDescriptorBasic, error) {
	return gcpdata.MetricDescriptorBasic{}, f.err
}

func (f failingBackends) ListMetricDescriptors(context.Context, string, string, int) ([]gcpdata.MetricDescriptorInfo, error) {
	return nil, f.err
}

func (f failingBackends) QueryTimeSeries(context.Context, gcpdata.QueryTimeSeriesParams) ([]gcpdata.MetricTimeSeries, gcpdata.QueryWarnings, error) {
	return nil, gcpdata.QueryWarnings{}, f.err
}

func (f failingBackends) QueryTimeSeriesAggregated(context.Context, gcpdata.QueryTimeSeriesParams, metrics.AggregationSpec) ([]gcpdata.MetricTimeSeries, gcpdata.QueryWarnings, error) {
	return nil, gcpdata.QueryWarnings{}, f.err
}

func (f failingBackends) GetResourceLabels(context.Context, string, string) ([]string, error) {
	return nil, f.err
}

// TestEveryToolSurfacesReauthHint pins that every tool turns rejected
// credentials into the re-authentication hint rather than a bare error.
func TestEveryToolSurfacesReauthHint(t *testing.T) {
	const metric = "custom.googleapis.com/checkout/latency"
	const traceID = "0123456789abcdef0123456789abcdef"
	now := time.Now().UTC()
	args := map[string]map[string]any{
		"logs_query":           {"filter": "severity>=ERROR"},
		"logs_by_trace":        {"trace_id": traceID},
		"logs_by_request_id":   {"request_id": "req-1"},
		"logs_find_requests":   {"url_pattern": "/api"},
		"logs_k8s":             {},
		"logs_services":        {},
		"logs_summary":         {},
		"errors_list":          {},
		"errors_get":           {"group_id": "group-1"},
		"errors_trends":        {},
		"trace_get":            {"trace_id": traceID},
		"trace_list":           {},
		"trace_find_from_logs": {"filter": "severity>=ERROR"},
		"metrics_list":         {},
		"metrics_snapshot":     {"metric_type": metric},
		"metrics_top_contributors": {
			"metric_type": metric, "dimension": "metric.labels.region",
		},
		"metrics_related": {"metric_type": metric},
		"metrics_compare": {
			"metric_type":   metric,
			"window_a_from": now.Add(-2 * time.Hour).Format(time.RFC3339),
			"window_a_to":   now.Add(-time.Hour).Format(time.RFC3339),
			"window_b_from": now.Add(-time.Hour).Format(time.RFC3339),
			"window_b_to":   now.Format(time.RFC3339),
		},
		"profiler_list":       {},
		"profiler_top":        {"profile_id": "p1"},
		"profiler_peek":       {"profile_id": "p1", "function_name": "main"},
		"profiler_flamegraph": {"profile_id": "p1"},
		"profiler_compare":    {"profile_id": "p1", "base_profile_id": "p0"},
		"profiler_trends":     {"profile_type": "CPU", "target": "svc"},
	}
	backends := failingBackends{err: status.Error(codes.Unauthenticated, "token expired")}
	deps := tools.Deps{
		Logs:     backends,
		Errors:   backends,
		Traces:   backends,
		Profiler: backends,
		Querier:  backends,
		Registry: metrics.NewRegistryFromMetaMap(map[string]metrics.MetricMeta{
			metric: {Kind: metrics.KindLatency, RelatedMetrics: []string{"custom.googleapis.com/checkout/errors"}},
		}),
		Project: tools.MustProjectPolicy("test-project"),
	}
	for _, spec := range toolSpecs {
		t.Run(spec.name, func(t *testing.T) {
			toolArgs, ok := args[spec.name]
			require.True(t, ok, "no arguments for tool %s", spec.name)
			// A bare server: the handler, not the tool-limits middleware, is
			// under test.
			srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.0.0"}, nil)
			spec.register(srv, deps)
			session := connectInMemory(t, srv)

			res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: spec.name, Arguments: toolArgs})
			require.NoError(t, err)
			require.True(t, res.IsError)
			require.Len(t, res.Content, 1)
			text, ok := res.Content[0].(*mcp.TextContent)
			require.True(t, ok)
			assert.Contains(t, text.Text, "gcloud auth application-default login")
		})
	}
}

// enumInputs names the input properties that take one of a fixed set of
// values, wherever a tool declares them.
var enumInputs = map[string]bool{
	"order": true, "window": true, "baseline_mode": true, "severity": true, "method": true,
	"order_by": true, "view": true, "sort_by": true, "profile_type": true, "kind": true,
}

// TestEnumInputsDeclareTheirValues pins that every enum input of every tool
// carries its values (and default, if any) in the schema, and that the
// description lists exactly those values.
func TestEnumInputsDeclareTheirValues(t *testing.T) {
	s := testServer(t)
	srv := s.newMCPInstance(s.completer)
	registerAllTools(srv, testToolDeps())
	for _, tool := range listToolsViaInMemory(t, srv) {
		raw, err := json.Marshal(tool.InputSchema)
		require.NoError(t, err)
		var schema jsonschema.Schema
		require.NoError(t, json.Unmarshal(raw, &schema))
		for name, prop := range schema.Properties {
			if !enumInputs[name] {
				assert.Empty(t, prop.Enum, "%s.%s has an enum but is not listed in enumInputs", tool.Name, name)
				continue
			}
			require.NotEmpty(t, prop.Enum, "%s.%s has no enum", tool.Name, name)
			values := make([]string, len(prop.Enum))
			for i, v := range prop.Enum {
				values[i] = v.(string)
			}
			want := "One of: " + strings.Join(values, ", ")
			if prop.Default != nil {
				var def string
				require.NoError(t, json.Unmarshal(prop.Default, &def))
				assert.Contains(t, values, def, "%s.%s default", tool.Name, name)
				want += ". Default: " + def
			}
			assert.True(t, strings.HasSuffix(prop.Description, ". "+want), "%s.%s description %q", tool.Name, name, prop.Description)
		}
	}
}

// connectInMemory connects an in-memory MCP client to srv for the lifetime
// of the test.
func connectInMemory(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ct, st := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, st) }()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.0.0"}, nil).Connect(ctx, ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}
