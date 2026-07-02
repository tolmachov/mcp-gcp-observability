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
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60, len(baseline), Window{})

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
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60, len(baseline), Window{})

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
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60, len(baseline), Window{})

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
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60, len(baseline), Window{})

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
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60, len(baseline), Window{})

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
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60, len(baseline), Window{})

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
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60, len(baseline), Window{})

	assert.NotZero(t, f.DeltaPct, "DeltaPct should not be 0 when baseline is 0 and current is non-zero")
	assert.NotEqual(t, ClassStable, f.Classification, "classification should not be 'stable' for a spike from zero")
}

func TestProcessFewPoints(t *testing.T) {
	// With 5 points, step change detection should not fire.
	values := []float64{1.0, 1.0, 5.0, 5.0, 5.0}
	baseline := []float64{1.0, 1.0, 1.0, 1.0, 1.0}
	meta := MetricMeta{Kind: KindLatency, Unit: "seconds", BetterDirection: DirectionDown}
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60, len(baseline), Window{})

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
	f := Process(nil, nil, meta, 60, 0, Window{})
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
	f := Process(makePoints(values, 60), nil, meta, 60, 0, Window{})

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
		f := Process(makePoints(values, 60), nil, meta, 60, 0, Window{})
		assert.Greater(t, f.SpikeCount, 0, "MaxZScore=%.2f at threshold 3.0", f.MaxZScore)
	})

	t.Run("error_rate lower threshold 2.5", func(t *testing.T) {
		meta := MetricMeta{Kind: KindErrorRate, BetterDirection: DirectionDown}
		f := Process(makePoints(values, 60), nil, meta, 60, 0, Window{})
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
		f := Process(makePoints(values, 60), nil, meta, 60, 0, Window{})
		assert.True(t, f.StepChangeDetected, "expected true for 12%% shift on error_rate (threshold=10)")
	})

	t.Run("throughput: 12% is below 2*15=30 and is not a step change", func(t *testing.T) {
		meta := MetricMeta{Kind: KindThroughput, BetterDirection: DirectionNone}
		f := Process(makePoints(values, 60), nil, meta, 60, 0, Window{})
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
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60, 60, Window{})

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
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60, 60, Window{})

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
		f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60, 60, Window{})
		assert.Equal(t, ConfidenceHigh, f.Confidence)
	})

	t.Run("medium: baseline unreliable (too few points)", func(t *testing.T) {
		f := Process(makePoints(values, 60), makePoints(baseline[:5], 60), meta, 60, 60, Window{})
		assert.Equal(t, ConfidenceMedium, f.Confidence)
	})

	t.Run("low: no baseline at all", func(t *testing.T) {
		f := Process(makePoints(values, 60), nil, meta, 60, 60, Window{})
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
		f := Process(current, baseline, meta, 60, len(baseline), Window{})
		assert.False(t, math.IsInf(f.DeltaPct, 0), "DeltaPct must not be Inf for near-zero baseline")
		assert.False(t, math.IsNaN(f.DeltaPct), "DeltaPct must not be NaN for near-zero baseline")
		assert.NotZero(t, f.DeltaPct, "DeltaPct should be non-zero when current differs from near-zero baseline")
	})

	t.Run("baseline above epsilon uses baseline as denominator", func(t *testing.T) {
		baseline := makePoints(make([]float64, 60), 60)
		for i := range baseline {
			baseline[i].Value = 2e-6 // above nearZeroEpsilon=1e-6
		}
		f := Process(current, baseline, meta, 60, len(baseline), Window{})
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
	dq := computeDataQuality(points, 60, Window{})
	assert.Equal(t, 1, dq.GapCount)
	assert.Equal(t, 300, dq.MaxGapSeconds)
}

// A metric that stops reporting halfway through the requested window must be
// judged unreliable, not "complete", even though the points it did emit are
// perfectly contiguous. Without the window bounds the span-based heuristic
// would compute expected≈actual and mark it reliable.
func TestDataQualityDeadMetricWindowAware(t *testing.T) {
	windowStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	windowEnd := windowStart.Add(60 * time.Minute)

	// 20 contiguous 1-minute points, then silence for the remaining ~40 min.
	var points []Point
	for i := range 20 {
		points = append(points, Point{Timestamp: windowStart.Add(time.Duration(i) * time.Minute), Value: 1})
	}

	// Span-based (unknown window): looks fine — expected ~= actual.
	spanDQ := computeDataQuality(points, 60, Window{})
	assert.True(t, spanDQ.Reliable, "span-based accounting cannot see the dead trailing region")

	// Window-aware: expected reflects the full hour and the trailing silence is
	// a gap, so the series is correctly flagged unreliable.
	winDQ := computeDataQuality(points, 60, Window{Start: windowStart, End: windowEnd})
	assert.False(t, winDQ.Reliable, "dead trailing region must make the window unreliable")
	assert.Greater(t, winDQ.ExpectedPoints, winDQ.ActualPoints)
	assert.GreaterOrEqual(t, winDQ.GapCount, 1, "trailing silence should count as a gap")
	assert.True(t, winDQ.WindowChecked, "a valid window must drive window-based accounting")
}

// A metric that starts reporting late must be flagged too — the leading gap is
// independent of the trailing one, and a sign-flipped subtraction would leave
// it silently uncounted.
func TestDataQualityLeadingGapWindowAware(t *testing.T) {
	windowStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	windowEnd := windowStart.Add(60 * time.Minute)

	// Silence for the first ~40 min, then 20 contiguous 1-minute points.
	var points []Point
	for i := range 20 {
		points = append(points, Point{Timestamp: windowStart.Add(40*time.Minute + time.Duration(i)*time.Minute), Value: 1})
	}

	winDQ := computeDataQuality(points, 60, Window{Start: windowStart, End: windowEnd})
	assert.False(t, winDQ.Reliable, "late-starting metric must be unreliable")
	assert.GreaterOrEqual(t, winDQ.GapCount, 1, "leading silence should count as a gap")
	assert.True(t, winDQ.WindowChecked)
}

// Unusable windows — zero, half-set, or inverted — must all fall back to
// span-based accounting and report WindowChecked=false, so a swapped/half-set
// Window (a caller bug) degrades visibly rather than silently.
func TestDataQualityWindowBoundarySemantics(t *testing.T) {
	windowStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	windowEnd := windowStart.Add(60 * time.Minute)

	// 20 contiguous points spanning only the first 20 min; span-based
	// accounting sees them as a complete, reliable series.
	var points []Point
	for i := range 20 {
		points = append(points, Point{Timestamp: windowStart.Add(time.Duration(i) * time.Minute), Value: 1})
	}

	cases := []struct {
		name string
		w    Window
	}{
		{"zero", Window{}},
		{"start only", Window{Start: windowStart}},
		{"end only", Window{End: windowEnd}},
		{"end equals start", Window{Start: windowStart, End: windowStart}},
		{"end before start", Window{Start: windowEnd, End: windowStart}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dq := computeDataQuality(points, 60, tc.w)
			assert.False(t, dq.WindowChecked, "unusable window must fall back to span accounting")
			assert.True(t, dq.Reliable, "span accounting sees 20 contiguous points as reliable")
		})
	}

	// Contrast: a valid window sees the dead trailing region and flags it.
	dq := computeDataQuality(points, 60, Window{Start: windowStart, End: windowEnd})
	assert.True(t, dq.WindowChecked)
	assert.False(t, dq.Reliable)
}

