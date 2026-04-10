package metrics

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Note: loadOverlay is defined in registry_file_test.go and shared
// across the metrics package tests.

func TestOverlayAggregation(t *testing.T) {
	t.Run("Positive: single-stage explicit override", testOverlayAggregationSingleStage)
	t.Run("Positive: two-stage explicit override", testOverlayAggregationTwoStage)
	t.Run("Positive: overlay without aggregation leaves nil", testOverlayAggregationAbsent)
	t.Run("Negative: invalid across_groups rejected", testOverlayAggregationInvalidReducer)
	t.Run("Negative: group_by without within_group rejected", testOverlayAggregationMissingWithinGroup)
	t.Run("Negative: unknown field rejected", testOverlayAggregationUnknownField)
	t.Run("Negative: invalid within_group reducer rejected", testOverlayAggregationInvalidWithinGroup)
	t.Run("Negative: group_by as string rejected", testOverlayAggregationGroupByWrongType)
	t.Run("Negative: group_by item not a string", testOverlayAggregationGroupByItemWrongType)
	t.Run("Negative: aggregation block as list rejected", testOverlayAggregationBlockWrongType)
	t.Run("Positive: overlay replace-semantics discards base fields", testOverlayAggregationReplaceSemantics)
	t.Run("Negative: bare group_by label name rejected at load", testOverlayAggregationBareGroupByLabel)
}

