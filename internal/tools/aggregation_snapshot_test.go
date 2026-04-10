package tools

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// TestSnapshotAggregationResolve exercises the end-to-end path where
// metrics.snapshot resolves an AggregationSpec from registry metadata and
// passes it to QueryTimeSeriesAggregated. The fake querier records the
// spec received and returns fabricated points so the snapshot handler
// produces a deterministic MetricSnapshotResult.
func TestSnapshotAggregationResolve(t *testing.T) {
	t.Run("Positive: business_kpi gauge uses sum by default", testSnapshotDefaultBusinessKPISum)
	t.Run("Positive: explicit two-stage spec passed through", testSnapshotExplicitTwoStage)
	t.Run("Positive: latency histogram uses mean by default", testSnapshotLatencyMean)
	t.Run("Positive: ratio override yields mean", testSnapshotRatioOverride)
	t.Run("Positive: pre_event baseline uses same spec as current", testSnapshotPreEventSpecThreading)
	t.Run("Positive: same_weekday_hour all 5 queries share spec", testSnapshotSameWeekdayHourSpecThreading)
}

const aggregationTestRegistryYAML = `metrics:
  "custom.googleapis.com/business_kpi_counter":
    kind: business_kpi
    unit: items
    better_direction: up
  "custom.googleapis.com/players_count":
    kind: business_kpi
    unit: players
    better_direction: up
    aggregation:
      group_by: [metric.labels.game_id]
      within_group: max
      across_groups: sum
  "custom.googleapis.com/latency_histogram":
    kind: latency
    unit: s
    better_direction: down
  "custom.googleapis.com/cache_hit_ratio":
    kind: business_kpi
    unit: ratio
    better_direction: up
    aggregation:
      across_groups: mean
`

