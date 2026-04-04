package gcpdata

import (
	"context"
	"errors"
	"fmt"
	"time"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// MetricDescriptorInfo describes a metric type discovered from Cloud Monitoring.
type MetricDescriptorInfo struct {
	Type        string            `json:"type"`
	DisplayName string            `json:"display_name"`
	Description string            `json:"description,omitempty"`
	MetricKind  string            `json:"metric_kind"`
	ValueType   string            `json:"value_type"`
	Unit        string            `json:"unit,omitempty"`
	Labels      []LabelDescriptor `json:"labels,omitempty"`
}

// LabelDescriptor describes a single label on a metric.
type LabelDescriptor struct {
	Key         string `json:"key"`
	Description string `json:"description,omitempty"`
}

// metricsQueryTimeout is the maximum time for a single Cloud Monitoring query.
const metricsQueryTimeout = 30 * time.Second

// MaxTimeSeries is the maximum number of time series returned by QueryTimeSeries
// to prevent runaway memory usage on high-cardinality queries.
const MaxTimeSeries = 500

// MetricTimeSeries holds a single time series with its label sets and points.
type MetricTimeSeries struct {
	MetricLabels   map[string]string `json:"metric_labels,omitempty"`
	ResourceLabels map[string]string `json:"resource_labels,omitempty"`
	MetricKind     string            `json:"metric_kind"`
	ValueType      string            `json:"value_type"`
	Points         []metrics.Point   `json:"points"`
	Truncated      bool              `json:"truncated,omitempty"`
}

// GetMetricKind returns the GCP metric kind (GAUGE, DELTA, CUMULATIVE) for a metric type.
// This is used to select the correct aligner before querying time series.
func GetMetricKind(ctx context.Context, client *monitoring.MetricClient, project, metricType string) (string, error) {
	filter := fmt.Sprintf(`metric.type = "%s"`, EscapeFilterValue(metricType))
	descriptors, err := ListMetricDescriptors(ctx, client, project, filter, 1)
	if err != nil {
		return "", err
	}
	if len(descriptors) == 0 {
		return "", fmt.Errorf("metric descriptor not found for %q in project %q", metricType, project)
	}
	return descriptors[0].MetricKind, nil
}

// ListMetricDescriptors returns metric descriptors matching the filter.
func ListMetricDescriptors(ctx context.Context, client *monitoring.MetricClient, project, filter string, limit int) ([]MetricDescriptorInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, metricsQueryTimeout)
	defer cancel()

	req := &monitoringpb.ListMetricDescriptorsRequest{
		Name:   fmt.Sprintf("projects/%s", project),
		Filter: filter,
	}

	var result []MetricDescriptorInfo
	it := client.ListMetricDescriptors(ctx, req)
	for i := 0; limit <= 0 || i < limit; i++ {
		desc, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("listing metric descriptors: %w", err)
		}
		info := MetricDescriptorInfo{
			Type:        desc.Type,
			DisplayName: desc.DisplayName,
			Description: desc.Description,
			MetricKind:  desc.MetricKind.String(),
			ValueType:   desc.ValueType.String(),
			Unit:        desc.Unit,
		}
		for _, l := range desc.Labels {
			info.Labels = append(info.Labels, LabelDescriptor{
				Key:         l.Key,
				Description: l.Description,
			})
		}
		result = append(result, info)
	}
	return result, nil
}

// QueryTimeSeriesParams configures a time series query.
type QueryTimeSeriesParams struct {
	Project       string
	MetricType    string
	LabelFilter   string
	Start         time.Time
	End           time.Time
	StepSeconds   int64
	MetricKind    string
	GroupByFields []string
	Reducer       monitoringpb.Aggregation_Reducer
}

// QueryTimeSeries fetches time series data from Cloud Monitoring.
// Returns at most MaxTimeSeries series. If the result is truncated,
// the last element has Truncated=true.
func QueryTimeSeries(ctx context.Context, client *monitoring.MetricClient, params QueryTimeSeriesParams) ([]MetricTimeSeries, error) {
	ctx, cancel := context.WithTimeout(ctx, metricsQueryTimeout)
	defer cancel()

	filter := fmt.Sprintf(`metric.type = "%s"`, EscapeFilterValue(params.MetricType))
	if params.LabelFilter != "" {
		filter += " AND " + params.LabelFilter
	}

	stepSeconds := params.StepSeconds
	if stepSeconds <= 0 {
		stepSeconds = 60
	}

	agg := buildAggregation(params.MetricKind, stepSeconds, params.GroupByFields, params.Reducer)

	req := &monitoringpb.ListTimeSeriesRequest{
		Name:   fmt.Sprintf("projects/%s", params.Project),
		Filter: filter,
		Interval: &monitoringpb.TimeInterval{
			StartTime: timestamppb.New(params.Start),
			EndTime:   timestamppb.New(params.End),
		},
		Aggregation: agg,
		View:        monitoringpb.ListTimeSeriesRequest_FULL,
	}

	var result []MetricTimeSeries
	it := client.ListTimeSeries(ctx, req)
	for {
		ts, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("listing time series: %w", err)
		}

		mts := MetricTimeSeries{
			MetricKind: ts.MetricKind.String(),
			ValueType:  ts.ValueType.String(),
		}
		if ts.Metric != nil {
			mts.MetricLabels = ts.Metric.Labels
		}
		if ts.Resource != nil {
			mts.ResourceLabels = ts.Resource.Labels
		}

		var unsupportedCount int
		for _, p := range ts.Points {
			if p.Interval == nil || p.Interval.EndTime == nil || p.Value == nil {
				continue
			}
			val, ok := extractValue(p.Value)
			if !ok {
				unsupportedCount++
				continue
			}
			mts.Points = append(mts.Points, metrics.Point{
				Timestamp: p.Interval.EndTime.AsTime(),
				Value:     val,
			})
		}
		if len(mts.Points) == 0 && unsupportedCount > 0 {
			return nil, fmt.Errorf("metric %q has %d points with unsupported value type (only int64, double, and distribution are supported)", params.MetricType, unsupportedCount)
		}

		result = append(result, mts)

		if len(result) >= MaxTimeSeries {
			result[len(result)-1].Truncated = true
			break
		}
	}
	return result, nil
}

func buildAggregation(metricKind string, stepSeconds int64, groupByFields []string, reducer monitoringpb.Aggregation_Reducer) *monitoringpb.Aggregation {
	agg := &monitoringpb.Aggregation{
		AlignmentPeriod: &durationpb.Duration{Seconds: stepSeconds},
	}

	switch metricKind {
	case "DELTA", "CUMULATIVE":
		agg.PerSeriesAligner = monitoringpb.Aggregation_ALIGN_RATE
	default:
		agg.PerSeriesAligner = monitoringpb.Aggregation_ALIGN_MEAN
	}

	if len(groupByFields) > 0 {
		agg.GroupByFields = groupByFields
		agg.CrossSeriesReducer = reducer
	}

	return agg
}

func extractValue(tv *monitoringpb.TypedValue) (float64, bool) {
	switch v := tv.Value.(type) {
	case *monitoringpb.TypedValue_Int64Value:
		return float64(v.Int64Value), true
	case *monitoringpb.TypedValue_DoubleValue:
		return v.DoubleValue, true
	case *monitoringpb.TypedValue_DistributionValue:
		if v.DistributionValue != nil {
			return v.DistributionValue.Mean, true
		}
		return 0, false
	default:
		return 0, false
	}
}
