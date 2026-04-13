package metrics

import (
	"math"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"pgregory.net/rapid"
)

// --- Shared generators ---

// finiteFloat generates finite float64 values in a range suitable for metric values.
func finiteFloat() *rapid.Generator[float64] {
	return rapid.Float64Range(-1e6, 1e6)
}

// direction generates a random BetterDirection.
func direction() *rapid.Generator[BetterDirection] {
	return rapid.Custom(func(t *rapid.T) BetterDirection {
		return []BetterDirection{DirectionDown, DirectionUp, DirectionNone}[rapid.IntRange(0, 2).Draw(t, "dir_idx")]
	})
}

// regularPoints returns n Points with equal step spacing and constant value.
func uniformPoints(n, stepSeconds int, value float64) []Point {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pts := make([]Point, n)
	for i := range pts {
		pts[i] = Point{
			Timestamp: t0.Add(time.Duration(i*stepSeconds) * time.Second),
			Value:     value,
		}
	}
	return pts
}

// pointsFromValues returns Points with equal step spacing from given values.
func pointsFromValues(values []float64, stepSeconds int) []Point {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pts := make([]Point, len(values))
	for i, v := range values {
		pts[i] = Point{
			Timestamp: t0.Add(time.Duration(i*stepSeconds) * time.Second),
			Value:     v,
		}
	}
	return pts
}

// === isBreach / isDegrading ===

// TestProperty_IsDegradingNoneAlwaysFalse verifies that DirectionNone never
// considers any delta as degrading — "no defined direction" means no regression.
func TestProperty_IsDegradingNoneAlwaysFalse(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		delta := rapid.Float64().Draw(t, "delta")
		assert.False(t, isDegrading(delta, DirectionNone))
	})
}

// TestProperty_IsBreachNoneSameAsDown verifies the documented fallback:
// DirectionNone uses the same breach logic as DirectionDown.
func TestProperty_IsBreachNoneSameAsDown(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		v := rapid.Float64().Draw(t, "v")
		thr := rapid.Float64().Draw(t, "thr")
		assert.Equal(t,
			isBreach(v, thr, DirectionDown),
			isBreach(v, thr, DirectionNone),
			"DirectionNone must behave identically to DirectionDown",
		)
	})
}

// TestProperty_IsBreachAntiSymmetric verifies that Down and Up are strict
// opposites: when value != threshold, exactly one of them is breached.
func TestProperty_IsBreachAntiSymmetric(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		v := finiteFloat().Draw(t, "v")
		thr := finiteFloat().Draw(t, "thr")
		if v == thr {
			return // at equality both directions return false — not antisymmetric
		}
		assert.NotEqual(t,
			isBreach(v, thr, DirectionDown),
			isBreach(v, thr, DirectionUp),
			"isBreach(Down) and isBreach(Up) should be opposites when v(%v) != thr(%v)", v, thr,
		)
	})
}

// === ComputeBaselineStats ===

// TestProperty_ComputeBaselineStats_ConstantSignal verifies that a flat
// baseline (all values equal) yields near-zero stddev and the correct mean.
// Stddev is compared with a relative epsilon (1e-10 of |v|) because float64
// arithmetic can introduce rounding when computing n*v/n for very small
// exponents (mean ≠ v by ~1 ULP), producing a tiny but non-zero stddev.
func TestProperty_ComputeBaselineStats_ConstantSignal(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 200).Draw(t, "n")
		v := finiteFloat().Draw(t, "v")
		pts := uniformPoints(n, 60, v)
		bs := ComputeBaselineStats(pts, n)

		// Use relative tolerance: float64 sum(n·v)/n can differ from v by up to
		// n·ε·|v| where ε ≈ 2.2e-16 (machine epsilon). For n ≤ 200 that is
		// at most ~4.4e-14·|v|, so 1e-12·|v| is a safe relative bound.
		tol := math.Max(math.Abs(v), 1e-300) * 1e-12
		assert.InDelta(t, v, bs.Mean, tol, "mean of constant signal must equal the constant value")
		assert.InDelta(t, 0.0, bs.Stddev, tol,
			"stddev of constant signal must be negligible (within float64 rounding)")
	})
}

// TestProperty_ComputeBaselineStats_ReliabilityFloor verifies the hard floor:
// fewer than minBaselinePoints (7) points are always unreliable regardless of
// the expected count.
func TestProperty_ComputeBaselineStats_ReliabilityFloor(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, minBaselinePoints-1).Draw(t, "n")
		v := finiteFloat().Draw(t, "v")
		pts := uniformPoints(n, 60, v)
		bs := ComputeBaselineStats(pts, 1000) // large expected so only floor matters

		assert.False(t, bs.Reliable, "fewer than %d points must always be unreliable", minBaselinePoints)
	})
}

