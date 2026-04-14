package gcpdata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/encoding/protojson"

	logging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	logtypepb "google.golang.org/genproto/googleapis/logging/type"
)

const logQueryTimeout = 30 * time.Second

var requestIDFieldPaths = []string{
	"jsonPayload.request_id",
	"jsonPayload.requestId",
	"labels.request_id",
	"labels.requestId",
}

// QueryLogs executes an arbitrary Cloud Logging query.
func QueryLogs(ctx context.Context, client *logging.Client, project, filter string, limit int, order, pageToken string) (*LogQueryResult, error) {
	ctx, cancel := context.WithTimeout(ctx, logQueryTimeout)
	defer cancel()

	var orderBy string
	switch order {
	case "asc":
		orderBy = "timestamp asc"
	case "desc":
		orderBy = "timestamp desc"
	default:
		return nil, fmt.Errorf("invalid order %q: must be \"asc\" or \"desc\"", order)
	}

	req := &loggingpb.ListLogEntriesRequest{
		ResourceNames: []string{fmt.Sprintf("projects/%s", project)},
		Filter:        filter,
		OrderBy:       orderBy,
		PageSize:      safeInt32(limit),
		PageToken:     pageToken,
	}

	return fetchLogEntries(ctx, client, req, limit)
}

// QueryLogsByTrace retrieves all logs for a given trace ID.
func QueryLogsByTrace(ctx context.Context, client *logging.Client, project, traceID, timeFilter string, limit int, pageToken string) (*LogQueryResult, error) {
	ctx, cancel := context.WithTimeout(ctx, logQueryTimeout)
	defer cancel()

	filter := AppendFilter(
		fmt.Sprintf(`trace="projects/%s/traces/%s"`, project, EscapeFilterValue(traceID)),
		timeFilter,
	)

	req := &loggingpb.ListLogEntriesRequest{
		ResourceNames: []string{fmt.Sprintf("projects/%s", project)},
		Filter:        filter,
		OrderBy:       "timestamp asc",
		PageSize:      safeInt32(limit),
		PageToken:     pageToken,
	}

	return fetchLogEntries(ctx, client, req, limit)
}

// QueryLogsByRequestID retrieves all logs for a given request ID.
func QueryLogsByRequestID(ctx context.Context, client *logging.Client, project, requestID, timeFilter string, limit int, pageToken string) (*LogQueryResult, error) {
	ctx, cancel := context.WithTimeout(ctx, logQueryTimeout)
	defer cancel()

	filter := AppendFilter(requestIDFilter(requestID), timeFilter)

	req := &loggingpb.ListLogEntriesRequest{
		ResourceNames: []string{fmt.Sprintf("projects/%s", project)},
		Filter:        filter,
		OrderBy:       "timestamp asc",
		PageSize:      safeInt32(limit),
		PageToken:     pageToken,
	}

	return fetchLogEntries(ctx, client, req, limit)
}

