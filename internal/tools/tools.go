package tools

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// Logging level constants for MCP log notifications.
const (
	logLevelWarning mcp.LoggingLevel = "warning"
	logLevelError   mcp.LoggingLevel = "error"
)

// notifyErrLog is used to log dropped MCP notification errors. Defaults to
// stderr; call SetNotifyLogger at server startup to route to the configured
// errOut writer instead. atomic.Pointer ensures SetNotifyLogger is safe to
// call concurrently with in-flight tool handlers.
var notifyErrLog atomic.Pointer[log.Logger]

func init() {
	notifyErrLog.Store(log.New(os.Stderr, "[mcp-gcp-observability] ", log.LstdFlags))
}

// SetNotifyLogger configures where notification-drop errors are written.
// Safe to call concurrently with tool handlers.
func SetNotifyLogger(l *log.Logger) { notifyErrLog.Store(l) }

// sendProgress sends a progress notification if the request includes a progress token.
func sendProgress(ctx context.Context, req *mcp.CallToolRequest, progress, total float64, message string) {
	if req == nil || req.Session == nil || req.Params == nil {
		return
	}
	token := req.Params.GetProgressToken()
	if token == nil {
		return
	}
	if err := req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
		ProgressToken: token,
		Progress:      progress,
		Total:         total,
		Message:       message,
	}); err != nil {
		notifyErrLog.Load().Printf("progress notification dropped: %v", err)
	}
}

// mcpLog sends a structured log message via MCP logging notification.
func mcpLog(ctx context.Context, req *mcp.CallToolRequest, level mcp.LoggingLevel, logger string, data any) {
	if req == nil || req.Session == nil {
		notifyErrLog.Load().Printf("[%s] %s: %v (no session)", level, logger, data)
		return
	}
	if err := req.Session.Log(ctx, &mcp.LoggingMessageParams{
		Level:  level,
		Logger: logger,
		Data:   data,
	}); err != nil {
		notifyErrLog.Load().Printf("log notification dropped (level=%s logger=%s): %v", level, logger, err)
	}
}

// logAggregationWarnings forwards non-fatal AggregationWarnings to the client.
// Helps operators spot registry typos and sparse coverage. windowLabel
// distinguishes current/baseline/windowA/windowB sites.
func logAggregationWarnings(ctx context.Context, req *mcp.CallToolRequest, logger, metricType, windowLabel string, warnings gcpdata.AggregationWarnings) {
	for _, msg := range aggregationWarningMessages(metricType, windowLabel, warnings) {
		mcpLog(ctx, req, logLevelWarning, logger, msg)
	}
}

// aggregationWarningMessages returns warning messages from AggregationWarnings.
// Pure function; testable without an MCP server context.
func aggregationWarningMessages(metricType, windowLabel string, warnings gcpdata.AggregationWarnings) []string {
	if !warnings.HasAny() {
		return nil
	}
	var msgs []string
	if warnings.TruncatedSeries {
		msgs = append(msgs, fmt.Sprintf(
			"metric %q (%s): query hit the server-side time-series cap (%d series) and the result is truncated; aggregates are computed from a partial set of series only. Narrow the filter or group cardinality before trusting the numbers.",
			metricType, windowLabel, gcpdata.MaxTimeSeries))
	}
	if warnings.SingleGroup {
		msgs = append(msgs, fmt.Sprintf(
			"metric %q (%s): two-stage aggregation returned %d group(s) — verify the configured group_by label actually exists on this metric (registry typo produces this symptom). The fold is mathematically safe but operators should re-check the registry entry.",
			metricType, windowLabel, warnings.GroupCount))
	}
	// Report departed groups (structural issue) before carry-forward (transient gap).
	if warnings.DepartedGroupBuckets > 0 {
		msgs = append(msgs, fmt.Sprintf(
			"metric %q (%s): %d of %d folded buckets dropped at least one departed group (series went silent longer than the carry-forward bound); %d distinct group series departed during the window. Investigate whether the upstream publisher (replica, tenant, leader) actually stopped or whether the bound is too tight.",
			metricType, windowLabel, warnings.DepartedGroupBuckets, warnings.TotalBuckets, warnings.DepartedSeries))
	}
	if warnings.CarryForwardBuckets > 0 {
		msgs = append(msgs, fmt.Sprintf(
			"metric %q (%s): %d of %d folded buckets used carry-forward for at least one group (transient gap, still within the carry-forward bound); numbers are usable but trend/spike detection may be noisy.",
			metricType, windowLabel, warnings.CarryForwardBuckets, warnings.TotalBuckets))
	}
	return msgs
}

