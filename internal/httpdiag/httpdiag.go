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

// Reject attaches an application-owned error code and static explanation to
// the enclosing HTTP diagnostic. Callers must never pass user input or errors
// from upstream services as the explanation.
func Reject(w http.ResponseWriter, code, reason string) {
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
// directly to the client and are neither buffered nor logged here.
func Handler(logger *slog.Logger, next http.Handler) http.Handler {
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
				"request_id", id, "http_method", safeHTTPMethod(r.Method),
				"route", safeRoute(r.URL.Path), "status", rw.status,
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
		"tasks/get", "tasks/list", "tasks/result", "tasks/cancel":
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

func safeRoute(path string) string {
	switch path {
	case "/", "/mcp", "/token", "/register", "/revoke", "/authorize", "/authorize/confirm",
		"/callback", "/readyz", "/healthz", "/__candidate/readyz",
		"/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource":
		return path
	default:
		return "other"
	}
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
// Match their static prefixes and emit only our own reason classes. Never log
// an unknown body verbatim, even if it is short or looks like a normal error.
func classifyError(body string) string {
	var rpc struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &rpc) == nil && rpc.Error != nil {
		body = rpc.Error.Message
	}
	for _, rule := range rejectionReasons {
		if strings.HasPrefix(body, rule.prefix) {
			return rule.reason
		}
	}
	return "unclassified_http_error"
}

var rejectionReasons = []struct{ prefix, reason string }{
	{"Accept must contain both", "accept_requires_json_and_sse"},
	{"Accept must contain 'text/event-stream'", "accept_requires_sse"},
	{"Content-Type must be", "unsupported_content_type"},
	{"Bad Request: Unsupported protocol version", "unsupported_protocol_version"},
	{"missing required Mcp-Method header", "missing_mcp_method_header"},
	{"missing required Mcp-Name header", "missing_mcp_name_header"},
	{"header mismatch: Mcp-Method", "mcp_method_header_mismatch"},
	{"header mismatch: Mcp-Name", "mcp_name_header_mismatch"},
	{"failed to extract name from parameters", "invalid_mcp_name_parameters"},
	{"JSON-RPC batching is not supported", "jsonrpc_batch_not_supported"},
	{"malformed payload:", "malformed_jsonrpc"},
	{"JSON RPC not handled:", "unsupported_rpc_method"},
	{"invalid request: unexpected id", "notification_has_id"},
	{"invalid request: missing id", "request_missing_id"},
	{"invalid request: missing required", "request_missing_params"},
	{"POST requires a non-empty body", "empty_request_body"},
	{"can't send Last-Event-ID for POST", "last_event_id_on_post"},
	{"malformed Last-Event-ID", "malformed_last_event_id"},
	{"Bad Request: DELETE requires", "delete_missing_session_id"},
	{"Bad Request: GET requires", "get_missing_session_id"},
	{"no bearer token", "missing_bearer_token"},
	{"invalid token:", "invalid_access_token"},
	{"token expired", "access_token_expired"},
	{"token missing expiration", "access_token_missing_expiration"},
	{"insufficient scope", "insufficient_scope"},
	{"request body exceeds", "request_body_too_large"},
	{"failed to read", "request_body_read_failed"},
	{"cross-origin request detected", "cross_origin_rejected"},
	{"Forbidden: invalid Host header", "invalid_host"},
	{"Method Not Allowed", "method_not_allowed"},
	{"Method not allowed", "method_not_allowed"},
	{"unsupported method", "method_not_allowed"},
	{"session not found", "session_not_found"},
	{"session user mismatch", "session_user_mismatch"},
	{"session is closing", "session_closing"},
	{"no server available", "no_server_available"},
	{"failed connection", "mcp_connection_failed"},
	{"transport not connected", "transport_not_connected"},
	{"stream replay unsupported", "stream_replay_unsupported"},
	{"stream ID conflicts", "stream_conflict"},
	{"failed to replay events", "stream_replay_failed"},
	{"storing stream:", "stream_store_failed"},
	{"OAuth state store unavailable", "oauth_store_unavailable"},
}
