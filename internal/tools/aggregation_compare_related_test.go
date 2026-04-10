package tools

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// TestCompareSameSpecBothWindows guards the core invariant of metrics.compare:
// both A and B windows MUST be queried with the SAME AggregationSpec, or the
// delta between them becomes meaningless (e.g. sum vs mean). A future
// refactor that re-resolves per window would silently ship without this
// assertion.
func TestCompareSameSpecBothWindows(t *testing.T) {
	const metricType = "custom.googleapis.com/players_count"
	registry := loadTestRegistry(t, aggregationTestRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds[metricType] = "GAUGE"
	fq.valueTypes[metricType] = "INT64"
	fq.aggregatedQueryFn = fixedAggregatedSeries(metricType, 150.0)

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsCompare(fq, registry, "test-project")
	ts.connect(ctx)
	defer ts.close()

	now := time.Now().UTC()
	_, err := ts.callTool(ctx, "metrics.compare", map[string]any{
		"metric_type":   metricType,
		"window_a_from": now.Add(-2 * time.Hour).Format(time.RFC3339),
		"window_a_to":   now.Add(-1 * time.Hour).Format(time.RFC3339),
		"window_b_from": now.Add(-1 * time.Hour).Format(time.RFC3339),
		"window_b_to":   now.Format(time.RFC3339),
	})
	require.NoError(t, err, "callTool")

	require.GreaterOrEqual(t, len(fq.aggregatedSpecs), 2, "expected at least 2 aggregated queries (A and B)")
	first := fq.aggregatedSpecs[0]
	for i, spec := range fq.aggregatedSpecs {
		assert.True(t, specEqual(spec, first), "aggregatedSpecs[%d] should match first spec (all windows must share spec)", i)
	}
	// The registered spec is two-stage max/sum — verify that is what flowed
	// through, not some accidentally-reset default.
	assert.True(t, first.IsTwoStage() && first.WithinGroup == metrics.ReducerMax && first.AcrossGroups == metrics.ReducerSum, "resolved spec should be two-stage max/sum")
}

// TestRelatedPerMetricSpecResolution guards that metrics.related resolves an
// AggregationSpec per related metric (not a shared strategy). A latency
// histogram (→ mean by default) next to a business_kpi counter (→ sum by
// default) should produce BOTH reducers in aggregatedSpecs. Requiring both
// catches the regression class where related_metrics accidentally share a
// single pre-resolved spec — we would see only one reducer flavor.
func TestRelatedPerMetricSpecResolution(t *testing.T) {
	const primary = "custom.googleapis.com/primary_counter"
	const relatedLatency = "custom.googleapis.com/related_latency"
	const relatedCounter = "custom.googleapis.com/related_counter"
	registryYAML := `metrics:
  "` + primary + `":
    kind: business_kpi
    unit: items
    better_direction: up
    related_metrics:
      - "` + relatedLatency + `"
      - "` + relatedCounter + `"
  "` + relatedLatency + `":
    kind: latency
    unit: s
    better_direction: down
  "` + relatedCounter + `":
    kind: business_kpi
    unit: events
    better_direction: up
`
	registry := loadTestRegistry(t, registryYAML)
	fq := newFakeQuerier()
	fq.metricKinds[primary] = "GAUGE"
	fq.valueTypes[primary] = "INT64"
	fq.metricKinds[relatedLatency] = "DELTA"
	fq.valueTypes[relatedLatency] = "DISTRIBUTION"
	fq.metricKinds[relatedCounter] = "DELTA"
	fq.valueTypes[relatedCounter] = "INT64"
	// Return the same fabricated series regardless of metric — we only
	// care which specs flow into the querier.
	fq.aggregatedQueryFn = fixedAggregatedSeries("", 1.0)

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsRelated(fq, registry, "test-project")
	ts.connect(ctx)
	defer ts.close()

	_, err := ts.callTool(ctx, "metrics.related", map[string]any{
		"metric_type": primary,
	})
	require.NoError(t, err, "callTool")

	// related queries each related metric for current + baseline, so with
	// two related metrics we expect at least 4 aggregated queries.
	require.GreaterOrEqual(t, len(fq.aggregatedSpecs), 4, "expected >=4 aggregated queries (2 metrics × (current+baseline))")

	// Both reducers MUST show up — sawSum confirms the counter's default
	// flowed through, sawMean confirms the latency histogram's default.
	// A regression that shares one spec across all related metrics would
	// only ever produce one flavor.
	var sawMean, sawSum bool
	for _, spec := range fq.aggregatedSpecs {
		switch spec.AcrossGroups {
		case metrics.ReducerMean:
			sawMean = true
		case metrics.ReducerSum:
			sawSum = true
		}
	}
	assert.True(t, sawMean, "expected at least one mean spec (latency default)")
	assert.True(t, sawSum, "expected at least one sum spec (business_kpi counter default)")

	// Within EACH related metric, current and baseline must share the same
	// spec (same-reducer-on-both-sides invariant). Group by reducer and
	// assert every spec in each group is identical.
	groups := make(map[metrics.Reducer][]metrics.AggregationSpec)
	for _, spec := range fq.aggregatedSpecs {
		groups[spec.AcrossGroups] = append(groups[spec.AcrossGroups], spec)
	}
	for reducer, specs := range groups {
		first := specs[0]
		for i, spec := range specs {
			assert.True(t, specEqual(spec, first), "reducer %v specs[%d] should diverge (current/baseline must match)", reducer, i)
		}
	}
}

// TestSnapshotAllSpecsEqual strengthens the existing snapshot coverage: not
// just aggregatedSpecs[0], but EVERY recorded spec for a single-metric call
// must be identical. Snapshot resolves the spec once and threads it through
// current + baseline, so divergence would be a regression.
func TestSnapshotAllSpecsEqual(t *testing.T) {
	metricType := "custom.googleapis.com/players_count"
	registry := loadTestRegistry(t, aggregationTestRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds[metricType] = "GAUGE"
	fq.valueTypes[metricType] = "INT64"
	fq.aggregatedQueryFn = fixedAggregatedSeries(metricType, 100.0)

	runAggregationSnapshot(t, fq, registry, metricType)

	require.GreaterOrEqual(t, len(fq.aggregatedSpecs), 2, "expected >=2 aggregated queries (current + baseline)")
	first := fq.aggregatedSpecs[0]
	for i, spec := range fq.aggregatedSpecs {
		assert.True(t, specEqual(spec, first), "aggregatedSpecs[%d] should match first spec", i)
	}
}

// specEqual compares two AggregationSpec values field-by-field. We can't use
// == because AggregationSpec contains a []string slice.
func specEqual(a, b metrics.AggregationSpec) bool {
	if a.WithinGroup != b.WithinGroup || a.AcrossGroups != b.AcrossGroups {
		return false
	}
	if len(a.GroupBy) != len(b.GroupBy) {
		return false
	}
	for i := range a.GroupBy {
		if a.GroupBy[i] != b.GroupBy[i] {
			return false
		}
	}
	return true
}