// TestProperty_ComputeBaselineStats_PointCountPreserved verifies that no
// points are silently dropped during aggregation.
func TestProperty_ComputeBaselineStats_PointCountPreserved(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 200).Draw(t, "n")
		v := finiteFloat().Draw(t, "v")
		pts := uniformPoints(n, 60, v)
		bs := ComputeBaselineStats(pts, n)

		assert.Equal(t, n, bs.PointCount)
	})
}

// TestProperty_ComputeBaselineStats_ReliabilityMonotone verifies that
// adding more points never makes a reliable baseline unreliable.
func TestProperty_ComputeBaselineStats_ReliabilityMonotone(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(minBaselinePoints, 200).Draw(t, "n")
		expected := rapid.IntRange(1, n).Draw(t, "expected")
		v := finiteFloat().Draw(t, "v")

		bsN := ComputeBaselineStats(uniformPoints(n, 60, v), expected)
		bsN1 := ComputeBaselineStats(uniformPoints(n+1, 60, v), expected)

		if bsN.Reliable {
			assert.True(t, bsN1.Reliable, "adding a point must not make a reliable baseline unreliable")
		}
	})
}

// === ComputeRobustBaselineStats ===

// TestProperty_ComputeRobustBaselineStats_SingleBucket verifies the fallback:
// a single non-empty bucket produces the same mean as plain ComputeBaselineStats.
func TestProperty_ComputeRobustBaselineStats_SingleBucket(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 50).Draw(t, "n")
		v := finiteFloat().Draw(t, "v")
		pts := uniformPoints(n, 60, v)

		robust := ComputeRobustBaselineStats([][]Point{pts}, n)
		plain := ComputeBaselineStats(pts, n)

		assert.InDelta(t, plain.Mean, robust.Mean, 1e-9)
		assert.Equal(t, plain.PointCount, robust.PointCount)
	})
}

// TestProperty_ComputeRobustBaselineStats_IdenticalBuckets verifies that
// when all weekly buckets are identical the robust mean equals the bucket mean.
func TestProperty_ComputeRobustBaselineStats_IdenticalBuckets(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		k := rapid.IntRange(2, 8).Draw(t, "buckets")
		n := rapid.IntRange(1, 30).Draw(t, "n")
		v := finiteFloat().Draw(t, "v")
		pts := uniformPoints(n, 60, v)

		buckets := make([][]Point, k)
		for i := range buckets {
			buckets[i] = pts
		}
		bs := ComputeRobustBaselineStats(buckets, n)

		assert.InDelta(t, v, bs.Mean, 1e-9, "mean of identical buckets must equal the bucket value")
		assert.GreaterOrEqual(t, bs.PointCount, 0)
	})
}

// === computeDataQuality ===

// TestProperty_ComputeDataQuality_RegularGrid verifies that a perfectly
// regular time series (equal spacing == stepSeconds) has no detected gaps.
func TestProperty_ComputeDataQuality_RegularGrid(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(2, 200).Draw(t, "n")
		stepSec := rapid.IntRange(1, 3600).Draw(t, "step")
		pts := uniformPoints(n, stepSec, 1.0)

		dq := computeDataQuality(pts, stepSec)

		assert.Equal(t, 0, dq.GapCount, "regular grid must have no gaps")
		assert.Equal(t, 0, dq.MaxGapSeconds, "regular grid must have zero max gap")
	})
}

// TestProperty_ComputeDataQuality_NonNegative verifies that gap metrics are
// always non-negative regardless of the point timestamps.
func TestProperty_ComputeDataQuality_NonNegative(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(0, 100).Draw(t, "n")
		stepSec := rapid.IntRange(1, 3600).Draw(t, "step")
		t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		pts := make([]Point, n)
		var cursor time.Time = t0
		for i := range pts {
			gap := time.Duration(rapid.IntRange(1, 10000).Draw(t, "gap")) * time.Second
			cursor = cursor.Add(gap)
			pts[i] = Point{Timestamp: cursor, Value: 1.0}
		}

		dq := computeDataQuality(pts, stepSec)

		assert.GreaterOrEqual(t, dq.GapCount, 0)
		assert.GreaterOrEqual(t, dq.MaxGapSeconds, 0)
	})
}

// TestProperty_ComputeDataQuality_EmptyOrSingle verifies degenerate inputs.
func TestProperty_ComputeDataQuality_EmptyOrSingle(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		dq := computeDataQuality(nil, 60)
		assert.Equal(t, 0, dq.GapCount)
		assert.Equal(t, 0, dq.MaxGapSeconds)
	})
	rapid.Check(t, func(t *rapid.T) {
		stepSec := rapid.IntRange(1, 3600).Draw(t, "step")
		pts := uniformPoints(1, stepSec, 1.0)
		dq := computeDataQuality(pts, stepSec)
		assert.Equal(t, 0, dq.GapCount, "single point must have no gaps")
		assert.Equal(t, 0, dq.MaxGapSeconds)
	})
}

