package server

import (
	"context"
	"time"

	"github.com/google/pprof/profile"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// stubBackends implements every gcpdata backend interface with panicking
// method bodies. Variant-build tests only register tools and list them; the
// handlers are never invoked, so a non-nil-but-unusable backend is exactly
// what requireLogs/requireErrors/requireTraces/requireProfiler/requireQuerier
// need to pass.
type stubBackends struct{}

var (
	_ gcpdata.LogsQuerier     = stubBackends{}
	_ gcpdata.ErrorsQuerier   = stubBackends{}
	_ gcpdata.TraceQuerier    = stubBackends{}
	_ gcpdata.ProfilerQuerier = stubBackends{}
	_ gcpdata.MetricsQuerier  = stubBackends{}
)

func (stubBackends) QueryLogs(context.Context, string, string, int, string, string) (*gcpdata.LogQueryResult, error) {
	panic("stubBackends: handler not invoked in this test")
}

func (stubBackends) QueryLogsByTrace(context.Context, string, string, string, int, string) (*gcpdata.LogQueryResult, error) {
	panic("stubBackends: handler not invoked in this test")
}

func (stubBackends) QueryLogsByRequestID(context.Context, string, string, string, int, string) (*gcpdata.LogQueryResult, error) {
	panic("stubBackends: handler not invoked in this test")
}

func (stubBackends) FindRequests(context.Context, string, string, string, int, bool, string, int) (*gcpdata.RequestList, error) {
	panic("stubBackends: handler not invoked in this test")
}

func (stubBackends) ListServices(context.Context, string, string) (*gcpdata.ServiceList, error) {
	panic("stubBackends: handler not invoked in this test")
}

func (stubBackends) SummarizeLogs(context.Context, string, string, gcpdata.ProgressFunc) (*gcpdata.LogsSummary, error) {
	panic("stubBackends: handler not invoked in this test")
}

func (stubBackends) FindTracesFromLogs(context.Context, string, string, string, int, int) (*gcpdata.TraceFromLogsList, error) {
	panic("stubBackends: handler not invoked in this test")
}

func (stubBackends) ListErrors(context.Context, string, int, int, string, string) (*gcpdata.ErrorGroupList, error) {
	panic("stubBackends: handler not invoked in this test")
}

func (stubBackends) GetErrorGroup(context.Context, string, string, int, string) (*gcpdata.ErrorGroupDetail, error) {
	panic("stubBackends: handler not invoked in this test")
}

func (stubBackends) AnalyzeErrorTrends(context.Context, string, int, int, string, string) (*gcpdata.ErrorTrendList, error) {
	panic("stubBackends: handler not invoked in this test")
}

func (stubBackends) GetTrace(context.Context, string, string) (*gcpdata.TraceDetail, error) {
	panic("stubBackends: handler not invoked in this test")
}

func (stubBackends) ListTraces(context.Context, string, string, string, string, time.Time, time.Time, int, string) (*gcpdata.TraceListResult, error) {
	panic("stubBackends: handler not invoked in this test")
}

func (stubBackends) ListProfiles(context.Context, string, string, string, string, string, int, string) (*gcpdata.ProfileListResult, error) {
	panic("stubBackends: handler not invoked in this test")
}

func (stubBackends) GetOrFetchProfile(context.Context, string, string) (*profile.Profile, gcpdata.ProfileMeta, error) {
	panic("stubBackends: handler not invoked in this test")
}

func (stubBackends) CompareProfiles(context.Context, string, string, string, int, int) (*gcpdata.ProfileCompareResult, *profile.Profile, error) {
	panic("stubBackends: handler not invoked in this test")
}

func (stubBackends) ComputeTrends(context.Context, string, string, string, string, int, int, int, func(int, int, string)) (*gcpdata.ProfileTrendsResult, error) {
	panic("stubBackends: handler not invoked in this test")
}

func (stubBackends) CacheProfile(string, *profile.Profile, gcpdata.ProfileMeta) {
	panic("stubBackends: handler not invoked in this test")
}

func (stubBackends) GetMetricDescriptor(context.Context, string, string) (gcpdata.MetricDescriptorBasic, error) {
	panic("stubBackends: handler not invoked in this test")
}

func (stubBackends) ListMetricDescriptors(context.Context, string, string, int) ([]gcpdata.MetricDescriptorInfo, error) {
	panic("stubBackends: handler not invoked in this test")
}

func (stubBackends) QueryTimeSeries(context.Context, gcpdata.QueryTimeSeriesParams) ([]gcpdata.MetricTimeSeries, error) {
	panic("stubBackends: handler not invoked in this test")
}

func (stubBackends) QueryTimeSeriesAggregated(context.Context, gcpdata.QueryTimeSeriesParams, metrics.AggregationSpec) ([]gcpdata.MetricTimeSeries, gcpdata.AggregationWarnings, error) {
	panic("stubBackends: handler not invoked in this test")
}

func (stubBackends) GetResourceLabels(context.Context, string, string) ([]string, error) {
	panic("stubBackends: handler not invoked in this test")
}
