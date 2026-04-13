package tools

import (
	"context"
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

func RegisterMetricsTop(s *mcp.Server, querier gcpdata.MetricsQuerier, registry *metrics.Registry, defaultProject string) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "metrics.top_contributors",
		Description: "Break down a metric by a label dimension to find which label values contribute most to an anomaly. " +
			"Shows each contributor's delta from baseline and share of the total anomaly. " +
			"The `dimension` parameter must be a fully-qualified label key — e.g. `metric.labels.response_code` or " +
			"`resource.labels.instance_id`. Call metrics.snapshot first to see `available_labels` " +
			"if you're unsure which namespace a label is in. " +
			"Use this after metrics.snapshot shows a regression — it answers 'which route/instance/status_code is responsible?' " +
			"For comparing time windows (e.g. before/after deploy), use metrics.compare instead.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			OpenWorldHint:  new(true),
			IdempotentHint: true,
		},
		InputSchema: inputSchemaWithEnums[MetricsTopInput](
			enumPatch{"window", enumWindow},
			enumPatch{"baseline_mode", enumBaselineMode},
		),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in MetricsTopInput) (*mcp.CallToolResult, *TopContributorsResult, error) {
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
		limit := clampLimit(in.Limit, 5, 20)
		stepSeconds := int64(60)

		windowDur, err := parseWindow(windowStr)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}

		if err := baselineMode.Validate(in.EventTime); err != nil {
			return errResult(err.Error()), nil, nil
		}

		if errMsg := validateTopContributorDimension(in.Dimension); errMsg != "" {
			return errResult(errMsg), nil, nil
		}

		meta := registry.Lookup(in.MetricType)
		now := time.Now().UTC()
		start := now.Add(-windowDur)

		sendProgress(ctx, req, 1, 4, "Looking up metric descriptor")

		descriptor, err := querier.GetMetricDescriptor(ctx, project, in.MetricType)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "metrics.top_contributors", fmt.Sprintf("metric descriptor lookup failed: %v", err))
			return errResult(fmt.Sprintf("Failed to look up metric descriptor: %v. Verify the metric_type.", err)), nil, nil
		}
		availableLabels := availableLabelsFromDescriptor(ctx, req, querier, project, in.MetricType, descriptor)

		aggSpec := meta.ResolveAggregation()
		if err := aggSpec.Validate(); err != nil {
			mcpLog(ctx, req, logLevelError, "metrics.top_contributors",
				fmt.Sprintf("registry misconfiguration for %s: %v", in.MetricType, err))
			return errResult(formatRegistryMisconfigError(in.MetricType, err)), nil, nil
		}
		if aggSpec.IsTwoStage() {
			mcpLog(ctx, req, logLevelWarning, "metrics.top_contributors",
				fmt.Sprintf("metric %q has two-stage aggregation (group_by=%v, within_group=%s, across_groups=%s); top_contributors only applies %s across the chosen dimension %q and ignores the within_group dedup stage. Per-contributor totals may differ from metrics.snapshot — fix by overriding the dimension or trust snapshot for headline numbers.",
					in.MetricType, aggSpec.GroupBy, aggSpec.WithinGroup, aggSpec.AcrossGroups, aggSpec.AcrossGroups, in.Dimension))
		}
		reducer := gcpdata.ReducerToGCP(aggSpec.AcrossGroups)

		sendProgress(ctx, req, 2, 4, "Querying current window grouped by "+in.Dimension)

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
		currentSeries, err := querier.QueryTimeSeries(ctx, currentParams)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "metrics.top_contributors", fmt.Sprintf("current window query failed: %v", err))
			if isInvalidFilterError(err) {
				return errResult(enrichInvalidFilterError(ctx, req, querier, project, in.MetricType, in.Filter, err)), nil, nil
			}
			return errResult(fmt.Sprintf("Failed to query metric: %v", err)), nil, nil
		}
		currentSeries, truncated := stripTruncatedSeries(currentSeries)
		if truncated {
			mcpLog(ctx, req, logLevelWarning, "metrics.top_contributors",
				fmt.Sprintf("metric %q (current): query hit the server-side time-series cap (%d series); contributors are computed from a partial set of series only. Narrow the filter or choose a lower-cardinality dimension before trusting shares.",
					in.MetricType, gcpdata.MaxTimeSeries))
		}
		unsupportedCount := reportUnsupportedPoints(ctx, req, "metrics.top_contributors", in.MetricType, currentSeries)

		if len(currentSeries) == 0 {
			msg := emptyWindowMessage(in.MetricType, windowStr, descriptor.Kind, in.Filter)
			msg += fmt.Sprintf(" Also check that dimension %q actually exists on this metric — see `available_labels` below.", in.Dimension)
			r := &TopContributorsResult{
				Dimension:       in.Dimension,
				Contributors:    []Contributor{},
				NoData:          true,
				Note:            msg,
				AvailableLabels: availableLabels,
			}
			return nil, r, nil
		}

		sendProgress(ctx, req, 3, 4, "Querying baseline ("+string(baselineMode)+")")

		var baselineErrNote string
		baselineByLabel, baselinePartialNote, err := queryContributorBaselines(ctx, req, querier, currentParams, windowDur, baselineMode, in.EventTime, in.Dimension)
		if err != nil {
			mcpLog(ctx, req, logLevelError, "metrics.top_contributors", fmt.Sprintf("baseline query failed: %v", err))
			baselineByLabel = map[string]contributorBaseline{}
			baselineErrNote = fmt.Sprintf("Baseline query (%s) failed: %v. Returning current-window contributors only; delta_pct and share_of_anomaly are not meaningful.",
				string(baselineMode), err)
		}

		expectedPerWindow := expectedPointsForWindow(windowDur, int(stepSeconds))

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
				base:    baselineByLabel[lv].toBaselineStats(baselineMode, expectedPerWindow),
			})
		}

		if missingCount == totalSeries {
			return errResult(fmt.Sprintf(
				"Dimension %q was not found in any series labels. Call metrics.snapshot on this metric_type and check `available_labels` for the valid keys (e.g. 'metric.labels.response_code' or 'resource.labels.instance_id').",
				in.Dimension,
			)), nil, nil
		}

		var partialCoverageNote string
		if missingCount > 0 {
			partialCoverageNote = fmt.Sprintf(
				"Partial dimension coverage: %d of %d series did not expose %q and were excluded from share_of_anomaly. Shares below sum to 100%% over the remaining %d attributable series.",
				missingCount, totalSeries, in.Dimension, len(attributed),
			)
			mcpLog(ctx, req, logLevelWarning, "metrics.top_contributors", partialCoverageNote)
		}

		type processedContrib struct {
			contributor Contributor
			absDelta    float64
		}

		var processed []processedContrib
		var totalAbsDelta float64

		for _, c := range attributed {
			f := metrics.ProcessWithBaselineStats(c.current, c.base, meta, int(stepSeconds))
			ad := math.Abs(f.DeltaAbs)
			totalAbsDelta += ad
			processed = append(processed, processedContrib{
				contributor: Contributor{
					LabelValue:               c.label,
					Current:                  f.Current,
					Baseline:                 f.Baseline,
					DeltaPct:                 f.DeltaPct,
					SLOBreach:                f.SLOBreach,
					Classification:           safeClassification(f.Classification),
					ClassificationConfidence: string(f.Confidence),
				},
				absDelta: ad,
			})
		}

		var results []Contributor
		for _, pc := range processed {
			c := pc.contributor
			if baselineErrNote != "" {
				// Baseline failed: delta values are relative to a zero baseline
				// and would appear as false regressions. Zero them out and let
				// Current drive the ranking instead.
				c.Baseline = 0
				c.DeltaPct = 0
				c.ShareOfAnomaly = 0
			} else if totalAbsDelta > 0 {
				c.ShareOfAnomaly = pc.absDelta / totalAbsDelta
			}
			results = append(results, c)
		}

		if baselineErrNote != "" {
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

		note := baselineErrNote
		appendNote := func(s string) {
			if s == "" {
				return
			}
			if note == "" {
				note = s
			} else {
				note = note + " " + s
			}
		}
		appendNote(baselinePartialNote)
		appendNote(partialCoverageNote)
		if aggSpec.IsTwoStage() {
			appendNote(fmt.Sprintf("This metric uses two-stage aggregation in the registry (group_by=%v, within_group=%s, across_groups=%s). `metrics.top_contributors` applies only %s across the requested dimension %q and does not run the within_group dedup stage, so contributor totals may differ from `metrics.snapshot` and `metrics.compare`.",
				aggSpec.GroupBy, aggSpec.WithinGroup, aggSpec.AcrossGroups, aggSpec.AcrossGroups, in.Dimension))
		}
		if truncated {
			appendNote(fmt.Sprintf("Query hit the server-side time-series cap (%d series). Contributors and share_of_anomaly are computed from a partial set of series only; narrow the filter or dimension cardinality before trusting the ranking.", gcpdata.MaxTimeSeries))
		}
		if unsupportedCount > 0 {
			appendNote(fmt.Sprintf("Dropped %d points with unsupported or malformed value types during decode (see server log).", unsupportedCount))
		}

		return nil, &TopContributorsResult{
			Dimension:       in.Dimension,
			Contributors:    results,
			Note:            note,
			AvailableLabels: availableLabels,
		}, nil
	})
}

