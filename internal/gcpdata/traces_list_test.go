package gcpdata

import (
	"testing"

	"cloud.google.com/go/trace/apiv1/tracepb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestBuildTraceFilter(t *testing.T) {
	tests := []struct {
		name       string
		rootName   string
		spanName   string
		minLatency string
		want       string
		wantErr    bool
	}{
		{"empty", "", "", "", "", false},
		{"root only", "myHandler", "", "", "root:myHandler", false},
		{"span only", "", "db.query", "", "span:db.query", false},
		{"latency ms", "", "", "500ms", "latency:500ms", false},
		{"latency seconds", "", "", "2.5s", "latency:2500ms", false},
		{"latency 1s", "", "", "1s", "latency:1000ms", false},
		{"all combined", "myHandler", "db.query", "100ms", "root:myHandler span:db.query latency:100ms", false},
		{"invalid latency", "", "", "abc", "", true},
		{"latency bare number", "", "", "150", "latency:150ms", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildTraceFilter(tt.rootName, tt.spanName, tt.minLatency)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseLatencyToMs(t *testing.T) {
	tests := []struct {
		input   string
		want    int64
		wantErr bool
	}{
		{"100ms", 100, false},
		{"1s", 1000, false},
		{"2.5s", 2500, false},
		{"0.5s", 500, false},
		{"150", 150, false},
		{"1.5ms", 2, false}, // ceil(1.5)
		{"abc", 0, true},
		{"", 0, true},
		{"ms", 0, true},
		{"s", 0, true},
		{"-500ms", 0, true},
		{"0ms", 0, true},
		{"-1s", 0, true},
		{"0", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseLatencyToMs(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseViewType(t *testing.T) {
	assert.Equal(t, tracepb.ListTracesRequest_ROOTSPAN, parseViewType(""))
	assert.Equal(t, tracepb.ListTracesRequest_ROOTSPAN, parseViewType("ROOTSPAN"))
	assert.Equal(t, tracepb.ListTracesRequest_ROOTSPAN, parseViewType("rootspan"))
	assert.Equal(t, tracepb.ListTracesRequest_MINIMAL, parseViewType("MINIMAL"))
	assert.Equal(t, tracepb.ListTracesRequest_MINIMAL, parseViewType("minimal"))
	assert.Equal(t, tracepb.ListTracesRequest_COMPLETE, parseViewType("COMPLETE"))
	assert.Equal(t, tracepb.ListTracesRequest_ROOTSPAN, parseViewType("unknown"))
}

func TestTraceToSummary(t *testing.T) {
	t.Run("with root span", func(t *testing.T) {
		start := timestamppb.Now()
		end := timestamppb.New(start.AsTime().Add(500_000_000)) // 500ms later

		trace := &tracepb.Trace{
			TraceId: "abc123",
			Spans: []*tracepb.TraceSpan{
				{
					SpanId:    1,
					Name:      "/api/users",
					StartTime: start,
					EndTime:   end,
					Labels:    map[string]string{"http.method": "GET"},
				},
			},
		}

		s := traceToSummary(trace)
		assert.Equal(t, "abc123", s.TraceID)
		assert.Equal(t, "/api/users", s.RootSpanName)
		assert.Equal(t, 1, s.SpanCount)
		assert.NotEmpty(t, s.StartTime)
		assert.NotEmpty(t, s.EndTime)
		assert.NotEmpty(t, s.Duration)
		assert.Equal(t, "GET", s.Labels["http.method"])
	})

	t.Run("no spans (MINIMAL view)", func(t *testing.T) {
		trace := &tracepb.Trace{
			TraceId: "def456",
			Spans:   nil,
		}

		s := traceToSummary(trace)
		assert.Equal(t, "def456", s.TraceID)
		assert.Empty(t, s.RootSpanName)
		assert.Equal(t, 0, s.SpanCount)
		assert.Empty(t, s.Duration)
	})

	t.Run("COMPLETE view: root not first", func(t *testing.T) {
		start := timestamppb.Now()
		end := timestamppb.New(start.AsTime().Add(200_000_000))

		trace := &tracepb.Trace{
			TraceId: "complete123",
			Spans: []*tracepb.TraceSpan{
				{SpanId: 2, ParentSpanId: 1, Name: "child-span", StartTime: start, EndTime: end},
				{SpanId: 1, ParentSpanId: 0, Name: "root-span", StartTime: start, EndTime: end},
			},
		}

		s := traceToSummary(trace)
		assert.Equal(t, "root-span", s.RootSpanName, "should find root by ParentSpanId==0, not assume Spans[0]")
		assert.Equal(t, 2, s.SpanCount)
	})

	t.Run("span with nil timestamps", func(t *testing.T) {
		trace := &tracepb.Trace{
			TraceId: "ghi789",
			Spans: []*tracepb.TraceSpan{
				{SpanId: 1, Name: "orphan"},
			},
		}

		s := traceToSummary(trace)
		assert.Equal(t, "orphan", s.RootSpanName)
		assert.Empty(t, s.Duration)
	})
}
