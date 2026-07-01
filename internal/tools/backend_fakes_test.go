package tools

import (
	"context"
	"time"

	"github.com/google/pprof/profile"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

// This file provides programmable fakes for the four backend interfaces so
// logs/errors/trace/profiler tool handlers can be unit-tested without a real
// GCP client. Each method delegates to a func field; an unset field returns
// zero values, which is enough for registration-only tests. Handler tests set
// the specific func they exercise.

type fakeLogs struct {
	queryLogs          func(ctx context.Context, project, filter string, limit int, order, pageToken string) (*gcpdata.LogQueryResult, error)
	queryLogsByTrace   func(ctx context.Context, project, traceID, timeFilter string, limit int, pageToken string) (*gcpdata.LogQueryResult, error)
	queryLogsByReqID   func(ctx context.Context, project, requestID, timeFilter string, limit int, pageToken string) (*gcpdata.LogQueryResult, error)
	findRequests       func(ctx context.Context, project, urlPattern, method string, statusCode int, tracedOnly bool, timeFilter string, limit int) (*gcpdata.RequestList, error)
	listServices       func(ctx context.Context, project, timeFilter string) (*gcpdata.ServiceList, error)
	summarizeLogs      func(ctx context.Context, project, filter string, onProgress gcpdata.ProgressFunc) (*gcpdata.LogsSummary, error)
	findTracesFromLogs func(ctx context.Context, project, filter, timeFilter string, scanLimit, resultLimit int) (*gcpdata.TraceFromLogsList, error)
}

func (f fakeLogs) QueryLogs(ctx context.Context, project, filter string, limit int, order, pageToken string) (*gcpdata.LogQueryResult, error) {
	if f.queryLogs == nil {
		return nil, nil
	}
	return f.queryLogs(ctx, project, filter, limit, order, pageToken)
}

func (f fakeLogs) QueryLogsByTrace(ctx context.Context, project, traceID, timeFilter string, limit int, pageToken string) (*gcpdata.LogQueryResult, error) {
	if f.queryLogsByTrace == nil {
		return nil, nil
	}
	return f.queryLogsByTrace(ctx, project, traceID, timeFilter, limit, pageToken)
}

func (f fakeLogs) QueryLogsByRequestID(ctx context.Context, project, requestID, timeFilter string, limit int, pageToken string) (*gcpdata.LogQueryResult, error) {
	if f.queryLogsByReqID == nil {
		return nil, nil
	}
	return f.queryLogsByReqID(ctx, project, requestID, timeFilter, limit, pageToken)
}

func (f fakeLogs) FindRequests(ctx context.Context, project, urlPattern, method string, statusCode int, tracedOnly bool, timeFilter string, limit int) (*gcpdata.RequestList, error) {
	if f.findRequests == nil {
		return nil, nil
	}
	return f.findRequests(ctx, project, urlPattern, method, statusCode, tracedOnly, timeFilter, limit)
}

func (f fakeLogs) ListServices(ctx context.Context, project, timeFilter string) (*gcpdata.ServiceList, error) {
	if f.listServices == nil {
		return nil, nil
	}
	return f.listServices(ctx, project, timeFilter)
}

func (f fakeLogs) SummarizeLogs(ctx context.Context, project, filter string, onProgress gcpdata.ProgressFunc) (*gcpdata.LogsSummary, error) {
	if f.summarizeLogs == nil {
		return nil, nil
	}
	return f.summarizeLogs(ctx, project, filter, onProgress)
}

func (f fakeLogs) FindTracesFromLogs(ctx context.Context, project, filter, timeFilter string, scanLimit, resultLimit int) (*gcpdata.TraceFromLogsList, error) {
	if f.findTracesFromLogs == nil {
		return nil, nil
	}
	return f.findTracesFromLogs(ctx, project, filter, timeFilter, scanLimit, resultLimit)
}

type fakeErrors struct {
	listErrors    func(ctx context.Context, project string, timeRangeHours, limit int, serviceFilter, versionFilter string) (*gcpdata.ErrorGroupList, error)
	getErrorGroup func(ctx context.Context, project, groupID string, limit int, pageToken string) (*gcpdata.ErrorGroupDetail, error)
	analyzeTrends func(ctx context.Context, project string, timeRangeHours, limit int, serviceFilter, versionFilter string) (*gcpdata.ErrorTrendList, error)
}

func (f fakeErrors) ListErrors(ctx context.Context, project string, timeRangeHours, limit int, serviceFilter, versionFilter string) (*gcpdata.ErrorGroupList, error) {
	if f.listErrors == nil {
		return nil, nil
	}
	return f.listErrors(ctx, project, timeRangeHours, limit, serviceFilter, versionFilter)
}

func (f fakeErrors) GetErrorGroup(ctx context.Context, project, groupID string, limit int, pageToken string) (*gcpdata.ErrorGroupDetail, error) {
	if f.getErrorGroup == nil {
		return nil, nil
	}
	return f.getErrorGroup(ctx, project, groupID, limit, pageToken)
}

func (f fakeErrors) AnalyzeErrorTrends(ctx context.Context, project string, timeRangeHours, limit int, serviceFilter, versionFilter string) (*gcpdata.ErrorTrendList, error) {
	if f.analyzeTrends == nil {
		return nil, nil
	}
	return f.analyzeTrends(ctx, project, timeRangeHours, limit, serviceFilter, versionFilter)
}

type fakeTraces struct {
	getTrace  func(ctx context.Context, project, traceID string) (*gcpdata.TraceDetail, error)
	listTrace func(ctx context.Context, project, filter, view, orderBy string, startTime, endTime time.Time, pageSize int, pageToken string) (*gcpdata.TraceListResult, error)
}

func (f fakeTraces) GetTrace(ctx context.Context, project, traceID string) (*gcpdata.TraceDetail, error) {
	if f.getTrace == nil {
		return nil, nil
	}
	return f.getTrace(ctx, project, traceID)
}

func (f fakeTraces) ListTraces(ctx context.Context, project, filter, view, orderBy string, startTime, endTime time.Time, pageSize int, pageToken string) (*gcpdata.TraceListResult, error) {
	if f.listTrace == nil {
		return nil, nil
	}
	return f.listTrace(ctx, project, filter, view, orderBy, startTime, endTime, pageSize, pageToken)
}

type fakeProfiler struct {
	listProfiles func(ctx context.Context, project, profileType, target, startTime, endTime string, pageSize int, pageToken string) (*gcpdata.ProfileListResult, error)
	getOrFetch   func(ctx context.Context, project, profileName string) (*profile.Profile, gcpdata.ProfileMeta, error)
	compare      func(ctx context.Context, project, currentID, baseID string, valueIndex, topN int) (*gcpdata.ProfileCompareResult, *profile.Profile, error)
	computeTrend func(ctx context.Context, project, profileType, target, functionFilter string, valueIndex, maxProfiles, maxFunctions int, progressFn func(current, total int, msg string)) (*gcpdata.ProfileTrendsResult, error)
	cached       map[string]*profile.Profile
}

func (f fakeProfiler) ListProfiles(ctx context.Context, project, profileType, target, startTime, endTime string, pageSize int, pageToken string) (*gcpdata.ProfileListResult, error) {
	if f.listProfiles == nil {
		return nil, nil
	}
	return f.listProfiles(ctx, project, profileType, target, startTime, endTime, pageSize, pageToken)
}

func (f fakeProfiler) GetOrFetchProfile(ctx context.Context, project, profileName string) (*profile.Profile, gcpdata.ProfileMeta, error) {
	if f.getOrFetch == nil {
		return nil, gcpdata.ProfileMeta{}, nil
	}
	return f.getOrFetch(ctx, project, profileName)
}

func (f fakeProfiler) CompareProfiles(ctx context.Context, project, currentID, baseID string, valueIndex, topN int) (*gcpdata.ProfileCompareResult, *profile.Profile, error) {
	if f.compare == nil {
		return nil, nil, nil
	}
	return f.compare(ctx, project, currentID, baseID, valueIndex, topN)
}

func (f fakeProfiler) ComputeTrends(ctx context.Context, project, profileType, target, functionFilter string, valueIndex, maxProfiles, maxFunctions int, progressFn func(current, total int, msg string)) (*gcpdata.ProfileTrendsResult, error) {
	if f.computeTrend == nil {
		return nil, nil
	}
	return f.computeTrend(ctx, project, profileType, target, functionFilter, valueIndex, maxProfiles, maxFunctions, progressFn)
}

func (f fakeProfiler) CacheProfile(key string, p *profile.Profile, _ gcpdata.ProfileMeta) {
	if f.cached != nil {
		f.cached[key] = p
	}
}

// allFakeBackends returns a Deps with every backend set to an empty (non-nil)
// fake, suitable for registration-only tests where handlers are not invoked.
func allFakeBackends() Deps {
	return Deps{
		Logs:     fakeLogs{},
		Errors:   fakeErrors{},
		Traces:   fakeTraces{},
		Profiler: fakeProfiler{},
	}
}
