package metrics

import (
	"math"
	"sort"
	"time"
)

// minPointsForSpikeDetection: minimum sample size for z-score spike detection (~3.0 threshold).
const minPointsForSpikeDetection = 10

// minBaselinePoints is the absolute floor below which a baseline is never
// considered reliable, regardless of the expected point count.
const minBaselinePoints = 7

// minCurrentWindowPoints is the absolute floor for a current window to be
// considered reliable. Without this, expected/2 with expected==1 reduces to
// 0 via integer division (1/2==0), making a single-point window always
// "reliable" and enabling false regression classifications.
const minCurrentWindowPoints = 3

// windowEdgeGapSteps is the gap tolerance, in step multiples, applied to the
// leading/trailing gaps between the requested window bounds and the first/last
// observed points. It is more lenient than the 2-step inter-point tolerance
// because the window boundary rarely aligns with emission times and — on a
// now-anchored trailing edge — absorbs Cloud Monitoring's ingestion lag. A
// genuinely dead region (many steps of silence) still exceeds it.
const windowEdgeGapSteps = 3

// nearZeroEpsilon: floors |Mean| in CV to avoid false noisy classification
// for metrics oscillating near zero (e.g., error_rate when healthy).
const nearZeroEpsilon = 1e-6

// saturationApproachFraction: the tail mean is considered saturated once it
// reaches this fraction of the configured capacity ceiling (within 5%).
const saturationApproachFraction = 0.95

// Process computes statistical features relative to a raw baseline.
// expectedBaselinePoints is the expected baseline point count for reliability
// checks (0 if unknown). window is the requested wall-clock range used for
// data-quality reliability; pass the zero Window when the range is unknown to
// fall back to span-based accounting (see Window).
func Process(points, baselinePoints []Point, meta MetricMeta, stepSeconds, expectedBaselinePoints int, window Window) SignalFeatures {
	baseline := ComputeBaselineStats(baselinePoints, expectedBaselinePoints)
	return ProcessWithBaselineStats(points, baseline, meta, stepSeconds, window)
}

// ProcessWithBaselineStats computes features using precomputed baseline stats
// (e.g. same_weekday_hour with median/MAD). window is the requested wall-clock
// range used for data-quality reliability; pass the zero Window when unknown.
func ProcessWithBaselineStats(points []Point, baseline BaselineStats, meta MetricMeta, stepSeconds int, window Window) SignalFeatures {
	var f SignalFeatures

	if len(points) == 0 {
		return f
	}

	// Copy before sorting to avoid mutating the caller's slice.
	pts := make([]Point, len(points))
	copy(pts, points)
	points = pts

	sort.Slice(points, func(i, j int) bool {
		return points[i].Timestamp.Before(points[j].Timestamp)
	})

	values := extractValues(points)
	f.Current = values[len(values)-1]

	// Basic statistics.
	f.Mean = mean(values)
	f.Stddev = stddev(values, f.Mean)
	// Floor the denominator for CV so that near-zero metrics do not blow up.
	if denom := math.Max(math.Abs(f.Mean), nearZeroEpsilon); denom > 0 {
		f.CV = f.Stddev / denom
	}

	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)

	f.Min = sorted[0]
	f.Max = sorted[len(sorted)-1]
	f.P50 = percentile(sorted, 0.50)
	f.P95 = percentile(sorted, 0.95)
	f.P99 = percentile(sorted, 0.99)

	if meta.Kind == KindLatency && f.P50 > 0 {
		f.TailRatio = f.P99 / f.P50
	}

	applyBaselineStats(&f, baseline)
	computeTrend(&f, points)
	computeStepChange(&f, points, meta.EffectiveThresholds())
	computeSpikes(&f, values, meta.EffectiveThresholds().SpikeZScore)
	computeSLOBreach(&f, points, meta, stepSeconds)
	computeSaturation(&f, values, meta)

	f.DataQuality = computeDataQuality(points, stepSeconds, window)
	f.Confidence = deriveConfidence(&f)
	f.Classification = Classify(&f, meta)

	return f
}

