package gcpdata

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
)

func TestGetResourceLabels_CachesOnSuccess(t *testing.T) {
	calls := 0
	q := &MonitoringQuerier{
		listDescriptors: func(ctx context.Context, _ *monitoring.MetricClient, _ string) ([]MonitoredResourceDescriptor, error) {
			calls++
			return []MonitoredResourceDescriptor{
				{Type: "pubsub_subscription", Labels: []string{"project_id", "subscription_id"}},
				{Type: "k8s_container", Labels: []string{"project_id", "cluster_name", "namespace_name"}},
			}, nil
		},
	}

	labels, err := q.GetResourceLabels(context.Background(), "proj", "pubsub_subscription")
	require.NoError(t, err)
	assert.True(t, equalStrings(labels, []string{"project_id", "subscription_id"}))

	labels2, err := q.GetResourceLabels(context.Background(), "proj", "k8s_container")
	require.NoError(t, err)
	assert.Len(t, labels2, 3)

	assert.Equal(t, 1, calls)
}

func TestGetResourceLabels_RetriesAfterError(t *testing.T) {
	// Regression guard: a transient failure on the first call must NOT
	// permanently disable label enrichment. Previously the code used
	// sync.Once + sticky error which latched the failure forever.
	calls := 0
	transient := errors.New("transient context canceled")
	q := &MonitoringQuerier{
		listDescriptors: func(ctx context.Context, _ *monitoring.MetricClient, _ string) ([]MonitoredResourceDescriptor, error) {
			calls++
			if calls == 1 {
				return nil, transient
			}
			return []MonitoredResourceDescriptor{
				{Type: "pubsub_subscription", Labels: []string{"project_id", "subscription_id"}},
			}, nil
		},
	}

	_, err := q.GetResourceLabels(context.Background(), "proj", "pubsub_subscription")
	require.ErrorIs(t, err, transient)

	labels, err := q.GetResourceLabels(context.Background(), "proj", "pubsub_subscription")
	require.NoError(t, err)
	assert.Len(t, labels, 2)

	assert.Equal(t, 2, calls)
}

func TestGetResourceLabels_UnknownTypeReturnsNilNil(t *testing.T) {
	q := &MonitoringQuerier{
		listDescriptors: func(ctx context.Context, _ *monitoring.MetricClient, _ string) ([]MonitoredResourceDescriptor, error) {
			return []MonitoredResourceDescriptor{
				{Type: "pubsub_subscription", Labels: []string{"project_id"}},
			}, nil
		},
	}

	labels, err := q.GetResourceLabels(context.Background(), "proj", "does_not_exist")
	assert.NoError(t, err)
	assert.Nil(t, labels)
}

func TestGetResourceLabels_ReturnsDefensiveCopy(t *testing.T) {
	q := &MonitoringQuerier{
		listDescriptors: func(ctx context.Context, _ *monitoring.MetricClient, _ string) ([]MonitoredResourceDescriptor, error) {
			return []MonitoredResourceDescriptor{
				{Type: "pubsub_subscription", Labels: []string{"project_id", "subscription_id"}},
			}, nil
		},
	}

	first, err := q.GetResourceLabels(context.Background(), "proj", "pubsub_subscription")
	require.NoError(t, err)
	// Mutate the returned slice.
	first[0] = "MUTATED"

	second, err := q.GetResourceLabels(context.Background(), "proj", "pubsub_subscription")
	require.NoError(t, err)
	assert.Equal(t, "project_id", second[0])
}

func equalStrings(a, b []string) bool {
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