// === computeSpikes ===

// TestProperty_ComputeSpikes_EqualValues verifies that a flat signal (all
// values equal) produces zero spikes and zero MaxZScore.
func TestProperty_ComputeSpikes_EqualValues(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(minPointsForSpikeDetection, 100).Draw(t, "n")
		v := finiteFloat().Draw(t, "v")
		values := make([]float64, n)
		for i := range values {
			values[i] = v
		}
		// Pre-populate Min/Max as ProcessWithBaselineStats does before calling computeSpikes.
		f := SignalFeatures{Min: v, Max: v}
		computeSpikes(&f, values, 3.0)

		assert.Equal(t, 0, f.SpikeCount, "constant signal must have no spikes")
		assert.Equal(t, 0.0, f.MaxZScore, "constant signal must have zero MaxZScore")
	})
}

// TestProperty_ComputeSpikes_RatioFormula verifies that SpikeRatio is always
// exactly SpikeCount / len(values).
func TestProperty_ComputeSpikes_RatioFormula(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(minPointsForSpikeDetection, 100).Draw(t, "n")
		values := rapid.SliceOfN(finiteFloat(), n, n).Draw(t, "values")
		var f SignalFeatures
		computeSpikes(&f, values, 3.0)

		expected := float64(f.SpikeCount) / float64(n)
		assert.InDelta(t, expected, f.SpikeRatio, 1e-12, "SpikeRatio must equal SpikeCount/N")
	})
}

// TestProperty_ComputeSpikes_CountInRange verifies SpikeCount ∈ [0, N] and
// MaxZScore ≥ 0.
func TestProperty_ComputeSpikes_CountInRange(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(minPointsForSpikeDetection, 100).Draw(t, "n")
		values := rapid.SliceOfN(finiteFloat(), n, n).Draw(t, "values")
		var f SignalFeatures
		computeSpikes(&f, values, 3.0)

		assert.GreaterOrEqual(t, f.SpikeCount, 0)
		assert.LessOrEqual(t, f.SpikeCount, n)
		assert.GreaterOrEqual(t, f.MaxZScore, 0.0)
	})
}

// === percentile ===

// TestProperty_Percentile_Bounds verifies that p=0 returns the minimum and
// p=1 returns the maximum.
func TestProperty_Percentile_Bounds(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 100).Draw(t, "n")
		vals := rapid.SliceOfN(finiteFloat(), n, n).Draw(t, "vals")
		sort.Float64s(vals)

		assert.Equal(t, vals[0], percentile(vals, 0.0), "p=0 must return minimum")
		assert.Equal(t, vals[len(vals)-1], percentile(vals, 1.0), "p=1 must return maximum")
	})
}

// TestProperty_Percentile_Monotone verifies that higher quantile → higher
// or equal value (monotone non-decreasing).
func TestProperty_Percentile_Monotone(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(2, 100).Draw(t, "n")
		vals := rapid.SliceOfN(finiteFloat(), n, n).Draw(t, "vals")
		sort.Float64s(vals)

		p1 := rapid.Float64Range(0, 1).Draw(t, "p1")
		p2 := rapid.Float64Range(0, 1).Draw(t, "p2")
		if p1 > p2 {
			p1, p2 = p2, p1
		}

		assert.LessOrEqual(t, percentile(vals, p1), percentile(vals, p2),
			"percentile must be monotone non-decreasing")
	})
}

// === median ===

// TestProperty_Median_ConstantSlice verifies that the median of a constant
// slice equals the constant.
func TestProperty_Median_ConstantSlice(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 100).Draw(t, "n")
		v := finiteFloat().Draw(t, "v")
		vals := make([]float64, n)
		for i := range vals {
			vals[i] = v
		}
		assert.InDelta(t, v, median(vals), 1e-9)
	})
}

// TestProperty_Median_OddLengthMiddleElement verifies that for odd-length
// sorted slices the median is the exact middle element.
func TestProperty_Median_OddLengthMiddleElement(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate odd length: 2k+1
		k := rapid.IntRange(0, 49).Draw(t, "k")
		n := 2*k + 1
		vals := rapid.SliceOfN(finiteFloat(), n, n).Draw(t, "vals")
		sorted := make([]float64, n)
		copy(sorted, vals)
		sort.Float64s(sorted)

		assert.Equal(t, sorted[k], median(sorted), "median of odd-length sorted slice must be middle element")
	})
}

