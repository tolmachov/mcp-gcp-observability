package metrics

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ErrInvalidAggregationSpec is returned (wrapped) when a caller hands an
// AggregationSpec that fails Validate to the query layer. Tool handlers use
// errors.Is to distinguish registry misconfiguration from transient GCP
// errors and escalate the log level accordingly.
var ErrInvalidAggregationSpec = errors.New("invalid aggregation spec")

type MetricKind string

const (
	KindLatency             MetricKind = "latency"
	KindThroughput          MetricKind = "throughput"
	KindErrorRate           MetricKind = "error_rate"
	KindResourceUtilization MetricKind = "resource_utilization"
	KindSaturation          MetricKind = "saturation"
	KindAvailability        MetricKind = "availability"
	KindBusinessKPI         MetricKind = "business_kpi"
	// KindFreshness covers data-age / lag metrics: replication lag, Kafka
	// consumer lag, PubSub pull lag, CDC lag, data pipeline staleness,
	// "seconds since last success" metrics. Semantically distinct from
	// latency (which measures request duration) because the distribution
	// is shaped by sync cycles: freshness grows monotonically until the
	// next successful sync resets it, instead of being request-driven.
	KindFreshness MetricKind = "freshness"
	KindUnknown   MetricKind = "unknown"
)

var validMetricKinds = map[MetricKind]bool{
	KindLatency: true, KindThroughput: true, KindErrorRate: true,
	KindResourceUtilization: true, KindSaturation: true, KindAvailability: true,
	KindBusinessKPI: true, KindFreshness: true, KindUnknown: true,
}

func (k MetricKind) IsValid() bool {
	return validMetricKinds[k]
}

// ValidMetricKindsForInput returns the kinds accepted as user input
// (excludes KindUnknown). Derived from validMetricKinds so it stays
// in sync automatically when new kinds are added.
func ValidMetricKindsForInput() []string {
	result := make([]string, 0, len(validMetricKinds)-1)
	for k := range validMetricKinds {
		if k != KindUnknown {
			result = append(result, string(k))
		}
	}
	sort.Strings(result)
	return result
}

type BetterDirection string

const (
	DirectionDown BetterDirection = "down"
	DirectionUp   BetterDirection = "up"
	DirectionNone BetterDirection = "none"
)

func (d BetterDirection) IsValid() bool {
	return d == DirectionDown || d == DirectionUp || d == DirectionNone
}

type MetricMeta struct {
	Kind            MetricKind      `yaml:"kind" json:"kind"`
	Unit            string          `yaml:"unit" json:"unit"`
	BetterDirection BetterDirection `yaml:"better_direction" json:"better_direction"`
	SLOThreshold    *float64        `yaml:"slo_threshold,omitempty" json:"slo_threshold,omitempty"`
	SaturationCap   *float64        `yaml:"saturation_cap,omitempty" json:"saturation_cap,omitempty"`
	RelatedMetrics  []string        `yaml:"related_metrics,omitempty" json:"related_metrics,omitempty"`
	// Keywords are additional search tokens used by Registry.List when
	// matching a user-supplied substring. They let callers find a metric by
	// service category or use-case synonym (e.g. "queue", "cache",
	// "database", "nosql") even when the metric name doesn't contain that
	// word. Not returned in tool output — this is a search-side index.
	Keywords   []string                  `yaml:"keywords,omitempty" json:"-"`
	Thresholds *ClassificationThresholds `yaml:"thresholds,omitempty" json:"-"`
	// Aggregation declares how to collapse the metric's time series into a
	// single scalar for snapshot/compare/related tools. If nil, the
	// per-kind default from DefaultAggregation is used. Callers should go
	// through MetricMeta.ResolveAggregation, which handles the fallback
	// and defensively clones GroupBy.
	Aggregation  *AggregationSpec `yaml:"aggregation,omitempty" json:"-"`
	AutoDetected bool             `yaml:"-" json:"auto_detected,omitempty"`
}

// Reducer names the cross-series reduction strategy. Values map to Cloud
// Monitoring Aggregation_Reducer values (REDUCE_MEAN/SUM/MAX/MIN).
type Reducer string

const (
	ReducerMean Reducer = "mean"
	ReducerSum  Reducer = "sum"
	ReducerMax  Reducer = "max"
	ReducerMin  Reducer = "min"
)

