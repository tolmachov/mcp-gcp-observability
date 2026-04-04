package metrics

import "math"

// Classification represents a metric behavior classification.
type Classification string

// Classification constants.
const (
	ClassStable              Classification = "stable"
	ClassNoisy               Classification = "noisy"
	ClassSpike               Classification = "spike"
	ClassStepRegression      Classification = "step_regression"
	ClassSustainedRegression Classification = "sustained_regression"
	ClassRecovery            Classification = "recovery"
	ClassSaturation          Classification = "saturation"
)

// IsValid returns true if the Classification is one of the defined constants.
func (c Classification) IsValid() bool {
	switch c {
	case ClassStable, ClassNoisy, ClassSpike, ClassStepRegression,
		ClassSustainedRegression, ClassRecovery, ClassSaturation:
		return true
	}
	return false
}

// Classify applies a deterministic decision tree to produce a classification.
func Classify(f *SignalFeatures, meta MetricMeta) Classification {
	thr := meta.EffectiveThresholds()
	absDelta := math.Abs(f.DeltaPct)
	degrading := isDegrading(f.DeltaPct, meta.BetterDirection)

	// 1. Saturation.
	if f.SaturationDetected {
		return ClassSaturation
	}

	// 2. Spike: short burst, not sustained.
	// SpikeRatio < 0.15: burst affects <15% of points. absDelta < 20: overall deviation is moderate.
	if f.MaxZScore >= thr.SpikeZScore && f.SpikeRatio < 0.15 && absDelta < 20 {
		return ClassSpike
	}

	// 3. No significant deviation.
	if absDelta < thr.SignificantDeltaPct {
		if f.CV > thr.CVForNoisy {
			return ClassNoisy
		}
		return ClassStable
	}

	// Beyond this point: absDelta >= significant threshold.

	// 4. Recovery: trend is moving back toward baseline after a significant deviation.
	// The trend must oppose the delta direction (i.e. trending toward baseline, not away).
	// Recovery applies even when the metric is still in a degraded state — what matters
	// is that the trend direction indicates it is returning toward baseline.
	isReturningToBaseline := (f.DeltaPct > 0 && f.TrendScore < -0.02) ||
		(f.DeltaPct < 0 && f.TrendScore > 0.02)
	if f.BreachRatio < 0.2 && isReturningToBaseline {
		return ClassRecovery
	}

	// 5. Step regression: sudden level shift.
	// CV < 0.35: signal is stable enough to detect a real shift (relaxed from 0.25 to reduce false positives on noisy-but-real steps).
	if math.Abs(f.StepChangePct) > 20 && f.CV < 0.35 && f.BreachRatio > thr.BreachRatioForRegress {
		return ClassStepRegression
	}

	// 6. Sustained regression: slow steady degradation.
	if degrading && f.BreachRatio > thr.BreachRatioForRegress && math.Abs(f.TrendScore) > 0.02 {
		return ClassSustainedRegression
	}

	// 7. High noise with significant deviation.
	// Higher bar than CVForNoisy (above): catches cases where delta is significant but signal is too unreliable to classify as regression.
	if f.CV > 0.4 {
		return ClassNoisy
	}

	// 8. Default for significant degradation without clear pattern.
	if degrading {
		return ClassStepRegression
	}

	return ClassStable
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