// applyBaselineStats copies precomputed baseline statistics into the features struct,
// computes delta vs current mean, and handles the near-zero denominator case: when
// the baseline is negligibly small (< nearZeroEpsilon), the current mean is used as
// the DeltaPct denominator to prevent division by an astronomically small value.
func applyBaselineStats(f *SignalFeatures, b BaselineStats) {
	f.BaselinePointCount = b.PointCount
	if b.PointCount == 0 {
		return
	}
	f.Baseline = b.Mean
	f.BaselineStddev = b.Stddev
	f.BaselineReliable = b.Reliable

	f.DeltaAbs = f.Mean - f.Baseline
	switch {
	case math.Abs(f.Baseline) > nearZeroEpsilon:
		f.DeltaPct = (f.DeltaAbs / math.Abs(f.Baseline)) * 100
	case f.Mean != 0:
		// Baseline is zero or negligibly small (< nearZeroEpsilon): dividing by it
		// would produce an astronomically large or infinite DeltaPct. Use current
		// mean as denominator instead — same logic as the zero-baseline case, since
		// no real metric has a meaningful baseline smaller than 1e-6.
		f.DeltaPct = (f.DeltaAbs / math.Abs(f.Mean)) * 100
	}
}

// computeTrend fits a linear regression across the window and normalizes the
// slope into a "total drift across the window as a fraction of baseline".
// This makes TrendScore and its thresholds window-length-independent: a
// TrendScore of 0.05 means the metric drifted by ~5% of baseline across the
// observed window, regardless of whether the window is 15 minutes or 24 hours.
func computeTrend(f *SignalFeatures, points []Point) {
	if len(points) < 3 {
		f.Trend = TrendFlat
		return
	}

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
		f.Trend = TrendFlat
		return
	}

	slope := (n*sumXY - sumX*sumY) / denom
	f.SlopePerMin = slope

	windowMinutes := points[len(points)-1].Timestamp.Sub(t0).Minutes()
	totalDrift := slope * windowMinutes

	// Normalize drift against baseline (preferred) or current mean as fallback.
	// Use an epsilon floor, not == 0: a baseline of 1e-9 is non-zero but would
	// otherwise be kept as the denominator, fail the nearZeroEpsilon guard
	// below, and silently leave TrendScore at 0 even when Mean is meaningful.
	denomNorm := math.Abs(f.Baseline)
	if denomNorm <= nearZeroEpsilon {
		denomNorm = math.Abs(f.Mean)
	}
	if denomNorm > nearZeroEpsilon {
		f.TrendScore = totalDrift / denomNorm
	}

	switch {
	case f.TrendScore > TrendFlatBand:
		f.Trend = TrendUp
	case f.TrendScore < -TrendFlatBand:
		f.Trend = TrendDown
	default:
		f.Trend = TrendFlat
	}
}

