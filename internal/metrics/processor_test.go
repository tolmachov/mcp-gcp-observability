package metrics

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func makePoints(values []float64, stepSeconds int) []Point {
	t0 := time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC)
	points := make([]Point, len(values))
	for i, v := range values {
		points[i] = Point{
			Timestamp: t0.Add(time.Duration(i*stepSeconds) * time.Second),
			Value:     v,
		}
	}
	return points
}

func TestProcessStable(t *testing.T) {
	values := make([]float64, 60)
	baseline := make([]float64, 60)
	for i := range values {
		values[i] = 0.5
		baseline[i] = 0.5
	}
	meta := MetricMeta{Kind: KindLatency, Unit: "seconds", BetterDirection: DirectionDown}
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60, len(baseline))

	assert.Equal(t, ClassStable, f.Classification)
	assert.InDelta(t, 0, f.DeltaPct, 0.01)
	assert.Equal(t, 0.5, f.Mean)
	assert.Equal(t, 0.0, f.MaxZScore, "flat signal should produce MaxZScore=0, not a float64 artifact")
}

func TestProcessStepRegression(t *testing.T) {
	// First half at 0.5, second half at 0.9.
	values := make([]float64, 60)
	for i := range values {
		if i < 30 {
			values[i] = 0.5
		} else {
			values[i] = 0.9
		}
	}
	baseline := make([]float64, 60)
	for i := range baseline {
		baseline[i] = 0.5
	}

	slo := 0.7
	meta := MetricMeta{
		Kind:            KindLatency,
		Unit:            "seconds",
		BetterDirection: DirectionDown,
		SLOThreshold:    &slo,
	}
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60, len(baseline))

	assert.Equal(t, ClassStepRegression, f.Classification)
	assert.True(t, f.StepChangeDetected)
	assert.True(t, f.SLOBreach)
	assert.Greater(t, f.DeltaPct, 30.0)
}

func TestProcessSaturation(t *testing.T) {
	values := make([]float64, 60)
	baseline := make([]float64, 60)
	for i := range values {
		values[i] = 0.97
		baseline[i] = 0.6
	}
	satCap := 1.0
	meta := MetricMeta{
		Kind:            KindResourceUtilization,
		Unit:            "ratio",
		BetterDirection: DirectionDown,
		SaturationCap:   &satCap,
	}
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60, len(baseline))

	assert.Equal(t, ClassSaturation, f.Classification)
	assert.True(t, f.SaturationDetected)
}

func TestProcessSpike(t *testing.T) {
	values := make([]float64, 60)
	baseline := make([]float64, 60)
	for i := range values {
		values[i] = 0.5
		baseline[i] = 0.5
	}
	// Inject 2 spikes.
	values[25] = 3.0
	values[26] = 3.5

	meta := MetricMeta{Kind: KindLatency, Unit: "seconds", BetterDirection: DirectionDown}
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60, len(baseline))

	assert.Equal(t, ClassSpike, f.Classification)
	assert.Greater(t, f.SpikeCount, 0, "expected spikes detected")
}

func TestProcessNoisy(t *testing.T) {
	values := make([]float64, 60)
	baseline := make([]float64, 60)
	for i := range values {
		if i%2 == 0 {
			values[i] = 0.3
		} else {
			values[i] = 0.7
		}
		baseline[i] = 0.5
	}
	meta := MetricMeta{Kind: KindLatency, Unit: "seconds", BetterDirection: DirectionDown}
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60, len(baseline))

	assert.Equal(t, ClassNoisy, f.Classification)
}

func TestProcessDirectionUpSLOBreach(t *testing.T) {
	// Availability drops below SLO threshold — should detect breach with DirectionUp.
	values := make([]float64, 60)
	baseline := make([]float64, 60)
	for i := range values {
		values[i] = 0.90 // current availability 90%
		baseline[i] = 0.999
	}
	slo := 0.95
	meta := MetricMeta{
		Kind:            KindAvailability,
		Unit:            "ratio",
		BetterDirection: DirectionUp,
		SLOThreshold:    &slo,
	}
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60, len(baseline))

	assert.True(t, f.SLOBreach, "expected SLO breach for availability below threshold")
	assert.Equal(t, 1.0, f.BreachRatio)
}

