package metrics

import (
	"math"
	"sort"
)

// BaselineStats is a precomputed summary of baseline data suitable for
// passing into ProcessWithBaselineStats. It decouples baseline aggregation
// strategy (concatenation vs. robust per-week aggregation) from the processor.
type BaselineStats struct {
	Mean       float64
	Stddev     float64
	PointCount int
	// Reliable: true when PointCount >= max(minBaselinePoints, expected/2).
	Reliable bool
}

// ComputeBaselineStats builds BaselineStats using plain mean/stddev.
// expectedPoints is used for reliability checks; pass 0 if unknown.
func ComputeBaselineStats(points []Point, expectedPoints int) BaselineStats {
	if len(points) == 0 {
		return BaselineStats{}
	}
	values := extractValues(points)
	m := mean(values)
	s := stddev(values, m)
	return BaselineStats{
		Mean:       m,
		Stddev:     s,
		PointCount: len(values),
		Reliable:   isBaselineReliable(len(values), expectedPoints),
	}
}

// ComputeRobustBaselineStats combines per-bucket baselines using robust estimation
// (median + MAD) to handle outlier buckets. Needs ≥2 buckets; falls back to plain
// aggregation for 0-1 buckets. expectedPointsPerBucket is for reliability checks.
func ComputeRobustBaselineStats(buckets [][]Point, expectedPointsPerBucket int) BaselineStats {
	// Filter out empty buckets and flatten as a fallback.
	var merged []Point
	var nonEmpty [][]Point
	for _, b := range buckets {
		if len(b) == 0 {
			continue
		}
		nonEmpty = append(nonEmpty, b)
		merged = append(merged, b...)
	}
	if len(nonEmpty) < 2 {
		// Not enough buckets for a robust estimate — fall back to plain aggregation.
		return ComputeBaselineStats(merged, expectedPointsPerBucket*len(buckets))
	}

	perBucketMeans := make([]float64, len(nonEmpty))
	perBucketStddevs := make([]float64, len(nonEmpty))
	for i, b := range nonEmpty {
		vals := extractValues(b)
		m := mean(vals)
		perBucketMeans[i] = m
		perBucketStddevs[i] = stddev(vals, m)
	}

	robustMean := median(perBucketMeans)

	// MAD-based stddev: 1.4826 normalizes MAD for normal distributions.
	devs := make([]float64, len(perBucketMeans))
	for i, m := range perBucketMeans {
		devs[i] = math.Abs(m - robustMean)
	}
	mad := median(devs)
	var robustStddev float64
	if mad > 0 {
		robustStddev = 1.4826 * mad
	} else {
		// All bucket means identical: use mean of stddevs for within-bucket variance.
		robustStddev = mean(perBucketStddevs)
	}

	totalExpected := expectedPointsPerBucket * len(buckets)
	return BaselineStats{
		Mean:       robustMean,
		Stddev:     robustStddev,
		PointCount: len(merged),
		Reliable:   isBaselineReliable(len(merged), totalExpected),
	}
}

// isBaselineReliable: actual >= max(minBaselinePoints, expected/2).
func isBaselineReliable(actual, expected int) bool {
	if actual < minBaselinePoints {
		return false
	}
	if expected > 0 && actual < expected/2 {
		return false
	}
	return true
}

// median returns the median; copies input before sorting to avoid mutation.
func median(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}
