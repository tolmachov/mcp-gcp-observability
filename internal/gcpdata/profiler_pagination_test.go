package gcpdata

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	cloudprofiler "cloud.google.com/go/cloudprofiler/apiv2"
	"cloud.google.com/go/cloudprofiler/apiv2/cloudprofilerpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type pagedProfilerServer struct {
	cloudprofilerpb.UnimplementedExportServiceServer
	seen []string
}

func testProfile(id string, typ cloudprofilerpb.ProfileType, target string, minute int) *cloudprofilerpb.Profile {
	return &cloudprofilerpb.Profile{
		Name: "projects/sample-project/profiles/" + id, ProfileType: typ,
		Deployment: &cloudprofilerpb.Deployment{ProjectId: "sample-project", Target: target},
		StartTime:  timestamppb.New(time.Date(2026, 8, 1, 0, minute, 0, 0, time.UTC)),
	}
}

func (s *pagedProfilerServer) ListProfiles(_ context.Context, req *cloudprofilerpb.ListProfilesRequest) (*cloudprofilerpb.ListProfilesResponse, error) {
	s.seen = append(s.seen, req.PageToken)
	switch req.PageToken {
	case "":
		profiles := []*cloudprofilerpb.Profile{
			testProfile("heap", cloudprofilerpb.ProfileType_HEAP, "svc", 0),
			testProfile("cpu-a", cloudprofilerpb.ProfileType_CPU, "svc", 1),
			testProfile("cpu-b", cloudprofilerpb.ProfileType_CPU, "svc", 2),
		}
		for i := len(profiles); i < 1000; i++ {
			profiles = append(profiles, testProfile("heap-filler", cloudprofilerpb.ProfileType_HEAP, "svc", i%60))
		}
		return &cloudprofilerpb.ListProfilesResponse{Profiles: profiles, NextPageToken: "api-2"}, nil
	case "api-2":
		return &cloudprofilerpb.ListProfilesResponse{Profiles: []*cloudprofilerpb.Profile{
			testProfile("cpu-c", cloudprofilerpb.ProfileType_CPU, "svc", 3),
		}}, nil
	default:
		return &cloudprofilerpb.ListProfilesResponse{}, nil
	}
}

func newPagedProfilerQuerier(t *testing.T) (*CloudProfilerQuerier, *pagedProfilerServer) {
	t.Helper()
	service := &pagedProfilerServer{}
	return newFakeProfilerQuerier(t, service, slog.New(slog.DiscardHandler)), service
}

// newFakeProfilerQuerier serves service over an in-memory gRPC connection and
// returns a CloudProfilerQuerier backed by it.
func newFakeProfilerQuerier(t *testing.T, service cloudprofilerpb.ExportServiceServer, logger *slog.Logger) *CloudProfilerQuerier {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	cloudprofilerpb.RegisterExportServiceServer(grpcServer, service)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(2*maxCompressedProfileBytes)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	client, err := cloudprofiler.NewExportClient(context.Background(), option.WithGRPCConn(conn))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	q := NewCloudProfilerQuerier(client, logger)
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func TestProfilerPaginationResumesInsideAPIPage(t *testing.T) {
	q, service := newPagedProfilerQuerier(t)
	params := ListProfilesParams{Project: "sample-project", ProfileType: "CPU", Target: "svc", PageSize: 1}

	first, err := q.ListProfiles(context.Background(), params)
	require.NoError(t, err)
	require.Len(t, first.Profiles, 1)
	assert.Equal(t, "projects/sample-project/profiles/cpu-a", first.Profiles[0].ProfileID)
	assert.True(t, first.Truncated)
	assert.NotEmpty(t, first.NextPageToken)

	params.PageToken = first.NextPageToken
	second, err := q.ListProfiles(context.Background(), params)
	require.NoError(t, err)
	require.Len(t, second.Profiles, 1)
	assert.Equal(t, "projects/sample-project/profiles/cpu-b", second.Profiles[0].ProfileID)
	assert.Equal(t, []string{"", ""}, service.seen, "second logical page must resume in the first API page")

	params.PageToken = second.NextPageToken
	third, err := q.ListProfiles(context.Background(), params)
	require.NoError(t, err)
	require.Len(t, third.Profiles, 1)
	assert.Equal(t, "projects/sample-project/profiles/cpu-c", third.Profiles[0].ProfileID)
	assert.False(t, third.Truncated)
	assert.Empty(t, third.NextPageToken)
	assert.Equal(t, []string{"", "", "", "api-2"}, service.seen)
}

func TestProfilerCursorIsOpaqueAndFilterBound(t *testing.T) {
	q, _ := newPagedProfilerQuerier(t)
	params := ListProfilesParams{Project: "sample-project", ProfileType: "CPU", PageSize: 1}
	first, err := q.ListProfiles(context.Background(), params)
	require.NoError(t, err)

	params.Target = "different"
	params.PageToken = first.NextPageToken
	_, err = q.ListProfiles(context.Background(), params)
	assert.ErrorContains(t, err, "cursor/filter mismatch")

	params.Target = ""
	params.PageToken = "api-2"
	_, err = q.ListProfiles(context.Background(), params)
	assert.ErrorContains(t, err, "invalid profiler cursor")
}

func TestProfilerUnfilteredPaginationEndsExplicitly(t *testing.T) {
	q, _ := newPagedProfilerQuerier(t)
	result, err := q.ListProfiles(context.Background(), ListProfilesParams{Project: "sample-project", ProfileType: "CPU", PageSize: 10})
	require.NoError(t, err)
	assert.Len(t, result.Profiles, 3)
	assert.False(t, result.Truncated)
	assert.Empty(t, result.NextPageToken)
}
