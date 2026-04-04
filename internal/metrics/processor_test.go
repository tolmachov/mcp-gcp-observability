package metrics

import (
	"math"
	"testing"
	"time"
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
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60)

	if f.Classification != ClassStable {
		t.Errorf("classification = %q, want %q", f.Classification, ClassStable)
	}
	if math.Abs(f.DeltaPct) > 0.01 {
		t.Errorf("delta_pct = %f, want ~0", f.DeltaPct)
	}
	if f.Mean != 0.5 {
		t.Errorf("mean = %f, want 0.5", f.Mean)
	}
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
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60)

	if f.Classification != ClassStepRegression {
		t.Errorf("classification = %q, want %q", f.Classification, ClassStepRegression)
	}
	if !f.StepChangeDetected {
		t.Error("expected step change detected")
	}
	if !f.SLOBreach {
		t.Error("expected SLO breach")
	}
	if f.DeltaPct < 30 {
		t.Errorf("delta_pct = %f, expected > 30", f.DeltaPct)
	}
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
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60)

	if f.Classification != ClassSaturation {
		t.Errorf("classification = %q, want %q", f.Classification, ClassSaturation)
	}
	if !f.SaturationDetected {
		t.Error("expected saturation detected")
	}
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
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60)

	if f.Classification != ClassSpike {
		t.Errorf("classification = %q, want %q", f.Classification, ClassSpike)
	}
	if f.SpikeCount == 0 {
		t.Error("expected spikes detected")
	}
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
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60)

	if f.Classification != ClassNoisy {
		t.Errorf("classification = %q, want %q", f.Classification, ClassNoisy)
	}
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
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60)

	if !f.SLOBreach {
		t.Error("expected SLO breach for availability below threshold")
	}
	if f.BreachRatio != 1.0 {
		t.Errorf("breach_ratio = %f, want 1.0", f.BreachRatio)
	}
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
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60)

	if f.DeltaPct == 0 {
		t.Error("DeltaPct should not be 0 when baseline is 0 and current is non-zero")
	}
	if f.Classification == ClassStable {
		t.Errorf("classification should not be 'stable' for a spike from zero, got %q", f.Classification)
	}
}

func TestProcessFewPoints(t *testing.T) {
	// With 5 points, step change detection should not fire.
	values := []float64{1.0, 1.0, 5.0, 5.0, 5.0}
	baseline := []float64{1.0, 1.0, 1.0, 1.0, 1.0}
	meta := MetricMeta{Kind: KindLatency, Unit: "seconds", BetterDirection: DirectionDown}
	f := Process(makePoints(values, 60), makePoints(baseline, 60), meta, 60)

	if f.StepChangeDetected {
		t.Error("step change should not be detected with fewer than 6 points")
	}
}

func TestIsBreachDirectionNone(t *testing.T) {
	// DirectionNone defaults to "value > threshold" (same as DirectionDown).
	// This is intentional — if someone sets an SLO on a directionless metric,
	// breach means exceeding the threshold.
	if !isBreach(1.5, 1.0, DirectionNone) {
		t.Error("expected breach when value > threshold with DirectionNone")
	}
	if isBreach(0.5, 1.0, DirectionNone) {
		t.Error("expected no breach when value < threshold with DirectionNone")
	}
}

func TestProcessEmptyPoints(t *testing.T) {
	meta := MetricMeta{Kind: KindLatency}
	f := Process(nil, nil, meta, 60)
	if f.Classification != "" {
		t.Errorf("expected empty classification for no points, got %q", f.Classification)
	}
}

func TestPercentile(t *testing.T) {
	sorted := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	p50 := percentile(sorted, 0.50)
	if math.Abs(p50-5.5) > 0.01 {
		t.Errorf("p50 = %f, want 5.5", p50)
	}
	p95 := percentile(sorted, 0.95)
	if math.Abs(p95-9.55) > 0.1 {
		t.Errorf("p95 = %f, want ~9.55", p95)
	}
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
	if dq.GapCount != 1 {
		t.Errorf("gap_count = %d, want 1", dq.GapCount)
	}
	if dq.MaxGapSeconds != 300 {
		t.Errorf("max_gap_seconds = %d, want 300", dq.MaxGapSeconds)
	}
}
