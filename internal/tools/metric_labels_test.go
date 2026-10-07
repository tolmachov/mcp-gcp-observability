package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

// pubsubSubDescriptor mimics the Cloud Monitoring descriptor for
// "pubsub.googleapis.com/subscription/sent_message_count": topic_id and
// delivery_type live on metric.labels, while the pubsub_subscription
// resource only carries project_id + subscription_id. This is exactly the
// setup where a naïve caller will use `resource.labels.topic_id` and get
// a cryptic InvalidArgument from the API.
func pubsubSubDescriptor() gcpdata.MetricDescriptorInfo {
	return gcpdata.MetricDescriptorInfo{
		Type:       "pubsub.googleapis.com/subscription/sent_message_count",
		MetricKind: "DELTA",
		ValueType:  "INT64",
		Labels: []gcpdata.LabelDescriptor{
			{Key: "topic_id"},
			{Key: "delivery_type"},
		},
		MonitoredResourceTypes: []string{"pubsub_subscription"},
	}
}

func pubsubSubResourceLabels() map[string][]string {
	return map[string][]string{
		"pubsub_subscription": {"project_id", "subscription_id"},
	}
}

func TestFetchAvailableLabels_PubsubSub(t *testing.T) {
	fq := newFakeQuerier()
	fq.descriptors = []gcpdata.MetricDescriptorInfo{pubsubSubDescriptor()}
	fq.resourceLabels = pubsubSubResourceLabels()

	labels, err := fetchAvailableLabels(context.Background(), fq, "my-project", "pubsub.googleapis.com/subscription/sent_message_count")
	require.NoError(t, err)
	require.NotNil(t, labels)
	assert.True(t, equalSortedStrings(labels.Metric, []string{"delivery_type", "topic_id"}))
	got, ok := labels.Resource["pubsub_subscription"]
	require.True(t, ok)
	assert.True(t, equalSortedStrings(got, []string{"project_id", "subscription_id"}))
}

func TestFetchAvailableLabels_DescriptorMissing(t *testing.T) {
	fq := newFakeQuerier()
	// No descriptors registered → ListMetricDescriptors returns empty → fetch must return nil.
	labels, err := fetchAvailableLabels(context.Background(), fq, "p", "x.googleapis.com/unknown")
	assert.NoError(t, err)
	assert.Nil(t, labels)
}

func TestFetchAvailableLabels_DescriptorListingFails(t *testing.T) {
	fq := newFakeQuerier()
	fq.listMetricDescriptorsErr = errors.New("upstream is down")
	labels, err := fetchAvailableLabels(context.Background(), fq, "p", "x.googleapis.com/foo")
	assert.Nil(t, labels)
	require.Error(t, err)
	assert.ErrorIs(t, err, fq.listMetricDescriptorsErr)
	assert.Equal(t, `listing the descriptor of metric "x.googleapis.com/foo": upstream is down`, err.Error())
}

func TestFetchAvailableLabels_UnknownResourceType(t *testing.T) {
	// Descriptor has resource type, but the fake doesn't know it. Should
	// still return metric labels and an empty resource map (= nil).
	fq := newFakeQuerier()
	fq.descriptors = []gcpdata.MetricDescriptorInfo{
		{
			Type:                   "x.googleapis.com/foo",
			Labels:                 []gcpdata.LabelDescriptor{{Key: "kind"}},
			MonitoredResourceTypes: []string{"unknown_type"},
		},
	}
	labels, err := fetchAvailableLabels(context.Background(), fq, "p", "x.googleapis.com/foo")
	require.NoError(t, err)
	require.NotNil(t, labels)
	assert.Empty(t, labels.IncompleteReason)
	assert.Equal(t, 1, len(labels.Metric))
	assert.Equal(t, "kind", labels.Metric[0])
	assert.Nil(t, labels.Resource)
}

