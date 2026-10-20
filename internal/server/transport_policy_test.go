package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-gcp-observability/internal/httpdiag"
	"github.com/tolmachov/mcp-gcp-observability/internal/httpdiag/httpdiagtest"
)

// Removing body observation from the entry policy must lose these safe MCP
// envelope fields from a rejection diagnostic.
func TestHTTPEntryHandlerObservesMCPBody(t *testing.T) {
	var logs bytes.Buffer
	handler := httpEntryHandler(httpdiagtest.Logger(&logs), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpdiag.Error(w, "test_rejection", "rejected", http.StatusBadRequest)
	}))
	body := `{"jsonrpc":"2.0","method":"tools/list","id":1}`
	req := httptest.NewRequest(http.MethodPost, "http://example.com/mcp", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	event := httpdiagtest.Event(t, &logs)
	assert.Equal(t, "test_rejection", event["reason"])
	assert.Equal(t, "tools/list", event["rpc_method"])
	assert.Equal(t, "single", event["body_shape"])
	assert.Equal(t, float64(len(body)), event["request_bytes"])
}

// Removing origin protection from the entry policy would dispatch this request.
func TestHTTPEntryHandlerRejectsCrossOriginBeforeBodyLimit(t *testing.T) {
	var logs bytes.Buffer
	called := false
	handler := httpEntryHandler(httpdiagtest.Logger(&logs), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "http://example.com/mcp", strings.NewReader(strings.Repeat("x", maxMCPRequestBytes+1)))
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.False(t, called)
	event := httpdiagtest.Event(t, &logs)
	assert.Equal(t, "cross_origin_rejected", event["reason"])
	assert.Equal(t, "", event["body_shape"])
}

func TestHTTPEntryHandlerAllowsOAuthCrossOriginPost(t *testing.T) {
	var logs bytes.Buffer
	called := false
	handler := httpEntryHandler(httpdiagtest.Logger(&logs), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "http://example.com/token", strings.NewReader("grant_type=unsupported"))
	req.Header.Set("Origin", "https://claude.ai")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.True(t, called)
	assert.Empty(t, logs.String())
}
