package authsrv

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

const testIssuer = "https://issuer.example"

// fakeIdP is a scriptable IdentityProvider.
type fakeIdP struct {
	claims        *IdentityClaims
	googleToken   *oauth2.Token
	grantedScope  string // scope string in the token response; "" = all requested
	exchangeErr   error
	refreshTok    *oauth2.Token
	refreshErr    error
	refreshedWith []string
	revoked       []string
	revokeErr     error
}

func (f *fakeIdP) AuthCodeURL(state string, _ ...oauth2.AuthCodeOption) string {
	return "https://accounts.google.example/auth?state=" + url.QueryEscape(state)
}

func (f *fakeIdP) Exchange(context.Context, string) (*oauth2.Token, error) {
	if f.exchangeErr != nil {
		return nil, f.exchangeErr
	}
	scope := f.grantedScope
	if scope == "" {
		scope = "openid email https://www.googleapis.com/auth/cloud-platform"
	}
	return f.googleToken.WithExtra(map[string]any{"id_token": "raw-id-token", "scope": scope}), nil
}

func (f *fakeIdP) Refresh(_ context.Context, refreshToken string) (*oauth2.Token, error) {
	f.refreshedWith = append(f.refreshedWith, refreshToken)
	if f.refreshErr != nil {
		return nil, f.refreshErr
	}
	return f.refreshTok, nil
}

func (f *fakeIdP) ValidateIDToken(context.Context, string) (*IdentityClaims, error) {
	if f.claims == nil {
		return nil, fmt.Errorf("no identity")
	}
	return f.claims, nil
}

func (f *fakeIdP) Revoke(_ context.Context, token string) error {
	if f.revokeErr != nil {
		return f.revokeErr
	}
	f.revoked = append(f.revoked, token)
	return nil
}

func testConfig(t *testing.T) *Config {
	t.Helper()
	return &Config{
		IssuerURL:          testIssuer,
		GoogleClientID:     "google-client",
		GoogleClientSecret: "google-secret",
		AllowedDomains:     []string{"example.com"},
		TokenKeys:          []string{testKey(t)},
	}
}

// newTestServer wires an AuthServer with the fake IdP and returns an
// httptest server with the auth routes mounted. The rate limiter is made
// effectively unlimited so protocol tests are not coupled to the budget.
func newTestServer(t *testing.T, cfg *Config, idp *fakeIdP) (*AuthServer, *httptest.Server) {
	t.Helper()
	a, err := NewWithProvider(cfg, slog.New(slog.DiscardHandler), idp)
	require.NoError(t, err)
	a.limiter = newIPRateLimiter(100000, 100000)

	mux := http.NewServeMux()
	a.Routes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return a, ts
}

func happyIdP() *fakeIdP {
	googleTok := &oauth2.Token{
		AccessToken:  "ya29.user-token",
		RefreshToken: "1//refresh",
		Expiry:       time.Now().Add(time.Hour),
	}
	return &fakeIdP{
		claims:      &IdentityClaims{Subject: "sub-123", Email: "dev@example.com", EmailVerified: true, HostedDomain: "example.com"},
		googleToken: googleTok,
		refreshTok:  &oauth2.Token{AccessToken: "ya29.refreshed", Expiry: time.Now().Add(time.Hour)},
	}
}

// noRedirectClient never follows redirects: the tests inspect Location.
func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func pkcePair() (verifier, challenge string) {
	verifier = "test-verifier-string-of-sufficient-length-42"
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// registerClient runs DCR and returns the issued client_id.
func registerClient(t *testing.T, ts *httptest.Server, redirectURIs ...string) string {
	return registerNamedClient(t, ts, "Test Client", redirectURIs...)
}

func registerNamedClient(t *testing.T, ts *httptest.Server, name string, redirectURIs ...string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"redirect_uris": redirectURIs,
		"client_name":   name,
	})
	require.NoError(t, err)
	resp, err := http.Post(ts.URL+"/register", "application/json", strings.NewReader(string(body)))
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var reg oauthex.ClientRegistrationResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&reg))
	require.True(t, strings.HasPrefix(reg.ClientID, prefixClientID))
	return reg.ClientID
}

var consentRequestRe = regexp.MustCompile(`name="request" value="([^"]+)"`)

