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
	loggingpb "cloud.google.com/go/logging/apiv2/loggingpb"
	logtypepb "google.golang.org/genproto/googleapis/logging/type"
)

// QueryLogs executes an arbitrary Cloud Logging query.
func QueryLogs(ctx context.Context, client *logging.Client, project, filter string, limit int, order, pageToken string) (*LogQueryResult, error) {
	orderBy := "timestamp desc"
	if order == "asc" {
		orderBy = "timestamp asc"
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
func QueryLogsByTrace(ctx context.Context, client *logging.Client, project, traceID string, limit int) (*LogQueryResult, error) {
	filter := fmt.Sprintf(`trace="projects/%s/traces/%s"`, project, EscapeFilterValue(traceID))

	req := &loggingpb.ListLogEntriesRequest{
		ResourceNames: []string{fmt.Sprintf("projects/%s", project)},
		Filter:        filter,
		OrderBy:       "timestamp asc",
		PageSize:      safeInt32(limit),
	}

	return fetchLogEntries(ctx, client, req, limit)
}

// FindRequests finds HTTP requests matching the given URL pattern.
func FindRequests(ctx context.Context, client *logging.Client, project, urlPattern, method string, statusCode int, tracedOnly bool, limit int) (*RequestList, error) {
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

	filter := strings.Join(parts, " AND ")

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
	for len(requests) < limit {
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

		// Extract request_id from jsonPayload
		if jp := entry.GetJsonPayload(); jp != nil {
			if v, ok := jp.Fields["request_id"]; ok {
				ri.RequestID = v.GetStringValue()
			}
		}

		// Extract service name from resource labels
		ri.Service = extractServiceName(entry)

		requests = append(requests, ri)
	}

	return &RequestList{
		Count:    len(requests),
		Requests: requests,
	}, nil
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

// ListServices discovers unique services in the project by querying recent logs.
func ListServices(ctx context.Context, client *logging.Client, project string) (*ServiceList, error) {
	// Limit to last 24 hours to avoid scanning very old entries
	startTime := time.Now().Add(-24 * time.Hour)

	req := &loggingpb.ListLogEntriesRequest{
		ResourceNames: []string{fmt.Sprintf("projects/%s", project)},
		Filter: fmt.Sprintf(
			`(resource.type="k8s_container" OR resource.type="cloud_run_revision") AND timestamp>="%s"`,
			startTime.Format(time.RFC3339),
		),
		OrderBy:  "timestamp desc",
		PageSize: 1000,
	}

	it := client.ListLogEntries(ctx, req)

	seen := make(map[string]ServiceInfo)
	for i := 0; i < 1000; i++ {
		entry, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("iterating log entries: %w", err)
		}

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
			key = "cloud_run:" + name
			info = ServiceInfo{
				Name:         name,
				ResourceType: resType,
			}
		case "k8s_container":
			ns := labels["namespace_name"]
			container := labels["container_name"]
			key = "k8s:" + ns + "/" + container
			info = ServiceInfo{
				Name:         container,
				ResourceType: resType,
				Namespace:    ns,
			}
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

	return &ServiceList{
		Count:    len(services),
		Services: services,
	}, nil
}

// SummarizeLogs aggregates log statistics over a time window.
func SummarizeLogs(ctx context.Context, client *logging.Client, project, filter string, lookbackMinutes int) (*LogsSummary, error) {
	startTime := time.Now().Add(-time.Duration(lookbackMinutes) * time.Minute)

	filterParts := []string{
		fmt.Sprintf(`timestamp>="%s"`, startTime.Format(time.RFC3339)),
	}
	if filter != "" {
		filterParts = append(filterParts, filter)
	}

	const maxScan = 1000

	req := &loggingpb.ListLogEntriesRequest{
		ResourceNames: []string{fmt.Sprintf("projects/%s", project)},
		Filter:        strings.Join(filterParts, "\n"),
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
	}

	summary := &LogsSummary{
		TotalEntries:         total,
		SeverityDistribution: severityDist,
		TopServices:          topN(serviceCounts, 10),
		TopErrors:            topNErrors(errorMessages, 10),
		SampleEntries:        samples,
		Truncated:            total >= maxScan,
	}

	return summary, nil
}

// extractErrorMessage gets a short error message from a log entry.
func extractErrorMessage(entry *loggingpb.LogEntry) string {
	switch p := entry.Payload.(type) {
	case *loggingpb.LogEntry_TextPayload:
		msg := p.TextPayload
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return msg
	case *loggingpb.LogEntry_JsonPayload:
		if p.JsonPayload != nil {
			if v, ok := p.JsonPayload.Fields["message"]; ok {
				msg := v.GetStringValue()
				if len(msg) > 200 {
					msg = msg[:200]
				}
				return msg
			}
		}
	}
	return ""
}

// topN returns the top N services by count.
func topN(counts map[string]int, n int) []ServiceCount {
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
	result := make([]ServiceCount, len(sorted))
	for i, kv := range sorted {
		result[i] = ServiceCount{Service: kv.key, Count: kv.count}
	}
	return result
}

// topNErrors returns the top N error messages by count.
func topNErrors(counts map[string]int, n int) []ErrorSample {
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
	result := make([]ErrorSample, len(sorted))
	for i, kv := range sorted {
		result[i] = ErrorSample{Message: kv.key, Count: kv.count}
	}
	return result
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
			le.TextPayload = fmt.Sprintf("[proto payload conversion failed: %v]", err)
		} else {
			var m map[string]any
			if jsonErr := json.Unmarshal(jsonBytes, &m); jsonErr != nil {
				le.TextPayload = string(jsonBytes)
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
