package tools

import (
	"context"

	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// withoutChart returns a copy of r without chart points, for the LLM-facing
// content.
func (r *CompareResult) withoutChart() *CompareResult {
	c := *r
	c.ChartPointsA = nil
	c.ChartPointsB = nil
	return &c
}

func RegisterMetricsCompare(s *mcp.Server, d Deps) {
	requireQuerier(d.Querier)
	requireRegistry(d.Registry)
	mcp.AddTool(s, &mcp.Tool{
		Name: "metrics_compare",
		Description: applyMode(d.Mode, "Compare two arbitrary time windows for the same metric. "+
			"Useful for deploy diff, before/after comparisons, or ad-hoc analysis. "+
			"Returns mean values, delta, trend shift, and classification for each window. "+
			"Also renders an interactive dual-series chart inline in the chat (hosts that support MCP app widgets). "+
			"For automatic baseline comparison (prev_window, same_weekday_hour), use metrics_snapshot instead."),
		Annotations: readOnlyAnnotations,
		// Meta here and in chartCallResult both carry the same URI deliberately:
		// this declaration lets hosts prefetch the resource from tools/list;
		// the per-call Meta binds the widget for hosts that skip tools/list caching.
		Meta: mcp.Meta{"ui": map[string]any{"resourceUri": compareChartStaticURI}},
		InputSchema: projectInputSchema[MetricsCompareInput](d.Project,
			nonEmptyProp("metric_type"),
			nonEmptyProp("window_a_from"),
			nonEmptyProp("window_a_to"),
			nonEmptyProp("window_b_from"),
			nonEmptyProp("window_b_to"),
		),
		OutputSchema: outputSchemaFor[CompareResult](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in MetricsCompareInput) (*mcp.CallToolResult, *CompareResult, error) {
		project, err := d.Project.Resolve(in.ProjectID)
		if err != nil {
			return ErrorResult(err.Error()), nil, nil
		}

		windowALabel := in.WindowALabel
		if windowALabel == "" {
			windowALabel = "window_a"
		}
		windowBLabel := in.WindowBLabel
		if windowBLabel == "" {
			windowBLabel = "window_b"
		}

		aFrom, err := parseRFC3339Opt(in.WindowAFrom, "window_a_from")
		if err != nil {
			return ErrorResult(err.Error()), nil, nil
		}
		aTo, err := parseRFC3339Opt(in.WindowATo, "window_a_to")
		if err != nil {
			return ErrorResult(err.Error()), nil, nil
		}
		bFrom, err := parseRFC3339Opt(in.WindowBFrom, "window_b_from")
		if err != nil {
			return ErrorResult(err.Error()), nil, nil
		}
		bTo, err := parseRFC3339Opt(in.WindowBTo, "window_b_to")
		if err != nil {
			return ErrorResult(err.Error()), nil, nil
		}

		if !aTo.After(aFrom) {
			return ErrorResult(fmt.Sprintf("window_a_to must be after window_a_from (got %s to %s)", aFrom.Format(time.RFC3339), aTo.Format(time.RFC3339))), nil, nil
		}
		if !bTo.After(bFrom) {
			return ErrorResult(fmt.Sprintf("window_b_to must be after window_b_from (got %s to %s)", bFrom.Format(time.RFC3339), bTo.Format(time.RFC3339))), nil, nil
		}

		meta := d.Registry.Lookup(in.MetricType)
		stepSeconds := int64(metrics.DefaultStepSeconds)

		sendProgress(ctx, req, 1, 4, "Looking up metric descriptor")

		descriptor, errRes := lookupMetricDescriptor(ctx, req, d.Querier, "metrics_compare", project, in.MetricType)
		if errRes != nil {
			return errRes, nil, nil
		}

		sendProgress(ctx, req, 2, 4, "Querying both windows")

		aggSpec, errRes := resolveValidAggSpec(ctx, req, "metrics_compare", in.MetricType, meta)
		if errRes != nil {
			return errRes, nil, nil
		}

		baseParams := gcpdata.QueryTimeSeriesParams{
			Project:     project,
			MetricType:  in.MetricType,
			LabelFilter: in.Filter,
			StepSeconds: stepSeconds,
			MetricKind:  descriptor.Kind,
			ValueType:   descriptor.ValueType,
		}

		windows := [2]struct {
			label    string
			from, to time.Time
		}{{windowALabel, aFrom, aTo}, {windowBLabel, bFrom, bTo}}
		params := make([]gcpdata.QueryTimeSeriesParams, len(windows))
		for i, w := range windows {
			params[i] = baseParams
			params[i].Start, params[i].End = w.from, w.to
		}
		results := runQueries(ctx, "metrics_compare", params,
			func(p gcpdata.QueryTimeSeriesParams) ([]gcpdata.MetricTimeSeries, gcpdata.QueryWarnings, error) {
				return d.Querier.QueryTimeSeriesAggregated(ctx, p, aggSpec)
			})
		warningsNote := joinNote(
			reportQueryWarnings(ctx, req, "metrics_compare", in.MetricType, windowALabel, results[0].warnings),
			reportQueryWarnings(ctx, req, "metrics_compare", in.MetricType, windowBLabel, results[1].warnings),
		)
		if errA, errB := results[0].err, results[1].err; errA != nil || errB != nil {
			var msgs []string
			if errA != nil {
				msgs = append(msgs, fmt.Sprintf("window A: %v", errA))
			}
			if errB != nil {
				msgs = append(msgs, fmt.Sprintf("window B: %v", errB))
			}
			msg := strings.Join(msgs, "; ")
			mcpLog(ctx, req, logLevelError, "metrics_compare", msg)
			return metricQueryErrorResult(ctx, req, d.Querier, project, in.MetricType, in.Filter,
				"Failed to query: "+msg, errA, errB), nil, nil
		}

		pointsA := mergePoints(results[0].series)
		pointsB := mergePoints(results[1].series)
		nonFinitePoints := results[0].warnings.NonFinitePoints + results[1].warnings.NonFinitePoints

		if len(pointsA) == 0 || len(pointsB) == 0 {
			var emptyLabels []string
			var notes []string
			for i, pts := range [2][]metrics.Point{pointsA, pointsB} {
				if len(pts) > 0 {
					continue
				}
				w := windows[i]
				emptyLabels = append(emptyLabels, w.label)
				windowDesc := fmt.Sprintf("%s (%s to %s)", w.label, w.from.Format(time.RFC3339), w.to.Format(time.RFC3339))
				notes = append(notes, emptyWindowMessage(in.MetricType, windowDesc, descriptor.Kind, in.Filter))
			}

			trendShift := "unchanged"
			switch {
			case len(pointsA) == 0 && len(pointsB) > 0:
				trendShift = "emerged"
			case len(pointsA) > 0 && len(pointsB) == 0:
				trendShift = "disappeared"
			}
			result := &CompareResult{
				WindowALabel:              windowALabel,
				WindowBLabel:              windowBLabel,
				TrendShift:                trendShift,
				ClassificationA:           string(metrics.ClassInsufficientData),
				ClassificationB:           string(metrics.ClassInsufficientData),
				ClassificationConfidenceA: string(metrics.ConfidenceLow),
				ClassificationConfidenceB: string(metrics.ConfidenceLow),
				NoData:                    true,
				NoDataWindows:             emptyLabels,
				Note:                      joinNote(append(notes, warningsNote)...),
				MetricType:                in.MetricType,
				Unit:                      meta.Unit,
				NonFinitePoints:           nonFinitePoints,
			}
			if len(pointsA) > 0 {
				expectedA := expectedPointsForWindow(aTo.Sub(aFrom), int(stepSeconds))
				fA := metrics.Process(pointsA, nil, meta, int(stepSeconds), expectedA, metrics.Window{Start: aFrom, End: aTo})
				result.WindowAMean = fA.Mean
				result.ClassificationA = string(fA.Classification)
				result.ClassificationConfidenceA = string(fA.Confidence)
			}
			if len(pointsB) > 0 {
				expectedB := expectedPointsForWindow(bTo.Sub(bFrom), int(stepSeconds))
				fB := metrics.Process(pointsB, nil, meta, int(stepSeconds), expectedB, metrics.Window{Start: bFrom, End: bTo})
				result.WindowBMean = fB.Mean
				result.ClassificationB = string(fB.Classification)
				result.ClassificationConfidenceB = string(fB.Confidence)
			}
			res, out := chartCallResult(result, result.withoutChart(), compareChartStaticURI)
			return res, out, nil
		}

		sendProgress(ctx, req, 3, 4, "Processing results")

		expectedBaseA := expectedPointsForWindow(aTo.Sub(aFrom), int(stepSeconds))
		// Window A has no baseline — it is the reference window for Window B.
		// Pass expectedBaselinePoints=0 to skip baseline reliability checks;
		// deriveConfidence returns ConfidenceLow when BaselinePointCount == 0.
		fA := metrics.Process(pointsA, nil, meta, int(stepSeconds), 0, metrics.Window{Start: aFrom, End: aTo})
		fB := metrics.Process(pointsB, pointsA, meta, int(stepSeconds), expectedBaseA, metrics.Window{Start: bFrom, End: bTo})

		trendShift := "unchanged"
		if fB.Classification.Severity() > fA.Classification.Severity() {
			trendShift = "degraded"
		} else if fB.Classification.Severity() < fA.Classification.Severity() {
			trendShift = "improved"
		}

		cmp := &CompareResult{
			WindowALabel:              windowALabel,
			WindowBLabel:              windowBLabel,
			WindowAMean:               fA.Mean,
			WindowBMean:               fB.Mean,
			DeltaPct:                  fB.DeltaPct,
			TrendShift:                trendShift,
			ClassificationA:           string(fA.Classification),
			ClassificationB:           string(fB.Classification),
			ClassificationConfidenceA: string(fA.Confidence),
			ClassificationConfidenceB: string(fB.Confidence),
			TrendScoreA:               fA.TrendScore,
			TrendScoreB:               fB.TrendScore,
			StepChangePct:             fB.StepChangePct,
			SLOBreachIntroduced:       fB.SLOBreach && !fA.SLOBreach,
			Note:                      warningsNote,
			NonFinitePoints:           nonFinitePoints,
			MetricType:                in.MetricType,
			Unit:                      meta.Unit,
			ChartPointsA:              toChartPoints(pointsA),
			ChartPointsB:              toChartPoints(pointsB),
		}
		if fB.StepChangeAt != nil {
			cmp.StepChangeAt = fB.StepChangeAt.Format(time.RFC3339)
		}
		res, out := chartCallResult(cmp, cmp.withoutChart(), compareChartStaticURI)
		return res, out, nil
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
	NonFinitePoints     int      `json:"non_finite_points"`

	// MetricType and Unit are included in both LLM content and structuredContent
	// so hosts and the LLM can interpret values correctly.
	MetricType string `json:"metric_type,omitempty"`
	Unit       string `json:"unit,omitempty"`

	// Chart data: present in structuredContent (the SDK serializes the second handler return value);
	// excluded from LLM content by chartCallResult (see withoutChart).
	// Also nil on error returns and on the no-data path, where chart points are never populated.
	ChartPointsA []chartPoint `json:"chart_points_a,omitempty"`
	ChartPointsB []chartPoint `json:"chart_points_b,omitempty"`
}
