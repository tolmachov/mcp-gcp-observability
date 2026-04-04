package metrics

import (
	"math"
	"sort"
	"time"
)

// Process computes all statistical features for the given points relative to a baseline.
func Process(points, baselinePoints []Point, meta MetricMeta, stepSeconds int) SignalFeatures {
	var f SignalFeatures

	if len(points) == 0 {
		return f
	}

	// Copy before sorting to avoid mutating the caller's slice.
	pts := make([]Point, len(points))
	copy(pts, points)
	points = pts

	// Sort by timestamp.
	sort.Slice(points, func(i, j int) bool {
		return points[i].Timestamp.Before(points[j].Timestamp)
	})

	values := extractValues(points)
	f.Current = values[len(values)-1]

	// Basic statistics.
	f.Mean = mean(values)
	f.Stddev = stddev(values, f.Mean)
	if f.Mean != 0 {
		f.CV = f.Stddev / math.Abs(f.Mean)
	}

	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)

	f.Min = sorted[0]
	f.Max = sorted[len(sorted)-1]
	f.P50 = percentile(sorted, 0.50)
	f.P95 = percentile(sorted, 0.95)
	f.P99 = percentile(sorted, 0.99)

	// Tail ratio (latency only).
	if meta.Kind == KindLatency && f.P50 > 0 {
		f.TailRatio = f.P99 / f.P50
	}

	// Baseline comparison.
	computeBaseline(&f, baselinePoints)

	// Trend via linear regression.
	computeTrend(&f, points)

	// Step change detection.
	computeStepChange(&f, points)

	// Spike detection.
	computeSpikes(&f, values)

	// SLO breach.
	computeSLOBreach(&f, points, meta, stepSeconds)

	// Saturation.
	computeSaturation(&f, values, meta)

	// Data quality.
	f.DataQuality = computeDataQuality(points, stepSeconds)

	// Classification.
	f.Classification = Classify(&f, meta)

	return f
}

func computeBaseline(f *SignalFeatures, baselinePoints []Point) {
	if len(baselinePoints) == 0 {
		return
	}
	bValues := extractValues(baselinePoints)
	f.Baseline = mean(bValues)
	f.BaselineStddev = stddev(bValues, f.Baseline)
	f.BaselineReliable = len(bValues) >= 7 // Minimum for reliable percentile estimates.

	f.DeltaAbs = f.Mean - f.Baseline
	if f.Baseline != 0 {
		f.DeltaPct = (f.DeltaAbs / math.Abs(f.Baseline)) * 100
	} else if f.Mean != 0 {
		// Baseline is zero but current is not — treat as a large deviation.
		// Use current mean as denominator to produce a meaningful percentage.
		f.DeltaPct = (f.DeltaAbs / math.Abs(f.Mean)) * 100
	}
}

func computeTrend(f *SignalFeatures, points []Point) {
	if len(points) < 3 {
		f.Trend = "flat"
		return
	}

	// Linear regression: y = a + b*x, where x is minutes from first point.
	t0 := points[0].Timestamp
	var sumX, sumY, sumXY, sumX2 float64
	n := float64(len(points))
	for _, p := range points {
		x := p.Timestamp.Sub(t0).Minutes()
		sumX += x
		sumY += p.Value
		sumXY += x * p.Value
		sumX2 += x * x
	}

	denom := n*sumX2 - sumX*sumX
	if denom == 0 {
		f.Trend = "flat"
		return
	}

	slope := (n*sumXY - sumX*sumY) / denom
	f.SlopePerMin = slope

	if f.Baseline != 0 {
		f.TrendScore = slope / math.Abs(f.Baseline)
	} else if f.Mean != 0 {
		f.TrendScore = slope / math.Abs(f.Mean)
	}

	switch {
	case f.TrendScore > 0.01:
		f.Trend = "up"
	case f.TrendScore < -0.01:
		f.Trend = "down"
	default:
		f.Trend = "flat"
	}
}

func computeStepChange(f *SignalFeatures, points []Point) {
	n := len(points)
	if n < 6 {
		return
	}

	third := n / 3
	firstThird := extractValues(points[:third])
	lastThird := extractValues(points[n-third:])

	meanFirst := mean(firstThird)
	meanLast := mean(lastThird)

	denom := math.Abs(meanFirst)
	if denom == 0 {
		denom = math.Abs(meanLast)
	}
	if denom == 0 {
		return
	}

	changePct := (meanLast - meanFirst) / denom * 100
	f.StepChangePct = changePct

	if math.Abs(changePct) > 20 {
		f.StepChangeDetected = true

		// Estimate step change time: find the midpoint crossing.
		midValue := (meanFirst + meanLast) / 2
		for i := third; i < n-third; i++ {
			if (meanLast > meanFirst && points[i].Value >= midValue) ||
				(meanLast < meanFirst && points[i].Value <= midValue) {
				t := points[i].Timestamp
				f.StepChangeAt = &t
				break
			}
		}
	}
}