// FindRequests finds HTTP requests matching the given URL pattern.
func FindRequests(ctx context.Context, client *logging.Client, project, urlPattern, method string, statusCode int, tracedOnly bool, timeFilter string, limit int) (*RequestList, error) {
	ctx, cancel := context.WithTimeout(ctx, logQueryTimeout)
	defer cancel()

	parts := []string{
		fmt.Sprintf(`httpRequest.requestUrl:"%s"`, EscapeFilterValue(urlPattern)),
	}
	if method != "" {
		parts = append(parts, fmt.Sprintf(`httpRequest.requestMethod="%s"`, EscapeFilterValue(method)))
	}
	if statusCode > 0 {
		parts = append(parts, fmt.Sprintf(`httpRequest.status=%d`, statusCode))
	}
	if tracedOnly {
		parts = append(parts, `trace!=""`)
	}

	filter := AppendFilter(strings.Join(parts, " AND "), timeFilter)

	// Request more entries than limit to account for entries without httpRequest
	pageSize := limit * 3
	if pageSize > 1000 {
		pageSize = 1000
	}

	req := &loggingpb.ListLogEntriesRequest{
		ResourceNames: []string{fmt.Sprintf("projects/%s", project)},
		Filter:        filter,
		OrderBy:       "timestamp desc",
		PageSize:      safeInt32(pageSize),
	}

	it := client.ListLogEntries(ctx, req)

	var requests []RequestInfo
	truncated := false
	for len(requests) <= limit {
		entry, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("iterating log entries: %w", err)
		}

		if entry.HttpRequest == nil {
			continue
		}

		var resType string
		if entry.Resource != nil {
			resType = entry.Resource.Type
		}

		ri := RequestInfo{
			Timestamp:    formatTimestamp(entry.Timestamp),
			Method:       entry.HttpRequest.RequestMethod,
			URL:          entry.HttpRequest.RequestUrl,
			Status:       int(entry.HttpRequest.Status),
			Latency:      formatLatency(entry.HttpRequest.Latency),
			ResourceType: resType,
		}

		// Extract trace_id from full trace path
		if entry.Trace != "" {
			ri.TraceID = extractTraceID(entry.Trace)
		}

		ri.RequestID = extractRequestID(entry)

		// Extract service name from resource labels
		ri.Service = extractServiceName(entry)

		if len(requests) == limit {
			truncated = true
			break
		}
		requests = append(requests, ri)
	}
	result := &RequestList{
		Count:    len(requests),
		Requests: requests,
	}
	if truncated {
		result.Truncated = true
		result.TruncationHint = fmt.Sprintf("Showing the first %d matching request(s). More matches exist; narrow the time range or URL pattern for a more focused sample set.", limit)
	}
	return result, nil
}

func requestIDFilter(requestID string) string {
	escaped := EscapeFilterValue(requestID)
	parts := make([]string, 0, len(requestIDFieldPaths))
	for _, path := range requestIDFieldPaths {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, path, escaped))
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

func extractRequestID(entry *loggingpb.LogEntry) string {
	if entry == nil {
		return ""
	}
	if jp := entry.GetJsonPayload(); jp != nil {
		for _, key := range []string{"request_id", "requestId"} {
			if v, ok := jp.Fields[key]; ok {
				if s := v.GetStringValue(); s != "" {
					return s
				}
			}
		}
	}
	for _, key := range []string{"request_id", "requestId"} {
		if entry.Labels != nil {
			if s := entry.Labels[key]; s != "" {
				return s
			}
		}
	}
	return ""
}

// fetchLogEntries is a shared helper for querying log entries.
func fetchLogEntries(ctx context.Context, client *logging.Client, req *loggingpb.ListLogEntriesRequest, limit int) (*LogQueryResult, error) {
	it := client.ListLogEntries(ctx, req)

	var entries []LogEntry
	for i := 0; i < limit; i++ {
		entry, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("iterating log entries: %w", err)
		}
		entries = append(entries, convertLogEntry(entry))
	}

	result := &LogQueryResult{
		Count:   len(entries),
		Entries: entries,
	}

	if tok := it.PageInfo().Token; tok != "" {
		result.NextPageToken = tok
	}

	return result, nil
}

