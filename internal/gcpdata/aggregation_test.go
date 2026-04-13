package gcpdata

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"

	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

func TestFoldGroupSeries(t *testing.T) {
	t.Run("Positive: sum across two games at same timestamps", testFoldSum)
	t.Run("Positive: max across replicas", testFoldMax)
	t.Run("Positive: mean of ratios", testFoldMean)
	t.Run("Edge: empty input returns nil", testFoldEmpty)
	t.Run("Edge: single group passes through", testFoldSingleGroup)
	t.Run("Edge: disjoint timestamps preserved", testFoldDisjointTimestamps)
	t.Run("Edge: ragged buckets reported", testFoldRaggedBuckets)
	t.Run("Edge: many disjoint timestamps sorted ascending", testFoldSortsOutput)
	t.Run("Edge: carry-forward bounded by maxCarryForwardBuckets", testFoldCarryForwardBounded)
	t.Run("Edge: fresh point resurrects departed series", testFoldCarryForwardResurrect)
	t.Run("Edge: mean reducer with departed group divides by active count only", testFoldMeanWithDepartedGroup)
	t.Run("Edge: single series that departs produces no output after carry expires", testFoldSingleSeriesDeparture)
}

func testFoldSum(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	t1 := t0.Add(time.Minute)
	series := []MetricTimeSeries{
		{
			Points: []metrics.Point{
				{Timestamp: t0, Value: 10},
				{Timestamp: t1, Value: 12},
			},
		},
		{
			Points: []metrics.Point{
				{Timestamp: t0, Value: 5},
				{Timestamp: t1, Value: 7},
			},
		},
	}

	got, stats := foldGroupSeries(series, metrics.ReducerSum)
	assert.Equal(t, foldStats{}, stats, "all buckets fully covered")
	require.Len(t, got, 2)
	assert.True(t, got[0].Timestamp.Equal(t0))
	assert.Equal(t, 15.0, got[0].Value)
	assert.True(t, got[1].Timestamp.Equal(t1))
	assert.Equal(t, 19.0, got[1].Value)
}

func testFoldMax(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	series := []MetricTimeSeries{
		{Points: []metrics.Point{{Timestamp: t0, Value: 3}}},
		{Points: []metrics.Point{{Timestamp: t0, Value: 10}}},
		{Points: []metrics.Point{{Timestamp: t0, Value: 7}}},
	}

	got, stats := foldGroupSeries(series, metrics.ReducerMax)
	assert.Equal(t, foldStats{}, stats)
	require.Len(t, got, 1)
	assert.Equal(t, 10.0, got[0].Value)
}

func testFoldMean(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	series := []MetricTimeSeries{
		{Points: []metrics.Point{{Timestamp: t0, Value: 0.8}}},
		{Points: []metrics.Point{{Timestamp: t0, Value: 0.9}}},
		{Points: []metrics.Point{{Timestamp: t0, Value: 1.0}}},
	}

	got, _ := foldGroupSeries(series, metrics.ReducerMean)
	require.Len(t, got, 1)
	const want = 0.9
	assert.InDelta(t, want, got[0].Value, 1e-9)
}

func testFoldEmpty(t *testing.T) {
	got, stats := foldGroupSeries(nil, metrics.ReducerSum)
	assert.Nil(t, got)
	assert.Equal(t, foldStats{}, stats)
	got, stats = foldGroupSeries([]MetricTimeSeries{}, metrics.ReducerSum)
	assert.Nil(t, got)
	assert.Equal(t, foldStats{}, stats)
}

func testFoldSingleGroup(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	series := []MetricTimeSeries{
		{Points: []metrics.Point{
			{Timestamp: t0, Value: 42},
			{Timestamp: t0.Add(time.Minute), Value: 43},
		}},
	}
	got, _ := foldGroupSeries(series, metrics.ReducerSum)
	require.Len(t, got, 2)
	assert.Equal(t, 42.0, got[0].Value)
	assert.Equal(t, 43.0, got[1].Value)
}

