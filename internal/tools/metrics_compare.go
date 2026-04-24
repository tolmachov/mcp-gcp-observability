package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// compareCallResult builds the LLM-facing CallToolResult for metrics_compare.
// It strips chart_points_a/b from the JSON text in Content so raw time-series
// data is never sent to the LLM. The caller returns the unmodified *CompareResult
// as the second handler value; the SDK serializes it into StructuredContent,
// making chart_points_a/b available to the UI widget.
func compareCallResult(result *CompareResult) *mcp.CallToolResult {
	// Shallow copy so we can zero ChartPoints without mutating the caller's struct.
	// The caller returns result as structuredContent (with ChartPoints intact).
	clone := *result
	clone.ChartPointsA = nil
	clone.ChartPointsB = nil
	analysisJSON, err := json.Marshal(&clone)
	if err != nil {
		slog.Error("[metrics-compare] BUG: failed to marshal result", "err", err)
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("internal error: failed to marshal result: %v", err)}},
		}
	}
	return &mcp.CallToolResult{
		Meta:    mcp.Meta{"ui": map[string]any{"resourceUri": compareChartStaticURI}},
		Content: []mcp.Content{&mcp.TextContent{Text: string(analysisJSON)}},
	}
}

func RegisterMetricsCompare(s *mcp.Server, querier gcpdata.MetricsQuerier, registry *metrics.Registry, defaultProject string, mode RegistrationMode) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "metrics_compare",
		Description: applyMode(mode, "Compare two arbitrary time windows for the same metric. "+
			"Useful for deploy diff, before/after comparisons, or ad-hoc analysis. "+
			"Returns mean values, delta, trend shift, and classification for each window. "+
			"Also renders an interactive dual-series chart inline in the chat (hosts that support MCP app widgets). "+
			"For automatic baseline comparison (prev_window, same_weekday_hour), use metrics_snapshot instead."),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		// Meta here and in compareCallResult both carry the same URI deliberately:
		// this declaration lets hosts prefetch the resource from tools/list;
		// the per-call Meta binds the widget for hosts that skip tools/list caching.
		Meta:         mcp.Meta{"ui": map[string]any{"resourceUri": compareChartStaticURI}},
		OutputSchema: outputSchemaFor[CompareResult](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in MetricsCompareInput) (*mcp.CallToolResult, *CompareResult, error) {
		if in.MetricType == "" {
			return errResult("metric_type is required"), nil, nil
		}
		project, err := resolveProject(in.ProjectID, defaultProject)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		windowALabel := in.WindowALabel
		if windowALabel == "" {
			windowALabel = "window_a"
		}
		windowBLabel := in.WindowBLabel
		if windowBLabel == "" {
			windowBLabel = "window_b"
		}

		aFrom, err := parseRFC3339(in.WindowAFrom, "window_a_from")
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		aTo, err := parseRFC3339(in.WindowATo, "window_a_to")
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		bFrom, err := parseRFC3339(in.WindowBFrom, "window_b_from")
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		bTo, err := parseRFC3339(in.WindowBTo, "window_b_to")
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		if !aTo.After(aFrom) {
			return errResult(fmt.Sprintf("window_a_to must be after window_a_from (got %s to %s)", aFrom.Format(time.RFC3339), aTo.Format(time.RFC3339))), nil, nil
		}
		if !bTo.After(bFrom) {
			return errResult(fmt.Sprintf("window_b_to must be after window_b_from (got %s to %s)", bFrom.Format(time.RFC3339), bTo.Format(time.RFC3339))), nil, nil
		}

		meta := registry.Lookup(in.MetricType)
		stepSeconds := int64(60)

		sendProgress(ctx, req, 1, 4, "Looking up metric descriptor")

		descriptor, err := querier.GetMetricDescriptor(ctx, project, in.MetricType)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "metrics_compare", fmt.Sprintf("metric descriptor lookup failed: %v", err))
			return errResult(fmt.Sprintf("Failed to look up metric descriptor: %v. Verify the metric_type.", err)), nil, nil
		}

		sendProgress(ctx, req, 2, 4, "Querying both windows")

		aggSpec := meta.ResolveAggregation()
		if err := aggSpec.Validate(); err != nil {
			mcpLog(ctx, req, logLevelError, "metrics_compare",
				fmt.Sprintf("registry misconfiguration for %s: %v", in.MetricType, err))
			return errResult(formatRegistryMisconfigError(in.MetricType, err)), nil, nil
		}

		baseParams := gcpdata.QueryTimeSeriesParams{
			Project:     project,
			MetricType:  in.MetricType,
			LabelFilter: in.Filter,
			StepSeconds: stepSeconds,
			MetricKind:  descriptor.Kind,
			ValueType:   descriptor.ValueType,
		}

		paramsA := baseParams
		paramsA.Start = aFrom
		paramsA.End = aTo

		paramsB := baseParams
		paramsB.Start = bFrom
		paramsB.End = bTo

		var seriesA, seriesB []gcpdata.MetricTimeSeries
		var warningsA, warningsB gcpdata.AggregationWarnings
		var errA, errB error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					stack := debug.Stack()
					msg := fmt.Sprintf("panic querying window A: %v\n%s", r, stack)
					notifyErrLog.Load().Error("metrics_compare: panic in window A goroutine", "panic", r, "stack", string(stack))
					mcpLog(ctx, req, logLevelError, "metrics_compare", msg)
					errA = fmt.Errorf("internal error: %v", r)
				}
			}()
			if ctx.Err() != nil {
				errA = ctx.Err()
				return
			}
			seriesA, warningsA, errA = querier.QueryTimeSeriesAggregated(ctx, paramsA, aggSpec)
		}()
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					stack := debug.Stack()
					msg := fmt.Sprintf("panic querying window B: %v\n%s", r, stack)
					notifyErrLog.Load().Error("metrics_compare: panic in window B goroutine", "panic", r, "stack", string(stack))
					mcpLog(ctx, req, logLevelError, "metrics_compare", msg)
					errB = fmt.Errorf("internal error: %v", r)
				}
			}()
			if ctx.Err() != nil {
				errB = ctx.Err()
				return
			}
			seriesB, warningsB, errB = querier.QueryTimeSeriesAggregated(ctx, paramsB, aggSpec)
		}()
		wg.Wait()
		logAggregationWarnings(ctx, req, "metrics_compare", in.MetricType, windowALabel, warningsA)
		logAggregationWarnings(ctx, req, "metrics_compare", in.MetricType, windowBLabel, warningsB)
		warningsNote := joinNote(
			aggregationWarningsNote(in.MetricType, windowALabel, warningsA),
			aggregationWarningsNote(in.MetricType, windowBLabel, warningsB),
		)
		if errA != nil || errB != nil {
			var msgs []string
			if errA != nil {
				msgs = append(msgs, fmt.Sprintf("window A: %v", errA))
			}
			if errB != nil {
				msgs = append(msgs, fmt.Sprintf("window B: %v", errB))
			}
			msg := strings.Join(msgs, "; ")
			mcpLog(ctx, req, logLevelError, "metrics_compare", msg)
			if invalidAggregationSpecError(errA) || invalidAggregationSpecError(errB) {
				return errResult(formatRegistryMisconfigError(in.MetricType, errors.Join(errA, errB))), nil, nil
			}
			if isInvalidFilterError(errA) || isInvalidFilterError(errB) {
				return errResult(enrichInvalidFilterError(ctx, req, querier, project, in.MetricType, in.Filter, errors.Join(errA, errB))), nil, nil
			}
			return errResult(fmt.Sprintf("Failed to query: %s", msg)), nil, nil
		}

		unsupportedCount := reportUnsupportedPoints(ctx, req, "metrics_compare", in.MetricType, seriesA) +
			reportUnsupportedPoints(ctx, req, "metrics_compare", in.MetricType, seriesB)

		pointsA := mergePoints(seriesA)
		pointsB := mergePoints(seriesB)

		if len(pointsA) == 0 || len(pointsB) == 0 {
			var emptyLabels []string
			var notes []string
			if len(pointsA) == 0 {
				emptyLabels = append(emptyLabels, windowALabel)
				windowDesc := fmt.Sprintf("%s (%s to %s)", windowALabel, aFrom.Format(time.RFC3339), aTo.Format(time.RFC3339))
				notes = append(notes, emptyWindowMessage(in.MetricType, windowDesc, descriptor.Kind, in.Filter))
			}
			if len(pointsB) == 0 {
				emptyLabels = append(emptyLabels, windowBLabel)
				windowDesc := fmt.Sprintf("%s (%s to %s)", windowBLabel, bFrom.Format(time.RFC3339), bTo.Format(time.RFC3339))
				notes = append(notes, emptyWindowMessage(in.MetricType, windowDesc, descriptor.Kind, in.Filter))
			}

			trendShift := "unchanged"
			switch {
			case len(pointsA) == 0 && len(pointsB) > 0:
				trendShift = "emerged"
			case len(pointsA) > 0 && len(pointsB) == 0:
				trendShift = "disappeared"
			}
			noDataNote := strings.Join(notes, "\n\n")
			if warningsNote != "" {
				if noDataNote != "" {
					noDataNote += "\n\n"
				}
				noDataNote += warningsNote
			}
			if unsupportedCount > 0 {
				if noDataNote != "" {
					noDataNote += "\n\n"
				}
				noDataNote += fmt.Sprintf("Dropped %d points with unsupported or malformed value types during decode (see server log).", unsupportedCount)
			}
			result := &CompareResult{
				WindowALabel:              windowALabel,
				WindowBLabel:              windowBLabel,
				TrendShift:                trendShift,
				ClassificationA:           string(metrics.ClassInsufficientData),
				ClassificationB:           string(metrics.ClassInsufficientData),
				ClassificationConfidenceA: "low",
				ClassificationConfidenceB: "low",
				NoData:                    true,
				NoDataWindows:             emptyLabels,
				Note:                      noDataNote,
				MetricType:                in.MetricType,
				Unit:                      meta.Unit,
			}
			if len(pointsA) > 0 {
				expectedA := expectedPointsForWindow(aTo.Sub(aFrom), int(stepSeconds))
				fA := metrics.Process(pointsA, nil, meta, int(stepSeconds), expectedA)
				result.WindowAMean = fA.Mean
				result.ClassificationA = safeClassification(fA.Classification)
				result.ClassificationConfidenceA = string(fA.Confidence)
			}
			if len(pointsB) > 0 {
				expectedB := expectedPointsForWindow(bTo.Sub(bFrom), int(stepSeconds))
				fB := metrics.Process(pointsB, nil, meta, int(stepSeconds), expectedB)
				result.WindowBMean = fB.Mean
				result.ClassificationB = safeClassification(fB.Classification)
				result.ClassificationConfidenceB = string(fB.Confidence)
			}
			callResult := compareCallResult(result)
			if callResult.IsError {
				return callResult, nil, nil
			}
			return callResult, result, nil
		}

		sendProgress(ctx, req, 3, 4, "Processing results")

		expectedBaseA := expectedPointsForWindow(aTo.Sub(aFrom), int(stepSeconds))
		// Window A has no baseline — it is the reference window for Window B.
		// Pass expectedBaselinePoints=0 to skip baseline reliability checks;
		// deriveConfidence returns ConfidenceLow when BaselinePointCount == 0.
		fA := metrics.Process(pointsA, nil, meta, int(stepSeconds), 0)
		fB := metrics.Process(pointsB, pointsA, meta, int(stepSeconds), expectedBaseA)

		trendShift := "unchanged"
		if classificationSeverity(fB.Classification) > classificationSeverity(fA.Classification) {
			trendShift = "degraded"
		} else if classificationSeverity(fB.Classification) < classificationSeverity(fA.Classification) {
			trendShift = "improved"
		}

		sloBreachIntroduced := fB.SLOBreach && !fA.SLOBreach

		var note string
		if warningsNote != "" {
			note = warningsNote
		}
		if unsupportedCount > 0 {
			note = joinNote(note, fmt.Sprintf("Dropped %d points with unsupported or malformed value types during decode (see server log).", unsupportedCount))
		}

		cmp := &CompareResult{
			WindowALabel:              windowALabel,
			WindowBLabel:              windowBLabel,
			WindowAMean:               fA.Mean,
			WindowBMean:               fB.Mean,
			DeltaPct:                  fB.DeltaPct,
			TrendShift:                trendShift,
			ClassificationA:           safeClassification(fA.Classification),
			ClassificationB:           safeClassification(fB.Classification),
			ClassificationConfidenceA: string(fA.Confidence),
			ClassificationConfidenceB: string(fB.Confidence),
			TrendScoreA:               fA.TrendScore,
			TrendScoreB:               fB.TrendScore,
			StepChangePct:             fB.StepChangePct,
			SLOBreachIntroduced:       sloBreachIntroduced,
			Note:                      note,
		}
		if fB.StepChangeAt != nil {
			cmp.StepChangeAt = fB.StepChangeAt.Format(time.RFC3339)
		}
		cmp.MetricType = in.MetricType
		cmp.Unit = meta.Unit
		cmp.ChartPointsA = toChartPoints(pointsA)
		cmp.ChartPointsB = toChartPoints(pointsB)
		callResult := compareCallResult(cmp)
		if callResult.IsError {
			return callResult, nil, nil
		}
		return callResult, cmp, nil
	})
}

