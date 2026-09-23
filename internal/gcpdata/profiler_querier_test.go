package gcpdata

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"testing"

	"cloud.google.com/go/cloudprofiler/apiv2/cloudprofilerpb"
	"github.com/google/pprof/profile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stockProfilerServer serves a fixed set of profiles, bytes included, in one
// page and counts the list scans it answers.
type stockProfilerServer struct {
	cloudprofilerpb.UnimplementedExportServiceServer
	profiles []*cloudprofilerpb.Profile

	mu    sync.Mutex
	lists int
}

func (s *stockProfilerServer) ListProfiles(context.Context, *cloudprofilerpb.ListProfilesRequest) (*cloudprofilerpb.ListProfilesResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lists++
	return &cloudprofilerpb.ListProfilesResponse{Profiles: s.profiles}, nil
}

func (s *stockProfilerServer) listCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lists
}

// stockProfile is a CPU profile of target "svc" carrying data as its bytes.
func stockProfile(id string, minute int, data []byte) *cloudprofilerpb.Profile {
	p := testProfile(id, cloudprofilerpb.ProfileType_CPU, "svc", minute)
	p.ProfileBytes = data
	return p
}

func encodeProfile(t *testing.T, p *profile.Profile) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, p.Write(&buf))
	return buf.Bytes()
}

// halvedTestProfile is buildTestProfile with every sample value halved.
func halvedTestProfile() *profile.Profile {
	p := buildTestProfile()
	p.Scale(0.5)
	return p
}

// emptyTestProfile has a sample type but no samples.
func emptyTestProfile() *profile.Profile {
	return &profile.Profile{SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}}}
}

func newStockProfilerQuerier(t *testing.T, profiles ...*cloudprofilerpb.Profile) (*CloudProfilerQuerier, *stockProfilerServer) {
	t.Helper()
	srv := &stockProfilerServer{profiles: profiles}
	return newFakeProfilerQuerier(t, srv, slog.New(slog.DiscardHandler)), srv
}

const stockProject = "sample-project"

func stockName(id string) string { return "projects/" + stockProject + "/profiles/" + id }

func TestGetOrFetchProfile_CacheHitSkipsScan(t *testing.T) {
	q, srv := newStockProfilerQuerier(t, stockProfile("cpu-a", 1, encodeProfile(t, buildTestProfile())))

	_, meta, err := q.GetOrFetchProfile(context.Background(), stockProject, "cpu-a")
	require.NoError(t, err)
	assert.Equal(t, stockName("cpu-a"), meta.ProfileID)

	p, cached, err := q.GetOrFetchProfile(context.Background(), stockProject, "cpu-a")
	require.NoError(t, err)
	assert.Len(t, p.Sample, 3)
	assert.Equal(t, meta, cached)
	assert.Equal(t, 1, srv.listCalls(), "the second fetch is served from the cache")
}

func TestGetOrFetchProfile_RejectsEmptyAndOversizedBytes(t *testing.T) {
	q, _ := newStockProfilerQuerier(t,
		stockProfile("empty", 1, nil),
		stockProfile("huge", 2, make([]byte, maxCompressedProfileBytes+1)),
	)

	_, _, err := q.GetOrFetchProfile(context.Background(), stockProject, "empty")
	assert.ErrorContains(t, err, "has no profile bytes")

	_, _, err = q.GetOrFetchProfile(context.Background(), stockProject, "huge")
	assert.ErrorIs(t, err, errUndecodableProfile)
	assert.ErrorContains(t, err, "compressed profile is")

	assert.Equal(t, 0, q.cache.Len(), "rejected profiles are not cached")
}

func TestGetOrFetchProfile_LogsRejectedCacheInsert(t *testing.T) {
	var logs bytes.Buffer
	srv := &stockProfilerServer{profiles: []*cloudprofilerpb.Profile{stockProfile("cpu-a", 1, encodeProfile(t, buildTestProfile()))}}
	q := newFakeProfilerQuerier(t, srv, slog.New(slog.NewTextHandler(&logs, nil)))
	q.cache.maxBytes = 1

	_, _, err := q.GetOrFetchProfile(context.Background(), stockProject, "cpu-a")
	require.NoError(t, err, "a profile the cache rejects is still returned")
	assert.Contains(t, logs.String(), "profile_not_cached")
	assert.Contains(t, logs.String(), "per-user cache limit")
}

func TestGetProfileOrDiff_WithBase(t *testing.T) {
	q, _ := newStockProfilerQuerier(t,
		stockProfile("cur", 2, encodeProfile(t, buildTestProfile())),
		stockProfile("base", 1, encodeProfile(t, halvedTestProfile())),
	)

	diff, meta, err := q.GetProfileOrDiff(context.Background(), stockProject, "cur", "base")
	require.NoError(t, err)
	assert.True(t, meta.IsDiff)
	assert.Equal(t, stockName("cur"), meta.ProfileID)
	_, total, _, err := TopFunctions(diff, 0, 0, "cumulative", "")
	require.NoError(t, err)
	assert.Equal(t, int64(50), total, "current total 100 minus base total 50")

	_, _, err = q.GetProfileOrDiff(context.Background(), stockProject, "cur", "missing")
	assert.ErrorContains(t, err, "fetching base profile:")
	_, _, err = q.GetProfileOrDiff(context.Background(), stockProject, "missing", "base")
	assert.ErrorContains(t, err, "fetching current profile:")
}

