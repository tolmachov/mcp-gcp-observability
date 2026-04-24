package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

type BaselineMode string

const (
	BaselineModePrevWindow      BaselineMode = "prev_window"
	BaselineModeSameWeekdayHour BaselineMode = "same_weekday_hour"
	BaselineModePreEvent        BaselineMode = "pre_event"
)

// Validate checks that the mode is a known value and that eventTime is provided
// when required. Returns an error suitable for returning directly to the user.
func (m BaselineMode) Validate(eventTime string) error {
	switch m {
	case BaselineModePrevWindow, BaselineModeSameWeekdayHour, BaselineModePreEvent:
	default:
		return fmt.Errorf("invalid baseline_mode %q: must be one of prev_window, same_weekday_hour, pre_event", m)
	}
	if m == BaselineModePreEvent && eventTime == "" {
		return errors.New("event_time is required when baseline_mode is 'pre_event'")
	}
	return nil
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

// snapshotCallResult builds the CallToolResult for metrics_snapshot. It sets
// Content to the JSON of the analysis result without chart_points, so that
// raw time-series data stays in structuredContent only and never reaches the LLM.
func snapshotCallResult(result *MetricSnapshotResult) *mcp.CallToolResult {
	// Shallow copy so we can zero ChartPoints without mutating the caller's struct.
	// The caller returns result as structuredContent (with ChartPoints intact).
	clone := *result
	clone.ChartPoints = nil
	analysisJSON, err := json.Marshal(&clone)
	if err != nil {
		// Should never happen: all fields are JSON-safe types.
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("internal error: failed to marshal result: %v", err)}},
		}
	}
	return &mcp.CallToolResult{
		// Signal the static chart widget resource on every call result so hosts
		// can associate this result with the chart iframe even if they missed the
		// resource URI in the tool definition (belt-and-suspenders).
		Meta:    mcp.Meta{"ui": map[string]any{"resourceUri": chartStaticURI}},
		Content: []mcp.Content{&mcp.TextContent{Text: string(analysisJSON)}},
	}
}

