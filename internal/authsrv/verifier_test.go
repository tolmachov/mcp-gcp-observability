package authsrv

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// issuedTokens runs the full flow against a fresh server and returns it with
// the issued token pair and the client that owns it.
func issuedTokens(t *testing.T) (*AuthServer, *httptest.Server, *tokenResponse, string) {
	t.Helper()
	a, ts := newTestServer(t, testConfig(t), happyIdP())
	const redirectURI = "http://localhost:41234/callback"
	clientID := registerClient(t, ts, redirectURI)
	verifier, challenge := pkcePair()
	code, _ := authorizeThroughCallback(t, ts, clientID, redirectURI, challenge, "s")
	tr, _, status := redeemCode(t, ts, clientID, redirectURI, code, verifier)
	require.Equal(t, http.StatusOK, status)
	return a, ts, tr, clientID
}

// failStore makes every grant read fail with err until the test ends.
func failStore(t *testing.T, a *AuthServer, err error) {
	t.Helper()
	base := a.store
	a.store = failingStateStore{oauthStateStore: base, err: err}
	t.Cleanup(func() { a.store = base })
}

// captureLogs routes a's logs into the returned buffer until the test ends.
func captureLogs(t *testing.T, a *AuthServer) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	base := a.logger
	a.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { a.logger = base })
	return &buf
}

func TestRequireBearerToken(t *testing.T) {
	a, _, tr, _ := issuedTokens(t)
	opts := &auth.RequireBearerTokenOptions{ResourceMetadataURL: testIssuer + ProtectedResourceMetadataPath}
	serve := func(t *testing.T, token string) (*httptest.ResponseRecorder, *auth.TokenInfo, bool) {
		t.Helper()
		var info *auth.TokenInfo
		called := false
		handler := a.RequireBearerToken(opts)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			called = true
			info = auth.TokenInfoFromContext(r.Context())
		}))
		req := httptest.NewRequest(http.MethodPost, testIssuer, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec, info, called
	}
	expire := func(t *testing.T) {
		a.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
		t.Cleanup(func() { a.now = time.Now })
	}
	assertUnauthorized := func(t *testing.T, rec *httptest.ResponseRecorder, called bool) {
		t.Helper()
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Contains(t, rec.Header().Get("WWW-Authenticate"), "resource_metadata=")
		assert.False(t, called)
	}
	assertUnavailable := func(t *testing.T, rec *httptest.ResponseRecorder, called bool) {
		t.Helper()
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Equal(t, "5", rec.Header().Get("Retry-After"))
		assert.False(t, called)
	}

	t.Run("valid token reaches next with token info", func(t *testing.T) {
		rec, info, called := serve(t, tr.AccessToken)
		assert.Equal(t, http.StatusOK, rec.Code)
		require.True(t, called)
		require.NotNil(t, info)
		assert.Equal(t, "sub-123", info.UserID)
		extra, ok := info.Extra[extraIdentityKey].(identityExtra)
		require.True(t, ok)
		assert.Equal(t, "dev@example.com", extra.Email)
		assert.Equal(t, "ya29.user-token", extra.GoogleAccessToken)
	})
	t.Run("garbage token is 401", func(t *testing.T) {
		rec, _, called := serve(t, "mcp_at_garbage")
		assertUnauthorized(t, rec, called)
	})
	t.Run("expired token is 401", func(t *testing.T) {
		expire(t)
		rec, _, called := serve(t, tr.AccessToken)
		assertUnauthorized(t, rec, called)
	})
	t.Run("store outage is 503 with retry-after", func(t *testing.T) {
		failStore(t, a, errors.New("firestore unavailable"))
		rec, _, called := serve(t, tr.AccessToken)
		assertUnavailable(t, rec, called)
	})
	t.Run("expired token during store outage is 503", func(t *testing.T) {
		// The grant is read before the expiry check (verifyAccessToken).
		expire(t)
		failStore(t, a, errors.New("firestore unavailable"))
		rec, _, called := serve(t, tr.AccessToken)
		assertUnavailable(t, rec, called)
	})
	t.Run("corrupt grant is 401, not 503", func(t *testing.T) {
		logs := captureLogs(t, a)
		failStore(t, a, corruptGrantError(slog.New(slog.DiscardHandler), "f", errors.New("bad field")))
		rec, _, called := serve(t, tr.AccessToken)
		assertUnauthorized(t, rec, called)
		assert.NotContains(t, logs.String(), "oauth_store_failure")
	})
}

// TestCorruptGrantIsRejectedEverywhere pins that an undecodable grant record
// is treated as a dead grant by the refresh and revoke endpoints too, not as
// a store outage the client should retry.
func TestCorruptGrantIsRejectedEverywhere(t *testing.T) {
	a, ts, tr, clientID := issuedTokens(t)
	failStore(t, a, corruptGrantError(slog.New(slog.DiscardHandler), "f", errors.New("bad field")))

	_, oe, status := refreshGrant(t, ts.URL, tr.RefreshToken, clientID)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "invalid_grant", oe.Error)

	resp, err := http.PostForm(ts.URL+"/revoke", url.Values{"token": {tr.RefreshToken}})
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestStoreFailureLogLevel pins that a store call failing because the client
// went away is not reported as a store failure at Error level.
func TestStoreFailureLogLevel(t *testing.T) {
	a, _, tr, _ := issuedTokens(t)
	failStore(t, a, context.Canceled)

	t.Run("client canceled", func(t *testing.T) {
		logs := captureLogs(t, a)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := a.verifyAccessToken(ctx, tr.AccessToken)
		require.ErrorIs(t, err, errStoreUnavailable)
		assert.Contains(t, logs.String(), "level=DEBUG msg=oauth_store_failure")
		assert.NotContains(t, logs.String(), "level=ERROR")
	})
	t.Run("live request", func(t *testing.T) {
		logs := captureLogs(t, a)
		_, err := a.verifyAccessToken(context.Background(), tr.AccessToken)
		require.ErrorIs(t, err, errStoreUnavailable)
		assert.Contains(t, logs.String(), "level=ERROR msg=oauth_store_failure")
	})
}
