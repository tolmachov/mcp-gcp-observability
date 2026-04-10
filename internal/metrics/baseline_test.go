package metrics

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestComputeBaselineStats_Reliability(t *testing.T) {
	tests := []struct {
		name         string
		pointCount   int
		expected     int
		wantReliable bool
	}{
		{name: "exactly the floor is reliable when expected is unknown", pointCount: 7, expected: 0, wantReliable: true},
		{name: "below the floor is never reliable", pointCount: 6, expected: 0, wantReliable: false},
		{name: "7 points is not enough for a 60-point expected window", pointCount: 7, expected: 60, wantReliable: false},
		{name: "30 points clears half of a 60-point expected window", pointCount: 30, expected: 60, wantReliable: true},
		{name: "29 points does not clear half of a 60-point expected window", pointCount: 29, expected: 60, wantReliable: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pts := make([]Point, tc.pointCount)
			stats := ComputeBaselineStats(pts, tc.expected)
			assert.Equal(t, tc.wantReliable, stats.Reliable, "actual=%d expected=%d", tc.pointCount, tc.expected)
		})
	}
}

func TestComputeBaselineStats_Empty(t *testing.T) {
	stats := ComputeBaselineStats(nil, 60)
	assert.Zero(t, stats.PointCount, "empty baseline should have zero point count")
	assert.False(t, stats.Reliable, "empty baseline should not be reliable")
}

// TestComputeRobustBaselineStats_IgnoresOutlierWeek verifies that a single
// poisoned historical week does not corrupt the robust baseline mean.
// This is the core reason robust aggregation replaces naive concatenation.
func TestComputeRobustBaselineStats_IgnoresOutlierWeek(t *testing.T) {
	normalWeek := make([]Point, 20)
	for i := range normalWeek {
		normalWeek[i].Value = 10
	}
	incidentWeek := make([]Point, 20)
	for i := range incidentWeek {
		incidentWeek[i].Value = 100
	}

	buckets := [][]Point{normalWeek, normalWeek, normalWeek, incidentWeek}
	stats := ComputeRobustBaselineStats(buckets, 20)

	// Median of {10, 10, 10, 100} = 10, so the robust mean ignores the incident.
	// A naive concatenation would give mean ≈ (10*60 + 100*20)/80 = 32.5.
	assert.InDelta(t, 10, stats.Mean, 0.01, "expected the outlier week to be ignored")
}

func TestComputeRobustBaselineStats_SingleBucketFallback(t *testing.T) {
	week := make([]Point, 20)
	for i := range week {
		week[i].Value = 5
	}
	// Only one non-empty bucket → fall back to plain aggregation.
	stats := ComputeRobustBaselineStats([][]Point{week, nil, nil, nil}, 20)
	assert.InDelta(t, 5, stats.Mean, 0.01)
	assert.Equal(t, 20, stats.PointCount)
}

func TestComputeRobustBaselineStats_IdenticalBuckets(t *testing.T) {
	// When all bucket means are identical, MAD is 0 and the fallback
	// should use the mean of per-bucket stddevs so within-bucket variance
	// is preserved.
	makeBucket := func(base float64) []Point {
		pts := make([]Point, 10)
		for i := range pts {
			// Jitter around the base value so each bucket has stddev > 0.
			pts[i].Value = base + float64(i%2)*0.2 - 0.1
		}
		return pts
	}
	buckets := [][]Point{makeBucket(5), makeBucket(5), makeBucket(5)}
	stats := ComputeRobustBaselineStats(buckets, 10)
	assert.Greater(t, stats.Stddev, 0.0, "per-bucket stddev fallback should kick in")
}
