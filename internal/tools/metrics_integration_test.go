package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// --- fake querier ---

type fakeQuerier struct {
	descriptors []gcpdata.MetricDescriptorInfo
	series      map[string][]gcpdata.MetricTimeSeries // key: metricType
	seriesFunc  func(params gcpdata.QueryTimeSeriesParams) []gcpdata.MetricTimeSeries
	// metricKinds and valueTypes feed GetMetricDescriptor. valueTypes is
	// optional — when absent for a given metricType, the fake returns an
	// empty ValueType, which exercises the numeric-safe aligner path. Set
	// valueTypes["foo"] = "DISTRIBUTION" to test the distribution path.
	metricKinds map[string]string
	valueTypes  map[string]string
	// queryFn, when non-nil, takes precedence over seriesFunc and
	// queryTimeSeriesErr. It lets tests produce per-query success/failure
	// decisions (e.g. "week -1 succeeds, weeks -2..-4 fail") for exercising
	// the robust weekly baseline partial-tolerance path.
	queryFn func(params gcpdata.QueryTimeSeriesParams) ([]gcpdata.MetricTimeSeries, error)
	// aggregatedQueryFn, when non-nil, overrides the default
	// QueryTimeSeriesAggregated behavior (which delegates to the
	// single-stage path). Tests that want to assert the spec passed into
	// the aggregated path set this.
	aggregatedQueryFn func(params gcpdata.QueryTimeSeriesParams, spec metrics.AggregationSpec) ([]gcpdata.MetricTimeSeries, gcpdata.AggregationWarnings, error)
	// aggregatedSpecs records every AggregationSpec received by
	// QueryTimeSeriesAggregated in call order. Tests can read it after
	// running a tool to assert the resolved aggregation strategy.
	aggregatedSpecs []metrics.AggregationSpec
	// aggregatedWarnings is the AggregationWarnings the fake returns from
	// QueryTimeSeriesAggregated. Zero value = "no warnings" (production
	// default). Tests that exercise warning plumbing populate this.
	aggregatedWarnings gcpdata.AggregationWarnings
	// queryLogMu guards concurrent appends to queryLog. Metrics handlers
	// fan out parallel QueryTimeSeries calls (compare, top_contributors,
	// snapshot same_weekday_hour), and without this lock the race detector
	// flags the append as a data race.
	queryLogMu sync.Mutex
	queryLog   []gcpdata.QueryTimeSeriesParams
	// resourceLabels feeds GetResourceLabels (monitored resource type →
	// label keys). Tests that exercise the label-enrichment path populate
	// it; tests that don't leave it nil and GetResourceLabels returns
	// (nil, nil) — matching the "unknown type" contract of the real impl.
	resourceLabels map[string][]string

	getMetricDescriptorErr   error
	listMetricDescriptorsErr error
	queryTimeSeriesErr       error
	getResourceLabelsErr     error
}

func newFakeQuerier() *fakeQuerier {
	return &fakeQuerier{
		series:      make(map[string][]gcpdata.MetricTimeSeries),
		metricKinds: make(map[string]string),
		valueTypes:  make(map[string]string),
	}
}

func (f *fakeQuerier) GetMetricDescriptor(_ context.Context, _, metricType string) (gcpdata.MetricDescriptorBasic, error) {
	if f.getMetricDescriptorErr != nil {
		return gcpdata.MetricDescriptorBasic{}, f.getMetricDescriptorErr
	}
	kind := f.metricKinds[metricType]
	valueType := f.valueTypes[metricType]
	// Match production contract: the real GCP API always returns both
	// MetricKind AND ValueType. If a test sets kind but forgets valueType,
	// default to INT64 — that way the test at least exercises a valid
	// aligner path instead of silently triggering the empty-string
	// fallback, which would let DELTA+DISTRIBUTION bugs hide.
	if kind != "" && valueType == "" {
		valueType = "INT64"
	}
	basic := gcpdata.MetricDescriptorBasic{
		Kind:      kind,
		ValueType: valueType,
	}
	// If a matching MetricDescriptorInfo is registered in f.descriptors,
	// also populate Labels and MonitoredResourceTypes from it. This keeps
	// the fake's GetMetricDescriptor aligned with the real querier, which
	// returns both in a single RPC — so availableLabelsFromDescriptor sees
	// the same shape in tests as in production.
	for _, d := range f.descriptors {
		if d.Type == metricType {
			basic.Labels = append([]gcpdata.LabelDescriptor(nil), d.Labels...)
			basic.MonitoredResourceTypes = append([]string(nil), d.MonitoredResourceTypes...)
			break
		}
	}
	return basic, nil
}

func (f *fakeQuerier) ListMetricDescriptors(_ context.Context, _, _ string, limit int) ([]gcpdata.MetricDescriptorInfo, error) {
	if f.listMetricDescriptorsErr != nil {
		return nil, f.listMetricDescriptorsErr
	}
	result := f.descriptors
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (f *fakeQuerier) QueryTimeSeries(_ context.Context, params gcpdata.QueryTimeSeriesParams) ([]gcpdata.MetricTimeSeries, error) {
	f.queryLogMu.Lock()
	f.queryLog = append(f.queryLog, params)
	f.queryLogMu.Unlock()
	if f.queryFn != nil {
		return f.queryFn(params)
	}
	if f.queryTimeSeriesErr != nil {
		return nil, f.queryTimeSeriesErr
	}
	if f.seriesFunc != nil {
		return f.seriesFunc(params), nil
	}
	return f.series[params.MetricType], nil
}

func (f *fakeQuerier) QueryTimeSeriesAggregated(ctx context.Context, params gcpdata.QueryTimeSeriesParams, spec metrics.AggregationSpec) ([]gcpdata.MetricTimeSeries, gcpdata.AggregationWarnings, error) {
	f.queryLogMu.Lock()
	f.aggregatedSpecs = append(f.aggregatedSpecs, spec)
	warnings := f.aggregatedWarnings
	f.queryLogMu.Unlock()
	if f.aggregatedQueryFn != nil {
		return f.aggregatedQueryFn(params, spec)
	}
	// Default behavior: defer to the single-stage QueryTimeSeries so
	// existing tests that only populate .series / .seriesFunc keep
	// working without knowing about aggregation.
	series, err := f.QueryTimeSeries(ctx, params)
	return series, warnings, err
}

func (f *fakeQuerier) GetResourceLabels(_ context.Context, _, resourceType string) ([]string, error) {
	if f.getResourceLabelsErr != nil {
		return nil, f.getResourceLabelsErr
	}
	labels, ok := f.resourceLabels[resourceType]
	if !ok {
		return nil, nil
	}
	out := make([]string, len(labels))
	copy(out, labels)
	return out, nil
}

// --- test helpers ---

func makeTimeSeries(baseTime time.Time, values []float64) gcpdata.MetricTimeSeries {
	var points []metrics.Point
	for i, v := range values {
		points = append(points, metrics.Point{
			Timestamp: baseTime.Add(time.Duration(i) * time.Minute),
			Value:     v,
		})
	}
	return gcpdata.MetricTimeSeries{MetricKind: "GAUGE", ValueType: "DOUBLE", Points: points}
}

func makeTimeSeriesWithLabels(baseTime time.Time, values []float64, metricLabels map[string]string) gcpdata.MetricTimeSeries {
	ts := makeTimeSeries(baseTime, values)
	ts.MetricLabels = metricLabels
	return ts
}

func loadTestRegistry(t *testing.T, yaml string) *metrics.Registry {
	t.Helper()
	path := t.TempDir() + "/registry.yaml"
	err := os.WriteFile(path, []byte(yaml), 0o644)
	require.NoError(t, err)
	reg, err := metrics.LoadRegistry(path)
	require.NoError(t, err)
	return reg
}

const testRegistryYAML = `metrics:
  "compute.googleapis.com/instance/cpu/utilization":
    kind: resource_utilization
    unit: ratio
    better_direction: down
    slo_threshold: 0.8
    saturation_cap: 1.0
    related_metrics:
      - "compute.googleapis.com/instance/memory/utilization"
  "compute.googleapis.com/instance/memory/utilization":
    kind: resource_utilization
    unit: ratio
    better_direction: down
    slo_threshold: 0.9
  "custom.googleapis.com/api/latency":
    kind: latency
    unit: seconds
    better_direction: down
    slo_threshold: 0.5
`

func parseResult(t *testing.T, result *mcp.CallToolResult, target any) {
	t.Helper()
	require.NotNil(t, result)
	require.False(t, result.IsError)
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			err := json.Unmarshal([]byte(tc.Text), target)
			require.NoError(t, err)
			return
		}
	}
	require.Fail(t, "no text content in result")
}

func expectError(t *testing.T, result *mcp.CallToolResult, contains string) {
	t.Helper()
	require.NotNil(t, result)
	require.True(t, result.IsError)
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			assert.Contains(t, tc.Text, contains)
			return
		}
	}
}

const cpuMetric = "compute.googleapis.com/instance/cpu/utilization"

// --- test data generators ---

// stableValues returns n values with deterministic small variance (±0.01 cycle).
func stableValues(n int, base float64) []float64 {
	vals := make([]float64, n)
	for i := range vals {
		vals[i] = base + float64(i%3)*0.01 - 0.01
	}
	return vals
}

// makeRisingValues returns n values linearly interpolated from lo to hi.
func makeRisingValues(n int, lo, hi float64) []float64 {
	vals := make([]float64, n)
	for i := range vals {
		vals[i] = lo + (hi-lo)*float64(i)/float64(n-1)
	}
	return vals
}

// makeSpikyValues returns n values: (n-outliers) copies of base followed by
// outliers copies of spike.
func makeSpikyValues(n, outliers int, base, spike float64) []float64 {
	vals := make([]float64, n)
	for i := range vals {
		if i < n-outliers {
			vals[i] = base
		} else {
			vals[i] = spike
		}
	}
	return vals
}

// makeStepValues returns n values: first half at lo, second half at hi.
func makeStepValues(n int, lo, hi float64) []float64 {
	vals := make([]float64, n)
	for i := range vals {
		if i < n/2 {
			vals[i] = lo
		} else {
			vals[i] = hi
		}
	}
	return vals
}

// makeAlternatingValues returns n values alternating between lo and hi.
func makeAlternatingValues(n int, lo, hi float64) []float64 {
	vals := make([]float64, n)
	for i := range vals {
		if i%2 == 0 {
			vals[i] = lo
		} else {
			vals[i] = hi
		}
	}
	return vals
}

