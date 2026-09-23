package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/codes"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// baselineMode selects the time ranges a metrics tool compares the current
// window against.
type baselineMode string

const (
	baselinePrevWindow      baselineMode = "prev_window"
	baselineSameWeekdayHour baselineMode = "same_weekday_hour"
	baselinePreEvent        baselineMode = "pre_event"
)

// baselineModes lists the supported baseline modes; the first is the default.
var baselineModes = []baselineMode{baselinePrevWindow, baselineSameWeekdayHour, baselinePreEvent}

// baselineSpec is a parsed baseline_mode input: the mode and, for pre_event,
// the event time the baseline ends at (zero for the other modes).
type baselineSpec struct {
	mode      baselineMode
	eventTime time.Time
}

// parseBaseline resolves the schema-validated baseline_mode input and, for
// pre_event, parses the required event_time once so a bad value is reported
// as an input error.
func parseBaseline(mode, eventTimeStr string) (baselineSpec, error) {
	b := baselineSpec{mode: baselineMode(mode)}
	if !slices.Contains(baselineModes, b.mode) {
		return baselineSpec{}, fmt.Errorf("invalid baseline_mode %q: must be one of %v", mode, baselineModes)
	}
	if b.mode != baselinePreEvent {
		return b, nil
	}
	if eventTimeStr == "" {
		return baselineSpec{}, errors.New("event_time is required when baseline_mode is 'pre_event'")
	}
	eventTime, err := parseRFC3339Opt(eventTimeStr, "event_time")
	if err != nil {
		return baselineSpec{}, err
	}
	b.eventTime = eventTime
	return b, nil
}

// weeklyBaselineWeeks is the number of prior same-weekday windows sampled for
// the same_weekday_hour baseline mode.
const weeklyBaselineWeeks = 4

// preEventBaselineSpan is how far before event_time the pre_event baseline
// looks.
const preEventBaselineSpan = 30 * time.Minute

// baselineWindow is one time range sampled for a baseline. label names it in
// warnings and errors.
type baselineWindow struct {
	start, end time.Time
	label      string
}

// windows returns the time ranges b samples as the baseline of the current
// window [start, end): the window just before it (prev_window), the same
// wall-clock window in each of the previous weeklyBaselineWeeks weeks
// (same_weekday_hour), or the preEventBaselineSpan before the event time
// (pre_event).
func (b baselineSpec) windows(start, end time.Time) []baselineWindow {
	switch b.mode {
	case baselineSameWeekdayHour:
		out := make([]baselineWindow, weeklyBaselineWeeks)
		for i := range out {
			days := -7 * (i + 1)
			out[i] = baselineWindow{
				start: start.AddDate(0, 0, days),
				end:   end.AddDate(0, 0, days),
				label: fmt.Sprintf("baseline (%s week -%d)", b.mode, i+1),
			}
		}
		return out
	case baselinePreEvent:
		return []baselineWindow{{start: b.eventTime.Add(-preEventBaselineSpan), end: b.eventTime, label: "baseline (pre_event)"}}
	default:
		return []baselineWindow{{start: start.Add(-end.Sub(start)), end: start, label: "baseline (prev_window)"}}
	}
}

// windowResult is the outcome of one time-series query.
type windowResult struct {
	series   []gcpdata.MetricTimeSeries
	warnings gcpdata.QueryWarnings
	err      error
}

// hasPoints reports whether the query succeeded with at least one point.
func (r windowResult) hasPoints() bool {
	return r.err == nil && slices.ContainsFunc(r.series, func(s gcpdata.MetricTimeSeries) bool { return len(s.Points) > 0 })
}

// queryFunc runs one time-series query; the metrics tools bind it to the raw
// or the aggregated querier method.
type queryFunc func(gcpdata.QueryTimeSeriesParams) ([]gcpdata.MetricTimeSeries, gcpdata.QueryWarnings, error)

// runQueries runs query once per params concurrently and returns the results
// by index. A panicking query is recorded as a *panicError in its result.
func runQueries(ctx context.Context, tool string, params []gcpdata.QueryTimeSeriesParams, query queryFunc) []windowResult {
	results := make([]windowResult, len(params))
	errs := runParallel(ctx, tool, len(params), 0, func(i int) error {
		r := &results[i]
		r.series, r.warnings, r.err = query(params[i])
		return r.err
	})
	for i, err := range errs {
		results[i].err = err
	}
	return results
}

