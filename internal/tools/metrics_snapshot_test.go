package tools

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// TestSnapshotCallResult verifies that chartCallResult excludes chart_points
// from the LLM-facing content while leaving the original struct intact.
func TestSnapshotCallResult(t *testing.T) {
	pts := []chartPoint{{TS: 1700000000, V: 1.0}, {TS: 1700000060, V: 2.0}}
	result := &MetricSnapshotResult{
		MetricType:  "test/metric",
		Kind:        "GAUGE",
		Unit:        "1",
		Trend:       "stable",
		ChartPoints: pts,
	}

	cr, structured := chartCallResult(result, result.withoutChart(), chartStaticURI)
	assert.Same(t, result, structured, "structuredContent must be the full result")

	t.Run("content does not contain chart_points", func(t *testing.T) {
		require.Len(t, cr.Content, 1)
		text := cr.Content[0].(*mcp.TextContent).Text
		assert.NotContains(t, text, "chart_points", "LLM content must not include raw time-series data")
		var parsed map[string]any
		require.NoError(t, json.Unmarshal([]byte(text), &parsed))
		_, hasChartPoints := parsed["chart_points"]
		assert.False(t, hasChartPoints, "chart_points key must be absent from marshaled content")
	})

	t.Run("original struct is not mutated", func(t *testing.T) {
		assert.Equal(t, pts, result.ChartPoints, "withoutChart must not modify the caller's struct")
	})

	t.Run("not an error result", func(t *testing.T) {
		assert.False(t, cr.IsError)
	})

	t.Run("meta carries static chart URI", func(t *testing.T) {
		ui, ok := cr.Meta["ui"].(map[string]any)
		require.True(t, ok, "_meta.ui must be present")
		assert.Equal(t, chartStaticURI, ui["resourceUri"])
	})
}

// TestEmptyWindowMessage verifies that the "metric exists but window is
// empty" error message is tailored per metric kind and acknowledges the
// presence of a label filter when one was used. These branches are hard to
// hit from integration tests because each handler only surfaces one kind
// per test — a dedicated unit test locks them in.
func TestEmptyWindowMessage(t *testing.T) {
	const metric = "pubsub.googleapis.com/subscription/dead_letter_message_count"
	const window = "1h"

	tests := []struct {
		name        string
		kind        gcpdata.MetricKind
		labelFilter string
		// substrs are phrases that must all appear in the message. Used
		// for structural assertions rather than brittle exact-match checks.
		wantSubstrs []string
		// notSubstrs are phrases that must NOT appear. Guards against
		// regressions to the old "verify the metric_type" wording.
		notSubstrs []string
	}{
		{
			name: "delta counter with no filter suggests inactive counter",
			kind: "DELTA",
			wantSubstrs: []string{
				metric,
				window,
				"registered in Cloud Monitoring",
				"no events occurred",
				"dead_letter_message_count",
			},
			notSubstrs: []string{"verify the metric_type", "No data found"},
		},
		{
			name: "cumulative counter uses the same delta wording",
			kind: "CUMULATIVE",
			wantSubstrs: []string{
				"counter was inactive",
				"no events occurred",
			},
			notSubstrs: []string{"verify the metric_type"},
		},
		{
			name: "gauge points at resources, not events",
			kind: "GAUGE",
			wantSubstrs: []string{
				"no matching resources reported values",
			},
			notSubstrs: []string{"no events occurred"},
		},
		{
			name: "unknown kind falls through with no kind hint",
			kind: "",
			wantSubstrs: []string{
				"registered in Cloud Monitoring",
			},
			notSubstrs: []string{"no events occurred", "no matching resources"},
		},
		{
			name:        "label filter present swaps the suffix",
			kind:        "DELTA",
			labelFilter: `resource.labels.subscription_id="sub-a"`,
			wantSubstrs: []string{
				`label filter "resource.labels.subscription_id=\"sub-a\"" may also be excluding`,
			},
			notSubstrs: []string{
				"Try widening the window or removing any dimension/filter",
			},
		},
		{
			name: "no label filter uses the widening hint",
			kind: "DELTA",
			wantSubstrs: []string{
				"Try widening the window or removing any dimension/filter",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := emptyWindowMessage(metric, window, tc.kind, tc.labelFilter)
			for _, s := range tc.wantSubstrs {
				assert.Contains(t, got, s, "message should contain substring")
			}
			for _, s := range tc.notSubstrs {
				assert.NotContains(t, got, s, "message should not contain forbidden phrase")
			}
		})
	}
}

