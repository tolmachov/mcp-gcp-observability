package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// TestRelatedAggregationResolve is the compare/related analog of
// TestSnapshotAggregationResolve. metrics.related looks up each correlated
// metric's OWN aggregation spec (they may be different kinds — a latency
// histogram next to a business_kpi counter), so the critical regression
// to guard against is one related metric silently inheriting the
// primary's reducer or the default falling back to mean for a kind that
// should be sum.
func TestRelatedAggregationResolve(t *testing.T) {
	t.Run("Positive: related two-stage metric carries its own spec", testRelatedExplicitTwoStage)
}

// relatedAggregationRegistryYAML wires the primary metric with a single
// related entry that is the two-stage players_count from the shared
// aggregationTestRegistryYAML. The primary uses the default single-stage
// business_kpi spec; the related must still resolve to two-stage or the
// regression is hidden.
const relatedAggregationRegistryYAML = `metrics:
  "custom.googleapis.com/business_kpi_primary":
    kind: business_kpi
    unit: items
    better_direction: up
    related_metrics:
      - "custom.googleapis.com/players_count"
  "custom.googleapis.com/players_count":
    kind: business_kpi
    unit: players
    better_direction: up
    aggregation:
      group_by: [metric.labels.game_id]
      within_group: max
      across_groups: sum
`

func testRelatedExplicitTwoStage(t *testing.T) {
	primary := "custom.googleapis.com/business_kpi_primary"
	related := "custom.googleapis.com/players_count"
	registry := loadTestRegistry(t, relatedAggregationRegistryYAML)

	fq := newFakeQuerier()
	fq.metricKinds[primary] = "GAUGE"
	fq.valueTypes[primary] = "INT64"
	fq.metricKinds[related] = "GAUGE"
	fq.valueTypes[related] = "INT64"
	// Same aggregated-query fake as the snapshot test — it ignores the
	// metric type and returns a flat series with a fixed value. Good
	// enough for this test because we only assert the AggregationSpec
	// recorded in fq.aggregatedSpecs, not the numerical output.
	fq.aggregatedQueryFn = fixedAggregatedSeries(related, 150.0)

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsRelated(fq, registry, "test-project")
	ts.connect(ctx)
	defer ts.close()

	_, err := ts.callTool(ctx, "metrics.related", map[string]any{
		"metric_type": primary,
		"project_id":  "test-project",
		"window":      "15m",
	})
	require.NoError(t, err, "callTool")

	// metrics.related fires two queries per related metric (current +
	// baseline). Both must carry the two-stage spec.
	require.GreaterOrEqual(t, len(fq.aggregatedSpecs), 2, "expected ≥2 aggregated calls")
	for i, spec := range fq.aggregatedSpecs {
		assert.True(t, spec.IsTwoStage(), "call %d: spec should be two-stage", i)
		assert.Equal(t, metrics.ReducerMax, spec.WithinGroup, "call %d: WithinGroup should be max", i)
		assert.Equal(t, metrics.ReducerSum, spec.AcrossGroups, "call %d: AcrossGroups should be sum", i)
	}
}
