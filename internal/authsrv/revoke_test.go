package authsrv

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRevokeRequiresRefreshSecret(t *testing.T) {
	for _, tokenKind := range []string{"wrong secret", "missing family", "rotated token", "current token", "upstream retry"} {
		t.Run(tokenKind, func(t *testing.T) {
			idp := happyIdP()
			a, err := NewWithProvider(testConfig(t), nil, idp)
			require.NoError(t, err)
			now := time.Now()
			claims, err := sealBlob(a.sealer, storedGrantBlob, refreshClaims{
				GoogleRefreshToken: "upstream-refresh", IssuedAt: now.Unix(),
			})
			require.NoError(t, err)
			rec := testGrant(now, "current-secret")
			rec.Claims = claims
			store, ok := a.store.(*memoryStateStore)
			require.True(t, ok)
			store.grants["family"] = rec
			token := prefixRefresh + "family.current-secret"
			switch tokenKind {
			case "wrong secret":
				token = prefixRefresh + "family.forged"
			case "missing family":
				token = prefixRefresh + "missing.current-secret"
			case "rotated token":
				next := rec
				next.ActiveSecretHash = tokenHash("next-secret")
				next.Generation++
				require.NoError(t, store.RotateGrant(context.Background(), "family", rec.ActiveSecretHash, next, now))
			case "upstream retry":
				idp.revokeErr = assert.AnError
			}
			revoke := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, "/revoke", strings.NewReader(url.Values{"token": {token}}.Encode()))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				w := httptest.NewRecorder()
				a.handleRevoke(w, r)
				return w
			}
			w := revoke()
			if tokenKind == "upstream retry" {
				require.Equal(t, http.StatusServiceUnavailable, w.Code)
				grant, getErr := store.GetGrant(context.Background(), "family")
				require.NoError(t, getErr)
				assert.Equal(t, "revoked", grant.Status)
				idp.revokeErr = nil
				w = revoke()
			}
			require.Equal(t, http.StatusOK, w.Code)
			grant, err := store.GetGrant(context.Background(), "family")
			require.NoError(t, err)
			if tokenKind == "current token" || tokenKind == "upstream retry" {
				assert.Equal(t, "revoked", grant.Status)
				assert.Equal(t, []string{"upstream-refresh"}, idp.revoked)
			} else {
				assert.Equal(t, "active", grant.Status)
				assert.Empty(t, idp.revoked)
			}
		})
	}
}
