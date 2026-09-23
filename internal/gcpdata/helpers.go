package gcpdata

import (
	"fmt"
	"math"
	"strings"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// safeInt32 converts int to int32, clamping to [0, math.MaxInt32] to prevent overflow or negative values.
func safeInt32(n int) int32 {
	if n < 0 {
		return 0
	}
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(n) //nolint:gosec // overflow is guarded by the checks above
}

// EscapeFilterValue escapes double quotes and backslashes in a value
// before embedding it in a Cloud Logging filter string.
func EscapeFilterValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// Severities lists the Cloud Logging severity levels in ascending order.
var Severities = []string{"DEFAULT", "DEBUG", "INFO", "NOTICE", "WARNING", "ERROR", "CRITICAL", "ALERT", "EMERGENCY"}

// AppendFilter joins two filter parts with newline (implicit AND).
// Returns the other part unchanged if either is empty.
func AppendFilter(base, part string) string {
	if base == "" {
		return part
	}
	if part == "" {
		return base
	}
	return base + "\n" + part
}

// formatTimestamp converts proto timestamp to UTC string with millisecond precision
// (e.g., "2006-01-02T15:04:05.000Z").
func formatTimestamp(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return ""
	}
	return ts.AsTime().Format("2006-01-02T15:04:05.000Z")
}

// formatLatency converts a proto duration to a human-readable string.
func formatLatency(d *durationpb.Duration) string {
	if d == nil {
		return ""
	}
	return formatDuration(d.AsDuration())
}

// formatDuration formats a time.Duration as a human-readable string (e.g. "1.234s" or "150.000ms").
// Negative durations (e.g. from clock skew) are treated as zero.
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d.Seconds() >= 1 {
		return fmt.Sprintf("%.3fs", d.Seconds())
	}
	return fmt.Sprintf("%.3fms", float64(d.Microseconds())/1000.0)
}

// extractTraceID extracts the trace ID from a full trace resource name.
// Input: "projects/my-project/traces/abc123"
// Output: "abc123"
func extractTraceID(trace string) string {
	parts := strings.Split(trace, "/")
	if len(parts) >= 4 {
		return parts[3]
	}
	return trace
}

// serviceLabelKeys are the resource labels that name a service, in priority
// order: Cloud Run, Kubernetes container and namespace, Cloud Functions.
var serviceLabelKeys = []string{"service_name", "container_name", "namespace_name", "function_name"}

// serviceName derives a service name from resource labels, falling back to
// the resource type when no service label is set.
func serviceName(resourceType string, labels map[string]string) string {
	for _, key := range serviceLabelKeys {
		if v := labels[key]; v != "" {
			return v
		}
	}
	return resourceType
}

// extractServiceName extracts a service name from a log entry's resource labels.
func extractServiceName(entry *loggingpb.LogEntry) string {
	if entry.Resource == nil {
		return ""
	}
	return serviceName(entry.Resource.Type, entry.Resource.Labels)
}

// structToMap converts a protobuf Struct to a Go map.
func structToMap(s *structpb.Struct) map[string]any {
	if s == nil {
		return nil
	}
	result := make(map[string]any, len(s.Fields))
	for k, v := range s.Fields {
		result[k] = valueToInterface(v)
	}
	return result
}

// valueToInterface converts a protobuf Value to a Go interface.
func valueToInterface(v *structpb.Value) any {
	if v == nil {
		return nil
	}
	switch v := v.Kind.(type) {
	case *structpb.Value_NullValue:
		return nil
	case *structpb.Value_NumberValue:
		return v.NumberValue
	case *structpb.Value_StringValue:
		return v.StringValue
	case *structpb.Value_BoolValue:
		return v.BoolValue
	case *structpb.Value_StructValue:
		return structToMap(v.StructValue)
	case *structpb.Value_ListValue:
		if v.ListValue == nil {
			return nil
		}
		items := make([]any, len(v.ListValue.Values))
		for i, item := range v.ListValue.Values {
			items[i] = valueToInterface(item)
		}
		return items
	default:
		return nil
	}
}
