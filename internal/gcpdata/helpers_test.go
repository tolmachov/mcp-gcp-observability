package gcpdata

import (
	"math"
	"testing"
	"time"

	loggingpb "cloud.google.com/go/logging/apiv2/loggingpb"
	monitoredres "google.golang.org/genproto/googleapis/api/monitoredres"
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
			if got != tt.want {
				t.Errorf("safeInt32(%d) = %d, want %d", tt.in, got, tt.want)
			}
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
			if got != tt.want {
				t.Errorf("formatDuration(%v) = %q, want %q", tt.dur, got, tt.want)
			}
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
			if got != tt.want {
				t.Errorf("EscapeFilterValue(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestIsValidSeverity(t *testing.T) {
	valid := []string{"ERROR", "INFO", "WARNING", "CRITICAL", "DEBUG", "DEFAULT", "NOTICE", "ALERT", "EMERGENCY", "error", "Error"}
	for _, s := range valid {
		if !IsValidSeverity(s) {
			t.Errorf("IsValidSeverity(%q) = false, want true", s)
		}
	}
	invalid := []string{"INVALID", "ERR", "", "warn", "FATAL"}
	for _, s := range invalid {
		if IsValidSeverity(s) {
			t.Errorf("IsValidSeverity(%q) = true, want false", s)
		}
	}
}

func TestFormatTimestamp(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		if got := formatTimestamp(nil); got != "" {
			t.Errorf("formatTimestamp(nil) = %q, want empty", got)
		}
	})

	t.Run("valid", func(t *testing.T) {
		ts := timestamppb.New(time.Date(2025, 3, 15, 10, 30, 0, 0, time.UTC))
		got := formatTimestamp(ts)
		want := "2025-03-15T10:30:00.000Z"
		if got != want {
			t.Errorf("formatTimestamp() = %q, want %q", got, want)
		}
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
			if got != tt.want {
				t.Errorf("formatLatency() = %q, want %q", got, tt.want)
			}
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
			if got != tt.want {
				t.Errorf("extractTraceID(%q) = %q, want %q", tt.trace, got, tt.want)
			}
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
			if got != tt.want {
				t.Errorf("extractServiceName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStructToMap(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		if got := structToMap(nil); got != nil {
			t.Errorf("structToMap(nil) = %v, want nil", got)
		}
	})

	t.Run("with fields", func(t *testing.T) {
		s, err := structpb.NewStruct(map[string]any{
			"key":    "value",
			"number": 42.0,
			"flag":   true,
			"nested": map[string]any{"inner": "data"},
			"list":   []any{"a", "b"},
		})
		if err != nil {
			t.Fatalf("failed to create struct: %v", err)
		}

		m := structToMap(s)
		if m["key"] != "value" {
			t.Errorf("key = %v, want 'value'", m["key"])
		}
		if m["number"] != 42.0 {
			t.Errorf("number = %v, want 42.0", m["number"])
		}
		if m["flag"] != true {
			t.Errorf("flag = %v, want true", m["flag"])
		}
		nested, ok := m["nested"].(map[string]any)
		if !ok || nested["inner"] != "data" {
			t.Errorf("nested = %v, want map with inner=data", m["nested"])
		}
		list, ok := m["list"].([]any)
		if !ok || len(list) != 2 {
			t.Errorf("list = %v, want [a, b]", m["list"])
		}
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
			if got != tt.want {
				t.Errorf("AppendFilter(%q, %q) = %q, want %q", tt.base, tt.part, got, tt.want)
			}
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
	if len(result) != 2 {
		t.Fatalf("topN returned %d items, want 2", len(result))
	}
	if result[0].Service != "svc-c" || result[0].Count != 200 {
		t.Errorf("first = %v, want svc-c:200", result[0])
	}
	if result[1].Service != "svc-a" || result[1].Count != 100 {
		t.Errorf("second = %v, want svc-a:100", result[1])
	}
}

func TestTopNErrors(t *testing.T) {
	counts := map[string]int{
		"timeout":    50,
		"null ptr":   100,
		"connection": 25,
	}

	result := topNErrors(counts, 2)
	if len(result) != 2 {
		t.Fatalf("topNErrors returned %d items, want 2", len(result))
	}
	if result[0].Message != "null ptr" || result[0].Count != 100 {
		t.Errorf("first = %v, want null ptr:100", result[0])
	}
}