func testSnapshotDefaultBusinessKPISum(t *testing.T) {
	metricType := "custom.googleapis.com/business_kpi_counter"
	registry := loadTestRegistry(t, aggregationTestRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds[metricType] = "GAUGE"
	fq.valueTypes[metricType] = "INT64"
	fq.aggregatedQueryFn = fixedAggregatedSeries(metricType, 42.0)

	runAggregationSnapshot(t, fq, registry, metricType)

	require.NotEmpty(t, fq.aggregatedSpecs)
	assert.Equal(t, metrics.ReducerSum, fq.aggregatedSpecs[0].AcrossGroups)
	assert.False(t, fq.aggregatedSpecs[0].IsTwoStage())
}

func testSnapshotExplicitTwoStage(t *testing.T) {
	metricType := "custom.googleapis.com/players_count"
	registry := loadTestRegistry(t, aggregationTestRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds[metricType] = "GAUGE"
	fq.valueTypes[metricType] = "INT64"
	fq.aggregatedQueryFn = fixedAggregatedSeries(metricType, 150.0)

	runAggregationSnapshot(t, fq, registry, metricType)

	require.NotEmpty(t, fq.aggregatedSpecs)
	spec := fq.aggregatedSpecs[0]
	assert.True(t, spec.IsTwoStage())
	assert.Equal(t, metrics.ReducerMax, spec.WithinGroup)
	assert.Equal(t, metrics.ReducerSum, spec.AcrossGroups)
	assert.Equal(t, []string{"metric.labels.game_id"}, spec.GroupBy)
}

func testSnapshotLatencyMean(t *testing.T) {
	metricType := "custom.googleapis.com/latency_histogram"
	registry := loadTestRegistry(t, aggregationTestRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds[metricType] = "DELTA"
	fq.valueTypes[metricType] = "DISTRIBUTION"
	fq.aggregatedQueryFn = fixedAggregatedSeries(metricType, 0.25)

	runAggregationSnapshot(t, fq, registry, metricType)

	assert.Equal(t, metrics.ReducerMean, fq.aggregatedSpecs[0].AcrossGroups)
}

func testSnapshotRatioOverride(t *testing.T) {
	metricType := "custom.googleapis.com/cache_hit_ratio"
	registry := loadTestRegistry(t, aggregationTestRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds[metricType] = "GAUGE"
	fq.valueTypes[metricType] = "DOUBLE"
	fq.aggregatedQueryFn = fixedAggregatedSeries(metricType, 0.93)

	runAggregationSnapshot(t, fq, registry, metricType)

	assert.Equal(t, metrics.ReducerMean, fq.aggregatedSpecs[0].AcrossGroups)
}

// fixedAggregatedSeries returns an aggregatedQueryFn that fabricates a
// single series with enough points to clear metrics.Process's minimum-
// reliable-point threshold. Each point carries the given value and is
// stamped at 10-second intervals ending at now(); both current and
// baseline windows fall into the generated range.
func fixedAggregatedSeries(_ string, value float64) func(gcpdata.QueryTimeSeriesParams, metrics.AggregationSpec) ([]gcpdata.MetricTimeSeries, gcpdata.AggregationWarnings, error) {
	return func(params gcpdata.QueryTimeSeriesParams, _ metrics.AggregationSpec) ([]gcpdata.MetricTimeSeries, gcpdata.AggregationWarnings, error) {
		const pointsPerWindow = 60
		points := make([]metrics.Point, pointsPerWindow)
		step := params.End.Sub(params.Start) / pointsPerWindow
		if step <= 0 {
			step = time.Second
		}
		for i := 0; i < pointsPerWindow; i++ {
			points[i] = metrics.Point{
				Timestamp: params.Start.Add(step * time.Duration(i+1)),
				Value:     value,
			}
		}
		return []gcpdata.MetricTimeSeries{{
			MetricKind: params.MetricKind,
			ValueType:  params.ValueType,
			Points:     points,
		}}, gcpdata.AggregationWarnings{}, nil
	}
}

// testSnapshotPreEventSpecThreading verifies that both the current-window query
// and the pre_event baseline query receive the SAME AggregationSpec. The
// invariant is that mixing reducers across the two halves of a comparison makes
// the delta meaningless (e.g. current as sum, baseline as mean).
func testSnapshotPreEventSpecThreading(t *testing.T) {
	metricType := "custom.googleapis.com/players_count"
	registry := loadTestRegistry(t, aggregationTestRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds[metricType] = "GAUGE"
	fq.valueTypes[metricType] = "INT64"
	fq.aggregatedQueryFn = fixedAggregatedSeries(metricType, 100.0)

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsSnapshot(fq, registry, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics.snapshot", map[string]any{
		"metric_type":   metricType,
		"project_id":    "test-project",
		"window":        "15m",
		"baseline_mode": "pre_event",
		"event_time":    time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339),
	})
	require.NoErrorf(t, err, "callTool: %v", err)
	var snap MetricSnapshotResult
	parseResult(t, result, &snap)

	// pre_event should produce at least 2 queries: current window + pre_event baseline.
	require.GreaterOrEqual(t, len(fq.aggregatedSpecs), 2, "expected >=2 aggregated queries (current + pre_event), got %d", len(fq.aggregatedSpecs))
	first := fq.aggregatedSpecs[0]
	for i, spec := range fq.aggregatedSpecs {
		assert.True(t, specEqual(spec, first), "aggregatedSpecs[%d] = %+v, want %+v (current and pre_event baseline must share spec)", i, spec, first)
	}
	assert.Equal(t, "pre_event", snap.BaselineMode)
}

// testSnapshotSameWeekdayHourSpecThreading verifies that ALL queries issued
// for same_weekday_hour baseline (1 current + 4 weekly) carry the same spec.
// Regression guard: a bug that re-resolves the spec inside the goroutine closure
// (e.g. after a refactor that moves the ResolveAggregation call) would produce
// different specs for different weeks.
func testSnapshotSameWeekdayHourSpecThreading(t *testing.T) {
	metricType := "custom.googleapis.com/players_count" // two-stage spec
	registry := loadTestRegistry(t, aggregationTestRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds[metricType] = "GAUGE"
	fq.valueTypes[metricType] = "INT64"
	fq.aggregatedQueryFn = fixedAggregatedSeries(metricType, 200.0)

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsSnapshot(fq, registry, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics.snapshot", map[string]any{
		"metric_type":   metricType,
		"project_id":    "test-project",
		"window":        "15m",
		"baseline_mode": "same_weekday_hour",
	})
	require.NoErrorf(t, err, "callTool: %v", err)
	var snap MetricSnapshotResult
	parseResult(t, result, &snap)

	// same_weekday_hour issues 1 current + 4 weekly baseline queries = 5 total.
	require.GreaterOrEqual(t, len(fq.aggregatedSpecs), 5, "expected >=5 aggregated queries (1 current + 4 weekly), got %d", len(fq.aggregatedSpecs))
	first := fq.aggregatedSpecs[0]
	for i, spec := range fq.aggregatedSpecs {
		assert.True(t, specEqual(spec, first), "aggregatedSpecs[%d] = %+v, want %+v (all weekly queries must share spec)", i, spec, first)
	}
	// Spot-check that the spec is the two-stage one from the registry.
	assert.True(t, first.IsTwoStage(), "expected two-stage spec (players_count has group_by), got: %+v", first)
	assert.Equal(t, "same_weekday_hour", snap.BaselineMode)
}

// TestSnapshotAggregationWarningsDoNotBreakResult verifies that non-zero
// AggregationWarnings returned by QueryTimeSeriesAggregated flow through
// the handler without preventing a valid result. This exercises the
// logAggregationWarnings → mcpLog plumbing: if the warning path panicked
// or short-circuited the handler, the result would be an error.
// Note: capturing the actual MCP log notification on the client side would
// require registering a LoggingHandler on the client — this test guards
// the code path without verifying the notification payload.
func TestSnapshotAggregationWarningsDoNotBreakResult(t *testing.T) {
	metricType := "custom.googleapis.com/business_kpi_counter"
	registry := loadTestRegistry(t, aggregationTestRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds[metricType] = "GAUGE"
	fq.valueTypes[metricType] = "INT64"
	// Populate .series (not aggregatedQueryFn) so the default QueryTimeSeriesAggregated
	// path is used, which propagates fq.aggregatedWarnings to the handler.
	fq.series[metricType] = []gcpdata.MetricTimeSeries{
		makeTimeSeries(time.Now().Add(-1*time.Hour), []float64{10, 20, 30, 40, 50, 60, 70, 80}),
	}
	// Set SingleGroup, DepartedGroupBuckets, and CarryForwardBuckets to cover
	// all three message-producing branches of aggregationWarningMessages (and
	// thus logAggregationWarnings).
	fq.aggregatedWarnings = gcpdata.AggregationWarnings{
		SingleGroup:          true,
		GroupCount:           1,
		DepartedGroupBuckets: 2,
		DepartedSeries:       1,
		CarryForwardBuckets:  3,
		TotalBuckets:         60,
	}

	snap := runAggregationSnapshot(t, fq, registry, metricType)
	assert.False(t, snap.NoData, "expected valid snapshot, got no_data=true")
	assert.NotEmpty(t, snap.Classification, "classification must not be empty — safeClassification guard may have been dropped")
}

func runAggregationSnapshot(t *testing.T, fq *fakeQuerier, registry *metrics.Registry, metricType string) *MetricSnapshotResult {
	t.Helper()
	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsSnapshot(fq, registry, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics.snapshot", map[string]any{
		"metric_type":   metricType,
		"project_id":    "test-project",
		"window":        "15m",
		"baseline_mode": "prev_window",
	})
	require.NoErrorf(t, err, "callTool: %v", err)
	var snap MetricSnapshotResult
	parseResult(t, result, &snap)
	return &snap
}