// queryWithBaseline runs the current-window query and then, only when it
// returned points, one query per baseline window concurrently. A failed or
// empty current window ends the tool call before the baseline matters, so
// querying the baseline anyway would only spend Monitoring quota; the
// baseline results are nil then.
func queryWithBaseline(ctx context.Context, tool string, current gcpdata.QueryTimeSeriesParams, windows []baselineWindow, query queryFunc) (windowResult, []windowResult) {
	cur := runQueries(ctx, tool, []gcpdata.QueryTimeSeriesParams{current}, query)[0]
	if !cur.hasPoints() {
		return cur, nil
	}
	params := make([]gcpdata.QueryTimeSeriesParams, len(windows))
	for i, w := range windows {
		params[i] = current
		params[i].Start, params[i].End = w.start, w.end
	}
	return cur, runQueries(ctx, tool, params, query)
}

// collectBaseline reports the warnings of the baseline windows and applies
// the baseline failure policy: the baseline fails only when no window produced
// data and at least one query failed; failed windows alongside usable ones
// yield a partial-failure note. The warnings of a multi-window mode are summed
// into one note so a warning repeated in every window is reported once. The
// returned note carries the partial-failure note and the warning note. When
// a query panicked the error wraps the *panicError.
func collectBaseline(ctx context.Context, req *mcp.CallToolRequest, tool, metricType string, mode baselineMode, windows []baselineWindow, results []windowResult) (string, error) {
	var warnings gcpdata.QueryWarnings
	var errs []error
	withData, panics := 0, 0
	for i, r := range results {
		warnings.Add(r.warnings)
		if r.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", windows[i].label, r.err))
		}
		if isPanic(r.err) {
			panics++
		}
		if r.hasPoints() {
			withData++
		}
	}
	warningsLabel := windows[0].label
	if len(windows) > 1 {
		warningsLabel = fmt.Sprintf("baseline (%s, summed over %d windows)", mode, len(windows))
	}
	warningsNote := reportQueryWarnings(ctx, req, tool, metricType, warningsLabel, warnings)
	if len(errs) == 0 {
		return warningsNote, nil
	}
	joined := errors.Join(errs...)
	if withData == 0 {
		if panics > 0 {
			return "", fmt.Errorf("%d of %d baseline queries panicked (a bug, not a transient failure): %w", panics, len(windows), joined)
		}
		if len(errs) == len(windows) {
			return "", fmt.Errorf("all %d baseline queries failed: %w", len(windows), joined)
		}
		return "", fmt.Errorf("%d of %d baseline queries failed and the rest returned no data: %w", len(errs), len(windows), joined)
	}
	var partial string
	if panics > 0 {
		mcpLog(ctx, req, logLevelError, tool,
			fmt.Sprintf("baseline partial failure: UNEXPECTED PANICS in %d of %d queries; %v", panics, len(windows), joined))
		partial = fmt.Sprintf("Baseline partial failure (%s): UNEXPECTED PANICS occurred in %d of %d baseline queries. This is a bug in the code, not a transient failure. Baseline computed from %d windows, but results may be unreliable. Please report this issue.",
			mode, panics, len(windows), withData)
	} else {
		mcpLog(ctx, req, logLevelWarning, tool,
			fmt.Sprintf("baseline partial failure: %d of %d queries failed (%v); using %d windows of data", len(errs), len(windows), joined, withData))
		partial = fmt.Sprintf("Baseline partial failure (%s): %d of %d baseline windows could not be fetched; baseline computed from %d windows. Results may be less reliable.",
			mode, len(errs), len(windows), withData)
	}
	return joinNote(partial, warningsNote), nil
}

// baselineCodeGuidance is the advice for baseline failures that retrying
// cannot fix and that sharedCodeGuidance does not cover.
var baselineCodeGuidance = []codeGuidance{
	{codes.NotFound, "Cloud Monitoring found no such metric or project for the baseline window; retrying will not help."},
	{codes.InvalidArgument, "Cloud Monitoring rejected the baseline query as invalid; retrying will not help."},
}

