package tools

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// MetricsTopHandler handles the metrics.top_contributors tool.
type MetricsTopHandler struct {
	querier        gcpdata.MetricsQuerier
	registry       *metrics.Registry
	defaultProject string
}

// NewMetricsTopHandler creates a new MetricsTopHandler.
func NewMetricsTopHandler(querier gcpdata.MetricsQuerier, registry *metrics.Registry, defaultProject string) *MetricsTopHandler {
	return &MetricsTopHandler{querier: querier, registry: registry, defaultProject: defaultProject}
}

// Tool returns the MCP tool definition.
func (h *MetricsTopHandler) Tool() mcp.Tool {
	return mcp.NewTool("metrics.top_contributors",
		mcp.WithDescription("Break down a metric by a label dimension to find which label values contribute most to an anomaly. "+
			"Shows each contributor's delta from baseline and share of the total anomaly. "+
			"Use this after metrics.snapshot shows a regression — it answers 'which route/instance/status_code is responsible?' "+
			"For comparing time windows (e.g. before/after deploy), use metrics.compare instead."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithString("project_id",
			mcp.Description("GCP project ID (uses default if not specified)"),
		),
		mcp.WithString("metric_type",
			mcp.Description("Full Cloud Monitoring metric type"),
			mcp.Required(),
		),
		mcp.WithString("dimension",
			mcp.Description("Label key to group by (e.g. 'metric.labels.response_code', 'resource.labels.instance_id')"),
			mcp.Required(),
		),
		mcp.WithString("filter",
			mcp.Description("Additional Cloud Monitoring label filter"),
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
			mcp.Description("Event time in RFC3339 for pre_event baseline"),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of contributors to return (default 5, max 20)"),
			mcp.Min(1),
		),
	)
}

// TopContributorsResult is the output for metrics.top_contributors.
type TopContributorsResult struct {
	Dimension    string        `json:"dimension"`
	Contributors []Contributor `json:"contributors"`
}

// Contributor describes a single label value's contribution to the anomaly.
type Contributor struct {
	LabelValue      string  `json:"label_value"`
	Current         float64 `json:"current"`
	Baseline        float64 `json:"baseline"`
	DeltaPct        float64 `json:"delta_pct"`
	ShareOfAnomaly  float64 `json:"share_of_anomaly"`
	SLOBreach       bool    `json:"slo_breach"`
	Classification  string  `json:"classification"`
}

// Handle processes the metrics.top_contributors tool request.
func (h *MetricsTopHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	metricType, err := request.RequireString("metric_type")
	if err != nil {
		return mcp.NewToolResultError("metric_type is required"), nil
	}
	dimension, err := request.RequireString("dimension")
	if err != nil {
		return mcp.NewToolResultError("dimension is required"), nil
	}

	project, errResult := requireProject(request, h.defaultProject)
	if errResult != nil {
		return errResult, nil
	}

	labelFilter := request.GetString("filter", "")
	windowStr := request.GetString("window", "1h")
	baselineMode := BaselineMode(request.GetString("baseline_mode", string(BaselineModePrevWindow)))
	eventTimeStr := request.GetString("event_time", "")
	limit := clampLimit(request.GetInt("limit", 5), 5, 20)
	stepSeconds := int64(60)

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

	// Look up GCP metric kind to select the correct aligner.
	gcpMetricKind, err := h.querier.GetMetricKind(ctx, project, metricType)
	if err != nil {
		mcpLog(ctx, mcp.LoggingLevelError, "metrics.top_contributors", fmt.Sprintf("metric descriptor lookup failed: %v", err))
		return mcp.NewToolResultError(fmt.Sprintf("Failed to look up metric descriptor: %v. Verify the metric_type.", err)), nil
	}

	reducer := monitoringpb.Aggregation_REDUCE_MEAN
	if meta.Kind == metrics.KindThroughput || meta.Kind == metrics.KindErrorRate {
		reducer = monitoringpb.Aggregation_REDUCE_SUM
	}

	sendProgress(ctx, request, 2, 4, "Querying current window grouped by "+dimension)

	// Current window grouped by dimension.
	currentParams := gcpdata.QueryTimeSeriesParams{
		Project:       project,
		MetricType:    metricType,
		LabelFilter:   labelFilter,
		Start:         start,
		End:           now,
		StepSeconds:   stepSeconds,
		MetricKind:    gcpMetricKind,
		GroupByFields: []string{dimension},
		Reducer:       reducer,
	}
	currentSeries, err := h.querier.QueryTimeSeries(ctx, currentParams)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to query metric: %v", err)), nil
	}

	if len(currentSeries) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("No data found for metric %q grouped by %q in the last %s. Verify the metric_type and dimension with metrics.list.", metricType, dimension, windowStr)), nil
	}

	sendProgress(ctx, request, 3, 4, "Querying baseline ("+string(baselineMode)+")")

	// Query baseline series, handling same_weekday_hour with 4-week lookback.
	baselineSeries, err := queryBaselineSeries(ctx, h.querier, currentParams, windowDur, baselineMode, eventTimeStr)
	if err != nil {
		mcpLog(ctx, mcp.LoggingLevelWarning, "metrics.top_contributors", fmt.Sprintf("baseline query failed: %v", err))
		return mcp.NewToolResultError(fmt.Sprintf("Failed to query baseline (%s): %v", string(baselineMode), err)), nil
	}

	// Index baseline by label value.
	baselineByLabel := make(map[string][]metrics.Point)
	for _, s := range baselineSeries {
		lv := labelValueFromSeries(s, dimension)
		baselineByLabel[lv] = append(baselineByLabel[lv], s.Points...)
	}

	// Process each contributor.
	type contribData struct {
		label    string
		current  []metrics.Point
		baseline []metrics.Point
	}
	var contribs []contribData
	for _, s := range currentSeries {
		lv := labelValueFromSeries(s, dimension)
		contribs = append(contribs, contribData{
			label:    lv,
			current:  s.Points,
			baseline: baselineByLabel[lv],
		})
	}

	type processedContrib struct {
		contributor Contributor
		absDelta   float64
	}

	var processed []processedContrib
	var totalAbsDelta float64

	for _, c := range contribs {
		f := metrics.Process(c.current, c.baseline, meta, int(stepSeconds))
		ad := math.Abs(f.DeltaAbs)
		totalAbsDelta += ad
		processed = append(processed, processedContrib{
			contributor: Contributor{
				LabelValue:     c.label,
				Current:        f.Current,
				Baseline:       f.Baseline,
				DeltaPct:       f.DeltaPct,
				SLOBreach:      f.SLOBreach,
				Classification: string(f.Classification),
			},
			absDelta: ad,
		})
	}

	// Check for all-unknown dimension values — likely a wrong dimension key.
	unknownCount := 0
	for _, c := range contribs {
		if c.label == "(unknown)" {
			unknownCount++
		}
	}
	if unknownCount == len(contribs) && len(contribs) > 0 {
		return mcp.NewToolResultError(fmt.Sprintf(
			"Dimension %q was not found in any series labels. Verify the dimension key matches a label from metrics.list (e.g. 'metric.labels.response_code' or 'resource.labels.instance_id').",
			dimension,
		)), nil
	}

	// Compute share_of_anomaly and collect results.
	var results []Contributor
	for _, pc := range processed {
		c := pc.contributor
		if totalAbsDelta > 0 {
			c.ShareOfAnomaly = pc.absDelta / totalAbsDelta
		}
		results = append(results, c)
	}

	// Sort by abs delta descending.
	sort.Slice(results, func(i, j int) bool {
		return math.Abs(results[i].DeltaPct) > math.Abs(results[j].DeltaPct)
	})

	if len(results) > limit {
		results = results[:limit]
	}

	return jsonResult(TopContributorsResult{
		Dimension:    dimension,
		Contributors: results,
	})
}