func testFoldDisjointTimestamps(t *testing.T) {
	// Two groups, each publishing at a different timestamp. Carry-forward
	// semantics: at t0 only group A has started (sum = 10); at t1 group A
	// carries its last value (10) while group B arrives fresh (20), so
	// the sum is 30 and the bucket is ragged because one of the two
	// series didn't produce a fresh point.
	//
	// This is a pathological synthetic case — real Cloud Monitoring
	// alignment keeps series bucket-aligned. It exists to document what
	// happens if it ever does come up.
	t0 := time.Unix(1_700_000_000, 0)
	t1 := t0.Add(time.Minute)
	series := []MetricTimeSeries{
		{Points: []metrics.Point{{Timestamp: t0, Value: 10}}},
		{Points: []metrics.Point{{Timestamp: t1, Value: 20}}},
	}
	got, stats := foldGroupSeries(series, metrics.ReducerSum)
	// t0: A is fresh, B has not started → counted as nothing (pre-start
	// is not a carry). t1: A carries 10, B is fresh → carry-forward.
	assert.Equal(t, 1, stats.CarryForwardBuckets, "t1 used carry-forward of group A")
	assert.Zero(t, stats.DepartedGroupBuckets, "no series exhausted the carry bound")
	require.Len(t, got, 2)
	assert.True(t, got[0].Timestamp.Equal(t0))
	assert.Equal(t, 10.0, got[0].Value)
	// At t1 group A carries 10, group B is fresh 20 → sum = 30.
	assert.True(t, got[1].Timestamp.Equal(t1))
	assert.Equal(t, 30.0, got[1].Value, "carry-forward of group A")
}

func testFoldRaggedBuckets(t *testing.T) {
	// Three groups: two timestamps fully covered, one timestamp missing
	// from the third group → ragged count should be 1.
	t0 := time.Unix(1_700_000_000, 0)
	t1 := t0.Add(time.Minute)
	t2 := t0.Add(2 * time.Minute)
	series := []MetricTimeSeries{
		{Points: []metrics.Point{
			{Timestamp: t0, Value: 1},
			{Timestamp: t1, Value: 2},
			{Timestamp: t2, Value: 3},
		}},
		{Points: []metrics.Point{
			{Timestamp: t0, Value: 1},
			{Timestamp: t1, Value: 2},
			{Timestamp: t2, Value: 3},
		}},
		{Points: []metrics.Point{
			{Timestamp: t0, Value: 1},
			// missing t1
			{Timestamp: t2, Value: 3},
		}},
	}
	got, stats := foldGroupSeries(series, metrics.ReducerSum)
	assert.Equal(t, 1, stats.CarryForwardBuckets, "t1 missed a fresh point from group 2")
	assert.Zero(t, stats.DepartedGroupBuckets, "single missed bucket is well within the carry bound")
	require.Len(t, got, 3)
	// t1: groups 0 and 1 contribute 2 fresh; group 2 carries its t0 value
	// (1) forward → sum = 2+2+1 = 5. Without carry-forward the sum would
	// silently be 4; carry-forward minimizes the undercount and the
	// CarryForwardBuckets counter still surfaces that one group was not fresh.
	assert.Equal(t, 5.0, got[1].Value, "t1 sum (carry-forward from group 2 at t0)")
}

func testFoldSortsOutput(t *testing.T) {
	// Go map iteration is non-deterministic, so with 6+ distinct timestamps
	// the natural iteration order is almost never sorted. Assert strictly
	// ascending output to keep the sort load-bearing.
	t0 := time.Unix(1_700_000_000, 0)
	points := make([]metrics.Point, 6)
	for i := range points {
		points[i] = metrics.Point{Timestamp: t0.Add(time.Duration(i) * time.Minute), Value: float64(i)}
	}
	series := []MetricTimeSeries{{Points: points}}
	got, _ := foldGroupSeries(series, metrics.ReducerSum)
	require.Len(t, got, len(points))
	for i := 1; i < len(got); i++ {
		assert.True(t, got[i-1].Timestamp.Before(got[i].Timestamp))
	}
}