// snapshotSeriesFunc returns a seriesFunc that serves different data for the
// current and baseline windows. It distinguishes them by comparing params.End
// against cutoff (now - windowDur/2): queries ending after the cutoff are
// the current window; queries ending before are the baseline window.
func snapshotSeriesFunc(now time.Time, windowDur time.Duration, currentVals, baselineVals []float64) func(gcpdata.QueryTimeSeriesParams) []gcpdata.MetricTimeSeries {
	cutoff := now.Add(-windowDur / 2)
	return func(params gcpdata.QueryTimeSeriesParams) []gcpdata.MetricTimeSeries {
		if params.End.After(cutoff) {
			return []gcpdata.MetricTimeSeries{makeTimeSeries(params.Start, currentVals)}
		}
		return []gcpdata.MetricTimeSeries{makeTimeSeries(params.Start, baselineVals)}
	}
}

// --- integration tests ---

func TestSnapshotIntegration_StableMetric(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["compute.googleapis.com/instance/cpu/utilization"] = "GAUGE"

	now := time.Now().UTC()
	base := now.Add(-time.Hour)
	baselineBase := base.Add(-time.Hour)

	fq.series["compute.googleapis.com/instance/cpu/utilization"] = []gcpdata.MetricTimeSeries{
		makeTimeSeries(base, stableValues(60, 0.50)),
		makeTimeSeries(baselineBase, stableValues(60, 0.49)),
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsSnapshot(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_snapshot", map[string]any{
		"metric_type": "compute.googleapis.com/instance/cpu/utilization",
	})
	require.NoError(t, err)

	var snap MetricSnapshotResult
	parseResult(t, result, &snap)

	assert.Equal(t, "compute.googleapis.com/instance/cpu/utilization", snap.MetricType)
	assert.Equal(t, "resource_utilization", snap.Kind)
	assert.Equal(t, string(metrics.ClassStable), snap.Classification)
	assert.False(t, snap.SLOBreach)
	assert.Equal(t, "prev_window", snap.BaselineMode)
}

func TestSnapshotIntegration_SLOBreach(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["compute.googleapis.com/instance/cpu/utilization"] = "GAUGE"

	now := time.Now().UTC()
	base := now.Add(-time.Hour)
	baselineBase := base.Add(-time.Hour)

	// Current: high utilization (above 0.8 SLO threshold).
	highValues := stableValues(60, 0.90)
	fq.series["compute.googleapis.com/instance/cpu/utilization"] = []gcpdata.MetricTimeSeries{
		makeTimeSeries(base, highValues),
		makeTimeSeries(baselineBase, stableValues(60, 0.50)),
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsSnapshot(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_snapshot", map[string]any{
		"metric_type": "compute.googleapis.com/instance/cpu/utilization",
	})
	require.NoError(t, err)

	var snap MetricSnapshotResult
	parseResult(t, result, &snap)

	assert.True(t, snap.SLOBreach)
	require.NotNil(t, snap.SLOThreshold)
	assert.Equal(t, 0.8, *snap.SLOThreshold)
	assert.NotEqual(t, string(metrics.ClassStable), snap.Classification)
}

func TestSnapshotIntegration_LatencyPercentiles(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["custom.googleapis.com/api/latency"] = "GAUGE"

	now := time.Now().UTC()
	base := now.Add(-time.Hour)
	baselineBase := base.Add(-time.Hour)

	// Latency with some high-tail values.
	values := make([]float64, 60)
	for i := range values {
		values[i] = 0.1
	}
	values[58] = 0.8 // spike near end
	values[59] = 0.9

	fq.series["custom.googleapis.com/api/latency"] = []gcpdata.MetricTimeSeries{
		makeTimeSeries(base, values),
		makeTimeSeries(baselineBase, stableValues(60, 0.1)),
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsSnapshot(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_snapshot", map[string]any{
		"metric_type": "custom.googleapis.com/api/latency",
	})
	require.NoError(t, err)

	var snap MetricSnapshotResult
	parseResult(t, result, &snap)

	assert.Equal(t, "latency", snap.Kind)
	require.NotNil(t, snap.Percentiles)
	assert.Greater(t, snap.Percentiles.P99, snap.Percentiles.P50)
}

func TestSnapshotIntegration_NoData(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["compute.googleapis.com/instance/cpu/utilization"] = "GAUGE"
	// No series data → empty result.

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsSnapshot(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_snapshot", map[string]any{
		"metric_type": "compute.googleapis.com/instance/cpu/utilization",
	})
	require.NoError(t, err)
	// An empty window is a legitimate state (zero-events DELTA counter,
	// inactive gauge), not a tool failure. The handler should return a
	// successful result with NoData=true and a kind-aware note, so callers
	// can distinguish "nothing happened" from a real error.
	require.False(t, result.IsError)
	var snap MetricSnapshotResult
	parseResult(t, result, &snap)
	assert.True(t, snap.NoData)
	assert.Contains(t, snap.Note, "no data points")
	assert.Equal(t, "insufficient_data", snap.Classification)
}

// TestSnapshotIntegration_DeltaDistributionAligner is a regression guard
// for the ALIGN_RATE-on-DELTA-DISTRIBUTION crash. Latency histograms like
// pubsub ack_latencies are DELTA+DISTRIBUTION and the old handler only
// looked at MetricKind when building the aligner, so it sent ALIGN_RATE
// and got a 400 from Cloud Monitoring. The fix threads ValueType through
// QueryTimeSeriesParams and the aligner picker uses ALIGN_MEAN for any
// distribution regardless of kind. This test asserts that ValueType
// actually reaches the querier.
func TestSnapshotIntegration_DeltaDistributionAligner(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	const metricType = "pubsub.googleapis.com/subscription/ack_latencies"
	fq.metricKinds[metricType] = "DELTA"
	fq.valueTypes[metricType] = "DISTRIBUTION"

	now := time.Now().UTC()
	base := now.Add(-time.Hour)
	baselineBase := base.Add(-time.Hour)
	fq.series[metricType] = []gcpdata.MetricTimeSeries{
		makeTimeSeries(base, stableValues(60, 100.0)),
		makeTimeSeries(baselineBase, stableValues(60, 100.0)),
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsSnapshot(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_snapshot", map[string]any{
		"metric_type": metricType,
	})
	require.NoError(t, err)
	if result.IsError {
		for _, c := range result.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				require.Fail(t, "unexpected tool error: "+tc.Text)
			}
		}
		t.Fatal("unexpected tool error")
	}

	// Every QueryTimeSeries call made by the handler — including the
	// separate baseline-window query in buildBaselineStats — must carry
	// DELTA+DISTRIBUTION. A previous version of this test only checked
	// calls whose MetricType matched the primary metric and could miss a
	// baseline-only regression. Walk every entry to be safe.
	if len(fq.queryLog) == 0 {
		t.Fatal("expected at least one QueryTimeSeries call")
	}
	for i, q := range fq.queryLog {
		if q.MetricType != metricType {
			continue
		}
		if q.ValueType != "DISTRIBUTION" {
			assert.Equal(t, "DISTRIBUTION", i, q.ValueType)
		}
		if q.MetricKind != "DELTA" {
			assert.Equal(t, "DELTA", i, q.MetricKind)
		}
	}
}

// TestCompareIntegration_DeltaDistributionAligner is the metrics_compare
// counterpart of the snapshot regression guard. compare builds baseParams
// once and copies it twice for the A/B windows — a future refactor could
// drop ValueType from either copy, and only a live GCP call would notice.
func TestCompareIntegration_DeltaDistributionAligner(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	const metricType = "pubsub.googleapis.com/subscription/ack_latencies"
	fq.metricKinds[metricType] = "DELTA"
	fq.valueTypes[metricType] = "DISTRIBUTION"

	now := time.Now().UTC()
	fq.seriesFunc = func(_ gcpdata.QueryTimeSeriesParams) []gcpdata.MetricTimeSeries {
		return []gcpdata.MetricTimeSeries{makeTimeSeries(now.Add(-time.Hour), stableValues(60, 100.0))}
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsCompare(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_compare", map[string]any{
		"metric_type":   metricType,
		"window_a_from": now.Add(-2 * time.Hour).Format(time.RFC3339),
		"window_a_to":   now.Add(-1 * time.Hour).Format(time.RFC3339),
		"window_b_from": now.Add(-1 * time.Hour).Format(time.RFC3339),
		"window_b_to":   now.Format(time.RFC3339),
	})
	require.NoError(t, err)
	require.False(t, result.IsError)
	assertAllQueriesDistribution(t, fq.queryLog, metricType)
}

// TestTopIntegration_DeltaDistributionAligner is the metrics_top_contributors
// counterpart. top has its own grouped/reducer code path distinct from snapshot.
func TestTopIntegration_DeltaDistributionAligner(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	const metricType = "pubsub.googleapis.com/subscription/ack_latencies"
	fq.metricKinds[metricType] = "DELTA"
	fq.valueTypes[metricType] = "DISTRIBUTION"

	now := time.Now().UTC()
	tsSeries := makeTimeSeriesWithLabels(now.Add(-time.Hour), stableValues(60, 100.0), map[string]string{"subscription_id": "sub-a"})
	fq.series[metricType] = []gcpdata.MetricTimeSeries{tsSeries}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsTop(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_top_contributors", map[string]any{
		"metric_type": metricType,
		"dimension":   "metric.labels.subscription_id",
	})
	require.NoError(t, err)
	require.False(t, result.IsError)
	assertAllQueriesDistribution(t, fq.queryLog, metricType)
}