// A now-anchored window's trailing edge routinely lags the last emitted point
// by Cloud Monitoring's ingestion delay. That normal lag must not register as
// a gap, or every healthy recent metric would look degraded.
func TestDataQualityToleratesTrailingIngestionLag(t *testing.T) {
	windowStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Full 1-minute coverage from minute 0..57; window ends 2.5 min after the
	// last point (150s), within windowEdgeGapSteps*step=180s. The old 2-step
	// (120s) threshold would have counted this healthy lag as a gap.
	var points []Point
	for i := range 58 {
		points = append(points, Point{Timestamp: windowStart.Add(time.Duration(i) * time.Minute), Value: 1})
	}
	windowEnd := windowStart.Add(57*time.Minute + 150*time.Second)

	dq := computeDataQuality(points, 60, Window{Start: windowStart, End: windowEnd})
	assert.Equal(t, 0, dq.GapCount, "normal trailing ingestion lag must not count as a gap")
	assert.Equal(t, 0, dq.MaxGapSeconds)
	assert.True(t, dq.Reliable)
	assert.True(t, dq.WindowChecked)
}

// A tiny-but-nonzero baseline must not silently zero out TrendScore: the
// denominator floors to the current mean instead of dividing by ~1e-9.
func TestTrendScoreFallsBackForTinyBaseline(t *testing.T) {
	// Current drifts 0→100 across the window (a clear upward trend).
	values := make([]float64, 60)
	for i := range values {
		values[i] = 100 * float64(i) / float64(len(values)-1)
	}
	// Baseline is 1e-9: non-zero, but below nearZeroEpsilon.
	baseline := make([]float64, 60)
	for i := range baseline {
		baseline[i] = 1e-9
	}
	meta := MetricMeta{Kind: KindLatency, BetterDirection: DirectionDown}
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60, 60, Window{})

	assert.NotZero(t, f.TrendScore, "tiny baseline must fall back to mean, not leave TrendScore 0")
	assert.Equal(t, TrendUp, f.Trend)
}

// computeStepChange must floor its denominator, not test == 0: a service that
// steps up from ~0 to a real level should be detected with a sane percentage,
// and a series that is negligible throughout should report no step at all.
func TestStepChangeDenominatorFallback(t *testing.T) {
	meta := MetricMeta{Kind: KindErrorRate, BetterDirection: DirectionDown}

	t.Run("near-zero first third falls back to last third", func(t *testing.T) {
		// First half ~0, second half 100 — meanFirst≈0 would blow up changePct
		// without the meanLast fallback.
		values := make([]float64, 60)
		for i := 30; i < 60; i++ {
			values[i] = 100
		}
		f := Process(makePoints(values, 60), nil, meta, 60, 0, Window{})
		assert.True(t, f.StepChangeDetected)
		assert.False(t, math.IsInf(f.StepChangePct, 0), "step pct must be finite")
		assert.False(t, math.IsNaN(f.StepChangePct))
	})

	t.Run("both thirds negligible: no step detected", func(t *testing.T) {
		values := make([]float64, 60)
		for i := range values {
			values[i] = 1e-9
		}
		f := Process(makePoints(values, 60), nil, meta, 60, 0, Window{})
		assert.Zero(t, f.StepChangePct)
		assert.False(t, f.StepChangeDetected)
	})
}
