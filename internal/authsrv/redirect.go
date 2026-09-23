package authsrv

import (
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// redirectPolicy decides which redirect URIs a client may register and use.
// Three tiers (RFC 8252 §7.3 for the first):
//  1. Loopback HTTP on any port/path — native clients (Claude Code, Cursor,
//     MCP inspector) bind an ephemeral localhost port per flow.
//  2. Exact-match HTTPS entries — hosted clients (claude.ai / Claude Desktop
//     connector callbacks by default, extendable via config).
//  3. Custom schemes (e.g. cursor://...) — only if explicitly listed.
type redirectPolicy struct {
	exact []string
}

// newRedirectPolicy builds the policy from the built-in defaults plus
// configured extras (exact HTTPS or custom-scheme URIs).
func newRedirectPolicy(extra []string) *redirectPolicy {
	return &redirectPolicy{exact: slices.Concat(defaultExtraRedirects, extra)}
}

// allowed reports whether a single redirect URI is acceptable.
func (p *redirectPolicy) allowed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.User != nil || u.Fragment != "" || u.Host == "" && u.Opaque == "" {
		return false
	}
	if u.Scheme == "http" {
		return isLoopbackHost(u.Hostname())
	}
	return slices.Contains(p.exact, raw)
}

func validAllowlistedRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.User != nil || u.Fragment != "" {
		return false
	}
	if u.Scheme == "http" {
		return u.Host != "" && isLoopbackHost(u.Hostname())
	}
	if u.Scheme == "https" {
		return u.Host != ""
	}
	return u.Scheme != "" && (u.Host != "" || u.Opaque != "" || u.Path != "")
}

// isLoopbackHost reports whether host is localhost or a loopback IP literal.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// matchRegistered reports whether the redirect_uri presented at /authorize
// matches one of the URIs registered for the client. Comparison is exact
// (OAuth 2.1), with one carve-out required by RFC 8252 §7.3: for loopback
// HTTP URIs the port may differ from the registered one, because native
// clients bind a fresh ephemeral port for every flow.
func matchRegistered(registered []string, raw string) bool {
	if slices.Contains(registered, raw) {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || !isLoopbackHost(u.Hostname()) || u.User != nil || u.Fragment != "" {
		return false
	}
	for _, r := range registered {
		ru, err := url.Parse(r)
		if err != nil || ru.Scheme != "http" || !isLoopbackHost(ru.Hostname()) {
			continue
		}
		if ru.User == nil && ru.Fragment == "" && strings.EqualFold(ru.Hostname(), u.Hostname()) &&
			ru.Path == u.Path && ru.RawQuery == u.RawQuery {
			return true
		}
	}
	return false
}

// redirectWithParams redirects to redirectURI with params, plus state when
// non-empty, merged into its query. Callers pass only redirect URIs already
// validated against the client's registration and the redirect policy (see
// handleAuthorize) or recovered from encrypted server-side state (see
// handleCallback).
func redirectWithParams(w http.ResponseWriter, r *http.Request, redirectURI, state string, params url.Values) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "invalid redirect", http.StatusBadRequest)
		return
	}
	q := u.Query()
	maps.Copy(q, params)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound) //nolint:gosec // G710: pre-validated redirect target
}
