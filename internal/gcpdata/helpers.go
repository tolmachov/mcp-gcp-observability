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

// validSeverities is the set of valid Cloud Logging severity levels.
var validSeverities = map[string]bool{
	"DEFAULT":   true,
	"DEBUG":     true,
	"INFO":      true,
	"NOTICE":    true,
	"WARNING":   true,
	"ERROR":     true,
	"CRITICAL":  true,
	"ALERT":     true,
	"EMERGENCY": true,
}

// IsValidSeverity checks if a severity string is a valid Cloud Logging severity.
func IsValidSeverity(s string) bool {
	return validSeverities[strings.ToUpper(s)]
}

// AppendFilter joins two filter parts with a newline (implicit AND in Cloud Logging filter syntax).
// Returns the other part unchanged if either part is empty.
func AppendFilter(base, part string) string {
	if base == "" {
		return part
	}
	if part == "" {
		return base
	}
	return base + "\n" + part
}

// formatTimestamp converts a proto timestamp to a UTC string with millisecond precision
// (e.g. "2006-01-02T15:04:05.000Z"). This is similar to RFC3339 but always includes
// exactly 3 fractional digits and a literal "Z" suffix.
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

// extractServiceName extracts a service name from a log entry's resource labels.
func extractServiceName(entry *loggingpb.LogEntry) string {
	if entry.Resource == nil {
		return ""
	}
	labels := entry.Resource.Labels

	// Cloud Run: service_name
	if name, ok := labels["service_name"]; ok {
		return name
	}
	// K8s: container_name or namespace_name
	if name, ok := labels["container_name"]; ok {
		return name
	}
	if name, ok := labels["namespace_name"]; ok {
		return name
	}
	return entry.Resource.Type
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