// === Process / ProcessWithBaselineStats (pipeline invariants) ===

// TestProperty_Process_SpikeAndBreachRatiosInRange verifies that both
// SpikeRatio and BreachRatio are in [0, 1] for any input.
func TestProperty_Process_SpikeAndBreachRatiosInRange(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 80).Draw(t, "n")
		values := rapid.SliceOfN(finiteFloat(), n, n).Draw(t, "values")
		pts := pointsFromValues(values, 60)

		slo := rapid.Float64Range(0, 2).Draw(t, "slo")
		meta := MetricMeta{
			Kind:            KindLatency,
			BetterDirection: DirectionDown,
			SLOThreshold:    &slo,
		}
		f := Process(pts, nil, meta, 60, 0)

		assert.GreaterOrEqual(t, f.SpikeRatio, 0.0, "SpikeRatio must be >= 0")
		assert.LessOrEqual(t, f.SpikeRatio, 1.0, "SpikeRatio must be <= 1")
		assert.GreaterOrEqual(t, f.BreachRatio, 0.0, "BreachRatio must be >= 0")
		assert.LessOrEqual(t, f.BreachRatio, 1.0, "BreachRatio must be <= 1")
	})
}

// TestProperty_Process_DeltaPctFinite verifies that DeltaPct is always a
// finite number (no NaN or Inf) when inputs are finite.
func TestProperty_Process_DeltaPctFinite(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 80).Draw(t, "n")
		values := rapid.SliceOfN(finiteFloat(), n, n).Draw(t, "values")
		bvalues := rapid.SliceOfN(finiteFloat(), n, n).Draw(t, "bvalues")

		f := Process(
			pointsFromValues(values, 60),
			pointsFromValues(bvalues, 60),
			MetricMeta{Kind: KindLatency, BetterDirection: DirectionDown},
			60, n,
		)

		assert.False(t, math.IsNaN(f.DeltaPct), "DeltaPct must not be NaN")
		assert.False(t, math.IsInf(f.DeltaPct, 0), "DeltaPct must not be Inf")
	})
}

// TestProperty_Process_NoBaselineNeverHighConfidence verifies that when no
// baseline is provided (nil), confidence is never "high" — delta signals
// computed against an absent baseline are meaningless.
func TestProperty_Process_NoBaselineNeverHighConfidence(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 80).Draw(t, "n")
		values := rapid.SliceOfN(finiteFloat(), n, n).Draw(t, "values")
		f := Process(
			pointsFromValues(values, 60),
			nil,
			MetricMeta{Kind: KindLatency, BetterDirection: DirectionDown},
			60, 0,
		)
		assert.NotEqual(t, ConfidenceHigh, f.Confidence,
			"no baseline must never produce high confidence")
	})
}

// TestProperty_Process_SpikeCountNonNegative verifies that SpikeCount is
// always >= 0.
func TestProperty_Process_SpikeCountNonNegative(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 80).Draw(t, "n")
		values := rapid.SliceOfN(finiteFloat(), n, n).Draw(t, "values")
		f := Process(
			pointsFromValues(values, 60),
			nil,
			MetricMeta{Kind: KindLatency, BetterDirection: DirectionDown},
			60, 0,
		)
		assert.GreaterOrEqual(t, f.SpikeCount, 0)
	})
}

// === Classify ===

// TestProperty_Classify_SaturationTakesPriority verifies that saturation is
// the highest-priority classification: if SaturationDetected is true, no
// other signal can override it.
func TestProperty_Classify_SaturationTakesPriority(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		f := &SignalFeatures{
			SaturationDetected: true,
			// Populate fields that other branches inspect so they are
			// non-trivially set; the saturation branch must still win.
			DataQuality: DataQuality{
				Reliable:       true,
				ActualPoints:   60,
				ExpectedPoints: 60,
			},
			BaselineReliable:   true,
			BaselinePointCount: 60,
			DeltaPct:           finiteFloat().Draw(t, "delta"),
			CV:                 rapid.Float64Range(0, 5).Draw(t, "cv"),
			MaxZScore:          rapid.Float64Range(0, 10).Draw(t, "z"),
			SpikeRatio:         rapid.Float64Range(0, 1).Draw(t, "spike_ratio"),
			BreachRatio:        rapid.Float64Range(0, 1).Draw(t, "breach_ratio"),
			TrendScore:         rapid.Float64Range(-0.5, 0.5).Draw(t, "trend"),
		}
		meta := MetricMeta{
			Kind:            KindResourceUtilization,
			BetterDirection: direction().Draw(t, "dir"),
		}
		assert.Equal(t, ClassSaturation, Classify(f, meta),
			"SaturationDetected must always yield ClassSaturation regardless of other fields")
	})
}
