package gcpdata

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	"cloud.google.com/go/errorreporting/apiv1beta1/errorreportingpb"
)

func TestClassifyErrorTrend(t *testing.T) {
	tests := []struct {
		name        string
		older       int64
		recent      int64
		haveBuckets bool
		want        string
	}{
		{"new", 0, 5, true, TrendNew},
		{"disappeared", 7, 0, true, TrendDisappeared},
		{"growing", 3, 9, true, TrendGrowing},
		{"shrinking", 9, 3, true, TrendShrinking},
		{"flat equal", 4, 4, true, TrendFlatCount},
		{"flat zero", 0, 0, true, TrendFlatCount},
		{"no buckets is flat", 0, 5, false, TrendFlatCount},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyErrorTrend(tc.older, tc.recent, tc.haveBuckets))
		})
	}
}

// timedCount builds a one-hour bucket starting hoursFromBase hours after base.
func timedCount(base time.Time, hoursFromBase int, count int64) *errorreportingpb.TimedCount {
	start := base.Add(time.Duration(hoursFromBase) * time.Hour)
	return &errorreportingpb.TimedCount{
		Count:     count,
		StartTime: timestamppb.New(start),
		EndTime:   timestamppb.New(start.Add(time.Hour)),
	}
}

func TestTimedCountsMidpoint(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	t.Run("no buckets", func(t *testing.T) {
		_, ok := timedCountsMidpoint([]*errorreportingpb.ErrorGroupStats{{}})
		assert.False(t, ok)
	})

	t.Run("spans full range across groups", func(t *testing.T) {
		stats := []*errorreportingpb.ErrorGroupStats{
			{TimedCounts: []*errorreportingpb.TimedCount{timedCount(base, 0, 1)}},
			{TimedCounts: []*errorreportingpb.TimedCount{timedCount(base, 3, 1)}}, // ends at +4h
		}
		mid, ok := timedCountsMidpoint(stats)
		require.True(t, ok)
		// window [base, base+4h] -> midpoint base+2h
		assert.Equal(t, base.Add(2*time.Hour), mid)
	})
}

func TestSplitTimedCounts(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	mid := base.Add(2 * time.Hour)
	counts := []*errorreportingpb.TimedCount{
		timedCount(base, 0, 2), // older
		timedCount(base, 1, 3), // older
		timedCount(base, 2, 4), // recent (starts at midpoint)
		timedCount(base, 3, 5), // recent
	}
	older, recent := splitTimedCounts(counts, mid, true)
	assert.Equal(t, int64(5), older)
	assert.Equal(t, int64(9), recent)

	o2, r2 := splitTimedCounts(counts, mid, false)
	assert.Zero(t, o2)
	assert.Zero(t, r2)
}

func TestAnalyzeTrendStats(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	// window [base, base+4h], midpoint base+2h.
	group := func(id string, count int64, buckets ...*errorreportingpb.TimedCount) *errorreportingpb.ErrorGroupStats {
		return &errorreportingpb.ErrorGroupStats{
			Group:            &errorreportingpb.ErrorGroup{GroupId: id},
			Count:            count,
			TimedCounts:      buckets,
			Representative:   &errorreportingpb.ErrorEvent{Message: "msg-" + id},
			AffectedServices: []*errorreportingpb.ServiceContext{{Service: "svc-" + id}},
		}
	}
	stats := []*errorreportingpb.ErrorGroupStats{
		group("flat", 4, timedCount(base, 0, 2), timedCount(base, 3, 2)),     // older 2, recent 2 -> flat
		group("new", 10, timedCount(base, 2, 10)),                            // older 0, recent 10 -> new
		group("growing", 12, timedCount(base, 0, 4), timedCount(base, 3, 8)), // older 4, recent 8 -> growing, delta 4
		group("gone", 6, timedCount(base, 0, 6)),                             // older 6, recent 0 -> disappeared
	}

	res := analyzeTrendStats(stats, 4)
	require.NotNil(t, res)
	assert.Equal(t, 4, res.Count)
	assert.Equal(t, 4, res.WindowHours)
	assert.Equal(t, map[string]int{TrendFlatCount: 1, TrendNew: 1, TrendGrowing: 1, TrendDisappeared: 1}, res.Summary)

	// Sorted by delta desc: new (delta 10), growing (delta 4), flat (0), gone (-6).
	gotOrder := []string{res.Trends[0].Trend, res.Trends[1].Trend, res.Trends[2].Trend, res.Trends[3].Trend}
	assert.Equal(t, []string{TrendNew, TrendGrowing, TrendFlatCount, TrendDisappeared}, gotOrder)

	// Representative fields are carried through.
	assert.Equal(t, "svc-new", res.Trends[0].Service)
	assert.Equal(t, "msg-new", res.Trends[0].Message)
	assert.Equal(t, int64(10), res.Trends[0].RecentCount)
	assert.Equal(t, int64(10), res.Trends[0].Delta)
}

func TestAnalyzeTrendStats_NoBuckets(t *testing.T) {
	stats := []*errorreportingpb.ErrorGroupStats{
		{Group: &errorreportingpb.ErrorGroup{GroupId: "a"}, Count: 5},
	}
	res := analyzeTrendStats(stats, 24)
	require.Len(t, res.Trends, 1)
	assert.Equal(t, TrendFlatCount, res.Trends[0].Trend)
	assert.Equal(t, int64(5), res.Trends[0].TotalCount)
	assert.NotEmpty(t, res.TruncationHint, "should explain that trend data was unavailable")
}
