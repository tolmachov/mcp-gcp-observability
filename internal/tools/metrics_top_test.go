package tools

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

func TestSplitDimension(t *testing.T) {
	tests := []struct {
		input      string
		wantPrefix string
		wantKey    string
	}{
		{"metric.labels.response_code", "metric", "response_code"},
		{"resource.labels.instance_id", "resource", "instance_id"},
		// Metadata namespaces exist so top_contributors can break down by
		// GCE system metadata (machine_type) or user-supplied labels (env).
		// A typo in these prefixes must NOT silently fall back to the bare
		// key — the test locks the four accepted prefixes.
		{"metadata.system_labels.machine_type", "metadata_system", "machine_type"},
		{"metadata.user_labels.env", "metadata_user", "env"},
		{"response_code", "", "response_code"},
		{"metric.labels.", "", "metric.labels."}, // malformed: treated as bare key
		{"resource.labels.zone", "resource", "zone"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			p := splitDimension(tt.input)
			assert.Equal(t, tt.wantPrefix, p.prefix)
			assert.Equal(t, tt.wantKey, p.key)
		})
	}
}

// TestLabelValueFromSeries_MetadataNamespaces verifies that the four
// supported dimension shapes all resolve to the correct label map on a
// MetricTimeSeries. Regression guard: swapping the switch arms for the two
// metadata namespaces used to compile and pass every existing test.
func TestLabelValueFromSeries_MetadataNamespaces(t *testing.T) {
	ts := gcpdata.MetricTimeSeries{
		MetricLabels:         map[string]string{"response_code": "200", "instance_id": "i-metric"},
		ResourceLabels:       map[string]string{"zone": "us-central1-a"},
		MetadataSystemLabels: map[string]string{"machine_type": "e2-medium"},
		MetadataUserLabels:   map[string]string{"env": "prod"},
	}

	tests := []struct {
		dimension string
		want      string
	}{
		{"metric.labels.response_code", "200"},
		{"resource.labels.zone", "us-central1-a"},
		{"metadata.system_labels.machine_type", "e2-medium"},
		{"metadata.user_labels.env", "prod"},
		// Unprefixed: fall back to every namespace until found.
		{"machine_type", "e2-medium"},
		{"instance_id", "i-metric"},
		// Missing everywhere: missing-dimension sentinel (distinct from ""
		// which means "label present, value empty").
		{"metric.labels.bogus", "(missing_dimension)"},
		{"resource.labels.instance_id", "(missing_dimension)"},
	}

	for _, tt := range tests {
		t.Run(tt.dimension, func(t *testing.T) {
			got := labelValueFromSeries(ts, tt.dimension)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestTopContributorsReducerFromRegistry locks the cache_hit_ratio
// regression fix: metrics.top_contributors must drive the cross-series
// reducer from the registry's resolved AggregationSpec, not from kind
// alone. A regression that hardcoded REDUCE_MEAN or reverted to the
// kind-based heuristic would silently change per-contributor numbers
// for ratio metrics — exactly the failure mode the PR exists to fix.
func TestTopContributorsReducerFromRegistry(t *testing.T) {
	cases := []struct {
		name        string
		metricType  string
		wantReducer monitoringpb.Aggregation_Reducer
	}{
		{
			// business_kpi with explicit `across_groups: mean` override.
			// Pre-fix code returned REDUCE_MEAN by the kind-based default
			// (KindBusinessKPI is NOT in the throughput/error_rate sum
			// branch), so this case happens to coincide with the old
			// hardcoded mean. The next case below catches the override
			// path that DID change.
			name:        "ratio override → mean",
			metricType:  "custom.googleapis.com/cache_hit_ratio",
			wantReducer: monitoringpb.Aggregation_REDUCE_MEAN,
		},
		{
			// business_kpi counter without override → SUM by registry
			// default. Pre-fix code returned REDUCE_MEAN here (the kind
			// heuristic only escalated throughput/error_rate to SUM, not
			// business_kpi). This is the case the fix actually changes.
			name:        "business_kpi counter → sum",
			metricType:  "custom.googleapis.com/business_kpi_counter",
			wantReducer: monitoringpb.Aggregation_REDUCE_SUM,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registry := loadTestRegistry(t, aggregationTestRegistryYAML)
			fq := newFakeQuerier()
			fq.metricKinds[tc.metricType] = "GAUGE"
			fq.valueTypes[tc.metricType] = "DOUBLE"
			fq.series[tc.metricType] = []gcpdata.MetricTimeSeries{
				makeTimeSeriesWithLabels(time.Now().Add(-30*time.Minute), []float64{1, 2, 3, 4, 5},
					map[string]string{"response_code": "200"}),
			}

			ctx := context.Background()
			tts := newTestToolServer(t)
			tts.registerMetricsTop(fq, registry, "test-project")
			tts.connect(ctx)
			defer tts.close()

			_, err := tts.callTool(ctx, "metrics.top_contributors", map[string]any{
				"metric_type": tc.metricType,
				"dimension":   "metric.labels.response_code",
				"window":      "15m",
			})
			require.NoError(t, err)

			require.NotEmpty(t, fq.queryLog, "handler did not call QueryTimeSeries")
			got := fq.queryLog[0].Reducer
			assert.Equal(t, tc.wantReducer, got)
		})
	}
}

// TestTopContributorsTwoStageDoesNotCrash locks the divergence-warning
// contract: when the registry uses two-stage aggregation, top_contributors
// drops the WithinGroup dedup stage and operators must see a warning so
// they know per-contributor totals may differ from snapshot/compare.
func TestTopContributorsTwoStageDoesNotCrash(t *testing.T) {
	const metricType = "custom.googleapis.com/players_count"
	registry := loadTestRegistry(t, aggregationTestRegistryYAML)

	// Sanity-check: the registry entry IS two-stage. If a future cleanup
	// removes the explicit aggregation block from aggregationTestRegistryYAML
	// this test silently no-ops, so assert the precondition explicitly.
	meta := registry.Lookup(metricType)
	require.True(t, meta.ResolveAggregation().IsTwoStage(), "test precondition broken: %s is not two-stage in the test registry", metricType)

	fq := newFakeQuerier()
	fq.metricKinds[metricType] = "GAUGE"
	fq.valueTypes[metricType] = "INT64"
	fq.series[metricType] = []gcpdata.MetricTimeSeries{
		makeTimeSeriesWithLabels(time.Now().Add(-30*time.Minute), []float64{10, 20, 30, 40, 50},
			map[string]string{"game_id": "g1"}),
	}

	ctx := context.Background()
	tts := newTestToolServer(t)
	tts.registerMetricsTop(fq, registry, "test-project")
	tts.connect(ctx)
	defer tts.close()

	result, err := tts.callTool(ctx, "metrics.top_contributors", map[string]any{
		"metric_type": metricType,
		"dimension":   "metric.labels.game_id",
		"window":      "15m",
	})
	require.NoError(t, err)
	require.False(t, result.IsError, "expected success, got error: %+v", result)

	// Assert the reducer that flowed through is AcrossGroups (sum), not
	// WithinGroup (max) — the two-stage spec collapses to single-stage
	// for top_contributors and AcrossGroups is the load-bearing field.
	require.NotEmpty(t, fq.queryLog, "handler did not call QueryTimeSeries")
	assert.Equal(t, monitoringpb.Aggregation_REDUCE_SUM, fq.queryLog[0].Reducer)

	var top TopContributorsResult
	unmarshalResult(t, result, &top)
	assert.Contains(t, top.Note, "two-stage aggregation")
	require.NotEmpty(t, top.Contributors, "expected at least one contributor, got none")
	// The resolved spec must still be flagged two-stage, otherwise the
	// warning code path (the silent-failure-hunter finding) is dead.
	if !meta.ResolveAggregation().IsTwoStage() {
		t.Error("post-handle: spec lost its two-stage flag — warning emit code is unreachable")
	}
	_ = metrics.ReducerSum // anchor the metrics import even if no other reference exists
}

func TestTopContributorsTruncationSentinelDoesNotMasqueradeAsMissingDimension(t *testing.T) {
	const metricType = "custom.googleapis.com/business_kpi_counter"
	registry := loadTestRegistry(t, aggregationTestRegistryYAML)

	fq := newFakeQuerier()
	fq.metricKinds[metricType] = "GAUGE"
	fq.valueTypes[metricType] = "INT64"
	fq.series[metricType] = []gcpdata.MetricTimeSeries{
		makeTimeSeriesWithLabels(time.Now().Add(-30*time.Minute), []float64{10, 20, 30, 40, 50},
			map[string]string{"response_code": "200"}),
		{Truncated: true},
	}

	ctx := context.Background()
	tts := newTestToolServer(t)
	tts.registerMetricsTop(fq, registry, "test-project")
	tts.connect(ctx)
	defer tts.close()

	result, err := tts.callTool(ctx, "metrics.top_contributors", map[string]any{
		"metric_type": metricType,
		"dimension":   "metric.labels.response_code",
		"window":      "15m",
	})
	require.NoError(t, err)
	require.False(t, result.IsError, "expected success, got error: %+v", result)

	var top TopContributorsResult
	unmarshalResult(t, result, &top)
	require.NotContains(t, top.Note, "Partial dimension coverage", "truncation sentinel must not look like missing-dimension coverage loss")
	require.Contains(t, top.Note, "time-series cap", "want explicit truncation warning")
	require.Len(t, top.Contributors, 1)
}

func TestTopContributorsRequiresQualifiedDimension(t *testing.T) {
	registry := loadTestRegistry(t, aggregationTestRegistryYAML)
	fq := newFakeQuerier()
	const metricType = "custom.googleapis.com/business_kpi_counter"
	fq.metricKinds[metricType] = "GAUGE"
	fq.valueTypes[metricType] = "DOUBLE"
	fq.descriptors = []gcpdata.MetricDescriptorInfo{{
		Type:       metricType,
		MetricKind: "GAUGE",
		ValueType:  "DOUBLE",
		Labels:     []gcpdata.LabelDescriptor{{Key: "response_code"}},
	}}

	ctx := context.Background()
	tts := newTestToolServer(t)
	tts.registerMetricsTop(fq, registry, "test-project")
	tts.connect(ctx)
	defer tts.close()

	result, err := tts.callTool(ctx, "metrics.top_contributors", map[string]any{
		"metric_type": metricType,
		"dimension":   "response_code",
		"window":      "15m",
	})
	require.NoError(t, err)
	require.True(t, result.IsError, "expected tool error, got success: %+v", result)
	got := textFromResult(t, result)
	assert.Contains(t, got, "fully-qualified label key")
}

func TestValidateTopContributorDimension(t *testing.T) {
	cases := []struct {
		dimension string
		wantErr   bool
	}{
		{"metric.labels.instance_id", false},
		{"resource.labels.zone", false},
		{"metadata.system_labels.machine_type", false},
		{"metadata.user_labels.env", false},
		{"instance_id", true},    // unqualified bare name
		{"metric.labels.", true}, // trailing dot, empty key
		{"metric.labels", true},  // prefix without dot separator
		{"", true},               // empty string
	}
	for _, tc := range cases {
		t.Run(tc.dimension, func(t *testing.T) {
			got := validateTopContributorDimension(tc.dimension)
			if tc.wantErr {
				assert.NotEmpty(t, got)
			} else {
				assert.Empty(t, got)
			}
		})
	}
}
