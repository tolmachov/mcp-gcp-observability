package metrics

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAutoDetect(t *testing.T) {
	tests := []struct {
		metricType string
		wantKind   MetricKind
		wantDir    BetterDirection
	}{
		{"compute.googleapis.com/instance/cpu/utilization", KindResourceUtilization, DirectionDown},
		{"run.googleapis.com/request_latencies", KindLatency, DirectionDown},
		{"run.googleapis.com/request_count", KindThroughput, DirectionNone},
		{"logging.googleapis.com/byte_count", KindThroughput, DirectionNone},
		{"compute.googleapis.com/instance/disk/read_bytes_count", KindThroughput, DirectionNone},
		{"custom.googleapis.com/error_count", KindErrorRate, DirectionDown},
		{"custom.googleapis.com/some_metric", KindUnknown, DirectionNone},
		{"appengine.googleapis.com/http/server/response_latencies", KindLatency, DirectionDown},
		{"compute.googleapis.com/instance/memory/balloon/ram_used", KindResourceUtilization, DirectionDown},
		// Freshness / lag detection — must beat the "duration"/"seconds"
		// fallbacks in the latency branch.
		{"pubsub.googleapis.com/subscription/oldest_unacked_message_age", KindFreshness, DirectionDown},
		{"custom.googleapis.com/replication_lag_seconds", KindFreshness, DirectionDown},
		{"custom.googleapis.com/consumer_group_lag", KindFreshness, DirectionDown},
		{"custom.googleapis.com/data_staleness_duration", KindFreshness, DirectionDown},
		{"custom.googleapis.com/seconds_since_last_success", KindFreshness, DirectionDown},
	}

	for _, tt := range tests {
		t.Run(tt.metricType, func(t *testing.T) {
			meta := autoDetect(tt.metricType)
			assert.Equal(t, tt.wantKind, meta.Kind, "kind mismatch")
			assert.Equal(t, tt.wantDir, meta.BetterDirection, "better_direction mismatch")
			assert.True(t, meta.AutoDetected, "expected AutoDetected=true")
		})
	}
}

func TestLoadRegistry(t *testing.T) {
	yamlContent := `
metrics:
  "compute.googleapis.com/instance/cpu/utilization":
    kind: resource_utilization
    unit: ratio
    better_direction: down
    saturation_cap: 1.0
    related_metrics:
      - "compute.googleapis.com/instance/disk/read_bytes_count"
  "run.googleapis.com/request_latencies":
    kind: latency
    unit: milliseconds
    better_direction: down
    slo_threshold: 500.0
`
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yamlContent), 0o644))

	reg, err := LoadRegistry(path)
	require.NoError(t, err)

	// Configured metric returns registry data.
	meta := reg.Lookup("compute.googleapis.com/instance/cpu/utilization")
	assert.Equal(t, KindResourceUtilization, meta.Kind, "kind should be resource_utilization")
	assert.False(t, meta.AutoDetected, "expected AutoDetected=false for configured metric")
	satCap := 1.0
	require.NotNil(t, meta.SaturationCap, "saturation_cap should not be nil")
	assert.Equal(t, satCap, *meta.SaturationCap, "saturation_cap should match")

	// SLO threshold.
	meta2 := reg.Lookup("run.googleapis.com/request_latencies")
	slo := 500.0
	require.NotNil(t, meta2.SLOThreshold, "slo_threshold should not be nil")
	assert.Equal(t, slo, *meta2.SLOThreshold, "slo_threshold should match")

	// Unknown metric falls back to auto-detect.
	meta3 := reg.Lookup("custom.googleapis.com/something")
	assert.True(t, meta3.AutoDetected, "expected AutoDetected=true for unknown metric")
}

func TestLoadRegistryInvalidKind(t *testing.T) {
	yamlContent := `
metrics:
  "test.googleapis.com/some_metric":
    kind: bananas
    unit: ratio
    better_direction: down
`
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yamlContent), 0o644))

	_, err := LoadRegistry(path)
	require.Error(t, err, "expected error for invalid kind")
}

func TestLoadRegistryInvalidDirection(t *testing.T) {
	yamlContent := `
metrics:
  "test.googleapis.com/some_metric":
    kind: latency
    unit: seconds
    better_direction: sideways
`
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yamlContent), 0o644))

	_, err := LoadRegistry(path)
	require.Error(t, err, "expected error for invalid better_direction")
}

