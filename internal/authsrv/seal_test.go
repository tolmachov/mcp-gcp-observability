package authsrv

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, masterKeyLen)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(b)
}

func testSealer(t *testing.T, keys ...string) *sealer {
	t.Helper()
	if len(keys) == 0 {
		keys = []string{testKey(t)}
	}
	ring, err := newKeyRing(keys)
	require.NoError(t, err)
	return newSealer(ring, "https://issuer.example")
}

func TestSealedArtifactsRoundTripAndSeparateKinds(t *testing.T) {
	s := testSealer(t)
	now := time.Now()
	in := accessClaims{Subject: "sub", FamilyID: "family", GoogleAccessToken: "g", IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix()}
	blob, err := sealBlob(s, accessBlob, in)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(blob, "mcp_at_v2_"))
	out, err := openBlob(s, accessBlob, blob, now)
	require.NoError(t, err)
	assert.Equal(t, in, out)
	_, err = openBlob(s, storedGrantBlob, blob, now)
	assert.ErrorIs(t, err, errInvalidBlob)
}

func TestStateExpiryAndKeyRotation(t *testing.T) {
	now := time.Now()
	oldKey, newKey := testKey(t), testKey(t)
	old := testSealer(t, oldKey)
	blob, err := sealBlob(old, stateBlob, stateClaims{ClientID: "c", IssuedAt: now.Unix()})
	require.NoError(t, err)
	rotated := testSealer(t, newKey, oldKey)
	_, err = openBlob(rotated, stateBlob, blob, now)
	require.NoError(t, err)
	dropped := testSealer(t, newKey)
	_, err = openBlob(dropped, stateBlob, blob, now)
	assert.ErrorIs(t, err, errInvalidBlob)
	_, err = openBlob(rotated, stateBlob, blob, now.Add(2*stateTTL))
	assert.ErrorIs(t, err, errBlobExpired)
}

func TestClientIDVersionAndSignature(t *testing.T) {
	s := testSealer(t)
	id, err := s.signClientID(clientIDClaims{RedirectURIs: []string{"http://localhost/cb"}, IssuedAt: time.Now().Unix()})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(id, "mcp_cid_v2_"))
	var claims clientIDClaims
	require.NoError(t, s.verifyClientID(id, &claims))
	assert.Equal(t, []string{"http://localhost/cb"}, claims.RedirectURIs)
}
