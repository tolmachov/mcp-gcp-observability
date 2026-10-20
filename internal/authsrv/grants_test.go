package authsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/tolmachov/mcp-gcp-observability/internal/httpdiag/httpdiagtest"
)

// A lifecycle retained before test dependencies change must still consult the
// owner's current store, clock, config and logger.
func TestGrantLifecycleUsesCurrentOwnerDependencies(t *testing.T) {
	a, err := NewWithProvider(testConfig(t), nil, happyIdP())
	require.NoError(t, err)
	grants := a.grants()
	now := time.Now().UTC().Truncate(time.Second).Add(2 * time.Hour)
	a.now = func() time.Time { return now }
	a.cfg.RefreshTokenTTL = 2 * time.Hour
	store := newMemoryStateStore()
	a.store = store
	verifier, challenge := pkcePair()
	cc := codeClaims{
		Subject: "subject", Email: "dev@example.com", Domain: "example.com", ClientID: "client",
		RedirectURI: "http://localhost/callback", CodeChallenge: challenge, Resource: testIssuer,
		Scopes: []string{"scope"}, GoogleAccessToken: "google-access", GoogleRefreshToken: "google-refresh",
		GoogleExpiry: now.Add(time.Hour).Unix(), IssuedAt: now.Unix(),
	}
	require.NoError(t, store.PutCode(context.Background(), tokenHash("code"), codeRecord{
		Claims: sealBlob(a.sealer, storedCodeBlob, cc), Status: "active", FamilyID: "family", ExpiresAt: now.Add(time.Minute),
	}))
	tokens, err := grants.ExchangeCode(context.Background(), codeExchange{
		Code: "code", ClientID: "client", RedirectURI: cc.RedirectURI, CodeVerifier: verifier, Resource: testIssuer,
	})
	require.NoError(t, err)
	rec, err := store.GetGrant(context.Background(), "family")
	require.NoError(t, err)
	assert.Equal(t, now.Add(2*time.Hour), rec.ExpiresAt)
	assert.Equal(t, "subject", tokens.Subject)
	assert.Equal(t, "google-access", tokens.GoogleAccessToken)

	a.cfg.AllowedDomains = []string{"other.example"}
	_, err = grants.PrepareRefresh(context.Background(), tokens.RefreshToken, "client")
	var rejected *grantRejection
	require.ErrorAs(t, err, &rejected)
	assert.Equal(t, "domain_not_allowed", rejected.reason)

	logs := captureLogs(t, a)
	a.store = failingStateStore{oauthStateStore: store, err: assert.AnError}
	access := sealBlob(a.sealer, accessBlob, accessClaims{FamilyID: "family", ExpiresAt: now.Add(time.Hour).Unix(), IssuedAt: now.Unix()})
	_, err = grants.VerifyAccess(context.Background(), access)
	require.ErrorIs(t, err, errStoreUnavailable)
	assert.Contains(t, logs.String(), "operation=verify_grant")
}