func TestRegistryList(t *testing.T) {
	reg := NewRegistry()
	reg.metrics["test.googleapis.com/cpu_utilization"] = MetricMeta{
		Kind:            KindResourceUtilization,
		Unit:            "ratio",
		BetterDirection: DirectionDown,
	}
	reg.metrics["test.googleapis.com/request_latency"] = MetricMeta{
		Kind:            KindLatency,
		Unit:            "seconds",
		BetterDirection: DirectionDown,
	}

	// Filter by match.
	entries := reg.List("cpu", "")
	require.Len(t, entries, 1, "should return 1 entry for cpu filter")
	assert.Equal(t, KindResourceUtilization, entries[0].Kind, "kind should be resource_utilization")

	// Filter by kind.
	entries = reg.List("", KindLatency)
	require.Len(t, entries, 1, "should return 1 entry for latency kind filter")

	// No filter returns all.
	entries = reg.List("", "")
	require.Len(t, entries, 2, "should return 2 entries with no filter")
}

// TestRegistryList_KeywordMatching verifies that the match substring is
// checked against Keywords in addition to the metric name, so category
// synonyms (database, nosql, cache) find metrics whose literal name does
// not contain that word.
func TestRegistryList_KeywordMatching(t *testing.T) {
	reg := NewRegistry()
	reg.metrics["firestore.googleapis.com/document/read_count"] = MetricMeta{
		Kind:            KindThroughput,
		Unit:            "count",
		BetterDirection: DirectionNone,
		Keywords:        []string{"database", "nosql", "document"},
	}
	reg.metrics["run.googleapis.com/request_count"] = MetricMeta{
		Kind:            KindThroughput,
		Unit:            "count",
		BetterDirection: DirectionNone,
		Keywords:        []string{"serverless", "container"},
	}

	cases := []struct {
		name  string
		match string
		want  string
	}{
		{"by metric name", "read_count", "firestore.googleapis.com/document/read_count"},
		{"by keyword database", "database", "firestore.googleapis.com/document/read_count"},
		{"by keyword nosql (case-insensitive)", "NoSQL", "firestore.googleapis.com/document/read_count"},
		{"by keyword serverless", "serverless", "run.googleapis.com/request_count"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries := reg.List(tc.match, "")
			require.Len(t, entries, 1, "List(%q) should return 1 entry", tc.match)
			assert.Equal(t, tc.want, entries[0].MetricType, "List(%q) metric type mismatch", tc.match)
		})
	}

	// Keyword that matches neither entry returns nothing.
	entries := reg.List("bananas", "")
	assert.Empty(t, entries, "List(\"bananas\") should return no entries")
}

// TestRegistryList_ServiceTokenMatching verifies the auto-derived service
// token from the metric type (e.g. "foo" from "foo.googleapis.com/bar")
// matches without needing explicit keywords. This removes the need to
// write keywords: [pubsub] on every pubsub metric — the prefix is free.
func TestRegistryList_ServiceTokenMatching(t *testing.T) {
	reg := NewRegistry()
	// Metric name contains neither "foo" nor any keyword for it —
	// matching must come from the auto-derived service token alone.
	reg.metrics["foo.googleapis.com/instance/widget_depth"] = MetricMeta{
		Kind:            KindSaturation,
		Unit:            "count",
		BetterDirection: DirectionDown,
	}

	entries := reg.List("foo", "")
	require.Len(t, entries, 1, "List(\"foo\") should return 1 entry for service-token match")

	// A substring of the service token also hits — useful for "sql" matching
	// "cloudsql.googleapis.com/...".
	reg.metrics["cloudsql.googleapis.com/database/cpu/utilization"] = MetricMeta{
		Kind:            KindResourceUtilization,
		Unit:            "ratio",
		BetterDirection: DirectionDown,
	}
	entries = reg.List("sql", "")
	found := false
	for _, e := range entries {
		if e.MetricType == "cloudsql.googleapis.com/database/cpu/utilization" {
			found = true
			break
		}
	}
	assert.True(t, found, "List(\"sql\") should match cloudsql metric via service-token substring")
}

func TestServiceToken(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"pubsub.googleapis.com/subscription/ack_latencies", "pubsub"},
		{"cloudsql.googleapis.com/database/up", "cloudsql"},
		{"kubernetes.io/container/restart_count", "kubernetes"},
		{"custom.googleapis.com/my_app/foo", "custom"},
		{"no_separator_here", "no_separator_here"},
		{"", ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, serviceToken(tc.in), "serviceToken(%q) mismatch", tc.in)
	}
}
