package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// MetricsCompareHandler handles the metrics.compare tool.
type MetricsCompareHandler struct {
	querier        gcpdata.MetricsQuerier
	registry       *metrics.Registry
	defaultProject string
}

// NewMetricsCompareHandler creates a new MetricsCompareHandler.
func NewMetricsCompareHandler(querier gcpdata.MetricsQuerier, registry *metrics.Registry, defaultProject string) *MetricsCompareHandler {
	return &MetricsCompareHandler{querier: querier, registry: registry, defaultProject: defaultProject}
}

// Tool returns the MCP tool definition.
func (h *MetricsCompareHandler) Tool() mcp.Tool {
	return mcp.NewTool("metrics.compare",
		mcp.WithDescription("Compare two arbitrary time windows for the same metric. "+
			"Useful for deploy diff, before/after comparisons, or ad-hoc analysis. "+
			"Returns mean values, delta, trend shift, and classification for each window. "+
			"For automatic baseline comparison (prev_window, same_weekday_hour), use metrics.snapshot instead."),
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
		mcp.WithString("filter",
			mcp.Description("Additional Cloud Monitoring label filter"),
		),
		mcp.WithString("window_a_from",
			mcp.Description("Start of window A in RFC3339 format"),
			mcp.Required(),
		),
		mcp.WithString("window_a_to",
			mcp.Description("End of window A in RFC3339 format"),
			mcp.Required(),
		),
		mcp.WithString("window_b_from",
			mcp.Description("Start of window B in RFC3339 format"),
			mcp.Required(),
		),
		mcp.WithString("window_b_to",
			mcp.Description("End of window B in RFC3339 format"),
			mcp.Required(),
		),
		mcp.WithString("window_a_label",
			mcp.Description("Label for window A (default 'window_a')"),
		),
		mcp.WithString("window_b_label",
			mcp.Description("Label for window B (default 'window_b')"),
		),
	)
}

// CompareResult is the output for metrics.compare.
type CompareResult struct {
	WindowALabel     string  `json:"window_a_label"`
	WindowBLabel     string  `json:"window_b_label"`
	WindowAMean      float64 `json:"window_a_mean"`
	WindowBMean      float64 `json:"window_b_mean"`
	DeltaPct         float64 `json:"delta_pct"`
	TrendShift       string  `json:"trend_shift"`
	ClassificationA  string  `json:"classification_a"`
	ClassificationB  string  `json:"classification_b"`
	StepChangeDetected  bool `json:"step_change_detected"`
	SLOBreachIntroduced bool `json:"slo_breach_introduced"`
}