type TopContributorsResult struct {
	Dimension    string        `json:"dimension"`
	Contributors []Contributor `json:"contributors"`
	NoData       bool          `json:"no_data,omitempty"`
	Note         string        `json:"note,omitempty"`

	AvailableLabels *AvailableLabels `json:"available_labels,omitempty"`
}

type Contributor struct {
	LabelValue               string  `json:"label_value"`
	Current                  float64 `json:"current"`
	Baseline                 float64 `json:"baseline"`
	DeltaPct                 float64 `json:"delta_pct"`
	ShareOfAnomaly           float64 `json:"share_of_anomaly"`
	SLOBreach                bool    `json:"slo_breach"`
	Classification           string  `json:"classification"`
	ClassificationConfidence string  `json:"classification_confidence"`
}

type contributorBaseline struct {
	buckets [][]metrics.Point
}

func (c contributorBaseline) toBaselineStats(mode BaselineMode, expectedPerWindow int) metrics.BaselineStats {
	if mode == BaselineModeSameWeekdayHour {
		return metrics.ComputeRobustBaselineStats(c.buckets, expectedPerWindow)
	}
	if len(c.buckets) == 0 {
		return metrics.BaselineStats{}
	}
	expected := expectedPerWindow
	if mode == BaselineModePreEvent {
		expected = 0
	}
	return metrics.ComputeBaselineStats(c.buckets[0], expected)
}

