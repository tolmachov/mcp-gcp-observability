package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
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

// baselineModes lists the supported baseline modes; the first is the default.
var baselineModes = []BaselineMode{BaselineModePrevWindow, BaselineModeSameWeekdayHour, BaselineModePreEvent}

// parseBaselineMode resolves the baseline_mode input ("" selects the
// default) and, for pre_event, parses the required event_time once so a bad
// value is reported as an input error. eventTime is zero for other modes.
func parseBaselineMode(raw, eventTimeStr string) (BaselineMode, time.Time, error) {
	mode := BaselineMode(raw)
	if mode == "" {
		mode = baselineModes[0]
	}
	if !slices.Contains(baselineModes, mode) {
		return "", time.Time{}, fmt.Errorf("invalid baseline_mode %q: must be one of %v", raw, baselineModes)
	}
	if mode != BaselineModePreEvent {
		return mode, time.Time{}, nil
	}
	if eventTimeStr == "" {
		return "", time.Time{}, errors.New("event_time is required when baseline_mode is 'pre_event'")
	}
	eventTime, err := parseRFC3339Opt(eventTimeStr, "event_time")
	if err != nil {
		return "", time.Time{}, err
	}
	return mode, eventTime, nil
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
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		// Static UI resource URI — signals chart support to the host for prefetch.
		// Per-call data is delivered via structuredContent through the MCP Apps bridge.
		Meta: mcp.Meta{"ui": map[string]any{"resourceUri": chartStaticURI}},
		InputSchema: projectInputSchema[MetricsSnapshotInput](d.Project,
			nonEmptyProp("metric_type"),
			enumProp("window", metricWindowNames()),
			enumProp("baseline_mode", baselineModes),
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

		baselineMode, eventTime, err := parseBaselineMode(in.BaselineMode, in.EventTime)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		meta := d.Registry.Lookup(in.MetricType)
		now := time.Now().UTC()
		start := now.Add(-windowDur)

		sendProgress(ctx, req, 1, 4, "Looking up metric descriptor")

		descriptor, errRes := lookupMetricDescriptor(ctx, req, d.Querier, "metrics_snapshot", project, in.MetricType)
		if errRes != nil {
			return errRes, nil, nil
		}

		sendProgress(ctx, req, 2, 4, "Querying current window")

		aggSpec, errRes := resolveValidAggSpec(ctx, req, "metrics_snapshot", in.MetricType, meta)
		if errRes != nil {
			return errRes, nil, nil
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
		currentSeries, currentWarnings, err := d.Querier.QueryTimeSeriesAggregated(ctx, currentParams, aggSpec)
		logAggregationWarnings(ctx, req, "metrics_snapshot", in.MetricType, "current", currentWarnings)
		currentWarningsNote := aggregationWarningsNote(in.MetricType, "current", currentWarnings)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "metrics_snapshot", fmt.Sprintf("current window query failed: %v", err))
			if invalidAggregationSpecError(err) {
				return errResult(formatRegistryMisconfigError(in.MetricType, err)), nil, nil
			}
			if isInvalidFilterError(err) {
				return errResult(enrichInvalidFilterError(ctx, req, d.Querier, project, in.MetricType, in.Filter, err)), nil, nil
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
					ExpectedPoints:  expected,
					ActualPoints:    0,
					NonFinitePoints: currentWarnings.NonFinitePoints,
					Reliable:        false,
				},
				Window: WindowInfo{
					From: start.Format(time.RFC3339),
					To:   now.Format(time.RFC3339),
				},
				AvailableLabels: availableLabelsFromDescriptor(ctx, req, d.Querier, project, in.MetricType, descriptor),
			}
			return snapshotCallResult(r), r, nil
		}

		sendProgress(ctx, req, 3, 4, "Querying baseline ("+string(baselineMode)+")")

		var baselineErrNote string
		baseline, baselinePartialNote, err := buildBaselineStats(ctx, req, d.Querier, currentParams, aggSpec, windowDur, baselineMode, eventTime, int(stepSeconds))
		if err != nil {
			mcpLog(ctx, req, logLevelError, "metrics_snapshot", fmt.Sprintf("baseline query failed: %v", err))
			baseline = metrics.BaselineStats{}
			// Classify error type to help user understand whether to retry or fix configuration
			if invalidAggregationSpecError(err) {
				baselineErrNote = fmt.Sprintf("Baseline skipped: registry misconfiguration for metric %q. Fix the aggregation block in the metrics registry YAML file; retrying will not help. %v",
					in.MetricType, err)
			} else {
				baselineErrNote = fmt.Sprintf("Baseline query (%s) temporarily failed: %v. You can retry. Returning current-window snapshot with baseline_reliable=false; delta fields are not meaningful.",
					string(baselineMode), err)
			}
		}

		sendProgress(ctx, req, 4, 4, "Processing results")

		// Process.
		f := metrics.ProcessWithBaselineStats(currentPoints, baseline, meta, int(stepSeconds), metrics.Window{Start: start, End: now})

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
		result.DataQuality.NonFinitePoints = currentWarnings.NonFinitePoints

		if f.StepChangeAt != nil {
			result.StepChangeAt = f.StepChangeAt.Format(time.RFC3339)
		}

		var unsupportedNote string
		if unsupportedCount > 0 {
			unsupportedNote = fmt.Sprintf("Dropped %d points with unsupported or malformed value types during decode (see server log).", unsupportedCount)
		}
		result.Note = joinNote(baselineErrNote, currentWarningsNote, baselinePartialNote, unsupportedNote)

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
	eventTime time.Time,
	stepSeconds int,
) (metrics.BaselineStats, string, error) {
	expectedPerWindow := expectedPointsForWindow(windowDur, stepSeconds)

	switch mode {
	case BaselineModeSameWeekdayHour:
		return buildRobustWeeklyBaseline(ctx, req, querier, params, aggSpec, expectedPerWindow)

	case BaselineModePreEvent:
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
	weekly := make([][]metrics.Point, weeklyBaselineWeeks)
	warningNotes := make([]string, 0, weeklyBaselineWeeks)
	warningNotesSeen := make(map[string]bool, weeklyBaselineWeeks)
	var noteMu sync.Mutex

	errs := runWeeklyBaseline(ctx, "metrics_snapshot", func(weeksBack int) error {
		p := params
		p.Start = params.Start.AddDate(0, 0, -7*weeksBack)
		p.End = params.End.AddDate(0, 0, -7*weeksBack)
		series, warnings, err := querier.QueryTimeSeriesAggregated(ctx, p, aggSpec)
		label := fmt.Sprintf("baseline (same_weekday_hour week -%d)", weeksBack)
		logAggregationWarnings(ctx, req, "metrics_snapshot", params.MetricType, label, warnings)
		if note := aggregationWarningsNote(params.MetricType, label, warnings); note != "" {
			noteMu.Lock()
			if !warningNotesSeen[note] {
				warningNotesSeen[note] = true
				warningNotes = append(warningNotes, note)
			}
			noteMu.Unlock()
		}
		if err != nil {
			return err
		}
		weekly[weeksBack-1] = mergePoints(series)
		return nil
	})

	nonEmpty := 0
	for _, w := range weekly {
		if len(w) > 0 {
			nonEmpty++
		}
	}
	if nonEmpty == 0 && len(errs) > 0 {
		return metrics.BaselineStats{}, "", fmt.Errorf("all %d baseline queries failed; first error: %w", len(errs), errors.Join(errs...))
	}
	partialNote := weeklyBaselinePartialNote(ctx, req, "metrics_snapshot", errs, nonEmpty)

	return metrics.ComputeRobustBaselineStats(weekly, expectedPerWeek),
		joinNote(partialNote, joinNote(warningNotes...)), nil
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

// parseWindow resolves the window input ("" selects defaultMetricWindow) to
// its name and duration.
func parseWindow(s string) (string, time.Duration, error) {
	if s == "" {
		s = defaultMetricWindow
	}
	for _, w := range metricWindows {
		if w.name == s {
			return w.name, w.dur, nil
		}
	}
	return "", 0, fmt.Errorf("invalid window %q: must be one of %v", s, metricWindowNames())
}
