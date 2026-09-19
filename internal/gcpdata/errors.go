package gcpdata

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/api/iterator"

	errorreporting "cloud.google.com/go/errorreporting/apiv1beta1"
	"cloud.google.com/go/errorreporting/apiv1beta1/errorreportingpb"
)

const errorReportingTimeout = 30 * time.Second

type ErrorWindow string

const (
	ErrorWindow1H  ErrorWindow = "1h"
	ErrorWindow6H  ErrorWindow = "6h"
	ErrorWindow24H ErrorWindow = "24h"
	ErrorWindow7D  ErrorWindow = "7d"
	ErrorWindow30D ErrorWindow = "30d"
)

type ErrorWindowSpec struct {
	Period errorreportingpb.QueryTimeRange_Period
	Span   time.Duration
	Bucket time.Duration
}

func (w ErrorWindow) Spec() (ErrorWindowSpec, bool) {
	switch w {
	case ErrorWindow1H:
		return ErrorWindowSpec{errorreportingpb.QueryTimeRange_PERIOD_1_HOUR, time.Hour, 5 * time.Minute}, true
	case ErrorWindow6H:
		return ErrorWindowSpec{errorreportingpb.QueryTimeRange_PERIOD_6_HOURS, 6 * time.Hour, 30 * time.Minute}, true
	case ErrorWindow24H:
		return ErrorWindowSpec{errorreportingpb.QueryTimeRange_PERIOD_1_DAY, 24 * time.Hour, time.Hour}, true
	case ErrorWindow7D:
		return ErrorWindowSpec{errorreportingpb.QueryTimeRange_PERIOD_1_WEEK, 7 * 24 * time.Hour, 6 * time.Hour}, true
	case ErrorWindow30D:
		return ErrorWindowSpec{errorreportingpb.QueryTimeRange_PERIOD_30_DAYS, 30 * 24 * time.Hour, 24 * time.Hour}, true
	default:
		return ErrorWindowSpec{}, false
	}
}

// ListErrors lists error groups sorted by occurrence count.
// Error Reporting only supports lookback periods ending at "now"; callers
// must not promise historical absolute-window semantics on top of this API.
func ListErrors(ctx context.Context, client *errorreporting.ErrorStatsClient, project string, window ErrorWindow, limit int, serviceFilter, versionFilter string) (*ErrorGroupList, error) {
	ctx, cancel := context.WithTimeout(ctx, errorReportingTimeout)
	defer cancel()
	spec, ok := window.Spec()
	if !ok {
		return nil, fmt.Errorf("invalid error window %q", window)
	}
	timeRangeBegin := time.Now().Add(-spec.Span).UTC()

	req := &errorreportingpb.ListGroupStatsRequest{
		ProjectName: fmt.Sprintf("projects/%s", project),
		TimeRange: &errorreportingpb.QueryTimeRange{
			Period: spec.Period,
		},
		PageSize: safeInt32(limit),
		Order:    errorreportingpb.ErrorGroupOrder_COUNT_DESC,
	}

	if serviceFilter != "" || versionFilter != "" {
		req.ServiceFilter = &errorreportingpb.ServiceContextFilter{
			Service: serviceFilter,
			Version: versionFilter,
		}
	}

	it := client.ListGroupStats(ctx, req)

	var groups []ErrorGroup
	for i := 0; i < limit; i++ {
		stats, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("iterating error groups: %w", err)
		}

		g := ErrorGroup{
			Count:     stats.Count,
			FirstSeen: formatTimestamp(stats.FirstSeenTime),
			LastSeen:  formatTimestamp(stats.LastSeenTime),
		}

		if stats.Group != nil {
			g.GroupID = stats.Group.GroupId
		}

		// Extract service and versions from affected services
		if len(stats.AffectedServices) > 0 {
			g.Service = stats.AffectedServices[0].Service
			versions := make(map[string]bool)
			for _, svc := range stats.AffectedServices {
				if svc.Version != "" {
					versions[svc.Version] = true
				}
			}
			for v := range versions {
				g.AffectedVersions = append(g.AffectedVersions, v)
			}
			sort.Strings(g.AffectedVersions)
		}

		// Extract message from representative event
		if stats.Representative != nil {
			g.Message = stats.Representative.Message
		}

		groups = append(groups, g)
	}

	result := &ErrorGroupList{Count: len(groups), Groups: groups, Window: window, TimeRangeBegin: timeRangeBegin.Format(time.RFC3339)}
	if resp, ok := it.Response.(*errorreportingpb.ListGroupStatsResponse); ok && resp.TimeRangeBegin != nil {
		result.TimeRangeBegin = formatTimestamp(resp.TimeRangeBegin)
	}
	if tok := it.PageInfo().Token; tok != "" {
		result.Truncated = true
		result.TruncationHint = fmt.Sprintf("Showing the first %d error group(s). More groups are available for this lookback window; narrow service/version filters or lower the time range for a more focused result.", limit)
	}
	return result, nil
}

