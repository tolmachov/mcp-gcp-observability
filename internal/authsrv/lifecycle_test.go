package authsrv

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// readAll drains and closes a response body.
func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return string(body)
}

// refreshGrant posts a refresh_token grant and decodes the outcome.
func refreshGrant(t *testing.T, tsURL, refreshToken, clientID string) (*tokenResponse, *oauthErrorResponse, int) {
	t.Helper()
	resp, err := http.PostForm(tsURL+"/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
	})
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	if resp.StatusCode == http.StatusOK {
		var tr tokenResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&tr))
		return &tr, nil, resp.StatusCode
	}
	var oe oauthErrorResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&oe))
	return nil, &oe, resp.StatusCode
}

// TestRefreshTokenTTLIsAbsolute pins the anchoring rule: the refresh-token
// TTL counts from the ORIGINAL login, not from the last refresh. If a
// refactor ever passes "now" as LoginAt on the refresh path, sessions become
// infinitely renewable and this test fails.
func TestRefreshTokenTTLIsAbsolute(t *testing.T) {
	idp := happyIdP()
	// The refreshed Google token must stay valid at whatever "now" the
	// server has been advanced to; give it a far-future expiry.
	idp.refreshTok = &oauth2.Token{AccessToken: "ya29.refreshed", Expiry: time.Now().Add(100 * 24 * time.Hour)}
	idp.googleToken.Expiry = time.Now().Add(100 * 24 * time.Hour)
	a, ts := newTestServer(t, testConfig(t), idp)
	const redirectURI = "http://localhost:41234/callback"
	clientID := registerClient(t, ts, redirectURI)
	verifier, challenge := pkcePair()
	code, _ := authorizeThroughCallback(t, ts, clientID, redirectURI, challenge, "s")
	tr, _, status := redeemCode(t, ts, clientID, redirectURI, code, verifier)
	require.Equal(t, http.StatusOK, status)

	loginTime := time.Now()
	ttl := a.cfg.refreshTokenTTL()

	// Just short of the TTL: refresh succeeds and yields a NEW refresh token.
	a.now = func() time.Time { return loginTime.Add(ttl - time.Hour) }
	t.Cleanup(func() { a.now = time.Now })
	refreshed, _, status := refreshGrant(t, ts.URL, tr.RefreshToken, clientID)
	require.Equal(t, http.StatusOK, status)
	require.NotEmpty(t, refreshed.RefreshToken)

	// Past the ORIGINAL login TTL: the freshly issued refresh token must be
	// rejected too — refreshing must not have extended the session.
	a.now = func() time.Time { return loginTime.Add(ttl + time.Hour) }
	_, oe, status := refreshGrant(t, ts.URL, refreshed.RefreshToken, clientID)
	require.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "invalid_grant", oe.Error)
	assert.Contains(t, oe.ErrorDescription, "expired")
}

// TestMintCapsAccessTokenAtGoogleExpiry pins the rule that makes the static
// per-request TokenSource sound: our access token never outlives the Google
// token embedded in it.
func TestMintCapsAccessTokenAtGoogleExpiry(t *testing.T) {
	t.Run("short google expiry caps expires_in", func(t *testing.T) {
		idp := happyIdP()
		idp.googleToken.Expiry = time.Now().Add(5 * time.Minute)
		_, ts := newTestServer(t, testConfig(t), idp)
		const redirectURI = "http://localhost:41234/callback"
		clientID := registerClient(t, ts, redirectURI)
		verifier, challenge := pkcePair()
		code, _ := authorizeThroughCallback(t, ts, clientID, redirectURI, challenge, "s")
		tr, _, status := redeemCode(t, ts, clientID, redirectURI, code, verifier)
		require.Equal(t, http.StatusOK, status)
		assert.LessOrEqual(t, tr.ExpiresIn, int64(5*60))
		assert.Greater(t, tr.ExpiresIn, int64(4*60))
	})
	t.Run("already-expired google token is rejected", func(t *testing.T) {
		idp := happyIdP()
		idp.googleToken.Expiry = time.Now().Add(-time.Minute)
		_, ts := newTestServer(t, testConfig(t), idp)
		const redirectURI = "http://localhost:41234/callback"
		clientID := registerClient(t, ts, redirectURI)
		verifier, challenge := pkcePair()
		code, _ := authorizeThroughCallback(t, ts, clientID, redirectURI, challenge, "s")
		_, oe, status := redeemCode(t, ts, clientID, redirectURI, code, verifier)
		require.Equal(t, http.StatusBadRequest, status)
		assert.Equal(t, "invalid_grant", oe.Error)
	})
}