func TestGrantLifecycleRefreshKeepsPreparedTimeAndClaims(t *testing.T) {
	a, err := NewWithProvider(testConfig(t), nil, happyIdP())
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Second)
	a.now = func() time.Time { return now }
	claims := refreshClaims{
		Subject: "subject", Email: "dev@example.com", Domain: "example.com", ClientID: "client", Resource: testIssuer,
		Scopes: []string{"scope-a", "scope-b"}, GoogleRefreshToken: "original-google-refresh", IssuedAt: now.Add(-time.Hour).Unix(),
	}
	store, ok := a.store.(*memoryStateStore)
	require.True(t, ok)
	rec := testGrant(now, "current")
	rec.Claims = sealBlob(a.sealer, storedGrantBlob, claims)
	store.grants["family"] = rec
	prepared, err := a.grants().PrepareRefresh(context.Background(), prefixRefresh+"family.current", "client")
	require.NoError(t, err)
	assert.Equal(t, "original-google-refresh", prepared.GoogleRefreshToken())

	// The upstream call can finish after expiry. The same operation-start
	// time must govern preparation, rotation and updated_at, as before.
	a.now = func() time.Time { return now.Add(2 * time.Hour) }
	tokens, err := a.grants().RotateRefresh(context.Background(), prepared, "replacement-google-refresh")
	require.NoError(t, err)
	next, err := store.GetGrant(context.Background(), "family")
	require.NoError(t, err)
	assert.Equal(t, rec.ExpiresAt, next.ExpiresAt)
	assert.Equal(t, now, next.UpdatedAt)
	assert.Equal(t, int64(2), next.Generation)
	assert.Equal(t, "active", next.Status)
	opened, err := openBlob(a.sealer, storedGrantBlob, next.Claims, now)
	require.NoError(t, err)
	claims.GoogleRefreshToken = "replacement-google-refresh"
	assert.Equal(t, claims, opened)
	assert.Equal(t, "subject", tokens.Subject)
	assert.Equal(t, []string{"scope-a", "scope-b"}, tokens.Scopes)
	id, secret, err := parseRefreshToken(tokens.RefreshToken)
	require.NoError(t, err)
	assert.Equal(t, "family", id)
	assert.True(t, secretMatches(secret, next.ActiveSecretHash))
	_, err = a.grants().PrepareRefresh(context.Background(), tokens.RefreshToken, "client")
	var rejected *grantRejection
	require.ErrorAs(t, err, &rejected)
	assert.Equal(t, "grant_inactive", rejected.reason)
}

type clockAdvancingIdP struct {
	*fakeIdP
	advance func()
}

func (p clockAdvancingIdP) Refresh(ctx context.Context, refresh string) (*oauth2.Token, error) {
	p.advance()
	return p.fakeIdP.Refresh(ctx, refresh)
}

func TestRefreshHTTPKeepsTimeCapturedBeforeUpstream(t *testing.T) {
	a, _, tr, clientID := issuedTokens(t)
	id, _, err := parseRefreshToken(tr.RefreshToken)
	require.NoError(t, err)
	before, err := a.store.GetGrant(context.Background(), id)
	require.NoError(t, err)
	started := before.ExpiresAt.Add(-time.Second)
	finished := before.ExpiresAt.Add(time.Second)
	a.now = func() time.Time { return started }
	idp := happyIdP()
	idp.refreshTok.Expiry = finished.Add(time.Hour)
	a.idp = clockAdvancingIdP{fakeIdP: idp, advance: func() { a.now = func() time.Time { return finished } }}
	r := formRequest(TokenPath, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tr.RefreshToken}, "client_id": {clientID}})
	w := httptest.NewRecorder()
	a.handleToken(w, r)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var minted tokenResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &minted))
	access, err := openBlob(a.sealer, accessBlob, minted.AccessToken, finished)
	require.NoError(t, err)
	assert.Equal(t, finished.Unix(), access.IssuedAt, "minting must use a fresh clock after the upstream call")
	assert.Greater(t, access.ExpiresAt, finished.Unix())
	after, err := a.store.GetGrant(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, started, after.UpdatedAt)
	assert.Equal(t, before.ExpiresAt, after.ExpiresAt)
}

type grantOperationFailureStore struct {
	oauthStateStore
	operation string
}

func (s grantOperationFailureStore) GetCode(ctx context.Context, key string) (codeRecord, error) {
	if s.operation == "get_code" {
		return codeRecord{}, assert.AnError
	}
	return s.oauthStateStore.GetCode(ctx, key)
}

func (s grantOperationFailureStore) RedeemCode(ctx context.Context, key string, now time.Time, rec grantRecord) error {
	if s.operation == "redeem_code" {
		return assert.AnError
	}
	return s.oauthStateStore.RedeemCode(ctx, key, now, rec)
}

func (s grantOperationFailureStore) GetGrant(ctx context.Context, key string) (grantRecord, error) {
	if s.operation == "get_grant" || s.operation == "revoke_get_grant" {
		return grantRecord{}, assert.AnError
	}
	return s.oauthStateStore.GetGrant(ctx, key)
}

func (s grantOperationFailureStore) RotateGrant(ctx context.Context, id, expected string, rec grantRecord, now time.Time) error {
	if s.operation == "rotate_grant" || s.operation == "revoke_replayed_grant" {
		return assert.AnError
	}
	return s.oauthStateStore.RotateGrant(ctx, id, expected, rec, now)
}

