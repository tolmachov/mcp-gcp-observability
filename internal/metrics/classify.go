package metrics

import "math"

type Classification string

const (
	ClassStable              Classification = "stable"
	ClassNoisy               Classification = "noisy"
	ClassSpike               Classification = "spike"
	ClassStepRegression      Classification = "step_regression"
	ClassSustainedRegression Classification = "sustained_regression"
	ClassRecovery            Classification = "recovery"
	ClassSaturation          Classification = "saturation"
	// ClassImprovement is a favorable delta continuing to improve (post-deploy signal).
	ClassImprovement Classification = "improvement"
	// ClassFlapping repeatedly crosses SLO threshold back and forth.
	ClassFlapping Classification = "flapping"
	// ClassInsufficientData means too sparse or unreliable baseline; cannot classify.
	ClassInsufficientData Classification = "insufficient_data"
	// ClassNotComputed is the zero value for Classification, returned when
	// ProcessWithBaselineStats is called with an empty points slice.
	// Distinct from ClassInsufficientData, which means "data exists but is
	// too sparse to trust" — ClassNotComputed means "no data at all, no
	// computation was attempted". Tool handlers should detect this via the
	// NoData path before calling Process, so this sentinel should never
	// appear in tool output.
	ClassNotComputed Classification = ""
)

// IsValid returns true if the Classification is one of the defined constants.
func (c Classification) IsValid() bool {
	switch c {
	case ClassStable, ClassNoisy, ClassSpike, ClassStepRegression,
		ClassSustainedRegression, ClassRecovery, ClassSaturation,
		ClassImprovement, ClassFlapping, ClassInsufficientData:
		return true
	}
	return false
}

// isDeltaBased reports whether a classification depends on baseline comparison.
// Excludes saturation (measured directly from capacity, not from baseline delta),
// stable (no delta analysis needed for lowest-severity), and flapping (based
// purely on SLO threshold crossings in the current window, independent of
// historical baseline).
func (c Classification) isDeltaBased() bool {
	switch c {
	case ClassSpike, ClassStepRegression, ClassSustainedRegression,
		ClassRecovery, ClassImprovement:
		return true
	}
	return false
}

// flappingTransitionRate is the minimum fraction of points that must be
// threshold crossings to be classified as flapping. For example, with a
// 60-point window this means at least 9 transitions (0.15 * 60). At 60-second
// step intervals, this represents roughly a crossing every 7 minutes, which is
// unambiguously oscillating behavior. Classification only runs when
// ActualPoints >= 10 (see classifyCore).
const flappingTransitionRate = 0.15

// flappingBreachRatioMin / flappingBreachRatioMax bracket the window of
// breach ratios that count as flapping. Outside this band the metric is
// either mostly healthy (recovery candidate) or mostly breached (sustained
// regression candidate).
const (
	flappingBreachRatioMin = 0.15
	flappingBreachRatioMax = 0.85
)

// Classify applies a deterministic decision tree to produce a classification.
//
// After the decision tree runs, a final data-quality pass downgrades
// regression-like classifications when the underlying data cannot support
// them:
//   - if the current window is unreliable (gaps, too few points) → noisy;
//   - if the baseline is unreliable and delta is the main evidence →
//     drop to stable, because |DeltaPct| computed against a flimsy baseline
//     cannot be trusted.
//
// This turns ambiguous "looks like a regression" signals into an honest
// "we don't have enough data to say" rather than false-positive alerts.
func Classify(f *SignalFeatures, meta MetricMeta) Classification {
	thr := meta.EffectiveThresholds()
	absDelta := math.Abs(f.DeltaPct)
	degrading := isDegrading(f.DeltaPct, meta.BetterDirection)

	class := classifyCore(f, meta, thr, absDelta, degrading)
	return applyReliabilityDowngrade(class, f)
}

