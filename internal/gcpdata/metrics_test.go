package gcpdata

import (
	"testing"

	monitoringpb "cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/genproto/googleapis/api/distribution"
)

func TestBuildAggregation(t *testing.T) {
	tests := []struct {
		name         string
		metricKind   string
		wantAligner  monitoringpb.Aggregation_Aligner
	}{
		{"gauge uses ALIGN_MEAN", "GAUGE", monitoringpb.Aggregation_ALIGN_MEAN},
		{"delta uses ALIGN_RATE", "DELTA", monitoringpb.Aggregation_ALIGN_RATE},
		{"cumulative uses ALIGN_RATE", "CUMULATIVE", monitoringpb.Aggregation_ALIGN_RATE},
		{"empty string defaults to ALIGN_MEAN", "", monitoringpb.Aggregation_ALIGN_MEAN},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agg := buildAggregation(tt.metricKind, 60, nil, monitoringpb.Aggregation_REDUCE_NONE)
			if agg.PerSeriesAligner != tt.wantAligner {
				t.Errorf("aligner = %v, want %v", agg.PerSeriesAligner, tt.wantAligner)
			}
		})
	}
}

func TestBuildAggregationWithGroupBy(t *testing.T) {
	agg := buildAggregation("GAUGE", 60, []string{"metric.labels.code"}, monitoringpb.Aggregation_REDUCE_MEAN)
	if agg.CrossSeriesReducer != monitoringpb.Aggregation_REDUCE_MEAN {
		t.Errorf("reducer = %v, want REDUCE_MEAN", agg.CrossSeriesReducer)
	}
	if len(agg.GroupByFields) != 1 || agg.GroupByFields[0] != "metric.labels.code" {
		t.Errorf("group_by = %v, want [metric.labels.code]", agg.GroupByFields)
	}
}

func TestExtractValue(t *testing.T) {
	tests := []struct {
		name    string
		value   *monitoringpb.TypedValue
		wantVal float64
		wantOk  bool
	}{
		{
			name:    "int64",
			value:   &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: 42}},
			wantVal: 42,
			wantOk:  true,
		},
		{
			name:    "double",
			value:   &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DoubleValue{DoubleValue: 3.14}},
			wantVal: 3.14,
			wantOk:  true,
		},
		{
			name: "distribution with mean",
			value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DistributionValue{
				DistributionValue: &distribution.Distribution{Mean: 1.5},
			}},
			wantVal: 1.5,
			wantOk:  true,
		},
		{
			name:    "nil distribution",
			value:   &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DistributionValue{}},
			wantVal: 0,
			wantOk:  false,
		},
		{
			name:    "bool value (unsupported)",
			value:   &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_BoolValue{BoolValue: true}},
			wantVal: 0,
			wantOk:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			val, ok := extractValue(tt.value)
			if ok != tt.wantOk {
				t.Errorf("ok = %v, want %v", ok, tt.wantOk)
			}
			if val != tt.wantVal {
				t.Errorf("val = %v, want %v", val, tt.wantVal)
			}
		})
	}
}
