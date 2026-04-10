package gcpdata

import (
	"testing"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/stretchr/testify/assert"
	"google.golang.org/genproto/googleapis/api/distribution"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestBuildAggregation(t *testing.T) {
	tests := []struct {
		name        string
		metricKind  string
		valueType   string
		wantAligner monitoringpb.Aggregation_Aligner
	}{
		// Numeric cases keep the classic kind-based behavior.
		{"gauge int uses ALIGN_MEAN", "GAUGE", "INT64", monitoringpb.Aggregation_ALIGN_MEAN},
		{"gauge double uses ALIGN_MEAN", "GAUGE", "DOUBLE", monitoringpb.Aggregation_ALIGN_MEAN},
		{"delta int uses ALIGN_RATE", "DELTA", "INT64", monitoringpb.Aggregation_ALIGN_RATE},
		{"cumulative int uses ALIGN_RATE", "CUMULATIVE", "INT64", monitoringpb.Aggregation_ALIGN_RATE},
		{"empty kind defaults to ALIGN_MEAN", "", "INT64", monitoringpb.Aggregation_ALIGN_MEAN},

		// Distribution aligners are kind-aware. Cloud Monitoring rejects
		// BOTH ALIGN_RATE and ALIGN_MEAN for DELTA/CUMULATIVE + DISTRIBUTION
		// (latency histograms like pubsub ack_latencies), so we use
		// ALIGN_DELTA there and ALIGN_MEAN only for GAUGE + DISTRIBUTION.
		// Regression guard: an earlier fix picked ALIGN_MEAN for all
		// distributions and traded one API error for another.
		{"delta distribution uses ALIGN_DELTA", "DELTA", "DISTRIBUTION", monitoringpb.Aggregation_ALIGN_DELTA},
		{"cumulative distribution uses ALIGN_DELTA", "CUMULATIVE", "DISTRIBUTION", monitoringpb.Aggregation_ALIGN_DELTA},
		{"gauge distribution uses ALIGN_MEAN", "GAUGE", "DISTRIBUTION", monitoringpb.Aggregation_ALIGN_MEAN},

		// Empty valueType fallback contract — pin the documented "callers
		// SHOULD set ValueType, but if absent we fall back to the kind-only
		// choice" behavior. If this ever changes, the production impact is
		// significant (distribution metrics will crash in-flight) so the
		// fallback is deliberately locked in here.
		{"fallback: empty valueType + delta uses ALIGN_RATE", "DELTA", "", monitoringpb.Aggregation_ALIGN_RATE},
		{"fallback: empty valueType + gauge uses ALIGN_MEAN", "GAUGE", "", monitoringpb.Aggregation_ALIGN_MEAN},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agg := buildAggregation(tt.metricKind, tt.valueType, 60, nil, monitoringpb.Aggregation_REDUCE_NONE)
			assert.Equal(t, tt.wantAligner, agg.PerSeriesAligner)
		})
	}
}

func TestBuildAggregationWithGroupBy(t *testing.T) {
	agg := buildAggregation("GAUGE", "DOUBLE", 60, []string{"metric.labels.code"}, monitoringpb.Aggregation_REDUCE_MEAN)
	assert.Equal(t, monitoringpb.Aggregation_REDUCE_MEAN, agg.CrossSeriesReducer)
	assert.Equal(t, []string{"metric.labels.code"}, agg.GroupByFields)
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
			// Count must be non-zero: a zero-Count distribution is either nil or
			// has no samples, making Mean a proto default of 0 regardless of what
			// the sender set. Count=3 produces a valid distribution.
			name: "distribution with mean",
			value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DistributionValue{
				DistributionValue: &distribution.Distribution{Mean: 1.5, Count: 3},
			}},
			wantVal: 1.5,
			wantOk:  true,
		},
		{
			// Zero-sample distribution: Count==0 means no samples were recorded.
			// Mean is indistinguishable from the proto default of 0, so we treat
			// this as unsupported to avoid pulling down aggregate statistics with
			// spurious zero data points.
			name: "zero-sample distribution",
			value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DistributionValue{
				DistributionValue: &distribution.Distribution{Count: 0},
			}},
			wantVal: 0,
			wantOk:  false,
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
			assert.Equal(t, tt.wantOk, ok)
			assert.Equal(t, tt.wantVal, val)
		})
	}
}

func TestFlattenStructValue(t *testing.T) {
	tests := []struct {
		name    string
		value   *structpb.Value
		wantStr string
		wantOk  bool
	}{
		{"nil value", nil, "", false},
		{"string", structpb.NewStringValue("e2-medium"), "e2-medium", true},
		// Regression guard: a legitimately empty string must come through as
		// ("", true), not be skipped. Previously the code used s != "" as the
		// string test, routing empty strings into a fallback that could emit
		// "<nil>" for null protobuf values.
		{"empty string", structpb.NewStringValue(""), "", true},
		{"number int-valued", structpb.NewNumberValue(42), "42", true},
		{"number fractional", structpb.NewNumberValue(1.5), "1.5", true},
		{"bool true", structpb.NewBoolValue(true), "true", true},
		{"bool false", structpb.NewBoolValue(false), "false", true},
		// Regression guard: null values must be dropped, not flattened to
		// "<nil>" via fmt.Sprint(interface{}).
		{"null value", structpb.NewNullValue(), "", false},
		{"struct value dropped", structpb.NewStructValue(&structpb.Struct{}), "", false},
		{"list value dropped", structpb.NewListValue(&structpb.ListValue{}), "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := flattenStructValue(tt.value)
			assert.Equal(t, tt.wantOk, ok)
			assert.Equal(t, tt.wantStr, got)
		})
	}
}
