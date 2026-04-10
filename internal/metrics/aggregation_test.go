package metrics

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAggregationSpec(t *testing.T) {
	t.Run("Validate", testAggregationSpecValidate)
	t.Run("IsTwoStage", testAggregationSpecIsTwoStage)
}

func testAggregationSpecValidate(t *testing.T) {
	cases := []struct {
		name    string
		spec    AggregationSpec
		wantErr bool
	}{
		{
			name:    "Positive: single-stage sum",
			spec:    AggregationSpec{AcrossGroups: ReducerSum},
			wantErr: false,
		},
		{
			name:    "Positive: single-stage mean",
			spec:    AggregationSpec{AcrossGroups: ReducerMean},
			wantErr: false,
		},
		{
			name: "Positive: two-stage max then sum",
			spec: AggregationSpec{
				GroupBy:      []string{"metric.labels.game_id"},
				WithinGroup:  ReducerMax,
				AcrossGroups: ReducerSum,
			},
			wantErr: false,
		},
		{
			name:    "Negative: empty across_groups",
			spec:    AggregationSpec{},
			wantErr: true,
		},
		{
			name:    "Negative: invalid across_groups value",
			spec:    AggregationSpec{AcrossGroups: Reducer("avg")},
			wantErr: true,
		},
		{
			name: "Negative: group_by set without within_group",
			spec: AggregationSpec{
				GroupBy:      []string{"metric.labels.game_id"},
				AcrossGroups: ReducerSum,
			},
			wantErr: true,
		},
		{
			name: "Negative: group_by with invalid within_group",
			spec: AggregationSpec{
				GroupBy:      []string{"metric.labels.game_id"},
				WithinGroup:  Reducer("bogus"),
				AcrossGroups: ReducerSum,
			},
			wantErr: true,
		},
		{
			name: "Edge: group_by with empty string key",
			spec: AggregationSpec{
				GroupBy:      []string{""},
				WithinGroup:  ReducerMax,
				AcrossGroups: ReducerSum,
			},
			wantErr: true,
		},
		{
			// The class of bug the strict validation exists to prevent:
			// a bare label name like "game_id" is silently dropped by
			// Cloud Monitoring's group_by_fields, collapsing two-stage
			// aggregation to a single group. Validate must reject it at
			// load time.
			name: "Negative: bare label name without qualifier",
			spec: AggregationSpec{
				GroupBy:      []string{"game_id"},
				WithinGroup:  ReducerMax,
				AcrossGroups: ReducerSum,
			},
			wantErr: true,
		},
		{
			// "metric.labels." with no tail is also a typo — the prefix
			// match must require a non-empty key segment after the dot.
			name: "Negative: namespace prefix with empty tail",
			spec: AggregationSpec{
				GroupBy:      []string{"metric.labels."},
				WithinGroup:  ReducerMax,
				AcrossGroups: ReducerSum,
			},
			wantErr: true,
		},
		{
			name: "Positive: resource.labels qualifier accepted",
			spec: AggregationSpec{
				GroupBy:      []string{"resource.labels.instance_id"},
				WithinGroup:  ReducerMax,
				AcrossGroups: ReducerSum,
			},
			wantErr: false,
		},
		{
			name: "Positive: resource.type exact match accepted",
			spec: AggregationSpec{
				GroupBy:      []string{"resource.type"},
				WithinGroup:  ReducerMean,
				AcrossGroups: ReducerSum,
			},
			wantErr: false,
		},
		{
			name: "Positive: metadata.system_labels qualifier accepted",
			spec: AggregationSpec{
				GroupBy:      []string{"metadata.system_labels.zone"},
				WithinGroup:  ReducerMax,
				AcrossGroups: ReducerSum,
			},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func testAggregationSpecIsTwoStage(t *testing.T) {
	single := AggregationSpec{AcrossGroups: ReducerSum}
	assert.False(t, single.IsTwoStage())
	two := AggregationSpec{
		GroupBy:      []string{"metric.labels.game_id"},
		WithinGroup:  ReducerMax,
		AcrossGroups: ReducerSum,
	}
	assert.True(t, two.IsTwoStage())
}

func TestDefaultAggregation(t *testing.T) {
	cases := []struct {
		name         string
		kind         MetricKind
		wantAcross   Reducer
		wantTwoStage bool
	}{
		{"business_kpi sums", KindBusinessKPI, ReducerSum, false},
		{"throughput sums", KindThroughput, ReducerSum, false},
		{"error_rate sums", KindErrorRate, ReducerSum, false},
		{"latency averages", KindLatency, ReducerMean, false},
		{"freshness takes max", KindFreshness, ReducerMax, false},
		{"saturation takes max", KindSaturation, ReducerMax, false},
		{"resource_utilization averages", KindResourceUtilization, ReducerMean, false},
		{"availability averages", KindAvailability, ReducerMean, false},
		{"unknown falls back to mean", KindUnknown, ReducerMean, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DefaultAggregation(tc.kind)
			assert.Equal(t, tc.wantAcross, got.AcrossGroups)
			assert.Equal(t, tc.wantTwoStage, got.IsTwoStage())
			// Every default must pass validation — otherwise downstream
			// callers that trust ResolveAggregation will ship an invalid
			// spec to the querier.
			require.NoError(t, got.Validate())
		})
	}
}

func TestResolveAggregation(t *testing.T) {
	t.Run("Positive: explicit aggregation wins over default", func(t *testing.T) {
		explicit := &AggregationSpec{AcrossGroups: ReducerMax}
		meta := MetricMeta{Kind: KindBusinessKPI, Aggregation: explicit}
		got := meta.ResolveAggregation()
		assert.Equal(t, ReducerMax, got.AcrossGroups)
	})

	t.Run("Positive: nil aggregation falls back to kind default", func(t *testing.T) {
		meta := MetricMeta{Kind: KindBusinessKPI}
		got := meta.ResolveAggregation()
		assert.Equal(t, ReducerSum, got.AcrossGroups)
	})

	t.Run("Positive: two-stage explicit preserved", func(t *testing.T) {
		explicit := &AggregationSpec{
			GroupBy:      []string{"metric.labels.game_id"},
			WithinGroup:  ReducerMax,
			AcrossGroups: ReducerSum,
		}
		meta := MetricMeta{Kind: KindBusinessKPI, Aggregation: explicit}
		got := meta.ResolveAggregation()
		assert.True(t, got.IsTwoStage())
		assert.Equal(t, []string{"metric.labels.game_id"}, got.GroupBy)
	})

	t.Run("Aliasing: caller mutation does not corrupt registry entry", func(t *testing.T) {
		// Regression guard for the GroupBy aliasing fix. Without the
		// deep clone in ResolveAggregation, mutating the returned slice
		// would also mutate the registry's stored AggregationSpec.
		explicit := &AggregationSpec{
			GroupBy:      []string{"metric.labels.tenant_id"},
			WithinGroup:  ReducerMax,
			AcrossGroups: ReducerSum,
		}
		meta := MetricMeta{Kind: KindBusinessKPI, Aggregation: explicit}
		got := meta.ResolveAggregation()
		got.GroupBy[0] = "metric.labels.HACKED"
		assert.Equal(t, "metric.labels.tenant_id", explicit.GroupBy[0])
	})
}

func TestValidMetricKindsForInput(t *testing.T) {
	got := ValidMetricKindsForInput()
	for _, want := range []string{"freshness", "business_kpi"} {
		assert.Contains(t, got, want)
	}
}

// TestHasKnownGroupByPrefix locks the qualifier-recognition contract
// directly. Validate covers the integration path; this test isolates
// the helper so a regression that drops a namespace or breaks the
// resource.type exact-match branch is caught immediately.
func TestHasKnownGroupByPrefix(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		// Positive cases — every supported namespace.
		{"metric.labels.response_code", true},
		{"resource.labels.instance_id", true},
		{"metadata.system_labels.zone", true},
		{"metadata.user_labels.team", true},
		{"resource.type", true},

		// Negative — bare label name (the bug class the validation exists to catch).
		{"game_id", false},
		// Negative — empty string.
		{"", false},
		// Negative — namespace prefix with no tail. Cloud Monitoring
		// would treat this as malformed; we reject it at validation
		// time so the YAML author sees the typo.
		{"metric.labels.", false},
		{"resource.labels.", false},
		// Negative — typo'd namespace.
		{"metrics.labels.foo", false},
		// Negative — resource.type must be exact, not a prefix.
		{"resource.type.foo", false},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			got := hasKnownGroupByPrefix(tc.key)
			assert.Equal(t, tc.want, got)
		})
	}
}
