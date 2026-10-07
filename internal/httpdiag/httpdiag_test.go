package httpdiag

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSDKRejectionsAreDiagnosableWithoutSecrets(t *testing.T) {
	const secret = "sensitive-marker-do-not-log"
	tests := []struct {
		name, body, reason string
		headers            map[string]string
	}{
		{name: "accept", body: `{"jsonrpc":"2.0","id":1,"method":"ping"}`, headers: map[string]string{"Accept": "application/json"}, reason: "accept_requires_json_and_sse"},
		{name: "content type", body: `{}`, headers: map[string]string{"Content-Type": "text/plain"}, reason: "unsupported_content_type"},
		{name: "version", body: `{"jsonrpc":"2.0","id":1,"method":"ping"}`, headers: map[string]string{"Mcp-Protocol-Version": "2024-01-01"}, reason: "unsupported_protocol_version"},
		{name: "future version", body: newProtocolRequest("tools/list", "2099-01-01", nil), headers: map[string]string{"Mcp-Protocol-Version": "2099-01-01", "Mcp-Method": "tools/list"}, reason: "unsupported_protocol_version"},
		{name: "missing request meta", body: `{"jsonrpc":"2.0","id":1,"method":"ping"}`, headers: map[string]string{"Mcp-Protocol-Version": "2026-07-28"}, reason: "invalid_request_meta"},
		{name: "missing method header", body: newProtocolRequest("tools/list", "2026-07-28", nil), headers: map[string]string{"Mcp-Protocol-Version": "2026-07-28"}, reason: "missing_mcp_method_header"},
		{name: "method header mismatch", body: newProtocolRequest("tools/list", "2026-07-28", nil), headers: map[string]string{"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call"}, reason: "mcp_method_header_mismatch"},
		{name: "missing name header", body: newProtocolRequest("tools/call", "2026-07-28", map[string]any{"name": "a"}), headers: map[string]string{"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call"}, reason: "missing_mcp_name_header"},
		{name: "name header mismatch", body: newProtocolRequest("tools/call", "2026-07-28", map[string]any{"name": "a"}), headers: map[string]string{"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call", "Mcp-Name": "b"}, reason: "mcp_name_header_mismatch"},
		{name: "missing version header", body: newProtocolRequest("tools/list", "2026-07-28", nil), headers: map[string]string{"Mcp-Protocol-Version": "", "Mcp-Method": "tools/list"}, reason: "missing_protocol_version_header"},
		{name: "version header mismatch", body: newProtocolRequest("tools/list", "2026-07-28", nil), headers: map[string]string{"Mcp-Method": "tools/list"}, reason: "protocol_version_header_mismatch"},
		{name: "method removed in new protocol", body: newProtocolRequest("ping", "2026-07-28", nil), headers: map[string]string{"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "ping"}, reason: "unsupported_rpc_method"},
		{name: "notification with id", body: `{"jsonrpc":"2.0","id":1,"method":"notifications/initialized"}`, reason: "notification_has_id"},
		{name: "missing id", body: `{"jsonrpc":"2.0","method":"ping"}`, reason: "request_missing_id"},
		{name: "missing params", body: `{"jsonrpc":"2.0","id":1,"method":"tools/call"}`, reason: "request_missing_params"},
		{name: "unknown method", body: `{"jsonrpc":"2.0","id":1,"method":"` + secret + `"}`, reason: "unsupported_rpc_method"},
		{name: "malformed payload", body: `{"jsonrpc":"2.0","id":{},"method":"` + secret + `"}`, reason: "malformed_jsonrpc"},
		{name: "batch", body: `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, reason: "jsonrpc_batch_not_supported"},
		{name: "empty body", reason: "empty_request_body"},
		{name: "last event ID", body: `{"jsonrpc":"2.0","id":1,"method":"ping"}`, headers: map[string]string{"Last-Event-ID": secret}, reason: "last_event_id_on_post"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			srv := mcp.NewServer(&mcp.Implementation{Name: "diagnostics", Version: "1"}, nil)
			sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true})
			h := Handler(slog.New(slog.NewJSONHandler(&logs, nil)), []string{"/"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ObserveBody(r, []byte(tt.body)); sdk.ServeHTTP(w, r) }))
			request := func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "http://example.com/?token="+secret, strings.NewReader(tt.body))
				r.Header.Set("Accept", "application/json, text/event-stream")
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Mcp-Protocol-Version", "2025-06-18")
				r.Header.Set("Authorization", "Bearer "+secret)
				r.Header.Set("Mcp-Session-Id", secret)
				r.Header.Set("User-Agent", "Claude-User")
				r.Header.Set("X-Cloud-Trace-Context", "0123456789abcdef0123456789abcdef/123;o=1")
				for k, v := range tt.headers {
					r.Header.Set(k, v)
				}
				return r
			}
			original := httptest.NewRecorder()
			sdk.ServeHTTP(original, request())
			got := httptest.NewRecorder()
			h.ServeHTTP(got, request())
			require.Equal(t, original.Code, got.Code)
			require.Equal(t, original.Body.String(), got.Body.String(), "diagnostics must not alter responses")
			require.GreaterOrEqual(t, got.Code, 400)
			var event map[string]any
			require.NoError(t, json.Unmarshal(logs.Bytes(), &event))
			assert.Equal(t, "http_request_rejected", event["msg"])
			assert.Equal(t, tt.reason, event["reason"])
			assert.Equal(t, "/", event["route"])
			assert.Equal(t, "claude", event["client"])
			assert.Equal(t, "0123456789abcdef0123456789abcdef", event["trace_id"])
			assert.NotEmpty(t, got.Header().Get("X-Request-ID"))
			assert.Equal(t, map[string]any{"id": got.Header().Get("X-Request-ID")}, event["logging.googleapis.com/operation"],
				"the request id is the indexed LogEntry operation id")
			assert.NotContains(t, logs.String(), secret)
			assert.NotContains(t, logs.String(), "private-tool")
		})
	}
}

func TestErrorCaptureIsBoundedAndUnknownBodiesStayPrivate(t *testing.T) {
	var logs bytes.Buffer
	h := Handler(slog.New(slog.NewJSONHandler(&logs, nil)), nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		for range 10 {
			_, _ = w.Write([]byte(strings.Repeat("sensitive", 1000)))
		}
		rw := w.(*responseWriter)
		assert.Len(t, rw.body, maxErrorBytes)
		assert.True(t, rw.truncated)
	}))
	r := httptest.NewRequest("POST", "http://example.com/private-secret?code=secret", nil)
	r.Header.Set("Mcp-Protocol-Version", "private-secret")
	r.Header.Set("User-Agent", "private-secret")
	r.Header.Set("X-Cloud-Trace-Context", "private-secret")
	h.ServeHTTP(httptest.NewRecorder(), r)
	assert.Contains(t, logs.String(), "unclassified_http_error")
	assert.Contains(t, logs.String(), `"route":"other"`)
	assert.NotContains(t, logs.String(), "sensitive")
	assert.NotContains(t, logs.String(), "private-secret")
}

func TestSuccessfulStreamFlushesBeforeHandlerCompletes(t *testing.T) {
	var logs bytes.Buffer
	finish := make(chan struct{})
	h := Handler(slog.New(slog.NewJSONHandler(&logs, nil)), nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		require.NoError(t, http.NewResponseController(w).Flush())
		assert.Empty(t, w.(*responseWriter).body, "successful response bodies must not be retained")
		select {
		case <-finish:
		case <-r.Context().Done():
		}
	}))
	ts := httptest.NewServer(h)
	defer ts.Close()
	defer close(finish)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", ts.URL, nil)
	require.NoError(t, err)
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	chunk := make([]byte, len("data: first\n\n"))
	_, err = io.ReadFull(resp.Body, chunk)
	require.NoError(t, err)
	assert.Equal(t, "data: first\n\n", string(chunk))
	assert.Empty(t, logs.String())
}

// newProtocolRequest is a sessionless (protocol 2026-07-28+) request: the
// protocol version and client identity travel in _meta on every request.
func newProtocolRequest(method, version string, params map[string]any) string {
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = map[string]any{
		mcp.MetaKeyProtocolVersion:    version,
		mcp.MetaKeyClientInfo:         mcp.Implementation{Name: "c", Version: "1"},
		mcp.MetaKeyClientCapabilities: mcp.ClientCapabilities{},
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		panic(err)
	}
	return string(body)
}

// TestClassifyErrorByCode pins JSON-RPC rejections the stateless test server
// cannot provoke: the code decides, and unknown codes never leak the message.
func TestClassifyErrorByCode(t *testing.T) {
	rpcError := func(code int64, msg string) string {
		body, err := jsonrpc.EncodeMessage(&jsonrpc.Response{Error: &jsonrpc.Error{Code: code, Message: msg}})
		require.NoError(t, err)
		return string(body)
	}
	assert.Equal(t, "missing_client_capabilities", classifyError(rpcError(mcp.CodeMissingRequiredClientCapabilities, "x")))
	assert.Equal(t, "mcp_param_header_mismatch", classifyError(rpcError(mcp.CodeHeaderMismatch, `header mismatch: missing Mcp-Param-Region header for parameter "region"`)))
	assert.Equal(t, "invalid_mcp_name_parameters", classifyError(rpcError(mcp.CodeHeaderMismatch, `failed to extract name from parameters for method "tools/call"`)))
	assert.Equal(t, "duplicate_request_id", classifyError(rpcError(jsonrpc.CodeInvalidRequest, "duplicate in-flight request ID 1")))
	assert.Equal(t, "invalid_request", classifyError(rpcError(jsonrpc.CodeInvalidRequest, "x")))
	assert.Equal(t, "invalid_params", classifyError(rpcError(jsonrpc.CodeInvalidParams, `unknown tool "x"`)))
	assert.Equal(t, "unsupported_rpc_method", classifyError(rpcError(jsonrpc.CodeMethodNotFound, `"ping" is not supported in the new protocol`)))
	assert.Equal(t, "unclassified_http_error", classifyError(rpcError(jsonrpc.CodeInternalError, "Accept must contain both")),
		"a plain-text prefix must not match a JSON-RPC error")
}

func TestSafeMethodKeepsNewProtocolMethods(t *testing.T) {
	for _, m := range []string{"server/discover", "subscriptions/listen"} {
		assert.Equal(t, m, safeMethod(m))
	}
}

type unwrappingWriter struct{ http.ResponseWriter }

func (w unwrappingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type opaqueWriter struct{ http.ResponseWriter }

// TestRejectReachesThroughWrappers pins that a reason set behind a
// middleware's writer wrapper reaches the diagnostic when the wrapper
// supports Unwrap, and that the body is classified as usual when it doesn't.
func TestRejectReachesThroughWrappers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		wrap   func(http.ResponseWriter) http.ResponseWriter
		reason string
	}{
		{"direct", func(w http.ResponseWriter) http.ResponseWriter { return w }, "own_reason"},
		{"unwrapping wrapper", func(w http.ResponseWriter) http.ResponseWriter { return unwrappingWriter{w} }, "own_reason"},
		{"opaque wrapper", func(w http.ResponseWriter) http.ResponseWriter { return opaqueWriter{w} }, "unclassified_http_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			h := Handler(slog.New(slog.NewJSONHandler(&logs, nil)), nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				Error(tc.wrap(w), "own_reason", "private detail", http.StatusServiceUnavailable)
			}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
			assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
			assert.Equal(t, "private detail\n", rec.Body.String())
			var event map[string]any
			require.NoError(t, json.Unmarshal(logs.Bytes(), &event))
			assert.Equal(t, tc.reason, event["reason"])
			assert.NotContains(t, logs.String(), "private detail")
		})
	}
}