// authorizeThroughCallback drives /authorize → consent → Google redirect →
// /callback and returns the authorization code delivered to redirectURI.
func authorizeThroughCallback(t *testing.T, ts *httptest.Server, clientID, redirectURI, challenge, state string) (code, gotState string) {
	t.Helper()
	client := noRedirectClient()

	authorizeURL := ts.URL + "/authorize?" + url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}.Encode()
	resp, err := client.Get(authorizeURL)
	require.NoError(t, err)
	page, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "authorize should render consent: %s", page)
	m := consentRequestRe.FindSubmatch(page)
	require.NotNil(t, m, "consent page should carry the sealed request")
	blob := string(m[1])

	resp, err = client.PostForm(ts.URL+"/authorize/confirm", url.Values{"request": {blob}})
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusFound, resp.StatusCode)
	googleURL, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	require.Equal(t, "accounts.google.example", googleURL.Host)
	sealedState := googleURL.Query().Get("state")
	require.NotEmpty(t, sealedState)

	resp, err = client.Get(ts.URL + "/callback?" + url.Values{
		"state": {sealedState},
		"code":  {"google-code"},
	}.Encode())
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusFound, resp.StatusCode)
	loc, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	require.Truef(t, strings.HasPrefix(loc.String(), redirectURI), "callback must redirect to the client, got %s", loc)
	require.Empty(t, loc.Query().Get("error"), "callback returned error: %s", loc.Query().Get("error_description"))
	return loc.Query().Get("code"), loc.Query().Get("state")
}

