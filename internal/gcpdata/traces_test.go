package gcpdata

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"cloud.google.com/go/trace/apiv1/tracepb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestBuildSpanTree(t *testing.T) {
	t.Run("empty input", func(t *testing.T) {
		spans := buildSpanTree(nil)
		assert.Len(t, spans, 0)
	})

	t.Run("single root span", func(t *testing.T) {
		spans := buildSpanTree([]*tracepb.TraceSpan{
			{
				SpanId:    1,
				Name:      "root",
				Kind:      tracepb.TraceSpan_RPC_SERVER,
				StartTime: timestamppb.Now(),
				EndTime:   timestamppb.Now(),
			},
		})
		require.Len(t, spans, 1)
		assert.Equal(t, "root", spans[0].Name)
		assert.Equal(t, "SERVER", spans[0].Kind)
		assert.Len(t, spans[0].Children, 0)
	})

	t.Run("parent-child relationship", func(t *testing.T) {
		start1 := timestamppb.Now()
		start2 := timestamppb.Now()
		spans := buildSpanTree([]*tracepb.TraceSpan{
			{
				SpanId:       2,
				ParentSpanId: 1,
				Name:         "child",
				StartTime:    start2,
				EndTime:      timestamppb.Now(),
			},
			{
				SpanId:    1,
				Name:      "root",
				StartTime: start1,
				EndTime:   timestamppb.Now(),
			},
		})
		require.Len(t, spans, 1)
		assert.Equal(t, "root", spans[0].Name)
		require.Len(t, spans[0].Children, 1)
		assert.Equal(t, "child", spans[0].Children[0].Name)
	})

	t.Run("multi-level tree", func(t *testing.T) {
		spans := buildSpanTree([]*tracepb.TraceSpan{
			{SpanId: 1, Name: "root", StartTime: timestamppb.Now(), EndTime: timestamppb.Now()},
			{SpanId: 2, ParentSpanId: 1, Name: "child", StartTime: timestamppb.Now(), EndTime: timestamppb.Now()},
			{SpanId: 3, ParentSpanId: 2, Name: "grandchild", StartTime: timestamppb.Now(), EndTime: timestamppb.Now()},
		})
		require.Len(t, spans, 1)
		require.Len(t, spans[0].Children, 1)
		require.Len(t, spans[0].Children[0].Children, 1)
		assert.Equal(t, "grandchild", spans[0].Children[0].Children[0].Name)
	})

	t.Run("orphaned span becomes root", func(t *testing.T) {
		spans := buildSpanTree([]*tracepb.TraceSpan{
			{SpanId: 1, Name: "root", StartTime: timestamppb.Now(), EndTime: timestamppb.Now()},
			{SpanId: 2, ParentSpanId: 999, Name: "orphan", StartTime: timestamppb.Now(), EndTime: timestamppb.Now()},
		})
		require.Len(t, spans, 2)
	})

	t.Run("multiple root spans", func(t *testing.T) {
		spans := buildSpanTree([]*tracepb.TraceSpan{
			{SpanId: 1, Name: "root1", StartTime: timestamppb.Now(), EndTime: timestamppb.Now()},
			{SpanId: 2, Name: "root2", StartTime: timestamppb.Now(), EndTime: timestamppb.Now()},
		})
		require.Len(t, spans, 2)
	})

	t.Run("duration computed from timestamps", func(t *testing.T) {
		start := timestamppb.Now()
		end := timestamppb.New(start.AsTime().Add(2500 * 1e6)) // 2.5 seconds
		spans := buildSpanTree([]*tracepb.TraceSpan{
			{SpanId: 1, Name: "timed", StartTime: start, EndTime: end},
		})
		assert.Equal(t, "2.500s", spans[0].Duration)
	})

	t.Run("siblings sorted by start time", func(t *testing.T) {
		early := timestamppb.New(timestamppb.Now().AsTime().Add(-2 * 1e9))
		late := timestamppb.Now()
		spans := buildSpanTree([]*tracepb.TraceSpan{
			{SpanId: 1, Name: "root", StartTime: early, EndTime: late},
			{SpanId: 3, ParentSpanId: 1, Name: "late-child", StartTime: late, EndTime: late},
			{SpanId: 2, ParentSpanId: 1, Name: "early-child", StartTime: early, EndTime: early},
		})
		require.Len(t, spans, 1)
		require.Len(t, spans[0].Children, 2)
		assert.Equal(t, "early-child", spans[0].Children[0].Name)
		assert.Equal(t, "late-child", spans[0].Children[1].Name)
	})

	t.Run("missing timestamps produce empty duration", func(t *testing.T) {
		spans := buildSpanTree([]*tracepb.TraceSpan{
			{SpanId: 1, Name: "no-times"},
		})
		assert.Equal(t, "", spans[0].Duration)
	})
}

func TestSpanKindString(t *testing.T) {
	tests := []struct {
		name string
		kind tracepb.TraceSpan_SpanKind
		want string
	}{
		{"server", tracepb.TraceSpan_RPC_SERVER, "SERVER"},
		{"client", tracepb.TraceSpan_RPC_CLIENT, "CLIENT"},
		{"unspecified", tracepb.TraceSpan_SPAN_KIND_UNSPECIFIED, "UNSPECIFIED"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := spanKindString(tt.kind)
			assert.Equal(t, tt.want, got)
		})
	}
}
