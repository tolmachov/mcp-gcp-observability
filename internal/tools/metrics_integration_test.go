package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// --- fake querier ---

type fakeQuerier struct {
	descriptors []gcpdata.MetricDescriptorInfo
	series      map[string][]gcpdata.MetricTimeSeries // key: metricType
	seriesFunc  func(params gcpdata.QueryTimeSeriesParams) []gcpdata.MetricTimeSeries
	metricKinds map[string]string // metricType → GAUGE/DELTA/etc.
	queryLog    []gcpdata.QueryTimeSeriesParams

	getMetricKindErr         error
	listMetricDescriptorsErr error
	queryTimeSeriesErr       error
}

func newFakeQuerier() *fakeQuerier {
	return &fakeQuerier{
		series:      make(map[string][]gcpdata.MetricTimeSeries),
		metricKinds: make(map[string]string),
	}
}

func (f *fakeQuerier) GetMetricKind(_ context.Context, _, metricType string) (string, error) {
	if f.getMetricKindErr != nil {
		return "", f.getMetricKindErr
	}
	return f.metricKinds[metricType], nil
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
	f.queryLog = append(f.queryLog, params)
	if f.queryTimeSeriesErr != nil {
		return nil, f.queryTimeSeriesErr
	}
	if f.seriesFunc != nil {
		return f.seriesFunc(params), nil
	}
	return f.series[params.MetricType], nil
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
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("writing registry: %v", err)
	}
	reg, err := metrics.LoadRegistry(path)
	if err != nil {
		t.Fatalf("loading registry: %v", err)
	}
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
	if result == nil {
		t.Fatal("nil result")
	}
	if result.IsError {
		for _, c := range result.Content {
			if tc, ok := c.(mcp.TextContent); ok {
				t.Fatalf("tool returned error: %s", tc.Text)
			}
		}
		t.Fatal("tool returned error with no text")
	}
	for _, c := range result.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			if err := json.Unmarshal([]byte(tc.Text), target); err != nil {
				t.Fatalf("unmarshaling result: %v\ntext: %s", err, tc.Text)
			}
			return
		}
	}
	t.Fatal("no text content in result")
}

func expectError(t *testing.T, result *mcp.CallToolResult, contains string) {
	t.Helper()
	if result == nil {
		t.Fatal("nil result")
	}
	if !result.IsError {
		t.Fatal("expected error result, got success")
	}
	for _, c := range result.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			if !strings.Contains(tc.Text, contains) {
				t.Errorf("error %q does not contain %q", tc.Text, contains)
			}
			return
		}
	}
}

