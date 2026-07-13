// Package authsrv implements the embedded OAuth 2.1 authorization server that
// protects the MCP HTTP transport. The server plays both RFC 9728 roles at
// once: it is the MCP protected resource and the authorization server that
// MCP clients discover, register against (RFC 7591), and obtain tokens from.
// Google (accounts.google.com) is the upstream identity provider: users
// authenticate with their Google account and grant Cloud access on their own
// behalf (bounded by their IAM permissions),
// and every artifact the server issues (authorization code, access token,
// refresh token, OAuth state, client ID) is a self-contained sealed blob, so
// no server-side session or token storage is required.
package authsrv

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Mode selects how the HTTP transport authenticates callers.
type Mode string

const (
	// ModeNone disables authentication (default).
	ModeNone Mode = "none"
	// ModeGoogle enables the embedded OAuth flow with Google as IdP.
	ModeGoogle Mode = "google"
)

// DefaultScopes are the Google OAuth scopes requested during login.
// The full cloud-platform scope is required: the Error Reporting and
// Profiler APIs reject cloud-platform.read-only (they accept no narrower
// scope at all), which surfaces as ACCESS_TOKEN_SCOPE_INSUFFICIENT on
// errors_*/profiler_* tools. The scope is not the authorization boundary —
// every call runs with the user's own token, so per-user IAM decides what
// can actually be read.
var DefaultScopes = []string{
	"openid",
	"email",
	"https://www.googleapis.com/auth/cloud-platform",
}

// defaultExtraRedirects are HTTPS redirect URIs accepted in addition to
// loopback URIs: the claude.ai / Claude Desktop connector callbacks.
var defaultExtraRedirects = []string{
	"https://claude.ai/api/mcp/auth_callback",
	"https://claude.com/api/mcp/auth_callback",
}

// defaultRefreshTokenTTL bounds how long a refresh token may be used before
// the user must log in again.
const defaultRefreshTokenTTL = 30 * 24 * time.Hour

// Config holds the settings of the embedded authorization server.
type Config struct {
	// IssuerURL is the public base URL of this service (no trailing slash),
	// e.g. "https://mcp-gcp-observability-xyz.a.run.app". It is the OAuth
	// issuer, the MCP resource identifier, and the AAD binding of all
	// sealed blobs.
	IssuerURL string
	// GoogleClientID / GoogleClientSecret identify the pre-registered Google
	// OAuth web client whose redirect URI is {IssuerURL}/callback.
	GoogleClientID     string
	GoogleClientSecret string
	// AllowedDomains lists Workspace domains (id_token `hd` claim) allowed
	// to log in, e.g. ["example.com"]. Optional when RequireProjectAccess
	// is set; at least one of the two must be configured.
	AllowedDomains []string
	// RequireProjectAccess, when set to a GCP project ID, requires every
	// account to hold resourcemanager.projects.get on that project — probed
	// via testIamPermissions with the user's own token — at login and on
	// every refresh. This gates access by IAM membership instead of (or in
	// addition to) a Workspace domain: both checks must pass when both are
	// configured. Per-request enforcement stays with IAM itself, which
	// evaluates the user's token on every GCP RPC.
	RequireProjectAccess string
	// AllowProjectChoice, when true, asks the user for a GCP project ID on
	// the consent page instead of pinning one server-side. Access to the
	// chosen project is verified the same way (testIamPermissions with the
	// user's token) at login and on every refresh, and the choice is
	// exposed as UserIdentity.Project so the per-user assembly can target
	// it. Mutually exclusive with RequireProjectAccess and SkipConsent.
	AllowProjectChoice bool
	// accessChecker overrides the Cloud Resource Manager prober in tests.
	accessChecker AccessChecker
	// TokenKeys are base64-encoded 32-byte master keys. The first key seals
	// new blobs; all keys can open existing ones, enabling rotation without
	// a mass logout.
	TokenKeys []string
	// ExtraRedirects are additional exact-match redirect URIs accepted at
	// registration on top of the defaults (claude.ai/claude.com callbacks)
	// and the always-allowed loopback URIs.
	ExtraRedirects []string
	// Scopes are the Google OAuth scopes requested during login.
	// Empty means DefaultScopes.
	Scopes []string
	// RefreshTokenTTL caps refresh-token lifetime. Zero means the default
	// (30 days).
	RefreshTokenTTL time.Duration
	// SkipConsent disables the consent interstitial on /authorize and
	// redirects straight to Google. Development only: the interstitial is
	// the confused-deputy defense for silently-registered clients.
	SkipConsent bool
}

