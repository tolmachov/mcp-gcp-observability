// Package httpdiag records HTTP rejection reasons without recording credentials,
// RPC arguments, response bodies, resource names, or URL query parameters.
package httpdiag

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxErrorBytes = 4096

type requestKey struct{}

type requestInfo struct {
	method string
	shape  string
}

// ObserveBody examines an already size-limited MCP envelope. It never retains
// the body or parameters. OAuth bodies must not be passed here.
func ObserveBody(r *http.Request, body []byte) {
	info, ok := r.Context().Value(requestKey{}).(*requestInfo)
	if !ok || (r.URL.Path != "/" && r.URL.Path != "/mcp") {
		return
	}
	trimmed := bytes.TrimSpace(body)
	info.shape = "invalid_json"
	if len(trimmed) == 0 {
		info.shape = "empty"
		return
	}
	if trimmed[0] == '[' {
		info.shape = "batch"
		return
	}
	var envelope struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(trimmed, &envelope) == nil {
		info.shape = "single"
		info.method = safeMethod(envelope.Method)
	}
}

// Error is http.Error for one of our own rejections: it records reason with
// Reject and writes msg as the plain-text body.
func Error(w http.ResponseWriter, reason, msg string, status int) {
	Reject(w, reason)
	http.Error(w, msg, status)
}

// Reject attaches the static reason class of one of our own rejections to
// the enclosing HTTP diagnostic, so it is never guessed from the response
// body. Use it directly only where the body is not plain text; otherwise use
// Error. Callers must never pass user input or errors from upstream services.
func Reject(w http.ResponseWriter, reason string) { RejectOAuth(w, "", reason) }

// RejectOAuth is Reject for an OAuth error response, additionally recording
// its RFC 6749 / RFC 7591 error code.
func RejectOAuth(w http.ResponseWriter, code, reason string) {
	for {
		if rw, ok := w.(*responseWriter); ok {
			rw.code, rw.reason = code, reason
			return
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		w = u.Unwrap()
	}
}

// Handler must enclose the body limiter, cross-origin protection and auth so
// that rejections from any layer are observed. Successful SSE responses stream
// directly to the client and are neither buffered nor logged here. routes are
// the served paths that may be logged verbatim; any other path is logged as
// "other" so probes and typos never leak into logs.
func Handler(logger *slog.Logger, routes []string, next http.Handler) http.Handler {
	known := make(map[string]bool, len(routes))
	for _, route := range routes {
		known[route] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		id := rand.Text()
		w.Header().Set("X-Request-ID", id)
		info := &requestInfo{}
		r = r.WithContext(context.WithValue(r.Context(), requestKey{}, info))
		rw := &responseWriter{ResponseWriter: w}
		defer func() {
			if rw.status < 400 {
				return
			}
			reason := rw.reason
			if reason == "" {
				reason = classifyError(string(rw.body))
			}
			attrs := []any{
				slog.Group("logging.googleapis.com/operation", "id", id), "http_method", safeHTTPMethod(r.Method),
				"route", safeRoute(known, r.URL.Path), "status", rw.status,
				"reason", reason, "duration_ms", time.Since(started).Milliseconds(),
				"client", clientClass(r.UserAgent()), "rpc_method", info.method,
				"body_shape", info.shape, "request_bytes", r.ContentLength,
				"protocol_version", protocolVersion(r.Header.Get("Mcp-Protocol-Version")),
				"has_session_id", r.Header.Get("Mcp-Session-Id") != "",
				"has_last_event_id", r.Header.Get("Last-Event-ID") != "",
				"has_mcp_method", r.Header.Get("Mcp-Method") != "",
				"has_mcp_name", r.Header.Get("Mcp-Name") != "",
				"error_body_truncated", rw.truncated,
			}
			if rw.code != "" {
				attrs = append(attrs, "oauth_error", rw.code)
			}
			traceID, _, _ := strings.Cut(r.Header.Get("X-Cloud-Trace-Context"), "/")
			if decoded, err := hex.DecodeString(traceID); err == nil && len(decoded) == 16 {
				attrs = append(attrs, "trace_id", traceID)
			}
			level := slog.LevelWarn
			severity := "WARNING"
			if rw.status >= 500 {
				level = slog.LevelError
				severity = "ERROR"
			}
			attrs = append(attrs, "severity", severity)
			logger.Log(r.Context(), level, "http_request_rejected", attrs...)
		}()
		next.ServeHTTP(rw, r)
	})
}

type responseWriter struct {
	http.ResponseWriter
	status    int
	body      []byte
	truncated bool
	code      string
	reason    string
}

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *responseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	if status >= 200 || status == http.StatusSwitchingProtocols {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	if w.status >= 400 {
		take := min(n, maxErrorBytes-len(w.body))
		w.body = append(w.body, p[:take]...)
		w.truncated = w.truncated || take < n
	}
	if err != nil {
		return n, fmt.Errorf("write HTTP response: %w", err)
	}
	return n, nil
}

func (w *responseWriter) Flush() { _ = w.FlushError() }

func (w *responseWriter) FlushError() error {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if err := http.NewResponseController(w.ResponseWriter).Flush(); err != nil {
		return fmt.Errorf("flush HTTP response: %w", err)
	}
	return nil
}