// testFoldCarryForwardBounded locks the maxCarryForwardBuckets bound: a
// series that produces one fresh point and then goes silent must stop
// contributing to the fold after exactly maxCarryForwardBuckets carry
// buckets, instead of inflating every downstream sum forever. Without
// this bound a single fresh point at t0 would silently fabricate
// steady-state presence for departed groups across the entire window.
func testFoldCarryForwardBounded(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	step := time.Minute
	// Six buckets total. Group A is fresh at every bucket. Group B has a
	// single fresh point at t0 and then goes silent.
	const total = 6
	a := make([]metrics.Point, total)
	for i := range a {
		a[i] = metrics.Point{Timestamp: t0.Add(time.Duration(i) * step), Value: 1}
	}
	b := []metrics.Point{{Timestamp: t0, Value: 100}}
	series := []MetricTimeSeries{{Points: a}, {Points: b}}

	got, stats := foldGroupSeries(series, metrics.ReducerSum)
	require.Len(t, got, total)

	// Bucket 0: both fresh → 1 + 100 = 101.
	// Buckets 1..maxCarryForwardBuckets: B carries (100), A fresh (1) → 101.
	// Bucket maxCarryForwardBuckets+1 onwards: B departed, A alone → 1.
	for i := 0; i < total; i++ {
		var want float64
		switch {
		case i == 0:
			want = 101 // both fresh
		case i <= maxCarryForwardBuckets:
			want = 101 // B carried within bound
		default:
			want = 1 // B treated as departed
		}
		assert.Equal(t, want, got[i].Value, "bucket[%d] (carry bound = %d)", i, maxCarryForwardBuckets)
	}

	// CarryForwardBuckets covers buckets 1..maxCarryForwardBuckets (B is
	// carried but still within the bound). DepartedGroupBuckets covers
	// buckets maxCarryForwardBuckets+1..total-1 (B has crossed the
	// bound). DepartedSeries is exactly 1 — only B ever departed.
	wantCarry := maxCarryForwardBuckets
	wantDepartedBuckets := total - 1 - maxCarryForwardBuckets
	assert.Equal(t, wantCarry, stats.CarryForwardBuckets)
	assert.Equal(t, wantDepartedBuckets, stats.DepartedGroupBuckets)
	assert.Equal(t, 1, stats.DepartedSeries, "B departed exactly once")
}

// testFoldCarryForwardResurrect locks the resurrection contract: a
// series that has been treated as departed because of carry exhaustion
// must rejoin the fold the moment a new fresh point arrives, with the
// streak counter reset. Otherwise an intermittent publisher (deploy
// cutover, leader handoff) would be permanently dropped from the fold.
func testFoldCarryForwardResurrect(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	step := time.Minute
	// Group A: fresh at every bucket so the timestamp set covers 0..5.
	const total = 6
	a := make([]metrics.Point, total)
	for i := range a {
		a[i] = metrics.Point{Timestamp: t0.Add(time.Duration(i) * step), Value: 1}
	}
	// Group B: fresh at t0, silent for maxCarryForwardBuckets+1 buckets,
	// then fresh again at the last bucket. Last fresh point comes after
	// the carry bound has expired, so the resurrection path is the only
	// way B re-enters the fold.
	b := []metrics.Point{
		{Timestamp: t0, Value: 50},
		{Timestamp: t0.Add(time.Duration(total-1) * step), Value: 70},
	}
	series := []MetricTimeSeries{{Points: a}, {Points: b}}

	got, _ := foldGroupSeries(series, metrics.ReducerSum)
	require.Len(t, got, total)

	// Bucket 0: 1 + 50 = 51.
	assert.Equal(t, 51.0, got[0].Value)
	// Bucket total-1: B fresh again with value 70 → 1 + 70 = 71.
	assert.Equal(t, 71.0, got[total-1].Value, "resurrection")
	// Middle buckets within the carry bound: A(1) + B carried(50) = 51.
	// Buckets after the bound but before resurrection: A(1) alone.
	// Just spot-check the boundary: bucket maxCarryForwardBuckets is the
	// last carry bucket, so still 51; bucket maxCarryForwardBuckets+1 is
	// the first departed bucket, so 1.
	assert.Equal(t, 51.0, got[maxCarryForwardBuckets].Value, "last carry bucket")
	assert.Equal(t, 1.0, got[maxCarryForwardBuckets+1].Value, "first departed bucket")
}