// Validate checks the configuration and normalizes IssuerURL.
func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("auth config must not be nil")
	}
	c.IssuerURL = strings.TrimRight(c.IssuerURL, "/")
	u, err := url.Parse(c.IssuerURL)
	if err != nil {
		return fmt.Errorf("invalid issuer URL %q: %w", c.IssuerURL, err)
	}
	switch {
	case u.Scheme == "https" && u.Host != "":
	case u.Scheme == "http" && isLoopbackHost(u.Hostname()):
		// http is acceptable only for local development.
	default:
		return fmt.Errorf("issuer URL %q must be https (or http on localhost)", c.IssuerURL)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("issuer URL %q must not contain a query or fragment", c.IssuerURL)
	}
	if c.GoogleClientID == "" {
		return fmt.Errorf("google client ID is required")
	}
	if c.GoogleClientSecret == "" {
		return fmt.Errorf("google client secret is required")
	}
	if len(c.AllowedDomains) == 0 && c.RequireProjectAccess == "" && !c.AllowProjectChoice {
		return fmt.Errorf("at least one login gate is needed: allowed domains, a required-access project, or project choice")
	}
	if c.AllowProjectChoice && c.RequireProjectAccess != "" {
		return fmt.Errorf("project choice and a required-access project are mutually exclusive")
	}
	if c.AllowProjectChoice && c.SkipConsent {
		return fmt.Errorf("project choice requires the consent page (disable skip-consent)")
	}
	for _, d := range c.AllowedDomains {
		if strings.TrimSpace(d) == "" {
			return fmt.Errorf("allowed domains must not contain empty entries")
		}
	}
	if len(c.TokenKeys) == 0 {
		return fmt.Errorf("at least one token key is required")
	}
	if _, err := newKeyRing(c.TokenKeys); err != nil {
		return fmt.Errorf("invalid token keys: %w", err)
	}
	for _, r := range c.ExtraRedirects {
		if _, err := url.Parse(r); err != nil {
			return fmt.Errorf("invalid extra redirect URI %q: %w", r, err)
		}
	}
	if c.RefreshTokenTTL < 0 {
		return fmt.Errorf("refresh token TTL must not be negative")
	}
	return nil
}

// scopes returns the configured Google scopes, defaulting to DefaultScopes.
func (c *Config) scopes() []string {
	if len(c.Scopes) > 0 {
		return c.Scopes
	}
	return DefaultScopes
}

// requiredGrantedScopes lists the scopes the user must actually grant on the
// Google consent screen. Google's granular consent lets a user untick
// individual sensitive scopes; the OIDC identity scopes are granted
// implicitly by signing in and are not individually tickable.
func (c *Config) requiredGrantedScopes() []string {
	var out []string
	for _, s := range c.scopes() {
		if s == "openid" || s == "email" || s == "profile" {
			continue
		}
		out = append(out, s)
	}
	return out
}

// refreshTokenTTL returns the configured refresh-token TTL or the default.
func (c *Config) refreshTokenTTL() time.Duration {
	if c.RefreshTokenTTL > 0 {
		return c.RefreshTokenTTL
	}
	return defaultRefreshTokenTTL
}

// isConsumerDomain reports whether d is a Google consumer-account email
// domain. Consumer accounts never carry an `hd` claim.
func isConsumerDomain(d string) bool {
	return strings.EqualFold(d, "gmail.com") || strings.EqualFold(d, "googlemail.com")
}

// domainAllowed reports whether an account is allowed to log in, given its
// Workspace domain (`hd` claim; empty for consumer accounts) and its
// Google-verified email address. Workspace accounts match on hd. Consumer
// accounts fall back to the email domain, but only when the matching
// allowlist entry is itself a consumer domain: listing gmail.com explicitly
// opts in to personal Google accounts (evaluation deployments), while a bare
// corp-domain email without hd is not a managed Workspace account and stays
// rejected.
func (c *Config) domainAllowed(hd, email string) bool {
	if hd != "" {
		for _, d := range c.AllowedDomains {
			if strings.EqualFold(hd, d) {
				return true
			}
		}
		return false
	}
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return false
	}
	emailDomain := email[at+1:]
	if !isConsumerDomain(emailDomain) {
		return false
	}
	for _, d := range c.AllowedDomains {
		if strings.EqualFold(emailDomain, d) {
			return true
		}
	}
	return false
}