// IsValid reports whether the reducer is one of the supported values. The
// empty string is NOT valid; use DefaultAggregation() to pick a fallback
// when the user hasn't set an explicit reducer.
func (r Reducer) IsValid() bool {
	switch r {
	case ReducerMean, ReducerSum, ReducerMax, ReducerMin:
		return true
	}
	return false
}

// AggregationSpec describes how to aggregate a metric's time series into a
// single scalar. Two forms are supported:
//
//  1. Single-stage (GroupBy empty): AcrossGroups is applied directly as the
//     CrossSeriesReducer in one Cloud Monitoring query.
//  2. Two-stage (GroupBy non-empty): the tool first queries with
//     GroupByFields=GroupBy and Reducer=WithinGroup — this collapses
//     dimensions like rolling-deploy pod replicas down to one series per
//     entity. Then the tool post-processes the per-group values in Go using
//     AcrossGroups to produce the final scalar. This is the only way to
//     express PromQL-style "MAX by entity → SUM" in a single MCP call
//     because the Cloud Monitoring v3 Aggregation API accepts only one
//     CrossSeriesReducer per request (last verified 2026-04 against the
//     projects.timeSeries.list reference; re-check this paragraph if
//     chained reducers ever become supported).
type AggregationSpec struct {
	// GroupBy is the first-stage grouping key set (e.g. ["game_id"]). When
	// empty, only AcrossGroups is used.
	GroupBy []string `yaml:"group_by,omitempty"`
	// WithinGroup is the reducer applied within each GroupBy bucket. Only
	// meaningful when GroupBy is non-empty; required in that case.
	WithinGroup Reducer `yaml:"within_group,omitempty"`
	// AcrossGroups is the reducer applied across all series (single-stage)
	// or across the per-group values produced by the first stage
	// (two-stage). Required whenever AggregationSpec is set.
	AcrossGroups Reducer `yaml:"across_groups,omitempty"`
}

// groupByLabelPrefixes is the set of qualifier prefixes Cloud Monitoring
// accepts for GroupByFields. An entry that does not start with any of
// these is almost certainly a registry typo (e.g. bare "game_id" where
// the author meant "metric.labels.game_id"), which we saw silently
// collapse two-stage aggregations to a single group in prod — the
// symptom was an empty result + a confusing SingleGroup warning deep
// inside QueryTimeSeriesAggregated. Rejecting it at registry load time
// surfaces the mistake immediately at the YAML that owns it. See
// https://cloud.google.com/monitoring/api/ref_v3/rest/v3/projects.timeSeries/list#Aggregation
// for the canonical list.
//
// "resource.type" is a separate exact-match qualifier (no `.labels.`
// namespace) and is handled outside this slice by hasKnownGroupByPrefix.
var groupByLabelPrefixes = []string{
	"metric.labels.",
	"resource.labels.",
	"metadata.system_labels.",
	"metadata.user_labels.",
}

// Validate enforces the schema rules described on AggregationSpec.
func (a AggregationSpec) Validate() error {
	// Accumulate every issue with errors.Join so an operator fixing a
	// broken registry sees them all in one load pass instead of one per
	// re-run. errors.Is(err, ErrInvalidAggregationSpec) still works
	// through joined errors, so escalation in tool callers is unaffected.
	var errs []error
	if !a.AcrossGroups.IsValid() {
		errs = append(errs, fmt.Errorf("across_groups must be one of mean|sum|max|min, got %q", a.AcrossGroups))
	}
	if len(a.GroupBy) == 0 && a.WithinGroup != "" {
		errs = append(errs, fmt.Errorf("within_group %q is set but group_by is empty — within_group is only meaningful in two-stage aggregation", a.WithinGroup))
	}
	if len(a.GroupBy) > 0 {
		if !a.WithinGroup.IsValid() {
			errs = append(errs, fmt.Errorf("within_group must be one of mean|sum|max|min when group_by is set, got %q", a.WithinGroup))
		}
		for i, k := range a.GroupBy {
			if k == "" {
				errs = append(errs, fmt.Errorf("group_by[%d] must be non-empty", i))
				continue
			}
			if !hasKnownGroupByPrefix(k) {
				errs = append(errs, fmt.Errorf("group_by[%d] = %q must start with one of metric.labels. | resource.labels. | metadata.system_labels. | metadata.user_labels. | resource.type (Cloud Monitoring qualifier — bare label names are silently dropped by the API)", i, k))
			}
		}
	}
	if joined := errors.Join(errs...); joined != nil {
		return fmt.Errorf("%w: %w", ErrInvalidAggregationSpec, joined)
	}
	return nil
}

