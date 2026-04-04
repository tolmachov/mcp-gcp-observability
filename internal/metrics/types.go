package metrics

import (
	"fmt"
	"time"
)

// MetricKind classifies what a metric measures.
type MetricKind string

const (
	KindLatency             MetricKind = "latency"
	KindThroughput          MetricKind = "throughput"
	KindErrorRate           MetricKind = "error_rate"
	KindResourceUtilization MetricKind = "resource_utilization"
	KindSaturation          MetricKind = "saturation"
	KindAvailability        MetricKind = "availability"
	KindBusinessKPI         MetricKind = "business_kpi"
	KindUnknown             MetricKind = "unknown"
)

// validMetricKinds is the set of valid MetricKind values for input validation.
var validMetricKinds = map[MetricKind]bool{
	KindLatency: true, KindThroughput: true, KindErrorRate: true,
	KindResourceUtilization: true, KindSaturation: true, KindAvailability: true,
	KindBusinessKPI: true, KindUnknown: true,
}

// IsValid returns true if the MetricKind is one of the defined constants.
func (k MetricKind) IsValid() bool {
	return validMetricKinds[k]
}

// ValidMetricKindsForInput returns the kinds accepted as user input (excludes unknown and business_kpi).
func ValidMetricKindsForInput() []string {
	return []string{"latency", "throughput", "error_rate", "resource_utilization", "saturation", "availability"}
}

// BetterDirection indicates which direction of change is desirable.
type BetterDirection string

const (
	DirectionDown BetterDirection = "down"
	DirectionUp   BetterDirection = "up"
	DirectionNone BetterDirection = "none"
)

// IsValid returns true if the BetterDirection is one of the defined constants.
func (d BetterDirection) IsValid() bool {
	return d == DirectionDown || d == DirectionUp || d == DirectionNone
}

// MetricMeta holds semantic information about a metric.
type MetricMeta struct {
	Kind            MetricKind              `yaml:"kind" json:"kind"`
	Unit            string                  `yaml:"unit" json:"unit"`
	BetterDirection BetterDirection         `yaml:"better_direction" json:"better_direction"`
	SLOThreshold    *float64                `yaml:"slo_threshold,omitempty" json:"slo_threshold,omitempty"`
	SaturationCap   *float64                `yaml:"saturation_cap,omitempty" json:"saturation_cap,omitempty"`
	RelatedMetrics  []string                `yaml:"related_metrics,omitempty" json:"related_metrics,omitempty"`
	Thresholds      *ClassificationThresholds `yaml:"thresholds,omitempty" json:"-"`
	AutoDetected    bool                    `yaml:"-" json:"auto_detected,omitempty"`
}

// EffectiveThresholds returns thresholds from the meta or defaults for the kind.
// If custom thresholds are set but invalid (e.g. zero-value struct), defaults are used instead.
func (m MetricMeta) EffectiveThresholds() ClassificationThresholds {
	if m.Thresholds != nil {
		if m.Thresholds.Validate() == nil {
			return *m.Thresholds
		}
	}
	return DefaultThresholdsFor(m.Kind)
}

// Validate checks that MetricMeta fields have valid values.
func (m MetricMeta) Validate(name string) error {
	if !m.Kind.IsValid() {
		return fmt.Errorf("metric %q: invalid kind %q", name, m.Kind)
	}
	if !m.BetterDirection.IsValid() {
		return fmt.Errorf("metric %q: invalid better_direction %q", name, m.BetterDirection)
	}
	if m.SLOThreshold != nil && *m.SLOThreshold < 0 {
		return fmt.Errorf("metric %q: slo_threshold must be non-negative", name)
	}
	if m.SaturationCap != nil && *m.SaturationCap <= 0 {
		return fmt.Errorf("metric %q: saturation_cap must be positive", name)
	}
	if m.Thresholds != nil {
		if err := m.Thresholds.Validate(); err != nil {
			return fmt.Errorf("metric %q: thresholds: %w", name, err)
		}
	}
	return nil
}

// ClassificationThresholds controls the sensitivity of classification.
type ClassificationThresholds struct {
	SignificantDeltaPct   float64 `yaml:"significant_delta_pct"`
	BreachRatioForRegress float64 `yaml:"breach_ratio_for_regression"`
	CVForNoisy            float64 `yaml:"cv_for_noisy"`
	SpikeZScore           float64 `yaml:"spike_zscore"`
}

// Validate checks that threshold values are within valid ranges.
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

// DefaultThresholdsFor returns the default thresholds for the given metric kind.
func DefaultThresholdsFor(kind MetricKind) ClassificationThresholds {
	if t, ok := defaultThresholds[kind]; ok {
		return t
	}
	return defaultThresholds[KindUnknown]
}

// defaultThresholds maps each kind to its default classification thresholds.
var defaultThresholds = map[MetricKind]ClassificationThresholds{
	KindLatency:             {SignificantDeltaPct: 10, BreachRatioForRegress: 0.30, CVForNoisy: 0.30, SpikeZScore: 3.0},
	KindErrorRate:           {SignificantDeltaPct: 5, BreachRatioForRegress: 0.20, CVForNoisy: 0.50, SpikeZScore: 2.5},
	KindThroughput:          {SignificantDeltaPct: 15, BreachRatioForRegress: 0.30, CVForNoisy: 0.25, SpikeZScore: 3.0},
	KindSaturation:          {SignificantDeltaPct: 5, BreachRatioForRegress: 0.50, CVForNoisy: 0.15, SpikeZScore: 3.0},
	KindResourceUtilization: {SignificantDeltaPct: 10, BreachRatioForRegress: 0.40, CVForNoisy: 0.20, SpikeZScore: 3.0},
	KindAvailability:        {SignificantDeltaPct: 5, BreachRatioForRegress: 0.20, CVForNoisy: 0.30, SpikeZScore: 3.0},
	KindBusinessKPI:         {SignificantDeltaPct: 10, BreachRatioForRegress: 0.30, CVForNoisy: 0.30, SpikeZScore: 3.0},
	KindUnknown:             {SignificantDeltaPct: 10, BreachRatioForRegress: 0.30, CVForNoisy: 0.30, SpikeZScore: 3.0},
}

// Point is a single time series data point.
type Point struct {
	Timestamp time.Time
	Value     float64
}

// SignalFeatures holds all computed statistical features for a time window.
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

	Baseline       float64
	BaselineStddev float64
	BaselineReliable bool
	DeltaAbs         float64
	DeltaPct         float64

	Trend       string
	SlopePerMin float64
	TrendScore  float64

	StepChangeDetected bool
	StepChangePct      float64
	StepChangeAt       *time.Time

	MaxZScore  float64
	SpikeCount int
	SpikeRatio float64

	SLOBreach                 bool
	BreachRatio               float64
	BreachDurationSeconds     int
	CurrentBreachStreakSeconds int

	SaturationDetected bool

	DataQuality DataQuality

	Classification Classification
}

// DataQuality describes the completeness of the time series data.
type DataQuality struct {
	ExpectedPoints int  `json:"expected_points"`
	ActualPoints   int  `json:"actual_points"`
	GapCount       int  `json:"gap_count"`
	MaxGapSeconds  int  `json:"max_gap_seconds"`
	Reliable       bool `json:"reliable"`
}