func testOverlayAggregationBareGroupByLabel(t *testing.T) {
	// The bug class strict validation exists to catch: a YAML author
	// writes "group_by: [game_id]" instead of "[metric.labels.game_id]".
	// Cloud Monitoring silently ignores the bare key and collapses
	// every series into one group, producing nonsense scalars. We must
	// fail the load loudly so the operator fixes the YAML.
	yaml := `
metrics:
  "custom.googleapis.com/players_count":
    kind: business_kpi
    unit: players
    better_direction: up
    aggregation:
      group_by: [game_id]
      within_group: max
      across_groups: sum
`
	path := filepath.Join(t.TempDir(), "registry.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o644), "write tmp registry")
	_, err := LoadRegistry(path)
	require.Error(t, err, "expected LoadRegistry error for bare group_by label")
	assert.Contains(t, err.Error(), "metric.labels.", "error should steer user toward metric.labels. qualifier")
}

func testOverlayAggregationSingleStage(t *testing.T) {
	yaml := `
metrics:
  "custom.googleapis.com/ratio_metric":
    kind: business_kpi
    unit: ratio
    better_direction: up
    aggregation:
      across_groups: mean
`
	reg := loadOverlay(t, yaml)
	meta := reg.Lookup("custom.googleapis.com/ratio_metric")
	require.NotNil(t, meta.Aggregation, "aggregation should not be nil")
	assert.Equal(t, ReducerMean, meta.Aggregation.AcrossGroups, "AcrossGroups should be mean")
	assert.False(t, meta.Aggregation.IsTwoStage(), "should be single-stage, not two-stage")
}

func testOverlayAggregationTwoStage(t *testing.T) {
	yaml := `
metrics:
  "custom.googleapis.com/players_count":
    kind: business_kpi
    unit: players
    better_direction: up
    aggregation:
      group_by: [metric.labels.game_id]
      within_group: max
      across_groups: sum
`
	reg := loadOverlay(t, yaml)
	meta := reg.Lookup("custom.googleapis.com/players_count")
	require.NotNil(t, meta.Aggregation, "aggregation should not be nil")
	require.True(t, meta.Aggregation.IsTwoStage(), "should be two-stage spec")
	assert.Equal(t, ReducerMax, meta.Aggregation.WithinGroup, "WithinGroup should be max")
	assert.Equal(t, ReducerSum, meta.Aggregation.AcrossGroups, "AcrossGroups should be sum")
	require.Len(t, meta.Aggregation.GroupBy, 1, "GroupBy should have one element")
	assert.Equal(t, "metric.labels.game_id", meta.Aggregation.GroupBy[0], "GroupBy element should be metric.labels.game_id")
}

func testOverlayAggregationAbsent(t *testing.T) {
	yaml := `
metrics:
  "custom.googleapis.com/boring_metric":
    kind: business_kpi
    unit: count
    better_direction: up
`
	reg := loadOverlay(t, yaml)
	meta := reg.Lookup("custom.googleapis.com/boring_metric")
	assert.Nil(t, meta.Aggregation, "Aggregation should be nil when not specified")
	// Default resolves via the kind — caller should still get a valid
	// spec when they ask for one.
	resolved := meta.ResolveAggregation()
	assert.Equal(t, ReducerSum, resolved.AcrossGroups, "resolved AcrossGroups should be sum by default")
}

func testOverlayAggregationInvalidReducer(t *testing.T) {
	yaml := `
metrics:
  "custom.googleapis.com/bad_metric":
    kind: business_kpi
    unit: count
    better_direction: up
    aggregation:
      across_groups: avg
`
	_, err := writeAndLoad(t, yaml)
	require.Error(t, err, "expected error for invalid reducer")
}

func testOverlayAggregationMissingWithinGroup(t *testing.T) {
	yaml := `
metrics:
  "custom.googleapis.com/bad_metric":
    kind: business_kpi
    unit: count
    better_direction: up
    aggregation:
      group_by: [metric.labels.game_id]
      across_groups: sum
`
	_, err := writeAndLoad(t, yaml)
	require.Error(t, err, "expected error for missing within_group")
}

func testOverlayAggregationUnknownField(t *testing.T) {
	yaml := `
metrics:
  "custom.googleapis.com/bad_metric":
    kind: business_kpi
    unit: count
    better_direction: up
    aggregation:
      acros_groups: sum
`
	_, err := writeAndLoad(t, yaml)
	require.Error(t, err, "expected error for typo'd field")
	assert.Contains(t, err.Error(), "unknown field", "error should mention unknown field")
}

func testOverlayAggregationInvalidWithinGroup(t *testing.T) {
	yaml := `
metrics:
  "custom.googleapis.com/bad_metric":
    kind: business_kpi
    unit: count
    better_direction: up
    aggregation:
      group_by: [metric.labels.x]
      within_group: median
      across_groups: sum
`
	_, err := writeAndLoad(t, yaml)
	require.Error(t, err, "expected error for invalid within_group")
	assert.Contains(t, err.Error(), "within_group", "error should mention within_group")
	assert.Contains(t, err.Error(), "median", "error should mention median")
}

func testOverlayAggregationGroupByWrongType(t *testing.T) {
	yaml := `
metrics:
  "custom.googleapis.com/bad_metric":
    kind: business_kpi
    unit: count
    better_direction: up
    aggregation:
      group_by: "metric.labels.x"
      within_group: max
      across_groups: sum
`
	_, err := writeAndLoad(t, yaml)
	require.Error(t, err, "expected error for group_by not-a-list")
	assert.Contains(t, err.Error(), "group_by", "error should mention group_by")
	assert.Contains(t, err.Error(), "list", "error should mention list")
}

func testOverlayAggregationGroupByItemWrongType(t *testing.T) {
	yaml := `
metrics:
  "custom.googleapis.com/bad_metric":
    kind: business_kpi
    unit: count
    better_direction: up
    aggregation:
      group_by: [42]
      within_group: max
      across_groups: sum
`
	_, err := writeAndLoad(t, yaml)
	require.Error(t, err, "expected error for non-string group_by item")
	assert.Contains(t, err.Error(), "group_by", "error should mention group_by")
}

func testOverlayAggregationBlockWrongType(t *testing.T) {
	yaml := `
metrics:
  "custom.googleapis.com/bad_metric":
    kind: business_kpi
    unit: count
    better_direction: up
    aggregation:
      - across_groups
      - sum
`
	_, err := writeAndLoad(t, yaml)
	require.Error(t, err, "expected error for aggregation as list")
	assert.Contains(t, err.Error(), "aggregation", "error should mention aggregation")
}

// testOverlayAggregationReplaceSemantics guards the documented rule that
// aggregation is a replace-semantics block — NOT a field-merge like
// thresholds. If a base registry declares a two-stage spec and an overlay
// specifies only `across_groups`, the result must be a fresh spec with
// the base's group_by and within_group discarded. This test drives
// mergeMetricFields directly because the base registry load itself would
// reject the partial overlay as an invalid spec (missing within_group).
func testOverlayAggregationReplaceSemantics(t *testing.T) {
	base := MetricMeta{
		Kind:            KindBusinessKPI,
		Unit:            "items",
		BetterDirection: DirectionUp,
		Aggregation: &AggregationSpec{
			GroupBy:      []string{"metric.labels.game_id"},
			WithinGroup:  ReducerMax,
			AcrossGroups: ReducerSum,
		},
	}
	overlay := map[string]any{
		"aggregation": map[string]any{
			"across_groups": "mean",
		},
	}
	merged, errs := mergeMetricFields(base, overlay)
	require.Empty(t, errs, "should have no merge errors")
	require.NotNil(t, merged.Aggregation, "merged.Aggregation should be the replacement spec")
	assert.Equal(t, ReducerMean, merged.Aggregation.AcrossGroups, "AcrossGroups should be mean")
	assert.Empty(t, merged.Aggregation.GroupBy, "GroupBy should be empty (replace-semantics discards base)")
	assert.Empty(t, merged.Aggregation.WithinGroup, "WithinGroup should be empty (replace-semantics discards base)")
}

// writeAndLoad is a variant of loadOverlay that returns the error
// instead of fatally failing. Used by negative tests that expect
// validation to reject the overlay.
func writeAndLoad(t *testing.T, yaml string) (*Registry, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "overlay.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o644))
	return LoadRegistry(path)
}
