package metrics

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadRegistryFileNotFound verifies that LoadRegistry wraps file-read
// errors with the path so operators can identify which file failed.
func TestLoadRegistryFileNotFound(t *testing.T) {
	const path = "/nonexistent/path/that/does/not/exist/registry.yaml"
	_, err := LoadRegistry(path)
	require.Error(t, err, "expected error for nonexistent file, got nil")
	msg := err.Error()
	assert.Contains(t, msg, "reading", "error should mention 'reading'")
	assert.Contains(t, msg, path, "error should contain the file path")
	assert.True(t, errors.Is(err, fs.ErrNotExist), "%w wrapping must be preserved")
}

// TestEmbeddedDefaultRegistry verifies that the registry YAML embedded into
// the binary (default_registry.yaml) parses cleanly, validates every entry,
// and is reachable via both NewDefaultRegistry() and LoadRegistry("") (the
// empty-path path falls back to embedded-only).
func TestEmbeddedDefaultRegistry(t *testing.T) {
	reg, err := NewDefaultRegistry()
	require.NoError(t, err, "NewDefaultRegistry() failed")

	got := reg.Count()
	// Soft floor — the shipped default set is intentionally sized so this
	// check is stable. When adding new metrics bump this; if the count
	// drops unexpectedly the YAML is probably silently malformed.
	assert.GreaterOrEqual(t, got, 50, "embedded default registry should load at least 50 metrics")

	// Empty-path LoadRegistry must return the same set as NewDefaultRegistry.
	reg2, err := LoadRegistry("")
	require.NoError(t, err, "LoadRegistry(\"\") failed")
	assert.Equal(t, got, reg2.Count(), "LoadRegistry(\"\") and NewDefaultRegistry() should return same count")

	// Sanity check: a few representative metrics from each section should
	// be present and come back with their configured kind.
	cases := []struct {
		metricType string
		wantKind   MetricKind
	}{
		{"compute.googleapis.com/instance/cpu/utilization", KindResourceUtilization},
		{"compute.googleapis.com/instance/disk/average_io_latency", KindLatency},
		{"cloudsql.googleapis.com/database/cpu/utilization", KindResourceUtilization},
		{"cloudsql.googleapis.com/database/replication/replica_lag", KindFreshness},
		{"cloudsql.googleapis.com/database/up", KindAvailability},
		{"kubernetes.io/container/memory/limit_utilization", KindSaturation},
		{"kubernetes.io/container/restart_count", KindErrorRate},
		{"pubsub.googleapis.com/subscription/oldest_unacked_message_age", KindFreshness},
		{"pubsub.googleapis.com/subscription/ack_latencies", KindLatency},
		{"pubsub.googleapis.com/subscription/num_undelivered_messages", KindSaturation},
	}
	for _, tc := range cases {
		t.Run(tc.metricType, func(t *testing.T) {
			meta := reg.Lookup(tc.metricType)
			assert.False(t, meta.AutoDetected, "%q should have explicit registry entry, not auto-detected", tc.metricType)
			assert.Equal(t, tc.wantKind, meta.Kind, "%q kind mismatch", tc.metricType)
		})
	}
}

// TestRegistryOverlay_FieldLevelMerge verifies that user-supplied YAML is
// merged field-by-field on top of the embedded defaults, not wholesale
// replaced. Fields not mentioned in the overlay should keep their base
// values.
func TestRegistryOverlay_FieldLevelMerge(t *testing.T) {
	// Only slo_threshold is specified in the overlay. Everything else
	// (kind, unit, direction, saturation_cap, related_metrics) should
	// come from the embedded default.
	overlay := `metrics:
  "compute.googleapis.com/instance/cpu/utilization":
    slo_threshold: 0.50
`
	reg := loadOverlay(t, overlay)

	cpu := reg.Lookup("compute.googleapis.com/instance/cpu/utilization")
	assert.False(t, cpu.AutoDetected, "cpu/utilization should not be auto-detected after overlay")
	// Overridden field.
	require.NotNil(t, cpu.SLOThreshold, "slo_threshold should not be nil")
	assert.Equal(t, 0.50, *cpu.SLOThreshold, "slo_threshold should be from overlay")
	// Fields that must survive from the base entry.
	assert.Equal(t, KindResourceUtilization, cpu.Kind, "kind should survive from base")
	assert.Equal(t, "ratio", cpu.Unit, "unit should survive from base")
	assert.Equal(t, DirectionDown, cpu.BetterDirection, "better_direction should survive from base")
	require.NotNil(t, cpu.SaturationCap, "saturation_cap should not be nil")
	assert.Equal(t, 1.0, *cpu.SaturationCap, "saturation_cap should survive from base")
	assert.NotEmpty(t, cpu.RelatedMetrics, "related_metrics should carry over from base")
}