// Handle processes the metrics.compare tool request.
func (h *MetricsCompareHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	metricType, err := request.RequireString("metric_type")
	if err != nil {
		return mcp.NewToolResultError("metric_type is required"), nil
	}

	project, errResult := requireProject(request, h.defaultProject)
	if errResult != nil {
		return errResult, nil
	}

	labelFilter := request.GetString("filter", "")
	windowALabel := request.GetString("window_a_label", "window_a")
	windowBLabel := request.GetString("window_b_label", "window_b")

	aFrom, err := requireRFC3339(request, "window_a_from")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	aTo, err := requireRFC3339(request, "window_a_to")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	bFrom, err := requireRFC3339(request, "window_b_from")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	bTo, err := requireRFC3339(request, "window_b_to")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if !aTo.After(aFrom) {
		return mcp.NewToolResultError(fmt.Sprintf("window_a_to must be after window_a_from (got %s to %s)", aFrom.Format(time.RFC3339), aTo.Format(time.RFC3339))), nil
	}
	if !bTo.After(bFrom) {
		return mcp.NewToolResultError(fmt.Sprintf("window_b_to must be after window_b_from (got %s to %s)", bFrom.Format(time.RFC3339), bTo.Format(time.RFC3339))), nil
	}

	meta := h.registry.Lookup(metricType)
	stepSeconds := int64(60)

	sendProgress(ctx, request, 1, 4, "Looking up metric descriptor")

	gcpMetricKind, err := h.querier.GetMetricKind(ctx, project, metricType)
	if err != nil {
		mcpLog(ctx, mcp.LoggingLevelError, "metrics.compare", fmt.Sprintf("metric descriptor lookup failed: %v", err))
		return mcp.NewToolResultError(fmt.Sprintf("Failed to look up metric descriptor: %v. Verify the metric_type.", err)), nil
	}

	sendProgress(ctx, request, 2, 4, "Querying both windows")

	baseParams := gcpdata.QueryTimeSeriesParams{
		Project:     project,
		MetricType:  metricType,
		LabelFilter: labelFilter,
		StepSeconds: stepSeconds,
		MetricKind:  gcpMetricKind,
		Reducer:     monitoringpb.Aggregation_REDUCE_MEAN,
	}

	// Query both windows in parallel.
	paramsA := baseParams
	paramsA.Start = aFrom
	paramsA.End = aTo

	paramsB := baseParams
	paramsB.Start = bFrom
	paramsB.End = bTo

	var seriesA, seriesB []gcpdata.MetricTimeSeries
	var errA, errB error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				mcpLog(ctx, mcp.LoggingLevelError, "metrics.compare", fmt.Sprintf("panic querying window A: %v", r))
				errA = fmt.Errorf("internal error: %v", r)
			}
		}()
		if ctx.Err() != nil {
			errA = ctx.Err()
			return
		}
		seriesA, errA = h.querier.QueryTimeSeries(ctx, paramsA)
	}()
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				mcpLog(ctx, mcp.LoggingLevelError, "metrics.compare", fmt.Sprintf("panic querying window B: %v", r))
				errB = fmt.Errorf("internal error: %v", r)
			}
		}()
		if ctx.Err() != nil {
			errB = ctx.Err()
			return
		}
		seriesB, errB = h.querier.QueryTimeSeries(ctx, paramsB)
	}()
	wg.Wait()
	if errA != nil || errB != nil {
		var msgs []string
		if errA != nil {
			msgs = append(msgs, fmt.Sprintf("window A: %v", errA))
		}
		if errB != nil {
			msgs = append(msgs, fmt.Sprintf("window B: %v", errB))
		}
		msg := strings.Join(msgs, "; ")
		mcpLog(ctx, mcp.LoggingLevelError, "metrics.compare", msg)
		return mcp.NewToolResultError(fmt.Sprintf("Failed to query: %s", msg)), nil
	}

	pointsA := mergePoints(seriesA)
	pointsB := mergePoints(seriesB)

	if len(pointsA) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("No data found for %s (%s to %s). Verify the metric_type with metrics.list and check the time range.", windowALabel, aFrom.Format(time.RFC3339), aTo.Format(time.RFC3339))), nil
	}
	if len(pointsB) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("No data found for %s (%s to %s). Verify the metric_type with metrics.list and check the time range.", windowBLabel, bFrom.Format(time.RFC3339), bTo.Format(time.RFC3339))), nil
	}

	sendProgress(ctx, request, 3, 4, "Processing results")

	// Process: A is self-baseline, B uses A as baseline.
	fA := metrics.Process(pointsA, nil, meta, int(stepSeconds))
	fB := metrics.Process(pointsB, pointsA, meta, int(stepSeconds))

	trendShift := "unchanged"
	if classificationSeverity(fB.Classification) > classificationSeverity(fA.Classification) {
		trendShift = "degraded"
	} else if classificationSeverity(fB.Classification) < classificationSeverity(fA.Classification) {
		trendShift = "improved"
	}

	sloBreachIntroduced := fB.SLOBreach && !fA.SLOBreach

	return jsonResult(CompareResult{
		WindowALabel:        windowALabel,
		WindowBLabel:        windowBLabel,
		WindowAMean:         fA.Mean,
		WindowBMean:         fB.Mean,
		DeltaPct:            fB.DeltaPct,
		TrendShift:          trendShift,
		ClassificationA:     string(fA.Classification),
		ClassificationB:     string(fB.Classification),
		StepChangeDetected:  fB.StepChangeDetected,
		SLOBreachIntroduced: sloBreachIntroduced,
	})
}

func requireRFC3339(request mcp.CallToolRequest, field string) (time.Time, error) {
	s, err := request.RequireString(field)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s is required", field)
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid %s %q: must be RFC3339 format", field, s)
	}
	return t, nil
}

func classificationSeverity(class metrics.Classification) int {
	switch class {
	case metrics.ClassStable:
		return 0
	case metrics.ClassNoisy:
		return 1
	case metrics.ClassRecovery:
		return 2
	case metrics.ClassSpike:
		return 3
	case metrics.ClassStepRegression:
		return 4
	case metrics.ClassSustainedRegression:
		return 5
	case metrics.ClassSaturation:
		return 6
	default:
		// Unknown classifications are treated as high severity (fail-safe).
		return 5
	}
}
