package metrics

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// RegistryConfig is the top-level YAML structure for the metrics registry.
type RegistryConfig struct {
	Metrics map[string]MetricMeta `yaml:"metrics"`
}

// Registry holds semantic metadata for known metrics and provides auto-detection for unknown ones.
type Registry struct {
	metrics map[string]MetricMeta
}

// NewRegistry creates an empty registry that relies on auto-detection only.
func NewRegistry() *Registry {
	return &Registry{metrics: make(map[string]MetricMeta)}
}

// LoadRegistry loads a registry from a YAML file at the given path.
func LoadRegistry(path string) (*Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg RegistryConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	if cfg.Metrics == nil {
		cfg.Metrics = make(map[string]MetricMeta)
	}

	for name, meta := range cfg.Metrics {
		if err := meta.Validate(name); err != nil {
			return nil, fmt.Errorf("invalid registry entry: %w", err)
		}
	}

	return &Registry{metrics: cfg.Metrics}, nil
}

// Lookup returns the MetricMeta for the given metric type.
// If the metric is in the registry, it returns the configured metadata.
// Otherwise, it auto-detects the kind from naming conventions.
func (r *Registry) Lookup(metricType string) MetricMeta {
	if meta, ok := r.metrics[metricType]; ok {
		return meta
	}
	return autoDetect(metricType)
}

// MetricListEntry is a single entry returned by List.
type MetricListEntry struct {
	MetricType      string          `json:"metric_type"`
	Kind            MetricKind      `json:"kind"`
	Unit            string          `json:"unit"`
	BetterDirection BetterDirection `json:"better_direction"`
	SLOThreshold    *float64        `json:"slo_threshold,omitempty"`
	RelatedMetrics  []string        `json:"related_metrics,omitempty"`
	AutoDetected    bool            `json:"auto_detected,omitempty"`
}

// List returns registry entries matching the given filters.
func (r *Registry) List(match string, kind MetricKind) []MetricListEntry {
	var result []MetricListEntry
	for name, meta := range r.metrics {
		if match != "" && !strings.Contains(strings.ToLower(name), strings.ToLower(match)) {
			continue
		}
		if kind != "" && meta.Kind != kind {
			continue
		}
		result = append(result, MetricListEntry{
			MetricType:      name,
			Kind:            meta.Kind,
			Unit:            meta.Unit,
			BetterDirection: meta.BetterDirection,
			SLOThreshold:    meta.SLOThreshold,
			RelatedMetrics:  meta.RelatedMetrics,
		})
	}
	return result
}

// Count returns the number of metrics configured in the registry.
func (r *Registry) Count() int {
	return len(r.metrics)
}

// RelatedMetrics returns the list of related metrics for the given metric type.
func (r *Registry) RelatedMetrics(metricType string) []string {
	if meta, ok := r.metrics[metricType]; ok {
		return meta.RelatedMetrics
	}
	return nil
}

// autoDetect infers MetricMeta from a metric type string using naming conventions.
func autoDetect(metricType string) MetricMeta {
	lower := strings.ToLower(metricType)
	// Use the last path segments for keyword matching.
	// e.g. "compute.googleapis.com/instance/cpu/utilization" → "instance/cpu/utilization"
	if idx := strings.Index(lower, "/"); idx >= 0 {
		lower = lower[idx:]
	}

	meta := MetricMeta{AutoDetected: true}

	switch {
	case containsAny(lower, "latency", "latencies", "duration", "response_time"):
		meta.Kind = KindLatency
		meta.Unit = "seconds"
		meta.BetterDirection = DirectionDown
	case containsAny(lower, "error", "fault", "abort"):
		meta.Kind = KindErrorRate
		meta.Unit = "count"
		meta.BetterDirection = DirectionDown
	case containsAny(lower, "byte", "bytes"):
		meta.Kind = KindThroughput
		meta.Unit = "bytes"
		meta.BetterDirection = DirectionNone
	case containsAny(lower, "utilization"):
		meta.Kind = KindResourceUtilization
		meta.Unit = "ratio"
		meta.BetterDirection = DirectionDown
	case containsAny(lower, "cpu", "memory"):
		meta.Kind = KindResourceUtilization
		meta.Unit = "ratio"
		meta.BetterDirection = DirectionDown
	case containsAny(lower, "count", "request", "total", "num_"):
		meta.Kind = KindThroughput
		meta.Unit = "count"
		meta.BetterDirection = DirectionNone
	default:
		meta.Kind = KindUnknown
		meta.Unit = ""
		meta.BetterDirection = DirectionNone
	}

	return meta
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
