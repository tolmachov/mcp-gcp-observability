package authsrv

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validConfig(t *testing.T) *Config {
	t.Helper()
	return &Config{IssuerURL: "https://mcp.example.com", GoogleClientID: "cid", GoogleClientSecret: "secret", AllowedDomains: []string{"example.com"}, StateProject: "state-project", TokenKeys: []string{testKey(t)}}
}

func TestConfigValidate(t *testing.T) {
	require.NoError(t, validConfig(t).Validate())
	pinned := validConfig(t)
	pinned.AllowedDomains = nil
	pinned.PinnedProject = "obs-project"
	require.NoError(t, pinned.Validate())

	fail := func(name string, mutate func(*Config), want string) {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig(t)
			mutate(cfg)
			err := cfg.Validate()
			require.Error(t, err)
			assert.ErrorContains(t, err, want)
		})
	}
	fail("non-loopback HTTP", func(c *Config) { c.IssuerURL = "http://example.com" }, "must be https")
	fail("userinfo", func(c *Config) { c.IssuerURL = "https://u@example.com" }, "invalid issuer")
	fail("unpinned domain gate", func(c *Config) { c.AllowedDomains = nil }, "AUTH_ALLOWED_DOMAINS")
	fail("state project", func(c *Config) { c.StateProject = "" }, "AUTH_STATE_PROJECT")
	fail("invalid state project", func(c *Config) { c.StateProject = "NOT_A_PROJECT" }, "invalid AUTH_STATE_PROJECT")
	fail("invalid pinned project", func(c *Config) { c.PinnedProject = "NOT_A_PROJECT" }, "invalid GCP_DEFAULT_PROJECT")
	fail("token keys", func(c *Config) { c.TokenKeys = nil }, "token key")
	fail("negative TTL", func(c *Config) { c.RefreshTokenTTL = -time.Hour }, "negative")
	fail("redirect fragment", func(c *Config) { c.ExtraRedirects = []string{"https://app.example/cb#x"} }, "redirect")
}

func TestNewWithProviderCopiesConfig(t *testing.T) {
	cfg := validConfig(t)
	a, err := NewWithProvider(cfg, nil, happyIdP())
	require.NoError(t, err)
	cfg.IssuerURL = "https://hijacked.example"
	cfg.AllowedDomains[0] = "evil.example"
	assert.Equal(t, "https://mcp.example.com", a.cfg.IssuerURL)
	assert.True(t, a.cfg.domainAllowed("example.com", "user@example.com"))
}

func TestDomainAllowed(t *testing.T) {
	workspace := &Config{AllowedDomains: []string{"example.com"}}
	consumer := &Config{AllowedDomains: []string{"gmail.com"}}
	assert.True(t, workspace.domainAllowed("EXAMPLE.com", "user@example.com"))
	assert.False(t, workspace.domainAllowed("", "user@example.com"))
	assert.True(t, consumer.domainAllowed("", "user@gmail.com"))
}
