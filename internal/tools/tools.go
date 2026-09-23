package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/pprof/profile"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// RegistrationMode controls description verbosity when registering tools.
// The zero value is ModeStandard (full descriptions).
type RegistrationMode int

const (
	ModeStandard RegistrationMode = iota // full descriptions
	ModeCompact                          // concise descriptions (first sentence only)
)

// compactDesc returns the first sentence of desc — everything up to and
// including the first ". " (period followed by space). If desc has no
// such sequence, it is returned unchanged.
//
// Caveat: this naive scan cuts at abbreviations like "e.g." or "i.e." that
// occur in the first sentence — see TestCompactDesc for the documented
// behavior. Tool descriptions should avoid such abbreviations in the
// opening sentence; TestCompactModeRealDescriptionsSane guards against
// regressions.
func compactDesc(desc string) string {
	idx := strings.Index(desc, ". ")
	if idx >= 0 {
		return desc[:idx+1]
	}
	return desc
}

// applyMode returns the full description for ModeStandard and the
// compactDesc-trimmed version for ModeCompact. Panics on unknown modes
// so that adding a new RegistrationMode without updating this switch
// fails loudly at startup rather than silently shipping the full string.
func applyMode(mode RegistrationMode, full string) string {
	switch mode {
	case ModeStandard:
		return full
	case ModeCompact:
		return compactDesc(full)
	default:
		panic(fmt.Sprintf("unknown RegistrationMode %d", int(mode)))
	}
}

// Deps bundles every dependency that any Register* function might need. Each
// Register* uses the subset relevant to its tool. Putting them in a single
// struct decouples adding a new dependency (one field change) from updating
// 22+ Register* call sites, and keeps every tool registration a uniform
// (server, deps) call; the variant builders set Mode per spec.
//
// Every backend is an interface (Logs/Errors/Traces/Profiler/Querier) so tool
// handlers can be unit-tested with fakes rather than a real GCP client.
type Deps struct {
	Logs     gcpdata.LogsQuerier
	Errors   gcpdata.ErrorsQuerier
	Traces   gcpdata.TraceQuerier
	Profiler gcpdata.ProfilerQuerier
	Querier  gcpdata.MetricsQuerier

	Registry *metrics.Registry
	Project  ProjectPolicy
	Mode     RegistrationMode
}

const (
	LogsHardLimit   = gcpdata.LogsHardLimit
	ErrorsHardLimit = 100
)

// WithMode returns a copy of d with Mode set to m. Variant builders use it to
// derive a per-variant Deps from the shared base, expressing "clone then set
// Mode" in code rather than leaving it to rely on Deps being passed by value.
func (d Deps) WithMode(m RegistrationMode) Deps {
	d.Mode = m
	return d
}

// readOnlyAnnotations is shared by every tool: all of them only read GCP
// observability data, so calls are idempotent and reach an open world.
var readOnlyAnnotations = &mcp.ToolAnnotations{
	ReadOnlyHint:   true,
	OpenWorldHint:  new(true),
	IdempotentHint: true,
}

// Logging level constants for MCP log notifications.
const (
	logLevelWarning mcp.LoggingLevel = "warning"
	logLevelError   mcp.LoggingLevel = "error"
)

// notifyErrLog is used to log dropped MCP notification errors. Defaults to
// slog.Default(); call SetNotifyLogger at server startup to route to the
// configured errOut writer instead. atomic.Pointer ensures SetNotifyLogger
// is safe to call concurrently with in-flight tool handlers.
var notifyErrLog atomic.Pointer[slog.Logger]

func init() {
	notifyErrLog.Store(slog.Default())
}

// SetNotifyLogger configures where notification-drop errors are written.
// Safe to call concurrently with tool handlers.
func SetNotifyLogger(l *slog.Logger) { notifyErrLog.Store(l) }

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
		notifyErrLog.Load().Warn("progress notification dropped", "err", err)
	}
}