// TestRegistryOverlay_RelatedMetricsExtend verifies that related_metrics is
// set-unioned with the base list, not replaced. Users can add custom
// correlations without having to re-list every default relation.
func TestRegistryOverlay_RelatedMetricsExtend(t *testing.T) {
	overlay := `metrics:
  "compute.googleapis.com/instance/cpu/utilization":
    related_metrics:
      - "custom.googleapis.com/myapp/inflight_requests"
      - "compute.googleapis.com/instance/cpu/scheduler_wait_time"  # already in base
`
	reg := loadOverlay(t, overlay)

	cpu := reg.Lookup("compute.googleapis.com/instance/cpu/utilization")

	// The base has these three relations (see default_registry.yaml).
	wantBase := []string{
		"compute.googleapis.com/instance/cpu/scheduler_wait_time",
		"compute.googleapis.com/instance/memory/balloon/ram_used",
		"compute.googleapis.com/instance/disk/average_io_latency",
	}
	for _, base := range wantBase {
		assert.True(t, containsString(cpu.RelatedMetrics, base), "related_metrics should include base entry %q", base)
	}

	// The user-added entry must be present.
	assert.True(t, containsString(cpu.RelatedMetrics, "custom.googleapis.com/myapp/inflight_requests"), "related_metrics should include user-added entry")

	// The duplicate (already in base) must not appear twice.
	count := 0
	for _, m := range cpu.RelatedMetrics {
		if m == "compute.googleapis.com/instance/cpu/scheduler_wait_time" {
			count++
		}
	}
	assert.Equal(t, 1, count, "duplicate relation should appear exactly once (set-union dedupe)")
}

// TestRegistryOverlay_KeywordsExtend verifies that keywords are
// set-unioned with the base list, not replaced — just like related_metrics.
// Users can add their own search synonyms without re-listing the embedded
// defaults, and duplicates (including case-insensitive) are dropped.
func TestRegistryOverlay_KeywordsExtend(t *testing.T) {
	overlay := `metrics:
  "pubsub.googleapis.com/subscription/oldest_unacked_message_age":
    keywords:
      - "kafka-equivalent"
      - "MESSAGING"   # already in base (set by Phase A) — must dedupe, case-insensitive
`
	reg := loadOverlay(t, overlay)

	pubsub := reg.Lookup("pubsub.googleapis.com/subscription/oldest_unacked_message_age")

	assert.True(t, containsString(pubsub.Keywords, "kafka-equivalent"), "keywords should include user-added entry")
	// Base keyword survives and case-insensitive dedupe leaves only one.
	count := 0
	for _, k := range pubsub.Keywords {
		if strings.EqualFold(k, "messaging") {
			count++
		}
	}
	assert.Greater(t, count, 0, "base keyword 'messaging' should survive after overlay")
	assert.Equal(t, 1, count, "duplicate keyword should appear exactly once (case-insensitive)")
}

// TestRegistryOverlay_ThresholdsFieldMerge verifies nested ClassificationThresholds
// field-level merge: overriding one threshold must not wipe the others.
func TestRegistryOverlay_ThresholdsFieldMerge(t *testing.T) {
	// error_rate default thresholds come from DefaultThresholdsFor(KindErrorRate):
	// SignificantDeltaPct=5, BreachRatioForRegress=0.20, CVForNoisy=0.50, SpikeZScore=2.5
	// We override only spike_zscore and expect the others to survive.
	overlay := `metrics:
  "cloudsql.googleapis.com/database/postgresql/deadlock_count":
    thresholds:
      spike_zscore: 4.0
`
	reg := loadOverlay(t, overlay)

	m := reg.Lookup("cloudsql.googleapis.com/database/postgresql/deadlock_count")
	thr := m.EffectiveThresholds()

	assert.Equal(t, 4.0, thr.SpikeZScore, "spike_zscore should be from overlay")
	// These come from the kind defaults (error_rate).
	assert.Equal(t, 5.0, thr.SignificantDeltaPct, "significant_delta_pct should be from error_rate default")
	assert.Equal(t, 0.20, thr.BreachRatioForRegress, "breach_ratio_for_regression should be from error_rate default")
	assert.Equal(t, 0.50, thr.CVForNoisy, "cv_for_noisy should be from error_rate default")
}

