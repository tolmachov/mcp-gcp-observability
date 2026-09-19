// Package authsrv implements the Google OAuth 2.1 authorization server for
// the HTTP MCP transport. Replay-sensitive state is persisted in Firestore.
package authsrv

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var gcpProjectIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)

var DefaultScopes = []string{"openid", "email", "https://www.googleapis.com/auth/cloud-platform"}

var defaultExtraRedirects = []string{
	"https://claude.ai/api/mcp/auth_callback",
	"https://claude.com/api/mcp/auth_callback",
}

const defaultRefreshTokenTTL = 30 * 24 * time.Hour

type Config struct {
	IssuerURL          string
	GoogleClientID     string
	GoogleClientSecret string
	AllowedDomains     []string
	PinnedProject      string
	StateProject       string
	StateDatabase      string
	TokenKeys          []string
	ExtraRedirects     []string
	Scopes             []string
	RefreshTokenTTL    time.Duration
	SkipConsent        bool

	accessChecker AccessChecker
	stateStore    oauthStateStore
}

func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("auth config must not be nil")
	}
	c.IssuerURL = strings.TrimRight(c.IssuerURL, "/")
	u, err := url.Parse(c.IssuerURL)
	if err != nil || !u.IsAbs() || u.User != nil {
		return fmt.Errorf("invalid issuer URL %q", c.IssuerURL)
	}
	if (u.Scheme != "https" || u.Host == "") && (u.Scheme != "http" || !isLoopbackHost(u.Hostname())) {
		return fmt.Errorf("issuer URL %q must be https (or http on localhost)", c.IssuerURL)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("issuer URL %q must not contain userinfo, query, or fragment", c.IssuerURL)
	}
	if c.GoogleClientID == "" || c.GoogleClientSecret == "" {
		return fmt.Errorf("google client ID and secret are required")
	}
	if c.PinnedProject == "" && len(c.AllowedDomains) == 0 {
		return fmt.Errorf("AUTH_ALLOWED_DOMAINS is required for an unpinned HTTP deployment")
	}
	if c.PinnedProject != "" && !gcpProjectIDPattern.MatchString(c.PinnedProject) {
		return fmt.Errorf("invalid GCP_DEFAULT_PROJECT %q", c.PinnedProject)
	}
	if c.stateStore == nil && c.StateProject == "" {
		return fmt.Errorf("AUTH_STATE_PROJECT is required")
	}
	if c.StateProject != "" && !gcpProjectIDPattern.MatchString(c.StateProject) {
		return fmt.Errorf("invalid AUTH_STATE_PROJECT %q", c.StateProject)
	}
	if c.StateDatabase == "" {
		c.StateDatabase = "(default)"
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
		if !validAllowlistedRedirect(r) {
			return fmt.Errorf("invalid extra redirect URI %q", r)
		}
	}
	if c.RefreshTokenTTL < 0 {
		return fmt.Errorf("refresh token TTL must not be negative")
	}
	return nil
}

func (c *Config) scopes() []string {
	if len(c.Scopes) > 0 {
		return c.Scopes
	}
	return DefaultScopes
}

func (c *Config) requiredGrantedScopes() []string {
	var out []string
	for _, s := range c.scopes() {
		if s != "openid" && s != "email" && s != "profile" {
			out = append(out, s)
		}
	}
	return out
}

func (c *Config) refreshTokenTTL() time.Duration {
	if c.RefreshTokenTTL > 0 {
		return c.RefreshTokenTTL
	}
	return defaultRefreshTokenTTL
}

func isConsumerDomain(d string) bool {
	return strings.EqualFold(d, "gmail.com") || strings.EqualFold(d, "googlemail.com")
}

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
	if at < 0 || !isConsumerDomain(email[at+1:]) {
		return false
	}
	for _, d := range c.AllowedDomains {
		if strings.EqualFold(email[at+1:], d) {
			return true
		}
	}
	return false
}
