package metrics

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"os"
	"slices"
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
	// names is the sorted key set of metrics, computed once at construction
	// (the registry is immutable afterwards).
	names []string
}

func newRegistry(m map[string]MetricMeta) *Registry {
	return &Registry{metrics: m, names: slices.Sorted(maps.Keys(m))}
}

// NewRegistry creates an empty registry that relies on auto-detection only.
// Use LoadRegistry("") if you want the embedded GCP defaults instead.
func NewRegistry() *Registry {
	return newRegistry(make(map[string]MetricMeta))
}

// NewRegistryFromMetaMap creates a Registry directly from a MetricMeta map,
// bypassing YAML parsing and load-time validation. Intended for tests that
// need to inject configurations (e.g. invalid AggregationSpec) that LoadRegistry
// would otherwise reject.
func NewRegistryFromMetaMap(m map[string]MetricMeta) *Registry {
	return newRegistry(maps.Clone(m))
}

// LoadRegistry loads a YAML overlay merged on top of embedded defaults.
// Merges field-by-field with explicit overwrites; related_metrics and
// keywords are extended (set-union). Validation runs on merged result.
// If path is empty, only embedded defaults are used.
func LoadRegistry(path string) (*Registry, error) {
	var base RegistryConfig
	if err := decodeRegistryYAML(defaultRegistryYAML, &base); err != nil {
		return nil, fmt.Errorf("parsing embedded default registry: %w", err)
	}
	if base.Metrics == nil {
		base.Metrics = make(map[string]MetricMeta)
	}
	for name, meta := range base.Metrics {
		if err := meta.Validate(name); err != nil {
			return nil, fmt.Errorf("invalid embedded registry entry %q: %w", name, err)
		}
	}

	if path == "" {
		return newRegistry(base.Metrics), nil
	}

	// The registry path is an operator-supplied config file (METRICS_REGISTRY_FILE
	// / --metrics-registry); loading it by that exact path is the whole point.
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is an intentional operator-provided config file
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var overlay struct {
		Metrics map[string]metricOverlay `yaml:"metrics"`
	}
	if err := decodeRegistryYAML(data, &overlay); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if err := rejectOverlayNulls(data); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	// Validate every entry in sorted order for deterministic, complete error reporting.
	var overlayErrs []error
	for _, name := range slices.Sorted(maps.Keys(overlay.Metrics)) {
		merged := overlay.Metrics[name].applyTo(base.Metrics[name])
		if err := merged.Validate(name); err != nil {
			overlayErrs = append(overlayErrs, fmt.Errorf("overlay metric %q: %w", name, err))
			continue
		}
		base.Metrics[name] = merged
	}
	if len(overlayErrs) > 0 {
		return nil, errors.Join(overlayErrs...)
	}

	return newRegistry(base.Metrics), nil
}

// decodeRegistryYAML decodes registry YAML into v, rejecting unknown keys so
// a typo like "acros_groups" fails the load instead of being ignored. Type
// mismatches are all collected by the decoder into one error with line
// numbers. An empty document decodes to the zero value.
func decodeRegistryYAML(data []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("decoding registry YAML: %w", err)
	}
	return nil
}

// rejectOverlayNulls fails on an explicit null (`key:`, `key: null`, `~`)
// in an overlay metric entry, reporting every one with its line. The typed
// decode cannot tell a null from an absent key — both leave the field nil —
// so a null meant to clear a base value would be silently ignored.
func rejectOverlayNulls(data []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("decoding registry YAML: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil
	}
	var errs []error
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "metrics" {
			continue
		}
		entries := root.Content[i+1]
		for j := 0; j+1 < len(entries.Content); j += 2 {
			errs = appendNulls(errs, entries.Content[j+1], "metrics."+entries.Content[j].Value)
		}
	}
	return errors.Join(errs...)
}

// appendNulls appends an error for every null node in the tree at n, whose
// YAML path is path.
func appendNulls(errs []error, n *yaml.Node, path string) []error {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.ShortTag() == "!!null" {
			errs = append(errs, fmt.Errorf("line %d: %s is null; omit the key to keep the base value", n.Line, path))
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			errs = appendNulls(errs, n.Content[i+1], path+"."+n.Content[i].Value)
		}
	case yaml.SequenceNode:
		for i, item := range n.Content {
			errs = appendNulls(errs, item, fmt.Sprintf("%s[%d]", path, i))
		}
	}
	return errs
}