func TestIsInvalidFilterError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"generic", errors.New("timeout"), false},
		{
			"real GCP wording wrapped as plain error",
			errors.New(`listing time series: rpc error: code = InvalidArgument desc = The supplied filter does not specify a valid combination of metric and monitored resource descriptors. The query will not return any time series.`),
			true,
		},
		{
			"mixed case fallback regex",
			errors.New("Filter does not specify a valid combination"),
			true,
		},
		{
			"gRPC status InvalidArgument with matching phrase",
			status.Error(codes.InvalidArgument, "The supplied filter does not specify a valid combination of metric and monitored resource descriptors."),
			true,
		},
		{
			// Regression guard: an InvalidArgument error with a DIFFERENT
			// message must not be mistaken for a filter error. The layered
			// detection (code + phrase) exists to prevent over-matching.
			"gRPC status InvalidArgument with unrelated message",
			status.Error(codes.InvalidArgument, "metric type is required"),
			false,
		},
		{
			// Regression guard: a non-InvalidArgument gRPC status that
			// happens to contain the phrase in its message must not match.
			"gRPC status NotFound with matching phrase",
			status.Error(codes.NotFound, "filter does not specify a valid combination (but really NotFound)"),
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isInvalidFilterError(tc.err))
		})
	}
}

func TestEnrichInvalidFilterError_MisplacedNamespaceHint(t *testing.T) {
	fq := newFakeQuerier()
	fq.descriptors = []gcpdata.MetricDescriptorInfo{pubsubSubDescriptor()}
	fq.resourceLabels = pubsubSubResourceLabels()

	origErr := errors.New(`rpc error: code = InvalidArgument desc = The supplied filter does not specify a valid combination [trace-id=abc123]`)
	msg := enrichInvalidFilterError(
		context.Background(),
		fq,
		"my-project",
		"pubsub.googleapis.com/subscription/sent_message_count",
		`resource.labels.topic_id = "ad-events"`,
		origErr,
	)

	// The error should clearly list available labels in both namespaces.
	for _, want := range []string{
		"metric.labels:",
		"topic_id",
		"delivery_type",
		"resource.labels:",
		"project_id",
		"subscription_id",
		"pubsub_subscription",
	} {
		assert.Contains(t, msg, want)
	}

	// Regression guard: the raw upstream error (including anything like a
	// trace ID GCP may include) must appear verbatim. Previously the code
	// hard-coded a canned "GCP said" line that discarded origErr.
	assert.Contains(t, msg, "trace-id=abc123")

	// And produce the "did you mean metric.labels.topic_id" hint for the
	// specific misplaced-namespace failure mode we're targeting.
	assert.Contains(t, msg, "try `metric.labels.topic_id`")
}

func TestEnrichInvalidFilterError_DescriptorUnavailableFallback(t *testing.T) {
	// If we can't fetch the descriptor (simulating a downstream API failure
	// during enrichment), we must not hide the original error — we should
	// still return something that names it plus a short note so the caller
	// isn't left with a silent pass-through.
	fq := newFakeQuerier()
	fq.listMetricDescriptorsErr = errors.New("upstream is down")
	origErr := errors.New("the supplied filter does not specify a valid combination")

	msg := enrichInvalidFilterError(context.Background(), fq, "p", "x.googleapis.com/foo", "resource.labels.bogus=1", origErr)
	assert.Contains(t, msg, "the supplied filter does not specify a valid combination")
	// The enrichment failure itself is named once, not just its consequence.
	assert.Contains(t, msg, `(Could not fetch label descriptors for "x.googleapis.com/foo" to suggest a fix: listing the descriptor of metric "x.googleapis.com/foo": upstream is down)`)
}

func TestMetricsSnapshot_AttachesAvailableLabels(t *testing.T) {
	fq := newFakeQuerier()
	fq.descriptors = []gcpdata.MetricDescriptorInfo{pubsubSubDescriptor()}
	fq.resourceLabels = pubsubSubResourceLabels()
	fq.metricKinds["pubsub.googleapis.com/subscription/sent_message_count"] = "DELTA"
	fq.valueTypes["pubsub.googleapis.com/subscription/sent_message_count"] = "INT64"
	baseTime := time.Now().UTC().Add(-2 * time.Hour)
	fq.series["pubsub.googleapis.com/subscription/sent_message_count"] = []gcpdata.MetricTimeSeries{
		makeTimeSeries(baseTime, []float64{10, 11, 12, 13, 14, 15, 16, 17, 18, 19}),
	}

	reg := loadTestRegistry(t, `metrics:
  "pubsub.googleapis.com/subscription/sent_message_count":
    kind: throughput
    unit: count
    better_direction: none
`)
	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsSnapshot(fq, reg, "my-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_snapshot", map[string]any{
		"metric_type": "pubsub.googleapis.com/subscription/sent_message_count",
		"window":      "1h",
	})
	require.NoError(t, err)
	require.False(t, result.IsError)

	var snap MetricSnapshotResult
	unmarshalResult(t, result, &snap)

	require.NotNil(t, snap.AvailableLabels)
	assert.True(t, equalSortedStrings(snap.AvailableLabels.Metric, []string{"delivery_type", "topic_id"}))
	got := snap.AvailableLabels.Resource["pubsub_subscription"]
	assert.True(t, equalSortedStrings(got, []string{"project_id", "subscription_id"}))
}

