package gcpdata

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	cloudtrace "cloud.google.com/go/trace/apiv1"
	"cloud.google.com/go/trace/apiv1/tracepb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const traceQueryTimeout = 30 * time.Second

// GetTrace retrieves a trace with all its spans by trace ID,
// returning spans as a tree based on parent-child relationships.
func GetTrace(ctx context.Context, client *cloudtrace.Client, project, traceID string) (*TraceDetail, error) {
	ctx, cancel := context.WithTimeout(ctx, traceQueryTimeout)
	defer cancel()

	trace, err := client.GetTrace(ctx, &tracepb.GetTraceRequest{
		ProjectId: project,
		TraceId:   traceID,
	})
	if err != nil {
		return nil, fmt.Errorf("getting trace: %w", err)
	}

	if len(trace.Spans) == 0 {
		return nil, fmt.Errorf("no spans found for trace %q: the trace may not exist, spans may have aged out of retention, or the service may not be instrumented for tracing", traceID)
	}

	return &TraceDetail{
		TraceID: trace.TraceId,
		Count:   len(trace.Spans),
		Spans:   buildSpanTree(trace.Spans),
	}, nil
}

// ListTraces searches for traces matching the given criteria.
// startTime and endTime are required by the Cloud Trace API.
func ListTraces(
	ctx context.Context,
	client *cloudtrace.Client,
	project string,
	filter string,
	view string,
	orderBy string,
	startTime, endTime time.Time,
	pageSize int,
	pageToken string,
) (*TraceListResult, error) {
	ctx, cancel := context.WithTimeout(ctx, traceQueryTimeout)
	defer cancel()

	req := &tracepb.ListTracesRequest{
		ProjectId: project,
		View:      parseViewType(view),
		PageSize:  int32(pageSize),
		StartTime: timestamppb.New(startTime),
		EndTime:   timestamppb.New(endTime),
	}
	if filter != "" {
		req.Filter = filter
	}
	if orderBy != "" {
		req.OrderBy = orderBy
	}
	if pageToken != "" {
		req.PageToken = pageToken
	}

	it := client.ListTraces(ctx, req)
	it.PageInfo().MaxSize = pageSize

	var traces []TraceSummary
	for {
		trace, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			if len(traces) > 0 {
				return &TraceListResult{
					Count:          len(traces),
					Traces:         traces,
					Truncated:      true,
					TruncationHint: fmt.Sprintf("Listing stopped after %d traces due to error: %v", len(traces), err),
				}, nil
			}
			return nil, fmt.Errorf("listing traces: %w", err)
		}
		traces = append(traces, traceToSummary(trace))
		if len(traces) >= pageSize {
			break
		}
	}

	result := &TraceListResult{
		Count:  len(traces),
		Traces: traces,
	}
	if nextToken := it.PageInfo().Token; nextToken != "" {
		result.NextPageToken = nextToken
	}
	if len(traces) >= pageSize {
		result.Truncated = true
		result.TruncationHint = fmt.Sprintf("Returned %d traces (limit reached). Use next_page_token to fetch more, or narrow filters.", pageSize)
	}

	return result, nil
}

// BuildTraceFilter compiles structured filter parameters into a Cloud Trace
// filter string. Returns "" if no params are set.
func BuildTraceFilter(rootName, spanName, minLatency string) (string, error) {
	var parts []string
	if rootName != "" {
		parts = append(parts, "root:"+rootName)
	}
	if spanName != "" {
		parts = append(parts, "span:"+spanName)
	}
	if minLatency != "" {
		ms, err := parseLatencyToMs(minLatency)
		if err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("latency:%dms", ms))
	}
	return strings.Join(parts, " "), nil
}

func parseLatencyToMs(s string) (int64, error) {
	s = strings.TrimSpace(s)
	var ms int64
	if val, ok := strings.CutSuffix(s, "ms"); ok {
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid min_latency %q: cannot parse as milliseconds", s)
		}
		ms = int64(math.Ceil(f))
	} else if val, ok := strings.CutSuffix(s, "s"); ok {
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid min_latency %q: cannot parse as seconds", s)
		}
		ms = int64(math.Ceil(f * 1000))
	} else {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid min_latency %q: use format like '100ms' or '1.5s'", s)
		}
		ms = int64(math.Ceil(f))
	}
	if ms <= 0 {
		return 0, fmt.Errorf("invalid min_latency %q: must be a positive duration", s)
	}
	return ms, nil
}