// ListServices discovers unique services by scanning recent logs for common GCP resource types.
func ListServices(ctx context.Context, client *logging.Client, project, timeFilter string) (*ServiceList, error) {
	ctx, cancel := context.WithTimeout(ctx, logQueryTimeout)
	defer cancel()

	const maxServicesScan = 1000

	filter := AppendFilter(
		`(resource.type="k8s_container" OR resource.type="cloud_run_revision" OR resource.type="cloud_function" OR resource.type="gae_app" OR resource.type="gce_instance")`,
		timeFilter,
	)

	req := &loggingpb.ListLogEntriesRequest{
		ResourceNames: []string{fmt.Sprintf("projects/%s", project)},
		Filter:        filter,
		OrderBy:       "timestamp desc",
		PageSize:      maxServicesScan,
	}

	it := client.ListLogEntries(ctx, req)

	scanned := 0
	seen := make(map[string]ServiceInfo)
	for i := 0; i < maxServicesScan; i++ {
		entry, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("iterating log entries: %w", err)
		}

		scanned++

		if entry.Resource == nil {
			continue
		}

		labels := entry.Resource.Labels
		resType := entry.Resource.Type

		var key string
		var info ServiceInfo

		switch resType {
		case "cloud_run_revision":
			name := labels["service_name"]
			if name == "" {
				continue
			}
			key = "cloud_run:" + name
			info = ServiceInfo{Name: name, ResourceType: resType}
		case "k8s_container":
			container := labels["container_name"]
			if container == "" {
				continue
			}
			ns := labels["namespace_name"]
			key = "k8s:" + ns + "/" + container
			info = ServiceInfo{Name: container, ResourceType: resType, Namespace: ns}
		case "cloud_function":
			name := labels["function_name"]
			if name == "" {
				continue
			}
			key = "cloud_function:" + name
			info = ServiceInfo{Name: name, ResourceType: resType}
		case "gae_app":
			module := labels["module_id"]
			if module == "" {
				continue
			}
			version := labels["version_id"]
			key = "gae_app:" + module
			name := module
			if version != "" {
				name = module + "/" + version
			}
			info = ServiceInfo{Name: name, ResourceType: resType}
		case "gce_instance":
			name := labels["instance_id"]
			if name == "" {
				continue
			}
			key = "gce_instance:" + name
			info = ServiceInfo{Name: name, ResourceType: resType}
		default:
			continue
		}

		if _, exists := seen[key]; !exists {
			seen[key] = info
		}
	}

	services := make([]ServiceInfo, 0, len(seen))
	for _, info := range seen {
		services = append(services, info)
	}
	sort.Slice(services, func(i, j int) bool {
		return services[i].Name < services[j].Name
	})

	result := &ServiceList{
		Count:    len(services),
		Services: services,
	}
	if scanned >= maxServicesScan {
		result.Truncated = true
		result.TruncationHint = fmt.Sprintf("Service list is based on %d sampled log entries and may be incomplete. Use logs_query with a specific resource.type filter to find services not listed here.", maxServicesScan)
	}
	return result, nil
}

// ProgressFunc is called periodically during a scan with the number of entries
// processed so far and the scan upper bound. A nil func disables reporting.
type ProgressFunc func(scanned, total int)

// SummarizeLogs aggregates log statistics by scanning up to maxScan recent entries matching the filter.
// If onProgress is non-nil, it is invoked every 100 scanned entries with (scanned, maxScan).
func SummarizeLogs(ctx context.Context, client *logging.Client, project, filter string, onProgress ProgressFunc) (*LogsSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, logQueryTimeout)
	defer cancel()

	const maxScan = 1000

	req := &loggingpb.ListLogEntriesRequest{
		ResourceNames: []string{fmt.Sprintf("projects/%s", project)},
		Filter:        filter,
		OrderBy:       "timestamp desc",
		PageSize:      maxScan,
	}

	it := client.ListLogEntries(ctx, req)

	severityDist := make(map[string]int)
	serviceCounts := make(map[string]int)
	errorMessages := make(map[string]int)
	var samples []LogEntry
	total := 0

	for i := 0; i < maxScan; i++ {
		entry, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("iterating log entries: %w", err)
		}
		total++

		// Count severity
		sev := entry.Severity.String()
		severityDist[sev]++

		// Count services
		svcName := extractServiceName(entry)
		if svcName != "" {
			serviceCounts[svcName]++
		}

		// Collect error messages
		if entry.Severity >= logtypepb.LogSeverity_ERROR {
			msg := extractErrorMessage(entry)
			if msg != "" {
				errorMessages[msg]++
			}
		}

		// Collect up to 5 samples
		if len(samples) < 5 {
			samples = append(samples, convertLogEntry(entry))
		}

		// Throttled progress reporting: every 100 entries.
		if onProgress != nil && total%100 == 0 {
			onProgress(total, maxScan)
		}
	}

	truncated := total >= maxScan
	summary := &LogsSummary{
		TotalEntries:         total,
		SeverityDistribution: severityDist,
		TopServices:          topN(serviceCounts, 10),
		TopErrors:            topNErrors(errorMessages, 10),
		SampleEntries:        samples,
		Truncated:            truncated,
	}
	if truncated {
		summary.TruncationHint = fmt.Sprintf("Results are based on %d sampled entries. Narrow the time range or add a filter for a more complete picture. Use logs_query for full results with pagination.", maxScan)
	}

	return summary, nil
}

const maxErrorMessageLen = 200