func RegisterMetricsSnapshot(s *mcp.Server, querier gcpdata.MetricsQuerier, registry *metrics.Registry, defaultProject string, mode RegistrationMode) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "metrics_snapshot",
		Description: applyMode(mode, "Get a semantic snapshot of a metric with baseline comparison, trend detection, and classification. "+
			"Returns current value, baseline delta, trend, SLO breach status, and a classification label. "+
			"Also renders an interactive time-series chart inline in the chat (hosts that support MCP app widgets). "+
			"The response includes `available_labels` — the metric.labels.* and resource.labels.* keys this metric accepts — "+
			"so follow-up calls can construct valid filters without guessing. "+
			"Use metrics_list first to discover metric_type values. "+
			"After getting a snapshot, use metrics_top_contributors to drill down by dimension, "+
			"or metrics_related to check correlated signals. "+
			"For comparing two specific time windows (e.g. before/after deploy), use metrics_compare instead."),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		// Static UI resource URI — signals chart support to the host for prefetch.
		// Per-call data is delivered via structuredContent through the MCP Apps bridge.
		Meta: mcp.Meta{"ui": map[string]any{"resourceUri": chartStaticURI}},
		InputSchema:  inputSchemaWithEnums[MetricsSnapshotInput](
			enumPatch{"window", enumWindow},
			enumPatch{"baseline_mode", enumBaselineMode},
		),
		OutputSchema: outputSchemaFor[MetricSnapshotResult](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in MetricsSnapshotInput) (*mcp.CallToolResult, *MetricSnapshotResult, error) {
		if in.MetricType == "" {
			return errResult("metric_type is required"), nil, nil
		}
		project, err := resolveProject(in.ProjectID, defaultProject)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		windowStr := in.Window
		if windowStr == "" {
			windowStr = "1h"
		}
		baselineMode := BaselineMode(in.BaselineMode)
		if baselineMode == "" {
			baselineMode = BaselineModePrevWindow
		}
		stepSeconds := int64(in.StepSeconds)
		if stepSeconds == 0 {
			stepSeconds = 60
		}
		if stepSeconds < 10 {
			return errResult("step_seconds must be at least 10"), nil, nil
		}

		windowDur, err := parseWindow(windowStr)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		if err := baselineMode.Validate(in.EventTime); err != nil {
			return errResult(err.Error()), nil, nil
		}

		meta := registry.Lookup(in.MetricType)
		now := time.Now().UTC()
		start := now.Add(-windowDur)

		sendProgress(ctx, req, 1, 4, "Looking up metric descriptor")

		descriptor, err := querier.GetMetricDescriptor(ctx, project, in.MetricType)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "metrics_snapshot", fmt.Sprintf("metric descriptor lookup failed: %v", err))
			return errResult(fmt.Sprintf("Failed to look up metric descriptor: %v. Verify the metric_type.", err)), nil, nil
		}

		sendProgress(ctx, req, 2, 4, "Querying current window")

		aggSpec := meta.ResolveAggregation()
		if err := aggSpec.Validate(); err != nil {
			mcpLog(ctx, req, logLevelError, "metrics_snapshot",
				fmt.Sprintf("registry misconfiguration for %s: %v", in.MetricType, err))
			return errResult(formatRegistryMisconfigError(in.MetricType, err)), nil, nil
		}

		// Query current window.
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
		currentSeries, currentWarnings, err := querier.QueryTimeSeriesAggregated(ctx, currentParams, aggSpec)
		logAggregationWarnings(ctx, req, "metrics_snapshot", in.MetricType, "current", currentWarnings)
		currentWarningsNote := aggregationWarningsNote(in.MetricType, "current", currentWarnings)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "metrics_snapshot", fmt.Sprintf("current window query failed: %v", err))
			if invalidAggregationSpecError(err) {
				return errResult(formatRegistryMisconfigError(in.MetricType, err)), nil, nil
			}
			if isInvalidFilterError(err) {
				return errResult(enrichInvalidFilterError(ctx, req, querier, project, in.MetricType, in.Filter, err)), nil, nil
			}
			return errResult(fmt.Sprintf("Failed to query metric: %v", err)), nil, nil
		}
		unsupportedCount := reportUnsupportedPoints(ctx, req, "metrics_snapshot", in.MetricType, currentSeries)

		currentPoints := mergePoints(currentSeries)
		if len(currentPoints) == 0 {
			expected := expectedPointsForWindow(windowDur, int(stepSeconds))
			r := &MetricSnapshotResult{
				MetricType:               in.MetricType,
				Kind:                     string(meta.Kind),
				Unit:                     meta.Unit,
				AutoDetected:             meta.AutoDetected,
				NoData:                   true,
				Note:                     joinNote(emptyWindowMessage(in.MetricType, windowStr, descriptor.Kind, in.Filter), currentWarningsNote),
				BaselineMode:             string(baselineMode),
				Trend:                    "unchanged",
				Classification:           string(metrics.ClassInsufficientData),
				ClassificationConfidence: "low",
				DataQuality: metrics.DataQuality{
					ExpectedPoints: expected,
					ActualPoints:   0,
					Reliable:       false,
				},
				Window: WindowInfo{
					From: start.Format(time.RFC3339),
					To:   now.Format(time.RFC3339),
				},
				AvailableLabels: availableLabelsFromDescriptor(ctx, req, querier, project, in.MetricType, descriptor),
			}
			return snapshotCallResult(r), r, nil
		}

		sendProgress(ctx, req, 3, 4, "Querying baseline ("+string(baselineMode)+")")

		var baselineErrNote string
		baseline, baselinePartialNote, err := buildBaselineStats(ctx, req, querier, currentParams, aggSpec, windowDur, baselineMode, in.EventTime, int(stepSeconds))
		if err != nil {
			mcpLog(ctx, req, logLevelError, "metrics_snapshot", fmt.Sprintf("baseline query failed: %v", err))
			baseline = metrics.BaselineStats{}
			// Classify error type to help user understand whether to retry or fix configuration
			if invalidAggregationSpecError(err) {
				baselineErrNote = fmt.Sprintf("Baseline skipped: registry misconfiguration for metric %q. Fix the aggregation block in the metrics_registry.yaml file; retrying will not help. %v",
					in.MetricType, err)
			} else {
				baselineErrNote = fmt.Sprintf("Baseline query (%s) temporarily failed: %v. You can retry. Returning current-window snapshot with baseline_reliable=false; delta fields are not meaningful.",
					string(baselineMode), err)
			}
		}

		sendProgress(ctx, req, 4, 4, "Processing results")

		// Process.
		f := metrics.ProcessWithBaselineStats(currentPoints, baseline, meta, int(stepSeconds))

		// Build output.
		result := &MetricSnapshotResult{
			MetricType:                 in.MetricType,
			Kind:                       string(meta.Kind),
			Unit:                       meta.Unit,
			AutoDetected:               meta.AutoDetected,
			Current:                    f.Current,
			Baseline:                   f.Baseline,
			DeltaPct:                   f.DeltaPct,
			BaselineMode:               string(baselineMode),
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
		}

		if f.StepChangeAt != nil {
			result.StepChangeAt = f.StepChangeAt.Format(time.RFC3339)
		}

		appendNote := func(msg string) {
			if msg == "" {
				return
			}
			if result.Note == "" {
				result.Note = msg
			} else {
				result.Note += " " + msg
			}
		}

		if baselineErrNote != "" {
			// Zero out baseline-derived fields: they are computed relative to a
			// zero baseline and would appear as a >100% regression even though
			// no real data is available for comparison.
			result.Baseline = 0
			result.DeltaPct = 0
			appendNote(baselineErrNote)
		}
		appendNote(currentWarningsNote)
		appendNote(baselinePartialNote)
		if unsupportedCount > 0 {
			appendNote(fmt.Sprintf("Dropped %d points with unsupported or malformed value types during decode (see server log).", unsupportedCount))
		}

		if meta.Kind == metrics.KindLatency {
			result.Percentiles = &PercentileInfo{
				P50:       f.P50,
				P95:       f.P95,
				P99:       f.P99,
				TailRatio: f.TailRatio,
			}
		}

		result.AvailableLabels = availableLabelsFromDescriptor(ctx, req, querier, project, in.MetricType, descriptor)

		result.ChartPoints = toChartPoints(currentPoints)
		return snapshotCallResult(result), result, nil
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
	// Included in structuredContent; snapshotCallResult excludes it from the
	// LLM-facing content field by marshaling a shallow copy with ChartPoints=nil.
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

