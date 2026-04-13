package metrics

import (
	_ "embed"
	"errors"
	"fmt"
	"maps"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// defaultRegistryYAML: embedded default semantic registry for standard GCP services.
// Provides out-of-the-box semantics with no external config.
//
//go:embed default_registry.yaml
var defaultRegistryYAML []byte

type RegistryConfig struct {
	Metrics map[string]MetricMeta `yaml:"metrics"`
}

type Registry struct {
	metrics map[string]MetricMeta
}

// NewRegistry creates an empty registry that relies on auto-detection only.
// Use NewDefaultRegistry if you want the embedded GCP defaults instead.
func NewRegistry() *Registry {
	return &Registry{metrics: make(map[string]MetricMeta)}
}

// NewDefaultRegistry populates a registry from embedded default YAML.
// Errors if embedded YAML fails to parse (tested, should not happen).
func NewDefaultRegistry() (*Registry, error) {
	cfg, err := parseRegistryBytes(defaultRegistryYAML)
	if err != nil {
		return nil, fmt.Errorf("parsing embedded default registry: %w", err)
	}
	return &Registry{metrics: cfg.Metrics}, nil
}

// NewRegistryFromMetaMap creates a Registry directly from a MetricMeta map,
// bypassing YAML parsing and load-time validation. Intended for tests that
// need to inject configurations (e.g. invalid AggregationSpec) that LoadRegistry
// would otherwise reject.
func NewRegistryFromMetaMap(m map[string]MetricMeta) *Registry {
	cp := make(map[string]MetricMeta, len(m))
	maps.Copy(cp, m)
	return &Registry{metrics: cp}
}

// LoadRegistry loads a YAML overlay merged on top of embedded defaults.
// Merges field-by-field with explicit overwrites. related_metrics
// is extended (set-union). Validation runs on merged result.
// If path is empty, only embedded defaults are used.
func LoadRegistry(path string) (*Registry, error) {
	base, err := parseRegistryBytes(defaultRegistryYAML)
	if err != nil {
		return nil, fmt.Errorf("parsing embedded default registry: %w", err)
	}

	if path == "" {
		return &Registry{metrics: base.Metrics}, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	overlay, err := parseRegistryRawBytes(data)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	// Collect all errors and sort for deterministic, complete error reporting.
	names := make([]string, 0, len(overlay))
	for name := range overlay {
		names = append(names, name)
	}
	sort.Strings(names)

	var overlayErrs []error
	for _, name := range names {
		fields := overlay[name]
		merged, errs := mergeMetricFields(base.Metrics[name], fields)
		for _, e := range errs {
			overlayErrs = append(overlayErrs, fmt.Errorf("overlay metric %q: %w", name, e))
		}
		if err := merged.Validate(name); err != nil {
			overlayErrs = append(overlayErrs, fmt.Errorf("overlay metric %q: %w", name, err))
			continue
		}
		base.Metrics[name] = merged
	}
	if len(overlayErrs) > 0 {
		return nil, errors.Join(overlayErrs...)
	}

	return &Registry{metrics: base.Metrics}, nil
}

// parseRegistryRawBytes parses the overlay YAML into a two-level map
// (metricType → fieldName → value) so we can distinguish "field present in
// YAML" from "field absent" — critical for field-level merge semantics.
// Typed unmarshal into MetricMeta would erase that distinction because Go
// zero values are indistinguishable from unset YAML keys.
func parseRegistryRawBytes(data []byte) (map[string]map[string]any, error) {
	var cfg struct {
		Metrics map[string]map[string]any `yaml:"metrics"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg.Metrics == nil {
		cfg.Metrics = make(map[string]map[string]any)
	}
	return cfg.Metrics, nil
}

// mergeMetricFields applies the user-supplied overlay fields on top of the
// base MetricMeta entry. Only keys present in the overlay are touched.
// related_metrics and keywords are set-unioned with base; all other fields
// replace.
//
// Type mismatches (e.g. `kind: 1` when a string is expected) are collected
// into the returned error slice rather than silently dropped, so operators
// see "overlay metric X: field `kind` must be string, got int" instead of
// discovering at runtime that their overlay edit had no effect. Merging
// continues past individual mismatches so a single typo doesn't hide later
// errors from the same file.
func mergeMetricFields(base MetricMeta, overlay map[string]any) (MetricMeta, []error) {
	result := base
	// Clone the slice so appending never mutates the shared base entry.
	if len(result.RelatedMetrics) > 0 {
		cloned := make([]string, len(result.RelatedMetrics))
		copy(cloned, result.RelatedMetrics)
		result.RelatedMetrics = cloned
	}

	var errs []error
	typeErr := func(field, want string, got any) {
		errs = append(errs, fmt.Errorf("field %q must be %s, got %T", field, want, got))
	}

	for key, val := range overlay {
		switch key {
		case "kind":
			if s, ok := val.(string); ok {
				result.Kind = MetricKind(s)
			} else {
				typeErr(key, "string", val)
			}
		case "unit":
			if s, ok := val.(string); ok {
				result.Unit = s
			} else {
				typeErr(key, "string", val)
			}
		case "better_direction":
			if s, ok := val.(string); ok {
				result.BetterDirection = BetterDirection(s)
			} else {
				typeErr(key, "string", val)
			}
		case "slo_threshold":
			if f, ok := toFloat64(val); ok {
				result.SLOThreshold = &f
			} else {
				typeErr(key, "number", val)
			}
		case "saturation_cap":
			if f, ok := toFloat64(val); ok {
				result.SaturationCap = &f
			} else {
				typeErr(key, "number", val)
			}
		case "related_metrics":
			// Extend, do not replace. Dedupe while preserving order so
			// base defaults appear first and user additions come after.
			list, ok := val.([]any)
			if !ok {
				typeErr(key, "list of strings", val)
				continue
			}
			seen := make(map[string]bool, len(result.RelatedMetrics))
			for _, m := range result.RelatedMetrics {
				seen[m] = true
			}
			for i, item := range list {
				s, ok := item.(string)
				if !ok {
					errs = append(errs, fmt.Errorf("field %q[%d] must be string, got %T", key, i, item))
					continue
				}
				if seen[s] {
					continue
				}
				seen[s] = true
				result.RelatedMetrics = append(result.RelatedMetrics, s)
			}
		case "keywords":
			// Extend, do not replace — same set-union semantics as
			// related_metrics. Users can add their own search synonyms
			// without having to re-list the embedded defaults.
			list, ok := val.([]any)
			if !ok {
				typeErr(key, "list of strings", val)
				continue
			}
			// Clone base slice so appending never mutates the shared base.
			if len(result.Keywords) > 0 {
				cloned := make([]string, len(result.Keywords))
				copy(cloned, result.Keywords)
				result.Keywords = cloned
			}
			seen := make(map[string]bool, len(result.Keywords))
			for _, k := range result.Keywords {
				seen[strings.ToLower(k)] = true
			}
			for i, item := range list {
				s, ok := item.(string)
				if !ok {
					errs = append(errs, fmt.Errorf("field %q[%d] must be string, got %T", key, i, item))
					continue
				}
				lk := strings.ToLower(s)
				if seen[lk] {
					continue
				}
				seen[lk] = true
				result.Keywords = append(result.Keywords, s)
			}
		case "thresholds":
			m, ok := val.(map[string]any)
			if !ok {
				typeErr(key, "map", val)
				continue
			}
			// Field-merge nested thresholds against existing ones (or
			// start from the kind-default if none were set).
			var thr ClassificationThresholds
			if result.Thresholds != nil {
				thr = *result.Thresholds
			} else {
				thr = DefaultThresholdsFor(result.Kind)
			}
			if terrs := mergeThresholds(&thr, m); len(terrs) > 0 {
				for _, e := range terrs {
					errs = append(errs, fmt.Errorf("thresholds: %w", e))
				}
			}
			result.Thresholds = &thr
		case "aggregation":
			m, ok := val.(map[string]any)
			if !ok {
				typeErr(key, "map", val)
				continue
			}
			// Replace-semantics: aggregation is a tightly-coupled triplet;
			// overlay replaces rather than field-merges. See mergeAggregation.
			var agg AggregationSpec
			aerrs := mergeAggregation(&agg, m)
			if len(aerrs) > 0 {
				for _, e := range aerrs {
					errs = append(errs, fmt.Errorf("aggregation: %w", e))
				}
				// Skip assignment on any merge error so a partially-built or
				// zero-value AggregationSpec cannot leak downstream. Validate()
				// on a half-populated spec produces a confusing message; we'd
				// rather the overlay load fail loudly and the caller fix the
				// YAML than ship a silently-broken registry entry.
				continue
			}
			result.Aggregation = &agg
		}
	}
	return result, errs
}

// mergeAggregation populates an AggregationSpec from overlay fields.
// Unlike mergeThresholds, it does not inherit from a base: aggregation is
// a tightly-coupled triplet (group_by/within_group/across_groups) and
// mixing user fields with base fields can produce nonsensical
// combinations, so callers pass a zero-value dst and restate every
// relevant field. Reducers are validated inline so typos surface at the
// specific field instead of later via an opaque empty-string Validate
// message. Unknown keys are reported as errors (defense-in-depth against
// typos like "acros_groups").
func mergeAggregation(dst *AggregationSpec, overlay map[string]any) []error {
	var errs []error
	for key, val := range overlay {
		switch key {
		case "group_by":
			list, ok := val.([]any)
			if !ok {
				errs = append(errs, fmt.Errorf("field %q must be list of strings, got %T", key, val))
				continue
			}
			dst.GroupBy = dst.GroupBy[:0]
			for i, item := range list {
				s, ok := item.(string)
				if !ok {
					errs = append(errs, fmt.Errorf("field %q[%d] must be string, got %T", key, i, item))
					continue
				}
				dst.GroupBy = append(dst.GroupBy, s)
			}
		case "within_group":
			s, ok := val.(string)
			if !ok {
				errs = append(errs, fmt.Errorf("field %q must be string, got %T", key, val))
				continue
			}
			r := Reducer(s)
			if !r.IsValid() {
				errs = append(errs, fmt.Errorf("field %q: invalid reducer %q (want mean|sum|max|min)", key, s))
				continue
			}
			dst.WithinGroup = r
		case "across_groups":
			s, ok := val.(string)
			if !ok {
				errs = append(errs, fmt.Errorf("field %q must be string, got %T", key, val))
				continue
			}
			r := Reducer(s)
			if !r.IsValid() {
				errs = append(errs, fmt.Errorf("field %q: invalid reducer %q (want mean|sum|max|min)", key, s))
				continue
			}
			dst.AcrossGroups = r
		default:
			errs = append(errs, fmt.Errorf("unknown field %q", key))
		}
	}
	return errs
}

// mergeThresholds applies overlay threshold fields on top of an existing
// ClassificationThresholds struct. Fields not present in the overlay are
// left as-is; values of the wrong type are reported via the returned
// error slice so operators can spot typos in their overlay files.
func mergeThresholds(dst *ClassificationThresholds, overlay map[string]any) []error {
	var errs []error
	for key, val := range overlay {
		switch key {
		case "significant_delta_pct", "breach_ratio_for_regression", "cv_for_noisy", "spike_zscore":
		default:
			continue
		}
		f, ok := toFloat64(val)
		if !ok {
			errs = append(errs, fmt.Errorf("field %q must be number, got %T", key, val))
			continue
		}
		// Second switch applies the value. The first switch (lines 349-353)
		// validates key presence by continuing on unknown keys, so reaching
		// here means key is one of the known valid cases.
		switch key { //nolint:GoDfaInspectionRunner // DFA detects unreachable cases, but second switch is intentionally exhaustive for validated keys
		case "significant_delta_pct":
			dst.SignificantDeltaPct = f
		case "breach_ratio_for_regression":
			dst.BreachRatioForRegress = f
		case "cv_for_noisy":
			dst.CVForNoisy = f
		case "spike_zscore":
			dst.SpikeZScore = f
		}
	}
	return errs
}

// toFloat64 accepts the numeric forms yaml.v3 can produce (int, int64,
// float64) and returns a float64. Other types are rejected.
func toFloat64(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	}
	return 0, false
}

// parseRegistryBytes parses raw YAML bytes into a RegistryConfig and
// validates every entry. It is used for both the embedded default registry
// and user-supplied overlay files.
func parseRegistryBytes(data []byte) (*RegistryConfig, error) {
	var cfg RegistryConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg.Metrics == nil {
		cfg.Metrics = make(map[string]MetricMeta)
	}
	for name, meta := range cfg.Metrics {
		if err := meta.Validate(name); err != nil {
			return nil, fmt.Errorf("invalid registry entry %q: %w", name, err)
		}
	}
	return &cfg, nil
}

// Lookup returns configured metadata if present, otherwise falls back to
// auto-detection from naming conventions.
func (r *Registry) Lookup(metricType string) MetricMeta {
	if meta, ok := r.metrics[metricType]; ok {
		return meta
	}
	return autoDetect(metricType)
}

type MetricListEntry struct {
	MetricType      string          `json:"metric_type"`
	Kind            MetricKind      `json:"kind"`
	Unit            string          `json:"unit"`
	BetterDirection BetterDirection `json:"better_direction"`
	SLOThreshold    *float64        `json:"slo_threshold,omitempty"`
	RelatedMetrics  []string        `json:"related_metrics,omitempty"`
	AutoDetected    bool            `json:"auto_detected,omitempty"`
}

// List returns registry entries matching the given filters. The match
// substring is compared (case-insensitive) against three things in order:
// the full metric type, the auto-derived service token (e.g. "pubsub" from
// "pubsub.googleapis.com/..."), and the metric's Keywords. The first hit
// wins. This lets callers find metrics by category synonyms like "queue",
// "cache", or "database" even when the metric name doesn't contain that
// word.
func (r *Registry) List(match string, kind MetricKind) []MetricListEntry {
	lowerMatch := strings.ToLower(match)
	var result []MetricListEntry
	for name, meta := range r.metrics {
		if kind != "" && meta.Kind != kind {
			continue
		}
		if lowerMatch != "" && !metricMatches(name, meta, lowerMatch) {
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

// metricMatches reports whether a metric satisfies the search substring.
// lowerMatch must already be lower-cased by the caller.
func metricMatches(name string, meta MetricMeta, lowerMatch string) bool {
	if strings.Contains(strings.ToLower(name), lowerMatch) {
		return true
	}
	if token := serviceToken(name); token != "" && strings.Contains(token, lowerMatch) {
		return true
	}
	for _, k := range meta.Keywords {
		if strings.Contains(strings.ToLower(k), lowerMatch) {
			return true
		}
	}
	return false
}

// serviceToken extracts the leading service identifier from a GCP metric
// type. The convention is "<service>.googleapis.com/<path>" or
// "<service>.io/<path>" (kubernetes.io). Returns the substring before the
// first '.' or '/', lower-cased. For custom metrics
// ("custom.googleapis.com/...") this returns "custom", which is acceptable
// because the main name-substring path covers them.
func serviceToken(metricType string) string {
	lower := strings.ToLower(metricType)
	// Cut at the first '.' or '/' — whichever comes first.
	end := len(lower)
	if i := strings.IndexAny(lower, "./"); i >= 0 {
		end = i
	}
	return lower[:end]
}

func (r *Registry) Count() int {
	return len(r.metrics)
}

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
	// Freshness must run BEFORE latency — some lag metrics contain the
	// substring "seconds" or "duration" ("lag_seconds", "staleness_duration")
	// and would otherwise be misclassified as latency, which has different
	// noise characteristics and stricter CV thresholds.
	case containsAny(lower, "lag", "staleness", "freshness", "_age", "seconds_since", "oldest_unacked"):
		meta.Kind = KindFreshness
		meta.Unit = "seconds"
		meta.BetterDirection = DirectionDown
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
