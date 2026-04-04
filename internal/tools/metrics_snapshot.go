package tools

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// BaselineMode defines how baseline data is selected for comparison.
type BaselineMode string

const (
	BaselineModePrevWindow      BaselineMode = "prev_window"
	BaselineModeSameWeekdayHour BaselineMode = "same_weekday_hour"
	BaselineModePreEvent        BaselineMode = "pre_event"
)

// MetricsSnapshotHandler handles the metrics.snapshot tool.
type MetricsSnapshotHandler struct {
	querier        gcpdata.MetricsQuerier
	registry       *metrics.Registry
	defaultProject string
}

// NewMetricsSnapshotHandler creates a new MetricsSnapshotHandler.
func NewMetricsSnapshotHandler(querier gcpdata.MetricsQuerier, registry *metrics.Registry, defaultProject string) *MetricsSnapshotHandler {
	return &MetricsSnapshotHandler{querier: querier, registry: registry, defaultProject: defaultProject}
}

// Tool returns the MCP tool definition.
func (h *MetricsSnapshotHandler) Tool() mcp.Tool {
	return mcp.NewTool("metrics.snapshot",
		mcp.WithDescription("Get a semantic snapshot of a metric with baseline comparison, trend detection, and classification. "+
			"Returns current value, baseline delta, trend, SLO breach status, and a classification label. "+
			"Use metrics.list first to discover metric_type values. "+
			"After getting a snapshot, use metrics.top_contributors to drill down by dimension, "+
			"or metrics.related to check correlated signals. "+
			"For comparing two specific time windows (e.g. before/after deploy), use metrics.compare instead."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithString("project_id",
			mcp.Description("GCP project ID (uses default if not specified)"),
		),
		mcp.WithString("metric_type",
			mcp.Description("Full Cloud Monitoring metric type (e.g. 'compute.googleapis.com/instance/cpu/utilization')"),
			mcp.Required(),
		),
		mcp.WithString("filter",
			mcp.Description("Additional Cloud Monitoring label filter (e.g. 'resource.labels.instance_id = \"123\"')"),
		),
		mcp.WithString("window",
			mcp.Description("Time window to analyze (default '1h')"),
			mcp.Enum("15m", "30m", "1h", "3h", "6h", "24h"),
		),
		mcp.WithString("baseline_mode",
			mcp.Description("Baseline comparison mode (default 'prev_window')"),
			mcp.Enum("prev_window", "same_weekday_hour", "pre_event"),
		),
		mcp.WithString("event_time",
			mcp.Description("Event time in RFC3339 format, required when baseline_mode is 'pre_event'"),
		),
		mcp.WithNumber("step_seconds",
			mcp.Description("Alignment period in seconds (default 60)"),
			mcp.Min(10),
		),
	)
}

// MetricSnapshotResult is the output for metrics.snapshot.
type MetricSnapshotResult struct {
	MetricType   string `json:"metric_type"`
	Kind         string `json:"kind"`
	Unit         string `json:"unit"`
	AutoDetected bool   `json:"auto_detected,omitempty"`

	Current          float64 `json:"current"`
	Baseline         float64 `json:"baseline"`
	DeltaPct         float64 `json:"delta_pct"`
	BaselineMode     string  `json:"baseline_mode"`
	BaselineReliable bool    `json:"baseline_reliable"`

	Trend          string `json:"trend"`
	Classification string `json:"classification"`

	SLOBreach                 bool     `json:"slo_breach"`
	SLOThreshold              *float64 `json:"slo_threshold,omitempty"`
	BreachDurationSeconds     int      `json:"breach_duration_seconds,omitempty"`
	CurrentBreachStreakSeconds int      `json:"current_breach_streak_seconds,omitempty"`

	StepChangeDetected bool   `json:"step_change_detected"`
	StepChangeAt       string `json:"step_change_at,omitempty"`

	Percentiles *PercentileInfo      `json:"percentiles,omitempty"`
	DataQuality metrics.DataQuality  `json:"data_quality"`
	Window      WindowInfo           `json:"window"`
}

// PercentileInfo holds percentile values.
type PercentileInfo struct {
	P50       float64 `json:"p50"`
	P95       float64 `json:"p95"`
	P99       float64 `json:"p99"`
	TailRatio float64 `json:"tail_ratio,omitempty"`
}

// WindowInfo describes a time window.
type WindowInfo struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Handle processes the metrics.snapshot tool request.
func (h *MetricsSnapshotHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	metricType, err := request.RequireString("metric_type")
	if err != nil {
		return mcp.NewToolResultError("metric_type is required"), nil
	}

	project, errResult := requireProject(request, h.defaultProject)
	if errResult != nil {
		return errResult, nil
	}

	labelFilter := request.GetString("filter", "")
	windowStr := request.GetString("window", "1h")
	baselineMode := BaselineMode(request.GetString("baseline_mode", string(BaselineModePrevWindow)))
	eventTimeStr := request.GetString("event_time", "")
	stepSeconds := int64(request.GetInt("step_seconds", 60))

	windowDur, err := parseWindow(windowStr)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if baselineMode == BaselineModePreEvent && eventTimeStr == "" {
		return mcp.NewToolResultError("event_time is required when baseline_mode is 'pre_event'"), nil
	}

	meta := h.registry.Lookup(metricType)
	now := time.Now().UTC()
	start := now.Add(-windowDur)

	sendProgress(ctx, request, 1, 4, "Looking up metric descriptor")

	// Look up GCP metric kind to select the correct aligner for both current and baseline queries.
	gcpMetricKind, err := h.querier.GetMetricKind(ctx, project, metricType)
	if err != nil {
		mcpLog(ctx, mcp.LoggingLevelError, "metrics.snapshot", fmt.Sprintf("metric descriptor lookup failed: %v", err))
		return mcp.NewToolResultError(fmt.Sprintf("Failed to look up metric descriptor: %v. Verify the metric_type.", err)), nil
	}

	sendProgress(ctx, request, 2, 4, "Querying current window")

	// Query current window.
	currentParams := gcpdata.QueryTimeSeriesParams{
		Project:     project,
		MetricType:  metricType,
		LabelFilter: labelFilter,
		Start:       start,
		End:         now,
		StepSeconds: stepSeconds,
		MetricKind:  gcpMetricKind,
		Reducer:     monitoringpb.Aggregation_REDUCE_MEAN,
	}
	currentSeries, err := h.querier.QueryTimeSeries(ctx, currentParams)
	if err != nil {
		mcpLog(ctx, mcp.LoggingLevelError, "metrics.snapshot", fmt.Sprintf("current window query failed: %v", err))
		return mcp.NewToolResultError(fmt.Sprintf("Failed to query metric: %v", err)), nil
	}

	currentPoints := mergePoints(currentSeries)
	if len(currentPoints) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("No data found for metric %q in the last %s. Verify the metric_type and filter.", metricType, windowStr)), nil
	}

	sendProgress(ctx, request, 3, 4, "Querying baseline ("+string(baselineMode)+")")

	// Query baseline.
	baselineSeries, err := queryBaselineSeries(ctx, h.querier, currentParams, windowDur, baselineMode, eventTimeStr)
	if err != nil {
		mcpLog(ctx, mcp.LoggingLevelWarning, "metrics.snapshot", fmt.Sprintf("baseline query failed: %v", err))
		return mcp.NewToolResultError(fmt.Sprintf("Failed to query baseline (%s): %v", string(baselineMode), err)), nil
	}
	baselinePoints := mergePoints(baselineSeries)

	sendProgress(ctx, request, 4, 4, "Processing results")

	// Process.
	f := metrics.Process(currentPoints, baselinePoints, meta, int(stepSeconds))

	// Build output.
	result := MetricSnapshotResult{
		MetricType:                metricType,
		Kind:                      string(meta.Kind),
		Unit:                      meta.Unit,
		AutoDetected:              meta.AutoDetected,
		Current:                   f.Current,
		Baseline:                  f.Baseline,
		DeltaPct:                  f.DeltaPct,
		BaselineMode:              string(baselineMode),
		BaselineReliable:          f.BaselineReliable,
		Trend:                     f.Trend,
		Classification:            string(f.Classification),
		SLOBreach:                 f.SLOBreach,
		SLOThreshold:              meta.SLOThreshold,
		BreachDurationSeconds:     f.BreachDurationSeconds,
		CurrentBreachStreakSeconds: f.CurrentBreachStreakSeconds,
		StepChangeDetected:        f.StepChangeDetected,
		DataQuality:               f.DataQuality,
		Window: WindowInfo{
			From: start.Format(time.RFC3339),
			To:   now.Format(time.RFC3339),
		},
	}

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

	return jsonResult(result)
}

