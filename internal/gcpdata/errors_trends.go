package gcpdata

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/durationpb"

	errorreporting "cloud.google.com/go/errorreporting/apiv1beta1"
	"cloud.google.com/go/errorreporting/apiv1beta1/errorreportingpb"
)

// Error trend categories, comparing the recent half of the window to the older half.
const (
	TrendNew         = "new"         // absent in the older half, present in the recent half
	TrendGrowing     = "growing"     // more occurrences in the recent half
	TrendShrinking   = "shrinking"   // fewer occurrences in the recent half
	TrendDisappeared = "disappeared" // present in the older half, absent in the recent half
	TrendFlatCount   = "flat"        // unchanged (or no timed-count data available)
)

// trendBucketsPerWindow controls how finely the analysis window is sliced. A
// dozen buckets keeps the older/recent split clean while bounding response size.
const trendBucketsPerWindow = 12

// AnalyzeErrorTrends queries Error Reporting group stats with timed counts and
// classifies how each group's frequency changed between the older and recent
// halves of the lookback window. Error Reporting only supports windows ending at
// "now", so the comparison is intra-window (first half vs second half) rather
// than against an arbitrary historical baseline.
func AnalyzeErrorTrends(ctx context.Context, client *errorreporting.ErrorStatsClient, project string, timeRangeHours, limit int, serviceFilter, versionFilter string) (*ErrorTrendList, error) {
	ctx, cancel := context.WithTimeout(ctx, errorReportingTimeout)
	defer cancel()

	bucket := time.Duration(timeRangeHours) * time.Hour / trendBucketsPerWindow
	if bucket < time.Minute {
		bucket = time.Minute
	}

	req := &errorreportingpb.ListGroupStatsRequest{
		ProjectName:        fmt.Sprintf("projects/%s", project),
		TimeRange:          &errorreportingpb.QueryTimeRange{Period: timeRangePeriod(timeRangeHours)},
		TimedCountDuration: durationpb.New(bucket),
		PageSize:           safeInt32(limit),
		Order:              errorreportingpb.ErrorGroupOrder_COUNT_DESC,
	}
	if serviceFilter != "" || versionFilter != "" {
		req.ServiceFilter = &errorreportingpb.ServiceContextFilter{
			Service: serviceFilter,
			Version: versionFilter,
		}
	}

	it := client.ListGroupStats(ctx, req)

	var stats []*errorreportingpb.ErrorGroupStats
	for i := 0; i < limit; i++ {
		s, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("iterating error group stats: %w", err)
		}
		stats = append(stats, s)
	}

	result := analyzeTrendStats(stats, timeRangeHours)
	if tok := it.PageInfo().Token; tok != "" {
		result.Truncated = true
		result.TruncationHint = fmt.Sprintf("Analyzed the first %d error group(s) by total count; more groups exist for this window. Narrow service/version filters or lower the time range for a focused result.", limit)
	}
	return result, nil
}

// analyzeTrendStats classifies error-group stats into trends. Split from
// AnalyzeErrorTrends so the classification is testable without an API client.
func analyzeTrendStats(stats []*errorreportingpb.ErrorGroupStats, timeRangeHours int) *ErrorTrendList {
	midpoint, haveBuckets := timedCountsMidpoint(stats)

	trends := make([]ErrorTrend, 0, len(stats))
	summary := map[string]int{}
	for _, s := range stats {
		older, recent := splitTimedCounts(s.TimedCounts, midpoint, haveBuckets)
		category := classifyErrorTrend(older, recent, haveBuckets)
		summary[category]++

		et := ErrorTrend{
			Trend:       category,
			OlderCount:  older,
			RecentCount: recent,
			Delta:       recent - older,
			TotalCount:  s.Count,
			FirstSeen:   formatTimestamp(s.FirstSeenTime),
			LastSeen:    formatTimestamp(s.LastSeenTime),
		}
		if s.Group != nil {
			et.GroupID = s.Group.GroupId
		}
		if len(s.AffectedServices) > 0 {
			et.Service = s.AffectedServices[0].Service
		}
		if s.Representative != nil {
			et.Message = s.Representative.Message
		}
		trends = append(trends, et)
	}

	// Most-worsened first: largest positive delta (new and growing groups) at the
	// top, then by recent volume, then by group ID for a stable order.
	sort.SliceStable(trends, func(i, j int) bool {
		if trends[i].Delta != trends[j].Delta {
			return trends[i].Delta > trends[j].Delta
		}
		if trends[i].RecentCount != trends[j].RecentCount {
			return trends[i].RecentCount > trends[j].RecentCount
		}
		return trends[i].GroupID < trends[j].GroupID
	})

	result := &ErrorTrendList{
		Count:       len(trends),
		WindowHours: timeRangeHours,
		Summary:     summary,
		Trends:      trends,
	}
	if !haveBuckets {
		result.TruncationHint = "Error Reporting returned no timed-count data for this window, so trend direction could not be determined; all groups are reported as flat with their total counts."
	}
	return result
}

// timedCountsMidpoint returns the temporal midpoint of the window actually
// covered by the returned timed-count buckets, and whether any buckets exist.
// Using the observed bucket span (rather than wall-clock now) keeps the
// older/recent split aligned with the data the API returned.
func timedCountsMidpoint(stats []*errorreportingpb.ErrorGroupStats) (time.Time, bool) {
	var minStart, maxEnd time.Time
	seen := false
	for _, s := range stats {
		for _, tc := range s.TimedCounts {
			if tc.StartTime == nil || tc.EndTime == nil {
				continue
			}
			start := tc.StartTime.AsTime()
			end := tc.EndTime.AsTime()
			if !seen {
				minStart, maxEnd, seen = start, end, true
				continue
			}
			if start.Before(minStart) {
				minStart = start
			}
			if end.After(maxEnd) {
				maxEnd = end
			}
		}
	}
	if !seen {
		return time.Time{}, false
	}
	return minStart.Add(maxEnd.Sub(minStart) / 2), true
}

// splitTimedCounts sums a group's timed-count buckets into older (before the
// midpoint) and recent (at or after the midpoint) totals. When no buckets are
// available it returns zeros, and the caller treats the group as flat.
func splitTimedCounts(counts []*errorreportingpb.TimedCount, midpoint time.Time, haveBuckets bool) (older, recent int64) {
	if !haveBuckets {
		return 0, 0
	}
	for _, tc := range counts {
		if tc.StartTime == nil {
			continue
		}
		if tc.StartTime.AsTime().Before(midpoint) {
			older += tc.Count
		} else {
			recent += tc.Count
		}
	}
	return older, recent
}

// classifyErrorTrend labels a group from its older/recent occurrence counts.
func classifyErrorTrend(older, recent int64, haveBuckets bool) string {
	if !haveBuckets {
		return TrendFlatCount
	}
	switch {
	case older == 0 && recent > 0:
		return TrendNew
	case older > 0 && recent == 0:
		return TrendDisappeared
	case recent > older:
		return TrendGrowing
	case recent < older:
		return TrendShrinking
	default:
		return TrendFlatCount
	}
}