// hasKnownGroupByPrefix reports whether key is qualified with one of the
// Cloud Monitoring GroupByFields namespaces. "resource.type" is matched
// as an exact key because it has no ".labels." suffix; everything else
// must carry a non-empty tail after the namespace prefix (so a bare
// "metric.labels." with nothing after it is rejected).
func hasKnownGroupByPrefix(key string) bool {
	if key == "resource.type" {
		return true
	}
	for _, p := range groupByLabelPrefixes {
		if strings.HasPrefix(key, p) && len(key) > len(p) {
			return true
		}
	}
	return false
}

// IsTwoStage reports whether the aggregation requires the two-stage
// (group-by + post-process) flow.
func (a AggregationSpec) IsTwoStage() bool {
	return len(a.GroupBy) > 0
}

// EffectiveThresholds falls back to kind defaults when custom thresholds are
// absent or invalid (e.g. a zero-value struct from partial YAML overlays).
func (m MetricMeta) EffectiveThresholds() ClassificationThresholds {
	if m.Thresholds != nil {
		if m.Thresholds.Validate() == nil {
			return *m.Thresholds
		}
	}
	return DefaultThresholdsFor(m.Kind)
}

func (m MetricMeta) Validate(name string) error {
	// Accumulate every issue with errors.Join so an operator editing a
	// registry entry sees all the things they got wrong in one go.
	var errs []error
	if !m.Kind.IsValid() {
		errs = append(errs, fmt.Errorf("metric %q: invalid kind %q", name, m.Kind))
	}
	if !m.BetterDirection.IsValid() {
		errs = append(errs, fmt.Errorf("metric %q: invalid better_direction %q", name, m.BetterDirection))
	}
	if m.SLOThreshold != nil && *m.SLOThreshold < 0 {
		errs = append(errs, fmt.Errorf("metric %q: slo_threshold must be non-negative", name))
	}
	if m.SaturationCap != nil && *m.SaturationCap <= 0 {
		errs = append(errs, fmt.Errorf("metric %q: saturation_cap must be positive", name))
	}
	if m.Thresholds != nil {
		if err := m.Thresholds.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("metric %q: thresholds: %w", name, err))
		}
	}
	if m.Aggregation != nil {
		if err := m.Aggregation.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("metric %q: aggregation: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

type ClassificationThresholds struct {
	SignificantDeltaPct   float64 `yaml:"significant_delta_pct"`
	BreachRatioForRegress float64 `yaml:"breach_ratio_for_regression"`
	CVForNoisy            float64 `yaml:"cv_for_noisy"`
	SpikeZScore           float64 `yaml:"spike_zscore"`
}

func (t ClassificationThresholds) Validate() error {
	if t.SignificantDeltaPct <= 0 {
		return fmt.Errorf("significant_delta_pct must be positive, got %v", t.SignificantDeltaPct)
	}
	if t.BreachRatioForRegress < 0 || t.BreachRatioForRegress > 1 {
		return fmt.Errorf("breach_ratio_for_regression must be in [0,1], got %v", t.BreachRatioForRegress)
	}
	if t.CVForNoisy <= 0 {
		return fmt.Errorf("cv_for_noisy must be positive, got %v", t.CVForNoisy)
	}
	if t.SpikeZScore <= 0 {
		return fmt.Errorf("spike_zscore must be positive, got %v", t.SpikeZScore)
	}
	return nil
}

func DefaultThresholdsFor(kind MetricKind) ClassificationThresholds {
	if t, ok := defaultThresholds[kind]; ok {
		return t
	}
	return defaultThresholds[KindUnknown]
}

var defaultThresholds = map[MetricKind]ClassificationThresholds{
	KindLatency:             {SignificantDeltaPct: 10, BreachRatioForRegress: 0.30, CVForNoisy: 0.30, SpikeZScore: 3.0},
	KindErrorRate:           {SignificantDeltaPct: 5, BreachRatioForRegress: 0.20, CVForNoisy: 0.50, SpikeZScore: 2.5},
	KindThroughput:          {SignificantDeltaPct: 15, BreachRatioForRegress: 0.30, CVForNoisy: 0.25, SpikeZScore: 3.0},
	KindSaturation:          {SignificantDeltaPct: 5, BreachRatioForRegress: 0.50, CVForNoisy: 0.15, SpikeZScore: 3.0},
	KindResourceUtilization: {SignificantDeltaPct: 10, BreachRatioForRegress: 0.40, CVForNoisy: 0.20, SpikeZScore: 3.0},
	KindAvailability:        {SignificantDeltaPct: 5, BreachRatioForRegress: 0.20, CVForNoisy: 0.30, SpikeZScore: 3.0},
	KindBusinessKPI:         {SignificantDeltaPct: 10, BreachRatioForRegress: 0.30, CVForNoisy: 0.30, SpikeZScore: 3.0},
	// Freshness: lag metrics naturally sawtooth with each sync cycle, so
	// CVForNoisy is high (0.50 like error_rate) to avoid false "noisy"
	// labels. BreachRatioForRegress is lower (0.20) because any sustained
	// breach on a freshness SLO is meaningful — unlike latency where brief
	// breaches on tail requests are expected.
	KindFreshness: {SignificantDeltaPct: 15, BreachRatioForRegress: 0.20, CVForNoisy: 0.50, SpikeZScore: 3.0},
	KindUnknown:   {SignificantDeltaPct: 10, BreachRatioForRegress: 0.30, CVForNoisy: 0.30, SpikeZScore: 3.0},
}

// Confidence summarizes how much the caller should trust the classification.
// It is derived from data quality and baseline reliability, not from raw
// signal strength.
type Confidence string

const (
	// ConfidenceHigh means both the current window and the baseline were
	// computed from reliable data (enough points, no large gaps).
	ConfidenceHigh Confidence = "high"
	// ConfidenceMedium means exactly one of {current window data, baseline}
	// is unreliable. The classification is still surfaced but the caller
	// should be cautious before acting on it.
	ConfidenceMedium Confidence = "medium"
	// ConfidenceLow means neither side is reliable (or no baseline at all),
	// and the classification has been downgraded away from regression-like
	// labels because the underlying data cannot support them.
	ConfidenceLow Confidence = "low"
)

// TrendFlatBand is the threshold (as a fraction of baseline per window) below
// which a trend is considered "flat". A TrendScore of 0.02 means the metric
// drifted by 2% of baseline across the measured window. This is independent
// of window length by construction.
const TrendFlatBand = 0.02

// TrendStrongBand is the threshold (as a fraction of baseline per window)
// above which a trend is considered strong enough to drive a
// sustained_regression or recovery classification. It is strictly larger
// than TrendFlatBand so a metric can be "trending up" without automatically
// being classified as regressing.
const TrendStrongBand = 0.05

// TrendDirection describes the direction a metric's value is moving across its
// window, as computed by linear regression in computeTrend.
type TrendDirection string

const (
	TrendFlat TrendDirection = "flat"
	TrendUp   TrendDirection = "up"
	TrendDown TrendDirection = "down"
)

func (t TrendDirection) IsValid() bool {
	return t == TrendFlat || t == TrendUp || t == TrendDown
}

type Point struct {
	Timestamp time.Time
	Value     float64
}

type SignalFeatures struct {
	Current float64
	Mean    float64
	P50     float64
	P95     float64
	P99     float64
	Min     float64
	Max     float64
	Stddev  float64
	CV      float64

	TailRatio float64

	Baseline           float64
	BaselineStddev     float64
	BaselineReliable   bool
	BaselinePointCount int
	DeltaAbs           float64
	DeltaPct           float64

	Trend       TrendDirection
	SlopePerMin float64
	TrendScore  float64

	StepChangeDetected bool
	StepChangePct      float64
	StepChangeAt       *time.Time

	MaxZScore  float64
	SpikeCount int
	SpikeRatio float64

	SLOBreach                  bool
	BreachRatio                float64
	BreachDurationSeconds      int
	CurrentBreachStreakSeconds int
	// BreachTransitions is the number of times the series crossed the SLO
	// threshold (breach↔non-breach boundary). Used for flapping detection:
	// a high transition rate with moderate BreachRatio indicates oscillation.
	BreachTransitions int

	SaturationDetected bool

	DataQuality DataQuality

	Classification Classification
	Confidence     Confidence
}

type DataQuality struct {
	ExpectedPoints int  `json:"expected_points"`
	ActualPoints   int  `json:"actual_points"`
	GapCount       int  `json:"gap_count"`
	MaxGapSeconds  int  `json:"max_gap_seconds"`
	Reliable       bool `json:"reliable"`
}