// queryBaselineSeries queries baseline time series, handling all baseline modes consistently.
// For same_weekday_hour, it queries 4 weeks back in parallel.
func queryBaselineSeries(ctx context.Context, querier gcpdata.MetricsQuerier, params gcpdata.QueryTimeSeriesParams, windowDur time.Duration, mode BaselineMode, eventTimeStr string) ([]gcpdata.MetricTimeSeries, error) {
	switch mode {
	case BaselineModeSameWeekdayHour:
		var allSeries []gcpdata.MetricTimeSeries
		var mu sync.Mutex
		var wg sync.WaitGroup
		var errs []error

		for w := 1; w <= 4; w++ {
			wg.Add(1)
			go func(weeksBack int) {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						mcpLog(ctx, mcp.LoggingLevelError, "metrics.snapshot", fmt.Sprintf("panic in baseline week -%d: %v", weeksBack, r))
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
				series, err := querier.QueryTimeSeries(ctx, p)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs = append(errs, fmt.Errorf("week -%d: %w", weeksBack, err))
					return
				}
				allSeries = append(allSeries, series...)
			}(w)
		}
		wg.Wait()
		// Partial success is acceptable for baselines — use whatever data we got.
		if len(allSeries) == 0 && len(errs) > 0 {
			return nil, fmt.Errorf("all %d baseline queries failed; first error: %w", len(errs), errors.Join(errs...))
		}
		if len(errs) > 0 && len(allSeries) > 0 {
			mcpLog(ctx, mcp.LoggingLevelWarning, "metrics.snapshot",
				fmt.Sprintf("baseline partial failure: %d of 4 weeks failed (%v); using %d weeks of data",
					len(errs), errors.Join(errs...), 4-len(errs)))
		}
		return allSeries, nil

	case BaselineModePreEvent:
		eventTime, err := time.Parse(time.RFC3339, eventTimeStr)
		if err != nil {
			return nil, fmt.Errorf("invalid event_time: %w", err)
		}
		p := params
		p.End = eventTime
		p.Start = eventTime.Add(-30 * time.Minute)
		return querier.QueryTimeSeries(ctx, p)

	default: // prev_window
		p := params
		p.End = params.Start
		p.Start = params.Start.Add(-windowDur)
		return querier.QueryTimeSeries(ctx, p)
	}
}

// mergePoints concatenates points from all series and sorts by timestamp.
// No deduplication: for same_weekday_hour baselines, duplicate timestamps from
// different weeks are intentional (more data = better baseline statistics).
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