// TestRelatedIntegration_DeltaDistributionAligner is the metrics_related
// counterpart. related runs its queries inside goroutines, which is the
// highest-risk place for a field to get dropped via closure capture.
func TestRelatedIntegration_DeltaDistributionAligner(t *testing.T) {
	const primary = "custom.googleapis.com/myapp/main"
	const related = "pubsub.googleapis.com/subscription/ack_latencies"
	registry := `metrics:
  "` + primary + `":
    kind: latency
    unit: ms
    better_direction: down
    related_metrics:
      - "` + related + `"
  "` + related + `":
    kind: latency
    unit: ms
    better_direction: down
`
	reg := loadTestRegistry(t, registry)
	fq := newFakeQuerier()
	fq.metricKinds[related] = "DELTA"
	fq.valueTypes[related] = "DISTRIBUTION"

	now := time.Now().UTC()
	fq.seriesFunc = func(p gcpdata.QueryTimeSeriesParams) []gcpdata.MetricTimeSeries {
		if p.MetricType != related {
			return nil
		}
		return []gcpdata.MetricTimeSeries{makeTimeSeries(now.Add(-time.Hour), stableValues(60, 100.0))}
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsRelated(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	_, err := ts.callTool(ctx, "metrics_related", map[string]any{"metric_type": primary})
	require.NoError(t, err)
	assertAllQueriesDistribution(t, fq.queryLog, related)
}

// assertAllQueriesDistribution walks every QueryTimeSeries call recorded by
// the fake querier and asserts that every call for the given metricType
// carries ValueType=DISTRIBUTION and MetricKind=DELTA. Used by the four
// handler regression guards for the ALIGN_RATE+DELTA+DISTRIBUTION bug.
func assertAllQueriesDistribution(t *testing.T, queryLog []gcpdata.QueryTimeSeriesParams, metricType string) {
	t.Helper()
	var count int
	for _, q := range queryLog {
		if q.MetricType != metricType {
			continue
		}
		count++
		assert.Equal(t, "DISTRIBUTION", q.ValueType)
		assert.Equal(t, "DELTA", q.MetricKind)
	}
	require.Greater(t, count, 0)
}

func TestSnapshotIntegration_MissingMetricType(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsSnapshot(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_snapshot", map[string]any{})
	require.NoError(t, err)
	expectError(t, result, "metric_type")
}

func TestSnapshotIntegration_PreEventBaseline(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["compute.googleapis.com/instance/cpu/utilization"] = "GAUGE"

	now := time.Now().UTC()
	base := now.Add(-time.Hour)

	fq.series["compute.googleapis.com/instance/cpu/utilization"] = []gcpdata.MetricTimeSeries{
		makeTimeSeries(base, stableValues(60, 0.50)),
	}

	eventTime := now.Add(-30 * time.Minute).Format(time.RFC3339)

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsSnapshot(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_snapshot", map[string]any{
		"metric_type":   "compute.googleapis.com/instance/cpu/utilization",
		"baseline_mode": "pre_event",
		"event_time":    eventTime,
	})
	require.NoError(t, err)

	var snap MetricSnapshotResult
	parseResult(t, result, &snap)

	if snap.BaselineMode != "pre_event" {
		assert.Equal(t, "pre_event", snap.BaselineMode)
	}
}

// TestSnapshotIntegration_SameWeekdayHour_AllWeeks verifies the happy path of
// the robust weekly baseline: all four historical weeks return data, so the
// handler runs the 4-goroutine fan-out, uses ComputeRobustBaselineStats, and
// returns a successful snapshot with baseline_mode = same_weekday_hour.
func TestSnapshotIntegration_SameWeekdayHour_AllWeeks(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["compute.googleapis.com/instance/cpu/utilization"] = "GAUGE"
	fq.valueTypes["compute.googleapis.com/instance/cpu/utilization"] = "DOUBLE"

	now := time.Now().UTC()
	base := now.Add(-time.Hour)
	stable := makeTimeSeries(base, stableValues(60, 0.50))

	// Thread-safe per-week hit counter. queryLog has no mutex so the 4
	// concurrent baseline goroutines race there; we use a dedicated
	// sync.Mutex around a set of distinct week offsets seen.
	var mu sync.Mutex
	weekOffsets := make(map[int]int)
	fq.seriesFunc = func(p gcpdata.QueryTimeSeriesParams) []gcpdata.MetricTimeSeries {
		mu.Lock()
		daysBack := int(now.Sub(p.Start).Hours()/24 + 0.5)
		weekOffsets[daysBack]++
		mu.Unlock()
		return []gcpdata.MetricTimeSeries{stable}
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsSnapshot(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_snapshot", map[string]any{
		"metric_type":   "compute.googleapis.com/instance/cpu/utilization",
		"baseline_mode": "same_weekday_hour",
	})
	require.NoError(t, err)
	require.False(t, result.IsError)

	var snap MetricSnapshotResult
	parseResult(t, result, &snap)

	if snap.BaselineMode != "same_weekday_hour" {
		assert.Equal(t, "same_weekday_hour", snap.BaselineMode)
	}
	if !snap.BaselineReliable {
		assert.True(t, snap.BaselineReliable)
	}
	if snap.Note != "" {
		assert.Empty(t, snap.Note)
	}
	// Expect 5 distinct query targets: the current window (0 days back)
	// plus weeks -1..-4. Uses the race-free seriesFunc counter, not queryLog.
	for _, want := range []int{0, 7, 14, 21, 28} {
		assert.NotZero(t, weekOffsets[want])
	}
}

// TestSnapshotIntegration_SameWeekdayHour_PartialFailure verifies the
// partial-tolerance contract: when 1 week returns data and 3 error, the
// handler still succeeds using the surviving week. This is the most complex
// concurrent path in the snapshot handler and has historically had zero
// coverage.
func TestSnapshotIntegration_SameWeekdayHour_PartialFailure(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["compute.googleapis.com/instance/cpu/utilization"] = "GAUGE"
	fq.valueTypes["compute.googleapis.com/instance/cpu/utilization"] = "DOUBLE"

	now := time.Now().UTC()
	currentWindowStart := now.Add(-time.Hour)
	// Current-window series and the one surviving week's series both need
	// recent-ish timestamps so ComputeBaselineStats doesn't reject them.
	current := makeTimeSeries(currentWindowStart, stableValues(60, 0.50))
	week1 := makeTimeSeries(currentWindowStart.AddDate(0, 0, -7), stableValues(60, 0.48))

	// queryFn identifies each query by the start-time offset from now:
	//   0 days back (roughly)  → current window → success
	//  -7 days back              → week -1 → success
	//  -14, -21, -28 days back   → weeks -2..-4 → fail
	fq.queryFn = func(params gcpdata.QueryTimeSeriesParams) ([]gcpdata.MetricTimeSeries, error) {
		daysBack := int(now.Sub(params.Start).Hours()/24 + 0.5)
		switch daysBack {
		case 0:
			return []gcpdata.MetricTimeSeries{current}, nil
		case 7:
			return []gcpdata.MetricTimeSeries{week1}, nil
		default:
			return nil, errors.New("simulated quota failure")
		}
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsSnapshot(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_snapshot", map[string]any{
		"metric_type":   "compute.googleapis.com/instance/cpu/utilization",
		"baseline_mode": "same_weekday_hour",
	})
	require.NoError(t, err)
	require.False(t, result.IsError)

	var snap MetricSnapshotResult
	parseResult(t, result, &snap)
	assert.Equal(t, "same_weekday_hour", snap.BaselineMode)
	// Lock in the partial-tolerance contract: the surviving week's data
	// must have reached ComputeRobustBaselineStats, producing a non-zero
	// baseline mean (~0.48 from the week1 series).
	assert.Greater(t, snap.Baseline, 0.0)
	// The partial-tolerance path must surface a warning note explaining that
	// some weekly samples could not be fetched. This distinguishes it from a
	// clean baseline (no note) and from all-fail (a different note pattern).
	if !strings.Contains(snap.Note, "Baseline partial failure") {
		assert.Contains(t, snap.Note, "partial failure warning mentioning missing weekly samples")
	}
}

// TestSnapshotIntegration_SameWeekdayHour_AllFail verifies the non-fatal
// degradation contract: when every baseline week fails, the current-window
// snapshot is still returned with baseline_reliable=false and a Note
// explaining why. Before I1 this returned a hard tool error instead.
func TestSnapshotIntegration_SameWeekdayHour_AllFail(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["compute.googleapis.com/instance/cpu/utilization"] = "GAUGE"
	fq.valueTypes["compute.googleapis.com/instance/cpu/utilization"] = "DOUBLE"

	now := time.Now().UTC()
	current := makeTimeSeries(now.Add(-time.Hour), stableValues(60, 0.50))

	// Current window succeeds; every weekly baseline query fails.
	fq.queryFn = func(params gcpdata.QueryTimeSeriesParams) ([]gcpdata.MetricTimeSeries, error) {
		daysBack := int(now.Sub(params.Start).Hours()/24 + 0.5)
		if daysBack == 0 {
			return []gcpdata.MetricTimeSeries{current}, nil
		}
		return nil, errors.New("simulated auth failure")
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsSnapshot(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_snapshot", map[string]any{
		"metric_type":   "compute.googleapis.com/instance/cpu/utilization",
		"baseline_mode": "same_weekday_hour",
	})
	require.NoError(t, err)
	require.False(t, result.IsError)

	var snap MetricSnapshotResult
	parseResult(t, result, &snap)

	if snap.BaselineReliable {
		t.Error("baseline_reliable = true, want false when all weekly baselines failed")
	}
	if snap.Note == "" || !strings.Contains(snap.Note, "Baseline query") {
		assert.NotEmpty(t, snap.Note)
	}
	// The I1 non-fatal contract: current-window stats must still be present
	// even when every baseline query failed. A refactor that zeroed the
	// current window on the way to building the result would regress this.
	if snap.Current == 0 {
		t.Error("current = 0, want the current-window value (~0.50) from the still-successful current query")
	}
	if snap.DataQuality.ActualPoints == 0 {
		assert.Greater(t, snap.DataQuality.ActualPoints, int32(0))
	}
}

func TestSnapshotIntegration_PreEventMissingEventTime(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsSnapshot(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_snapshot", map[string]any{
		"metric_type":   "compute.googleapis.com/instance/cpu/utilization",
		"baseline_mode": "pre_event",
	})
	require.NoError(t, err)
	expectError(t, result, "event_time is required")
}

func TestSnapshotIntegration_InvalidWindow(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsSnapshot(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_snapshot", map[string]any{
		"metric_type": "compute.googleapis.com/instance/cpu/utilization",
		"window":      "99h",
	})
	require.NoError(t, err)
	expectError(t, result, "99h")
}

// --- metrics_list tests ---

func TestListIntegration_RegistryAndAPI(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.descriptors = []gcpdata.MetricDescriptorInfo{
		{Type: "compute.googleapis.com/instance/disk/read_bytes_count", DisplayName: "Disk Read Bytes", MetricKind: "DELTA", ValueType: "INT64", Unit: "By"},
		{Type: "compute.googleapis.com/instance/cpu/utilization", DisplayName: "CPU Utilization", MetricKind: "GAUGE", ValueType: "DOUBLE", Unit: "ratio"},
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsList(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	// Scope the query to compute instance metrics — without a match filter
	// the embedded default registry (~150 entries) would overflow the
	// default limit of 50 and the specific assertions below would become
	// non-deterministic due to Go map iteration order.
	result, err := ts.callTool(ctx, "metrics_list", map[string]any{
		"match": "compute.googleapis.com/instance",
	})
	require.NoError(t, err)

	var list MetricsListResult
	parseResult(t, result, &list)

	if list.Count == 0 {
		t.Fatal("expected at least one metric")
	}

	// Registry metrics should appear.
	found := false
	for _, m := range list.Metrics {
		if m.MetricType == "compute.googleapis.com/instance/cpu/utilization" {
			found = true
			if m.Kind != "resource_utilization" {
				assert.Equal(t, "resource_utilization", m.Kind)
			}
		}
	}
	if !found {
		t.Error("cpu/utilization not found in list")
	}

	// API-only metric should also appear (deduplicated).
	apiFound := false
	for _, m := range list.Metrics {
		if m.MetricType == "compute.googleapis.com/instance/disk/read_bytes_count" {
			apiFound = true
		}
	}
	if !apiFound {
		t.Error("disk/read_bytes_count not found in list")
	}
}

func TestListIntegration_FilterByMatch(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsList(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_list", map[string]any{
		"match": "cpu",
	})
	require.NoError(t, err)

	var list MetricsListResult
	parseResult(t, result, &list)

	for _, m := range list.Metrics {
		if !strings.Contains(strings.ToLower(m.MetricType), "cpu") {
			assert.Contains(t, m.MetricType, "cpu")
		}
	}
}

func TestListIntegration_FilterByKind(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsList(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_list", map[string]any{
		"kind": "latency",
	})
	require.NoError(t, err)

	var list MetricsListResult
	parseResult(t, result, &list)

	for _, m := range list.Metrics {
		if m.Kind != "latency" {
			assert.Equal(t, "latency", m.Kind)
		}
	}
}