func safeMethod(method string) string {
	switch method {
	case "initialize", "ping", "tools/list", "tools/call", "resources/list", "resources/read",
		"resources/templates/list", "resources/subscribe", "resources/unsubscribe", "prompts/list",
		"prompts/get", "completion/complete", "logging/setLevel", "notifications/initialized",
		"notifications/cancelled", "notifications/progress", "notifications/roots/list_changed", //nolint:misspell // MCP wire spelling
		"tasks/get", "tasks/list", "tasks/result", "tasks/cancel", "server/discover", "subscriptions/listen":
		return method
	default:
		return "unknown"
	}
}

func safeHTTPMethod(method string) string {
	switch method {
	case "GET", "POST", "DELETE", "PUT", "PATCH", "HEAD", "OPTIONS":
		return method
	default:
		return "other"
	}
}

func safeRoute(known map[string]bool, path string) string {
	if known[path] {
		return path
	}
	return "other"
}

func clientClass(agent string) string {
	switch {
	case strings.HasPrefix(agent, "codex-mcp-client/"):
		return "codex"
	case agent == "Claude-User":
		return "claude"
	default:
		return "other"
	}
}

func protocolVersion(value string) string {
	if value == "" {
		return "absent"
	}
	if _, err := time.Parse("2006-01-02", value); err == nil {
		return value
	}
	return "invalid"
}

// SDK errors sometimes echo the JSON payload, method, URI, or Last-Event-ID.
// Match a JSON-RPC error by its code and static message prefix, a plain-text
// body by its static prefix, and emit only our own reason classes. Never log
// an unknown body verbatim, even if it is short or looks like a normal error.
func classifyError(body string) string {
	var rpc struct {
		Error *jsonrpc.Error `json:"error"`
	}
	if json.Unmarshal([]byte(body), &rpc) == nil && rpc.Error != nil {
		for _, rule := range rpcRejectionReasons {
			if rule.code == rpc.Error.Code && strings.HasPrefix(rpc.Error.Message, rule.prefix) {
				return rule.reason
			}
		}
		return "unclassified_http_error"
	}
	for _, rule := range rejectionReasons {
		if strings.HasPrefix(body, rule.prefix) {
			return rule.reason
		}
	}
	return "unclassified_http_error"
}

// rpcRejectionReasons is matched in order, so a specific prefix must precede
// a broader one with the same code; an empty prefix matches any message.
var rpcRejectionReasons = []struct {
	code           int64
	prefix, reason string
}{
	{mcp.CodeUnsupportedProtocolVersion, "", "unsupported_protocol_version"},
	{mcp.CodeHeaderMismatch, "Mcp-Protocol-Version header is required", "missing_protocol_version_header"},
	{mcp.CodeHeaderMismatch, "Mcp-Protocol-Version header ", "protocol_version_header_mismatch"},
	{mcp.CodeHeaderMismatch, "missing required Mcp-Method header", "missing_mcp_method_header"},
	{mcp.CodeHeaderMismatch, "missing required Mcp-Name header", "missing_mcp_name_header"},
	{mcp.CodeHeaderMismatch, "header mismatch: Mcp-Method", "mcp_method_header_mismatch"},
	{mcp.CodeHeaderMismatch, "header mismatch: Mcp-Name", "mcp_name_header_mismatch"},
	{mcp.CodeHeaderMismatch, "failed to extract name from parameters", "invalid_mcp_name_parameters"},
	{mcp.CodeHeaderMismatch, "", "mcp_param_header_mismatch"},
	{mcp.CodeMissingRequiredClientCapabilities, "", "missing_client_capabilities"},
	{jsonrpc.CodeMethodNotFound, "", "unsupported_rpc_method"},
	{jsonrpc.CodeInvalidParams, "missing or invalid _meta field", "invalid_request_meta"},
	{jsonrpc.CodeInvalidParams, "invalid _meta field", "invalid_request_meta"},
	{jsonrpc.CodeInvalidParams, "", "invalid_params"},
	{jsonrpc.CodeInvalidRequest, "duplicate in-flight request ID", "duplicate_request_id"},
	{jsonrpc.CodeInvalidRequest, "", "invalid_request"},
}

// rejectionReasons classifies the plain-text bodies the go-sdk stateless
// streamable handler can write in this server. Our own rejections are marked with Reject where they
// happen and never appear here. SDK bodies our configuration rules out have
// no row: stateful-only ones (sessions, GET streams, replay) because both MCP
// handlers run with Stateless: true, its body-limit and read errors because
// limitRequestBody runs first, connection failures because getServer never
// returns nil and a fresh transport without an EventStore always connects,
// and bearer-token ones because authsrv marks every rejection it delegates.
var rejectionReasons = []struct{ prefix, reason string }{
	{"Accept must contain both", "accept_requires_json_and_sse"},
	{"Content-Type must be", "unsupported_content_type"},
	{"Bad Request: Unsupported protocol version", "unsupported_protocol_version"},
	{"JSON-RPC batching is not supported", "jsonrpc_batch_not_supported"},
	{"malformed payload:", "malformed_jsonrpc"},
	{"JSON RPC not handled:", "unsupported_rpc_method"},
	{"invalid request: unexpected id", "notification_has_id"},
	{"invalid request: missing id", "request_missing_id"},
	{"invalid request: missing required", "request_missing_params"},
	{"POST requires a non-empty body", "empty_request_body"},
	{"can't send Last-Event-ID for POST", "last_event_id_on_post"},
	{"Forbidden: invalid Host header", "invalid_host"},
	{"Method Not Allowed", "method_not_allowed"},
}