// queryContributorBaselines returns (baseline map, partialNote, error).
// partialNote is non-empty when some weekly queries failed but enough
// data remains (same_weekday_hour only); the caller surfaces it in Note.
func queryContributorBaselines(
	ctx context.Context,
	req *mcp.CallToolRequest,
	querier gcpdata.MetricsQuerier,
	currentParams gcpdata.QueryTimeSeriesParams,
	windowDur time.Duration,
	mode BaselineMode,
	eventTimeStr string,
	dimension string,
) (map[string]contributorBaseline, string, error) {
	result := make(map[string]contributorBaseline)
	var partialNote string

	addBucket := func(idx, total int, series []gcpdata.MetricTimeSeries) {
		byLabel := make(map[string][]metrics.Point)
		for _, s := range series {
			lv := labelValueFromSeries(s, dimension)
			byLabel[lv] = append(byLabel[lv], s.Points...)
		}
		for lv, pts := range byLabel {
			cb := result[lv]
			if cb.buckets == nil {
				cb.buckets = make([][]metrics.Point, total)
			}
			cb.buckets[idx] = pts
			result[lv] = cb
		}
	}

	switch mode {
	case BaselineModeSameWeekdayHour:
		const weeks = 4
		perWeek := make([][]gcpdata.MetricTimeSeries, weeks)
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
						msg := fmt.Sprintf("panic in baseline week -%d: %v\n%s", weeksBack, r, stack)
						notifyErrLog.Load().Printf("metrics.top_contributors: %s", msg)
						mcpLog(ctx, req, logLevelError, "metrics.top_contributors", msg)
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
				p := currentParams
				p.Start = currentParams.Start.AddDate(0, 0, -7*weeksBack)
				p.End = currentParams.End.AddDate(0, 0, -7*weeksBack)
				series, err := querier.QueryTimeSeries(ctx, p)
				series, truncated := stripTruncatedSeries(series)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs = append(errs, fmt.Errorf("week -%d: %w", weeksBack, err))
					return
				}
				if truncated {
					mcpLog(ctx, req, logLevelWarning, "metrics.top_contributors",
						fmt.Sprintf("baseline week -%d: time series result was truncated at server limit; baseline may be incomplete", weeksBack))
				}
				perWeek[weeksBack-1] = series
			}(w)
		}
		wg.Wait()

		nonEmpty := 0
		for i, ws := range perWeek {
			if len(ws) == 0 {
				continue
			}
			nonEmpty++
			addBucket(i, weeks, ws)
		}
		if nonEmpty == 0 && len(errs) > 0 {
			return nil, "", fmt.Errorf("all %d baseline queries failed; first error: %w", len(errs), errors.Join(errs...))
		}
		if len(errs) > 0 && nonEmpty > 0 {
			mcpLog(ctx, req, logLevelWarning, "metrics.top_contributors",
				fmt.Sprintf("baseline partial failure: %d of %d weeks failed (%v); using %d weeks of data",
					len(errs), weeks, errors.Join(errs...), nonEmpty))
			partialNote = fmt.Sprintf("Baseline partial failure (%s): %d of %d weekly samples could not be fetched; baseline computed from %d weeks. Results may be less reliable.",
				string(BaselineModeSameWeekdayHour), len(errs), weeks, nonEmpty)
		}
		return result, partialNote, nil

	case BaselineModePreEvent:
		eventTime, err := time.Parse(time.RFC3339, eventTimeStr)
		if err != nil {
			return nil, "", fmt.Errorf("invalid event_time: %w", err)
		}
		p := currentParams
		p.End = eventTime
		p.Start = eventTime.Add(-30 * time.Minute)
		series, err := querier.QueryTimeSeries(ctx, p)
		if err != nil {
			return nil, "", fmt.Errorf("querying pre_event baseline: %w", err)
		}
		series, truncated := stripTruncatedSeries(series)
		if truncated {
			mcpLog(ctx, req, logLevelWarning, "metrics.top_contributors",
				"pre_event baseline: time series result was truncated at server limit; baseline may be incomplete")
		}
		addBucket(0, 1, series)
		return result, "", nil

	default: // prev_window
		p := currentParams
		p.End = currentParams.Start
		p.Start = currentParams.Start.Add(-windowDur)
		series, err := querier.QueryTimeSeries(ctx, p)
		if err != nil {
			return nil, "", fmt.Errorf("querying prev_window baseline: %w", err)
		}
		series, truncated := stripTruncatedSeries(series)
		if truncated {
			mcpLog(ctx, req, logLevelWarning, "metrics.top_contributors",
				"prev_window baseline: time series result was truncated at server limit; baseline may be incomplete")
		}
		addBucket(0, 1, series)
		return result, "", nil
	}
}

