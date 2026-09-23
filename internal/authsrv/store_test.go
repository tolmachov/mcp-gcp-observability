package authsrv

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testGrant(now time.Time, secret string) grantRecord {
	return grantRecord{
		ActiveSecretHash: tokenHash(secret),
		Generation:       1,
		Status:           "active",
		ExpiresAt:        now.Add(time.Hour),
		UpdatedAt:        now,
	}
}

func TestAuthorizationCodeReplayRevokesGrantFamily(t *testing.T) {
	store := newMemoryStateStore()
	now := time.Now()
	require.NoError(t, store.PutCode(context.Background(), "code", codeRecord{
		Status: "active", FamilyID: "family", ExpiresAt: now.Add(time.Minute),
	}))

	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			errs <- store.RedeemCode(context.Background(), "code", now, testGrant(now, "s1"))
		}()
	}
	close(start)
	first, second := <-errs, <-errs
	assert.True(t, (first == nil && errors.Is(second, errCodeReplay)) || (second == nil && errors.Is(first, errCodeReplay)))
	grant, err := store.GetGrant(context.Background(), "family")
	require.NoError(t, err)
	assert.Equal(t, "revoked", grant.Status)
}

func TestAuthorizationStateIsSingleUse(t *testing.T) {
	store := newMemoryStateStore()
	now := time.Now()
	rec := authorizationStateRecord{Claims: "encrypted", Status: "active", ExpiresAt: now.Add(time.Minute)}
	require.NoError(t, store.PutAuthorizationState(context.Background(), "state", rec))
	got, err := store.UseAuthorizationState(context.Background(), "state", now)
	require.NoError(t, err)
	assert.Equal(t, rec.Claims, got.Claims)
	require.ErrorIs(t, func() error {
		_, err := store.UseAuthorizationState(context.Background(), "state", now)
		return err
	}(), errStateReplay)
}

func TestConcurrentRefreshReplayRevokesFamily(t *testing.T) {
	store := newMemoryStateStore()
	now := time.Now()
	store.grants["family"] = testGrant(now, "current")

	start := make(chan struct{})
	errs := make(chan error, 2)
	for i := range 2 {
		go func(i int) {
			<-start
			next := testGrant(now, "next-"+string(rune('a'+i)))
			next.Generation = 2
			errs <- store.RotateGrant(context.Background(), "family", tokenHash("current"), next, now)
		}(i)
	}
	close(start)
	first, second := <-errs, <-errs
	assert.True(t, (first == nil && errors.Is(second, errGrantReplay)) || (second == nil && errors.Is(first, errGrantReplay)))
	grant, err := store.GetGrant(context.Background(), "family")
	require.NoError(t, err)
	assert.Equal(t, "revoked", grant.Status)
}

func TestOldRefreshAfterRotationRevokesFamily(t *testing.T) {
	store := newMemoryStateStore()
	now := time.Now()
	store.grants["family"] = testGrant(now, "old")
	next := testGrant(now, "new")
	next.Generation = 2
	require.NoError(t, store.RotateGrant(context.Background(), "family", tokenHash("old"), next, now))
	require.ErrorIs(t, store.RotateGrant(context.Background(), "family", tokenHash("old"), grantRecord{}, now), errGrantReplay)
	grant, err := store.GetGrant(context.Background(), "family")
	require.NoError(t, err)
	assert.Equal(t, "revoked", grant.Status)
}

func TestStoreChecksAbsoluteExpiry(t *testing.T) {
	store := newMemoryStateStore()
	now := time.Now()
	store.codes["expired"] = codeRecord{Status: "active", FamilyID: "f", ExpiresAt: now.Add(-time.Second)}
	require.ErrorIs(t, store.RedeemCode(context.Background(), "expired", now, testGrant(now, "s")), errStateNotFound)
	store.grants["expired"] = grantRecord{Status: "active", ActiveSecretHash: tokenHash("s"), ExpiresAt: now.Add(-time.Second)}
	require.ErrorIs(t, store.RotateGrant(context.Background(), "expired", tokenHash("s"), grantRecord{}, now), errGrantInactive)
}

type failingStateStore struct {
	oauthStateStore
	err error
}

func (s failingStateStore) Health(context.Context) error { return s.err }
func (s failingStateStore) GetGrant(context.Context, string) (grantRecord, error) {
	return grantRecord{}, s.err
}

func TestStoreOutageIs503Not401(t *testing.T) {
	base := newMemoryStateStore()
	failure := errors.New("firestore unavailable")
	cfg := testConfig(t)
	cfg.stateStore = failingStateStore{oauthStateStore: base, err: failure}
	a, err := newAuthServer(cfg, nil, happyIdP())
	require.NoError(t, err)
	now := time.Now()
	token, err := sealBlob(a.sealer, accessBlob, accessClaims{FamilyID: "family", ExpiresAt: now.Add(time.Hour).Unix()})
	require.NoError(t, err)

	called := false
	handler := a.RequireBearerToken(nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	req := httptest.NewRequest(http.MethodPost, testIssuer, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	assert.False(t, called)
}

func TestMemoryStoreConcurrentAccessIsRaceSafe(t *testing.T) {
	store := newMemoryStateStore()
	now := time.Now()
	store.grants["f"] = testGrant(now, "s")
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = store.GetGrant(context.Background(), "f")
		}()
	}
	wg.Wait()
}
