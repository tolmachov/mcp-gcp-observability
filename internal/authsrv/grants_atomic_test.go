package authsrv

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Rotate after the lifecycle's initial lookup, immediately before the actual
// revoke transaction. A possession check only on the lookup snapshot would
// wrongly revoke the new refresh token in this deterministic interleaving.
type rotateBeforeRevokeStore struct {
	oauthStateStore
}

func (s rotateBeforeRevokeStore) RevokeGrantIfSecret(ctx context.Context, id, expected string, now time.Time) (bool, error) {
	rec, err := s.GetGrant(ctx, id)
	if err != nil {
		return false, err
	}
	rec.ActiveSecretHash = tokenHash("replacement")
	rec.Generation++
	if err := s.RotateGrant(ctx, id, expected, rec, now); err != nil {
		return false, err
	}
	return s.oauthStateStore.RevokeGrantIfSecret(ctx, id, expected, now)
}

func TestRevokePossessionRemainsAtomicWithRotation(t *testing.T) {
	forEachGrantStore(t, func(t *testing.T, store oauthStateStore) {
		idp := happyIdP()
		a, err := NewWithProvider(testConfig(t), nil, idp)
		require.NoError(t, err)
		now := time.Now().UTC()
		a.now = func() time.Time { return now }
		family := "revoke-race-" + randomOpaque(8)
		rec := testGrant(now, "current")
		rec.Claims = sealBlob(a.sealer, storedGrantBlob, refreshClaims{GoogleRefreshToken: "upstream", IssuedAt: now.Unix()})
		seedGrant(t, store, family, rec, now)
		a.store = rotateBeforeRevokeStore{oauthStateStore: store}
		w := httptest.NewRecorder()
		a.handleRevoke(w, formRequest(RevokePath, url.Values{"token": {prefixRefresh + family + ".current"}}))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		got, err := store.GetGrant(context.Background(), family)
		require.NoError(t, err)
		assert.Equal(t, "active", got.Status, "a token rotated after lookup must not revoke the new family state")
		assert.Equal(t, int64(2), got.Generation)
		assert.Empty(t, idp.revoked)
	})
}

func TestConditionalRevokePreservesPossessionForUpstreamRetry(t *testing.T) {
	forEachGrantStore(t, func(t *testing.T, store oauthStateStore) {
		now := time.Now().UTC()
		for _, state := range []string{"active", "revoked", "expired"} {
			t.Run(state, func(t *testing.T) {
				family := "revoke-" + randomOpaque(8)
				rec := testGrant(now, "current")
				switch state {
				case "revoked":
					rec.Status = "revoked"
				case "expired":
					rec.ExpiresAt = now.Add(-time.Second)
				}
				seedGrant(t, store, family, rec, now)
				revoked, err := store.RevokeGrantIfSecret(context.Background(), family, tokenHash("wrong"), now)
				require.NoError(t, err)
				assert.False(t, revoked)
				unchanged, err := store.GetGrant(context.Background(), family)
				require.NoError(t, err)
				assert.Equal(t, rec.Status, unchanged.Status)
				revoked, err = store.RevokeGrantIfSecret(context.Background(), family, tokenHash("current"), now)
				require.NoError(t, err)
				assert.True(t, revoked)
				got, err := store.GetGrant(context.Background(), family)
				require.NoError(t, err)
				assert.Equal(t, "revoked", got.Status)
				assert.Equal(t, rec.ActiveSecretHash, got.ActiveSecretHash)
				assert.Equal(t, rec.ExpiresAt.UnixNano(), got.ExpiresAt.UnixNano())
			})
		}
	})
}

func forEachGrantStore(t *testing.T, run func(*testing.T, oauthStateStore)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { run(t, newMemoryStateStore()) })
	t.Run("firestore", func(t *testing.T) {
		if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
			t.Skip("FIRESTORE_EMULATOR_HOST is not set")
		}
		store, err := newFirestoreStateStore(context.Background(), "mcp-observability-ci", "(default)", slog.New(slog.DiscardHandler))
		require.NoError(t, err)
		t.Cleanup(func() { _ = store.Close() })
		run(t, store)
	})
}

func seedGrant(t *testing.T, store oauthStateStore, family string, rec grantRecord, now time.Time) {
	t.Helper()
	key := "seed-" + randomOpaque(8)
	require.NoError(t, store.PutCode(context.Background(), key, codeRecord{Status: "active", FamilyID: family, ExpiresAt: now.Add(time.Minute)}))
	require.NoError(t, store.RedeemCode(context.Background(), key, now, rec))
}