// baselineFailureAdvice tells whether retrying can fix err, the failure
// collectBaseline returned for results: a panic is a bug; otherwise the
// guidance for each distinct gRPC code among the failed queries, where codes
// without specific guidance are worth a retry.
func baselineFailureAdvice(err error, results []windowResult) string {
	if isPanic(err) {
		return "This is a bug in the code, not a transient failure; retrying will not help. Please report this issue."
	}
	errs := make([]error, len(results))
	for i, r := range results {
		errs[i] = r.err
	}
	return errorGuidance(errs, "You can retry.", baselineCodeGuidance...)
}

// toChartPoints converts metric points to the compact chartPoint slice used by
// the chart widget, filtering out NaN and Inf values.
func toChartPoints(pts []metrics.Point) []chartPoint {
	out := make([]chartPoint, 0, len(pts))
	for _, p := range pts {
		if !math.IsNaN(p.Value) && !math.IsInf(p.Value, 0) {
			out = append(out, chartPoint{TS: p.Timestamp.Unix(), V: p.Value})
		}
	}
	return out
}

// chartCallResult returns the handler results of a chart tool: result goes to
// structuredContent, where the chart widget reads its points, while the
// LLM-facing text content is the JSON of llm — a copy of result with the
// chart points removed, so raw time-series data never reaches the model. The
// ui meta binds the widget at uri on every result, for hosts that missed the
// resource URI in the tool definition.
func chartCallResult[T any](result, llm *T, uri string) (*mcp.CallToolResult, *T) {
	text, err := json.Marshal(llm)
	if err != nil {
		return errResult(fmt.Sprintf("internal error: failed to marshal result: %v", err)), nil
	}
	return &mcp.CallToolResult{
		Meta:    mcp.Meta{"ui": map[string]any{"resourceUri": uri}},
		Content: []mcp.Content{&mcp.TextContent{Text: string(text)}},
	}, result
}

// withoutChart returns a copy of r without chart points, for the LLM-facing
// content.
func (r *MetricSnapshotResult) withoutChart() *MetricSnapshotResult {
	c := *r
	c.ChartPoints = nil
	return &c
}