// TestResourceParameterFlow exercises RFC 8707 resource binding the way real
// MCP clients do: resource sent at both /authorize and /token.
func TestResourceParameterFlow(t *testing.T) {
	run := func(t *testing.T, authorizeRes, tokenRes string, wantStatus int) {
		t.Helper()
		_, ts := newTestServer(t, testConfig(t), happyIdP())
		const redirectURI = "http://localhost:41234/callback"
		clientID := registerClient(t, ts, redirectURI)
		verifier, challenge := pkcePair()
		client := noRedirectClient()

		v := url.Values{
			"client_id":             {clientID},
			"redirect_uri":          {redirectURI},
			"response_type":         {"code"},
			"code_challenge":        {challenge},
			"code_challenge_method": {"S256"},
			"resource":              {authorizeRes},
		}
		resp, err := client.Get(ts.URL + "/authorize?" + v.Encode())
		require.NoError(t, err)
		page := readAll(t, resp)
		require.Equal(t, http.StatusOK, resp.StatusCode, "authorize should accept resource %q: %s", authorizeRes, page)
		m := consentRequestRe.FindStringSubmatch(page)
		require.NotNil(t, m)

		resp, err = client.PostForm(ts.URL+"/authorize/confirm", url.Values{"request": {m[1]}})
		require.NoError(t, err)
		_ = resp.Body.Close()
		googleURL, err := url.Parse(resp.Header.Get("Location"))
		require.NoError(t, err)
		resp, err = client.Get(ts.URL + "/callback?" + url.Values{
			"state": {googleURL.Query().Get("state")}, "code": {"google-code"},
		}.Encode())
		require.NoError(t, err)
		_ = resp.Body.Close()
		loc, err := url.Parse(resp.Header.Get("Location"))
		require.NoError(t, err)
		require.Empty(t, loc.Query().Get("error"))

		form := url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {loc.Query().Get("code")},
			"client_id":     {clientID},
			"redirect_uri":  {redirectURI},
			"code_verifier": {verifier},
			"resource":      {tokenRes},
		}
		tokResp, err := http.PostForm(ts.URL+"/token", form)
		require.NoError(t, err)
		defer tokResp.Body.Close() //nolint:errcheck // test cleanup
		assert.Equal(t, wantStatus, tokResp.StatusCode)
	}

	t.Run("issuer resource accepted end to end", func(t *testing.T) {
		run(t, testIssuer, testIssuer, http.StatusOK)
	})
	t.Run("trailing slash normalized", func(t *testing.T) {
		run(t, testIssuer+"/", testIssuer+"/", http.StatusOK)
	})
	t.Run("foreign resource at token rejected", func(t *testing.T) {
		run(t, testIssuer, "https://other.example", http.StatusBadRequest)
	})
}

// TestLoopbackPortMayDifferThroughToken runs the flow exactly like a native
// client (Claude Code, Cursor): the redirect_uri port at /authorize differs
// from the registered one (RFC 8252 §7.3), and /token receives the same
// per-flow URI.
func TestLoopbackPortMayDifferThroughToken(t *testing.T) {
	_, ts := newTestServer(t, testConfig(t), happyIdP())
	clientID := registerClient(t, ts, "http://localhost:41234/callback")
	verifier, challenge := pkcePair()

	const flowRedirect = "http://localhost:59999/callback"
	code, _ := authorizeThroughCallback(t, ts, clientID, flowRedirect, challenge, "s")
	tr, _, status := redeemCode(t, ts, clientID, flowRedirect, code, verifier)
	require.Equal(t, http.StatusOK, status)
	assert.NotEmpty(t, tr.AccessToken)
}

