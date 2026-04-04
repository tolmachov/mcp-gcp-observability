package metrics

import (
	"os"
	"path/filepath"
	"testing"
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
	}

	for _, tt := range tests {
		t.Run(tt.metricType, func(t *testing.T) {
			meta := autoDetect(tt.metricType)
			if meta.Kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", meta.Kind, tt.wantKind)
			}
			if meta.BetterDirection != tt.wantDir {
				t.Errorf("better_direction = %q, want %q", meta.BetterDirection, tt.wantDir)
			}
			if !meta.AutoDetected {
				t.Error("expected AutoDetected=true")
			}
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
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}

	reg, err := LoadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}

	// Configured metric returns registry data.
	meta := reg.Lookup("compute.googleapis.com/instance/cpu/utilization")
	if meta.Kind != KindResourceUtilization {
		t.Errorf("kind = %q, want resource_utilization", meta.Kind)
	}
	if meta.AutoDetected {
		t.Error("expected AutoDetected=false for configured metric")
	}
	satCap := 1.0
	if meta.SaturationCap == nil || *meta.SaturationCap != satCap {
		t.Errorf("saturation_cap = %v, want %v", meta.SaturationCap, satCap)
	}

	// SLO threshold.
	meta2 := reg.Lookup("run.googleapis.com/request_latencies")
	slo := 500.0
	if meta2.SLOThreshold == nil || *meta2.SLOThreshold != slo {
		t.Errorf("slo_threshold = %v, want %v", meta2.SLOThreshold, slo)
	}

	// Unknown metric falls back to auto-detect.
	meta3 := reg.Lookup("custom.googleapis.com/something")
	if !meta3.AutoDetected {
		t.Error("expected AutoDetected=true for unknown metric")
	}
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
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadRegistry(path)
	if err == nil {
		t.Fatal("expected error for invalid kind, got nil")
	}
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
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadRegistry(path)
	if err == nil {
		t.Fatal("expected error for invalid better_direction, got nil")
	}
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
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Kind != KindResourceUtilization {
		t.Errorf("kind = %q, want resource_utilization", entries[0].Kind)
	}

	// Filter by kind.
	entries = reg.List("", KindLatency)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	// No filter returns all.
	entries = reg.List("", "")
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
}