func computeSpikes(f *SignalFeatures, values []float64) {
	if len(values) < 3 {
		return
	}

	m := mean(values)
	s := stddev(values, m)
	if s == 0 {
		return
	}

	for _, v := range values {
		z := math.Abs(v-m) / s
		if z > f.MaxZScore {
			f.MaxZScore = z
		}
		if z >= 3.0 { // Fixed threshold for counting spikes (distinct from configurable SpikeZScore used in classification).
			f.SpikeCount++
		}
	}
	f.SpikeRatio = float64(f.SpikeCount) / float64(len(values))
}

func computeSLOBreach(f *SignalFeatures, points []Point, meta MetricMeta, stepSeconds int) {
	if meta.SLOThreshold == nil {
		return
	}
	threshold := *meta.SLOThreshold
	step := time.Duration(stepSeconds) * time.Second
	if step == 0 {
		step = 60 * time.Second
	}

	var breachCount int
	var breachDuration time.Duration
	var currentStreak time.Duration
	var streakBroken bool

	// Iterate backwards from the most recent point to compute the current breach streak.
	// The streak accumulates from the last point until the first non-breach is encountered.
	for i := len(points) - 1; i >= 0; i-- {
		breached := isBreach(points[i].Value, threshold, meta.BetterDirection)
		if breached {
			breachCount++
			breachDuration += step
			if !streakBroken {
				currentStreak += step
			}
		} else {
			streakBroken = true
		}
	}

	f.SLOBreach = breachCount > 0
	f.BreachRatio = float64(breachCount) / float64(len(points))
	f.BreachDurationSeconds = int(breachDuration.Seconds())
	f.CurrentBreachStreakSeconds = int(currentStreak.Seconds())
}

func isBreach(value, threshold float64, dir BetterDirection) bool {
	switch dir {
	case DirectionDown:
		return value > threshold
	case DirectionUp:
		return value < threshold
	default:
		return value > threshold
	}
}

func computeSaturation(f *SignalFeatures, values []float64, meta MetricMeta) {
	if meta.SaturationCap == nil || len(values) == 0 {
		return
	}
	satCap := *meta.SaturationCap

	// Check mean of last 10% of points.
	tailSize := len(values) / 10
	if tailSize < 1 {
		tailSize = 1
	}
	tail := values[len(values)-tailSize:]
	tailMean := mean(tail)

	f.SaturationDetected = tailMean >= 0.95*satCap // Within 5% of capacity ceiling.
}

func computeDataQuality(points []Point, stepSeconds int) DataQuality {
	if len(points) == 0 {
		return DataQuality{Reliable: false}
	}
	if stepSeconds <= 0 {
		stepSeconds = 60
	}

	step := time.Duration(stepSeconds) * time.Second
	windowDuration := points[len(points)-1].Timestamp.Sub(points[0].Timestamp)
	expected := int(windowDuration/step) + 1

	var gapCount int
	var maxGap time.Duration
	for i := 1; i < len(points); i++ {
		gap := points[i].Timestamp.Sub(points[i-1].Timestamp)
		if gap > 2*step {
			gapCount++
			if gap > maxGap {
				maxGap = gap
			}
		}
	}

	actual := len(points)
	reliable := actual >= expected/2 && gapCount <= 3

	return DataQuality{
		ExpectedPoints: expected,
		ActualPoints:   actual,
		GapCount:       gapCount,
		MaxGapSeconds:  int(maxGap.Seconds()),
		Reliable:       reliable,
	}
}

// --- math helpers ---

func extractValues(points []Point) []float64 {
	vals := make([]float64, len(points))
	for i, p := range points {
		vals[i] = p.Value
	}
	return vals
}

func mean(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

// stddev computes the population standard deviation (divides by N, not N-1).
func stddev(vals []float64, m float64) float64 {
	if len(vals) < 2 {
		return 0
	}
	var sum float64
	for _, v := range vals {
		d := v - m
		sum += d * d
	}
	return math.Sqrt(sum / float64(len(vals)))
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	idx := p * float64(len(sorted)-1)
	lower := int(math.Floor(idx))
	upper := int(math.Ceil(idx))
	if lower == upper {
		return sorted[lower]
	}
	frac := idx - float64(lower)
	return sorted[lower]*(1-frac) + sorted[upper]*frac
}
