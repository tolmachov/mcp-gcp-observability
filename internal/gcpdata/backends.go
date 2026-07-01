package gcpdata

import (
	"context"
	"time"

	cloudprofiler "cloud.google.com/go/cloudprofiler/apiv2"
	errorreporting "cloud.google.com/go/errorreporting/apiv1beta1"
	logging "cloud.google.com/go/logging/apiv2"
	cloudtrace "cloud.google.com/go/trace/apiv1"
	"github.com/google/pprof/profile"
)

// This file defines one interface per GCP backend (logging, error reporting,
// cloud trace, cloud profiler) plus a concrete implementation that wraps the
// corresponding SDK client. It mirrors MetricsQuerier/MonitoringQuerier: tool
// handlers depend on the interface so they can be unit-tested with fakes, while
// the concrete wrappers forward to the package's free functions.

// LogsQuerier abstracts Cloud Logging read operations used by tool handlers.
type LogsQuerier interface {
	QueryLogs(ctx context.Context, project, filter string, limit int, order, pageToken string) (*LogQueryResult, error)
	QueryLogsByTrace(ctx context.Context, project, traceID, timeFilter string, limit int, pageToken string) (*LogQueryResult, error)
	QueryLogsByRequestID(ctx context.Context, project, requestID, timeFilter string, limit int, pageToken string) (*LogQueryResult, error)
	FindRequests(ctx context.Context, project, urlPattern, method string, statusCode int, tracedOnly bool, timeFilter string, limit int) (*RequestList, error)
	ListServices(ctx context.Context, project, timeFilter string) (*ServiceList, error)
	SummarizeLogs(ctx context.Context, project, filter string, onProgress ProgressFunc) (*LogsSummary, error)
	// FindTracesFromLogs scans logs to correlate trace IDs; it runs on the
	// logging client, so it lives here rather than on TraceQuerier.
	FindTracesFromLogs(ctx context.Context, project, filter, timeFilter string, scanLimit, resultLimit int) (*TraceFromLogsList, error)
}

// ErrorsQuerier abstracts Error Reporting operations used by tool handlers.
type ErrorsQuerier interface {
	ListErrors(ctx context.Context, project string, timeRangeHours, limit int, serviceFilter, versionFilter string) (*ErrorGroupList, error)
	GetErrorGroup(ctx context.Context, project, groupID string, limit int, pageToken string) (*ErrorGroupDetail, error)
	AnalyzeErrorTrends(ctx context.Context, project string, timeRangeHours, limit int, serviceFilter, versionFilter string) (*ErrorTrendList, error)
}

// TraceQuerier abstracts Cloud Trace operations used by tool handlers.
type TraceQuerier interface {
	GetTrace(ctx context.Context, project, traceID string) (*TraceDetail, error)
	ListTraces(ctx context.Context, project, filter, view, orderBy string, startTime, endTime time.Time, pageSize int, pageToken string) (*TraceListResult, error)
}

// ProfilerQuerier abstracts Cloud Profiler operations used by tool handlers.
// The profile cache is an implementation detail owned by the concrete querier,
// so handlers no longer thread a *ProfileCache through every call.
type ProfilerQuerier interface {
	ListProfiles(ctx context.Context, project, profileType, target, startTime, endTime string, pageSize int, pageToken string) (*ProfileListResult, error)
	GetOrFetchProfile(ctx context.Context, project, profileName string) (*profile.Profile, ProfileMeta, error)
	CompareProfiles(ctx context.Context, project, currentID, baseID string, valueIndex, topN int) (*ProfileCompareResult, *profile.Profile, error)
	ComputeTrends(ctx context.Context, project, profileType, target, functionFilter string, valueIndex, maxProfiles, maxFunctions int, progressFn func(current, total int, msg string)) (*ProfileTrendsResult, error)
	// CacheProfile stores a profile (e.g. a computed diff) so a later
	// GetOrFetchProfile for the same key can serve it without a download.
	CacheProfile(key string, p *profile.Profile, meta ProfileMeta)
}

// LoggingQuerier implements LogsQuerier against a real Cloud Logging client.
type LoggingQuerier struct{ client *logging.Client }

// NewLoggingQuerier wraps a Cloud Logging client as a LogsQuerier.
func NewLoggingQuerier(client *logging.Client) *LoggingQuerier {
	if client == nil {
		panic("NewLoggingQuerier: client must not be nil")
	}
	return &LoggingQuerier{client: client}
}

func (q *LoggingQuerier) QueryLogs(ctx context.Context, project, filter string, limit int, order, pageToken string) (*LogQueryResult, error) {
	return QueryLogs(ctx, q.client, project, filter, limit, order, pageToken)
}

func (q *LoggingQuerier) QueryLogsByTrace(ctx context.Context, project, traceID, timeFilter string, limit int, pageToken string) (*LogQueryResult, error) {
	return QueryLogsByTrace(ctx, q.client, project, traceID, timeFilter, limit, pageToken)
}

func (q *LoggingQuerier) QueryLogsByRequestID(ctx context.Context, project, requestID, timeFilter string, limit int, pageToken string) (*LogQueryResult, error) {
	return QueryLogsByRequestID(ctx, q.client, project, requestID, timeFilter, limit, pageToken)
}

func (q *LoggingQuerier) FindRequests(ctx context.Context, project, urlPattern, method string, statusCode int, tracedOnly bool, timeFilter string, limit int) (*RequestList, error) {
	return FindRequests(ctx, q.client, project, urlPattern, method, statusCode, tracedOnly, timeFilter, limit)
}

