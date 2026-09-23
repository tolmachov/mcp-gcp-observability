package gcpdata

import (
	"context"
	"fmt"
	"slices"
	"sync"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"golang.org/x/sync/singleflight"

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
	// QueryTimeSeries runs a raw query. The warnings carry truncation and
	// dropped-point counts that callers should forward to the client.
	QueryTimeSeries(ctx context.Context, params QueryTimeSeriesParams) ([]MetricTimeSeries, QueryWarnings, error)
	// QueryTimeSeriesAggregated runs a query with a high-level
	// AggregationSpec. See QueryTimeSeriesAggregated in metrics.go for the
	// single-stage vs two-stage semantics. snapshot/compare/related should
	// prefer this over QueryTimeSeries to honor per-metric aggregation
	// declared in the registry instead of silently falling back to mean.
	// The warnings additionally cover the fold (single-group collapse,
	// ragged buckets).
	QueryTimeSeriesAggregated(ctx context.Context, params QueryTimeSeriesParams, spec metrics.AggregationSpec) ([]MetricTimeSeries, QueryWarnings, error)

	// GetResourceLabels returns the label keys defined for a monitored
	// resource type (e.g. "pubsub_subscription" → ["project_id",
	// "subscription_id"]). Returns (nil, nil) if the type is not known to
	// the API — callers should treat "unknown" as "no hint available", not
	// as an error.
	GetResourceLabels(ctx context.Context, project, resourceType string) ([]string, error)
}

// MonitoringQuerier implements MetricsQuerier using a real Cloud Monitoring client.
type MonitoringQuerier struct {
	client *monitoring.MetricClient

	// listDescriptors fetches monitored resource descriptors. Overridable for tests.
	listDescriptors func(ctx context.Context, client *monitoring.MetricClient, project string) ([]MonitoredResourceDescriptor, error)
	// resourceLabels caches the listDescriptors result; shared process-wide
	// by NewMonitoringQuerier, overridable for tests.
	resourceLabels *resourceLabelsCache
}

// NewMonitoringQuerier wraps a Cloud Monitoring client as a MetricsQuerier.
func NewMonitoringQuerier(client *monitoring.MetricClient) *MonitoringQuerier {
	if client == nil {
		panic("NewMonitoringQuerier: client must not be nil")
	}
	return &MonitoringQuerier{
		client:          client,
		listDescriptors: listMonitoredResourceDescriptors,
		resourceLabels:  sharedResourceLabels,
	}
}

// GetResourceLabels returns label keys for a monitored resource type, or
// (nil, nil) if the type is not found. Returns a defensive copy.
func (q *MonitoringQuerier) GetResourceLabels(ctx context.Context, project, resourceType string) ([]string, error) {
	byType, err := q.resourceLabels.get(ctx, project, func(ctx context.Context) ([]MonitoredResourceDescriptor, error) {
		return q.listDescriptors(ctx, q.client, project)
	})
	if err != nil {
		return nil, err
	}
	labels, ok := byType[resourceType]
	if !ok {
		return nil, nil
	}
	return slices.Clone(labels), nil
}

// sharedResourceLabels is the process-wide resource-label cache used by every
// MonitoringQuerier, including the per-user ones of the shared deployment.
// Monitored-resource schemas are global to Cloud Monitoring, so sharing them
// across users leaks nothing; entries are still keyed by project because the
// listing RPC is project-scoped and may fail for one project but not another.
var sharedResourceLabels = newResourceLabelsCache()

// resourceLabelsCache maps project → monitored resource type → label keys.
// Each project is fetched once: concurrent misses share one RPC through the
// singleflight group, the RPC runs without holding mu, and a failed fetch is
// not cached so the next call retries.
type resourceLabelsCache struct {
	mu        sync.Mutex
	byProject map[string]map[string][]string
	fetches   singleflight.Group
}

func newResourceLabelsCache() *resourceLabelsCache {
	return &resourceLabelsCache{byProject: make(map[string]map[string][]string)}
}

func (c *resourceLabelsCache) get(ctx context.Context, project string, fetch func(context.Context) ([]MonitoredResourceDescriptor, error)) (map[string][]string, error) {
	c.mu.Lock()
	byType, ok := c.byProject[project]
	c.mu.Unlock()
	if ok {
		return byType, nil
	}
	v, err, _ := c.fetches.Do(project, func() (any, error) {
		descs, err := fetch(ctx)
		if err != nil {
			return nil, err
		}
		byType := make(map[string][]string, len(descs))
		for _, d := range descs {
			byType[d.Type] = d.Labels
		}
		c.mu.Lock()
		c.byProject[project] = byType
		c.mu.Unlock()
		return byType, nil
	})
	if err != nil {
		return nil, fmt.Errorf("resource labels for project %q: %w", project, err)
	}
	return v.(map[string][]string), nil
}
