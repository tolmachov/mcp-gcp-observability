package authsrv

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFirestoreStoreEmulator exercises the production transaction
// implementation. CI sets FIRESTORE_EMULATOR_HOST; local unit runs can omit it.
func TestFirestoreStoreEmulator(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("FIRESTORE_EMULATOR_HOST is not set")
	}
	ctx := context.Background()
	store, err := newFirestoreStateStore(ctx, "mcp-observability-ci", "(default)", slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	suffix, err := randomOpaque(8)
	require.NoError(t, err)
	stateKey := "state-" + suffix
	require.NoError(t, store.PutAuthorizationState(ctx, stateKey, authorizationStateRecord{
		Claims: "encrypted", Status: "active", ExpiresAt: now.Add(time.Minute),
	}))
	state, err := store.UseAuthorizationState(ctx, stateKey, now)
	require.NoError(t, err)
	assert.Equal(t, "encrypted", state.Claims)
	_, err = store.UseAuthorizationState(ctx, stateKey, now)
	require.ErrorIs(t, err, errStateReplay)

	codeKey, family := "code-"+suffix, "family-"+suffix
	require.NoError(t, store.PutCode(ctx, codeKey, codeRecord{Status: "active", FamilyID: family, ExpiresAt: now.Add(time.Minute)}))

	grant := testGrant(now, "current")
	require.NoError(t, store.RedeemCode(ctx, codeKey, now, grant))
	require.ErrorIs(t, store.RedeemCode(ctx, codeKey, now, grant), errCodeReplay)
	revoked, err := store.GetGrant(ctx, family)
	require.NoError(t, err)
	assert.Equal(t, "revoked", revoked.Status)

	family2 := family + "-rotation"
	ref := store.grant(family2)
	_, err = ref.Create(ctx, grant)
	require.NoError(t, err)
	next := testGrant(now, "next")
	next.Generation = 2
	require.NoError(t, store.RotateGrant(ctx, family2, tokenHash("current"), next, now))
	require.ErrorIs(t, store.RotateGrant(ctx, family2, tokenHash("current"), grantRecord{}, now), errGrantReplay)
	revoked, err = store.GetGrant(ctx, family2)
	require.NoError(t, err)
	assert.Equal(t, "revoked", revoked.Status)

	concurrentCodeKey, concurrentCodeFamily := "code-race-"+suffix, family+"-code-race"
	require.NoError(t, store.PutCode(ctx, concurrentCodeKey, codeRecord{
		Status: "active", FamilyID: concurrentCodeFamily, ExpiresAt: now.Add(time.Minute),
	}))
	codeResults := runConcurrentStoreOperations(func() error {
		return store.RedeemCode(ctx, concurrentCodeKey, now, testGrant(now, "code-race"))
	})
	assertOneSuccessAndReplay(t, codeResults, errCodeReplay)
	revoked, err = store.GetGrant(ctx, concurrentCodeFamily)
	require.NoError(t, err)
	assert.Equal(t, "revoked", revoked.Status, "a concurrent code redemption must revoke the resulting family")

	concurrentRefreshFamily := family + "-refresh-race"
	_, err = store.grant(concurrentRefreshFamily).Create(ctx, testGrant(now, "refresh-race"))
	require.NoError(t, err)
	refreshResults := runConcurrentStoreOperations(func() error {
		next := testGrant(now, "next")
		next.Generation = 2
		return store.RotateGrant(ctx, concurrentRefreshFamily, tokenHash("refresh-race"), next, now)
	})
	assertOneSuccessAndReplay(t, refreshResults, errGrantReplay)
	revoked, err = store.GetGrant(ctx, concurrentRefreshFamily)
	require.NoError(t, err)
	assert.Equal(t, "revoked", revoked.Status, "a concurrent refresh must revoke the whole family")

	family3 := family + "-expiry"
	expired := testGrant(now, "expired")
	expired.ExpiresAt = now.Add(-time.Second)
	_, err = store.grant(family3).Create(ctx, expired)
	require.NoError(t, err)
	require.ErrorIs(t, store.RotateGrant(ctx, family3, tokenHash("expired"), grantRecord{}, now), errGrantInactive)

	require.NoError(t, store.RevokeGrant(ctx, "missing-"+suffix, now))
	_, err = store.GetGrant(ctx, "missing-"+suffix)
	assert.True(t, errors.Is(err, errStateNotFound))

	corrupt := family + "-corrupt"
	_, err = store.grant(corrupt).Create(ctx, map[string]any{"status": 42})
	require.NoError(t, err)
	_, err = store.GetGrant(ctx, corrupt)
	assert.ErrorIs(t, err, errGrantCorrupt)
}

func runConcurrentStoreOperations(operation func() error) [2]error {
	start := make(chan struct{})
	results := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			results <- operation()
		}()
	}
	ready.Wait()
	close(start)
	return [2]error{<-results, <-results}
}

func assertOneSuccessAndReplay(t *testing.T, results [2]error, replay error) {
	t.Helper()
	assert.True(t,
		(results[0] == nil && errors.Is(results[1], replay)) ||
			(results[1] == nil && errors.Is(results[0], replay)),
		"expected one success and one replay error, got %v and %v", results[0], results[1])
}