// GetErrorGroup retrieves details for a specific error group.
func GetErrorGroup(ctx context.Context, client *errorreporting.ErrorStatsClient, project, groupID string, limit int, pageToken string) (*ErrorGroupDetail, error) {
	ctx, cancel := context.WithTimeout(ctx, errorReportingTimeout)
	defer cancel()

	req := &errorreportingpb.ListEventsRequest{
		ProjectName: fmt.Sprintf("projects/%s", project),
		GroupId:     groupID,
		PageSize:    safeInt32(limit),
		PageToken:   pageToken,
	}

	it := client.ListEvents(ctx, req)

	detail := &ErrorGroupDetail{
		GroupID: groupID,
	}

	var instances []ErrorInstance
	for i := 0; i < limit; i++ {
		event, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("iterating error events: %w", err)
		}

		headline, stackTrace := splitReportedErrorMessage(event.Message)
		inst := ErrorInstance{
			Timestamp:  formatTimestamp(event.EventTime),
			Message:    headline,
			StackTrace: stackTrace,
		}

		if event.ServiceContext != nil {
			inst.Service = event.ServiceContext.Service
			inst.Version = event.ServiceContext.Version
		}
		if event.Context != nil {
			inst.Context = convertErrorContext(event.Context)
		}

		// Set detail-level fields from first event
		if len(instances) == 0 {
			detail.Message = headline
			if event.ServiceContext != nil {
				detail.Service = event.ServiceContext.Service
			}
		}

		instances = append(instances, inst)
	}

	if len(instances) == 0 {
		return nil, fmt.Errorf("no events found for error group %q in the current time window: the group may have been resolved or events may have aged out of retention", groupID)
	}

	detail.Instances = instances
	if tok := it.PageInfo().Token; tok != "" {
		detail.Truncated = true
		detail.NextPageToken = tok
		detail.TruncationHint = fmt.Sprintf("Showing the first %d event(s) for this error group. More events are available; repeat with a larger limit if you need a broader sample.", limit)
	}
	if resp := it.Response; resp != nil {
		if lr, ok := resp.(*errorreportingpb.ListEventsResponse); ok && lr.TimeRangeBegin != nil {
			detail.TimeRangeBegin = formatTimestamp(lr.TimeRangeBegin)
		}
	}
	return detail, nil
}

func splitReportedErrorMessage(message string) (headline, stackTrace string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return "", ""
	}
	lines := strings.Split(message, "\n")
	headline = strings.TrimSpace(lines[0])
	if headline == "" {
		headline = message
	}
	if len(lines) > 1 {
		stackTrace = message
	}
	return headline, stackTrace
}

func convertErrorContext(ctx *errorreportingpb.ErrorContext) *ErrorContext {
	if ctx == nil {
		return nil
	}
	out := &ErrorContext{
		User: ctx.User,
	}
	if h := ctx.HttpRequest; h != nil {
		out.HTTPRequest = &ErrorHTTPRequest{
			Method:             h.Method,
			URL:                h.Url,
			UserAgent:          h.UserAgent,
			Referrer:           h.Referrer,
			ResponseStatusCode: h.ResponseStatusCode,
			RemoteIP:           h.RemoteIp,
		}
	}
	if rl := ctx.ReportLocation; rl != nil {
		out.ReportLocation = &ErrorSourceLocation{
			FilePath:     rl.FilePath,
			LineNumber:   rl.LineNumber,
			FunctionName: rl.FunctionName,
		}
	}
	if out.User == "" && out.HTTPRequest == nil && out.ReportLocation == nil {
		return nil
	}
	return out
}
