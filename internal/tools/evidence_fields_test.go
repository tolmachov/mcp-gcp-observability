package tools

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

// TestSnapshotEvidenceFields verifies that SignalFeatures evidence fields are
// propagated into MetricSnapshotResult for each classification scenario.
// Each subtest uses a different signal pattern to trigger a specific feature.
func TestSnapshotEvidenceFields(t *testing.T) {
	now := time.Now().UTC()

	tests := []struct {
		name     string
		current  []float64
		baseline []float64
		check    func(t *testing.T, snap MetricSnapshotResult)
	}{
		{
			name:     "stddev_and_cv",
			current:  stableValues(60, 0.50), // has small deterministic variance
			baseline: stableValues(60, 0.49),
			check: func(t *testing.T, snap MetricSnapshotResult) {
				assert.Greater(t, snap.Stddev, 0.0, "stddev should be populated for a non-constant signal")
				assert.Greater(t, snap.CV, 0.0, "cv should be populated for a non-constant signal")
			},
		},
		{
			name:     "trend_score",
			current:  makeRisingValues(60, 0.10, 0.90), // clear upward trend
			baseline: stableValues(60, 0.30),
			check: func(t *testing.T, snap MetricSnapshotResult) {
				assert.NotEqual(t, 0.0, snap.TrendScore, "trend_score should be set for a clearly rising signal")
			},
		},
		{
			// 55 points at 0.5, 5 points at 5.0 → z ≈ 3.3 (> default SpikeZScore=3.0 for resource_utilization).
			name:     "spike_fields",
			current:  makeSpikyValues(60, 5, 0.5, 5.0),
			baseline: stableValues(60, 0.50),
			check: func(t *testing.T, snap MetricSnapshotResult) {
				assert.Greater(t, snap.MaxZScore, 0.0, "max_z_score should be set for a signal with outliers")
				assert.Greater(t, snap.SpikeCount, 0, "spike_count should be nonzero for points exceeding the z-threshold")
				assert.Greater(t, snap.SpikeRatio, 0.0, "spike_ratio should be set when spike_count > 0")
			},
		},
		{
			name:     "step_change_pct",
			current:  makeStepValues(60, 0.10, 0.80), // clear jump at midpoint
			baseline: stableValues(60, 0.10),
			check: func(t *testing.T, snap MetricSnapshotResult) {
				assert.NotEqual(t, 0.0, snap.StepChangePct, "step_change_pct should reflect the jump in the signal")
			},
		},
		{
			// cpu/utilization has slo_threshold=0.8, better_direction=down.
			// Alternating 0.60/0.90 crosses the threshold every point → many transitions.
			name:     "breach_transitions",
			current:  makeAlternatingValues(60, 0.60, 0.90),
			baseline: stableValues(60, 0.50),
			check: func(t *testing.T, snap MetricSnapshotResult) {
				assert.True(t, snap.SLOBreach, "SLOBreach should be true when signal crosses the threshold")
				assert.Greater(t, snap.BreachTransitions, 0, "breach_transitions should count SLO threshold crossings")
			},
		},
		{
			// cpu/utilization has saturation_cap=1.0; 0.99 is within the 5% window.
			name:     "saturation_detected",
			current:  stableValues(60, 0.99),
			baseline: stableValues(60, 0.50),
			check: func(t *testing.T, snap MetricSnapshotResult) {
				assert.True(t, snap.SaturationDetected, "saturation_detected should be true when signal is within 5%% of saturation_cap")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg := loadTestRegistry(t, testRegistryYAML)
			fq := newFakeQuerier()
			fq.metricKinds[cpuMetric] = "GAUGE"
			fq.seriesFunc = snapshotSeriesFunc(now, time.Hour, tc.current, tc.baseline)

			ctx := context.Background()
			ts := newTestToolServer(t)
			ts.registerMetricsSnapshot(fq, reg, "test-project")
			ts.connect(ctx)
			defer ts.close()

			result, err := ts.callTool(ctx, "metrics_snapshot", map[string]any{"metric_type": cpuMetric})
			require.NoError(t, err)

			var snap MetricSnapshotResult
			parseResult(t, result, &snap)
			tc.check(t, snap)
		})
	}
}