func TestListIntegration_InvalidKind(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsList(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_list", map[string]any{
		"kind": "bogus",
	})
	require.NoError(t, err)
	expectError(t, result, "bogus")
}

func TestListIntegration_Truncation(t *testing.T) {
	reg := metrics.NewRegistry()
	fq := newFakeQuerier()

	// Generate more descriptors than the limit.
	for i := range 60 {
		fq.descriptors = append(fq.descriptors, gcpdata.MetricDescriptorInfo{
			Type: strings.Repeat("a", 10) + string(rune('A'+i%26)) + "/metric",
		})
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsList(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_list", map[string]any{
		"limit": 5.0,
	})
	require.NoError(t, err)

	var list MetricsListResult
	parseResult(t, result, &list)

	if list.Count > 5 {
		assert.LessOrEqual(t, list.Count, 5)
	}
}

// --- metrics_top_contributors tests ---

func TestTopContributorsIntegration(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["custom.googleapis.com/api/latency"] = "GAUGE"

	now := time.Now().UTC()
	base := now.Add(-time.Hour)

	// Two routes: /api/users is slow, /api/health is normal.
	fq.series["custom.googleapis.com/api/latency"] = []gcpdata.MetricTimeSeries{
		makeTimeSeriesWithLabels(base, stableValues(60, 0.8), map[string]string{"response_code": "200"}),
		makeTimeSeriesWithLabels(base, stableValues(60, 0.1), map[string]string{"response_code": "404"}),
		// Baseline (prev_window) - same data returned for simplicity.
		makeTimeSeriesWithLabels(base.Add(-time.Hour), stableValues(60, 0.1), map[string]string{"response_code": "200"}),
		makeTimeSeriesWithLabels(base.Add(-time.Hour), stableValues(60, 0.1), map[string]string{"response_code": "404"}),
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsTop(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_top_contributors", map[string]any{
		"metric_type": "custom.googleapis.com/api/latency",
		"dimension":   "metric.labels.response_code",
	})
	require.NoError(t, err)

	var top TopContributorsResult
	parseResult(t, result, &top)

	if top.Dimension != "metric.labels.response_code" {
		assert.NotEmpty(t, top.Dimension)
	}
	if len(top.Contributors) == 0 {
		t.Fatal("expected at least one contributor")
	}
	for _, c := range top.Contributors {
		assert.NotEmpty(t, c.Classification)
	}
}

func TestTopContributorsIntegration_MissingDimension(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsTop(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_top_contributors", map[string]any{
		"metric_type": "custom.googleapis.com/api/latency",
	})
	require.NoError(t, err)
	expectError(t, result, "dimension")
}

func TestTopContributorsIntegration_NoData(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["custom.googleapis.com/api/latency"] = "GAUGE"

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsTop(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_top_contributors", map[string]any{
		"metric_type": "custom.googleapis.com/api/latency",
		"dimension":   "metric.labels.route",
	})
	require.NoError(t, err)
	// Same contract as snapshot: empty window is a legitimate state, not a
	// failure. The handler returns a successful result with NoData=true,
	// empty Contributors, and a kind-aware note that also mentions the
	// dimension hint.
	if result.IsError {
		t.Fatal("expected success result, got error")
	}
	var top TopContributorsResult
	parseResult(t, result, &top)
	if !top.NoData {
		t.Error("expected NoData=true for empty window")
	}
	if len(top.Contributors) != 0 {
		assert.Empty(t, top.Contributors)
	}
	if !strings.Contains(top.Note, "no data points") {
		assert.NotEmpty(t, top.Note)
	}
	if !strings.Contains(top.Note, "dimension") {
		assert.Contains(t, top.Note, "dimension")
	}
}

// TestTopContributorsIntegration_DimensionMissingEverywhere verifies the
// all-missing tool error branch: when NO series exposes the requested
// dimension key, the handler returns a hard tool error pointing the caller
// at metrics_snapshot.available_labels. A regression that fell through to
// normal processing would return zero contributors silently.
func TestTopContributorsIntegration_DimensionMissingEverywhere(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["custom.googleapis.com/api/latency"] = "GAUGE"

	now := time.Now().UTC()
	base := now.Add(-time.Hour)

	// Two series, neither with the requested dimension label.
	fq.series["custom.googleapis.com/api/latency"] = []gcpdata.MetricTimeSeries{
		makeTimeSeriesWithLabels(base, stableValues(60, 0.5), map[string]string{"response_code": "200"}),
		makeTimeSeriesWithLabels(base, stableValues(60, 0.5), map[string]string{"response_code": "404"}),
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsTop(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_top_contributors", map[string]any{
		"metric_type": "custom.googleapis.com/api/latency",
		"dimension":   "metric.labels.nonexistent_key",
	})
	require.NoError(t, err)
	if !result.IsError {
		require.False(t, result.IsError)
	}
	msg := textFromResult(t, result)
	for _, want := range []string{"not found in any series labels", "available_labels", "metrics_snapshot"} {
		if !strings.Contains(msg, want) {
			assert.Contains(t, msg, want)
		}
	}
}

// TestTopContributorsIntegration_PartialDimensionCoverage verifies the
// partial-coverage contract: when some series expose the dimension and
// others do not, the handler (a) returns only the attributable contributors,
// (b) computes shares over that subset so they sum to 100%, and (c) surfaces
// a warning in Note. A regression that included the missing bucket in
// totalAbsDelta would dilute shares of real contributors.
func TestTopContributorsIntegration_PartialDimensionCoverage(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["custom.googleapis.com/api/latency"] = "GAUGE"

	now := time.Now().UTC()
	base := now.Add(-time.Hour)

	// Three current-window series: two expose route, one does not.
	// Corresponding baseline for the prev_window query mirrors the shape.
	fq.seriesFunc = func(_ gcpdata.QueryTimeSeriesParams) []gcpdata.MetricTimeSeries {
		return []gcpdata.MetricTimeSeries{
			makeTimeSeriesWithLabels(base, stableValues(60, 0.8), map[string]string{"route": "/users"}),
			makeTimeSeriesWithLabels(base, stableValues(60, 0.2), map[string]string{"route": "/health"}),
			// No "route" label anywhere — missing bucket.
			makeTimeSeriesWithLabels(base, stableValues(60, 0.5), map[string]string{"response_code": "200"}),
		}
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsTop(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_top_contributors", map[string]any{
		"metric_type": "custom.googleapis.com/api/latency",
		"dimension":   "metric.labels.route",
	})
	require.NoError(t, err)
	require.False(t, result.IsError)

	var top TopContributorsResult
	parseResult(t, result, &top)

	// Exactly the attributable contributors — the missing bucket must not
	// appear under label_value="(missing_dimension)".
	require.Len(t, top.Contributors, 2)
	for _, c := range top.Contributors {
		if c.LabelValue != "(missing_dimension)" {
			assert.NotEmpty(t, c.Classification)
		}
	}

	// Shares must sum to 100% over the attributable subset (no dilution
	// from the unlabelled series). Allow a small epsilon for float math.
	var shareSum float64
	for _, c := range top.Contributors {
		shareSum += c.ShareOfAnomaly
	}
	if shareSum > 0 {
		assert.InDelta(t, shareSum, 1.0, 0.01)
	}

	assert.Contains(t, top.Note, "Partial dimension coverage")
}

// TestTopContributorsIntegration_SameWeekdayHour_PartialBaselineFail verifies
// that when some same_weekday_hour baseline weeks fail, the handler still
// returns contributors and surfaces "Baseline partial failure" in Note.
// Regression guard: queryContributorBaselines returns (map, partialNote, error)
// — a refactor that dropped the partialNote return would pass other tests but
// break the operator-visible signal.
func TestTopContributorsIntegration_SameWeekdayHour_PartialBaselineFail(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["custom.googleapis.com/api/latency"] = "GAUGE"
	fq.valueTypes["custom.googleapis.com/api/latency"] = "DOUBLE"

	now := time.Now().UTC()
	current := makeTimeSeriesWithLabels(now.Add(-time.Hour), stableValues(60, 0.8), map[string]string{"response_code": "200"})

	// Current window (0 days back) and week -7 succeed; weeks -14, -21, -28 fail.
	fq.queryFn = func(params gcpdata.QueryTimeSeriesParams) ([]gcpdata.MetricTimeSeries, error) {
		daysBack := int(now.Sub(params.Start).Hours()/24 + 0.5)
		switch daysBack {
		case 0:
			return []gcpdata.MetricTimeSeries{current}, nil
		case 7:
			return []gcpdata.MetricTimeSeries{makeTimeSeriesWithLabels(
				now.Add(-time.Hour).AddDate(0, 0, -7), stableValues(60, 0.75),
				map[string]string{"response_code": "200"})}, nil
		default:
			return nil, errors.New("simulated quota failure")
		}
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsTop(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_top_contributors", map[string]any{
		"metric_type":   "custom.googleapis.com/api/latency",
		"dimension":     "metric.labels.response_code",
		"baseline_mode": "same_weekday_hour",
	})
	require.NoError(t, err)
	require.False(t, result.IsError)

	var top TopContributorsResult
	parseResult(t, result, &top)

	if len(top.Contributors) == 0 {
		t.Fatal("expected at least one contributor")
	}
	if !strings.Contains(top.Note, "Baseline partial failure") {
		assert.NotEmpty(t, top.Note)
	}
}

// --- metrics_related tests ---