// TestRegistryOverlay_NewMetric verifies that an entry present only in the
// overlay (not in the embedded defaults) is added as a brand-new metric,
// and that missing required fields cause a validation error rather than
// silently producing an invalid entry.
func TestRegistryOverlay_NewMetric(t *testing.T) {
	overlay := `metrics:
  "custom.googleapis.com/my_app/widgets_per_second":
    kind: throughput
    unit: count
    better_direction: none
`
	reg := loadOverlay(t, overlay)

	widgets := reg.Lookup("custom.googleapis.com/my_app/widgets_per_second")
	assert.False(t, widgets.AutoDetected, "custom widgets metric should not be auto-detected")
	assert.Equal(t, KindThroughput, widgets.Kind, "widgets kind should be throughput")
}

// TestRegistryOverlay_NewMetricMissingKind verifies that a new metric
// introduced only by the overlay must still include required fields;
// a partial entry for a non-existent base metric should fail validation.
func TestRegistryOverlay_NewMetricMissingKind(t *testing.T) {
	// slo_threshold alone is not enough when there is no base entry to
	// merge into — kind will be empty and Validate() must reject it.
	overlay := `metrics:
  "custom.googleapis.com/ghost":
    slo_threshold: 0.5
`
	dir := t.TempDir()
	overlayPath := filepath.Join(dir, "overlay.yaml")
	require.NoError(t, os.WriteFile(overlayPath, []byte(overlay), 0o644), "writing overlay")
	_, err := LoadRegistry(overlayPath)
	require.Error(t, err, "expected validation error for partial overlay on brand-new metric")
}

// TestRegistryOverlay_UntouchedDefaults verifies entries not mentioned in
// the overlay remain intact.
func TestRegistryOverlay_UntouchedDefaults(t *testing.T) {
	overlay := `metrics:
  "compute.googleapis.com/instance/cpu/utilization":
    slo_threshold: 0.50
`
	reg := loadOverlay(t, overlay)

	// An unrelated default must still be there.
	sql := reg.Lookup("cloudsql.googleapis.com/database/cpu/utilization")
	assert.False(t, sql.AutoDetected, "cloudsql cpu/utilization should not be lost after overlay")
	assert.Equal(t, KindResourceUtilization, sql.Kind, "cloudsql cpu/utilization kind should be preserved")
}

// TestRegistryOverlay_TypeMismatchCollectsAllErrors verifies that when an
// overlay entry contains multiple fields with wrong Go types, LoadRegistry
// collects every mismatch into a single returned error (via errors.Join)
// instead of bailing on the first one. This is the R2-C3 contract: an
// operator fixing a broken overlay should see every mistake per run, not
// play whack-a-mole across map-iteration-order failures.
func TestRegistryOverlay_TypeMismatchCollectsAllErrors(t *testing.T) {
	// Two wrong types on the same base metric — `kind: 1` (should be
	// string) and `slo_threshold: "high"` (should be number). A regression
	// that early-returned on the first mismatch would produce an error
	// containing only one of the two substrings.
	overlay := `metrics:
  "compute.googleapis.com/instance/cpu/utilization":
    kind: 1
    slo_threshold: "high"
`
	dir := t.TempDir()
	overlayPath := filepath.Join(dir, "overlay.yaml")
	require.NoError(t, os.WriteFile(overlayPath, []byte(overlay), 0o644), "writing overlay")
	_, err := LoadRegistry(overlayPath)
	require.Error(t, err, "expected error for overlay with type mismatches")
	msg := err.Error()
	for _, want := range []string{
		`field "kind" must be string`,
		`field "slo_threshold" must be number`,
	} {
		assert.Contains(t, msg, want, "error message should include all type mismatches")
	}
}

// loadOverlay is a test helper that writes the given YAML to a temp file
// and loads it as a registry overlay on top of the embedded defaults.
func loadOverlay(t *testing.T, yaml string) *Registry {
	t.Helper()
	dir := t.TempDir()
	overlayPath := filepath.Join(dir, "overlay.yaml")
	require.NoError(t, os.WriteFile(overlayPath, []byte(yaml), 0o644))
	reg, err := LoadRegistry(overlayPath)
	require.NoError(t, err)
	return reg
}

func containsString(list []string, needle string) bool {
	for _, s := range list {
		if s == needle {
			return true
		}
	}
	return false
}