// TestLabelEnrichmentSoftDegradation pins that a failed resource-label
// listing is a soft degradation on every result path of metrics_snapshot and
// metrics_top_contributors: the tool still succeeds, metric labels come from
// the already-fetched descriptor, and available_labels names the incomplete
// resource types and the reason. IncompleteTypes distinguishes "RPC failed"
// from "metric has no resource labels".
func TestLabelEnrichmentSoftDegradation(t *testing.T) {
	const metricType = "pubsub.googleapis.com/subscription/sent_message_count"
	const wantReason = `Resource label keys of metric "` + metricType + `" could not be fetched ` +
		`(permission denied on ListMonitoredResourceDescriptors); available_labels.resource is incomplete for pubsub_subscription.`
	reg := loadTestRegistry(t, `metrics:
  "pubsub.googleapis.com/subscription/sent_message_count":
    kind: throughput
    unit: count
    better_direction: none
`)

	runSnapshot := func(t *testing.T, fq *fakeQuerier) (bool, *AvailableLabels) {
		t.Helper()
		snap := runAggregationSnapshot(t, fq, reg, metricType)
		return snap.NoData, snap.AvailableLabels
	}
	runTop := func(t *testing.T, fq *fakeQuerier) (bool, *AvailableLabels) {
		t.Helper()
		ctx := context.Background()
		ts := newTestToolServer(t)
		ts.registerMetricsTop(fq, reg, "test-project")
		ts.connect(ctx)
		defer ts.close()
		result, err := ts.callTool(ctx, "metrics_top_contributors", map[string]any{
			"metric_type": metricType,
			"dimension":   "metric.labels.topic_id",
			"window":      "15m",
		})
		require.NoError(t, err)
		var top TopContributorsResult
		parseResult(t, result, &top)
		return top.NoData, top.AvailableLabels
	}

	cases := []struct {
		name     string
		run      func(*testing.T, *fakeQuerier) (bool, *AvailableLabels)
		withData bool
	}{
		{"snapshot with data", runSnapshot, true},
		{"snapshot without data", runSnapshot, false},
		{"top contributors with data", runTop, true},
		{"top contributors without data", runTop, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fq := newFakeQuerier()
			fq.descriptors = []gcpdata.MetricDescriptorInfo{pubsubSubDescriptor()}
			fq.getResourceLabelsErr = errors.New("permission denied on ListMonitoredResourceDescriptors")
			fq.metricKinds[metricType] = "DELTA"
			fq.valueTypes[metricType] = "INT64"
			if tc.withData {
				fq.series[metricType] = []gcpdata.MetricTimeSeries{
					makeTimeSeriesWithLabels(time.Now().UTC().Add(-10*time.Minute), []float64{10, 11, 12, 13, 14, 15, 16, 17, 18, 19},
						map[string]string{"topic_id": "ad-events"}),
				}
			}

			noData, labels := tc.run(t, fq)

			assert.Equal(t, !tc.withData, noData)
			require.NotNil(t, labels)
			assert.Equal(t, []string{"delivery_type", "topic_id"}, labels.Metric)
			assert.Nil(t, labels.Resource)
			assert.Equal(t, []string{"pubsub_subscription"}, labels.IncompleteTypes)
			assert.Equal(t, wantReason, labels.IncompleteReason)
		})
	}
}