func TestRelatedIntegration_FindsRelatedMetrics(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["compute.googleapis.com/instance/memory/utilization"] = "GAUGE"

	now := time.Now().UTC()
	base := now.Add(-time.Hour)

	// Related metric has data.
	fq.series["compute.googleapis.com/instance/memory/utilization"] = []gcpdata.MetricTimeSeries{
		makeTimeSeries(base, stableValues(60, 0.60)),
		makeTimeSeries(base.Add(-time.Hour), stableValues(60, 0.58)),
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsRelated(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_related", map[string]any{
		"metric_type": "compute.googleapis.com/instance/cpu/utilization",
	})
	require.NoError(t, err)

	var related RelatedSignalsResult
	parseResult(t, result, &related)

	if len(related.RelatedSignals) == 0 {
		t.Fatal("expected at least one related signal")
	}
	if related.RelatedSignals[0].MetricType != "compute.googleapis.com/instance/memory/utilization" {
		assert.NotEmpty(t, related.RelatedSignals[0].MetricType)
	}
	for _, s := range related.RelatedSignals {
		if s.Classification == "" {
			assert.NotEmpty(t, s.Classification)
		}
	}
}

func TestRelatedIntegration_NoRelatedConfigured(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsRelated(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	// memory/utilization has no related_metrics in registry.
	result, err := ts.callTool(ctx, "metrics_related", map[string]any{
		"metric_type": "compute.googleapis.com/instance/memory/utilization",
	})
	require.NoError(t, err)

	var related RelatedSignalsResult
	parseResult(t, result, &related)

	if related.Message == "" {
		t.Error("expected message about no related metrics")
	}
}

func TestRelatedIntegration_SkipsMetricWithNoData(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["compute.googleapis.com/instance/memory/utilization"] = "GAUGE"
	// No series data for memory metric → should be skipped.

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsRelated(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_related", map[string]any{
		"metric_type": "compute.googleapis.com/instance/cpu/utilization",
	})
	require.NoError(t, err)

	var related RelatedSignalsResult
	parseResult(t, result, &related)

	if len(related.Skipped) == 0 {
		t.Error("expected skipped signal for metric with no data")
	}
	if related.Partial {
		t.Error("Partial = true, want false: no-data is benign, not an RPC failure")
	}
}

// relatedTestRegistryYAML has a primary metric with THREE related signals,
// needed by the Partial / all-failed / mixed tests below that want to
// distinguish "one RPC fails" from "some succeed, some fail".
const relatedTestRegistryYAML = `metrics:
  "compute.googleapis.com/instance/cpu/utilization":
    kind: resource_utilization
    unit: ratio
    better_direction: down
    slo_threshold: 0.8
    saturation_cap: 1.0
    related_metrics:
      - "compute.googleapis.com/instance/memory/utilization"
      - "compute.googleapis.com/instance/disk/read_bytes_count"
      - "compute.googleapis.com/instance/network/received_bytes_count"
  "compute.googleapis.com/instance/memory/utilization":
    kind: resource_utilization
    unit: ratio
    better_direction: down
  "compute.googleapis.com/instance/disk/read_bytes_count":
    kind: throughput
    unit: bytes
    better_direction: none
  "compute.googleapis.com/instance/network/received_bytes_count":
    kind: throughput
    unit: bytes
    better_direction: none
`

// TestRelatedIntegration_PartialWithRPCFailures verifies that when some
// related signals succeed and others hit real RPC failures, the tool
// returns a success with Partial=true, the surviving signals in
// RelatedSignals, and the failures in Skipped. This is the primary
// defense-in-depth for the Partial flag introduced in R2-C1.
func TestRelatedIntegration_PartialWithRPCFailures(t *testing.T) {
	reg := loadTestRegistry(t, relatedTestRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["compute.googleapis.com/instance/memory/utilization"] = "GAUGE"
	fq.metricKinds["compute.googleapis.com/instance/disk/read_bytes_count"] = "DELTA"
	fq.metricKinds["compute.googleapis.com/instance/network/received_bytes_count"] = "DELTA"
	fq.valueTypes["compute.googleapis.com/instance/memory/utilization"] = "DOUBLE"
	fq.valueTypes["compute.googleapis.com/instance/disk/read_bytes_count"] = "INT64"
	fq.valueTypes["compute.googleapis.com/instance/network/received_bytes_count"] = "INT64"

	now := time.Now().UTC()
	base := now.Add(-time.Hour)
	memSeries := makeTimeSeries(base, stableValues(60, 0.60))

	// memory succeeds, disk/network fail with RPC errors. queryFn must
	// return data for the baseline window too, so we key off MetricType
	// rather than time range.
	fq.queryFn = func(p gcpdata.QueryTimeSeriesParams) ([]gcpdata.MetricTimeSeries, error) {
		switch p.MetricType {
		case "compute.googleapis.com/instance/memory/utilization":
			return []gcpdata.MetricTimeSeries{memSeries}, nil
		case "compute.googleapis.com/instance/disk/read_bytes_count":
			return nil, errors.New("permission denied")
		case "compute.googleapis.com/instance/network/received_bytes_count":
			return nil, errors.New("quota exceeded")
		}
		return nil, nil
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsRelated(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_related", map[string]any{
		"metric_type": "compute.googleapis.com/instance/cpu/utilization",
	})
	require.NoError(t, err)
	require.False(t, result.IsError)

	var related RelatedSignalsResult
	parseResult(t, result, &related)

	// loadTestRegistry overlays on top of the embedded default registry, so
	// cpu/utilization may have more related metrics than the three declared
	// in the test overlay. Assert on the signals/skips *we control* rather
	// than exact counts.
	foundMemorySuccess := false
	for _, s := range related.RelatedSignals {
		if s.MetricType == "compute.googleapis.com/instance/memory/utilization" {
			foundMemorySuccess = true
		}
	}
	if !foundMemorySuccess {
		assert.NotEmpty(t, related.RelatedSignals)
	}
	foundPermDenied, foundQuota := false, false
	for _, s := range related.Skipped {
		if s.MetricType == "compute.googleapis.com/instance/disk/read_bytes_count" && strings.Contains(s.Reason, "permission denied") {
			foundPermDenied = true
		}
		if s.MetricType == "compute.googleapis.com/instance/network/received_bytes_count" && strings.Contains(s.Reason, "quota exceeded") {
			foundQuota = true
		}
	}
	if !foundPermDenied {
		t.Error("disk read_bytes_count skip missing permission-denied reason")
	}
	if !foundQuota {
		t.Error("network received_bytes_count skip missing quota reason")
	}
	if !related.Partial {
		t.Error("Partial = false, want true when real RPC failures are present")
	}
	// The Note field must carry a human-readable summary of the RPC failures
	// so that an operator (or LLM) reading the result doesn't need to parse
	// every Skipped entry to understand why correlation coverage is partial.
	if !strings.Contains(related.Note, "RPC failures") {
		assert.Contains(t, related.Note, "RPC")
	}
}

// TestRelatedIntegration_AllRPCFailures_ToolError verifies the all-failed
// branch: when every signal hits a real RPC failure, the handler returns
// a hard tool error listing distinct reasons. Without this check a fully
// broken auth/quota/project state would masquerade as "empty correlations".
func TestRelatedIntegration_AllRPCFailures_ToolError(t *testing.T) {
	reg := loadTestRegistry(t, relatedTestRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["compute.googleapis.com/instance/memory/utilization"] = "GAUGE"
	fq.metricKinds["compute.googleapis.com/instance/disk/read_bytes_count"] = "DELTA"
	fq.metricKinds["compute.googleapis.com/instance/network/received_bytes_count"] = "DELTA"
	fq.queryTimeSeriesErr = errors.New("permission denied on everything")

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsRelated(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_related", map[string]any{
		"metric_type": "compute.googleapis.com/instance/cpu/utilization",
	})
	require.NoError(t, err)
	if !result.IsError {
		require.Fail(t, "expected tool error when all signals fail, got success")
	}
	msg := textFromResult(t, result)
	if !strings.Contains(msg, "correlation coverage is unavailable") {
		assert.Contains(t, msg, "failed")
	}
	if !strings.Contains(msg, "permission denied on everything") {
		assert.NotEmpty(t, msg)
	}
}

// TestRelatedIntegration_BenignSkipsDoNotMarkPartial verifies the contract
// that "no data in window" for every related signal is NOT a failure: the
// result is a success with Partial=false, empty RelatedSignals, and
// populated Skipped. A regression in classifyErr or the benign classification
// would flip Partial=true here.
func TestRelatedIntegration_BenignSkipsDoNotMarkPartial(t *testing.T) {
	reg := loadTestRegistry(t, relatedTestRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["compute.googleapis.com/instance/memory/utilization"] = "GAUGE"
	fq.metricKinds["compute.googleapis.com/instance/disk/read_bytes_count"] = "DELTA"
	fq.metricKinds["compute.googleapis.com/instance/network/received_bytes_count"] = "DELTA"
	// Every related query returns empty series with no error → benign skip.
	fq.seriesFunc = func(_ gcpdata.QueryTimeSeriesParams) []gcpdata.MetricTimeSeries { return nil }

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsRelated(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_related", map[string]any{
		"metric_type": "compute.googleapis.com/instance/cpu/utilization",
	})
	require.NoError(t, err)
	require.False(t, result.IsError)
	var related RelatedSignalsResult
	parseResult(t, result, &related)
	assert.Empty(t, related.RelatedSignals)
	require.NotEmpty(t, related.Skipped)
	require.False(t, related.Partial)
}

// TestRelatedIntegration_MixedBenignAndRealFailures verifies that a mix of
// benign skips and real RPC failures (with NO successes) still trips the
// all-failed branch — the tool error, not a silently empty result. This is
// the exact scenario round 2 flagged as hidden by the old
// `rpcFailures == len(skipped)` guard.
func TestRelatedIntegration_MixedBenignAndRealFailures(t *testing.T) {
	reg := loadTestRegistry(t, relatedTestRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["compute.googleapis.com/instance/memory/utilization"] = "GAUGE"
	fq.metricKinds["compute.googleapis.com/instance/disk/read_bytes_count"] = "DELTA"
	fq.metricKinds["compute.googleapis.com/instance/network/received_bytes_count"] = "DELTA"
	fq.valueTypes["compute.googleapis.com/instance/memory/utilization"] = "DOUBLE"
	fq.valueTypes["compute.googleapis.com/instance/disk/read_bytes_count"] = "INT64"
	fq.valueTypes["compute.googleapis.com/instance/network/received_bytes_count"] = "INT64"

	fq.queryFn = func(p gcpdata.QueryTimeSeriesParams) ([]gcpdata.MetricTimeSeries, error) {
		switch p.MetricType {
		case "compute.googleapis.com/instance/memory/utilization":
			// Benign: empty series, no error.
			return nil, nil
		case "compute.googleapis.com/instance/disk/read_bytes_count":
			return nil, errors.New("permission denied")
		case "compute.googleapis.com/instance/network/received_bytes_count":
			return nil, errors.New("quota exceeded")
		}
		return nil, nil
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsRelated(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_related", map[string]any{
		"metric_type": "compute.googleapis.com/instance/cpu/utilization",
	})
	require.NoError(t, err)
	if !result.IsError {
		require.False(t, result.IsError)
	}
	msg := textFromResult(t, result)
	if !strings.Contains(msg, "permission denied") {
		assert.Contains(t, msg, "failure")
	}
	// Benign reasons should NOT be listed in the distinct-reasons summary.
	if strings.Contains(msg, "no data") || strings.Contains(msg, "no events") {
		assert.NotContains(t, msg, "benign")
	}
}

// TestClassifyErr is a unit table test for the benign-vs-real classifier
// introduced in R2-C1. Regression guard: swapping the errors.Is checks or
// dropping the "deadline exceeded → real failure" branch would flip
// Partial / all-failed semantics site-wide.
func TestClassifyErr(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantReason string
		wantBenign bool
	}{
		{"nil", nil, "", true},
		{"client cancelled", context.Canceled, "cancelled", true},
		{"deadline exceeded", context.DeadlineExceeded, "deadline exceeded: context deadline exceeded", false},
		{"wrapped deadline exceeded",
			fmt.Errorf("outer: %w", context.DeadlineExceeded),
			"deadline exceeded: outer: context deadline exceeded", false},
		{"wrapped canceled",
			fmt.Errorf("query: %w", context.Canceled),
			"cancelled", true},
		{"generic RPC error", errors.New("permission denied"), "permission denied", false},
		{"grpc canceled", status.Error(codes.Canceled, "rpc cancelled"), "cancelled", true},
		{"grpc deadline exceeded",
			status.Error(codes.DeadlineExceeded, "rpc timed out"),
			"deadline exceeded: rpc error: code = DeadlineExceeded desc = rpc timed out", false},
		{"grpc not found",
			status.Error(codes.NotFound, "metric not found"),
			"metric type not found in project — check the registry entry is correct: rpc error: code = NotFound desc = metric not found", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, benign := classifyErr(tc.err)
			if reason != tc.wantReason {
				assert.Equal(t, tc.wantReason, reason)
			}
			if benign != tc.wantBenign {
				assert.Equal(t, tc.wantBenign, benign)
			}
		})
	}
}

