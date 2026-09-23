package gcpdata

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
)

func TestGetResourceLabels_CachesOnSuccess(t *testing.T) {
	calls := 0
	q := &MonitoringQuerier{
		resourceLabels: newResourceLabelsCache(),
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
		resourceLabels: newResourceLabelsCache(),
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
		resourceLabels: newResourceLabelsCache(),
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
		resourceLabels: newResourceLabelsCache(),
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

// blockingDescriptors is a listDescriptors fake whose calls announce
// themselves on started and then block until release is closed.
type blockingDescriptors struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
	err     error
}

func newBlockingDescriptors(err error) *blockingDescriptors {
	return &blockingDescriptors{started: make(chan struct{}, 8), release: make(chan struct{}), err: err}
}

func (b *blockingDescriptors) list(ctx context.Context, _ *monitoring.MetricClient, _ string) ([]MonitoredResourceDescriptor, error) {
	b.calls.Add(1)
	b.started <- struct{}{}
	<-b.release
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b.err != nil {
		return nil, b.err
	}
	return []MonitoredResourceDescriptor{{Type: "gce_instance", Labels: []string{"instance_id"}}}, nil
}

// TestGetResourceLabels_SharedAcrossQueriers pins the process-wide contract:
// concurrent misses on one querier share one fetch, its success serves every
// querier through the shared cache, and projects are cached independently.
// The fetch count does not depend on scheduling: a caller that arrives after
// the fetch finished hits the cache instead of fetching again.
func TestGetResourceLabels_SharedAcrossQueriers(t *testing.T) {
	cache := newResourceLabelsCache()
	fake := newBlockingDescriptors(nil)
	a := &MonitoringQuerier{resourceLabels: cache, listDescriptors: fake.list}
	b := &MonitoringQuerier{resourceLabels: cache, listDescriptors: fake.list}

	var wg sync.WaitGroup
	lookup := func() {
		labels, err := a.GetResourceLabels(context.Background(), "proj", "gce_instance")
		assert.NoError(t, err)
		assert.Equal(t, []string{"instance_id"}, labels)
	}
	wg.Go(lookup)
	<-fake.started // the fetch is in flight and blocked
	for range 3 {
		wg.Go(lookup)
	}
	close(fake.release)
	wg.Wait()
	assert.Equal(t, int32(1), fake.calls.Load(), "one fetch for concurrent misses on a querier")

	labels, err := b.GetResourceLabels(context.Background(), "proj", "gce_instance")
	require.NoError(t, err)
	assert.Equal(t, []string{"instance_id"}, labels)
	assert.Equal(t, int32(1), fake.calls.Load(), "another querier reuses the cached success")

	_, err = b.GetResourceLabels(context.Background(), "other-proj", "gce_instance")
	require.NoError(t, err)
	assert.Equal(t, int32(2), fake.calls.Load(), "a different project is fetched separately")
}

// TestGetResourceLabels_LeaderCancelDoesNotFailWaiter guards against the
// shared fetch inheriting the first caller's cancellation: the caller that
// started it gives up, the one waiting on it still gets the labels.
func TestGetResourceLabels_LeaderCancelDoesNotFailWaiter(t *testing.T) {
	fake := newBlockingDescriptors(nil)
	q := &MonitoringQuerier{resourceLabels: newResourceLabelsCache(), listDescriptors: fake.list}

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := q.GetResourceLabels(leaderCtx, "proj", "gce_instance")
		leaderErr <- err
	}()
	<-fake.started

	waiter := make(chan []string, 1)
	go func() {
		labels, err := q.GetResourceLabels(context.Background(), "proj", "gce_instance")
		assert.NoError(t, err)
		waiter <- labels
	}()

	cancelLeader()
	require.ErrorIs(t, <-leaderErr, context.Canceled, "the leader returns on its own cancellation")
	close(fake.release)
	assert.Equal(t, []string{"instance_id"}, <-waiter)
	assert.Equal(t, int32(1), fake.calls.Load())
}

// TestGetResourceLabels_FailureStaysWithItsQuerier guards the shared
// deployment: a user whose credentials cannot list descriptors must not hand
// that error to a concurrent user whose client works.
func TestGetResourceLabels_FailureStaysWithItsQuerier(t *testing.T) {
	cache := newResourceLabelsCache()
	denied := errors.New("permission denied")
	failing := newBlockingDescriptors(denied)
	working := newBlockingDescriptors(nil)
	a := &MonitoringQuerier{resourceLabels: cache, listDescriptors: failing.list}
	b := &MonitoringQuerier{resourceLabels: cache, listDescriptors: working.list}

	aErr := make(chan error, 1)
	go func() {
		_, err := a.GetResourceLabels(context.Background(), "proj", "gce_instance")
		aErr <- err
	}()
	<-failing.started
	bLabels := make(chan []string, 1)
	go func() {
		labels, err := b.GetResourceLabels(context.Background(), "proj", "gce_instance")
		assert.NoError(t, err)
		bLabels <- labels
	}()
	<-working.started // both fetches are in flight at once

	close(failing.release)
	require.ErrorIs(t, <-aErr, denied)
	close(working.release)
	assert.Equal(t, []string{"instance_id"}, <-bLabels)
}
