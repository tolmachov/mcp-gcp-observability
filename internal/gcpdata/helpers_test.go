package gcpdata

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestSafeInt32(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want int32
	}{
		{"positive", 100, 100},
		{"zero", 0, 0},
		{"negative", -5, 0},
		{"max int32", math.MaxInt32, math.MaxInt32},
		{"over max int32", math.MaxInt32 + 1, math.MaxInt32},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := safeInt32(tt.in)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name string
		dur  time.Duration
		want string
	}{
		{"milliseconds", 150 * time.Millisecond, "150.000ms"},
		{"sub-millisecond", 500 * time.Microsecond, "0.500ms"},
		{"seconds", 2500 * time.Millisecond, "2.500s"},
		{"exactly one second", time.Second, "1.000s"},
		{"zero", 0, "0.000ms"},
		{"negative treated as zero", -500 * time.Millisecond, "0.000ms"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatDuration(tt.dur)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestEscapeFilterValue(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello", "hello"},
		{"with quotes", `say "hi"`, `say \"hi\"`},
		{"with backslash", `path\to`, `path\\to`},
		{"both", `a\"b`, `a\\\"b`},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EscapeFilterValue(tt.in)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIsValidSeverity(t *testing.T) {
	valid := []string{"ERROR", "INFO", "WARNING", "CRITICAL", "DEBUG", "DEFAULT", "NOTICE", "ALERT", "EMERGENCY", "error", "Error"}
	for _, s := range valid {
		assert.True(t, IsValidSeverity(s), "expected IsValidSeverity(%q) to be true", s)
	}
	invalid := []string{"INVALID", "ERR", "", "warn", "FATAL"}
	for _, s := range invalid {
		assert.False(t, IsValidSeverity(s), "expected IsValidSeverity(%q) to be false", s)
	}
}

func TestFormatTimestamp(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		got := formatTimestamp(nil)
		assert.Equal(t, "", got)
	})

	t.Run("valid", func(t *testing.T) {
		ts := timestamppb.New(time.Date(2025, 3, 15, 10, 30, 0, 0, time.UTC))
		got := formatTimestamp(ts)
		want := "2025-03-15T10:30:00.000Z"
		assert.Equal(t, want, got)
	})
}

func TestFormatLatency(t *testing.T) {
	tests := []struct {
		name string
		dur  *durationpb.Duration
		want string
	}{
		{"nil", nil, ""},
		{"milliseconds", durationpb.New(150 * time.Millisecond), "150.000ms"},
		{"seconds", durationpb.New(2500 * time.Millisecond), "2.500s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatLatency(tt.dur)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestExtractTraceID(t *testing.T) {
	tests := []struct {
		name  string
		trace string
		want  string
	}{
		{"full path", "projects/my-project/traces/abc123", "abc123"},
		{"bare id", "abc123", "abc123"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractTraceID(tt.trace)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestExtractServiceName(t *testing.T) {
	tests := []struct {
		name  string
		entry *loggingpb.LogEntry
		want  string
	}{
		{
			"nil resource",
			&loggingpb.LogEntry{},
			"",
		},
		{
			"cloud run",
			&loggingpb.LogEntry{
				Resource: &monitoredres.MonitoredResource{
					Type:   "cloud_run_revision",
					Labels: map[string]string{"service_name": "my-service"},
				},
			},
			"my-service",
		},
		{
			"k8s container",
			&loggingpb.LogEntry{
				Resource: &monitoredres.MonitoredResource{
					Type:   "k8s_container",
					Labels: map[string]string{"container_name": "web", "namespace_name": "prod"},
				},
			},
			"web",
		},
		{
			"k8s namespace fallback",
			&loggingpb.LogEntry{
				Resource: &monitoredres.MonitoredResource{
					Type:   "k8s_container",
					Labels: map[string]string{"namespace_name": "prod"},
				},
			},
			"prod",
		},
		{
			"fallback to resource type",
			&loggingpb.LogEntry{
				Resource: &monitoredres.MonitoredResource{
					Type:   "gce_instance",
					Labels: map[string]string{},
				},
			},
			"gce_instance",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractServiceName(tt.entry)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestStructToMap(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		got := structToMap(nil)
		assert.Nil(t, got)
	})

	t.Run("with fields", func(t *testing.T) {
		s, err := structpb.NewStruct(map[string]any{
			"key":    "value",
			"number": 42.0,
			"flag":   true,
			"nested": map[string]any{"inner": "data"},
			"list":   []any{"a", "b"},
		})
		require.NoError(t, err)

		m := structToMap(s)
		assert.Equal(t, "value", m["key"])
		assert.Equal(t, 42.0, m["number"])
		assert.Equal(t, true, m["flag"])
		nested, ok := m["nested"].(map[string]any)
		assert.True(t, ok)
		assert.Equal(t, "data", nested["inner"])
		list, ok := m["list"].([]any)
		assert.True(t, ok)
		assert.Len(t, list, 2)
	})
}

func TestAppendFilter(t *testing.T) {
	tests := []struct {
		name string
		base string
		part string
		want string
	}{
		{"both empty", "", "", ""},
		{"empty base", "", "part", "part"},
		{"empty part", "base", "", "base"},
		{"both set", "base", "part", "base\npart"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AppendFilter(tt.base, tt.part)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestTopN(t *testing.T) {
	counts := map[string]int{
		"svc-a": 100,
		"svc-b": 50,
		"svc-c": 200,
		"svc-d": 10,
	}

	result := topN(counts, 2)
	require.Len(t, result, 2)
	assert.Equal(t, "svc-c", result[0].Service)
	assert.Equal(t, 200, result[0].Count)
	assert.Equal(t, "svc-a", result[1].Service)
	assert.Equal(t, 100, result[1].Count)
}

func TestTopNErrors(t *testing.T) {
	counts := map[string]int{
		"timeout":    50,
		"null ptr":   100,
		"connection": 25,
	}

	result := topNErrors(counts, 2)
	require.Len(t, result, 2)
	assert.Equal(t, "null ptr", result[0].Message)
	assert.Equal(t, 100, result[0].Count)
}