// TestAuthorizeConfirmRejections covers the CSRF boundary of the consent
// form: only a sealed, unexpired request blob may proceed to Google.
func TestAuthorizeConfirmRejections(t *testing.T) {
	a, ts := newTestServer(t, testConfig(t), happyIdP())
	client := noRedirectClient()

	t.Run("forged blob renders error, no redirect", func(t *testing.T) {
		resp, err := client.PostForm(ts.URL+"/authorize/confirm", url.Values{"request": {"forged"}})
		require.NoError(t, err)
		body := readAll(t, resp)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Empty(t, resp.Header.Get("Location"))
		assert.Contains(t, body, "Invalid request")
	})
	t.Run("expired blob renders distinct error", func(t *testing.T) {
		blob, err := sealBlob(a.sealer, stateBlob, stateClaims{
			ClientID: "c", RedirectURI: "http://localhost/cb", CodeChallenge: "x",
			IssuedAt: time.Now().Add(-2 * stateTTL).Unix(),
		})
		require.NoError(t, err)
		resp, err := client.PostForm(ts.URL+"/authorize/confirm", url.Values{"request": {blob}})
		require.NoError(t, err)
		body := readAll(t, resp)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Contains(t, body, "Request expired")
	})
	t.Run("missing blob renders error", func(t *testing.T) {
		resp, err := client.PostForm(ts.URL+"/authorize/confirm", url.Values{})
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
}

// TestRefreshRotationPassthrough pins the handling of Google's occasional
// refresh-token rotation: a rotated token must be carried into the new
// sealed refresh token; without rotation the original is kept.
func TestRefreshRotationPassthrough(t *testing.T) {
	idp := happyIdP()
	idp.refreshTok = &oauth2.Token{
		AccessToken:  "ya29.refreshed",
		RefreshToken: "1//rotated",
		Expiry:       time.Now().Add(time.Hour),
	}
	_, ts := newTestServer(t, testConfig(t), idp)
	const redirectURI = "http://localhost:41234/callback"
	clientID := registerClient(t, ts, redirectURI)
	verifier, challenge := pkcePair()
	code, _ := authorizeThroughCallback(t, ts, clientID, redirectURI, challenge, "s")
	tr, _, status := redeemCode(t, ts, clientID, redirectURI, code, verifier)
	require.Equal(t, http.StatusOK, status)

	// First refresh: server presents the original Google refresh token and
	// receives a rotated one.
	first, _, status := refreshGrant(t, ts.URL, tr.RefreshToken, clientID)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, []string{"1//refresh"}, idp.refreshedWith)

	// Second refresh: the sealed token must now carry the ROTATED value.
	idp.refreshTok.RefreshToken = "" // no further rotation
	_, _, status = refreshGrant(t, ts.URL, first.RefreshToken, clientID)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, []string{"1//refresh", "1//rotated"}, idp.refreshedWith)
}

// TestRefreshRejectsRemovedDomain: the token endpoint itself enforces the
// allowlist, so a removed domain stops minting tokens instead of relying
// solely on the verifier.
func TestRefreshRejectsRemovedDomain(t *testing.T) {
	a, ts := newTestServer(t, testConfig(t), happyIdP())
	const redirectURI = "http://localhost:41234/callback"
	clientID := registerClient(t, ts, redirectURI)
	verifier, challenge := pkcePair()
	code, _ := authorizeThroughCallback(t, ts, clientID, redirectURI, challenge, "s")
	tr, _, status := redeemCode(t, ts, clientID, redirectURI, code, verifier)
	require.Equal(t, http.StatusOK, status)

	a.cfg.AllowedDomains = []string{"other.example"}
	t.Cleanup(func() { a.cfg.AllowedDomains = []string{"example.com"} })
	_, oe, status := refreshGrant(t, ts.URL, tr.RefreshToken, clientID)
	require.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "invalid_grant", oe.Error)
}

// TestRevokeVariants covers the access-token branch and the transient
// upstream failure (RFC 7009 §2.2.1: 503, the client retries).
func TestRevokeVariants(t *testing.T) {
	setup := func(t *testing.T, idp *fakeIdP) (tsURL string, tr *tokenResponse) {
		t.Helper()
		_, ts := newTestServer(t, testConfig(t), idp)
		const redirectURI = "http://localhost:41234/callback"
		clientID := registerClient(t, ts, redirectURI)
		verifier, challenge := pkcePair()
		code, _ := authorizeThroughCallback(t, ts, clientID, redirectURI, challenge, "s")
		tr, _, status := redeemCode(t, ts, clientID, redirectURI, code, verifier)
		require.Equal(t, http.StatusOK, status)
		return ts.URL, tr
	}

	t.Run("access token revokes the embedded google access token", func(t *testing.T) {
		idp := happyIdP()
		tsURL, tr := setup(t, idp)
		resp, err := http.PostForm(tsURL+"/revoke", url.Values{"token": {tr.AccessToken}})
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, []string{"ya29.user-token"}, idp.revoked)
	})
	t.Run("transient upstream failure yields 503 with retry-after", func(t *testing.T) {
		idp := happyIdP()
		idp.revokeErr = assert.AnError
		tsURL, tr := setup(t, idp)
		resp, err := http.PostForm(tsURL+"/revoke", url.Values{"token": {tr.RefreshToken}})
		require.NoError(t, err)
		defer resp.Body.Close() //nolint:errcheck // test cleanup
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		assert.NotEmpty(t, resp.Header.Get("Retry-After"))
		var oe oauthErrorResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&oe))
		assert.Equal(t, "temporarily_unavailable", oe.Error)
	})
}