// TestCompareEvidenceFields verifies that evidence fields are propagated into
// CompareResult. Each subtest uses a different signal pattern.
func TestCompareEvidenceFields(t *testing.T) {
	now := time.Now().UTC()
	aFrom := now.Add(-2 * time.Hour)
	aTo := now.Add(-1 * time.Hour)
	bFrom := now.Add(-1 * time.Hour)
	bTo := now

	tests := []struct {
		name      string
		seriesFor func(params gcpdata.QueryTimeSeriesParams) []gcpdata.MetricTimeSeries
		check     func(t *testing.T, cmp CompareResult)
	}{
		{
			name: "trend_scores",
			seriesFor: func(params gcpdata.QueryTimeSeriesParams) []gcpdata.MetricTimeSeries {
				return []gcpdata.MetricTimeSeries{makeTimeSeries(params.Start, makeRisingValues(60, 0.10, 0.80))}
			},
			check: func(t *testing.T, cmp CompareResult) {
				assert.NotEqual(t, 0.0, cmp.TrendScoreA, "trend_score_a should be set for a trending window A")
				assert.NotEqual(t, 0.0, cmp.TrendScoreB, "trend_score_b should be set for a trending window B")
			},
		},
		{
			// Window B (starts at bFrom = now-1h) gets a step; window A gets stable values.
			// The routing condition (params.Start > aTo-30min) separates the two windows:
			// window A starts at now-2h (before the cutoff), window B at now-1h (after).
			name: "step_change_pct",
			seriesFor: func(params gcpdata.QueryTimeSeriesParams) []gcpdata.MetricTimeSeries {
				if params.Start.After(aTo.Add(-30 * time.Minute)) {
					return []gcpdata.MetricTimeSeries{makeTimeSeries(params.Start, makeStepValues(60, 0.10, 0.80))}
				}
				return []gcpdata.MetricTimeSeries{makeTimeSeries(params.Start, stableValues(60, 0.10))}
			},
			check: func(t *testing.T, cmp CompareResult) {
				assert.NotEqual(t, 0.0, cmp.StepChangePct, "step_change_pct should reflect the step in window B")
				// Window A received stable data, so fA.StepChangePct ≈ 0. If the
				// implementation accidentally used fA instead of fB, this assertion
				// would fail. A 0.10→0.80 step in window B yields ~700%.
				assert.Greater(t, cmp.StepChangePct, 100.0, "step_change_pct magnitude should be consistent with window B's 0.10→0.80 step")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg := loadTestRegistry(t, testRegistryYAML)
			fq := newFakeQuerier()
			fq.metricKinds[cpuMetric] = "GAUGE"
			fq.seriesFunc = tc.seriesFor

			ctx := context.Background()
			ts := newTestToolServer(t)
			ts.registerMetricsCompare(fq, reg, "test-project")
			ts.connect(ctx)
			defer ts.close()

			result, err := ts.callTool(ctx, "metrics_compare", map[string]any{
				"metric_type":   cpuMetric,
				"window_a_from": aFrom.Format(time.RFC3339),
				"window_a_to":   aTo.Format(time.RFC3339),
				"window_b_from": bFrom.Format(time.RFC3339),
				"window_b_to":   bTo.Format(time.RFC3339),
			})
			require.NoError(t, err)

			var cmp CompareResult
			parseResult(t, result, &cmp)
			tc.check(t, cmp)
		})
	}
}

// TestRelatedSignalEvidence_TrendScoreCV verifies that trend_score and cv are
// propagated into RelatedSignal for the configured related metric.
func TestRelatedSignalEvidence_TrendScoreCV(t *testing.T) {
	const relatedMetric = "compute.googleapis.com/instance/memory/utilization"

	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds[cpuMetric] = "GAUGE"
	fq.metricKinds[relatedMetric] = "GAUGE"
	fq.seriesFunc = func(params gcpdata.QueryTimeSeriesParams) []gcpdata.MetricTimeSeries {
		return []gcpdata.MetricTimeSeries{makeTimeSeries(params.Start, makeRisingValues(60, 0.10, 0.80))}
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsRelated(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_related", map[string]any{"metric_type": cpuMetric})
	require.NoError(t, err)

	var res RelatedSignalsResult
	parseResult(t, result, &res)
	require.NotEmpty(t, res.RelatedSignals, "expected at least one related signal")

	var found *RelatedSignal
	for i := range res.RelatedSignals {
		if res.RelatedSignals[i].MetricType == relatedMetric {
			found = &res.RelatedSignals[i]
			break
		}
	}
	require.NotNilf(t, found, "expected %s in related signals", relatedMetric)
	assert.NotEqual(t, 0.0, found.TrendScore, "trend_score should be set for a rising signal")
	assert.Greater(t, found.CV, 0.0, "cv should be populated for a signal with variance")
}

// TestContributorEvidence_CV verifies that cv is propagated into Contributor
// for time series that have variance.
func TestContributorEvidence_CV(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds[cpuMetric] = "GAUGE"

	now := time.Now().UTC()
	base := now.Add(-time.Hour)

	current := []gcpdata.MetricTimeSeries{
		makeTimeSeriesWithLabels(base, stableValues(60, 0.50), map[string]string{"instance_id": "inst-a"}),
		makeTimeSeriesWithLabels(base, makeRisingValues(60, 0.30, 0.70), map[string]string{"instance_id": "inst-b"}),
	}
	baseline := []gcpdata.MetricTimeSeries{
		makeTimeSeriesWithLabels(base.Add(-time.Hour), stableValues(60, 0.45), map[string]string{"instance_id": "inst-a"}),
		makeTimeSeriesWithLabels(base.Add(-time.Hour), stableValues(60, 0.40), map[string]string{"instance_id": "inst-b"}),
	}

	fq.seriesFunc = func(params gcpdata.QueryTimeSeriesParams) []gcpdata.MetricTimeSeries {
		if params.End.After(now.Add(-30 * time.Minute)) {
			return current
		}
		return baseline
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsTop(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_top_contributors", map[string]any{
		"metric_type": cpuMetric,
		"dimension":   "metric.labels.instance_id",
	})
	require.NoError(t, err)

	var top TopContributorsResult
	parseResult(t, result, &top)
	require.Len(t, top.Contributors, 2, "expected both contributors")

	var instA, instB *Contributor
	for i := range top.Contributors {
		switch top.Contributors[i].LabelValue {
		case "inst-a":
			instA = &top.Contributors[i]
		case "inst-b":
			instB = &top.Contributors[i]
		}
	}
	require.NotNil(t, instA, "expected contributor inst-a")
	require.NotNil(t, instB, "expected contributor inst-b")
	// inst-b has a rising signal (higher variance) so its CV should exceed inst-a's (stable).
	assert.Greater(t, instB.CV, instA.CV, "inst-b (rising signal) should have higher CV than inst-a (stable signal)")
}