// TestBaselineWindows pins the exact bounds and labels every baseline mode
// samples for a fixed current window and event time.
func TestBaselineWindows(t *testing.T) {
	start := time.Date(2026, 3, 18, 10, 0, 0, 0, time.UTC) // a Wednesday
	end := start.Add(time.Hour)
	eventTime := time.Date(2026, 3, 18, 9, 45, 0, 0, time.UTC)
	at := func(month time.Month, day, hour, minute int) time.Time {
		return time.Date(2026, month, day, hour, minute, 0, 0, time.UTC)
	}
	for _, tc := range []struct {
		mode baselineMode
		want []baselineWindow
	}{
		{baselinePrevWindow, []baselineWindow{
			{start: at(3, 18, 9, 0), end: at(3, 18, 10, 0), label: "baseline (prev_window)"},
		}},
		{baselinePreEvent, []baselineWindow{
			{start: at(3, 18, 9, 15), end: at(3, 18, 9, 45), label: "baseline (pre_event)"},
		}},
		{baselineSameWeekdayHour, []baselineWindow{
			{start: at(3, 11, 10, 0), end: at(3, 11, 11, 0), label: "baseline (same_weekday_hour week -1)"},
			{start: at(3, 4, 10, 0), end: at(3, 4, 11, 0), label: "baseline (same_weekday_hour week -2)"},
			{start: at(2, 25, 10, 0), end: at(2, 25, 11, 0), label: "baseline (same_weekday_hour week -3)"},
			{start: at(2, 18, 10, 0), end: at(2, 18, 11, 0), label: "baseline (same_weekday_hour week -4)"},
		}},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			b := baselineSpec{mode: tc.mode}
			if tc.mode == baselinePreEvent {
				b.eventTime = eventTime
			}
			assert.Equal(t, tc.want, b.windows(start, end))
		})
	}
}

// TestQueryWithBaselineSkipsBaselineWithoutCurrentData pins the quota
// contract: the baseline windows are queried only after the current window
// returned points.
func TestQueryWithBaselineSkipsBaselineWithoutCurrentData(t *testing.T) {
	start := time.Date(2026, 3, 18, 10, 0, 0, 0, time.UTC)
	current := gcpdata.QueryTimeSeriesParams{Start: start, End: start.Add(time.Hour), StepSeconds: 60}
	windows := baselineSpec{mode: baselineSameWeekdayHour}.windows(current.Start, current.End)
	withPoints := []gcpdata.MetricTimeSeries{makeTimeSeries(start, []float64{1})}
	for _, tc := range []struct {
		name          string
		series        []gcpdata.MetricTimeSeries
		err           error
		wantQueries   int32
		wantBaselines int
	}{
		{"current failed", nil, errors.New("boom"), 1, 0},
		{"current empty", nil, nil, 1, 0},
		{"current series without points", []gcpdata.MetricTimeSeries{{}}, nil, 1, 0},
		{"current has data", withPoints, nil, 1 + int32(len(windows)), len(windows)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var queries atomic.Int32
			cur, baseline := queryWithBaseline(context.Background(), "test", current, windows,
				func(p gcpdata.QueryTimeSeriesParams) ([]gcpdata.MetricTimeSeries, gcpdata.QueryWarnings, error) {
					queries.Add(1)
					if p.Start.Equal(current.Start) {
						return tc.series, gcpdata.QueryWarnings{}, tc.err
					}
					return withPoints, gcpdata.QueryWarnings{}, nil
				})
			assert.Equal(t, tc.wantQueries, queries.Load())
			assert.Equal(t, tc.err, cur.err)
			assert.Len(t, baseline, tc.wantBaselines)
		})
	}
}