// --- metrics_compare tests ---

func TestCompareIntegration_StableWindows(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["compute.googleapis.com/instance/cpu/utilization"] = "GAUGE"

	now := time.Now().UTC()
	aFrom := now.Add(-2 * time.Hour)
	aTo := now.Add(-time.Hour)
	bFrom := now.Add(-time.Hour)
	bTo := now

	fq.series["compute.googleapis.com/instance/cpu/utilization"] = []gcpdata.MetricTimeSeries{
		makeTimeSeries(aFrom, stableValues(60, 0.50)),
		makeTimeSeries(bFrom, stableValues(60, 0.51)),
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsCompare(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_compare", map[string]any{
		"metric_type":    "compute.googleapis.com/instance/cpu/utilization",
		"window_a_from":  aFrom.Format(time.RFC3339),
		"window_a_to":    aTo.Format(time.RFC3339),
		"window_b_from":  bFrom.Format(time.RFC3339),
		"window_b_to":    bTo.Format(time.RFC3339),
		"window_a_label": "before",
		"window_b_label": "after",
	})
	require.NoError(t, err)

	var cmp CompareResult
	parseResult(t, result, &cmp)

	if cmp.WindowALabel != "before" {
		assert.Equal(t, "before", cmp.WindowALabel)
	}
	if cmp.WindowBLabel != "after" {
		assert.Equal(t, "after", cmp.WindowBLabel)
	}
	if cmp.TrendShift != "unchanged" {
		assert.Equal(t, "unchanged", cmp.TrendShift)
	}
	if cmp.SLOBreachIntroduced {
		t.Error("unexpected SLO breach introduced for values ~0.50")
	}
}

func TestCompareIntegration_Degradation(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["compute.googleapis.com/instance/cpu/utilization"] = "GAUGE"

	now := time.Now().UTC().Truncate(time.Second)
	aFrom := now.Add(-2 * time.Hour)
	bFrom := now.Add(-time.Hour)
	bTo := now

	// Use seriesFunc to return different data based on query time range.
	fq.seriesFunc = func(params gcpdata.QueryTimeSeriesParams) []gcpdata.MetricTimeSeries {
		if params.Start.Before(bFrom) {
			// Window A: low utilization.
			return []gcpdata.MetricTimeSeries{makeTimeSeries(params.Start, stableValues(60, 0.30))}
		}
		// Window B: high utilization.
		return []gcpdata.MetricTimeSeries{makeTimeSeries(params.Start, stableValues(60, 0.90))}
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsCompare(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_compare", map[string]any{
		"metric_type":   "compute.googleapis.com/instance/cpu/utilization",
		"window_a_from": aFrom.Format(time.RFC3339),
		"window_a_to":   bFrom.Format(time.RFC3339),
		"window_b_from": bFrom.Format(time.RFC3339),
		"window_b_to":   bTo.Format(time.RFC3339),
	})
	require.NoError(t, err)

	var cmp CompareResult
	parseResult(t, result, &cmp)

	if cmp.TrendShift == "unchanged" {
		t.Error("expected degradation for 0.30 → 0.90 on a 'lower is better' metric")
	}
	if !cmp.SLOBreachIntroduced {
		t.Error("expected SLO breach introduced (window B ~0.90 > 0.8 threshold)")
	}
}

func TestCompareIntegration_InvalidWindowOrder(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()

	now := time.Now().UTC()

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsCompare(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_compare", map[string]any{
		"metric_type":   "compute.googleapis.com/instance/cpu/utilization",
		"window_a_from": now.Format(time.RFC3339),
		"window_a_to":   now.Add(-time.Hour).Format(time.RFC3339),
		"window_b_from": now.Add(-2 * time.Hour).Format(time.RFC3339),
		"window_b_to":   now.Add(-time.Hour).Format(time.RFC3339),
	})
	require.NoError(t, err)
	expectError(t, result, "must be after")
}

func TestCompareIntegration_MissingRequiredFields(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsCompare(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	// Missing metric_type.
	result, err := ts.callTool(ctx, "metrics_compare", map[string]any{
		"window_a_from": "2025-01-01T00:00:00Z",
		"window_a_to":   "2025-01-01T01:00:00Z",
		"window_b_from": "2025-01-01T01:00:00Z",
		"window_b_to":   "2025-01-01T02:00:00Z",
	})
	require.NoError(t, err)
	expectError(t, result, "metric_type")

	// Missing window_a_from.
	result, err = ts.callTool(ctx, "metrics_compare", map[string]any{
		"metric_type":   "compute.googleapis.com/instance/cpu/utilization",
		"window_a_to":   "2025-01-01T01:00:00Z",
		"window_b_from": "2025-01-01T01:00:00Z",
		"window_b_to":   "2025-01-01T02:00:00Z",
	})
	require.NoError(t, err)
	expectError(t, result, "window_a_from")
}

// TestCompareIntegration_NoDataPartial pins the partial-window NoData
// semantics: when one window is empty and the other has real data the
// response must be a success with NoData=true, the non-empty side must
// carry real stats (not zero), and TrendShift must reflect the transition
// rather than defaulting to "unchanged". Regression guard for the former
// behavior where all three cases collapsed into trend_shift="unchanged".
func TestCompareIntegration_NoDataPartial(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["compute.googleapis.com/instance/cpu/utilization"] = "GAUGE"

	now := time.Now().UTC().Truncate(time.Second)
	aFrom := now.Add(-2 * time.Hour)
	aTo := now.Add(-time.Hour)
	bFrom := now.Add(-time.Hour)
	bTo := now

	// Window A empty (inactive), window B populated.
	fq.seriesFunc = func(params gcpdata.QueryTimeSeriesParams) []gcpdata.MetricTimeSeries {
		if params.Start.Before(bFrom) {
			return nil
		}
		return []gcpdata.MetricTimeSeries{makeTimeSeries(params.Start, stableValues(60, 0.50))}
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsCompare(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_compare", map[string]any{
		"metric_type":    "compute.googleapis.com/instance/cpu/utilization",
		"window_a_from":  aFrom.Format(time.RFC3339),
		"window_a_to":    aTo.Format(time.RFC3339),
		"window_b_from":  bFrom.Format(time.RFC3339),
		"window_b_to":    bTo.Format(time.RFC3339),
		"window_a_label": "before",
		"window_b_label": "after",
	})
	require.NoError(t, err)

	var cmp CompareResult
	parseResult(t, result, &cmp)

	require.True(t, cmp.NoData)
	require.Len(t, cmp.NoDataWindows, 1)
	assert.Equal(t, "before", cmp.NoDataWindows[0])
	assert.Equal(t, "emerged", cmp.TrendShift)
	assert.Greater(t, cmp.WindowBMean, 0.0)
	if cmp.WindowAMean != 0 {
		assert.Equal(t, 0.0, cmp.WindowAMean)
	}
	if cmp.ClassificationA != string(metrics.ClassInsufficientData) {
		assert.Equal(t, metrics.ClassInsufficientData, cmp.ClassificationA)
	}
	if cmp.ClassificationB == string(metrics.ClassInsufficientData) {
		assert.NotEqual(t, "", cmp.ClassificationB)
	}
	if cmp.Note == "" {
		t.Error("Note should explain the empty window")
	}
}

// TestCompareIntegration_NoDataPartial_Disappeared is the mirror of the
// partial-window test above: window A has data, window B is empty. The
// transition must surface as TrendShift="disappeared", not "unchanged" —
// this is the symmetric step-change case the compare tool exists to
// highlight. Without this test a one-line switch-arm flip in
// metrics_compare.go:256-291 would ship silently.
func TestCompareIntegration_NoDataPartial_Disappeared(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["compute.googleapis.com/instance/cpu/utilization"] = "GAUGE"

	now := time.Now().UTC().Truncate(time.Second)
	aFrom := now.Add(-2 * time.Hour)
	bFrom := now.Add(-time.Hour)
	bTo := now

	// Window A populated, window B empty — the opposite of the
	// NoDataPartial test above.
	fq.seriesFunc = func(params gcpdata.QueryTimeSeriesParams) []gcpdata.MetricTimeSeries {
		if params.Start.Before(bFrom) {
			return []gcpdata.MetricTimeSeries{makeTimeSeries(params.Start, stableValues(60, 0.50))}
		}
		return nil
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsCompare(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_compare", map[string]any{
		"metric_type":    "compute.googleapis.com/instance/cpu/utilization",
		"window_a_from":  aFrom.Format(time.RFC3339),
		"window_a_to":    bFrom.Format(time.RFC3339),
		"window_b_from":  bFrom.Format(time.RFC3339),
		"window_b_to":    bTo.Format(time.RFC3339),
		"window_a_label": "before",
		"window_b_label": "after",
	})
	require.NoError(t, err)

	var cmp CompareResult
	parseResult(t, result, &cmp)

	require.True(t, cmp.NoData)
	require.Len(t, cmp.NoDataWindows, 1)
	assert.Equal(t, "after", cmp.NoDataWindows[0])
	if cmp.TrendShift != "disappeared" {
		assert.Equal(t, "disappeared", cmp.TrendShift)
	}
	if cmp.WindowAMean == 0 {
		assert.Greater(t, cmp.WindowAMean, 0.0)
	}
	if cmp.WindowBMean != 0 {
		assert.Equal(t, 0.0, cmp.WindowBMean)
	}
	if cmp.ClassificationB != string(metrics.ClassInsufficientData) {
		assert.Equal(t, metrics.ClassInsufficientData, cmp.ClassificationB)
	}
	if cmp.ClassificationA == string(metrics.ClassInsufficientData) {
		assert.NotEqual(t, "", cmp.ClassificationA)
	}
	if cmp.Note == "" {
		t.Error("Note should explain the empty window")
	}
}

// TestCompareIntegration_NoDataBothWindows verifies the both-empty case:
// NoData=true, both NoDataWindows entries, TrendShift=unchanged, note
// contains both explanations.
func TestCompareIntegration_NoDataBothWindows(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["compute.googleapis.com/instance/cpu/utilization"] = "GAUGE"

	now := time.Now().UTC().Truncate(time.Second)
	aFrom := now.Add(-2 * time.Hour)
	aTo := now.Add(-time.Hour)
	bFrom := now.Add(-time.Hour)
	bTo := now

	// Both windows empty.
	fq.series["compute.googleapis.com/instance/cpu/utilization"] = nil

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsCompare(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_compare", map[string]any{
		"metric_type":   "compute.googleapis.com/instance/cpu/utilization",
		"window_a_from": aFrom.Format(time.RFC3339),
		"window_a_to":   aTo.Format(time.RFC3339),
		"window_b_from": bFrom.Format(time.RFC3339),
		"window_b_to":   bTo.Format(time.RFC3339),
	})
	require.NoError(t, err)

	var cmp CompareResult
	parseResult(t, result, &cmp)

	require.True(t, cmp.NoData)
	require.Len(t, cmp.NoDataWindows, 2)
	assert.Equal(t, "unchanged", cmp.TrendShift)
}

