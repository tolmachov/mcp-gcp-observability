package gcpdata

import (
	"context"
	"sync"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"

	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// MetricsQuerier abstracts Cloud Monitoring query operations
// so that tool handlers can be tested without a real GCP client.
type MetricsQuerier interface {
	// GetMetricDescriptor returns the metric kind and value type needed to
	// select the correct per-series aligner when querying time series.
	// Both are required because, for example, ALIGN_RATE cannot be applied
	// to DELTA+DISTRIBUTION metrics (the GCP API rejects it outright).
	GetMetricDescriptor(ctx context.Context, project, metricType string) (MetricDescriptorBasic, error)
	ListMetricDescriptors(ctx context.Context, project, filter string, limit int) ([]MetricDescriptorInfo, error)
	QueryTimeSeries(ctx context.Context, params QueryTimeSeriesParams) ([]MetricTimeSeries, error)
	// QueryTimeSeriesAggregated runs a query with a high-level
	// AggregationSpec. See QueryTimeSeriesAggregated in metrics.go for the
	// single-stage vs two-stage semantics. snapshot/compare/related should
	// prefer this over QueryTimeSeries to honor per-metric aggregation
	// declared in the registry instead of silently falling back to mean.
	// The second return value carries non-fatal warnings (single-group
	// collapse, ragged buckets) that callers should forward to mcpLog.
	QueryTimeSeriesAggregated(ctx context.Context, params QueryTimeSeriesParams, spec metrics.AggregationSpec) ([]MetricTimeSeries, AggregationWarnings, error)

	// GetResourceLabels returns the label keys defined for a monitored
	// resource type (e.g. "pubsub_subscription" → ["project_id",
	// "subscription_id"]). The result is cached process-wide because
	// monitored-resource schemas are global to Cloud Monitoring. Returns
	// (nil, nil) if the type is not known to the API — callers should
	// treat "unknown" as "no hint available", not as an error.
	GetResourceLabels(ctx context.Context, project, resourceType string) ([]string, error)
}

// MonitoringQuerier implements MetricsQuerier using a real Cloud Monitoring client.
type MonitoringQuerier struct {
	client *monitoring.MetricClient

	// listDescriptors fetches monitored resource descriptors. Overridable for tests.
	listDescriptors func(ctx context.Context, client *monitoring.MetricClient, project string) ([]MonitoredResourceDescriptor, error)

	// resourceLabels caches label keys per monitored resource type (lazily populated,
	// O(1) after first call). Global schemas, safe to share across projects.
	// Priming failures not cached — retried on next call.
	resourceLabelsMu sync.Mutex
	resourceLabels   map[string][]string
}

// NewMonitoringQuerier wraps a Cloud Monitoring client as a MetricsQuerier.
func NewMonitoringQuerier(client *monitoring.MetricClient) *MonitoringQuerier {
	if client == nil {
		panic("NewMonitoringQuerier: client must not be nil")
	}
	return &MonitoringQuerier{
		client:          client,
		listDescriptors: ListMonitoredResourceDescriptors,
	}
}

func (q *MonitoringQuerier) GetMetricDescriptor(ctx context.Context, project, metricType string) (MetricDescriptorBasic, error) {
	return GetMetricDescriptor(ctx, q.client, project, metricType)
}

func (q *MonitoringQuerier) ListMetricDescriptors(ctx context.Context, project, filter string, limit int) ([]MetricDescriptorInfo, error) {
	return ListMetricDescriptors(ctx, q.client, project, filter, limit)
}

func (q *MonitoringQuerier) QueryTimeSeries(ctx context.Context, params QueryTimeSeriesParams) ([]MetricTimeSeries, error) {
	return QueryTimeSeries(ctx, q.client, params)
}

func (q *MonitoringQuerier) QueryTimeSeriesAggregated(ctx context.Context, params QueryTimeSeriesParams, spec metrics.AggregationSpec) ([]MetricTimeSeries, AggregationWarnings, error) {
	return QueryTimeSeriesAggregated(ctx, q.client, params, spec)
}

// GetResourceLabels returns label keys for a monitored resource type.
// Primes process-wide cache on first call; (nil, nil) if type not found.
// Returns a defensive copy.
func (q *MonitoringQuerier) GetResourceLabels(ctx context.Context, project, resourceType string) ([]string, error) {
	q.resourceLabelsMu.Lock()
	defer q.resourceLabelsMu.Unlock()

	if q.resourceLabels == nil {
		descs, err := q.listDescriptors(ctx, q.client, project)
		if err != nil {
			return nil, err
		}
		m := make(map[string][]string, len(descs))
		for _, d := range descs {
			m[d.Type] = d.Labels
		}
		q.resourceLabels = m
	}

	labels, ok := q.resourceLabels[resourceType]
	if !ok {
		return nil, nil
	}
	out := make([]string, len(labels))
	copy(out, labels)
	return out, nil
}