// redeemCode exchanges an authorization code at /token.
func redeemCode(t *testing.T, ts *httptest.Server, clientID, redirectURI, code, verifier string) (*tokenResponse, *oauthErrorResponse, int) {
	t.Helper()
	resp, err := http.PostForm(ts.URL+"/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
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

func TestMetadataEndpoints(t *testing.T) {
	_, ts := newTestServer(t, testConfig(t), happyIdP())

	var prm oauthex.ProtectedResourceMetadata
	resp, err := http.Get(ts.URL + "/.well-known/oauth-protected-resource")
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&prm))
	assert.Equal(t, testIssuer, prm.Resource)
	assert.Equal(t, []string{testIssuer}, prm.AuthorizationServers)

	var meta oauthex.AuthServerMeta
	resp2, err := http.Get(ts.URL + "/.well-known/oauth-authorization-server")
	require.NoError(t, err)
	defer resp2.Body.Close() //nolint:errcheck // test cleanup
	require.NoError(t, json.NewDecoder(resp2.Body).Decode(&meta))
	assert.Equal(t, testIssuer, meta.Issuer)
	assert.Equal(t, testIssuer+"/authorize", meta.AuthorizationEndpoint)
	assert.Equal(t, testIssuer+"/token", meta.TokenEndpoint)
	assert.Equal(t, testIssuer+"/register", meta.RegistrationEndpoint)
	assert.Equal(t, []string{"S256"}, meta.CodeChallengeMethodsSupported)
	assert.Equal(t, []string{"none"}, meta.TokenEndpointAuthMethodsSupported)

	resp3, err := http.Get(ts.URL + "/jwks.json")
	require.NoError(t, err)
	defer resp3.Body.Close() //nolint:errcheck // test cleanup
	body, err := io.ReadAll(resp3.Body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"keys":[]}`, string(body))
}

func TestFullAuthorizationFlow(t *testing.T) {
	a, ts := newTestServer(t, testConfig(t), happyIdP())
	const redirectURI = "http://localhost:41234/callback"

	clientID := registerClient(t, ts, redirectURI)
	verifier, challenge := pkcePair()
	code, gotState := authorizeThroughCallback(t, ts, clientID, redirectURI, challenge, "client-state-1")
	assert.Equal(t, "client-state-1", gotState)
	assert.True(t, strings.HasPrefix(code, prefixCode))

	tr, _, status := redeemCode(t, ts, clientID, redirectURI, code, verifier)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "Bearer", tr.TokenType)
	assert.True(t, strings.HasPrefix(tr.AccessToken, prefixAccess))
	assert.True(t, strings.HasPrefix(tr.RefreshToken, prefixRefresh))
	assert.Positive(t, tr.ExpiresIn)

	// The verifier accepts the access token and exposes the identity.
	info, err := a.Verifier()(context.Background(), tr.AccessToken, nil)
	require.NoError(t, err)
	assert.Equal(t, "sub-123", info.UserID)
	extra, ok := info.Extra[extraIdentityKey].(identityExtra)
	require.True(t, ok)
	assert.Equal(t, "dev@example.com", extra.Email)
	assert.Equal(t, "ya29.user-token", extra.GoogleAccessToken)
	assert.False(t, info.Expiration.IsZero())

	// Refresh grant yields a fresh pair wrapping the refreshed Google token.
	resp, err := http.PostForm(ts.URL+"/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {tr.RefreshToken},
		"client_id":     {clientID},
	})
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var refreshed tokenResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&refreshed))
	info2, err := a.Verifier()(context.Background(), refreshed.AccessToken, nil)
	require.NoError(t, err)
	extra2, ok := info2.Extra[extraIdentityKey].(identityExtra)
	require.True(t, ok)
	assert.Equal(t, "ya29.refreshed", extra2.GoogleAccessToken)
}

func TestAuthorizeRejections(t *testing.T) {
	_, ts := newTestServer(t, testConfig(t), happyIdP())
	const redirectURI = "http://localhost:41234/callback"
	clientID := registerClient(t, ts, redirectURI)
	_, challenge := pkcePair()
	client := noRedirectClient()

	get := func(v url.Values) *http.Response {
		resp, err := client.Get(ts.URL + "/authorize?" + v.Encode())
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp
	}
	base := func() url.Values {
		return url.Values{
			"client_id":             {clientID},
			"redirect_uri":          {redirectURI},
			"response_type":         {"code"},
			"code_challenge":        {challenge},
			"code_challenge_method": {"S256"},
		}
	}

	t.Run("bogus client_id renders error, no redirect", func(t *testing.T) {
		v := base()
		v.Set("client_id", "mcp_cid_forged.blob")
		resp := get(v)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Empty(t, resp.Header.Get("Location"))
	})
	t.Run("unregistered redirect_uri renders error, no redirect", func(t *testing.T) {
		v := base()
		v.Set("redirect_uri", "https://evil.example/cb")
		resp := get(v)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Empty(t, resp.Header.Get("Location"))
	})
	t.Run("loopback port may differ from registration", func(t *testing.T) {
		v := base()
		v.Set("redirect_uri", "http://localhost:59999/callback")
		resp := get(v)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})
	t.Run("missing PKCE redirects with invalid_request", func(t *testing.T) {
		v := base()
		v.Del("code_challenge")
		resp := get(v)
		require.Equal(t, http.StatusFound, resp.StatusCode)
		loc, err := url.Parse(resp.Header.Get("Location"))
		require.NoError(t, err)
		assert.Equal(t, "invalid_request", loc.Query().Get("error"))
	})
	t.Run("plain PKCE method redirects with invalid_request", func(t *testing.T) {
		v := base()
		v.Set("code_challenge_method", "plain")
		resp := get(v)
		require.Equal(t, http.StatusFound, resp.StatusCode)
		loc, err := url.Parse(resp.Header.Get("Location"))
		require.NoError(t, err)
		assert.Equal(t, "invalid_request", loc.Query().Get("error"))
	})
	t.Run("foreign resource redirects with invalid_target", func(t *testing.T) {
		v := base()
		v.Set("resource", "https://other-server.example")
		resp := get(v)
		require.Equal(t, http.StatusFound, resp.StatusCode)
		loc, err := url.Parse(resp.Header.Get("Location"))
		require.NoError(t, err)
		assert.Equal(t, "invalid_target", loc.Query().Get("error"))
	})
}

func TestRegisterRejections(t *testing.T) {
	_, ts := newTestServer(t, testConfig(t), happyIdP())

	post := func(body string) (*http.Response, *oauthex.ClientRegistrationError) {
		resp, err := http.Post(ts.URL+"/register", "application/json", strings.NewReader(body))
		require.NoError(t, err)
		defer resp.Body.Close() //nolint:errcheck // test cleanup
		var regErr oauthex.ClientRegistrationError
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&regErr))
		return resp, &regErr
	}

	t.Run("no redirect_uris", func(t *testing.T) {
		resp, regErr := post(`{"client_name":"x"}`)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Equal(t, "invalid_redirect_uri", regErr.ErrorCode)
	})
	t.Run("non-loopback http", func(t *testing.T) {
		resp, regErr := post(`{"redirect_uris":["http://intranet.example/cb"]}`)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Equal(t, "invalid_redirect_uri", regErr.ErrorCode)
	})
	t.Run("unsupported grant type", func(t *testing.T) {
		resp, regErr := post(`{"redirect_uris":["http://localhost/cb"],"grant_types":["implicit"]}`)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Equal(t, "invalid_client_metadata", regErr.ErrorCode)
	})
	t.Run("claude.ai callback allowed by default", func(t *testing.T) {
		resp, err := http.Post(ts.URL+"/register", "application/json",
			strings.NewReader(`{"redirect_uris":["https://claude.ai/api/mcp/auth_callback"]}`))
		require.NoError(t, err)
		defer resp.Body.Close() //nolint:errcheck // test cleanup
		assert.Equal(t, http.StatusCreated, resp.StatusCode)
	})
}

func TestCallbackRejections(t *testing.T) {
	const redirectURI = "http://localhost:41234/callback"
	client := noRedirectClient()

	runCallback := func(t *testing.T, idp *fakeIdP) *url.URL {
		t.Helper()
		_, ts := newTestServer(t, testConfig(t), idp)
		clientID := registerClient(t, ts, redirectURI)
		_, challenge := pkcePair()

		// Drive authorize+confirm to get a legitimate sealed state.
		resp, err := client.Get(ts.URL + "/authorize?" + url.Values{
			"client_id": {clientID}, "redirect_uri": {redirectURI},
			"response_type": {"code"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"},
		}.Encode())
		require.NoError(t, err)
		page, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		_ = resp.Body.Close()
		m := consentRequestRe.FindSubmatch(page)
		require.NotNil(t, m)
		resp, err = client.PostForm(ts.URL+"/authorize/confirm", url.Values{"request": {string(m[1])}})
		require.NoError(t, err)
		_ = resp.Body.Close()
		googleURL, err := url.Parse(resp.Header.Get("Location"))
		require.NoError(t, err)

		resp, err = client.Get(ts.URL + "/callback?" + url.Values{
			"state": {googleURL.Query().Get("state")}, "code": {"google-code"},
		}.Encode())
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusFound, resp.StatusCode)
		loc, err := url.Parse(resp.Header.Get("Location"))
		require.NoError(t, err)
		return loc
	}

	t.Run("forged state renders error", func(t *testing.T) {
		_, ts := newTestServer(t, testConfig(t), happyIdP())
		resp, err := client.Get(ts.URL + "/callback?state=forged&code=x")
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
	t.Run("unverified email denied", func(t *testing.T) {
		idp := happyIdP()
		idp.claims.EmailVerified = false
		loc := runCallback(t, idp)
		assert.Equal(t, "access_denied", loc.Query().Get("error"))
	})
	t.Run("wrong domain denied", func(t *testing.T) {
		idp := happyIdP()
		idp.claims.HostedDomain = "evil.example"
		loc := runCallback(t, idp)
		assert.Equal(t, "access_denied", loc.Query().Get("error"))
	})
	t.Run("no domain claim denied", func(t *testing.T) {
		idp := happyIdP()
		idp.claims.HostedDomain = ""
		loc := runCallback(t, idp)
		assert.Equal(t, "access_denied", loc.Query().Get("error"))
	})
	t.Run("exchange failure reports server_error", func(t *testing.T) {
		idp := happyIdP()
		idp.exchangeErr = fmt.Errorf("boom")
		loc := runCallback(t, idp)
		assert.Equal(t, "server_error", loc.Query().Get("error"))
	})
}

func TestTokenRejections(t *testing.T) {
	a, ts := newTestServer(t, testConfig(t), happyIdP())
	const redirectURI = "http://localhost:41234/callback"
	clientID := registerClient(t, ts, redirectURI)
	verifier, challenge := pkcePair()

	freshCode := func(t *testing.T) string {
		code, _ := authorizeThroughCallback(t, ts, clientID, redirectURI, challenge, "s")
		return code
	}

	t.Run("wrong verifier", func(t *testing.T) {
		_, oe, status := redeemCode(t, ts, clientID, redirectURI, freshCode(t), "wrong-verifier")
		assert.Equal(t, http.StatusBadRequest, status)
		assert.Equal(t, "invalid_grant", oe.Error)
	})
	t.Run("wrong client_id", func(t *testing.T) {
		otherClient := registerNamedClient(t, ts, "Another Client", redirectURI)
		require.NotEqual(t, clientID, otherClient)
		_, oe, status := redeemCode(t, ts, otherClient, redirectURI, freshCode(t), verifier)
		require.Equal(t, http.StatusBadRequest, status)
		assert.Equal(t, "invalid_grant", oe.Error)
	})
	t.Run("wrong redirect_uri", func(t *testing.T) {
		_, oe, status := redeemCode(t, ts, clientID, "http://localhost:1/cb", freshCode(t), verifier)
		assert.Equal(t, http.StatusBadRequest, status)
		assert.Equal(t, "invalid_grant", oe.Error)
	})
	t.Run("garbage code", func(t *testing.T) {
		_, oe, status := redeemCode(t, ts, clientID, redirectURI, "mcp_ac_garbage", verifier)
		assert.Equal(t, http.StatusBadRequest, status)
		assert.Equal(t, "invalid_grant", oe.Error)
	})
	t.Run("expired code", func(t *testing.T) {
		code := freshCode(t)
		a.now = func() time.Time { return time.Now().Add(2 * codeTTL) }
		t.Cleanup(func() { a.now = time.Now })
		_, oe, status := redeemCode(t, ts, clientID, redirectURI, code, verifier)
		assert.Equal(t, http.StatusBadRequest, status)
		assert.Equal(t, "invalid_grant", oe.Error)
	})
	t.Run("unsupported grant", func(t *testing.T) {
		resp, err := http.PostForm(ts.URL+"/token", url.Values{"grant_type": {"password"}})
		require.NoError(t, err)
		defer resp.Body.Close() //nolint:errcheck // test cleanup
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
	t.Run("refresh with revoked upstream grant", func(t *testing.T) {
		idp := happyIdP()
		idp.refreshErr = &oauth2.RetrieveError{ErrorCode: "invalid_grant"}
		_, ts2 := newTestServer(t, testConfig(t), idp)
		cid := registerClient(t, ts2, redirectURI)
		code, _ := authorizeThroughCallback(t, ts2, cid, redirectURI, challenge, "s")
		tr, _, status := redeemCode(t, ts2, cid, redirectURI, code, verifier)
		require.Equal(t, http.StatusOK, status)

		resp, err := http.PostForm(ts2.URL+"/token", url.Values{
			"grant_type": {"refresh_token"}, "refresh_token": {tr.RefreshToken}, "client_id": {cid},
		})
		require.NoError(t, err)
		defer resp.Body.Close() //nolint:errcheck // test cleanup
		var oe oauthErrorResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&oe))
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Equal(t, "invalid_grant", oe.Error)
	})
}

func TestVerifierRejections(t *testing.T) {
	a, ts := newTestServer(t, testConfig(t), happyIdP())
	const redirectURI = "http://localhost:41234/callback"
	clientID := registerClient(t, ts, redirectURI)
	verifier, challenge := pkcePair()
	code, _ := authorizeThroughCallback(t, ts, clientID, redirectURI, challenge, "s")
	tr, _, status := redeemCode(t, ts, clientID, redirectURI, code, verifier)
	require.Equal(t, http.StatusOK, status)

	t.Run("garbage token", func(t *testing.T) {
		_, err := a.Verifier()(context.Background(), "mcp_at_garbage", nil)
		assert.ErrorIs(t, err, auth.ErrInvalidToken)
	})
	t.Run("refresh token is not an access token", func(t *testing.T) {
		_, err := a.Verifier()(context.Background(), tr.RefreshToken, nil)
		assert.ErrorIs(t, err, auth.ErrInvalidToken)
	})
	t.Run("expired token", func(t *testing.T) {
		a.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
		t.Cleanup(func() { a.now = time.Now })
		_, err := a.Verifier()(context.Background(), tr.AccessToken, nil)
		assert.ErrorIs(t, err, auth.ErrInvalidToken)
	})
	t.Run("domain removed from allowlist", func(t *testing.T) {
		a.cfg.AllowedDomains = []string{"other.example"}
		t.Cleanup(func() { a.cfg.AllowedDomains = []string{"example.com"} })
		_, err := a.Verifier()(context.Background(), tr.AccessToken, nil)
		assert.ErrorIs(t, err, auth.ErrInvalidToken)
	})
}

func TestRevoke(t *testing.T) {
	idp := happyIdP()
	_, ts := newTestServer(t, testConfig(t), idp)
	const redirectURI = "http://localhost:41234/callback"
	clientID := registerClient(t, ts, redirectURI)
	verifier, challenge := pkcePair()
	code, _ := authorizeThroughCallback(t, ts, clientID, redirectURI, challenge, "s")
	tr, _, status := redeemCode(t, ts, clientID, redirectURI, code, verifier)
	require.Equal(t, http.StatusOK, status)

	resp, err := http.PostForm(ts.URL+"/revoke", url.Values{"token": {tr.RefreshToken}})
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, []string{"1//refresh"}, idp.revoked)

	// Invalid tokens still yield 200 (RFC 7009) and revoke nothing upstream.
	resp, err = http.PostForm(ts.URL+"/revoke", url.Values{"token": {"garbage"}})
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Len(t, idp.revoked, 1)
}

func TestIdentityAndTokenSourceFromContext(t *testing.T) {
	t.Run("absent on plain context", func(t *testing.T) {
		_, ok := Identity(context.Background())
		assert.False(t, ok)
		_, ok = GoogleTokenSource(context.Background())
		assert.False(t, ok)
	})
}
