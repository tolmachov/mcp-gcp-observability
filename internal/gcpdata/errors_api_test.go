package gcpdata

import (
	"context"
	"net"
	"testing"
	"time"

	errorreporting "cloud.google.com/go/errorreporting/apiv1beta1"
	"cloud.google.com/go/errorreporting/apiv1beta1/errorreportingpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
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

func newCaptureErrorStatsClient(t *testing.T) (*errorreporting.ErrorStatsClient, *captureErrorStatsServer) {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	service := &captureErrorStatsServer{begin: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)}
	errorreportingpb.RegisterErrorStatsServiceServer(grpcServer, service)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	client, err := errorreporting.NewErrorStatsClient(context.Background(), option.WithGRPCConn(conn))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client, service
}

func TestErrorWindowsReachAPIExactly(t *testing.T) {
	client, service := newCaptureErrorStatsClient(t)
	windows := []ErrorWindow{ErrorWindow1H, ErrorWindow6H, ErrorWindow24H, ErrorWindow7D, ErrorWindow30D}
	for _, window := range windows {
		t.Run(string(window), func(t *testing.T) {
			spec, ok := window.Spec()
			require.True(t, ok)

			listed, err := ListErrors(context.Background(), client, "sample-project", window, 5, "", "")
			require.NoError(t, err)
			require.NotNil(t, service.last)
			assert.Equal(t, spec.Period, service.last.GetTimeRange().GetPeriod())
			assert.Nil(t, service.last.TimedCountDuration)
			assert.Equal(t, window, listed.Window)
			listedBegin, err := time.Parse(time.RFC3339Nano, listed.TimeRangeBegin)
			require.NoError(t, err)
			assert.Equal(t, service.begin, listedBegin)

			trends, err := AnalyzeErrorTrends(context.Background(), client, "sample-project", window, 5, "", "")
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