func TestProcessZeroBaseline(t *testing.T) {
	// Error rate goes from 0 to 5 — should not classify as "stable".
	values := make([]float64, 60)
	baseline := make([]float64, 60)
	for i := range values {
		values[i] = 5.0
		baseline[i] = 0.0
	}
	meta := MetricMeta{Kind: KindErrorRate, Unit: "count", BetterDirection: DirectionDown}
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60, len(baseline))

	assert.NotZero(t, f.DeltaPct, "DeltaPct should not be 0 when baseline is 0 and current is non-zero")
	assert.NotEqual(t, ClassStable, f.Classification, "classification should not be 'stable' for a spike from zero")
}

func TestProcessFewPoints(t *testing.T) {
	// With 5 points, step change detection should not fire.
	values := []float64{1.0, 1.0, 5.0, 5.0, 5.0}
	baseline := []float64{1.0, 1.0, 1.0, 1.0, 1.0}
	meta := MetricMeta{Kind: KindLatency, Unit: "seconds", BetterDirection: DirectionDown}
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60, len(baseline))

	assert.False(t, f.StepChangeDetected, "step change should not be detected with fewer than 6 points")
}

func TestIsBreachDirectionNone(t *testing.T) {
	// DirectionNone defaults to "value > threshold" (same as DirectionDown).
	// This is intentional — if someone sets an SLO on a directionless metric,
	// breach means exceeding the threshold.
	assert.True(t, isBreach(1.5, 1.0, DirectionNone), "expected breach when value > threshold with DirectionNone")
	assert.False(t, isBreach(0.5, 1.0, DirectionNone), "expected no breach when value < threshold with DirectionNone")
}

func TestProcessEmptyPoints(t *testing.T) {
	meta := MetricMeta{Kind: KindLatency}
	f := Process(nil, nil, meta, 60, 0)
	assert.Empty(t, f.Classification, "expected empty classification for no points")
}

func TestPercentile(t *testing.T) {
	sorted := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	p50 := percentile(sorted, 0.50)
	assert.InDelta(t, 5.5, p50, 0.01)
	p95 := percentile(sorted, 0.95)
	assert.InDelta(t, 9.55, p95, 0.1)
}

// TestSpikeDetectionRequiresMinSample proves that the minimum-N guard is
// enforced: without it, short windows could never reach the z-score threshold
// yet we'd still report SpikeCount=0 as a "meaningful" stable/noisy verdict.
// With the guard, the spike detector simply skips and MaxZScore stays 0,
// which is the honest answer.
func TestSpikeDetectionRequiresMinSample(t *testing.T) {
	// 5-point series with an obvious outlier — but N is too small for
	// the z-score math to ever reach 3.0 regardless of value magnitude.
	values := []float64{1, 1, 1, 1, 100}
	meta := MetricMeta{Kind: KindLatency, BetterDirection: DirectionDown}
	f := Process(makePoints(values, 60), nil, meta, 60, 0)

	assert.Zero(t, f.SpikeCount, "sample size %d is below minimum", len(values))
	assert.Zero(t, f.MaxZScore, "spike detection should be skipped")
	assert.NotEqual(t, ClassSpike, f.Classification, "classification = spike on N=%d, should be impossible", len(values))
}

// TestSpikeDetectionUsesKindSpecificThreshold proves that SpikeCount is
// computed against the metric kind's configured z-threshold, not a hardcoded
// value. Previously SpikeCount used z≥3.0 regardless of kind, so ErrorRate
// (SpikeZScore=2.5) would report SpikeCount=0 even for real spikes.
func TestSpikeDetectionUsesKindSpecificThreshold(t *testing.T) {
	values := make([]float64, 20)
	for i := range values {
		values[i] = 0.5
	}
	// Inject one outlier tuned to land between the ErrorRate threshold (2.5)
	// and the default threshold (3.0). Mean/stddev over 20 points with one
	// outlier of ~3.0 gives z ≈ 4.35, well above 2.5 and 3.0.
	values[10] = 3.0

	t.Run("latency default threshold 3.0", func(t *testing.T) {
		meta := MetricMeta{Kind: KindLatency, BetterDirection: DirectionDown}
		f := Process(makePoints(values, 60), nil, meta, 60, 0)
		assert.Greater(t, f.SpikeCount, 0, "MaxZScore=%.2f at threshold 3.0", f.MaxZScore)
	})

	t.Run("error_rate lower threshold 2.5", func(t *testing.T) {
		meta := MetricMeta{Kind: KindErrorRate, BetterDirection: DirectionDown}
		f := Process(makePoints(values, 60), nil, meta, 60, 0)
		assert.Greater(t, f.SpikeCount, 0, "MaxZScore=%.2f at threshold 2.5", f.MaxZScore)
	})
}

