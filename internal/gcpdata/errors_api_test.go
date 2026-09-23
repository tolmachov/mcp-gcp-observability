package gcpdata

import (
	"context"
	"testing"
	"time"

	errorreporting "cloud.google.com/go/errorreporting/apiv1beta1"
	"cloud.google.com/go/errorreporting/apiv1beta1/errorreportingpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type captureErrorStatsServer struct {
	errorreportingpb.UnimplementedErrorStatsServiceServer
	last  *errorreportingpb.ListGroupStatsRequest
	begin time.Time
}

func (s *captureErrorStatsServer) ListGroupStats(_ context.Context, req *errorreportingpb.ListGroupStatsRequest) (*errorreportingpb.ListGroupStatsResponse, error) {
	s.last = req
	return &errorreportingpb.ListGroupStatsResponse{TimeRangeBegin: timestamppb.New(s.begin)}, nil
}

func newCaptureErrorStatsQuerier(t *testing.T) (*ErrorReportingQuerier, *captureErrorStatsServer) {
	t.Helper()
	service := &captureErrorStatsServer{begin: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)}
	conn := bufconnClientConn(t, func(s *grpc.Server) { errorreportingpb.RegisterErrorStatsServiceServer(s, service) })
	client, err := errorreporting.NewErrorStatsClient(context.Background(), option.WithGRPCConn(conn))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return NewErrorReportingQuerier(client), service
}

func TestErrorWindowsReachAPIExactly(t *testing.T) {
	q, service := newCaptureErrorStatsQuerier(t)
	for _, window := range ErrorWindows() {
		t.Run(string(window), func(t *testing.T) {
			spec, ok := window.Spec()
			require.True(t, ok)

			listed, err := q.ListErrors(context.Background(), "sample-project", window, 5, "", "")
			require.NoError(t, err)
			require.NotNil(t, service.last)
			assert.Equal(t, spec.Period, service.last.GetTimeRange().GetPeriod())
			assert.Nil(t, service.last.TimedCountDuration)
			assert.Equal(t, window, listed.Window)
			listedBegin, err := time.Parse(time.RFC3339Nano, listed.TimeRangeBegin)
			require.NoError(t, err)
			assert.Equal(t, service.begin, listedBegin)

			trends, err := q.AnalyzeErrorTrends(context.Background(), "sample-project", window, 5, "", "")
			require.NoError(t, err)
			assert.Equal(t, spec.Period, service.last.GetTimeRange().GetPeriod())
			require.NotNil(t, service.last.TimedCountDuration)
			assert.Equal(t, spec.Bucket, service.last.TimedCountDuration.AsDuration())
			assert.Equal(t, window, trends.Window)
			trendsBegin, err := time.Parse(time.RFC3339Nano, trends.TimeRangeBegin)
			require.NoError(t, err)
			assert.Equal(t, service.begin, trendsBegin)
		})
	}
}
