package tools

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

func RegisterMetricsTop(s *mcp.Server, d Deps) {
	requireQuerier(d.Querier)
	requireRegistry(d.Registry)
	mcp.AddTool(s, &mcp.Tool{
		Name: "metrics_top_contributors",
		Description: applyMode(d.Mode, "Break down a metric by a label dimension to find which label values contribute most to an anomaly. "+
			"Shows each contributor's delta from baseline and share of the total anomaly. "+
			"The `dimension` parameter must be a fully-qualified label key — e.g. `metric.labels.response_code` or "+
			"`resource.labels.instance_id`. Call metrics_snapshot first to see `available_labels` "+
			"if you're unsure which namespace a label is in. "+
			"Use this after metrics_snapshot shows a regression — it answers 'which route/instance/status_code is responsible?' "+
			"For comparing time windows (e.g. before/after deploy), use metrics_compare instead."),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		InputSchema: projectInputSchema[MetricsTopInput](d.Project,
			nonEmptyProp("metric_type"),
			nonEmptyProp("dimension"),
			enumProp("window", metricWindowNames()),
			enumProp("baseline_mode", baselineModes),
		),
		OutputSchema: outputSchemaFor[TopContributorsResult](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in MetricsTopInput) (*mcp.CallToolResult, *TopContributorsResult, error) {
		project, err := d.Project.Resolve(in.ProjectID)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		limit := clampLimit(in.Limit, 5, 20)
		stepSeconds := int64(metrics.DefaultStepSeconds)

		windowStr, windowDur, err := parseWindow(in.Window)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		baselineMode, eventTime, err := parseBaselineMode(in.BaselineMode, in.EventTime)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		if errMsg := validateTopContributorDimension(in.Dimension); errMsg != "" {
			return errResult(errMsg), nil, nil
		}

		meta := d.Registry.Lookup(in.MetricType)
		now := time.Now().UTC()
		start := now.Add(-windowDur)

		sendProgress(ctx, req, 1, 3, "Looking up metric descriptor")

		descriptor, errRes := lookupMetricDescriptor(ctx, req, d.Querier, "metrics_top_contributors", project, in.MetricType)
		if errRes != nil {
			return errRes, nil, nil
		}
		availableLabels := availableLabelsFromDescriptor(ctx, req, d.Querier, project, in.MetricType, descriptor)

		aggSpec, errRes := resolveValidAggSpec(ctx, req, "metrics_top_contributors", in.MetricType, meta)
		if errRes != nil {
			return errRes, nil, nil
		}
		if aggSpec.IsTwoStage() {
			mcpLog(ctx, req, logLevelWarning, "metrics_top_contributors",
				fmt.Sprintf("metric %q has two-stage aggregation (group_by=%v, within_group=%s, across_groups=%s); top_contributors only applies %s across the chosen dimension %q and ignores the within_group dedup stage. Per-contributor totals may differ from metrics_snapshot — fix by overriding the dimension or trust snapshot for headline numbers.",
					in.MetricType, aggSpec.GroupBy, aggSpec.WithinGroup, aggSpec.AcrossGroups, aggSpec.AcrossGroups, in.Dimension))
		}
		reducer := gcpdata.ReducerToGCP(aggSpec.AcrossGroups)

		sendProgress(ctx, req, 2, 3, "Querying current window grouped by "+in.Dimension+" and baseline ("+string(baselineMode)+")")

		currentParams := gcpdata.QueryTimeSeriesParams{
			Project:       project,
			MetricType:    in.MetricType,
			LabelFilter:   in.Filter,
			Start:         start,
			End:           now,
			StepSeconds:   stepSeconds,
			MetricKind:    descriptor.Kind,
			ValueType:     descriptor.ValueType,
			GroupByFields: []string{in.Dimension},
			Reducer:       reducer,
		}
		baselineWindows := baselineMode.windows(start, now, eventTime)
		current, baselineResults := queryWithBaseline(ctx, "metrics_top_contributors", currentParams, baselineWindows,
			func(p gcpdata.QueryTimeSeriesParams) ([]gcpdata.MetricTimeSeries, gcpdata.QueryWarnings, error) {
				return d.Querier.QueryTimeSeries(ctx, p)
			})

		currentWarningsNote := reportQueryWarnings(ctx, req, "metrics_top_contributors", in.MetricType, "current", current.warnings)
		if err := current.err; err != nil {
			mcpLog(ctx, req, logLevelError, "metrics_top_contributors", fmt.Sprintf("current window query failed: %v", err))
			if isInvalidFilterError(err) {
				return errResult(enrichInvalidFilterError(ctx, req, d.Querier, project, in.MetricType, in.Filter, err)), nil, nil
			}
			return errResult(fmt.Sprintf("Failed to query metric: %v", err)), nil, nil
		}
		currentSeries := current.series

		if len(currentSeries) == 0 {
			msg := emptyWindowMessage(in.MetricType, windowStr, descriptor.Kind, in.Filter)
			msg += fmt.Sprintf(" Also check that dimension %q actually exists on this metric — see `available_labels` below.", in.Dimension)
			r := &TopContributorsResult{
				Dimension:       in.Dimension,
				Contributors:    []Contributor{},
				NoData:          true,
				Note:            joinNote(msg, currentWarningsNote),
				AvailableLabels: availableLabels,
				NonFinitePoints: current.warnings.NonFinitePoints,
			}
			return nil, r, nil
		}

		sendProgress(ctx, req, 3, 3, "Processing results")

		// Without usable baseline data every delta is 0, so contributors are
		// ranked by current value instead.
		var baselineErrNote, noBaselineDataNote string
		rankByCurrent := false
		baselineNote, err := collectBaseline(ctx, req, "metrics_top_contributors", in.MetricType, baselineMode, baselineWindows, baselineResults)
		baselineByLabel := map[string][][]metrics.Point{}
		if err != nil {
			mcpLog(ctx, req, logLevelError, "metrics_top_contributors", fmt.Sprintf("baseline query failed: %v", err))
			baselineErrNote = fmt.Sprintf("Baseline query (%s) failed: %v. Returning current-window contributors only; delta_pct and share_of_anomaly are not meaningful.",
				string(baselineMode), err)
			rankByCurrent = true
		} else if !slices.ContainsFunc(baselineResults, windowResult.hasPoints) {
			// Every baseline query succeeded but returned no data (e.g. a
			// service or label value younger than the baseline window).
			// Without this note every delta_pct is 0, which is
			// indistinguishable from "nothing changed".
			noBaselineDataNote = fmt.Sprintf("Baseline (%s) had no data in any of its %d window(s); delta_pct and share_of_anomaly are 0 and contributors are ranked by current value.",
				string(baselineMode), len(baselineWindows))
			rankByCurrent = true
		} else {
			baselineByLabel = contributorBaselines(baselineResults, in.Dimension)
		}

		// Every window of a mode has the same span. pre_event contributors
		// are judged without an expected point count, only the absolute
		// minimum.
		span := baselineWindows[0].end.Sub(baselineWindows[0].start)
		expectedPerWindow := expectedPointsForWindow(span, int(stepSeconds))
		if baselineMode == BaselineModePreEvent {
			expectedPerWindow = 0
		}

		type contribData struct {
			label   string
			current []metrics.Point
			base    metrics.BaselineStats
		}
		var attributed []contribData
		totalSeries := len(currentSeries)
		missingCount := 0
		for _, s := range currentSeries {
			lv := labelValueFromSeries(s, in.Dimension)
			if lv == missingDimensionLabel {
				missingCount++
				continue
			}
			attributed = append(attributed, contribData{
				label:   lv,
				current: s.Points,
				base:    metrics.ComputeRobustBaselineStats(baselineByLabel[lv], expectedPerWindow),
			})
		}

		if missingCount == totalSeries {
			return errResult(fmt.Sprintf(
				"Dimension %q was not found in any series labels. Call metrics_snapshot on this metric_type and check `available_labels` for the valid keys (e.g. 'metric.labels.response_code' or 'resource.labels.instance_id').",
				in.Dimension,
			)), nil, nil
		}

		var partialCoverageNote string
		if missingCount > 0 {
			partialCoverageNote = fmt.Sprintf(
				"Partial dimension coverage: %d of %d series did not expose %q and were excluded from share_of_anomaly. Shares below sum to 100%% over the remaining %d attributable series.",
				missingCount, totalSeries, in.Dimension, len(attributed),
			)
			mcpLog(ctx, req, logLevelWarning, "metrics_top_contributors", partialCoverageNote)
		}

		type processedContrib struct {
			contributor Contributor
			absDelta    float64
		}

		var processed []processedContrib
		var totalAbsDelta float64

		for _, c := range attributed {
			f := metrics.ProcessWithBaselineStats(c.current, c.base, meta, int(stepSeconds), metrics.Window{Start: start, End: now})
			ad := math.Abs(f.DeltaAbs)
			totalAbsDelta += ad
			processed = append(processed, processedContrib{
				contributor: Contributor{
					LabelValue:               c.label,
					Current:                  f.Current,
					Baseline:                 f.Baseline,
					DeltaPct:                 f.DeltaPct,
					CV:                       f.CV,
					SLOBreach:                f.SLOBreach,
					Classification:           string(f.Classification),
					ClassificationConfidence: string(f.Confidence),
					BaselineReliable:         f.BaselineReliable,
				},
				absDelta: ad,
			})
		}

		var results []Contributor
		for _, pc := range processed {
			c := pc.contributor
			if totalAbsDelta > 0 {
				c.ShareOfAnomaly = pc.absDelta / totalAbsDelta
			}
			results = append(results, c)
		}

		if rankByCurrent {
			sort.Slice(results, func(i, j int) bool {
				return results[i].Current > results[j].Current
			})
		} else {
			sort.Slice(results, func(i, j int) bool {
				return math.Abs(results[i].DeltaPct) > math.Abs(results[j].DeltaPct)
			})
		}

		if len(results) > limit {
			results = results[:limit]
		}

		var twoStageNote string
		if aggSpec.IsTwoStage() {
			twoStageNote = fmt.Sprintf("This metric uses two-stage aggregation in the registry (group_by=%v, within_group=%s, across_groups=%s). `metrics_top_contributors` applies only %s across the requested dimension %q and does not run the within_group dedup stage, so contributor totals may differ from `metrics_snapshot` and `metrics_compare`.",
				aggSpec.GroupBy, aggSpec.WithinGroup, aggSpec.AcrossGroups, aggSpec.AcrossGroups, in.Dimension)
		}
		note := joinNote(baselineErrNote, baselineNote, noBaselineDataNote, partialCoverageNote, twoStageNote, currentWarningsNote)

		return nil, &TopContributorsResult{
			Dimension:       in.Dimension,
			Contributors:    results,
			Note:            note,
			AvailableLabels: availableLabels,
			NonFinitePoints: current.warnings.NonFinitePoints,
		}, nil
	})
}

type TopContributorsResult struct {
	Dimension       string        `json:"dimension"`
	Contributors    []Contributor `json:"contributors"`
	NoData          bool          `json:"no_data,omitempty"`
	Note            string        `json:"note,omitempty"`
	NonFinitePoints int           `json:"non_finite_points"`

	AvailableLabels *AvailableLabels `json:"available_labels,omitempty"`
}

type Contributor struct {
	LabelValue               string  `json:"label_value"`
	Current                  float64 `json:"current"`
	Baseline                 float64 `json:"baseline"`
	DeltaPct                 float64 `json:"delta_pct"`
	ShareOfAnomaly           float64 `json:"share_of_anomaly"`
	CV                       float64 `json:"cv,omitempty"`
	SLOBreach                bool    `json:"slo_breach"`
	Classification           string  `json:"classification"`
	ClassificationConfidence string  `json:"classification_confidence"`
	// BaselineReliable is false when this contributor's baseline had too few
	// points to trust delta_pct — e.g. a label value absent from most baseline
	// weeks. Mirrors MetricSnapshotResult.BaselineReliable so a zero delta on
	// one contributor can be told apart from a genuine no-change.
	BaselineReliable bool `json:"baseline_reliable"`
}

// contributorBaselines groups the successful baseline windows' points by the
// dimension's label value: one bucket per window, nil where the label value
// had no series or the window failed.
func contributorBaselines(results []windowResult, dimension string) map[string][][]metrics.Point {
	byLabel := make(map[string][][]metrics.Point)
	for i, r := range results {
		if r.err != nil {
			continue
		}
		for _, s := range r.series {
			lv := labelValueFromSeries(s, dimension)
			if byLabel[lv] == nil {
				byLabel[lv] = make([][]metrics.Point, len(results))
			}
			byLabel[lv][i] = append(byLabel[lv][i], s.Points...)
		}
	}
	return byLabel
}

const missingDimensionLabel = "(missing_dimension)"

// labelValueFromSeries returns the value of a fully-qualified dimension
// (validated by validateTopContributorDimension) on s, or missingDimensionLabel.
func labelValueFromSeries(s gcpdata.MetricTimeSeries, dimension string) string {
	prefix, key, _ := metrics.SplitLabelKey(dimension)
	var labels map[string]string
	switch prefix {
	case metrics.MetricLabelsPrefix:
		labels = s.MetricLabels
	case metrics.ResourceLabelsPrefix:
		labels = s.ResourceLabels
	case metrics.MetadataSystemLabelsPrefix:
		labels = s.MetadataSystemLabels
	case metrics.MetadataUserLabelsPrefix:
		labels = s.MetadataUserLabels
	}
	if v, ok := labels[key]; ok {
		return v
	}
	return missingDimensionLabel
}

func validateTopContributorDimension(dimension string) string {
	if _, _, ok := metrics.SplitLabelKey(dimension); !ok {
		return fmt.Sprintf(
			"dimension %q must be a fully-qualified label key such as `metric.labels.response_code`, `resource.labels.instance_id`, `metadata.system_labels.machine_type`, or `metadata.user_labels.env`.",
			dimension,
		)
	}
	return ""
}
