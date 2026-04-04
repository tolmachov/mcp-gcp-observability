package gcpdata

import (
	"context"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
)

// MetricsQuerier abstracts Cloud Monitoring query operations
// so that tool handlers can be tested without a real GCP client.
type MetricsQuerier interface {
	GetMetricKind(ctx context.Context, project, metricType string) (string, error)
	ListMetricDescriptors(ctx context.Context, project, filter string, limit int) ([]MetricDescriptorInfo, error)
	QueryTimeSeries(ctx context.Context, params QueryTimeSeriesParams) ([]MetricTimeSeries, error)
}

// MonitoringQuerier implements MetricsQuerier using a real Cloud Monitoring client.
type MonitoringQuerier struct {
	client *monitoring.MetricClient
}

// NewMonitoringQuerier wraps a Cloud Monitoring client as a MetricsQuerier.
func NewMonitoringQuerier(client *monitoring.MetricClient) *MonitoringQuerier {
	if client == nil {
		panic("NewMonitoringQuerier: client must not be nil")
	}
	return &MonitoringQuerier{client: client}
}

func (q *MonitoringQuerier) GetMetricKind(ctx context.Context, project, metricType string) (string, error) {
	return GetMetricKind(ctx, q.client, project, metricType)
}

func (q *MonitoringQuerier) ListMetricDescriptors(ctx context.Context, project, filter string, limit int) ([]MetricDescriptorInfo, error) {
	return ListMetricDescriptors(ctx, q.client, project, filter, limit)
}

func (q *MonitoringQuerier) QueryTimeSeries(ctx context.Context, params QueryTimeSeriesParams) ([]MetricTimeSeries, error) {
	return QueryTimeSeries(ctx, q.client, params)
}