// --- stable data: 60 points of ~0.50 with small variance ---
func stableValues(n int, base float64) []float64 {
	vals := make([]float64, n)
	for i := range vals {
		// deterministic small variation
		vals[i] = base + float64(i%3)*0.01 - 0.01
	}
	return vals
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

	h := NewMetricsSnapshotHandler(fq, reg, "test-project")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{
		"metric_type": "compute.googleapis.com/instance/cpu/utilization",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var snap MetricSnapshotResult
	parseResult(t, result, &snap)

	if snap.MetricType != "compute.googleapis.com/instance/cpu/utilization" {
		t.Errorf("metric_type = %q", snap.MetricType)
	}
	if snap.Kind != "resource_utilization" {
		t.Errorf("kind = %q, want resource_utilization", snap.Kind)
	}
	if snap.Classification != string(metrics.ClassStable) {
		t.Errorf("classification = %q, want stable", snap.Classification)
	}
	if snap.SLOBreach {
		t.Error("unexpected SLO breach for value ~0.50 with threshold 0.8")
	}
	if snap.BaselineMode != "prev_window" {
		t.Errorf("baseline_mode = %q, want prev_window", snap.BaselineMode)
	}
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

	h := NewMetricsSnapshotHandler(fq, reg, "test-project")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{
		"metric_type": "compute.googleapis.com/instance/cpu/utilization",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var snap MetricSnapshotResult
	parseResult(t, result, &snap)

	if !snap.SLOBreach {
		t.Error("expected SLO breach for value ~0.90 with threshold 0.8")
	}
	if snap.SLOThreshold == nil || *snap.SLOThreshold != 0.8 {
		t.Errorf("slo_threshold = %v, want 0.8", snap.SLOThreshold)
	}
	if snap.Classification == string(metrics.ClassStable) {
		t.Error("expected non-stable classification for major delta")
	}
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

	h := NewMetricsSnapshotHandler(fq, reg, "test-project")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{
		"metric_type": "custom.googleapis.com/api/latency",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var snap MetricSnapshotResult
	parseResult(t, result, &snap)

	if snap.Kind != "latency" {
		t.Errorf("kind = %q, want latency", snap.Kind)
	}
	if snap.Percentiles == nil {
		t.Fatal("expected percentiles for latency metric")
	}
	if snap.Percentiles.P99 <= snap.Percentiles.P50 {
		t.Errorf("P99 (%f) should be > P50 (%f) for latency with tail", snap.Percentiles.P99, snap.Percentiles.P50)
	}
}

func TestSnapshotIntegration_NoData(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["compute.googleapis.com/instance/cpu/utilization"] = "GAUGE"
	// No series data → empty result.

	h := NewMetricsSnapshotHandler(fq, reg, "test-project")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{
		"metric_type": "compute.googleapis.com/instance/cpu/utilization",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectError(t, result, "No data found")
}

func TestSnapshotIntegration_MissingMetricType(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()

	h := NewMetricsSnapshotHandler(fq, reg, "test-project")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectError(t, result, "metric_type is required")
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
	h := NewMetricsSnapshotHandler(fq, reg, "test-project")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{
		"metric_type":   "compute.googleapis.com/instance/cpu/utilization",
		"baseline_mode": "pre_event",
		"event_time":    eventTime,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var snap MetricSnapshotResult
	parseResult(t, result, &snap)

	if snap.BaselineMode != "pre_event" {
		t.Errorf("baseline_mode = %q, want pre_event", snap.BaselineMode)
	}
}

func TestSnapshotIntegration_PreEventMissingEventTime(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()

	h := NewMetricsSnapshotHandler(fq, reg, "test-project")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{
		"metric_type":   "compute.googleapis.com/instance/cpu/utilization",
		"baseline_mode": "pre_event",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectError(t, result, "event_time is required")
}

func TestSnapshotIntegration_InvalidWindow(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()

	h := NewMetricsSnapshotHandler(fq, reg, "test-project")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{
		"metric_type": "compute.googleapis.com/instance/cpu/utilization",
		"window":      "99h",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectError(t, result, "invalid window")
}

// --- metrics.list tests ---

func TestListIntegration_RegistryAndAPI(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.descriptors = []gcpdata.MetricDescriptorInfo{
		{Type: "compute.googleapis.com/instance/disk/read_bytes_count", DisplayName: "Disk Read Bytes", MetricKind: "DELTA", ValueType: "INT64", Unit: "By"},
		{Type: "compute.googleapis.com/instance/cpu/utilization", DisplayName: "CPU Utilization", MetricKind: "GAUGE", ValueType: "DOUBLE", Unit: "ratio"},
	}

	h := NewMetricsListHandler(fq, reg, "test-project")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

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
				t.Errorf("cpu util kind = %q, want resource_utilization", m.Kind)
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

	h := NewMetricsListHandler(fq, reg, "test-project")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{
		"match": "cpu",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var list MetricsListResult
	parseResult(t, result, &list)

	for _, m := range list.Metrics {
		if !strings.Contains(strings.ToLower(m.MetricType), "cpu") {
			t.Errorf("metric %q does not contain 'cpu'", m.MetricType)
		}
	}
}

func TestListIntegration_FilterByKind(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()

	h := NewMetricsListHandler(fq, reg, "test-project")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{
		"kind": "latency",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var list MetricsListResult
	parseResult(t, result, &list)

	for _, m := range list.Metrics {
		if m.Kind != "latency" {
			t.Errorf("metric %q has kind %q, want latency", m.MetricType, m.Kind)
		}
	}
}

func TestListIntegration_InvalidKind(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()

	h := NewMetricsListHandler(fq, reg, "test-project")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{
		"kind": "bogus",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectError(t, result, "invalid kind")
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

	h := NewMetricsListHandler(fq, reg, "test-project")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{
		"limit": 5.0,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var list MetricsListResult
	parseResult(t, result, &list)

	if list.Count > 5 {
		t.Errorf("count = %d, want <= 5", list.Count)
	}
}

// --- metrics.top_contributors tests ---

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

	h := NewMetricsTopHandler(fq, reg, "test-project")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{
		"metric_type": "custom.googleapis.com/api/latency",
		"dimension":   "metric.labels.response_code",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var top TopContributorsResult
	parseResult(t, result, &top)

	if top.Dimension != "metric.labels.response_code" {
		t.Errorf("dimension = %q", top.Dimension)
	}
	if len(top.Contributors) == 0 {
		t.Fatal("expected at least one contributor")
	}
}

func TestTopContributorsIntegration_MissingDimension(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()

	h := NewMetricsTopHandler(fq, reg, "test-project")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{
		"metric_type": "custom.googleapis.com/api/latency",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectError(t, result, "dimension is required")
}

func TestTopContributorsIntegration_NoData(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["custom.googleapis.com/api/latency"] = "GAUGE"

	h := NewMetricsTopHandler(fq, reg, "test-project")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{
		"metric_type": "custom.googleapis.com/api/latency",
		"dimension":   "metric.labels.route",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectError(t, result, "No data found")
}

// --- metrics.related tests ---

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

	h := NewMetricsRelatedHandler(fq, reg, "test-project")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{
		"metric_type": "compute.googleapis.com/instance/cpu/utilization",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var related RelatedSignalsResult
	parseResult(t, result, &related)

	if len(related.RelatedSignals) == 0 {
		t.Fatal("expected at least one related signal")
	}
	if related.RelatedSignals[0].MetricType != "compute.googleapis.com/instance/memory/utilization" {
		t.Errorf("related metric = %q", related.RelatedSignals[0].MetricType)
	}
}

func TestRelatedIntegration_NoRelatedConfigured(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()

	// memory/utilization has no related_metrics in registry.
	h := NewMetricsRelatedHandler(fq, reg, "test-project")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{
		"metric_type": "compute.googleapis.com/instance/memory/utilization",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

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

	h := NewMetricsRelatedHandler(fq, reg, "test-project")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{
		"metric_type": "compute.googleapis.com/instance/cpu/utilization",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var related RelatedSignalsResult
	parseResult(t, result, &related)

	if len(related.Skipped) == 0 {
		t.Error("expected skipped signal for metric with no data")
	}
}

// --- metrics.compare tests ---

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

	h := NewMetricsCompareHandler(fq, reg, "test-project")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{
		"metric_type":  "compute.googleapis.com/instance/cpu/utilization",
		"window_a_from": aFrom.Format(time.RFC3339),
		"window_a_to":   aTo.Format(time.RFC3339),
		"window_b_from": bFrom.Format(time.RFC3339),
		"window_b_to":   bTo.Format(time.RFC3339),
		"window_a_label": "before",
		"window_b_label": "after",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var cmp CompareResult
	parseResult(t, result, &cmp)

	if cmp.WindowALabel != "before" {
		t.Errorf("window_a_label = %q, want before", cmp.WindowALabel)
	}
	if cmp.WindowBLabel != "after" {
		t.Errorf("window_b_label = %q, want after", cmp.WindowBLabel)
	}
	if cmp.TrendShift != "unchanged" {
		t.Errorf("trend_shift = %q, want unchanged for similar values", cmp.TrendShift)
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
	aTo := now.Add(-time.Hour)
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

	h := NewMetricsCompareHandler(fq, reg, "test-project")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{
		"metric_type":   "compute.googleapis.com/instance/cpu/utilization",
		"window_a_from": aFrom.Format(time.RFC3339),
		"window_a_to":   aTo.Format(time.RFC3339),
		"window_b_from": bFrom.Format(time.RFC3339),
		"window_b_to":   bTo.Format(time.RFC3339),
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

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
	h := NewMetricsCompareHandler(fq, reg, "test-project")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{
		"metric_type":   "compute.googleapis.com/instance/cpu/utilization",
		"window_a_from": now.Format(time.RFC3339),
		"window_a_to":   now.Add(-time.Hour).Format(time.RFC3339),
		"window_b_from": now.Add(-2 * time.Hour).Format(time.RFC3339),
		"window_b_to":   now.Add(-time.Hour).Format(time.RFC3339),
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectError(t, result, "must be after")
}

func TestCompareIntegration_MissingRequiredFields(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()

	h := NewMetricsCompareHandler(fq, reg, "test-project")

	// Missing metric_type.
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{
		"window_a_from": "2025-01-01T00:00:00Z",
		"window_a_to":   "2025-01-01T01:00:00Z",
		"window_b_from": "2025-01-01T01:00:00Z",
		"window_b_to":   "2025-01-01T02:00:00Z",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectError(t, result, "metric_type is required")

	// Missing window_a_from.
	result, err = h.Handle(context.Background(), makeRequest(map[string]any{
		"metric_type":   "compute.googleapis.com/instance/cpu/utilization",
		"window_a_to":   "2025-01-01T01:00:00Z",
		"window_b_from": "2025-01-01T01:00:00Z",
		"window_b_to":   "2025-01-01T02:00:00Z",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectError(t, result, "window_a_from is required")
}

// --- project resolution tests ---

func TestSnapshotIntegration_UsesDefaultProject(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["compute.googleapis.com/instance/cpu/utilization"] = "GAUGE"
	fq.series["compute.googleapis.com/instance/cpu/utilization"] = []gcpdata.MetricTimeSeries{
		makeTimeSeries(time.Now().UTC().Add(-time.Hour), stableValues(60, 0.50)),
	}

	h := NewMetricsSnapshotHandler(fq, reg, "default-proj")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{
		"metric_type": "compute.googleapis.com/instance/cpu/utilization",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should succeed using default project.
	var snap MetricSnapshotResult
	parseResult(t, result, &snap)

	// Verify the querier received the default project.
	if len(fq.queryLog) == 0 {
		t.Fatal("expected queries to be logged")
	}
	if fq.queryLog[0].Project != "default-proj" {
		t.Errorf("project = %q, want default-proj", fq.queryLog[0].Project)
	}
}

func TestSnapshotIntegration_OverridesProject(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["compute.googleapis.com/instance/cpu/utilization"] = "GAUGE"
	fq.series["compute.googleapis.com/instance/cpu/utilization"] = []gcpdata.MetricTimeSeries{
		makeTimeSeries(time.Now().UTC().Add(-time.Hour), stableValues(60, 0.50)),
	}

	h := NewMetricsSnapshotHandler(fq, reg, "default-proj")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{
		"metric_type": "compute.googleapis.com/instance/cpu/utilization",
		"project_id":  "override-proj",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var snap MetricSnapshotResult
	parseResult(t, result, &snap)

	if len(fq.queryLog) == 0 {
		t.Fatal("expected queries to be logged")
	}
	if fq.queryLog[0].Project != "override-proj" {
		t.Errorf("project = %q, want override-proj", fq.queryLog[0].Project)
	}
}

// --- error propagation tests ---

func TestSnapshotIntegration_MetricKindError(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.getMetricKindErr = errForTest("metric descriptor lookup failed")

	h := NewMetricsSnapshotHandler(fq, reg, "test-project")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{
		"metric_type": "compute.googleapis.com/instance/cpu/utilization",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectError(t, result, "Failed to look up metric descriptor")
}

func TestSnapshotIntegration_QueryError(t *testing.T) {
	reg := loadTestRegistry(t, testRegistryYAML)
	fq := newFakeQuerier()
	fq.metricKinds["compute.googleapis.com/instance/cpu/utilization"] = "GAUGE"
	fq.queryTimeSeriesErr = errForTest("permission denied")

	h := NewMetricsSnapshotHandler(fq, reg, "test-project")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{
		"metric_type": "compute.googleapis.com/instance/cpu/utilization",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectError(t, result, "Failed to query metric")
}

func TestListIntegration_APIError(t *testing.T) {
	reg := metrics.NewRegistry()
	fq := newFakeQuerier()
	fq.listMetricDescriptorsErr = errForTest("quota exceeded")

	h := NewMetricsListHandler(fq, reg, "test-project")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
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

	h := NewMetricsSnapshotHandler(fq, reg, "test-project")
	result, err := h.Handle(context.Background(), makeRequest(map[string]any{
		"metric_type": "custom.googleapis.com/myapp/request_count",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var snap MetricSnapshotResult
	parseResult(t, result, &snap)

	if !snap.AutoDetected {
		t.Error("expected auto_detected = true for unregistered metric")
	}
	if snap.Kind != "throughput" {
		t.Errorf("kind = %q, want throughput (auto-detected from 'request_count')", snap.Kind)
	}
}

type testError struct{ msg string }

func (e testError) Error() string { return e.msg }

func errForTest(msg string) error { return testError{msg: msg} }