// --- project resolution tests ---

func TestSnapshotIntegration_UsesDefaultProject(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["compute.googleapis.com/instance/cpu/utilization"] = "GAUGE"
	fq.series["compute.googleapis.com/instance/cpu/utilization"] = []gcpdata.MetricTimeSeries{
		makeTimeSeries(time.Now().UTC().Add(-time.Hour), stableValues(60, 0.50)),
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsSnapshot(fq, reg, "default-proj")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_snapshot", map[string]any{
		"metric_type": "compute.googleapis.com/instance/cpu/utilization",
	})
	require.NoError(t, err)

	// Should succeed using default project.
	var snap MetricSnapshotResult
	parseResult(t, result, &snap)

	// Verify the querier received the default project.
	if len(fq.queryLog) == 0 {
		t.Fatal("expected queries to be logged")
	}
	if fq.queryLog[0].Project != "default-proj" {
		assert.Equal(t, "default-proj", fq.queryLog[0].Project)
	}
}

func TestSnapshotIntegration_OverridesProject(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["compute.googleapis.com/instance/cpu/utilization"] = "GAUGE"
	fq.series["compute.googleapis.com/instance/cpu/utilization"] = []gcpdata.MetricTimeSeries{
		makeTimeSeries(time.Now().UTC().Add(-time.Hour), stableValues(60, 0.50)),
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsSnapshot(fq, reg, "default-proj")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_snapshot", map[string]any{
		"metric_type": "compute.googleapis.com/instance/cpu/utilization",
		"project_id":  "override-proj",
	})
	require.NoError(t, err)

	var snap MetricSnapshotResult
	parseResult(t, result, &snap)

	if len(fq.queryLog) == 0 {
		t.Fatal("expected queries to be logged")
	}
	if fq.queryLog[0].Project != "override-proj" {
		assert.Equal(t, "override-proj", fq.queryLog[0].Project)
	}
}

// --- error propagation tests ---

func TestSnapshotIntegration_MetricKindError(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.getMetricDescriptorErr = errForTest("metric descriptor lookup failed")

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsSnapshot(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_snapshot", map[string]any{
		"metric_type": "compute.googleapis.com/instance/cpu/utilization",
	})
	require.NoError(t, err)
	expectError(t, result, "Failed to look up metric descriptor")
}

func TestSnapshotIntegration_QueryError(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["compute.googleapis.com/instance/cpu/utilization"] = "GAUGE"
	fq.queryTimeSeriesErr = errForTest("permission denied")

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsSnapshot(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_snapshot", map[string]any{
		"metric_type": "compute.googleapis.com/instance/cpu/utilization",
	})
	require.NoError(t, err)
	expectError(t, result, "Failed to query metric")
}

func TestListIntegration_APIError(t *testing.T) {
	reg := metrics.NewRegistry()
	fq := newFakeQuerier()
	fq.listMetricDescriptorsErr = errForTest("quota exceeded")

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsList(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_list", map[string]any{})
	require.NoError(t, err)
	expectError(t, result, "Failed to list metrics")
}

// --- auto-detection tests ---

func TestSnapshotIntegration_AutoDetectedMetric(t *testing.T) {
	reg := metrics.NewRegistry() // empty registry → auto-detection
	fq := newFakeQuerier()
	fq.metricKinds["custom.googleapis.com/myapp/request_count"] = "DELTA"

	now := time.Now().UTC()
	base := now.Add(-time.Hour)

	fq.series["custom.googleapis.com/myapp/request_count"] = []gcpdata.MetricTimeSeries{
		makeTimeSeries(base, stableValues(60, 100)),
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsSnapshot(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_snapshot", map[string]any{
		"metric_type": "custom.googleapis.com/myapp/request_count",
	})
	require.NoError(t, err)

	var snap MetricSnapshotResult
	parseResult(t, result, &snap)

	if !snap.AutoDetected {
		t.Error("expected auto_detected = true for unregistered metric")
	}
	if snap.Kind != "throughput" {
		assert.Equal(t, "throughput", snap.Kind)
	}
}

// --- registry misconfiguration escalation tests ---

func TestSnapshotRegistryMisconfigError(t *testing.T) {
	// Use NewRegistryFromMetaMap to inject an invalid AggregationSpec that
	// bypasses load-time validation. The pre-flight Validate() in the handler
	// must catch it before issuing any RPC, so aggregatedSpecs stays empty.
	metricType := "compute.googleapis.com/instance/cpu/utilization"
	reg := metrics.NewRegistryFromMetaMap(map[string]metrics.MetricMeta{
		metricType: {
			Kind:            metrics.KindResourceUtilization,
			BetterDirection: metrics.DirectionDown,
			Aggregation: &metrics.AggregationSpec{
				AcrossGroups: "", // invalid: empty reducer
			},
		},
	})
	fq := newFakeQuerier()
	fq.metricKinds[metricType] = "GAUGE"

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsSnapshot(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_snapshot", map[string]any{
		"metric_type": metricType,
	})
	require.NoError(t, err)
	expectError(t, result, "Registry misconfiguration")
	expectError(t, result, metricType)
	if len(fq.aggregatedSpecs) != 0 {
		assert.Empty(t, fq.aggregatedSpecs)
	}
}

func TestCompareRegistryMisconfigError(t *testing.T) {
	// Use NewRegistryFromMetaMap to inject an invalid AggregationSpec that
	// bypasses load-time validation. The pre-flight Validate() in the handler
	// must catch it before issuing any RPC, so aggregatedSpecs stays empty.
	metricType := "compute.googleapis.com/instance/cpu/utilization"
	reg := metrics.NewRegistryFromMetaMap(map[string]metrics.MetricMeta{
		metricType: {
			Kind:            metrics.KindResourceUtilization,
			BetterDirection: metrics.DirectionDown,
			Aggregation: &metrics.AggregationSpec{
				AcrossGroups: "", // invalid: empty reducer
			},
		},
	})
	fq := newFakeQuerier()
	fq.metricKinds[metricType] = "GAUGE"

	now := time.Now().UTC()
	aFrom := now.Add(-2 * time.Hour).Format(time.RFC3339)
	aTo := now.Add(-time.Hour).Format(time.RFC3339)
	bFrom := now.Add(-time.Hour).Format(time.RFC3339)
	bTo := now.Format(time.RFC3339)

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsCompare(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_compare", map[string]any{
		"metric_type":   metricType,
		"window_a_from": aFrom,
		"window_a_to":   aTo,
		"window_b_from": bFrom,
		"window_b_to":   bTo,
	})
	require.NoError(t, err)
	expectError(t, result, "Registry misconfiguration")
	expectError(t, result, metricType)
	if len(fq.aggregatedSpecs) != 0 {
		assert.Empty(t, fq.aggregatedSpecs)
	}
}

func TestRelatedRegistryMisconfigError(t *testing.T) {
	// metrics_related catches an invalid AggregationSpec in the pre-flight
	// Validate() guard inside each per-signal goroutine. The signal is skipped
	// (logged at Error level) rather than causing a direct tool error. When
	// ALL signals are skipped via pre-flight misconfig, the all-failed branch
	// fires and includes "Registry misconfiguration" in the error text.
	primaryMetric := "compute.googleapis.com/instance/cpu/utilization"
	relatedMetric := "compute.googleapis.com/instance/memory/utilization"

	// Inject an invalid AggregationSpec directly on the related metric,
	// bypassing load-time validation, to exercise the pre-flight Validate()
	// guard before any RPC is issued for that signal.
	reg := metrics.NewRegistryFromMetaMap(map[string]metrics.MetricMeta{
		primaryMetric: {
			Kind:            metrics.KindResourceUtilization,
			BetterDirection: metrics.DirectionDown,
			RelatedMetrics:  []string{relatedMetric},
		},
		relatedMetric: {
			Kind:            metrics.KindResourceUtilization,
			BetterDirection: metrics.DirectionDown,
			Aggregation: &metrics.AggregationSpec{
				GroupBy:      []string{"metric.labels.zone"},
				WithinGroup:  "", // missing — Validate() rejects this
				AcrossGroups: metrics.ReducerMean,
			},
		},
	})
	fq := newFakeQuerier()
	fq.metricKinds[primaryMetric] = "GAUGE"
	fq.metricKinds[relatedMetric] = "GAUGE"

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsRelated(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_related", map[string]any{
		"metric_type": primaryMetric,
	})
	require.NoError(t, err)
	// All related signals failed via pre-flight, so the all-failed branch
	// fires with "Registry misconfiguration" in the skip reason.
	expectError(t, result, "real RPC failures")
	expectError(t, result, "Registry misconfiguration")
	// No data query should have been issued for the related metric.
	if len(fq.queryLog) > 0 {
		assert.Empty(t, fq.queryLog)
	}
}

