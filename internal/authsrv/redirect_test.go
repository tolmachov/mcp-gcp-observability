package authsrv

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRedirectPolicyRequiresExactAllowlistForHTTPSAndCustomSchemes(t *testing.T) {
	policy := newRedirectPolicy([]string{"https://client.example/callback?tenant=a", "cursor://oauth/callback"})
	assert.True(t, policy.allowed("https://client.example/callback?tenant=a"))
	assert.False(t, policy.allowed("https://client.example/callback?tenant=b"))
	assert.False(t, policy.allowed("https://unlisted.example/callback"))
	assert.True(t, policy.allowed("cursor://oauth/callback"))
	assert.False(t, policy.allowed("cursor://oauth/other"))
}