func RegisterMetricsSnapshot(s *mcp.Server, d Deps) {
	requireQuerier(d.Querier)
	requireRegistry(d.Registry)
	mcp.AddTool(s, &mcp.Tool{
		Name: "metrics_snapshot",
		Description: applyMode(d.Mode, "Get a semantic snapshot of a metric with baseline comparison, trend detection, and classification. "+
			"Returns current value, baseline delta, trend, SLO breach status, and a classification label. "+
			"Also renders an interactive time-series chart inline in the chat (hosts that support MCP app widgets). "+
			"The response includes `available_labels` — the metric.labels.* and resource.labels.* keys this metric accepts — "+
			"so follow-up calls can construct valid filters without guessing. "+
			"Use metrics_list first to discover metric_type values. "+
			"After getting a snapshot, use metrics_top_contributors to drill down by dimension, "+
			"or metrics_related to check correlated signals. "+
			"For comparing two specific time windows (e.g. before/after deploy), use metrics_compare instead."),
		Annotations: readOnlyAnnotations,
		// Static UI resource URI — signals chart support to the host for prefetch.
		// Per-call data is delivered via structuredContent through the MCP Apps bridge.
		Meta: mcp.Meta{"ui": map[string]any{"resourceUri": chartStaticURI}},
		InputSchema: projectInputSchema[MetricsSnapshotInput](d.Project,
			nonEmptyProp("metric_type"),
			enumProp("window", metricWindowNames(), defaultMetricWindow),
			enumProp("baseline_mode", baselineModes, baselineModes[0]),
		),
		OutputSchema: outputSchemaFor[MetricSnapshotResult](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in MetricsSnapshotInput) (*mcp.CallToolResult, *MetricSnapshotResult, error) {
		project, err := d.Project.Resolve(in.ProjectID)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		stepSeconds := int64(in.StepSeconds)
		if stepSeconds == 0 {
			stepSeconds = metrics.DefaultStepSeconds
		}
		if stepSeconds < 10 {
			return errResult("step_seconds must be at least 10"), nil, nil
		}

		windowStr, windowDur, err := parseWindow(in.Window)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		baseline, err := parseBaseline(in.BaselineMode, in.EventTime)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		meta := d.Registry.Lookup(in.MetricType)
		now := time.Now().UTC()
		start := now.Add(-windowDur)

		sendProgress(ctx, req, 1, 3, "Looking up metric descriptor")

		descriptor, errRes := lookupMetricDescriptor(ctx, req, d.Querier, "metrics_snapshot", project, in.MetricType)
		if errRes != nil {
			return errRes, nil, nil
		}

		aggSpec, errRes := resolveValidAggSpec(ctx, req, "metrics_snapshot", in.MetricType, meta)
		if errRes != nil {
			return errRes, nil, nil
		}

		sendProgress(ctx, req, 2, 3, "Querying current window and baseline ("+string(baseline.mode)+")")

		currentParams := gcpdata.QueryTimeSeriesParams{
			Project:     project,
			MetricType:  in.MetricType,
			LabelFilter: in.Filter,
			Start:       start,
			End:         now,
			StepSeconds: stepSeconds,
			MetricKind:  descriptor.Kind,
			ValueType:   descriptor.ValueType,
		}
		baselineWindows := baseline.windows(start, now)
		current, baselineResults := queryWithBaseline(ctx, "metrics_snapshot", currentParams, baselineWindows,
			func(p gcpdata.QueryTimeSeriesParams) ([]gcpdata.MetricTimeSeries, gcpdata.QueryWarnings, error) {
				return d.Querier.QueryTimeSeriesAggregated(ctx, p, aggSpec)
			})

		currentWarningsNote := reportQueryWarnings(ctx, req, "metrics_snapshot", in.MetricType, "current", current.warnings)
		if err := current.err; err != nil {
			mcpLog(ctx, req, logLevelError, "metrics_snapshot", fmt.Sprintf("current window query failed: %v", err))
			if invalidAggregationSpecError(err) {
				return errResult(formatRegistryMisconfigError(in.MetricType, err)), nil, nil
			}
			if isInvalidFilterError(err) {
				return errResult(enrichInvalidFilterError(ctx, req, d.Querier, project, in.MetricType, in.Filter, err)), nil, nil
			}
			return gcpErrorResult(fmt.Sprintf("Failed to query metric: %v", err), err, ""), nil, nil
		}

		currentPoints := mergePoints(current.series)
		if len(currentPoints) == 0 {
			expected := expectedPointsForWindow(windowDur, int(stepSeconds))
			r := &MetricSnapshotResult{
				MetricType:               in.MetricType,
				Kind:                     string(meta.Kind),
				Unit:                     meta.Unit,
				AutoDetected:             meta.AutoDetected,
				NoData:                   true,
				Note:                     joinNote(emptyWindowMessage(in.MetricType, windowStr, descriptor.Kind, in.Filter), currentWarningsNote),
				BaselineMode:             string(baseline.mode),
				Trend:                    "unchanged",
				Classification:           string(metrics.ClassInsufficientData),
				ClassificationConfidence: string(metrics.ConfidenceLow),
				DataQuality: metrics.DataQuality{
					ExpectedPoints:  expected,
					ActualPoints:    0,
					NonFinitePoints: current.warnings.NonFinitePoints,
					Reliable:        false,
				},
				Window: WindowInfo{
					From: start.Format(time.RFC3339),
					To:   now.Format(time.RFC3339),
				},
				AvailableLabels: availableLabelsFromDescriptor(ctx, req, d.Querier, project, in.MetricType, descriptor),
			}
			res, out := chartCallResult(r, r.withoutChart(), chartStaticURI)
			return res, out, nil
		}

		var baselineErrNote string
		baselineNote, err := collectBaseline(ctx, req, "metrics_snapshot", in.MetricType, baseline.mode, baselineWindows, baselineResults)
		var baselineStats metrics.BaselineStats
		if err != nil {
			mcpLog(ctx, req, logLevelError, "metrics_snapshot", fmt.Sprintf("baseline query failed: %v", err))
			baselineErrNote = fmt.Sprintf("Baseline query (%s) failed: %v. %s Returning current-window snapshot with baseline_reliable=false; delta fields are not meaningful.",
				baseline.mode, err, baselineFailureAdvice(err, baselineResults))
		} else {
			buckets := make([][]metrics.Point, len(baselineResults))
			for i, r := range baselineResults {
				if r.err == nil {
					buckets[i] = mergePoints(r.series)
				}
			}
			// Every window of a mode has the same span. A single-window mode
			// makes ComputeRobustBaselineStats reduce to plain mean/stddev.
			span := baselineWindows[0].end.Sub(baselineWindows[0].start)
			baselineStats = metrics.ComputeRobustBaselineStats(buckets, expectedPointsForWindow(span, int(stepSeconds)))
		}

		sendProgress(ctx, req, 3, 3, "Processing results")

		// Process.
		f := metrics.ProcessWithBaselineStats(currentPoints, baselineStats, meta, int(stepSeconds), metrics.Window{Start: start, End: now})

		// Build output.
		result := &MetricSnapshotResult{
			MetricType:                 in.MetricType,
			Kind:                       string(meta.Kind),
			Unit:                       meta.Unit,
			AutoDetected:               meta.AutoDetected,
			Current:                    f.Current,
			Baseline:                   f.Baseline,
			DeltaPct:                   f.DeltaPct,
			BaselineMode:               string(baseline.mode),
			BaselineReliable:           f.BaselineReliable,
			Stddev:                     f.Stddev,
			CV:                         f.CV,
			Trend:                      string(f.Trend),
			TrendScore:                 f.TrendScore,
			Classification:             string(f.Classification),
			ClassificationConfidence:   string(f.Confidence),
			SLOBreach:                  f.SLOBreach,
			SLOThreshold:               meta.SLOThreshold,
			BreachDurationSeconds:      f.BreachDurationSeconds,
			CurrentBreachStreakSeconds: f.CurrentBreachStreakSeconds,
			BreachTransitions:          f.BreachTransitions,
			StepChangePct:              f.StepChangePct,
			MaxZScore:                  f.MaxZScore,
			SpikeCount:                 f.SpikeCount,
			SpikeRatio:                 f.SpikeRatio,
			SaturationDetected:         f.SaturationDetected,
			DataQuality:                f.DataQuality,
			Window: WindowInfo{
				From: start.Format(time.RFC3339),
				To:   now.Format(time.RFC3339),
			},
			Note: joinNote(baselineErrNote, currentWarningsNote, baselineNote),
		}
		result.DataQuality.NonFinitePoints = current.warnings.NonFinitePoints

		if f.StepChangeAt != nil {
			result.StepChangeAt = f.StepChangeAt.Format(time.RFC3339)
		}

		if meta.Kind == metrics.KindLatency {
			result.Percentiles = &PercentileInfo{
				P50:       f.P50,
				P95:       f.P95,
				P99:       f.P99,
				TailRatio: f.TailRatio,
			}
		}

		result.AvailableLabels = availableLabelsFromDescriptor(ctx, req, d.Querier, project, in.MetricType, descriptor)

		result.ChartPoints = toChartPoints(currentPoints)
		res, out := chartCallResult(result, result.withoutChart(), chartStaticURI)
		return res, out, nil
	})
}

type MetricSnapshotResult struct {
	MetricType   string `json:"metric_type"`
	Kind         string `json:"kind"`
	Unit         string `json:"unit"`
	AutoDetected bool   `json:"auto_detected,omitempty"`

	NoData bool   `json:"no_data,omitempty"`
	Note   string `json:"note,omitempty"`

	Current          float64 `json:"current"`
	Baseline         float64 `json:"baseline"`
	DeltaPct         float64 `json:"delta_pct"`
	BaselineMode     string  `json:"baseline_mode"`
	BaselineReliable bool    `json:"baseline_reliable"`

	// Distribution (all metric kinds).
	Stddev float64 `json:"stddev,omitempty"`
	CV     float64 `json:"cv,omitempty"`

	// Trend: direction string + normalized magnitude.
	// trend_score is total drift across the window as a fraction of baseline
	// (e.g. 0.10 = drifted 10% of baseline). Window-length independent.
	Trend      string  `json:"trend"`
	TrendScore float64 `json:"trend_score,omitempty"`

	Classification           string `json:"classification"`
	ClassificationConfidence string `json:"classification_confidence"`

	SLOBreach                  bool     `json:"slo_breach"`
	SLOThreshold               *float64 `json:"slo_threshold,omitempty"`
	BreachDurationSeconds      int      `json:"breach_duration_seconds,omitempty"`
	CurrentBreachStreakSeconds int      `json:"current_breach_streak_seconds,omitempty"`
	// BreachTransitions counts SLO threshold crossings in the window.
	// High value with moderate breach_ratio indicates flapping/oscillation.
	BreachTransitions int `json:"breach_transitions,omitempty"`

	// StepChange: timestamp + magnitude (% shift between first and last thirds).
	StepChangeAt  string  `json:"step_change_at,omitempty"`
	StepChangePct float64 `json:"step_change_pct,omitempty"`

	// Spike evidence: z-score of the most extreme point and spike count/ratio.
	MaxZScore  float64 `json:"max_z_score,omitempty"`
	SpikeCount int     `json:"spike_count,omitempty"`
	SpikeRatio float64 `json:"spike_ratio,omitempty"`

	// SaturationDetected is true when the tail of the series is within 5% of
	// the configured saturation_cap. Mirrors the classification label explicitly.
	SaturationDetected bool `json:"saturation_detected,omitempty"`

	Percentiles *PercentileInfo     `json:"percentiles,omitempty"`
	DataQuality metrics.DataQuality `json:"data_quality"`
	Window      WindowInfo          `json:"window"`

	AvailableLabels *AvailableLabels `json:"available_labels,omitempty"`

	// ChartPoints holds the raw time-series points for the UI chart widget.
	// Included in structuredContent; chartCallResult excludes it from the
	// LLM-facing content field (see withoutChart).
	ChartPoints []chartPoint `json:"chart_points,omitempty"`
}

type PercentileInfo struct {
	P50       float64 `json:"p50"`
	P95       float64 `json:"p95"`
	P99       float64 `json:"p99"`
	TailRatio float64 `json:"tail_ratio,omitempty"`
}

type WindowInfo struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// expectedPointsForWindow is the ideal point count for a window of the given
// duration at the given step size.
func expectedPointsForWindow(windowDur time.Duration, stepSeconds int) int {
	return int(windowDur/(time.Duration(stepSeconds)*time.Second)) + 1
}

// mergePoints concatenates points from all series and sorts by timestamp.
func mergePoints(series []gcpdata.MetricTimeSeries) []metrics.Point {
	var all []metrics.Point
	for _, s := range series {
		all = append(all, s.Points...)
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].Timestamp.Before(all[j].Timestamp)
	})
	return all
}

