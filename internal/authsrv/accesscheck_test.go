package authsrv

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCRMAccessChecker(t *testing.T) {
	newChecker := func(t *testing.T, handler http.HandlerFunc) *crmAccessChecker {
		t.Helper()
		srv := httptest.NewServer(handler)
		t.Cleanup(srv.Close)
		c := newCRMAccessChecker()
		c.endpoint = srv.URL
		return c
	}

	t.Run("permission held", func(t *testing.T) {
		var gotPath, gotAuth string
		var gotBody map[string][]string
		c := newChecker(t, func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
			_ = json.NewEncoder(w).Encode(map[string][]string{"permissions": {projectAccessPermission}})
		})
		ok, err := c.HasProjectAccess(context.Background(), "ya29.tok", "my-proj")
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, "/v3/projects/my-proj:testIamPermissions", gotPath)
		assert.Equal(t, "Bearer ya29.tok", gotAuth)
		assert.Equal(t, []string{projectAccessPermission}, gotBody["permissions"])
	})
	t.Run("no permissions is a definitive no", func(t *testing.T) {
		c := newChecker(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{}`))
		})
		ok, err := c.HasProjectAccess(context.Background(), "t", "my-proj")
		require.NoError(t, err)
		assert.False(t, ok)
	})
	t.Run("403 is a definitive no", func(t *testing.T) {
		c := newChecker(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		})
		ok, err := c.HasProjectAccess(context.Background(), "t", "my-proj")
		require.NoError(t, err)
		assert.False(t, ok)
	})
	t.Run("500 is transient", func(t *testing.T) {
		c := newChecker(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		_, err := c.HasProjectAccess(context.Background(), "t", "my-proj")
		assert.Error(t, err)
	})
}

// fakeAccessChecker is the AccessChecker test double for flow tests.
type fakeAccessChecker struct {
	allowed  bool
	err      error
	tokens   []string // Google access tokens the probe was called with
	projects []string // projects the probe was called for
}

func (f *fakeAccessChecker) HasProjectAccess(_ context.Context, googleAccessToken, project string) (bool, error) {
	f.tokens = append(f.tokens, googleAccessToken)
	f.projects = append(f.projects, project)
	if f.err != nil {
		return false, f.err
	}
	return f.allowed, nil
}

// projectAccessConfig gates by IAM project access only: no Workspace domain
// allowlist, matching teams whose members use personal Google accounts.
func projectAccessConfig(t *testing.T, chk AccessChecker) *Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.AllowedDomains = nil
	cfg.RequireProjectAccess = "obs-project"
	cfg.accessChecker = chk
	return cfg
}

// consumerIdP is happyIdP for a personal Google account: no hd claim.
func consumerIdP() *fakeIdP {
	idp := happyIdP()
	idp.claims.HostedDomain = ""
	idp.claims.Email = "dev@gmail.com"
	return idp
}

// loginRedirect walks register→authorize→confirm→callback and returns the
// final redirect back to the client, for asserting success or error params.
func loginRedirect(t *testing.T, ts *httptest.Server) (clientID string, loc *url.URL) {
	t.Helper()
	return loginRedirectProject(t, ts, "")
}

// loginRedirectProject is loginRedirect with a project submitted on the
// consent form (project-choice mode).
func loginRedirectProject(t *testing.T, ts *httptest.Server, project string) (clientID string, loc *url.URL) {
	t.Helper()
	client := noRedirectClient()
	const redirectURI = "http://127.0.0.1:9999/cb"
	clientID = registerClient(t, ts, redirectURI)
	_, challenge := pkcePair()

	page := fetchConsentPage(t, ts, clientID, redirectURI, challenge)
	m := consentRequestRe.FindSubmatch(page)
	require.NotNil(t, m)

	form := url.Values{"request": {string(m[1])}}
	if project != "" {
		form.Set("project", project)
	}
	resp, err := client.PostForm(ts.URL+"/authorize/confirm", form)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusFound, resp.StatusCode)
	googleURL, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)

	resp, err = client.Get(ts.URL + "/callback?" + url.Values{
		"state": {googleURL.Query().Get("state")},
		"code":  {"google-code"},
	}.Encode())
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusFound, resp.StatusCode)
	loc, err = url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	return clientID, loc
}

// fetchConsentPage runs /authorize and returns the consent page HTML.
func fetchConsentPage(t *testing.T, ts *httptest.Server, clientID, redirectURI, challenge string) []byte {
	t.Helper()
	resp, err := noRedirectClient().Get(ts.URL + "/authorize?" + url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode())
	require.NoError(t, err)
	page, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return page
}

func TestCallbackRejectsUngrantedScope(t *testing.T) {
	// Google's granular consent let the user untick the Cloud checkbox: the
	// token response carries only the OIDC scopes. The callback must name
	// the real cause instead of a misleading "no access to the project".
	idp := happyIdP()
	idp.grantedScope = "openid email"
	_, ts := newTestServer(t, testConfig(t), idp)
	_, loc := loginRedirect(t, ts)
	assert.Equal(t, "access_denied", loc.Query().Get("error"))
	assert.Contains(t, loc.Query().Get("error_description"), "permission was not granted")
}

func TestProjectAccessGate(t *testing.T) {
	t.Run("consumer account with project access logs in", func(t *testing.T) {
		chk := &fakeAccessChecker{allowed: true}
		_, ts := newTestServer(t, projectAccessConfig(t, chk), consumerIdP())
		clientID, loc := loginRedirect(t, ts)
		require.Empty(t, loc.Query().Get("error"), "login failed: %s", loc.Query().Get("error_description"))
		require.NotEmpty(t, loc.Query().Get("code"))
		_ = clientID
		// The probe ran exactly once, with the user's own Google token,
		// against the pinned project.
		assert.Equal(t, []string{"ya29.user-token"}, chk.tokens)
		assert.Equal(t, []string{"obs-project"}, chk.projects)
	})
	t.Run("account without project access is denied", func(t *testing.T) {
		chk := &fakeAccessChecker{allowed: false}
		_, ts := newTestServer(t, projectAccessConfig(t, chk), consumerIdP())
		_, loc := loginRedirect(t, ts)
		assert.Equal(t, "access_denied", loc.Query().Get("error"))
		assert.Empty(t, loc.Query().Get("code"))
	})
	t.Run("transient probe failure fails closed with server_error", func(t *testing.T) {
		chk := &fakeAccessChecker{err: errors.New("crm unavailable")}
		_, ts := newTestServer(t, projectAccessConfig(t, chk), consumerIdP())
		_, loc := loginRedirect(t, ts)
		assert.Equal(t, "server_error", loc.Query().Get("error"))
	})
	t.Run("refresh is cut off after access is revoked", func(t *testing.T) {
		chk := &fakeAccessChecker{allowed: true}
		_, ts := newTestServer(t, projectAccessConfig(t, chk), consumerIdP())
		clientID, loc := loginRedirect(t, ts)
		require.Empty(t, loc.Query().Get("error"))
		verifier, _ := pkcePair()
		tr, _, status := redeemCode(t, ts, clientID, "http://127.0.0.1:9999/cb", loc.Query().Get("code"), verifier)
		require.Equal(t, http.StatusOK, status)
		require.NotEmpty(t, tr.RefreshToken)

		// Access still granted: refresh succeeds and re-probes with the
		// refreshed Google token.
		refreshed, _, status := refreshGrant(t, ts.URL, tr.RefreshToken, clientID)
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, []string{"ya29.user-token", "ya29.refreshed"}, chk.tokens)

		// IAM role removed: the next refresh must fail with invalid_grant.
		chk.allowed = false
		_, oe, status := refreshGrant(t, ts.URL, refreshed.RefreshToken, clientID)
		require.Equal(t, http.StatusBadRequest, status)
		assert.Equal(t, "invalid_grant", oe.Error)

		// Transient probe failure on refresh is 503, not a dead grant.
		chk.allowed = true
		chk.err = errors.New("crm unavailable")
		_, oe, status = refreshGrant(t, ts.URL, refreshed.RefreshToken, clientID)
		require.Equal(t, http.StatusServiceUnavailable, status)
		assert.Equal(t, "temporarily_unavailable", oe.Error)
	})
}

// chooserConfig enables project choice: no domain allowlist, no pinned
// project — the user names a project and must have IAM access to it.
func chooserConfig(t *testing.T, chk AccessChecker) *Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.AllowedDomains = nil
	cfg.AllowProjectChoice = true
	cfg.accessChecker = chk
	return cfg
}

func TestProjectChoice(t *testing.T) {
	t.Run("consent asks for a project and the grant is bound to it", func(t *testing.T) {
		chk := &fakeAccessChecker{allowed: true}
		a, ts := newTestServer(t, chooserConfig(t, chk), consumerIdP())

		clientID := registerClient(t, ts, "http://127.0.0.1:9999/cb")
		_, challenge := pkcePair()
		page := fetchConsentPage(t, ts, clientID, "http://127.0.0.1:9999/cb", challenge)
		assert.Contains(t, string(page), `name="project"`, "consent page must ask for a project")

		clientID2, loc := loginRedirectProject(t, ts, "team-observability")
		require.Empty(t, loc.Query().Get("error"), "login failed: %s", loc.Query().Get("error_description"))
		// The access probe ran against the user's chosen project.
		assert.Equal(t, []string{"team-observability"}, chk.projects)

		verifier, _ := pkcePair()
		tr, _, status := redeemCode(t, ts, clientID2, "http://127.0.0.1:9999/cb", loc.Query().Get("code"), verifier)
		require.Equal(t, http.StatusOK, status)

		// The choice is sealed into the access token and exposed to the
		// per-user assembly via the verifier.
		info, err := a.Verifier()(context.Background(), tr.AccessToken, nil)
		require.NoError(t, err)
		extra, ok := info.Extra[extraIdentityKey].(identityExtra)
		require.True(t, ok)
		assert.Equal(t, "team-observability", extra.Project)

		// Refresh re-probes the same chosen project.
		_, _, status = refreshGrant(t, ts.URL, tr.RefreshToken, clientID2)
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, []string{"team-observability", "team-observability"}, chk.projects)
	})
	t.Run("missing project is rejected at confirm", func(t *testing.T) {
		chk := &fakeAccessChecker{allowed: true}
		_, ts := newTestServer(t, chooserConfig(t, chk), consumerIdP())
		clientID := registerClient(t, ts, "http://127.0.0.1:9999/cb")
		_, challenge := pkcePair()
		page := fetchConsentPage(t, ts, clientID, "http://127.0.0.1:9999/cb", challenge)
		m := consentRequestRe.FindSubmatch(page)
		require.NotNil(t, m)

		resp, err := noRedirectClient().PostForm(ts.URL+"/authorize/confirm", url.Values{"request": {string(m[1])}})
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Contains(t, string(body), "Invalid project ID")
		assert.Empty(t, chk.projects, "no probe must run without a project")
	})
	t.Run("malformed project id is rejected at confirm", func(t *testing.T) {
		chk := &fakeAccessChecker{allowed: true}
		_, ts := newTestServer(t, chooserConfig(t, chk), consumerIdP())
		clientID := registerClient(t, ts, "http://127.0.0.1:9999/cb")
		_, challenge := pkcePair()
		page := fetchConsentPage(t, ts, clientID, "http://127.0.0.1:9999/cb", challenge)
		m := consentRequestRe.FindSubmatch(page)
		require.NotNil(t, m)

		resp, err := noRedirectClient().PostForm(ts.URL+"/authorize/confirm", url.Values{
			"request": {string(m[1])}, "project": {"Bad_Project!"},
		})
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
	t.Run("no access to the chosen project is denied", func(t *testing.T) {
		chk := &fakeAccessChecker{allowed: false}
		_, ts := newTestServer(t, chooserConfig(t, chk), consumerIdP())
		_, loc := loginRedirectProject(t, ts, "someone-elses-project")
		assert.Equal(t, "access_denied", loc.Query().Get("error"))
		assert.Contains(t, loc.Query().Get("error_description"), "someone-elses-project")
	})
	t.Run("pinned mode ignores a smuggled project field", func(t *testing.T) {
		chk := &fakeAccessChecker{allowed: true}
		_, ts := newTestServer(t, projectAccessConfig(t, chk), consumerIdP())
		_, loc := loginRedirectProject(t, ts, "attacker-project")
		require.Empty(t, loc.Query().Get("error"))
		// The probe must target the pinned project, not the form value.
		assert.Equal(t, []string{"obs-project"}, chk.projects)
	})
}

// TestRefreshPredatingProjectChoice: a refresh token minted before project
// choice was enabled (no project inside) must force a re-login, not mint a
// grant whose assembly can never build.
func TestRefreshPredatingProjectChoice(t *testing.T) {
	chk := &fakeAccessChecker{allowed: true}
	domainCfg := testConfig(t)
	domainCfg.accessChecker = chk

	// Mint a grant on a domain-only server (no project in the claims).
	_, ts := newTestServer(t, domainCfg, happyIdP())
	clientID, loc := loginRedirect(t, ts)
	require.Empty(t, loc.Query().Get("error"))
	verifier, _ := pkcePair()
	tr, _, status := redeemCode(t, ts, clientID, "http://127.0.0.1:9999/cb", loc.Query().Get("code"), verifier)
	require.Equal(t, http.StatusOK, status)

	// Same keys and issuer, but redeployed with project choice enabled.
	chooserCfg := chooserConfig(t, chk)
	chooserCfg.TokenKeys = domainCfg.TokenKeys
	_, ts2 := newTestServer(t, chooserCfg, happyIdP())

	_, oe, status := refreshGrant(t, ts2.URL, tr.RefreshToken, clientID)
	require.Equal(t, http.StatusBadRequest, status)
	require.NotNil(t, oe)
	assert.Equal(t, "invalid_grant", oe.Error)
	assert.Contains(t, oe.ErrorDescription, "log in again")
}
