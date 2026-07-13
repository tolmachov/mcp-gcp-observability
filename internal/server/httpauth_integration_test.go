package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

// bearerRoundTripper injects a static bearer token into every request.
type bearerRoundTripper struct {
	token string
	next  http.RoundTripper
}

func (rt bearerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+rt.token)
	return rt.next.RoundTrip(r) //nolint:wrapcheck // transparent RoundTripper
}

// newAuthedMCPTestServer wires the real per-user assembly builder (real gapic
// client construction with a static token source — no network I/O) behind
// RequireBearerToken with the pool-test verifier, mirroring runHTTPWithAuth's
// MCP path.
func newAuthedMCPTestServer(t *testing.T, variantID string) (*httptest.Server, *userPool) {
	t.Helper()
	s := &Server{
		cfg:       &gcpclient.Config{DefaultProject: "test-project", LogsMaxLimit: 1000, ErrorsMaxLimit: 100},
		completer: &promptCompleter{},
		version:   "test",
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	pool := newUserPool(ctx, s.userAssemblyBuilder(metrics.NewRegistry(), variantID), s.logger)
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Logf("pool.Close: %v", err)
		}
	})

	handler := auth.RequireBearerToken(poolTestVerifier(), nil)(pool)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts, pool
}

// connectMCP opens an MCP session over streamable HTTP as the given user.
func connectMCP(t *testing.T, ctx context.Context, url, userToken string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: url,
		HTTPClient: &http.Client{
			Transport: bearerRoundTripper{token: userToken, next: http.DefaultTransport},
		},
		DisableStandaloneSSE: true,
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// TestHTTPAuthMCPIntegration drives real MCP sessions through the
// bearer-protected per-user pool: each user gets an isolated assembly with
// the full tool set, and unauthenticated requests are rejected with a 401.
func TestHTTPAuthMCPIntegration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ts, pool := newAuthedMCPTestServer(t, string(VariantFull))

	alice := connectMCP(t, ctx, ts.URL, "alice")
	tools1, err := alice.ListTools(ctx, nil)
	require.NoError(t, err)
	assert.Len(t, tools1.Tools, allToolsCount)

	bob := connectMCP(t, ctx, ts.URL, "bob")
	tools2, err := bob.ListTools(ctx, nil)
	require.NoError(t, err)
	assert.Len(t, tools2.Tools, allToolsCount)

	assert.Equal(t, 2, pool.size(), "two users must get two isolated assemblies")

	t.Run("no token is rejected", func(t *testing.T) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL, nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close() //nolint:errcheck // test cleanup
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})
}

// TestHTTPAuthMCPIntegrationVariants exercises the variants-negotiation path
// (no forced variant) end-to-end through the pool.
func TestHTTPAuthMCPIntegrationVariants(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ts, pool := newAuthedMCPTestServer(t, "")

	session := connectMCP(t, ctx, ts.URL, "alice")
	tools, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	assert.NotEmpty(t, tools.Tools)
	assert.Equal(t, 1, pool.size())
}