// --- unsupported points tests ---

func TestSnapshotUnsupportedPointsNonFatal(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["compute.googleapis.com/instance/cpu/utilization"] = "GAUGE"

	now := time.Now().UTC()
	base := now.Add(-time.Hour)
	baselineBase := base.Add(-time.Hour)

	tsSeries := makeTimeSeries(base, stableValues(60, 0.50))
	tsSeries.UnsupportedCount = 3
	fq.series["compute.googleapis.com/instance/cpu/utilization"] = []gcpdata.MetricTimeSeries{
		tsSeries,
		makeTimeSeries(baselineBase, stableValues(60, 0.49)),
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsSnapshot(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_snapshot", map[string]any{
		"metric_type": "compute.googleapis.com/instance/cpu/utilization",
	})
	require.NoError(t, err)

	// Unsupported points are non-fatal: result must succeed.
	var snap MetricSnapshotResult
	parseResult(t, result, &snap)

	if snap.MetricType != "compute.googleapis.com/instance/cpu/utilization" {
		assert.NotEmpty(t, snap.MetricType)
	}
	if !strings.Contains(snap.Note, "Dropped") {
		assert.Contains(t, snap.Note, "it to mention dropped unsupported points")
	}
}

func TestTopRegistryMisconfigError(t *testing.T) {
	// Inject a metric with an invalid AggregationSpec (group_by set but
	// within_group missing) directly into the registry, bypassing load-time
	// validation, to exercise the pre-flight aggSpec.Validate() guard in
	// metrics_top_contributors before any GCP query is issued.
	metricType := "custom.googleapis.com/test/bad_agg"
	reg := metrics.NewRegistryFromMetaMap(map[string]metrics.MetricMeta{
		metricType: {
			Kind:            metrics.KindThroughput,
			BetterDirection: metrics.DirectionUp,
			Aggregation: &metrics.AggregationSpec{
				GroupBy:      []string{"metric.labels.game_id"},
				WithinGroup:  "", // missing — Validate() rejects this
				AcrossGroups: metrics.ReducerSum,
			},
		},
	})
	fq := newFakeQuerier()
	fq.metricKinds[metricType] = "GAUGE"

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsTop(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_top_contributors", map[string]any{
		"metric_type": metricType,
		"dimension":   "metric.labels.game_id",
	})
	require.NoError(t, err)
	expectError(t, result, "Registry misconfiguration")
	expectError(t, result, metricType)
	// No data query should have been issued — only the descriptor lookup.
	if len(fq.queryLog) > 0 {
		assert.Empty(t, fq.queryLog)
	}
}

func TestTopUnsupportedPointsNonFatal(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	metricType := "compute.googleapis.com/instance/cpu/utilization"
	fq.metricKinds[metricType] = "GAUGE"

	now := time.Now().UTC()
	base := now.Add(-time.Hour)
	tsSeries := makeTimeSeriesWithLabels(base, stableValues(60, 0.50), map[string]string{"instance_id": "i-123"})
	tsSeries.UnsupportedCount = 5
	fq.series[metricType] = []gcpdata.MetricTimeSeries{tsSeries}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsTop(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_top_contributors", map[string]any{
		"metric_type": metricType,
		"dimension":   "metric.labels.instance_id",
	})
	require.NoError(t, err)
	// Unsupported points are non-fatal: result must succeed.
	var top TopContributorsResult
	parseResult(t, result, &top)
	if top.Dimension != "metric.labels.instance_id" {
		assert.Equal(t, "metric.labels.instance_id", top.Dimension)
	}
	if !strings.Contains(top.Note, "Dropped") {
		assert.Contains(t, top.Note, "dropped")
	}
}

func TestCompareUnsupportedPointsNonFatal(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	metricType := "compute.googleapis.com/instance/cpu/utilization"
	fq.metricKinds[metricType] = "GAUGE"

	now := time.Now().UTC()
	base := now.Add(-2 * time.Hour)
	tsSeries := makeTimeSeries(base, stableValues(60, 0.50))
	tsSeries.UnsupportedCount = 3
	fq.series[metricType] = []gcpdata.MetricTimeSeries{tsSeries}

	aFrom := now.Add(-2 * time.Hour).Format(time.RFC3339)
	aTo := now.Add(-time.Hour).Format(time.RFC3339)
	bFrom := now.Add(-time.Hour).Format(time.RFC3339)
	bTo := now.Format(time.RFC3339)

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsCompare(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_compare", map[string]any{
		"metric_type":   metricType,
		"window_a_from": aFrom,
		"window_a_to":   aTo,
		"window_b_from": bFrom,
		"window_b_to":   bTo,
	})
	require.NoError(t, err)
	var cmp CompareResult
	parseResult(t, result, &cmp)
	if cmp.WindowALabel != "window_a" {
		assert.Equal(t, "window_a", cmp.WindowALabel)
	}
	if !strings.Contains(cmp.Note, "Dropped") {
		assert.Contains(t, cmp.Note, "Dropped")
	}
}

func TestRelatedUnsupportedPointsNonFatal(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	primaryMetric := "compute.googleapis.com/instance/cpu/utilization"
	relatedMetric := "compute.googleapis.com/instance/memory/utilization"
	fq.metricKinds[primaryMetric] = "GAUGE"
	fq.metricKinds[relatedMetric] = "GAUGE"

	now := time.Now().UTC()
	base := now.Add(-time.Hour)

	tsSeries := makeTimeSeries(base, stableValues(60, 0.50))
	tsSeries.UnsupportedCount = 2
	fq.series[relatedMetric] = []gcpdata.MetricTimeSeries{tsSeries}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsRelated(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_related", map[string]any{
		"metric_type": primaryMetric,
	})
	require.NoError(t, err)
	var rel RelatedSignalsResult
	parseResult(t, result, &rel)
	// The related signal must have been processed and appear in the result.
	if len(rel.RelatedSignals) == 0 {
		assert.Greater(t, len(rel.RelatedSignals), 0)
	}
}

func TestSnapshotTruncationWarningSurfacedInNote(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	metricType := "compute.googleapis.com/instance/cpu/utilization"
	fq.metricKinds[metricType] = "GAUGE"
	fq.valueTypes[metricType] = "DOUBLE"
	fq.series[metricType] = []gcpdata.MetricTimeSeries{
		makeTimeSeries(time.Now().UTC().Add(-time.Hour), stableValues(60, 0.50)),
	}
	fq.aggregatedWarnings = gcpdata.AggregationWarnings{TruncatedSeries: true}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsSnapshot(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_snapshot", map[string]any{
		"metric_type": metricType,
	})
	require.NoError(t, err)
	var snap MetricSnapshotResult
	parseResult(t, result, &snap)
	if !strings.Contains(snap.Note, "truncated") {
		assert.Contains(t, snap.Note, "truncation")
	}
}

func TestSnapshotBaselineTruncationWarningSurfacedInNote(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	metricType := "compute.googleapis.com/instance/cpu/utilization"
	fq.metricKinds[metricType] = "GAUGE"
	fq.valueTypes[metricType] = "DOUBLE"
	fq.aggregatedQueryFn = func(params gcpdata.QueryTimeSeriesParams, _ metrics.AggregationSpec) ([]gcpdata.MetricTimeSeries, gcpdata.AggregationWarnings, error) {
		series := []gcpdata.MetricTimeSeries{
			makeTimeSeries(params.Start, stableValues(60, 0.50)),
		}
		if params.End.Before(time.Now().UTC().Add(-30 * time.Minute)) {
			return series, gcpdata.AggregationWarnings{TruncatedSeries: true}, nil
		}
		return series, gcpdata.AggregationWarnings{}, nil
	}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsSnapshot(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_snapshot", map[string]any{
		"metric_type": metricType,
	})
	require.NoError(t, err)
	var snap MetricSnapshotResult
	parseResult(t, result, &snap)
	if !strings.Contains(snap.Note, "baseline (prev_window)") {
		assert.Contains(t, snap.Note, "baseline")
	}
	if !strings.Contains(snap.Note, "truncated") {
		assert.Contains(t, snap.Note, "truncation")
	}
}

func TestCompareTruncationWarningSurfacedInNote(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	metricType := "compute.googleapis.com/instance/cpu/utilization"
	fq.metricKinds[metricType] = "GAUGE"
	fq.valueTypes[metricType] = "DOUBLE"
	fq.series[metricType] = []gcpdata.MetricTimeSeries{
		makeTimeSeries(time.Now().UTC().Add(-2*time.Hour), stableValues(60, 0.50)),
	}
	fq.aggregatedWarnings = gcpdata.AggregationWarnings{TruncatedSeries: true}

	now := time.Now().UTC().Truncate(time.Second)
	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsCompare(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_compare", map[string]any{
		"metric_type":   metricType,
		"window_a_from": now.Add(-2 * time.Hour).Format(time.RFC3339),
		"window_a_to":   now.Add(-1 * time.Hour).Format(time.RFC3339),
		"window_b_from": now.Add(-1 * time.Hour).Format(time.RFC3339),
		"window_b_to":   now.Format(time.RFC3339),
	})
	require.NoError(t, err)
	var cmp CompareResult
	parseResult(t, result, &cmp)
	if !strings.Contains(cmp.Note, "truncated") {
		assert.Contains(t, cmp.Note, "truncation")
	}
}

func TestRelatedTruncationWarningSetsPartialAndNote(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	primaryMetric := "compute.googleapis.com/instance/cpu/utilization"
	relatedMetric := "compute.googleapis.com/instance/memory/utilization"
	fq.metricKinds[primaryMetric] = "GAUGE"
	fq.metricKinds[relatedMetric] = "GAUGE"
	fq.valueTypes[relatedMetric] = "DOUBLE"
	fq.series[relatedMetric] = []gcpdata.MetricTimeSeries{
		makeTimeSeries(time.Now().UTC().Add(-time.Hour), stableValues(60, 0.50)),
	}
	fq.aggregatedWarnings = gcpdata.AggregationWarnings{TruncatedSeries: true}

	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsRelated(fq, reg, "test-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_related", map[string]any{
		"metric_type": primaryMetric,
	})
	require.NoError(t, err)
	var rel RelatedSignalsResult
	parseResult(t, result, &rel)
	if !rel.Partial {
		t.Fatal("Partial = false, want true when truncation warning is present")
	}
	if !strings.Contains(rel.Note, "truncated") {
		assert.Contains(t, rel.Note, "truncation")
	}
}

type testError struct{ msg string }

func (e testError) Error() string { return e.msg }

func errForTest(msg string) error { return testError{msg: msg} }