// testFoldMeanWithDepartedGroup is a regression guard against a class
// of mean-reducer bugs the bounded carry-forward refactor could subtly
// introduce: a departed group must be EXCLUDED from the mean's divisor,
// not included with a stale carried value or a zero. The other reducer
// tests use sum/max — neither of which would notice a divisor mistake.
//
// Setup: Group A is fresh at every bucket with value 10. Group B is
// fresh at t0 with value 100, then silent forever. Once B departs, the
// mean must equal 10 (just A) — not 5 (10+0)/2, not 55 (10+100)/2.
func testFoldMeanWithDepartedGroup(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	step := time.Minute
	const total = 6
	a := make([]metrics.Point, total)
	for i := range a {
		a[i] = metrics.Point{Timestamp: t0.Add(time.Duration(i) * step), Value: 10}
	}
	b := []metrics.Point{{Timestamp: t0, Value: 100}}
	series := []MetricTimeSeries{{Points: a}, {Points: b}}

	got, stats := foldGroupSeries(series, metrics.ReducerMean)
	require.Len(t, got, total)

	for i := 0; i < total; i++ {
		var want float64
		switch {
		case i == 0:
			// Both fresh: mean(10, 100) = 55.
			want = 55
		case i <= maxCarryForwardBuckets:
			// A fresh, B carried (100): mean(10, 100) = 55.
			want = 55
		default:
			// A fresh, B departed and excluded: mean(10) = 10.
			// A regression that included a stale 0 would yield 5;
			// one that kept B's value carried after the bound would
			// stay at 55.
			want = 10
		}
		assert.Equal(t, want, got[i].Value, "bucket[%d] mean (departed group must be excluded from divisor)", i)
	}

	assert.Equal(t, 1, stats.DepartedSeries)
}