// metricOverlay is one metric entry of a registry overlay file. Pointer and
// slice fields are nil when the key is absent (rejectOverlayNulls rules out
// explicit nulls), so only keys present in the YAML touch the base entry.
type metricOverlay struct {
	Kind            *MetricKind        `yaml:"kind"`
	Unit            *string            `yaml:"unit"`
	BetterDirection *BetterDirection   `yaml:"better_direction"`
	SLOThreshold    *float64           `yaml:"slo_threshold"`
	SaturationCap   *float64           `yaml:"saturation_cap"`
	RelatedMetrics  []string           `yaml:"related_metrics"`
	Keywords        []string           `yaml:"keywords"`
	Thresholds      *thresholdsOverlay `yaml:"thresholds"`
	// Aggregation has replace semantics: it is a tightly-coupled triplet
	// (group_by/within_group/across_groups), and mixing overlay fields with
	// base fields could produce nonsensical combinations.
	Aggregation *AggregationSpec `yaml:"aggregation"`
}

// thresholdsOverlay field-merges into the base (or kind-default) thresholds.
type thresholdsOverlay struct {
	SignificantDeltaPct   *float64 `yaml:"significant_delta_pct"`
	BreachRatioForRegress *float64 `yaml:"breach_ratio_for_regression"`
	CVForNoisy            *float64 `yaml:"cv_for_noisy"`
	SpikeZScore           *float64 `yaml:"spike_zscore"`
}

// applyTo returns base with the overlay applied: scalar fields replace,
// related_metrics and keywords are set-unioned (keywords case-insensitively),
// thresholds field-merge and aggregation replaces. base is not mutated.
func (o metricOverlay) applyTo(base MetricMeta) MetricMeta {
	m := base
	setIfPresent(&m.Kind, o.Kind)
	setIfPresent(&m.Unit, o.Unit)
	setIfPresent(&m.BetterDirection, o.BetterDirection)
	if o.SLOThreshold != nil {
		m.SLOThreshold = o.SLOThreshold
	}
	if o.SaturationCap != nil {
		m.SaturationCap = o.SaturationCap
	}
	m.RelatedMetrics = unionStrings(base.RelatedMetrics, o.RelatedMetrics, func(s string) string { return s })
	m.Keywords = unionStrings(base.Keywords, o.Keywords, strings.ToLower)
	if o.Thresholds != nil {
		// Start from the existing thresholds, or the defaults of the
		// (possibly just overridden) kind.
		thr := DefaultThresholdsFor(m.Kind)
		if base.Thresholds != nil {
			thr = *base.Thresholds
		}
		setIfPresent(&thr.SignificantDeltaPct, o.Thresholds.SignificantDeltaPct)
		setIfPresent(&thr.BreachRatioForRegress, o.Thresholds.BreachRatioForRegress)
		setIfPresent(&thr.CVForNoisy, o.Thresholds.CVForNoisy)
		setIfPresent(&thr.SpikeZScore, o.Thresholds.SpikeZScore)
		m.Thresholds = &thr
	}
	if o.Aggregation != nil {
		m.Aggregation = o.Aggregation
	}
	return m
}

func setIfPresent[T any](dst, v *T) {
	if v != nil {
		*dst = *v
	}
}

// unionStrings returns base followed by the items of extra whose key is not
// already present, preserving order. base is never mutated.
func unionStrings(base, extra []string, key func(string) string) []string {
	if len(extra) == 0 {
		return base
	}
	out := slices.Clone(base)
	seen := make(map[string]bool, len(base)+len(extra))
	for _, s := range base {
		seen[key(s)] = true
	}
	for _, s := range extra {
		if k := key(s); !seen[k] {
			seen[k] = true
			out = append(out, s)
		}
	}
	return out
}

// Lookup returns configured metadata if present, otherwise falls back to
// auto-detection from naming conventions.
func (r *Registry) Lookup(metricType string) MetricMeta {
	if meta, ok := r.metrics[metricType]; ok {
		return meta
	}
	return autoDetect(metricType)
}

// List yields the registry entries matching the given filters, in metric
// type order. The match substring is compared (case-insensitive) against
// three things in order: the full metric type, the auto-derived service
// token (e.g. "pubsub" from "pubsub.googleapis.com/..."), and the metric's
// Keywords. This lets callers find metrics by category
// synonyms like "queue", "cache", or "database" even when the metric name
// doesn't contain that word.
func (r *Registry) List(match string, kind MetricKind) iter.Seq2[string, MetricMeta] {
	lowerMatch := strings.ToLower(match)
	return func(yield func(string, MetricMeta) bool) {
		for _, name := range r.names {
			meta := r.metrics[name]
			if kind != "" && meta.Kind != kind {
				continue
			}
			if lowerMatch != "" && !metricMatches(name, meta, lowerMatch) {
				continue
			}
			if !yield(name, meta) {
				return
			}
		}
	}
}

// Names returns every configured metric type, sorted. The slice is shared;
// callers must not modify it.
func (r *Registry) Names() []string {
	return r.names
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
