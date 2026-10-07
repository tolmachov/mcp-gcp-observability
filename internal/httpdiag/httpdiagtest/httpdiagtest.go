// Package httpdiagtest helps tests assert the rejection httpdiag records.
package httpdiagtest

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-gcp-observability/internal/httpdiag"
)

// Logger returns a JSON logger writing into logs, as httpdiag.Handler expects.
func Logger(logs *bytes.Buffer) *slog.Logger { return slog.New(slog.NewJSONHandler(logs, nil)) }

// Serve runs r through h wrapped in httpdiag.Handler and returns the
// response and the reason recorded for it ("" when it was not rejected).
func Serve(t testing.TB, h http.Handler, r *http.Request) (*httptest.ResponseRecorder, string) {
	t.Helper()
	var logs bytes.Buffer
	rec := httptest.NewRecorder()
	httpdiag.Handler(Logger(&logs), nil, h).ServeHTTP(rec, r)
	if rec.Code < http.StatusBadRequest {
		require.Zero(t, logs.Len(), "a response below 400 must not log a rejection")
		return rec, ""
	}
	reason, _ := Event(t, &logs)["reason"].(string)
	return rec, reason
}

// Event returns the single http_request_rejected event in logs.
func Event(t testing.TB, logs *bytes.Buffer) map[string]any {
	t.Helper()
	var found map[string]any
	for line := range bytes.Lines(logs.Bytes()) {
		var event map[string]any
		require.NoError(t, json.Unmarshal(line, &event))
		if event["msg"] == "http_request_rejected" {
			require.Nil(t, found, "more than one rejection event")
			found = event
		}
	}
	require.NotNil(t, found, "no rejection event")
	return found
}
