package tools

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// TestCompareAggregationResolve mirrors TestSnapshotAggregationResolve for
// the metrics_compare tool. The critical regression to guard against is
// metrics_compare.go forgetting to thread the resolved AggregationSpec
// through to QueryTimeSeriesAggregated — which would silently fall back
// to averaging and make the A vs B delta meaningless.
func TestCompareAggregationResolve(t *testing.T) {
	t.Run("Positive: explicit two-stage spec passed through to both windows", testCompareExplicitTwoStage)
	t.Run("Positive: business_kpi default is sum on both windows", testCompareDefaultBusinessKPISum)
}

func testCompareExplicitTwoStage(t *testing.T) {
	metricType := "custom.googleapis.com/players_count"
	registry := loadTestRegistry(t, aggregationTestRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds[metricType] = "GAUGE"
	fq.valueTypes[metricType] = "INT64"
	fq.aggregatedQueryFn = fixedAggregatedSeries(metricType, 150.0)

	runAggregationCompare(t, fq, registry, metricType)

	// compare issues two parallel queries — both must carry the resolved
	// two-stage spec or window A / window B will be computed with
	// different reducers and the delta becomes spurious.
	require.GreaterOrEqual(t, len(fq.aggregatedSpecs), 2)
	for _, spec := range fq.aggregatedSpecs {
		assert.True(t, spec.IsTwoStage())
		assert.Equal(t, metrics.ReducerMax, spec.WithinGroup)
		assert.Equal(t, metrics.ReducerSum, spec.AcrossGroups)
		assert.Equal(t, []string{"metric.labels.game_id"}, spec.GroupBy)
	}
}

func testCompareDefaultBusinessKPISum(t *testing.T) {
	metricType := "custom.googleapis.com/business_kpi_counter"
	registry := loadTestRegistry(t, aggregationTestRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds[metricType] = "GAUGE"
	fq.valueTypes[metricType] = "INT64"
	fq.aggregatedQueryFn = fixedAggregatedSeries(metricType, 42.0)

	runAggregationCompare(t, fq, registry, metricType)

	require.GreaterOrEqual(t, len(fq.aggregatedSpecs), 2)
	for _, spec := range fq.aggregatedSpecs {
		assert.Equal(t, metrics.ReducerSum, spec.AcrossGroups)
		assert.False(t, spec.IsTwoStage())
	}
}

func runAggregationCompare(t *testing.T, fq *fakeQuerier, registry *metrics.Registry, metricType string) {
	t.Helper()
	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsCompare(fq, registry, "test-project")
	ts.connect(ctx)
	defer ts.close()

	// Use windows that fall inside fixedAggregatedSeries' generated range.
	now := time.Now().UTC()
	windowA := now.Add(-2 * time.Hour)
	windowAEnd := now.Add(-time.Hour)
	windowB := now.Add(-time.Hour)
	windowBEnd := now
	_, err := ts.callTool(ctx, "metrics_compare", map[string]any{
		"metric_type":   metricType,
		"project_id":    "test-project",
		"window_a_from": windowA.Format(time.RFC3339),
		"window_a_to":   windowAEnd.Format(time.RFC3339),
		"window_b_from": windowB.Format(time.RFC3339),
		"window_b_to":   windowBEnd.Format(time.RFC3339),
	})
	require.NoError(t, err)
}