// TestCollectBaseline pins the baseline failure policy and its exact errors.
func TestCollectBaseline(t *testing.T) {
	start := time.Date(2026, 3, 18, 10, 0, 0, 0, time.UTC)
	weekly := baselineSpec{mode: baselineSameWeekdayHour}.windows(start, start.Add(time.Hour))
	prev := baselineSpec{mode: baselinePrevWindow}.windows(start, start.Add(time.Hour))
	data := windowResult{series: []gcpdata.MetricTimeSeries{makeTimeSeries(start, []float64{1})}}
	failed := windowResult{err: errors.New("boom")}
	panicked := windowResult{err: &panicError{value: "bug"}}
	empty := windowResult{}
	collect := func(mode baselineMode, windows []baselineWindow, results ...windowResult) (string, error) {
		return collectBaseline(context.Background(), nil, "test", "m", mode, windows, results)
	}

	t.Run("single window failed", func(t *testing.T) {
		_, err := collect(baselinePrevWindow, prev, failed)
		require.EqualError(t, err, "all 1 baseline queries failed: baseline (prev_window): boom")
	})
	t.Run("some failed and the rest had no data", func(t *testing.T) {
		_, err := collect(baselineSameWeekdayHour, weekly, failed, empty, failed, empty)
		require.EqualError(t, err, "2 of 4 baseline queries failed and the rest returned no data: "+
			"baseline (same_weekday_hour week -1): boom\nbaseline (same_weekday_hour week -3): boom")
	})
	t.Run("panic without data is a distinct error", func(t *testing.T) {
		_, err := collect(baselineSameWeekdayHour, weekly, failed, panicked, failed, failed)
		require.EqualError(t, err, "1 of 4 baseline queries panicked (a bug, not a transient failure): "+
			"baseline (same_weekday_hour week -1): boom\nbaseline (same_weekday_hour week -2): panic: bug\n"+
			"baseline (same_weekday_hour week -3): boom\nbaseline (same_weekday_hour week -4): boom")
		var pe *panicError
		assert.ErrorAs(t, err, &pe)
	})
	t.Run("failures beside data are a partial note", func(t *testing.T) {
		note, err := collect(baselineSameWeekdayHour, weekly, data, failed, data, data)
		require.NoError(t, err)
		assert.Equal(t, "Baseline partial failure (same_weekday_hour): 1 of 4 baseline windows could not be fetched; baseline computed from 3 windows. Results may be less reliable.", note)
	})
	t.Run("warnings of every window collapse into one note", func(t *testing.T) {
		warned := data
		warned.warnings = gcpdata.QueryWarnings{NonFinitePoints: 2, TruncatedSeries: true}
		note, err := collect(baselineSameWeekdayHour, weekly, warned, warned, warned, warned)
		require.NoError(t, err)
		assert.Equal(t, `metric "m" (baseline (same_weekday_hour, summed over 4 windows)): discarded 8 non-finite point(s) (NaN or infinity); no public numeric field contains a non-finite value. `+
			`metric "m" (baseline (same_weekday_hour, summed over 4 windows)): query hit the server-side time-series cap (500 series) and the result is truncated; aggregates are computed from a partial set of series only. Narrow the filter or group cardinality before trusting the numbers.`, note)
	})
}

// TestSnapshotBaselineFailureAdvice pins that the snapshot note tells a
// panic from a retryable failure and words non-retryable gRPC codes with the
// shared code guidance.
func TestSnapshotBaselineFailureAdvice(t *testing.T) {
	const metricType = "compute.googleapis.com/instance/cpu/utilization"
	for _, tc := range []struct {
		name     string
		failWith func() error
		want     []string
		notWant  []string
	}{
		{"panic", func() error { panic("bug") }, []string{"panicked", "This is a bug in the code"}, []string{"You can retry"}},
		{"permission denied", func() error { return status.Error(codes.PermissionDenied, "denied") }, []string{"lacks IAM permission"}, []string{"You can retry"}},
		{"not found", func() error { return status.Error(codes.NotFound, "gone") }, []string{"retrying will not help"}, []string{"You can retry"}},
		{"invalid argument", func() error { return status.Error(codes.InvalidArgument, "bad") }, []string{"rejected the baseline query"}, []string{"You can retry"}},
		{"unknown", func() error { return errors.New("flaky") }, []string{"You can retry"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fq := newFakeQuerier()
			fq.metricKinds[metricType] = gcpdata.MetricKindGauge
			now := time.Now().UTC()
			current := makeTimeSeries(now.Add(-time.Hour), stableValues(60, 0.5))
			fq.queryFn = func(p gcpdata.QueryTimeSeriesParams) ([]gcpdata.MetricTimeSeries, error) {
				if now.Sub(p.Start) < 2*time.Hour && now.Sub(p.End) < time.Minute {
					return []gcpdata.MetricTimeSeries{current}, nil
				}
				return nil, tc.failWith()
			}
			ctx := context.Background()
			ts := newTestToolServer(t)
			ts.registerMetricsSnapshot(fq, metrics.NewRegistry(), "test-project")
			ts.connect(ctx)
			defer ts.close()

			result, err := ts.callTool(ctx, "metrics_snapshot", map[string]any{"metric_type": metricType})
			require.NoError(t, err)
			require.False(t, result.IsError)
			var snap MetricSnapshotResult
			parseResult(t, result, &snap)
			assert.False(t, snap.BaselineReliable)
			for _, want := range tc.want {
				assert.Contains(t, snap.Note, want)
			}
			for _, notWant := range tc.notWant {
				assert.NotContains(t, snap.Note, notWant)
			}
		})
	}
}
