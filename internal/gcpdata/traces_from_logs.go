package gcpdata

import (
	"context"
	"fmt"
	"sort"
	"strings"

	logging "cloud.google.com/go/logging/apiv2"
)

// sampleMessageMaxLen caps the sample log line stored per discovered trace.
const sampleMessageMaxLen = 300

// severityRanks orders Cloud Logging severities so the most severe entry of a
// trace can be selected as its representative sample.
var severityRanks = map[string]int{
	"DEFAULT":   0,
	"DEBUG":     1,
	"INFO":      2,
	"NOTICE":    3,
	"WARNING":   4,
	"ERROR":     5,
	"CRITICAL":  6,
	"ALERT":     7,
	"EMERGENCY": 8,
}

// FindTracesFromLogs scans logs matching a filter, groups the matching entries
// by trace ID, and returns the distinct traces with aggregated context. It
// bridges the "found something in logs -> inspect its trace" workflow: feed an
// arbitrary log filter (e.g. severity>=ERROR for a service) and pivot to
// trace_get on the returned IDs. Entries without a trace are skipped.
func FindTracesFromLogs(ctx context.Context, client *logging.Client, project, filter, timeFilter string, scanLimit, resultLimit int) (*TraceFromLogsList, error) {
	logs, err := QueryLogs(ctx, client, project, AppendFilter(filter, timeFilter), scanLimit, "desc", "")
	if err != nil {
		return nil, err
	}
	return aggregateTracesFromLogs(logs, resultLimit), nil
}

// aggregateTracesFromLogs groups scanned log entries by trace ID and assembles
// the response. Split from FindTracesFromLogs so the aggregation is testable
// without a Cloud Logging client.
func aggregateTracesFromLogs(logs *LogQueryResult, resultLimit int) *TraceFromLogsList {
	type aggregate struct {
		trace   TraceFromLog
		sevRank int
	}
	byTrace := map[string]*aggregate{}
	var order []string // discovery order, for a stable pre-sort baseline

	for i := range logs.Entries {
		e := &logs.Entries[i]
		if e.Trace == "" {
			continue
		}
		id := extractTraceID(e.Trace)
		a := byTrace[id]
		if a == nil {
			a = &aggregate{
				trace:   TraceFromLog{TraceID: id, FirstSeen: e.Timestamp, LastSeen: e.Timestamp},
				sevRank: -1,
			}
			byTrace[id] = a
			order = append(order, id)
		}
		a.trace.LogCount++
		// Timestamps are fixed-format UTC strings, so lexical compare is chronological.
		if e.Timestamp != "" {
			if a.trace.FirstSeen == "" || e.Timestamp < a.trace.FirstSeen {
				a.trace.FirstSeen = e.Timestamp
			}
			if e.Timestamp > a.trace.LastSeen {
				a.trace.LastSeen = e.Timestamp
			}
		}
		if r := severityRank(e.Severity); r > a.sevRank {
			a.sevRank = r
			a.trace.MaxSeverity = e.Severity
			if msg := sampleLogMessage(e); msg != "" {
				a.trace.SampleMessage = msg
			}
		}
		if a.trace.Service == "" {
			a.trace.Service = serviceFromResourceInfo(e.Resource)
		}
		if a.trace.ResourceType == "" && e.Resource != nil {
			a.trace.ResourceType = e.Resource.Type
		}
	}

	traces := make([]TraceFromLog, 0, len(order))
	for _, id := range order {
		traces = append(traces, byTrace[id].trace)
	}
	// Most active first, then most recent, then trace ID for stability.
	sort.SliceStable(traces, func(i, j int) bool {
		if traces[i].LogCount != traces[j].LogCount {
			return traces[i].LogCount > traces[j].LogCount
		}
		if traces[i].LastSeen != traces[j].LastSeen {
			return traces[i].LastSeen > traces[j].LastSeen
		}
		return traces[i].TraceID < traces[j].TraceID
	})

	result := &TraceFromLogsList{ScannedEntries: logs.Count}
	var hints []string
	if logs.NextPageToken != "" {
		hints = append(hints, fmt.Sprintf("Scanned the first %d log entries; more match the filter. Narrow the filter or time range to cover the full set.", logs.Count))
	}
	if len(traces) > resultLimit {
		hints = append(hints, fmt.Sprintf("Found %d distinct traces; returning the top %d by log volume.", len(traces), resultLimit))
		traces = traces[:resultLimit]
	}
	if len(hints) > 0 {
		result.Truncated = true
		result.TruncationHint = strings.Join(hints, " ")
	}
	result.Traces = traces
	result.Count = len(traces)
	return result
}

func severityRank(s string) int {
	return severityRanks[strings.ToUpper(s)]
}

// sampleLogMessage extracts a short representative line from a log entry,
// preferring the text payload and falling back to common JSON message fields.
func sampleLogMessage(e *LogEntry) string {
	if msg := firstLine(e.TextPayload); msg != "" {
		return msg
	}
	for _, key := range []string{"message", "msg", "error", "event"} {
		if v, ok := e.JSONPayload[key]; ok {
			if s, ok := v.(string); ok {
				if msg := firstLine(s); msg != "" {
					return msg
				}
			}
		}
	}
	return ""
}

// firstLine returns the first non-empty line of s, capped at sampleMessageMaxLen.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if len(s) > sampleMessageMaxLen {
		s = s[:sampleMessageMaxLen] + "..."
	}
	return s
}

// serviceFromResourceInfo derives a service name from a converted resource's
// labels, mirroring extractServiceName for the post-conversion LogEntry shape.
func serviceFromResourceInfo(r *ResourceInfo) string {
	if r == nil {
		return ""
	}
	for _, key := range []string{"service_name", "container_name", "namespace_name", "function_name"} {
		if v := r.Labels[key]; v != "" {
			return v
		}
	}
	return r.Type
}
