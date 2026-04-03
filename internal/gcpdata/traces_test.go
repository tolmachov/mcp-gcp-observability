package gcpdata

import (
	"testing"

	tracepb "cloud.google.com/go/trace/apiv1/tracepb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestBuildSpanTree(t *testing.T) {
	t.Run("empty input", func(t *testing.T) {
		spans := buildSpanTree(nil)
		if len(spans) != 0 {
			t.Errorf("expected 0 spans, got %d", len(spans))
		}
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
		if len(spans) != 1 {
			t.Fatalf("expected 1 root span, got %d", len(spans))
		}
		if spans[0].Name != "root" {
			t.Errorf("name = %q, want %q", spans[0].Name, "root")
		}
		if spans[0].Kind != "SERVER" {
			t.Errorf("kind = %q, want %q", spans[0].Kind, "SERVER")
		}
		if len(spans[0].Children) != 0 {
			t.Errorf("expected 0 children, got %d", len(spans[0].Children))
		}
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
		if len(spans) != 1 {
			t.Fatalf("expected 1 root span, got %d", len(spans))
		}
		if spans[0].Name != "root" {
			t.Errorf("root name = %q, want %q", spans[0].Name, "root")
		}
		if len(spans[0].Children) != 1 {
			t.Fatalf("expected 1 child, got %d", len(spans[0].Children))
		}
		if spans[0].Children[0].Name != "child" {
			t.Errorf("child name = %q, want %q", spans[0].Children[0].Name, "child")
		}
	})

	t.Run("multi-level tree", func(t *testing.T) {
		spans := buildSpanTree([]*tracepb.TraceSpan{
			{SpanId: 1, Name: "root", StartTime: timestamppb.Now(), EndTime: timestamppb.Now()},
			{SpanId: 2, ParentSpanId: 1, Name: "child", StartTime: timestamppb.Now(), EndTime: timestamppb.Now()},
			{SpanId: 3, ParentSpanId: 2, Name: "grandchild", StartTime: timestamppb.Now(), EndTime: timestamppb.Now()},
		})
		if len(spans) != 1 {
			t.Fatalf("expected 1 root, got %d", len(spans))
		}
		if len(spans[0].Children) != 1 {
			t.Fatalf("expected 1 child of root, got %d", len(spans[0].Children))
		}
		if len(spans[0].Children[0].Children) != 1 {
			t.Fatalf("expected 1 grandchild, got %d", len(spans[0].Children[0].Children))
		}
		if spans[0].Children[0].Children[0].Name != "grandchild" {
			t.Errorf("grandchild name = %q", spans[0].Children[0].Children[0].Name)
		}
	})

	t.Run("orphaned span becomes root", func(t *testing.T) {
		spans := buildSpanTree([]*tracepb.TraceSpan{
			{SpanId: 1, Name: "root", StartTime: timestamppb.Now(), EndTime: timestamppb.Now()},
			{SpanId: 2, ParentSpanId: 999, Name: "orphan", StartTime: timestamppb.Now(), EndTime: timestamppb.Now()},
		})
		if len(spans) != 2 {
			t.Fatalf("expected 2 root spans (root + orphan), got %d", len(spans))
		}
	})

	t.Run("multiple root spans", func(t *testing.T) {
		spans := buildSpanTree([]*tracepb.TraceSpan{
			{SpanId: 1, Name: "root1", StartTime: timestamppb.Now(), EndTime: timestamppb.Now()},
			{SpanId: 2, Name: "root2", StartTime: timestamppb.Now(), EndTime: timestamppb.Now()},
		})
		if len(spans) != 2 {
			t.Fatalf("expected 2 root spans, got %d", len(spans))
		}
	})

	t.Run("duration computed from timestamps", func(t *testing.T) {
		start := timestamppb.Now()
		end := timestamppb.New(start.AsTime().Add(2500 * 1e6)) // 2.5 seconds
		spans := buildSpanTree([]*tracepb.TraceSpan{
			{SpanId: 1, Name: "timed", StartTime: start, EndTime: end},
		})
		if spans[0].Duration != "2.500s" {
			t.Errorf("duration = %q, want %q", spans[0].Duration, "2.500s")
		}
	})

	t.Run("siblings sorted by start time", func(t *testing.T) {
		early := timestamppb.New(timestamppb.Now().AsTime().Add(-2 * 1e9))
		late := timestamppb.Now()
		spans := buildSpanTree([]*tracepb.TraceSpan{
			{SpanId: 1, Name: "root", StartTime: early, EndTime: late},
			{SpanId: 3, ParentSpanId: 1, Name: "late-child", StartTime: late, EndTime: late},
			{SpanId: 2, ParentSpanId: 1, Name: "early-child", StartTime: early, EndTime: early},
		})
		if len(spans) != 1 {
			t.Fatalf("expected 1 root, got %d", len(spans))
		}
		if len(spans[0].Children) != 2 {
			t.Fatalf("expected 2 children, got %d", len(spans[0].Children))
		}
		if spans[0].Children[0].Name != "early-child" {
			t.Errorf("first child = %q, want %q (should be sorted by start time)", spans[0].Children[0].Name, "early-child")
		}
		if spans[0].Children[1].Name != "late-child" {
			t.Errorf("second child = %q, want %q", spans[0].Children[1].Name, "late-child")
		}
	})

	t.Run("missing timestamps produce empty duration", func(t *testing.T) {
		spans := buildSpanTree([]*tracepb.TraceSpan{
			{SpanId: 1, Name: "no-times"},
		})
		if spans[0].Duration != "" {
			t.Errorf("duration = %q, want empty", spans[0].Duration)
		}
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
			if got != tt.want {
				t.Errorf("spanKindString(%v) = %q, want %q", tt.kind, got, tt.want)
			}
		})
	}
}
