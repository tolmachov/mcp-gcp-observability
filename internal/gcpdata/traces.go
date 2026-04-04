package gcpdata

import (
	"context"
	"fmt"
	"sort"
	"time"

	cloudtrace "cloud.google.com/go/trace/apiv1"
	"cloud.google.com/go/trace/apiv1/tracepb"
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