const missingDimensionLabel = "(missing_dimension)"

func labelValueFromSeries(s gcpdata.MetricTimeSeries, dimension string) string {
	parts := splitDimension(dimension)
	switch parts.prefix {
	case "metric":
		if v, ok := s.MetricLabels[parts.key]; ok {
			return v
		}
	case "resource":
		if v, ok := s.ResourceLabels[parts.key]; ok {
			return v
		}
	case "metadata_system":
		if v, ok := s.MetadataSystemLabels[parts.key]; ok {
			return v
		}
	case "metadata_user":
		if v, ok := s.MetadataUserLabels[parts.key]; ok {
			return v
		}
	}
	if parts.prefix != "" {
		return missingDimensionLabel
	}
	if v, ok := s.MetricLabels[parts.key]; ok {
		return v
	}
	if v, ok := s.ResourceLabels[parts.key]; ok {
		return v
	}
	if v, ok := s.MetadataSystemLabels[parts.key]; ok {
		return v
	}
	if v, ok := s.MetadataUserLabels[parts.key]; ok {
		return v
	}
	return missingDimensionLabel
}

type dimensionParts struct {
	prefix string
	key    string
}

func splitDimension(dimension string) dimensionParts {
	if key, ok := strings.CutPrefix(dimension, "metric.labels."); ok && key != "" {
		return dimensionParts{prefix: "metric", key: key}
	}
	if key, ok := strings.CutPrefix(dimension, "resource.labels."); ok && key != "" {
		return dimensionParts{prefix: "resource", key: key}
	}
	if key, ok := strings.CutPrefix(dimension, "metadata.system_labels."); ok && key != "" {
		return dimensionParts{prefix: "metadata_system", key: key}
	}
	if key, ok := strings.CutPrefix(dimension, "metadata.user_labels."); ok && key != "" {
		return dimensionParts{prefix: "metadata_user", key: key}
	}
	return dimensionParts{key: dimension}
}

func validateTopContributorDimension(dimension string) string {
	parts := splitDimension(dimension)
	if parts.prefix == "" || parts.key == "" {
		return fmt.Sprintf(
			"dimension %q must be a fully-qualified label key such as `metric.labels.response_code`, `resource.labels.instance_id`, `metadata.system_labels.machine_type`, or `metadata.user_labels.env`.",
			dimension,
		)
	}
	return ""
}