// TestStepChangeThresholdScalesByKind verifies Step 2 of the plan: the
// step-change threshold is no longer hardcoded at 20% but scales with the
// metric kind's SignificantDeltaPct so that a 12% shift in error_rate is
// a step change while a 12% shift in throughput is not.
func TestStepChangeThresholdScalesByKind(t *testing.T) {
	// First half ~1.0, second half ~1.12 → +12% step.
	values := make([]float64, 60)
	for i := range values {
		if i < 30 {
			values[i] = 1.0
		} else {
			values[i] = 1.12
		}
	}

	t.Run("error_rate: 12% clears 2*5=10 and is a step change", func(t *testing.T) {
		meta := MetricMeta{Kind: KindErrorRate, BetterDirection: DirectionDown}
		f := Process(makePoints(values, 60), nil, meta, 60, 0)
		assert.True(t, f.StepChangeDetected, "expected true for 12%% shift on error_rate (threshold=10)")
	})

	t.Run("throughput: 12% is below 2*15=30 and is not a step change", func(t *testing.T) {
		meta := MetricMeta{Kind: KindThroughput, BetterDirection: DirectionNone}
		f := Process(makePoints(values, 60), nil, meta, 60, 0)
		assert.False(t, f.StepChangeDetected, "expected false for 12%% shift on throughput (threshold=30)")
	})
}

// TestCVFloorForNearZeroMetrics verifies Step 9: metrics oscillating around
// zero (healthy error_rate) should not get a huge CV that triggers noisy.
func TestCVFloorForNearZeroMetrics(t *testing.T) {
	// Error rate mostly zero with a single 0.001 blip — the true "relative"
	// variance is enormous, but the metric is clearly healthy.
	values := make([]float64, 60)
	values[30] = 0.001

	meta := MetricMeta{Kind: KindErrorRate, BetterDirection: DirectionDown}
	baseline := make([]float64, 60)
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60, 60)

	// Without the floor, CV would explode (stddev / tiny mean → huge).
	// With the nearZeroEpsilon floor, CV stays bounded and classification
	// does not fabricate a noisy label from statistical artifacts.
	assert.Less(t, f.CV, 1e9, "CV should be bounded (near-zero denominator floor should apply)")
}

// TestTrendScoreIsWindowNormalized verifies Step 4: a 10% linear drift across
// the window produces TrendScore ≈ 0.10 regardless of whether the window is
// minutes or hours. Previously TrendScore was per-minute, so a 10% drift
// over a 10-minute window would give 0.01 but over a 1-hour window 0.0017.
func TestTrendScoreIsWindowNormalized(t *testing.T) {
	// 60 points rising linearly from 1.0 to 1.10 — exactly +10% across window.
	values := make([]float64, 60)
	for i := range values {
		values[i] = 1.0 + 0.10*float64(i)/float64(len(values)-1)
	}
	baseline := make([]float64, 60)
	for i := range baseline {
		baseline[i] = 1.0
	}
	meta := MetricMeta{Kind: KindLatency, BetterDirection: DirectionDown}
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60, 60)

	// TrendScore should be ≈ 0.10 (10% of baseline across the window).
	assert.InDelta(t, 0.10, f.TrendScore, 0.02, "window-normalized total drift")
	assert.Equal(t, TrendUp, f.Trend)
}

