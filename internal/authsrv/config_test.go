package authsrv

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validConfig(t *testing.T) *Config {
	t.Helper()
	return &Config{
		IssuerURL:          "https://mcp.example.com",
		GoogleClientID:     "cid",
		GoogleClientSecret: "secret",
		AllowedDomains:     []string{"example.com"},
		TokenKeys:          []string{testKey(t)},
	}
}

func TestConfigValidate(t *testing.T) {
	t.Run("valid https config passes", func(t *testing.T) {
		assert.NoError(t, validConfig(t).Validate())
	})
	t.Run("trailing slash is normalized", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.IssuerURL = "https://mcp.example.com/"
		require.NoError(t, cfg.Validate())
		assert.Equal(t, "https://mcp.example.com", cfg.IssuerURL)
	})
	t.Run("project access gate instead of domains", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.AllowedDomains = nil
		cfg.RequireProjectAccess = "obs-project"
		assert.NoError(t, cfg.Validate())
	})
	t.Run("project choice alone is a valid gate", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.AllowedDomains = nil
		cfg.AllowProjectChoice = true
		assert.NoError(t, cfg.Validate())
	})
	t.Run("http localhost allowed for development", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.IssuerURL = "http://localhost:8080"
		assert.NoError(t, cfg.Validate())
	})

	fail := func(name string, mutate func(*Config), wantSubstr string) {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig(t)
			mutate(cfg)
			err := cfg.Validate()
			require.Error(t, err)
			assert.ErrorContains(t, err, wantSubstr)
		})
	}

	fail("http non-loopback issuer", func(c *Config) { c.IssuerURL = "http://intranet.example" }, "must be https")
	fail("issuer with query", func(c *Config) { c.IssuerURL = "https://mcp.example.com?x=1" }, "query or fragment")
	fail("issuer with fragment", func(c *Config) { c.IssuerURL = "https://mcp.example.com#frag" }, "query or fragment")
	fail("missing client id", func(c *Config) { c.GoogleClientID = "" }, "client ID is required")
	fail("missing client secret", func(c *Config) { c.GoogleClientSecret = "" }, "client secret is required")
	fail("no allowed domains", func(c *Config) { c.AllowedDomains = nil }, "allowed domain")
	fail("empty domain entry", func(c *Config) { c.AllowedDomains = []string{" "} }, "empty entries")
	fail("project choice with pinned project", func(c *Config) {
		c.RequireProjectAccess = "p-1234"
		c.AllowProjectChoice = true
	}, "mutually exclusive")
	fail("project choice with skip-consent", func(c *Config) {
		c.AllowProjectChoice = true
		c.SkipConsent = true
	}, "consent")
	fail("no token keys", func(c *Config) { c.TokenKeys = nil }, "token key")
	fail("bad token key", func(c *Config) { c.TokenKeys = []string{"nope"} }, "invalid token keys")
	fail("negative refresh TTL", func(c *Config) { c.RefreshTokenTTL = -time.Hour }, "must not be negative")
}

func TestNewWithProviderCopiesConfig(t *testing.T) {
	cfg := validConfig(t)
	a, err := NewWithProvider(cfg, nil, happyIdP())
	require.NoError(t, err)

	// Mutating the caller's config after construction must not affect the
	// server (the sealer AAD and metadata must stay in sync).
	cfg.IssuerURL = "https://hijacked.example"
	cfg.AllowedDomains = []string{"evil.example"}
	assert.Equal(t, "https://mcp.example.com", a.cfg.IssuerURL)
	assert.True(t, a.cfg.domainAllowed("example.com", "user@example.com"))
}

func TestDomainAllowed(t *testing.T) {
	workspace := &Config{AllowedDomains: []string{"example.com"}}
	consumer := &Config{AllowedDomains: []string{"gmail.com"}}

	// Workspace accounts match on the hd claim, case-insensitively.
	assert.True(t, workspace.domainAllowed("example.com", "user@example.com"))
	assert.True(t, workspace.domainAllowed("EXAMPLE.com", "user@example.com"))
	assert.False(t, workspace.domainAllowed("other.com", "user@other.com"))

	// A corp-domain email without hd is not a managed Workspace account:
	// the consumer fallback must not apply to non-consumer domains.
	assert.False(t, workspace.domainAllowed("", "user@example.com"))

	// Consumer accounts (no hd) match on the verified email domain, but
	// only when the allowlist explicitly opts in to a consumer domain.
	assert.True(t, consumer.domainAllowed("", "user@gmail.com"))
	assert.True(t, consumer.domainAllowed("", "user@GMAIL.com"))
	assert.False(t, consumer.domainAllowed("", "user@example.com"))
	assert.False(t, workspace.domainAllowed("", "user@gmail.com"))
	assert.False(t, consumer.domainAllowed("", "not-an-email"))
	// A forged hd claim never falls back to the email domain.
	assert.False(t, consumer.domainAllowed("example.com", "user@gmail.com"))
}