// startProgressHeartbeat emits a progress notification every interval until the
// returned stop function is called, keeping the MCP client's request-timeout
// timer from firing during a long-running operation (progress notifications
// reset that timer). It is a no-op when the request carries no progress token.
//
// A single goroutine owns all sends, and stop() blocks until that goroutine has
// exited, so no notification is ever sent after the handler returns. Handlers
// that already stream real progress (e.g. profiler_trends) don't need this.
func startProgressHeartbeat(ctx context.Context, req *mcp.CallToolRequest, message string) (stop func()) {
	if req == nil || req.Params == nil || req.Params.GetProgressToken() == nil {
		return func() {}
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		var ticks float64
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				ticks++
				// Total is unknown (0 = indeterminate); the incrementing
				// Progress value is enough to reset the client's timer.
				sendProgress(ctx, req, ticks, 0, message)
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}

// mcpLog sends a structured log message via MCP logging notification.
func mcpLog(ctx context.Context, req *mcp.CallToolRequest, level mcp.LoggingLevel, logger string, data any) {
	if req == nil || req.Session == nil {
		notifyErrLog.Load().Warn("log notification: no session", "level", level, "logger", logger, "data", data)
		return
	}
	if err := req.Session.Log(ctx, &mcp.LoggingMessageParams{
		Level:  level,
		Logger: logger,
		Data:   data,
	}); err != nil {
		notifyErrLog.Load().Warn("log notification dropped", "level", level, "logger", logger, "err", err)
	}
}

// reportQueryWarnings logs each non-fatal query warning to the client and
// returns them joined as a note for the tool result, so the log and the note
// always say the same thing. windowLabel names the queried window (current,
// baseline (prev_window), window_a, ...).
func reportQueryWarnings(ctx context.Context, req *mcp.CallToolRequest, tool, metricType, windowLabel string, warnings gcpdata.QueryWarnings) string {
	msgs := queryWarningMessages(metricType, windowLabel, warnings)
	for _, msg := range msgs {
		mcpLog(ctx, req, logLevelWarning, tool, msg)
	}
	return joinNote(msgs...)
}

// queryWarningMessages returns one message per warning in warnings.
// Pure function; testable without an MCP server context.
func queryWarningMessages(metricType, windowLabel string, warnings gcpdata.QueryWarnings) []string {
	var msgs []string
	if warnings.UnsupportedPoints > 0 {
		msgs = append(msgs, fmt.Sprintf(
			"metric %q (%s): dropped %d point(s) with unsupported or malformed value types during decode.",
			metricType, windowLabel, warnings.UnsupportedPoints))
	}
	if warnings.NonFinitePoints > 0 {
		msgs = append(msgs, fmt.Sprintf(
			"metric %q (%s): discarded %d non-finite point(s) (NaN or infinity); no public numeric field contains a non-finite value.",
			metricType, windowLabel, warnings.NonFinitePoints))
	}
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

// joinNote joins the non-empty parts with a space.
func joinNote(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, " ")
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

// metricQueryErrorResult reports failed time-series queries of metricType as
// a tool error: a registry misconfiguration names the YAML to fix, an invalid
// label filter lists the labels the metric accepts, and any other failure is
// msg followed by the guidance for errs. Nil errs are ignored.
func metricQueryErrorResult(ctx context.Context, req *mcp.CallToolRequest, q gcpdata.MetricsQuerier, project, metricType, filter, msg string, errs ...error) *mcp.CallToolResult {
	if slices.ContainsFunc(errs, invalidAggregationSpecError) {
		return ErrorResult(formatRegistryMisconfigError(metricType, errors.Join(errs...)))
	}
	if slices.ContainsFunc(errs, isInvalidFilterError) {
		return ErrorResult(enrichInvalidFilterError(ctx, req, q, project, metricType, filter, errors.Join(errs...)))
	}
	return gcpErrorsResult(msg, errs, "")
}

// lookupMetricDescriptor fetches the Cloud Monitoring descriptor for metricType,
// logging and returning a ready-to-send ErrorResult on failure. Shared by the
// snapshot/top/compare handlers, which all need the descriptor's Kind and
// ValueType to build a query. On success the returned *mcp.CallToolResult is
// nil; callers forward a non-nil one as (errRes, nil, nil). tool names the
// caller for the log message.
func lookupMetricDescriptor(ctx context.Context, req *mcp.CallToolRequest, q gcpdata.MetricsQuerier, tool, project, metricType string) (gcpdata.MetricDescriptorBasic, *mcp.CallToolResult) {
	descriptor, err := q.GetMetricDescriptor(ctx, project, metricType)
	if err != nil {
		mcpLog(ctx, req, logLevelError, tool, fmt.Sprintf("metric descriptor lookup failed: %v", err))
		return descriptor, gcpErrorResult(fmt.Sprintf("Failed to look up metric descriptor: %v", err), err, "Verify the metric_type.")
	}
	return descriptor, nil
}

// resolveValidAggSpec resolves meta's aggregation strategy and validates it,
// logging and returning a ready-to-send ErrorResult on registry
// misconfiguration. Shared by the snapshot/top/compare handlers; on success the
// returned *mcp.CallToolResult is nil. metrics_related is intentionally not a
// caller — it skips a misconfigured related metric rather than failing the
// whole request.
func resolveValidAggSpec(ctx context.Context, req *mcp.CallToolRequest, tool, metricType string, meta metrics.MetricMeta) (metrics.AggregationSpec, *mcp.CallToolResult) {
	aggSpec := meta.ResolveAggregation()
	if err := aggSpec.Validate(); err != nil {
		mcpLog(ctx, req, logLevelError, tool,
			fmt.Sprintf("registry misconfiguration for %s: %v", metricType, err))
		return aggSpec, ErrorResult(formatRegistryMisconfigError(metricType, err))
	}
	return aggSpec, nil
}

// ErrorResult creates a tool error result.
func ErrorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

// parseRFC3339Opt parses an optional RFC3339 input field; an empty string
// yields the zero time. field names the input in the error message.
func parseRFC3339Opt(s, field string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid %s %q: must be RFC3339 format (e.g. 2025-01-15T00:00:00Z)", field, s)
	}
	return t, nil
}

// parseTimeRange parses the start_time/end_time inputs. A missing end
// defaults to now and a missing start to defaultSpan before the end; the
// range must be non-empty.
func parseTimeRange(startStr, endStr string, defaultSpan time.Duration) (start, end time.Time, err error) {
	start, err = parseRFC3339Opt(startStr, "start_time")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end, err = parseRFC3339Opt(endStr, "end_time")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if end.IsZero() {
		end = time.Now().UTC()
	}
	if start.IsZero() {
		start = end.Add(-defaultSpan)
	}
	if !end.After(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("end_time must be after start_time (got start=%s, end=%s)",
			start.Format(time.RFC3339), end.Format(time.RFC3339))
	}
	return start, end, nil
}

// buildTimeFilter constructs a Cloud Logging timestamp filter from
// TimeFilterInput, defaulting to the 24 hours before end_time (or now). The
// upper bound is only emitted when end_time is given, so an open range keeps
// matching entries ingested while the query runs.
func buildTimeFilter(in TimeFilterInput) (string, error) {
	start, end, err := parseTimeRange(in.StartTime, in.EndTime, 24*time.Hour)
	if err != nil {
		return "", err
	}
	filter := fmt.Sprintf(`timestamp>="%s"`, start.Format(time.RFC3339Nano))
	if in.EndTime != "" {
		filter = gcpdata.AppendFilter(filter, fmt.Sprintf(`timestamp<="%s"`, end.Format(time.RFC3339Nano)))
	}
	return filter, nil
}

// requireLogs/requireErrors/requireTraces/requireProfiler validate that the
// backend a tool needs was wired into Deps, panicking on programming errors at
// registration time rather than nil-dereferencing on the first request.
func requireLogs(q gcpdata.LogsQuerier) {
	if q == nil {
		panic("nil LogsQuerier")
	}
}

func requireErrors(q gcpdata.ErrorsQuerier) {
	if q == nil {
		panic("nil ErrorsQuerier")
	}
}

func requireTraces(q gcpdata.TraceQuerier) {
	if q == nil {
		panic("nil TraceQuerier")
	}
}

func requireProfiler(q gcpdata.ProfilerQuerier) {
	if q == nil {
		panic("nil ProfilerQuerier")
	}
}

// loadProfile resolves the project and fetches the profile (or the
// request-local current-minus-base diff when baseProfileID is set) for
// profiler_top, profiler_peek and profiler_flamegraph. On failure it returns
// the tool error result to send back.
func loadProfile(ctx context.Context, req *mcp.CallToolRequest, d Deps, tool, projectID, profileID, baseProfileID string) (*profile.Profile, gcpdata.ProfileMeta, *mcp.CallToolResult) {
	project, err := d.Project.Resolve(projectID)
	if err != nil {
		return nil, gcpdata.ProfileMeta{}, ErrorResult(err.Error())
	}
	// Fetching an uncached profile scans the Export API and can run long on
	// large projects; heartbeat progress keeps the client request alive.
	stopHeartbeat := startProgressHeartbeat(ctx, req, "Downloading profile…")
	p, meta, err := d.Profiler.GetProfileOrDiff(ctx, project, profileID, baseProfileID)
	stopHeartbeat()
	if err != nil {
		mcpLog(ctx, req, logLevelError, tool, fmt.Sprintf("fetch profile failed: %v", err))
		return nil, gcpdata.ProfileMeta{}, gcpErrorResult(fmt.Sprintf("Failed to fetch profile: %v", err), err, "")
	}
	return p, meta, nil
}

// requireQuerier/requireRegistry guard the two dependencies every metrics tool
// dereferences (d.Querier for the Cloud Monitoring API, d.Registry for metric
// metadata). Like the backend guards above, they turn a missing wiring into a
// loud registration-time panic — converted to a clean startup error by the
// variant builders' recover — instead of a nil-dereference on the first
// metrics_* request mid-session.
func requireQuerier(q gcpdata.MetricsQuerier) {
	if q == nil {
		panic("nil MetricsQuerier")
	}
}

func requireRegistry(r *metrics.Registry) {
	if r == nil {
		panic("nil metrics Registry")
	}
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

// panicError wraps a value recovered from a panic in a runParallel task. It
// exists so a code bug (which no retry will fix) is told apart from a
// transient fetch failure via errors.As (see causeGuidance), rather than by
// substring-matching the error text.
type panicError struct {
	value any
}

func (e *panicError) Error() string {
	return fmt.Sprintf("panic: %v", e.value)
}

// isPanic reports whether err wraps a recovered panic.
func isPanic(err error) bool {
	var pe *panicError
	return errors.As(err, &pe)
}

// runParallel calls task(i) for every i in [0, n), running at most limit
// tasks at a time (limit <= 0 means all at once), and returns the error of
// each task by index (nil on success). Tasks should write only their own
// result slot; any other shared state needs its own synchronization. A task
// is not started once ctx is done — its slot gets a "not started" error
// wrapping ctx.Err() — but a running task must observe cancellation itself. A
// panic is recovered, logged with its stack (labeled with tool) to the
// server-side notifyErrLog, and recorded as a *panicError (detectable via
// isPanic) rather than crashing the server.
func runParallel(ctx context.Context, tool string, n, limit int, task func(i int) error) []error {
	if limit <= 0 {
		limit = n
	}
	errs := make([]error, n)
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					notifyErrLog.Load().Error(tool+": panic in parallel task", "index", i, "panic", r, "stack", string(debug.Stack()))
					errs[i] = &panicError{value: r}
				}
			}()
			if err := ctx.Err(); err != nil {
				errs[i] = fmt.Errorf("not started: %w", err)
				return
			}
			errs[i] = task(i)
		})
	}
	wg.Wait()
	return errs
}