// buildBaselineStats queries baseline data in the requested mode and returns
// precomputed BaselineStats ready for metrics.ProcessWithBaselineStats.
// note is non-empty when the baseline succeeded with caveats (for example
// weekly partial failures or aggregation warnings); the caller should surface
// it in the tool result Note field.
func buildBaselineStats(
	ctx context.Context,
	req *mcp.CallToolRequest,
	querier gcpdata.MetricsQuerier,
	params gcpdata.QueryTimeSeriesParams,
	aggSpec metrics.AggregationSpec,
	windowDur time.Duration,
	mode BaselineMode,
	eventTimeStr string,
	stepSeconds int,
) (metrics.BaselineStats, string, error) {
	expectedPerWindow := expectedPointsForWindow(windowDur, stepSeconds)

	switch mode {
	case BaselineModeSameWeekdayHour:
		return buildRobustWeeklyBaseline(ctx, req, querier, params, aggSpec, expectedPerWindow)

	case BaselineModePreEvent:
		eventTime, err := time.Parse(time.RFC3339, eventTimeStr)
		if err != nil {
			return metrics.BaselineStats{}, "", fmt.Errorf("invalid event_time: %w", err)
		}
		p := params
		p.End = eventTime
		p.Start = eventTime.Add(-30 * time.Minute)
		series, warnings, err := querier.QueryTimeSeriesAggregated(ctx, p, aggSpec)
		logAggregationWarnings(ctx, req, "metrics_snapshot", params.MetricType, "baseline (pre_event)", warnings)
		if err != nil {
			return metrics.BaselineStats{}, "", fmt.Errorf("querying pre_event baseline: %w", err)
		}
		preEventExpected := expectedPointsForWindow(30*time.Minute, stepSeconds)
		return metrics.ComputeBaselineStats(mergePoints(series), preEventExpected),
			aggregationWarningsNote(params.MetricType, "baseline (pre_event)", warnings), nil

	default: // prev_window
		p := params
		p.End = params.Start
		p.Start = params.Start.Add(-windowDur)
		series, warnings, err := querier.QueryTimeSeriesAggregated(ctx, p, aggSpec)
		logAggregationWarnings(ctx, req, "metrics_snapshot", params.MetricType, "baseline (prev_window)", warnings)
		if err != nil {
			return metrics.BaselineStats{}, "", fmt.Errorf("querying prev_window baseline: %w", err)
		}
		return metrics.ComputeBaselineStats(mergePoints(series), expectedPerWindow),
			aggregationWarningsNote(params.MetricType, "baseline (prev_window)", warnings), nil
	}
}