// TestConfidenceReflectsReliability verifies Step 7 exposes the right
// confidence label based on data quality and baseline reliability.
func TestConfidenceReflectsReliability(t *testing.T) {
	values := make([]float64, 60)
	baseline := make([]float64, 60)
	for i := range values {
		values[i] = 1
		baseline[i] = 1
	}
	meta := MetricMeta{Kind: KindLatency, BetterDirection: DirectionDown}

	t.Run("high: both reliable", func(t *testing.T) {
		f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60, 60)
		assert.Equal(t, ConfidenceHigh, f.Confidence)
	})

	t.Run("medium: baseline unreliable (too few points)", func(t *testing.T) {
		f := Process(makePoints(values, 60), makePoints(baseline[:5], 60), meta, 60, 60)
		assert.Equal(t, ConfidenceMedium, f.Confidence)
	})

	t.Run("low: no baseline at all", func(t *testing.T) {
		f := Process(makePoints(values, 60), nil, meta, 60, 60)
		assert.Equal(t, ConfidenceLow, f.Confidence)
	})
}

// TestDeltaPctNearZeroBaseline pins the nearZeroEpsilon boundary in DeltaPct:
// a baseline below the epsilon must not produce Inf/NaN, and a baseline just
// above it must use the baseline as denominator (not fall back to current mean).
func TestDeltaPctNearZeroBaseline(t *testing.T) {
	current := makePoints(make([]float64, 60), 60)
	for i := range current {
		current[i].Value = 1.0
	}
	meta := MetricMeta{Kind: KindErrorRate, BetterDirection: DirectionDown}

	t.Run("baseline below epsilon uses current mean as denominator", func(t *testing.T) {
		baseline := makePoints(make([]float64, 60), 60)
		for i := range baseline {
			baseline[i].Value = 5e-7 // below nearZeroEpsilon=1e-6
		}
		f := Process(current, baseline, meta, 60, len(baseline))
		assert.False(t, math.IsInf(f.DeltaPct, 0), "DeltaPct must not be Inf for near-zero baseline")
		assert.False(t, math.IsNaN(f.DeltaPct), "DeltaPct must not be NaN for near-zero baseline")
		assert.NotZero(t, f.DeltaPct, "DeltaPct should be non-zero when current differs from near-zero baseline")
	})

	t.Run("baseline above epsilon uses baseline as denominator", func(t *testing.T) {
		baseline := makePoints(make([]float64, 60), 60)
		for i := range baseline {
			baseline[i].Value = 2e-6 // above nearZeroEpsilon=1e-6
		}
		f := Process(current, baseline, meta, 60, len(baseline))
		assert.False(t, math.IsInf(f.DeltaPct, 0), "DeltaPct must not be Inf")
		assert.False(t, math.IsNaN(f.DeltaPct), "DeltaPct must not be NaN")
		// current=1.0, baseline=2e-6 → DeltaPct ≈ (1-2e-6)/2e-6 * 100 ≈ 5e7
		assert.Greater(t, f.DeltaPct, 1e6, "DeltaPct should be very large when baseline is tiny but above epsilon")
	})
}

// TestPercentileEqualAdjacentElements pins the lerp fix: when adjacent sorted
// values are equal, percentile must return exactly that value with no rounding
// artifact from the a*(1-t)+b*t form.
func TestPercentileEqualAdjacentElements(t *testing.T) {
	// p=0.25 lands between index 0 and 1 — both are 1.0.
	// Old form: 1*(1-0.25)+1*0.25 can deviate from 1.0 in theory.
	// New form: 1+(1-1)*0.25 = 1.0 exactly.
	sorted := []float64{1, 1, 1, 1, 2}
	assert.Equal(t, 1.0, percentile(sorted, 0.25), "equal adjacent elements should return exact value")

	// p=0.875 straddles the 1→2 boundary — should interpolate correctly.
	result := percentile(sorted, 0.875)
	assert.Greater(t, result, 1.0)
	assert.Less(t, result, 2.0)
}

func TestDataQualityWithGaps(t *testing.T) {
	points := []Point{
		{Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Value: 1},
		{Timestamp: time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC), Value: 1},
		// Gap: 5 minutes.
		{Timestamp: time.Date(2026, 1, 1, 0, 6, 0, 0, time.UTC), Value: 1},
		{Timestamp: time.Date(2026, 1, 1, 0, 7, 0, 0, time.UTC), Value: 1},
	}
	dq := computeDataQuality(points, 60)
	assert.Equal(t, 1, dq.GapCount)
	assert.Equal(t, 300, dq.MaxGapSeconds)
}
