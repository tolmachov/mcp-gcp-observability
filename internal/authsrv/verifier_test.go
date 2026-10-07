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

	"github.com/tolmachov/mcp-gcp-observability/internal/httpdiag/httpdiagtest"
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

// rejectionReason returns the reason class of a verifier token rejection.
func rejectionReason(t *testing.T, err error) string {
	t.Helper()
	var rejected *tokenRejection
	require.ErrorAs(t, err, &rejected)
	return rejected.reason
}

func TestRequireBearerToken(t *testing.T) {
	a, _, tr, _ := issuedTokens(t)
	type served struct {
		rec    *httptest.ResponseRecorder
		info   *auth.TokenInfo // nil unless next was reached
		reason string          // the httpdiag rejection reason, "" when not rejected
	}
	serve := func(t *testing.T, authorization string) served {
		t.Helper()
		var out served
		handler := a.RequireBearerToken()(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			out.info = auth.TokenInfoFromContext(r.Context())
		}))
		req := httptest.NewRequest(http.MethodPost, testIssuer, nil)
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		out.rec, out.reason = httpdiagtest.Serve(t, handler, req)
		return out
	}
	bearer := func(token string) string { return "Bearer " + token }
	expire := func(t *testing.T) {
		a.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
		t.Cleanup(func() { a.now = time.Now })
	}
	assertUnauthorized := func(t *testing.T, got served, reason string) {
		t.Helper()
		assert.Equal(t, http.StatusUnauthorized, got.rec.Code)
		assert.Contains(t, got.rec.Header().Get("WWW-Authenticate"), "resource_metadata=")
		assert.Nil(t, got.info)
		assert.Equal(t, reason, got.reason)
	}
	assertUnavailable := func(t *testing.T, got served) {
		t.Helper()
		assert.Equal(t, http.StatusServiceUnavailable, got.rec.Code)
		assert.Equal(t, "5", got.rec.Header().Get("Retry-After"))
		assert.Nil(t, got.info)
		assert.Equal(t, "oauth_store_unavailable", got.reason)
	}

	t.Run("valid token reaches next with token info", func(t *testing.T) {
		got := serve(t, bearer(tr.AccessToken))
		assert.Equal(t, http.StatusOK, got.rec.Code)
		require.NotNil(t, got.info)
		assert.Equal(t, "sub-123", got.info.UserID)
		extra, ok := got.info.Extra[extraIdentityKey].(identityExtra)
		require.True(t, ok)
		assert.Equal(t, "dev@example.com", extra.Email)
		assert.Equal(t, "ya29.user-token", extra.GoogleAccessToken)
		assert.Empty(t, got.reason)
	})
	t.Run("missing or non-bearer header is 401", func(t *testing.T) {
		assertUnauthorized(t, serve(t, ""), "missing_bearer_token")
		assertUnauthorized(t, serve(t, "Basic "+tr.AccessToken), "missing_bearer_token")
	})
	t.Run("garbage token is 401", func(t *testing.T) {
		assertUnauthorized(t, serve(t, bearer("mcp_at_garbage")), "invalid_access_token")
	})
	t.Run("expired token is 401", func(t *testing.T) {
		expire(t)
		assertUnauthorized(t, serve(t, bearer(tr.AccessToken)), "access_token_expired")
	})
	t.Run("store outage is 503 with retry-after", func(t *testing.T) {
		failStore(t, a, errors.New("firestore unavailable"))
		assertUnavailable(t, serve(t, bearer(tr.AccessToken)))
	})
	t.Run("expired token during store outage is 503", func(t *testing.T) {
		// The grant is read before the expiry check (verifyAccessToken).
		expire(t)
		failStore(t, a, errors.New("firestore unavailable"))
		assertUnavailable(t, serve(t, bearer(tr.AccessToken)))
	})
	t.Run("corrupt grant is 401, not 503", func(t *testing.T) {
		logs := captureLogs(t, a)
		failStore(t, a, corruptGrantError(slog.New(slog.DiscardHandler), "f", errors.New("bad field")))
		assertUnauthorized(t, serve(t, bearer(tr.AccessToken)), "grant_not_found")
		assert.NotContains(t, logs.String(), "oauth_store_failure")
	})
}

// TestRequireBearerTokenExpiryIsTheVerifiers pins that the verifier's clock
// alone decides expiry: the SDK middleware never re-checks a token the
// verifier accepted against the wall clock.
func TestRequireBearerTokenExpiryIsTheVerifiers(t *testing.T) {
	a, ts := newTestServer(t, testConfig(t), happyIdP())
	// Issue and verify on a clock that runs behind the wall clock, so the
	// token is still valid for the verifier but expired by the wall clock.
	a.now = func() time.Time { return time.Now().Add(-accessTokenTTL - time.Hour) }
	const redirectURI = "http://localhost:41234/callback"
	clientID := registerClient(t, ts, redirectURI)
	verifier, challenge := pkcePair()
	code, _ := authorizeThroughCallback(t, ts, clientID, redirectURI, challenge, "s")
	tr, _, status := redeemCode(t, ts, clientID, redirectURI, code, verifier)
	require.Equal(t, http.StatusOK, status)

	called := false
	handler := a.RequireBearerToken()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	req := httptest.NewRequest(http.MethodPost, testIssuer, nil)
	req.Header.Set("Authorization", "Bearer "+tr.AccessToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, called)
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
