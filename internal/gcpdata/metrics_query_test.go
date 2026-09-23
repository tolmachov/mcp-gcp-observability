package gcpdata

import (
	"context"
	"math"
	"net"
	"strconv"
	"testing"
	"time"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// stockMetricServer answers every ListTimeSeries with a fixed set of series in
// one page.
type stockMetricServer struct {
	monitoringpb.UnimplementedMetricServiceServer
	series []*monitoringpb.TimeSeries
}

func (s *stockMetricServer) ListTimeSeries(context.Context, *monitoringpb.ListTimeSeriesRequest) (*monitoringpb.ListTimeSeriesResponse, error) {
	return &monitoringpb.ListTimeSeriesResponse{TimeSeries: s.series}, nil
}

func newStockMonitoringQuerier(t *testing.T, series []*monitoringpb.TimeSeries) *MonitoringQuerier {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	monitoringpb.RegisterMetricServiceServer(grpcServer, &stockMetricServer{series: series})
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	client, err := monitoring.NewMetricClient(context.Background(), option.WithGRPCConn(conn))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return NewMonitoringQuerier(client)
}

var stockPointTime = time.Date(2026, 8, 1, 0, 1, 0, 0, time.UTC)

func stockPoint(v *monitoringpb.TypedValue) *monitoringpb.Point {
	return &monitoringpb.Point{
		Interval: &monitoringpb.TimeInterval{EndTime: timestamppb.New(stockPointTime)},
		Value:    v,
	}
}

func doublePoint(v float64) *monitoringpb.Point {
	return stockPoint(&monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DoubleValue{DoubleValue: v}})
}

// stockSeries returns n single-point gauge series, one per game_id. The first
// also carries a BOOL point (unsupported) and a NaN point (non-finite).
func stockSeries(n int) []*monitoringpb.TimeSeries {
	series := make([]*monitoringpb.TimeSeries, n)
	for i := range series {
		series[i] = &monitoringpb.TimeSeries{
			Metric:     &metric.Metric{Labels: map[string]string{"game_id": strconv.Itoa(i)}},
			MetricKind: metric.MetricDescriptor_GAUGE,
			ValueType:  metric.MetricDescriptor_DOUBLE,
			Points:     []*monitoringpb.Point{doublePoint(1)},
		}
	}
	series[0].Points = append(series[0].Points,
		stockPoint(&monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_BoolValue{BoolValue: true}}),
		doublePoint(math.NaN()),
	)
	return series
}

func stockQueryParams() QueryTimeSeriesParams {
	return QueryTimeSeriesParams{
		Project: "sample-project", MetricType: "custom.googleapis.com/players",
		Start: stockPointTime.Add(-time.Hour), End: stockPointTime,
		StepSeconds: 60, MetricKind: MetricKindGauge, ValueType: "DOUBLE",
	}
}

func TestQueryTimeSeries_Truncation(t *testing.T) {
	t.Run("exactly MaxTimeSeries is complete", func(t *testing.T) {
		q := newStockMonitoringQuerier(t, stockSeries(MaxTimeSeries))
		series, warnings, err := q.QueryTimeSeries(context.Background(), stockQueryParams())
		require.NoError(t, err)
		assert.Len(t, series, MaxTimeSeries)
		assert.False(t, warnings.TruncatedSeries)
		assert.Equal(t, 1, warnings.UnsupportedPoints)
		assert.Equal(t, 1, warnings.NonFinitePoints)
		assert.Len(t, series[0].Points, 1, "only the finite double point survives decoding")
	})

	t.Run("one more than MaxTimeSeries is truncated", func(t *testing.T) {
		q := newStockMonitoringQuerier(t, stockSeries(MaxTimeSeries+1))
		series, warnings, err := q.QueryTimeSeries(context.Background(), stockQueryParams())
		require.NoError(t, err)
		assert.Len(t, series, MaxTimeSeries)
		assert.True(t, warnings.TruncatedSeries)
	})
}

// TestQueryTimeSeriesAggregated_KeepsQueryWarnings pins that the two-stage
// fold carries the first stage's truncation and decoding warnings through.
func TestQueryTimeSeriesAggregated_KeepsQueryWarnings(t *testing.T) {
	q := newStockMonitoringQuerier(t, stockSeries(MaxTimeSeries+1))
	spec := metrics.AggregationSpec{GroupBy: []string{"metric.labels.game_id"}, WithinGroup: metrics.ReducerSum, AcrossGroups: metrics.ReducerSum}

	series, warnings, err := q.QueryTimeSeriesAggregated(context.Background(), stockQueryParams(), spec)
	require.NoError(t, err)
	require.Len(t, series, 1)
	require.Len(t, series[0].Points, 1)
	assert.InDelta(t, float64(MaxTimeSeries), series[0].Points[0].Value, 0)
	assert.True(t, warnings.TruncatedSeries)
	assert.Equal(t, 1, warnings.UnsupportedPoints)
	assert.Equal(t, 1, warnings.NonFinitePoints)
	assert.Equal(t, MaxTimeSeries, warnings.GroupCount)
	assert.Equal(t, 1, warnings.TotalBuckets)
}