func stripTruncatedSeries(series []gcpdata.MetricTimeSeries) ([]gcpdata.MetricTimeSeries, bool) {
	return gcpdata.StripTruncationSentinel(series)
}

func joinNote(parts ...string) string {
	var out string
	for _, part := range parts {
		if part == "" {
			continue
		}
		if out == "" {
			out = part
		} else {
			out += " " + part
		}
	}
	return out
}

func aggregationWarningsNote(metricType, windowLabel string, warnings gcpdata.AggregationWarnings) string {
	return joinNote(aggregationWarningMessages(metricType, windowLabel, warnings)...)
}

// invalidAggregationSpecError reports whether err is a registry validation error
// (operator must fix YAML) vs a transient GCP failure (safe to retry).
func invalidAggregationSpecError(err error) bool {
	return errors.Is(err, metrics.ErrInvalidAggregationSpec)
}

// formatRegistryMisconfigError wraps validation errors in a user-facing message
// pointing to the YAML that needs fixing.
func formatRegistryMisconfigError(metricType string, err error) string {
	return fmt.Sprintf("Registry misconfiguration for metric %q: %v. Fix the metric's aggregation block in the registry YAML — retrying will not help.", metricType, err)
}

// reportUnsupportedPoints sums and logs dropped points across series.
// Returns total so callers can surface drops on results.
func reportUnsupportedPoints(ctx context.Context, req *mcp.CallToolRequest, tool, metricType string, series []gcpdata.MetricTimeSeries) int {
	total := 0
	for _, s := range series {
		total += s.UnsupportedCount
	}
	if total > 0 {
		mcpLog(ctx, req, logLevelWarning, tool,
			fmt.Sprintf("metric %q: dropped %d points with unsupported or malformed value types during decode", metricType, total))
	}
	return total
}

// errResult creates a tool error result.
func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

// buildTimeFilter constructs a Cloud Logging timestamp filter from TimeFilterInput.
func buildTimeFilter(in TimeFilterInput) (string, error) {
	startTime := in.StartTime
	endTime := in.EndTime

	if startTime == "" && endTime == "" {
		startTime = time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	}

	var parsedStart, parsedEnd time.Time
	var filter string
	if startTime != "" {
		var err error
		parsedStart, err = time.Parse(time.RFC3339, startTime)
		if err != nil {
			return "", fmt.Errorf("invalid start_time %q: must be RFC3339 format (e.g. 2025-01-15T00:00:00Z)", startTime)
		}
		filter = fmt.Sprintf(`timestamp>="%s"`, startTime)
	}
	if endTime != "" {
		var err error
		parsedEnd, err = time.Parse(time.RFC3339, endTime)
		if err != nil {
			return "", fmt.Errorf("invalid end_time %q: must be RFC3339 format (e.g. 2025-01-15T23:59:59Z)", endTime)
		}
		// Default start to 24h before end to avoid unbounded scans
		if startTime == "" {
			parsedStart = parsedEnd.Add(-24 * time.Hour)
			filter = fmt.Sprintf(`timestamp>="%s"`, parsedStart.Format(time.RFC3339))
		}
		filter = gcpdata.AppendFilter(filter, fmt.Sprintf(`timestamp<="%s"`, endTime))
	}

	if !parsedStart.IsZero() && !parsedEnd.IsZero() && !parsedEnd.After(parsedStart) {
		return "", fmt.Errorf("end_time must be after start_time (got start=%s, end=%s)", startTime, endTime)
	}

	return filter, nil
}

// requireClient validates that client is non-nil, panicking on programming errors.
func requireClient(client *gcpclient.Client) {
	if client == nil {
		panic("nil client")
	}
}

// resolveProject returns the project ID, falling back to defaultProject.
func resolveProject(projectID, defaultProject string) (string, error) {
	if projectID != "" {
		return projectID, nil
	}
	if defaultProject != "" {
		return defaultProject, nil
	}
	return "", fmt.Errorf("project_id must not be empty: either omit it to use the default project, or provide a valid project ID")
}

// clampLimit returns limit clamped to [1, maxLimit], falling back to fallback
// when limit is non-positive.
func clampLimit(limit, fallback, maxLimit int) int {
	if limit <= 0 {
		return fallback
	}
	if limit > maxLimit {
		return maxLimit
	}
	return limit
}

// ptrTrue returns a pointer to true, for use in ToolAnnotations.
func ptrTrue() *bool {
	v := true
	return &v
}

// safeClassification converts a Classification to string for JSON output,
// mapping the zero value ClassNotComputed ("") to ClassInsufficientData
// so consumers never receive an empty classification string.
func safeClassification(c metrics.Classification) string {
	if c == metrics.ClassNotComputed {
		return string(metrics.ClassInsufficientData)
	}
	return string(c)
}