func TestEnrichInvalidFilterError_ResourceLabelsFailureNote(t *testing.T) {
	fq := newFakeQuerier()
	fq.descriptors = []gcpdata.MetricDescriptorInfo{pubsubSubDescriptor()}
	fq.getResourceLabelsErr = errors.New("permission denied on ListMonitoredResourceDescriptors")
	origErr := errors.New("the supplied filter does not specify a valid combination")

	msg := enrichInvalidFilterError(context.Background(), fq, "p",
		"pubsub.googleapis.com/subscription/sent_message_count", `resource.labels.topic_id = "ad-events"`, origErr)

	assert.Contains(t, msg, "Filter invalid for pubsub.googleapis.com/subscription/sent_message_count.")
	assert.Contains(t, msg, "\nNote: Resource label keys of metric")
	assert.Contains(t, msg, "permission denied on ListMonitoredResourceDescriptors")
}

func TestMetricsSnapshot_EnrichedErrorOnInvalidFilter(t *testing.T) {
	fq := newFakeQuerier()
	fq.descriptors = []gcpdata.MetricDescriptorInfo{pubsubSubDescriptor()}
	fq.resourceLabels = pubsubSubResourceLabels()
	fq.metricKinds["pubsub.googleapis.com/subscription/sent_message_count"] = "DELTA"
	fq.valueTypes["pubsub.googleapis.com/subscription/sent_message_count"] = "INT64"
	// Simulate GCP rejecting the filter with the exact wording we match on.
	fq.queryTimeSeriesErr = errors.New(`rpc error: code = InvalidArgument desc = The supplied filter does not specify a valid combination of metric and monitored resource descriptors.`)

	reg := loadTestRegistry(t, `metrics:
  "pubsub.googleapis.com/subscription/sent_message_count":
    kind: throughput
    unit: count
    better_direction: none
`)
	ctx := context.Background()
	ts := newTestToolServer(t)
	ts.registerMetricsSnapshot(fq, reg, "my-project")
	ts.connect(ctx)
	defer ts.close()

	result, err := ts.callTool(ctx, "metrics_snapshot", map[string]any{
		"metric_type": "pubsub.googleapis.com/subscription/sent_message_count",
		"filter":      `resource.labels.topic_id = "ad-events"`,
	})
	require.NoError(t, err)
	require.True(t, result.IsError)

	msg := textFromResult(t, result)
	for _, want := range []string{
		"Filter invalid",
		"metric.labels:",
		"topic_id",
		"resource.labels:",
		"subscription_id",
		"try `metric.labels.topic_id`",
	} {
		assert.Contains(t, msg, want)
	}
}

// --- helpers ---

func equalSortedStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func unmarshalResult(t *testing.T, result *mcp.CallToolResult, into any) {
	t.Helper()
	text := textFromResult(t, result)
	err := json.Unmarshal([]byte(text), into)
	require.NoError(t, err)
}

func textFromResult(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	require.NotNil(t, result)
	require.Greater(t, len(result.Content), 0)
	tc, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	return tc.Text
}

// countingLabelsQuerier counts GetResourceLabels calls.
type countingLabelsQuerier struct {
	*fakeQuerier
	calls int
}

func (q *countingLabelsQuerier) GetResourceLabels(ctx context.Context, project, resourceType string) ([]string, error) {
	q.calls++
	return q.fakeQuerier.GetResourceLabels(ctx, project, resourceType)
}

// TestAvailableLabelsStopsAfterResourceLabelsFailure pins that a failed
// resource-label listing is not repeated for every remaining resource type:
// the rest are reported incomplete straight away.
func TestAvailableLabelsStopsAfterResourceLabelsFailure(t *testing.T) {
	fq := newFakeQuerier()
	fq.getResourceLabelsErr = errors.New("listing failed")
	q := &countingLabelsQuerier{fakeQuerier: fq}
	desc := gcpdata.MetricDescriptorBasic{MonitoredResourceTypes: []string{"a", "b", "c"}}

	labels := availableLabelsFromDescriptor(context.Background(), q, "p", "m", desc)

	assert.Equal(t, 1, q.calls)
	assert.Equal(t, []string{"a", "b", "c"}, labels.IncompleteTypes)
	assert.Nil(t, labels.Resource)
	assert.Equal(t, `Resource label keys of metric "m" could not be fetched (listing failed); available_labels.resource is incomplete for a, b, c.`, labels.IncompleteReason)
}