func emptyWindowMessage(metricType, window string, kind gcpdata.MetricKind, labelFilter string) string {
	base := fmt.Sprintf("Metric %q has no data points in the last %s. The metric is registered in Cloud Monitoring but the window is empty. Likely cause: %s.",
		metricType, window, gcpdata.EmptyWindowReason(kind))
	if labelFilter != "" {
		base += fmt.Sprintf(" The label filter %q may also be excluding every series — try removing it.", labelFilter)
	} else {
		base += " Try widening the window or removing any dimension/filter to confirm."
	}
	return base
}

// metricWindows lists the supported metric analysis windows in ascending
// order.
var metricWindows = []struct {
	name string
	dur  time.Duration
}{
	{"15m", 15 * time.Minute},
	{"30m", 30 * time.Minute},
	{"1h", time.Hour},
	{"3h", 3 * time.Hour},
	{"6h", 6 * time.Hour},
	{"24h", 24 * time.Hour},
}

// defaultMetricWindow is the window used when the input omits one.
const defaultMetricWindow = "1h"

// metricWindowNames returns the names of metricWindows for the input schema.
func metricWindowNames() []string {
	names := make([]string, len(metricWindows))
	for i, w := range metricWindows {
		names[i] = w.name
	}
	return names
}

// parseWindow resolves the schema-validated window input to its name and
// duration.
func parseWindow(s string) (string, time.Duration, error) {
	for _, w := range metricWindows {
		if w.name == s {
			return w.name, w.dur, nil
		}
	}
	return "", 0, fmt.Errorf("invalid window %q: must be one of %v", s, metricWindowNames())
}