func (q *LoggingQuerier) ListServices(ctx context.Context, project, timeFilter string) (*ServiceList, error) {
	return ListServices(ctx, q.client, project, timeFilter)
}

func (q *LoggingQuerier) SummarizeLogs(ctx context.Context, project, filter string, onProgress ProgressFunc) (*LogsSummary, error) {
	return SummarizeLogs(ctx, q.client, project, filter, onProgress)
}

func (q *LoggingQuerier) FindTracesFromLogs(ctx context.Context, project, filter, timeFilter string, scanLimit, resultLimit int) (*TraceFromLogsList, error) {
	return FindTracesFromLogs(ctx, q.client, project, filter, timeFilter, scanLimit, resultLimit)
}

// ErrorReportingQuerier implements ErrorsQuerier against a real Error Reporting client.
type ErrorReportingQuerier struct {
	client *errorreporting.ErrorStatsClient
}

// NewErrorReportingQuerier wraps an Error Reporting stats client as an ErrorsQuerier.
func NewErrorReportingQuerier(client *errorreporting.ErrorStatsClient) *ErrorReportingQuerier {
	if client == nil {
		panic("NewErrorReportingQuerier: client must not be nil")
	}
	return &ErrorReportingQuerier{client: client}
}

func (q *ErrorReportingQuerier) ListErrors(ctx context.Context, project string, timeRangeHours, limit int, serviceFilter, versionFilter string) (*ErrorGroupList, error) {
	return ListErrors(ctx, q.client, project, timeRangeHours, limit, serviceFilter, versionFilter)
}

func (q *ErrorReportingQuerier) GetErrorGroup(ctx context.Context, project, groupID string, limit int, pageToken string) (*ErrorGroupDetail, error) {
	return GetErrorGroup(ctx, q.client, project, groupID, limit, pageToken)
}

func (q *ErrorReportingQuerier) AnalyzeErrorTrends(ctx context.Context, project string, timeRangeHours, limit int, serviceFilter, versionFilter string) (*ErrorTrendList, error) {
	return AnalyzeErrorTrends(ctx, q.client, project, timeRangeHours, limit, serviceFilter, versionFilter)
}

// CloudTraceQuerier implements TraceQuerier against a real Cloud Trace client.
type CloudTraceQuerier struct{ client *cloudtrace.Client }

// NewCloudTraceQuerier wraps a Cloud Trace client as a TraceQuerier.
func NewCloudTraceQuerier(client *cloudtrace.Client) *CloudTraceQuerier {
	if client == nil {
		panic("NewCloudTraceQuerier: client must not be nil")
	}
	return &CloudTraceQuerier{client: client}
}

func (q *CloudTraceQuerier) GetTrace(ctx context.Context, project, traceID string) (*TraceDetail, error) {
	return GetTrace(ctx, q.client, project, traceID)
}

func (q *CloudTraceQuerier) ListTraces(ctx context.Context, project, filter, view, orderBy string, startTime, endTime time.Time, pageSize int, pageToken string) (*TraceListResult, error) {
	return ListTraces(ctx, q.client, project, filter, view, orderBy, startTime, endTime, pageSize, pageToken)
}

// CloudProfilerQuerier implements ProfilerQuerier against a real Cloud Profiler
// export client. It owns the profile cache shared across all profiler calls.
type CloudProfilerQuerier struct {
	svc   *cloudprofiler.ExportClient
	cache *ProfileCache
}

// NewCloudProfilerQuerier wraps a Cloud Profiler export client as a
// ProfilerQuerier, creating an internal profile cache of the given size.
func NewCloudProfilerQuerier(svc *cloudprofiler.ExportClient, cacheSize int) *CloudProfilerQuerier {
	if svc == nil {
		panic("NewCloudProfilerQuerier: svc must not be nil")
	}
	return &CloudProfilerQuerier{svc: svc, cache: NewProfileCache(cacheSize)}
}

func (q *CloudProfilerQuerier) ListProfiles(ctx context.Context, project, profileType, target, startTime, endTime string, pageSize int, pageToken string) (*ProfileListResult, error) {
	return ListProfiles(ctx, q.svc, project, profileType, target, startTime, endTime, pageSize, pageToken)
}

func (q *CloudProfilerQuerier) GetOrFetchProfile(ctx context.Context, project, profileName string) (*profile.Profile, ProfileMeta, error) {
	return GetOrFetchProfile(ctx, q.svc, q.cache, project, profileName)
}

func (q *CloudProfilerQuerier) CompareProfiles(ctx context.Context, project, currentID, baseID string, valueIndex, topN int) (*ProfileCompareResult, *profile.Profile, error) {
	return CompareProfiles(ctx, q.svc, q.cache, project, currentID, baseID, valueIndex, topN)
}

func (q *CloudProfilerQuerier) ComputeTrends(ctx context.Context, project, profileType, target, functionFilter string, valueIndex, maxProfiles, maxFunctions int, progressFn func(current, total int, msg string)) (*ProfileTrendsResult, error) {
	return ComputeTrends(ctx, q.svc, q.cache, project, profileType, target, functionFilter, valueIndex, maxProfiles, maxFunctions, progressFn)
}

func (q *CloudProfilerQuerier) CacheProfile(key string, p *profile.Profile, meta ProfileMeta) {
	q.cache.Put(key, p, meta)
}