// computeStepChange detects sudden level shifts by comparing the first and
// last thirds of the window. The threshold for what counts as a "step" scales
// with the metric kind's significance threshold (2×), so for example an error
// rate step of 12% (SignificantDeltaPct=5 → step threshold 10) is detected,
// whereas a throughput step of 12% (SignificantDeltaPct=15 → step threshold 30)
// is treated as noise.
func computeStepChange(f *SignalFeatures, points []Point, thr ClassificationThresholds) {
	n := len(points)
	if n < 6 {
		return
	}

	third := n / 3
	firstThird := extractValues(points[:third])
	lastThird := extractValues(points[n-third:])

	meanFirst := mean(firstThird)
	meanLast := mean(lastThird)

	// Floor the denominator at nearZeroEpsilon rather than testing == 0.
	// A meanFirst of, say, 1e-9 is technically non-zero but dividing by it
	// produces an astronomically large changePct from ordinary noise. Prefer
	// meanFirst; fall back to meanLast; give up only when both are negligible.
	var denom float64
	switch {
	case math.Abs(meanFirst) > nearZeroEpsilon:
		denom = math.Abs(meanFirst)
	case math.Abs(meanLast) > nearZeroEpsilon:
		denom = math.Abs(meanLast)
	default:
		return
	}

	changePct := (meanLast - meanFirst) / denom * 100
	f.StepChangePct = changePct

	stepThreshold := 2 * thr.SignificantDeltaPct
	if math.Abs(changePct) > stepThreshold {
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

// computeSpikes counts outlier points by z-score. Requires a minimum sample
// size because with small N the achievable z is mathematically bounded by
// √(N-1); trying to detect spikes in a 5-point series is meaningless and
// silently produces false negatives.
func computeSpikes(f *SignalFeatures, values []float64, spikeZ float64) {
	if len(values) < minPointsForSpikeDetection {
		return
	}

	// Guard using Min/Max (already populated by the caller) rather than
	// relying solely on stddev == 0. When all values are nearly (but not
	// bitwise) identical, the accumulated squared residuals can produce a
	// near-zero but non-zero s, yielding a spurious MaxZScore. The if s == 0
	// check below handles the exact-equality case; this check handles the
	// near-zero case by skipping spike detection entirely when Min == Max.
	if f.Min == f.Max {
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
		if z >= spikeZ {
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
	var transitions int
	// prevBreachState tracks the previous point's breach status for
	// transition counting. -1 = uninitialized, 0 = not breached, 1 = breached.
	prevBreachState := -1

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

	// Second pass forward to count boundary crossings. Done separately so
	// the logic stays obvious: the backward pass deals with streak/duration
	// accounting, this pass only measures oscillation.
	for i := range points {
		state := 0
		if isBreach(points[i].Value, threshold, meta.BetterDirection) {
			state = 1
		}
		if prevBreachState != -1 && state != prevBreachState {
			transitions++
		}
		prevBreachState = state
	}

	f.SLOBreach = breachCount > 0
	f.BreachRatio = float64(breachCount) / float64(len(points))
	f.BreachDurationSeconds = int(breachDuration.Seconds())
	f.CurrentBreachStreakSeconds = int(currentStreak.Seconds())
	f.BreachTransitions = transitions
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
	tailSize := max(len(values)/10, 1)
	tail := values[len(values)-tailSize:]
	tailMean := mean(tail)

	f.SaturationDetected = tailMean >= saturationApproachFraction*satCap
}

func computeDataQuality(points []Point, stepSeconds int, window Window) DataQuality {
	if len(points) == 0 {
		return DataQuality{Reliable: false}
	}
	if stepSeconds <= 0 {
		stepSeconds = 60
	}

	step := time.Duration(stepSeconds) * time.Second
	first := points[0].Timestamp
	last := points[len(points)-1].Timestamp

	// Expected point count: prefer the requested window bounds so a metric that
	// stopped reporting mid-window is not judged "complete" just because the
	// points it did emit are contiguous. Fall back to the observed span when the
	// window is unknown (zero) or malformed (see Window doc).
	windowChecked := window.known()
	spanStart, spanEnd := first, last
	if windowChecked {
		spanStart, spanEnd = window.Start, window.End
	}
	expected := int(spanEnd.Sub(spanStart)/step) + 1

	var gapCount int
	var maxGap time.Duration
	consider := func(gap, threshold time.Duration) {
		if gap > threshold {
			gapCount++
			if gap > maxGap {
				maxGap = gap
			}
		}
	}
	// Leading/trailing gaps against the window edges catch a metric that
	// started late or died before the window ended. Skipped for an unusable
	// window. They use a more lenient threshold than inter-point gaps because
	// the window boundary rarely aligns with emission times, and the trailing
	// edge additionally absorbs Cloud Monitoring's ingestion lag (commonly
	// 1–4 minutes for a now-anchored window) — without the extra slack every
	// healthy recent metric would report a spurious trailing gap.
	if windowChecked {
		consider(first.Sub(window.Start), windowEdgeGapSteps*step)
		consider(window.End.Sub(last), windowEdgeGapSteps*step)
	}
	for i := 1; i < len(points); i++ {
		consider(points[i].Timestamp.Sub(points[i-1].Timestamp), 2*step)
	}

	actual := len(points)
	reliable := actual >= minCurrentWindowPoints && actual >= expected/2 && gapCount <= 3

	return DataQuality{
		ExpectedPoints: expected,
		ActualPoints:   actual,
		GapCount:       gapCount,
		MaxGapSeconds:  int(maxGap.Seconds()),
		Reliable:       reliable,
		WindowChecked:  windowChecked,
	}
}

// deriveConfidence summarizes how much the caller should trust the classification.
//   - high:   both window data and baseline are reliable.
//   - medium: exactly one of the two is unreliable.
//   - low:    neither is reliable, or no baseline was provided at all
//     (in which case delta-based signals are meaningless regardless of
//     how clean the current window is).
func deriveConfidence(f *SignalFeatures) Confidence {
	if f.BaselinePointCount == 0 {
		// No baseline at all — delta/trend/recovery signals are all zero
		// by construction. Caller should not treat classification as
		// anything better than a best-effort guess from the window alone.
		return ConfidenceLow
	}
	dq := f.DataQuality.Reliable
	base := f.BaselineReliable
	switch {
	case dq && base:
		return ConfidenceHigh
	case dq || base:
		return ConfidenceMedium
	default:
		return ConfidenceLow
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
// Kept as population stddev because small-sample bias is handled separately
// via minPointsForSpikeDetection; using sample stddev (/(N-1)) would require
// re-tuning every threshold in defaultThresholds.
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
	// Use the lerp form a+(b-a)*t instead of a*(1-t)+b*t: when a==b the
	// former returns a exactly, while the latter can yield a*(1-t)+a*t ≠ a
	// due to float64 rounding, breaking the monotone guarantee for equal
	// adjacent elements.
	return sorted[lower] + (sorted[upper]-sorted[lower])*frac
}