// extractErrorMessage gets a short error message from a log entry, truncated to maxErrorMessageLen.
func extractErrorMessage(entry *loggingpb.LogEntry) string {
	var msg string
	switch p := entry.Payload.(type) {
	case *loggingpb.LogEntry_TextPayload:
		msg = p.TextPayload
	case *loggingpb.LogEntry_JsonPayload:
		if p.JsonPayload != nil {
			if v, ok := p.JsonPayload.Fields["message"]; ok {
				msg = v.GetStringValue()
			}
		}
	}
	if len(msg) > maxErrorMessageLen {
		msg = msg[:maxErrorMessageLen]
	}
	return msg
}

// topNBy returns the top N entries from a string->int count map, sorted descending by count.
// The convert function transforms each key-count pair into the desired result type.
func topNBy[T any](counts map[string]int, n int, convert func(string, int) T) []T {
	type kv struct {
		key   string
		count int
	}
	sorted := make([]kv, 0, len(counts))
	for k, v := range counts {
		sorted = append(sorted, kv{k, v})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].count > sorted[j].count })
	if len(sorted) > n {
		sorted = sorted[:n]
	}
	result := make([]T, len(sorted))
	for i, kv := range sorted {
		result[i] = convert(kv.key, kv.count)
	}
	return result
}

// topN returns the top N services by count.
func topN(counts map[string]int, n int) []ServiceCount {
	return topNBy(counts, n, func(k string, c int) ServiceCount {
		return ServiceCount{Service: k, Count: c}
	})
}

// topNErrors returns the top N error messages by count.
func topNErrors(counts map[string]int, n int) []ErrorSample {
	return topNBy(counts, n, func(k string, c int) ErrorSample {
		return ErrorSample{Message: k, Count: c}
	})
}

// convertLogEntry converts a proto LogEntry to our normalized type.
func convertLogEntry(entry *loggingpb.LogEntry) LogEntry {
	le := LogEntry{
		Timestamp: formatTimestamp(entry.Timestamp),
		Severity:  entry.Severity.String(),
		LogName:   entry.LogName,
		InsertID:  entry.InsertId,
		Trace:     entry.Trace,
		SpanID:    entry.SpanId,
		Labels:    entry.Labels,
	}

	// Resource
	if entry.Resource != nil {
		le.Resource = &ResourceInfo{
			Type:   entry.Resource.Type,
			Labels: entry.Resource.Labels,
		}
	}

	// Payload
	switch p := entry.Payload.(type) {
	case *loggingpb.LogEntry_TextPayload:
		le.TextPayload = p.TextPayload
	case *loggingpb.LogEntry_JsonPayload:
		le.JSONPayload = structToMap(p.JsonPayload)
	case *loggingpb.LogEntry_ProtoPayload:
		jsonBytes, err := protojson.Marshal(p.ProtoPayload)
		if err != nil {
			le.PayloadConversionError = fmt.Sprintf("proto marshaling failed: %v", err)
			le.TextPayload = fmt.Sprintf("[proto payload conversion failed: %v]", err)
		} else {
			var m map[string]any
			if jsonErr := json.Unmarshal(jsonBytes, &m); jsonErr != nil {
				le.PayloadConversionError = fmt.Sprintf("JSON unmarshaling failed: %v", jsonErr)
				le.TextPayload = string(jsonBytes) // fallback to raw JSON string
			} else {
				le.JSONPayload = m
			}
		}
	}

	// HTTP Request
	if entry.HttpRequest != nil {
		le.HTTPRequest = &HTTPRequestInfo{
			Method:       entry.HttpRequest.RequestMethod,
			URL:          entry.HttpRequest.RequestUrl,
			Status:       int(entry.HttpRequest.Status),
			ResponseSize: entry.HttpRequest.ResponseSize,
			UserAgent:    entry.HttpRequest.UserAgent,
			RemoteIP:     entry.HttpRequest.RemoteIp,
			Latency:      formatLatency(entry.HttpRequest.Latency),
		}
	}

	// Operation
	if entry.Operation != nil {
		le.Operation = &OperationInfo{
			ID:       entry.Operation.Id,
			Producer: entry.Operation.Producer,
		}
	}

	return le
}