// classifyCore is the pure decision tree that looks only at the signal
// features. The reliability-aware downgrade is applied separately.
func classifyCore(f *SignalFeatures, meta MetricMeta, thr ClassificationThresholds, absDelta float64, degrading bool) Classification {
	// 1. Saturation.
	if f.SaturationDetected {
		return ClassSaturation
	}

	// 2. Spike: short burst, not sustained.
	// SpikeRatio < 0.15: burst affects <15% of points.
	// absDelta < 20: overall deviation is moderate (spike has not shifted the mean much).
	if f.MaxZScore >= thr.SpikeZScore && f.SpikeRatio < 0.15 && absDelta < 20 {
		return ClassSpike
	}

	// 3. Flapping: metric oscillates across the SLO threshold. Orthogonal
	// to delta — a flapping metric can have a near-zero mean delta while
	// still crossing the threshold repeatedly. Gated on an actual SLO
	// being configured and on enough total points for the transition rate
	// to be meaningful.
	if meta.SLOThreshold != nil && f.DataQuality.ActualPoints >= 10 {
		transitionRate := float64(f.BreachTransitions) / float64(f.DataQuality.ActualPoints)
		if transitionRate >= flappingTransitionRate &&
			f.BreachRatio >= flappingBreachRatioMin &&
			f.BreachRatio <= flappingBreachRatioMax {
			return ClassFlapping
		}
	}

	// 4. No significant deviation.
	if absDelta < thr.SignificantDeltaPct {
		if f.CV > thr.CVForNoisy {
			return ClassNoisy
		}
		return ClassStable
	}

	// Beyond this point: absDelta >= significant threshold.

	// 5. Recovery: trend is moving back toward baseline after a significant deviation.
	// Requires both an active SLO (so BreachRatio is meaningful) AND a trend
	// strong enough to oppose the delta. Without an SLO, BreachRatio is
	// always 0 and this branch would fire on any ordinary direction reversal.
	if meta.SLOThreshold != nil && f.BreachRatio < 0.2 {
		isReturningToBaseline := (f.DeltaPct > 0 && f.TrendScore < -TrendFlatBand) ||
			(f.DeltaPct < 0 && f.TrendScore > TrendFlatBand)
		if isReturningToBaseline {
			return ClassRecovery
		}
	}

	// 6. Step regression: sudden level shift.
	// StepChangeDetected already enforces the kind-scaled step threshold
	// (2 * SignificantDeltaPct) inside the processor; here we only gate
	// on noise level and whether the SLO is being breached often enough.
	if f.StepChangeDetected && f.CV < 0.35 && f.BreachRatio > thr.BreachRatioForRegress {
		return ClassStepRegression
	}

	// 7. Sustained regression: slow steady degradation.
	if degrading && f.BreachRatio > thr.BreachRatioForRegress && math.Abs(f.TrendScore) > TrendStrongBand {
		return ClassSustainedRegression
	}

	// 8. Improvement: mirror of sustained_regression. Significant delta in
	// the favorable direction, trend continuing in that direction, and
	// (if an SLO is configured) we are not currently breaching it. For
	// DirectionNone metrics "improvement" is not defined.
	if !degrading && meta.BetterDirection != DirectionNone {
		favorableTrend := (meta.BetterDirection == DirectionDown && f.TrendScore < -TrendStrongBand) ||
			(meta.BetterDirection == DirectionUp && f.TrendScore > TrendStrongBand)
		if favorableTrend && f.BreachRatio < 0.2 {
			return ClassImprovement
		}
	}

	// 9. High noise with significant deviation.
	// Higher bar than CVForNoisy: catches cases where delta is significant
	// but signal is too unreliable to classify as regression.
	if f.CV > 0.4 {
		return ClassNoisy
	}

	// 10. Default for significant degradation without clear pattern.
	if degrading {
		return ClassStepRegression
	}

	return ClassStable
}

// applyReliabilityDowngrade drops delta-based classifications when the
// underlying data is too sparse or gap-ridden to support them. Saturation
// and stable are never downgraded: saturation is a direct capacity
// measurement, stable is already the lowest-severity label.
//
// The downgrade target is ClassInsufficientData rather than noisy/stable
// so the caller can distinguish "metric is genuinely flaky" (noisy) from
// "we don't have enough data to judge" (insufficient_data). Noisy is still
// returned for the core decision when CV is high — that is real noise, not
// a data gap.
func applyReliabilityDowngrade(class Classification, f *SignalFeatures) Classification {
	if class == ClassSaturation {
		return class
	}

	// The current window itself is unreliable (gap-ridden or too sparse).
	// We cannot distinguish a real regression from missing data, so any
	// delta-based verdict becomes insufficient_data. Flapping also requires
	// a reliable current window to count transitions meaningfully.
	if !f.DataQuality.Reliable && (class.isDeltaBased() || class == ClassFlapping) {
		return ClassInsufficientData
	}

	// The baseline is too thin to trust — we can see the current window
	// but cannot compare it against history confidently. Flapping does not
	// depend on the baseline (it only looks at SLO threshold crossings in
	// the current window), so it is not affected here.
	if !f.BaselineReliable && class.isDeltaBased() {
		return ClassInsufficientData
	}

	return class
}

// isDegrading returns true if the delta direction is unfavorable.
// For DirectionNone (e.g. throughput, auto-detected unknown metrics),
// no direction is considered degrading — the classifier can still detect
// spikes, noise, and saturation, but not sustained/step regressions.
// This is intentional: without knowing which direction is bad, calling
// a change a "regression" would be misleading.
func isDegrading(deltaPct float64, dir BetterDirection) bool {
	switch dir {
	case DirectionDown:
		return deltaPct > 0 // value went up, which is bad
	case DirectionUp:
		return deltaPct < 0 // value went down, which is bad
	default:
		return false
	}
}