// buildRobustWeeklyBaseline queries the same wall-clock window for each of
// the last 4 weeks in parallel and combines them via median/MAD.
// partialNote is non-empty when some weeks failed but enough data remains.
func buildRobustWeeklyBaseline(
	ctx context.Context,
	req *mcp.CallToolRequest,
	querier gcpdata.MetricsQuerier,
	params gcpdata.QueryTimeSeriesParams,
	aggSpec metrics.AggregationSpec,
	expectedPerWeek int,
) (metrics.BaselineStats, string, error) {
	const weeks = 4
	weekly := make([][]metrics.Point, weeks)
	warningNotes := make([]string, 0, weeks)
	warningNotesSeen := make(map[string]bool, weeks)
	var mu sync.Mutex
	var wg sync.WaitGroup
	var errs []error

	for w := 1; w <= weeks; w++ {
		wg.Add(1)
		go func(weeksBack int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					stack := debug.Stack()
					notifyErrLog.Load().Error("metrics_snapshot: panic in baseline goroutine", "weeks_back", weeksBack, "panic", r, "stack", string(stack))
					mu.Lock()
					errs = append(errs, fmt.Errorf("week -%d: panic: %v", weeksBack, r))
					mu.Unlock()
				}
			}()
			if ctx.Err() != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("week -%d: %w", weeksBack, ctx.Err()))
				mu.Unlock()
				return
			}
			p := params
			p.Start = params.Start.AddDate(0, 0, -7*weeksBack)
			p.End = params.End.AddDate(0, 0, -7*weeksBack)
			series, warnings, err := querier.QueryTimeSeriesAggregated(ctx, p, aggSpec)
			logAggregationWarnings(ctx, req, "metrics_snapshot", params.MetricType,
				fmt.Sprintf("baseline (same_weekday_hour week -%d)", weeksBack), warnings)
			warningNote := aggregationWarningsNote(params.MetricType,
				fmt.Sprintf("baseline (same_weekday_hour week -%d)", weeksBack), warnings)
			mu.Lock()
			defer mu.Unlock()
			if warningNote != "" && !warningNotesSeen[warningNote] {
				warningNotesSeen[warningNote] = true
				warningNotes = append(warningNotes, warningNote)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("week -%d: %w", weeksBack, err))
				return
			}
			weekly[weeksBack-1] = mergePoints(series)
		}(w)
	}
	wg.Wait()

	nonEmpty := 0
	for _, w := range weekly {
		if len(w) > 0 {
			nonEmpty++
		}
	}
	if nonEmpty == 0 && len(errs) > 0 {
		return metrics.BaselineStats{}, "", fmt.Errorf("all %d baseline queries failed; first error: %w", len(errs), errors.Join(errs...))
	}
	var partialNote string
	if len(errs) > 0 && nonEmpty > 0 {
		// Check if any errors were panics (which indicate code bugs, not transient failures)
		hasPanic := false
		for _, e := range errs {
			if strings.Contains(e.Error(), "panic") {
				hasPanic = true
				break
			}
		}
		if hasPanic {
			mcpLog(ctx, req, logLevelError, "metrics_snapshot",
				fmt.Sprintf("baseline partial failure: UNEXPECTED PANICS in %d of %d weeks; %v",
					len(errs), weeks, errors.Join(errs...)))
			partialNote = fmt.Sprintf("Baseline partial failure (%s): UNEXPECTED PANICS occurred in %d of %d weekly queries. This is a bug in the code, not a transient failure. Baseline computed from %d weeks, but results may be unreliable. Please report this issue.",
				string(BaselineModeSameWeekdayHour), len(errs), weeks, nonEmpty)
		} else {
			mcpLog(ctx, req, logLevelWarning, "metrics_snapshot",
				fmt.Sprintf("baseline partial failure: %d of %d weeks failed (%v); using %d weeks of data",
					len(errs), weeks, errors.Join(errs...), nonEmpty))
			partialNote = fmt.Sprintf("Baseline partial failure (%s): %d of %d weekly samples could not be fetched; baseline computed from %d weeks. Results may be less reliable.",
				string(BaselineModeSameWeekdayHour), len(errs), weeks, nonEmpty)
		}
	}

	return metrics.ComputeRobustBaselineStats(weekly, expectedPerWeek),
		joinNote(partialNote, joinNote(warningNotes...)), nil
}

// expectedPointsForWindow is the ideal point count for a window of the given
// duration at the given step size.
func expectedPointsForWindow(windowDur time.Duration, stepSeconds int) int {
	if stepSeconds <= 0 {
		stepSeconds = 60
	}
	step := time.Duration(stepSeconds) * time.Second
	if step <= 0 {
		return 0
	}
	return int(windowDur/step) + 1
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

func emptyWindowMessage(metricType, window, kind, labelFilter string) string {
	base := fmt.Sprintf("Metric %q has no data points in the last %s. The metric is registered in Cloud Monitoring but the window is empty.",
		metricType, window)

	switch kind {
	case "DELTA", "CUMULATIVE":
		base += " For DELTA/CUMULATIVE counters this almost always means no events occurred during the window — for example, dead_letter_message_count has no data when no messages were forwarded to a DLQ."
	case "GAUGE":
		base += " For GAUGE metrics this usually means no matching resources are reporting values — check that resources exist and the metric is being collected."
	}
	if labelFilter != "" {
		base += fmt.Sprintf(" The label filter %q may also be excluding every series — try removing it.", labelFilter)
	} else {
		base += " Try widening the window or removing any dimension/filter to confirm."
	}
	return base
}

func parseWindow(s string) (time.Duration, error) {
	switch s {
	case "15m":
		return 15 * time.Minute, nil
	case "30m":
		return 30 * time.Minute, nil
	case "1h":
		return time.Hour, nil
	case "3h":
		return 3 * time.Hour, nil
	case "6h":
		return 6 * time.Hour, nil
	case "24h":
		return 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("invalid window %q: must be one of 15m, 30m, 1h, 3h, 6h, 24h", s)
	}
}
