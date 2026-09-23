package server

import (
	"context"
	"time"

	"github.com/google/pprof/profile"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// stubBackends implements every gcpdata backend interface with methods that
// all fail with err. Variant-build tests only register tools and list them,
// so they leave err nil: a non-nil-but-unusable backend is exactly what
// requireLogs/requireErrors/requireTraces/requireProfiler/requireQuerier need
// to pass, and a handler invoked anyway panics.
type stubBackends struct{ err error }

// fail returns the error every method fails with.
func (s stubBackends) fail() error {
	if s.err == nil {
		panic("stubBackends: handler invoked without an error to fail with")
	}
	return s.err
}

var (
	_ gcpdata.LogsQuerier     = stubBackends{}
	_ gcpdata.ErrorsQuerier   = stubBackends{}
	_ gcpdata.TraceQuerier    = stubBackends{}
	_ gcpdata.ProfilerQuerier = stubBackends{}
	_ gcpdata.MetricsQuerier  = stubBackends{}
)

func (s stubBackends) QueryLogs(context.Context, string, string, int, string, string) (*gcpdata.LogQueryResult, error) {
	return nil, s.fail()
}

func (s stubBackends) QueryLogsByTrace(context.Context, string, string, string, int, string) (*gcpdata.LogQueryResult, error) {
	return nil, s.fail()
}

func (s stubBackends) QueryLogsByRequestID(context.Context, string, string, string, int, string) (*gcpdata.LogQueryResult, error) {
	return nil, s.fail()
}

func (s stubBackends) FindRequests(context.Context, gcpdata.FindRequestsParams) (*gcpdata.RequestList, error) {
	return nil, s.fail()
}

func (s stubBackends) ListServices(context.Context, string, string) (*gcpdata.ServiceList, error) {
	return nil, s.fail()
}

func (s stubBackends) SummarizeLogs(context.Context, string, string, gcpdata.ProgressFunc) (*gcpdata.LogsSummary, error) {
	return nil, s.fail()
}

func (s stubBackends) FindTracesFromLogs(context.Context, string, string, string, int, int) (*gcpdata.TraceFromLogsList, error) {
	return nil, s.fail()
}

func (s stubBackends) ListErrors(context.Context, string, gcpdata.ErrorWindow, int, string, string) (*gcpdata.ErrorGroupList, error) {
	return nil, s.fail()
}

func (s stubBackends) GetErrorGroup(context.Context, string, string, int, string) (*gcpdata.ErrorGroupDetail, error) {
	return nil, s.fail()
}

func (s stubBackends) AnalyzeErrorTrends(context.Context, string, gcpdata.ErrorWindow, int, string, string) (*gcpdata.ErrorTrendList, error) {
	return nil, s.fail()
}

func (s stubBackends) GetTrace(context.Context, string, string) (*gcpdata.TraceDetail, error) {
	return nil, s.fail()
}

func (s stubBackends) ListTraces(context.Context, string, string, string, string, time.Time, time.Time, int, string) (*gcpdata.TraceListResult, error) {
	return nil, s.fail()
}

func (s stubBackends) ListProfiles(context.Context, gcpdata.ListProfilesParams) (*gcpdata.ProfileListResult, error) {
	return nil, s.fail()
}

func (s stubBackends) GetOrFetchProfile(context.Context, string, string) (*profile.Profile, gcpdata.ProfileMeta, error) {
	return nil, gcpdata.ProfileMeta{}, s.fail()
}

func (s stubBackends) GetProfileOrDiff(context.Context, string, string, string) (*profile.Profile, gcpdata.ProfileMeta, error) {
	return nil, gcpdata.ProfileMeta{}, s.fail()
}

func (s stubBackends) CompareProfiles(context.Context, string, string, string, int, int) (*gcpdata.ProfileCompareResult, error) {
	return nil, s.fail()
}

func (s stubBackends) ComputeTrends(context.Context, gcpdata.ComputeTrendsParams, func(int, int, string)) (*gcpdata.ProfileTrendsResult, error) {
	return nil, s.fail()
}

func (stubBackends) Close() error { return nil }

func (s stubBackends) GetMetricDescriptor(context.Context, string, string) (gcpdata.MetricDescriptorBasic, error) {
	return gcpdata.MetricDescriptorBasic{}, s.fail()
}

func (s stubBackends) ListMetricDescriptors(context.Context, string, string, int) ([]gcpdata.MetricDescriptorInfo, error) {
	return nil, s.fail()
}

func (s stubBackends) QueryTimeSeries(context.Context, gcpdata.QueryTimeSeriesParams) ([]gcpdata.MetricTimeSeries, gcpdata.QueryWarnings, error) {
	return nil, gcpdata.QueryWarnings{}, s.fail()
}

func (s stubBackends) QueryTimeSeriesAggregated(context.Context, gcpdata.QueryTimeSeriesParams, metrics.AggregationSpec) ([]gcpdata.MetricTimeSeries, gcpdata.QueryWarnings, error) {
	return nil, gcpdata.QueryWarnings{}, s.fail()
}

func (s stubBackends) GetResourceLabels(context.Context, string, string) ([]string, error) {
	return nil, s.fail()
}