type CompareResult struct {
	WindowALabel              string  `json:"window_a_label"`
	WindowBLabel              string  `json:"window_b_label"`
	WindowAMean               float64 `json:"window_a_mean"`
	WindowBMean               float64 `json:"window_b_mean"`
	DeltaPct                  float64 `json:"delta_pct"`
	TrendShift                string  `json:"trend_shift"`
	ClassificationA           string  `json:"classification_a"`
	ClassificationB           string  `json:"classification_b"`
	ClassificationConfidenceA string  `json:"classification_confidence_a"`
	ClassificationConfidenceB string  `json:"classification_confidence_b"`
	// TrendScoreA: normalized total drift within window A, expressed as a fraction
	// of window A's own mean (window A has no external baseline).
	// TrendScoreB: normalized total drift within window B, expressed as a fraction
	// of window A's mean (which is used as the baseline for window B).
	TrendScoreA float64 `json:"trend_score_a,omitempty"`
	TrendScoreB float64 `json:"trend_score_b,omitempty"`
	// StepChangeAt is the estimated timestamp of a level shift in window B.
	// StepChangePct is the magnitude of that shift (% difference between first
	// and last thirds of window B).
	StepChangeAt        string   `json:"step_change_at,omitempty"`
	StepChangePct       float64  `json:"step_change_pct,omitempty"`
	SLOBreachIntroduced bool     `json:"slo_breach_introduced"`
	NoData              bool     `json:"no_data,omitempty"`
	NoDataWindows       []string `json:"no_data_windows,omitempty"`
	Note                string   `json:"note,omitempty"`

	// MetricType and Unit are included in both LLM content and structuredContent
	// so hosts and the LLM can interpret values correctly.
	MetricType string `json:"metric_type,omitempty"`
	Unit       string `json:"unit,omitempty"`

	// Chart data: present in structuredContent (the SDK serializes the second handler return value);
	// excluded from LLM content by compareCallResult (zeroed on the shallow copy).
	// Also nil on error returns and on the no-data path, where chart points are never populated.
	ChartPointsA []chartPoint `json:"chart_points_a,omitempty"`
	ChartPointsB []chartPoint `json:"chart_points_b,omitempty"`
}

func parseRFC3339(s, field string) (time.Time, error) {
	if s == "" {
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
	case metrics.ClassInsufficientData:
		return 0
	case metrics.ClassStable:
		return 0
	case metrics.ClassImprovement:
		return -1
	case metrics.ClassNoisy:
		return 1
	case metrics.ClassRecovery:
		return 2
	case metrics.ClassSpike:
		return 3
	case metrics.ClassFlapping:
		return 4
	case metrics.ClassStepRegression:
		return 5
	case metrics.ClassSustainedRegression:
		return 6
	case metrics.ClassSaturation:
		return 7
	default:
		// Unknown classifications are treated as high severity (fail-safe).
		return 6
	}
}