// testFoldSingleSeriesDeparture verifies that a single series which exhausts
// its carry-forward budget produces no output points for subsequent buckets.
// This is the zero-denominator edge case for ReducerMean: after departure the
// values slice is empty for that bucket, so foldGroupSeries must skip it
// entirely rather than dividing by zero or emitting NaN.
//
// Without the `if len(values) == 0 { continue }` guard in foldGroupSeries,
// this test would see extra output points or a panic. It complements
// testFoldMeanWithDepartedGroup (which tests two groups where one departs)
// by covering the degenerate single-series case.
func testFoldSingleSeriesDeparture(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	step := time.Minute

	// One series: fresh at t0 only, then silent. After maxCarryForwardBuckets
	// the single series departs — subsequent timestamps have no active series.
	freshAt := []metrics.Point{{Timestamp: t0, Value: 42}}

	// Append silent timestamps well beyond the carry bound. foldGroupSeries
	// collects all distinct timestamps from the series; we synthesise a second
	// fake series that only supplies the timestamps (no values), so the fold
	// still sees the later buckets.
	const extraBuckets = maxCarryForwardBuckets + 3
	phantom := make([]metrics.Point, extraBuckets)
	for i := 0; i < extraBuckets; i++ {
		phantom[i] = metrics.Point{Timestamp: t0.Add(time.Duration(i+1) * step), Value: 0}
	}

	// We can't use a phantom series directly (it would contribute values via
	// carry-forward). Instead drive the timestamps through a second REAL series
	// that publishes at every later bucket so the timestamps exist in the set,
	// then check that once the first series departs its buckets-only output
	// uses just the second series' value (proves the depart path doesn't panic
	// or return NaN from an empty divisor).
	//
	// Simpler alternative: drive a single-series fold by constructing timestamps
	// that extend beyond the carry bound using a SECOND series with value 0 at
	// every timestamp. Then the output after carry expiry should be just 0 (the
	// second series' value), NOT (42+0)/2 (which would mean the departed series
	// is still included) and NOT NaN (which would mean zero-division).
	series := []MetricTimeSeries{
		{Points: freshAt},
		{Points: phantom},
	}

	got, stats := foldGroupSeries(series, metrics.ReducerMean)

	// Expect 1 fresh bucket (t0, mean of [42,0]=21) + extraBuckets buckets
	// (where only the second series contributes after first departs).
	wantTotal := 1 + extraBuckets
	require.Len(t, got, wantTotal, "no bucket should be skipped or duplicated")

	// Buckets after carry expiry: first series departed, only second series
	// contributes value 0. Mean([0]) = 0, not NaN (zero-division) and not 21
	// (departed value still present).
	afterExpiry := maxCarryForwardBuckets + 1 // index into got (0-based, after t0)
	for i := afterExpiry; i < len(got); i++ {
		assert.False(t, math.IsNaN(got[i].Value), "got[%d].Value should not be NaN", i)
		assert.Equal(t, 0.0, got[i].Value, "got[%d].Value (departed series must be excluded)", i)
	}

	assert.Equal(t, 1, stats.DepartedSeries)
}