func TestCompareProfiles(t *testing.T) {
	q, _ := newStockProfilerQuerier(t,
		stockProfile("cur", 2, encodeProfile(t, buildTestProfile())),
		stockProfile("base", 1, encodeProfile(t, halvedTestProfile())),
	)

	res, err := q.CompareProfiles(context.Background(), stockProject, "cur", "base", 0, 2)
	require.NoError(t, err)
	assert.Equal(t, stockName("cur"), res.CurrentMeta.ProfileID)
	assert.Equal(t, stockName("base"), res.BaseMeta.ProfileID)
	assert.Equal(t, CompareSummary{
		TotalCurrent: 100, TotalBase: 50, TotalDelta: 50, TotalDeltaPct: 100,
		FunctionsRegressed: 5,
	}, res.Summary)
	require.Len(t, res.TopRegressions, 2)
	assert.Equal(t, "main.main", res.TopRegressions[0].Name)
	assert.Equal(t, int64(50), res.TopRegressions[0].Delta)
	assert.Empty(t, res.TopImprovements)
	assert.True(t, res.Truncated)
}

func trendsParams() ComputeTrendsParams {
	return ComputeTrendsParams{Project: stockProject, ProfileType: "CPU", Target: "svc", MaxProfiles: 10, MaxFunctions: 5}
}

func TestComputeTrends_AllProfilesEmptyWarns(t *testing.T) {
	empty := encodeProfile(t, emptyTestProfile())
	q, _ := newStockProfilerQuerier(t, stockProfile("a", 1, empty), stockProfile("b", 2, empty))

	res, err := q.ComputeTrends(context.Background(), trendsParams(), nil)
	require.NoError(t, err)
	assert.Empty(t, res.Functions)
	assert.Equal(t, 2, res.AnalyzedCount)
	assert.Contains(t, res.Warning, "All 2 analyzed profiles had no samples for value_index 0")
}

// TestComputeTrends_UndecodableProfileCountsAsParseError pins that bytes the
// prefetch cached but that do not parse are reported as undecodable rather than
// as a download failure, dropped from the cache, and not downloaded again.
func TestComputeTrends_UndecodableProfileCountsAsParseError(t *testing.T) {
	q, srv := newStockProfilerQuerier(t,
		stockProfile("good", 1, encodeProfile(t, buildTestProfile())),
		stockProfile("bad", 2, []byte("not a profile")),
	)

	res, err := q.ComputeTrends(context.Background(), trendsParams(), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.AnalyzedCount)
	assert.Equal(t, 1, res.ParseErrors)
	assert.Zero(t, res.DownloadErrors)
	assert.NotEmpty(t, res.Functions)
	assert.Contains(t, res.Warning, "1 undecodable")
	assert.False(t, q.cache.contains(profileCacheKey(stockProject, stockName("bad"))))
	assert.Equal(t, 2, srv.listCalls(), "one metadata listing and one prefetch scan")
}

func TestPrefetchProfiles_StopsBeforeEvictingOwnEntries(t *testing.T) {
	data := encodeProfile(t, buildTestProfile())
	q, _ := newStockProfilerQuerier(t, stockProfile("a", 1, data), stockProfile("b", 2, data), stockProfile("c", 3, data))
	q.cache.maxBytes = int64(2*len(data) + 1)

	wanted := map[string]bool{stockName("a"): true, stockName("b"): true, stockName("c"): true}
	res := q.prefetchProfiles(context.Background(), stockProject, wanted)
	assert.Equal(t, 2, res.Cached)
	assert.True(t, res.Full)
	assert.True(t, q.cache.contains(profileCacheKey(stockProject, stockName("a"))), "the run's first profile is not evicted")
	assert.Contains(t, res.warning(3), "cached 2/3 profiles")
	assert.Contains(t, res.warning(3), "per-user profile cache limit")
}

func TestPrefetchProfiles_OversizedProfileIsSkippedWithCause(t *testing.T) {
	small := encodeProfile(t, emptyTestProfile())
	big := encodeProfile(t, buildTestProfile())
	require.Greater(t, len(big), 2*len(small))
	q, _ := newStockProfilerQuerier(t, stockProfile("a", 1, small), stockProfile("big", 2, big), stockProfile("c", 3, small))
	q.cache.maxBytes = int64(2 * len(small))

	wanted := map[string]bool{stockName("a"): true, stockName("big"): true, stockName("c"): true}
	res := q.prefetchProfiles(context.Background(), stockProject, wanted)
	assert.Equal(t, 2, res.Cached)
	assert.Equal(t, 1, res.Skipped)
	assert.False(t, res.Full)
	assert.ErrorContains(t, res.LastSkip, "per-user cache limit")
}