func labelValueFromSeries(s gcpdata.MetricTimeSeries, dimension string) string {
	// Dimension can be "metric.labels.X" or "resource.labels.X".
	// Try metric labels first, then resource labels.
	parts := splitDimension(dimension)
	if parts.prefix == "metric" {
		if v, ok := s.MetricLabels[parts.key]; ok {
			return v
		}
	}
	if parts.prefix == "resource" {
		if v, ok := s.ResourceLabels[parts.key]; ok {
			return v
		}
	}
	// Fallback: try both.
	if v, ok := s.MetricLabels[parts.key]; ok {
		return v
	}
	if v, ok := s.ResourceLabels[parts.key]; ok {
		return v
	}
	return "(unknown)"
}

type dimensionParts struct {
	prefix string
	key    string
}

func splitDimension(dimension string) dimensionParts {
	// "metric.labels.response_code" → prefix="metric", key="response_code"
	// "resource.labels.instance_id" → prefix="resource", key="instance_id"
	// "response_code" → prefix="", key="response_code"
	if key, ok := strings.CutPrefix(dimension, "metric.labels."); ok && key != "" {
		return dimensionParts{prefix: "metric", key: key}
	}
	if key, ok := strings.CutPrefix(dimension, "resource.labels."); ok && key != "" {
		return dimensionParts{prefix: "resource", key: key}
	}
	return dimensionParts{key: dimension}
}