func parseViewType(view string) tracepb.ListTracesRequest_ViewType {
	switch strings.ToUpper(view) {
	case "MINIMAL":
		return tracepb.ListTracesRequest_MINIMAL
	case "COMPLETE":
		return tracepb.ListTracesRequest_COMPLETE
	default:
		return tracepb.ListTracesRequest_ROOTSPAN
	}
}

func traceToSummary(t *tracepb.Trace) TraceSummary {
	s := TraceSummary{
		TraceID:   t.TraceId,
		SpanCount: len(t.Spans),
	}
	if len(t.Spans) > 0 {
		// Find root span (ParentSpanId == 0). Fall back to first span if none found.
		root := t.Spans[0]
		for _, span := range t.Spans {
			if span.ParentSpanId == 0 {
				root = span
				break
			}
		}
		s.RootSpanName = root.Name
		s.StartTime = formatTimestamp(root.StartTime)
		s.EndTime = formatTimestamp(root.EndTime)
		s.Labels = root.Labels
		if root.StartTime != nil && root.EndTime != nil {
			s.Duration = formatDuration(root.EndTime.AsTime().Sub(root.StartTime.AsTime()))
		}
	}
	return s
}

// buildSpanTree converts a flat slice of proto spans into a tree sorted by start time.
// Spans whose parent is not found in the slice are treated as root spans.
func buildSpanTree(protoSpans []*tracepb.TraceSpan) []TraceSpan {
	spans := make([]TraceSpan, len(protoSpans))
	idToIdx := make(map[uint64]int, len(protoSpans))
	parentIDs := make([]uint64, len(protoSpans))

	for i, s := range protoSpans {
		idToIdx[s.SpanId] = i
		parentIDs[i] = s.ParentSpanId
		spans[i] = TraceSpan{
			SpanID:    fmt.Sprintf("%d", s.SpanId),
			Name:      s.Name,
			Kind:      spanKindString(s.Kind),
			StartTime: formatTimestamp(s.StartTime),
			EndTime:   formatTimestamp(s.EndTime),
			Labels:    s.Labels,
		}
		if s.StartTime != nil && s.EndTime != nil {
			spans[i].Duration = formatDuration(s.EndTime.AsTime().Sub(s.StartTime.AsTime()))
		}
	}

	childrenOf := make(map[int][]int)
	var rootIdxs []int
	for i, pid := range parentIDs {
		if pid == 0 {
			rootIdxs = append(rootIdxs, i)
			continue
		}
		if parentIdx, ok := idToIdx[pid]; ok {
			childrenOf[parentIdx] = append(childrenOf[parentIdx], i)
		} else {
			rootIdxs = append(rootIdxs, i)
		}
	}

	var build func(idx int) TraceSpan
	build = func(idx int) TraceSpan {
		s := spans[idx]
		for _, childIdx := range childrenOf[idx] {
			s.Children = append(s.Children, build(childIdx))
		}
		sortSpansByStartTime(s.Children)
		return s
	}

	roots := make([]TraceSpan, 0, len(rootIdxs))
	for _, idx := range rootIdxs {
		roots = append(roots, build(idx))
	}
	sortSpansByStartTime(roots)

	return roots
}

func sortSpansByStartTime(spans []TraceSpan) {
	sort.Slice(spans, func(i, j int) bool {
		return spans[i].StartTime < spans[j].StartTime
	})
}

func spanKindString(k tracepb.TraceSpan_SpanKind) string {
	switch k {
	case tracepb.TraceSpan_RPC_SERVER:
		return "SERVER"
	case tracepb.TraceSpan_RPC_CLIENT:
		return "CLIENT"
	default:
		return "UNSPECIFIED"
	}
}
