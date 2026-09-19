package authsrv

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCRMAccessChecker(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string][]string{"permissions": {projectAccessPermission}})
	}))
	defer srv.Close()
	c := newCRMAccessChecker()
	c.endpoint = srv.URL
	ok, err := c.HasProjectAccess(context.Background(), "ya29.user", "my-project")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "Bearer ya29.user", gotAuth)
}

type fakeAccessChecker struct {
	allowed  bool
	err      error
	tokens   []string
	projects []string
}

func (f *fakeAccessChecker) HasProjectAccess(_ context.Context, token, project string) (bool, error) {
	f.tokens = append(f.tokens, token)
	f.projects = append(f.projects, project)
	return f.allowed, f.err
}

func TestPinnedProjectAccessGate(t *testing.T) {
	chk := &fakeAccessChecker{allowed: true}
	cfg := testConfig(t)
	cfg.AllowedDomains = nil
	cfg.PinnedProject = "obs-project"
	cfg.accessChecker = chk
	_, ts := newTestServer(t, cfg, happyIdP())
	clientID, loc := loginRedirect(t, ts)
	require.Empty(t, loc.Query().Get("error"))
	verifier, _ := pkcePair()
	tr, _, status := redeemCode(t, ts, clientID, "http://127.0.0.1:9999/cb", loc.Query().Get("code"), verifier)
	require.Equal(t, http.StatusOK, status)
	refreshed, _, status := refreshGrant(t, ts.URL, tr.RefreshToken, clientID)
	require.Equal(t, http.StatusOK, status)
	assert.NotEmpty(t, refreshed.RefreshToken)
	assert.Equal(t, []string{"obs-project", "obs-project"}, chk.projects)

	chk.err = errors.New("IAM unavailable")
	_, oe, status := refreshGrant(t, ts.URL, refreshed.RefreshToken, clientID)
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, "temporarily_unavailable", oe.Error)
}

func loginRedirect(t *testing.T, ts *httptest.Server) (string, *url.URL) {
	t.Helper()
	const redirectURI = "http://127.0.0.1:9999/cb"
	clientID := registerClient(t, ts, redirectURI)
	_, challenge := pkcePair()
	code, _ := authorizeThroughCallback(t, ts, clientID, redirectURI, challenge, "")
	u, err := url.Parse(redirectURI + "?code=" + url.QueryEscape(code))
	require.NoError(t, err)
	return clientID, u
}