func (s grantOperationFailureStore) RevokeGrantIfSecret(ctx context.Context, id, expected string, now time.Time) (bool, error) {
	if s.operation == "revoke_grant" {
		return false, assert.AnError
	}
	return s.oauthStateStore.RevokeGrantIfSecret(ctx, id, expected, now)
}

func formRequest(path string, form url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func TestGrantLifecycleStoreFailureOperationsThroughHTTP(t *testing.T) {
	for _, operation := range []string{"get_code", "redeem_code", "get_grant", "revoke_replayed_grant", "rotate_grant", "revoke_get_grant", "revoke_grant"} {
		t.Run(operation, func(t *testing.T) {
			a, _, tr, clientID := issuedTokens(t)
			logs := captureLogs(t, a)
			form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tr.RefreshToken}, "client_id": {clientID}}
			path := TokenPath
			if operation == "get_code" || operation == "redeem_code" {
				verifier, challenge := pkcePair()
				now := a.now()
				require.NoError(t, a.store.PutCode(context.Background(), tokenHash("fresh-code"), codeRecord{
					Claims: sealBlob(a.sealer, storedCodeBlob, codeClaims{ClientID: clientID, RedirectURI: "http://localhost/callback", CodeChallenge: challenge, IssuedAt: now.Unix()}),
					Status: "active", FamilyID: "fresh-family", ExpiresAt: now.Add(time.Minute),
				}))
				form = url.Values{"grant_type": {"authorization_code"}, "code": {"fresh-code"}, "client_id": {clientID}, "redirect_uri": {"http://localhost/callback"}, "code_verifier": {verifier}}
			} else if operation == "revoke_replayed_grant" {
				family, _, err := parseRefreshToken(tr.RefreshToken)
				require.NoError(t, err)
				form.Set("refresh_token", prefixRefresh+family+".forged")
			} else if strings.HasPrefix(operation, "revoke_") {
				path = RevokePath
				form = url.Values{"token": {tr.RefreshToken}}
			}
			a.store = grantOperationFailureStore{oauthStateStore: a.store, operation: operation}
			handler := http.HandlerFunc(a.handleToken)
			if path == RevokePath {
				handler = a.handleRevoke
			}
			rec, reason := httpdiagtest.Serve(t, handler, formRequest(path, form))
			assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
			assert.Equal(t, "5", rec.Header().Get("Retry-After"))
			assert.Equal(t, "oauth_store_unavailable", reason)
			assert.Contains(t, logs.String(), "operation="+operation)
		})
	}
}

func TestGrantLifecycleCodeReplayPrecedesExpiryAndIgnoresReplayStoreFailure(t *testing.T) {
	a, err := NewWithProvider(testConfig(t), nil, happyIdP())
	require.NoError(t, err)
	now := time.Now()
	store := newMemoryStateStore()
	require.NoError(t, store.PutCode(context.Background(), tokenHash("used"), codeRecord{Status: "used", FamilyID: "family", ExpiresAt: now.Add(-time.Hour)}))
	a.store = grantOperationFailureStore{oauthStateStore: store, operation: "redeem_code"}
	_, err = a.grants().ExchangeCode(context.Background(), codeExchange{Code: "used"})
	var rejected *grantRejection
	require.ErrorAs(t, err, &rejected)
	assert.Equal(t, "authorization_code_reused", rejected.reason)
	assert.False(t, errors.Is(err, assert.AnError))
}

func TestGrantTokenErrorRejectsUnexpectedFailureWithoutDisclosure(t *testing.T) {
	a, err := NewWithProvider(testConfig(t), nil, happyIdP())
	require.NoError(t, err)
	secret := "submitted-credential-must-not-leak"
	failure := fmt.Errorf("unexpected failure: %w", errors.New(secret))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.grantTokenError(r.Context(), w, failure)
	})
	w, reason := httpdiagtest.Serve(t, handler, httptest.NewRequest(http.MethodPost, TokenPath, nil))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, "unexpected_grant_failure", reason)
	assert.Contains(t, w.Body.String(), `"error":"server_error"`)
	assert.NotContains(t, w.Body.String(), secret)
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
}
