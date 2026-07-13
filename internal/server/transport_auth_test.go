package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/tolmachov/mcp-gcp-observability/internal/authsrv"
)

// fakeServerIdP is a minimal authsrv.IdentityProvider for wiring tests.
type fakeServerIdP struct{}

func (fakeServerIdP) AuthCodeURL(state string, _ ...oauth2.AuthCodeOption) string {
	return "https://accounts.google.example/auth?state=" + url.QueryEscape(state)
}

func (fakeServerIdP) Exchange(context.Context, string) (*oauth2.Token, error) {
	tok := &oauth2.Token{
		AccessToken:  "ya29.wiring-test",
		RefreshToken: "1//wiring",
		Expiry:       time.Now().Add(time.Hour),
	}
	return tok.WithExtra(map[string]any{
		"id_token": "raw",
		"scope":    "openid email https://www.googleapis.com/auth/cloud-platform",
	}), nil
}

func (fakeServerIdP) Refresh(context.Context, string) (*oauth2.Token, error) {
	return &oauth2.Token{AccessToken: "ya29.refreshed", Expiry: time.Now().Add(time.Hour)}, nil
}

func (fakeServerIdP) ValidateIDToken(context.Context, string) (*authsrv.IdentityClaims, error) {
	return &authsrv.IdentityClaims{
		Subject: "sub-wiring", Email: "dev@example.com",
		EmailVerified: true, HostedDomain: "example.com",
	}, nil
}

func (fakeServerIdP) Revoke(context.Context, string) error { return nil }

var wiringConsentRe = regexp.MustCompile(`name="request" value="([^"]+)"`)

// newWiringServer assembles the EXACT production HTTP surface of
// runHTTPWithAuth — buildAuthMux wrapped in the cross-origin protection with
// the production bypass list — backed by a stub per-user pool.
func newWiringServer(t *testing.T) (*httptest.Server, *countingBuilder) {
	t.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)

	ts := httptest.NewServer(nil) // URL needed before the mux exists
	t.Cleanup(ts.Close)

	cfg := &authsrv.Config{
		IssuerURL:          ts.URL,
		GoogleClientID:     "cid",
		GoogleClientSecret: "secret",
		AllowedDomains:     []string{"example.com"},
		TokenKeys:          []string{base64.StdEncoding.EncodeToString(key)},
	}
	as, err := authsrv.NewWithProvider(cfg, slog.New(slog.DiscardHandler), fakeServerIdP{})
	require.NoError(t, err)

	b := newCountingBuilder()
	pool := newUserPool(context.Background(), b.builder(), discardLogger())
	t.Cleanup(func() { _ = pool.Close() })

	handler := withCrossOriginProtection(buildAuthMux(as, cfg.IssuerURL, pool), authCORSBypassPaths...)
	ts.Config.Handler = handler
	return ts, b
}

// obtainToken drives the complete OAuth flow over the production mux and
// returns a live access token.
func obtainToken(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	regResp, err := http.Post(ts.URL+"/register", "application/json",
		strings.NewReader(`{"redirect_uris":["http://localhost:53123/callback"],"client_name":"wiring"}`))
	require.NoError(t, err)
	defer regResp.Body.Close() //nolint:errcheck // test cleanup
	require.Equal(t, http.StatusCreated, regResp.StatusCode)
	var reg struct {
		ClientID string `json:"client_id"`
	}
	require.NoError(t, json.NewDecoder(regResp.Body).Decode(&reg))

	verifier := "wiring-test-verifier-of-sufficient-length-42"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	resp, err := client.Get(ts.URL + "/authorize?" + url.Values{
		"client_id":             {reg.ClientID},
		"redirect_uri":          {"http://localhost:53123/callback"},
		"response_type":         {"code"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"resource":              {ts.URL},
	}.Encode())
	require.NoError(t, err)
	page, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	m := wiringConsentRe.FindSubmatch(page)
	require.NotNil(t, m)

	resp, err = client.PostForm(ts.URL+"/authorize/confirm", url.Values{"request": {string(m[1])}})
	require.NoError(t, err)
	_ = resp.Body.Close()
	googleURL, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)

	resp, err = client.Get(ts.URL + "/callback?" + url.Values{
		"state": {googleURL.Query().Get("state")}, "code": {"gcode"},
	}.Encode())
	require.NoError(t, err)
	_ = resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	require.Empty(t, loc.Query().Get("error"))

	tokResp, err := http.PostForm(ts.URL+"/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {loc.Query().Get("code")},
		"client_id":     {reg.ClientID},
		"redirect_uri":  {"http://localhost:53123/callback"},
		"code_verifier": {verifier},
		"resource":      {ts.URL},
	})
	require.NoError(t, err)
	defer tokResp.Body.Close() //nolint:errcheck // test cleanup
	require.Equal(t, http.StatusOK, tokResp.StatusCode)
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	require.NoError(t, json.NewDecoder(tokResp.Body).Decode(&tok))
	require.NotEmpty(t, tok.AccessToken)
	return tok.AccessToken
}

// TestProductionAuthWiring exercises the exact handler composition served by
// runHTTPWithAuth: discovery challenge, full OAuth flow, cross-origin
// exemptions, and bearer-gated MCP dispatch through the pool.
func TestProductionAuthWiring(t *testing.T) {
	ts, b := newWiringServer(t)

	t.Run("unauthenticated MCP request gets 401 with discovery challenge", func(t *testing.T) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/", strings.NewReader("{}"))
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Contains(t, resp.Header.Get("WWW-Authenticate"), "resource_metadata=")
		assert.Contains(t, resp.Header.Get("WWW-Authenticate"), "/.well-known/oauth-protected-resource")
	})

	token := obtainToken(t, ts)

	t.Run("real token reaches the per-user pool", func(t *testing.T) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/", strings.NewReader("{}"))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "hello dev@example.com", string(body))
		assert.Equal(t, 1, b.buildCount("dev@example.com"))
	})

	t.Run("cross-origin POST to /token is exempt from protection", func(t *testing.T) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/token",
			strings.NewReader("grant_type=unsupported"))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "https://claude.ai")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		// Reaches the handler (which rejects the grant type) instead of
		// being blocked by cross-origin protection.
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("cross-origin POST to the MCP endpoint is blocked", func(t *testing.T) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/", strings.NewReader("{}"))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("healthz is open", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/healthz")
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})
}

// TestRunRejectsAuthOnStdio pins the Run-level guard.
func TestRunRejectsAuthOnStdio(t *testing.T) {
	s := testServer(t)
	err := s.Run(context.Background(), RunOptions{
		Transport: TransportStdio,
		Auth:      &authsrv.Config{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires --transport http")
}