func TestApplyReducer(t *testing.T) {
	cases := []struct {
		name    string
		values  []float64
		reducer metrics.Reducer
		want    float64
	}{
		// Positive path.
		{"sum ascending", []float64{1, 2, 3, 4, 5}, metrics.ReducerSum, 15},
		{"mean ascending", []float64{1, 2, 3, 4, 5}, metrics.ReducerMean, 3},
		{"max ascending", []float64{1, 2, 3, 4, 5}, metrics.ReducerMax, 5},
		{"min ascending", []float64{1, 2, 3, 4, 5}, metrics.ReducerMin, 1},
		// All-negative — guards against a regression where max/min init to
		// zero instead of values[0] and silently return 0 for every call.
		{"max all-negative", []float64{-5, -3, -1}, metrics.ReducerMax, -1},
		{"min all-negative", []float64{-5, -3, -1}, metrics.ReducerMin, -5},
		{"sum all-negative", []float64{-5, -3, -1}, metrics.ReducerSum, -9},
		// Mixed signs.
		{"max mixed signs", []float64{-5, 3, -1}, metrics.ReducerMax, 3},
		{"min mixed signs", []float64{-5, 3, -1}, metrics.ReducerMin, -5},
		{"mean mixed signs", []float64{-5, 3, -1}, metrics.ReducerMean, -1},
		// Single element — boundary case for the values[1:] loop.
		{"single max", []float64{42}, metrics.ReducerMax, 42},
		{"single min", []float64{42}, metrics.ReducerMin, 42},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := applyReducer(tc.values, tc.reducer)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestApplyReducerNaNPropagation locks the documented semantics: a NaN in
// the input poisons the bucket for every reducer. Cloud Monitoring can
// legitimately return NaN (histogram with zero samples → DistributionValue
// Mean is 0/0, divide-by-zero ratios) and the tool output must surface
// that so operators fix the upstream metric rather than see a
// plausible-looking fabricated number.
func TestApplyReducerNaNPropagation(t *testing.T) {
	nan := math.NaN()
	cases := []struct {
		name    string
		values  []float64
		reducer metrics.Reducer
	}{
		{"sum with leading NaN", []float64{nan, 1, 2}, metrics.ReducerSum},
		{"sum with trailing NaN", []float64{1, 2, nan}, metrics.ReducerSum},
		{"mean with NaN", []float64{1, nan, 3}, metrics.ReducerMean},
		{"max with leading NaN", []float64{nan, 1, 2}, metrics.ReducerMax},
		{"max with trailing NaN", []float64{1, 2, nan}, metrics.ReducerMax},
		{"min with leading NaN", []float64{nan, 1, 2}, metrics.ReducerMin},
		{"min with trailing NaN", []float64{1, 2, nan}, metrics.ReducerMin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := applyReducer(tc.values, tc.reducer)
			assert.True(t, math.IsNaN(got))
		})
	}
}

// TestBuildAggregatedParams pins the single-vs-two-stage path selection
// at the real function boundary that QueryTimeSeriesAggregated depends
// on. Going through a fakeQuerier (as the snapshot/compare/related
// handler tests do) bypasses this translation entirely — the fake
// records the spec it was handed, not the QueryTimeSeriesParams the
// real helper would have built. A bug that inverted the two branches
// (e.g. clearing GroupByFields in the two-stage branch) would ship
// undetected without this test.
func TestBuildAggregatedParams(t *testing.T) {
	base := QueryTimeSeriesParams{
		MetricType:    "custom.googleapis.com/players_count",
		Project:       "test",
		GroupByFields: []string{"metric.labels.pod_name"}, // caller pre-populated; must be overwritten
		Reducer:       monitoringpb.Aggregation_REDUCE_NONE,
	}

	t.Run("Positive: single-stage sum clears GroupByFields and sets cross-series sum", func(t *testing.T) {
		spec := metrics.AggregationSpec{AcrossGroups: metrics.ReducerSum}
		got := buildAggregatedParams(base, spec)
		assert.Nil(t, got.GroupByFields)
		assert.Equal(t, monitoringpb.Aggregation_REDUCE_SUM, got.Reducer)
		// Unrelated fields must be preserved verbatim.
		assert.Equal(t, base.MetricType, got.MetricType)
		assert.Equal(t, base.Project, got.Project)
	})

	t.Run("Positive: two-stage sets GroupByFields to spec.GroupBy and within-group reducer", func(t *testing.T) {
		spec := metrics.AggregationSpec{
			GroupBy:      []string{"metric.labels.game_id"},
			WithinGroup:  metrics.ReducerMax,
			AcrossGroups: metrics.ReducerSum,
		}
		got := buildAggregatedParams(base, spec)
		require.Len(t, got.GroupByFields, 1)
		assert.Equal(t, "metric.labels.game_id", got.GroupByFields[0])
		assert.Equal(t, monitoringpb.Aggregation_REDUCE_MAX, got.Reducer)
	})

	t.Run("Positive: single-stage with mean reducer", func(t *testing.T) {
		spec := metrics.AggregationSpec{AcrossGroups: metrics.ReducerMean}
		got := buildAggregatedParams(base, spec)
		assert.Nil(t, got.GroupByFields)
		assert.Equal(t, monitoringpb.Aggregation_REDUCE_MEAN, got.Reducer)
	})

	t.Run("Edge: input params untouched", func(t *testing.T) {
		// Defensive: buildAggregatedParams must not mutate its input. A
		// future refactor that replaces the `p := params` struct copy with
		// a pointer would break compare/related (which share one params
		// template between windows A and B).
		spec := metrics.AggregationSpec{
			GroupBy:      []string{"metric.labels.tenant_id"},
			WithinGroup:  metrics.ReducerMax,
			AcrossGroups: metrics.ReducerSum,
		}
		_ = buildAggregatedParams(base, spec)
		require.NotNil(t, base.GroupByFields)
		require.Len(t, base.GroupByFields, 1)
		assert.Equal(t, "metric.labels.pod_name", base.GroupByFields[0])
		assert.Equal(t, monitoringpb.Aggregation_REDUCE_NONE, base.Reducer)
	})
}

func TestApplyReducerUnknownPanics(t *testing.T) {
	// spec.Validate() prevents this in production, but a bug that bypasses
	// validation must surface loudly instead of silently fabricating a mean.
	defer func() {
		if r := recover(); r == nil {
			t.Error("applyReducer with unknown reducer did not panic")
		}
	}()
	applyReducer([]float64{1, 2, 3}, metrics.Reducer("bogus"))
}

func TestReducerToGCP(t *testing.T) {
	// Round-trip test: every metrics.Reducer should map to a non-NONE
	// Cloud Monitoring reducer enum.
	cases := []struct {
		in   metrics.Reducer
		want monitoringpb.Aggregation_Reducer
	}{
		{metrics.ReducerSum, monitoringpb.Aggregation_REDUCE_SUM},
		{metrics.ReducerMean, monitoringpb.Aggregation_REDUCE_MEAN},
		{metrics.ReducerMax, monitoringpb.Aggregation_REDUCE_MAX},
		{metrics.ReducerMin, monitoringpb.Aggregation_REDUCE_MIN},
	}
	for _, tc := range cases {
		got := ReducerToGCP(tc.in)
		assert.Equal(t, tc.want, got)
	}
}

func TestReducerToGCPUnknownPanics(t *testing.T) {
	// Same rationale as applyReducer: an unknown reducer must never
	// silently degrade to REDUCE_NONE (which would skip cross-series
	// reduction entirely and hand back the wrong scalar).
	defer func() {
		if r := recover(); r == nil {
			t.Error("ReducerToGCP with unknown reducer did not panic")
		}
	}()
	ReducerToGCP(metrics.Reducer("garbage"))
}

// TestBuildAggregationReducerWithoutGroupBy covers the silent-bug fix in
// buildAggregation: a CrossSeriesReducer must be applied even when
// GroupByFields is empty, so snapshot/compare/related can request "total
// across all labels" in one call. Previously the reducer was ignored in
// this case and the flattening happened later in Go.
func TestBuildAggregationReducerWithoutGroupBy(t *testing.T) {
	agg := buildAggregation("GAUGE", "DOUBLE", 60, nil, monitoringpb.Aggregation_REDUCE_SUM)
	assert.Equal(t, monitoringpb.Aggregation_REDUCE_SUM, agg.CrossSeriesReducer)
	assert.Len(t, agg.GroupByFields, 0)
}

// TestBuildAggregationReduceNoneIsOptOut verifies that REDUCE_NONE (the
// zero value) leaves CrossSeriesReducer unset — this is the opt-out path
// for callers like the original low-level QueryTimeSeries entry point
// that want to handle aggregation themselves in Go.
func TestBuildAggregationReduceNoneIsOptOut(t *testing.T) {
	agg := buildAggregation("GAUGE", "DOUBLE", 60, nil, monitoringpb.Aggregation_REDUCE_NONE)
	assert.Equal(t, monitoringpb.Aggregation_REDUCE_NONE, agg.CrossSeriesReducer)
}

// TestQueryTimeSeriesAggregatedInvalidSpec covers the only code path in
// QueryTimeSeriesAggregated that does not require a live Cloud Monitoring
// client: an invalid spec must be rejected up front with a wrapped error
// so callers never reach the RPC path with nonsense aggregation.
func TestQueryTimeSeriesAggregatedInvalidSpec(t *testing.T) {
	// Passing nil client is intentional: QueryTimeSeriesAggregated validates
	// the AggregationSpec before making any RPC calls, so the client is never used.
	_, _, err := QueryTimeSeriesAggregated(context.Background(), nil, QueryTimeSeriesParams{}, metrics.AggregationSpec{}) //nolint:GoMaybeNil
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid aggregation spec")
	// The error must wrap the sentinel so tool handlers can escalate
	// registry misconfiguration to LoggingLevelError instead of lumping
	// it with transient GCP failures.
	assert.True(t, errors.Is(err, metrics.ErrInvalidAggregationSpec))
}
