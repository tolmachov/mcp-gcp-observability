package authsrv

import "time"

// Artifact lifetimes. Authorization codes are single-shot by protocol but
// cannot be marked used statelessly, so their TTL is aggressive and PKCE is
// mandatory. Access tokens are additionally capped at the embedded Google
// token's expiry when minted.
const (
	stateTTL       = 10 * time.Minute
	codeTTL        = 60 * time.Second
	accessTokenTTL = 55 * time.Minute
)

// stateClaims ride the OAuth `state` parameter to Google and back. They carry
// everything /callback needs to resume the client's authorization request.
type stateClaims struct {
	ClientID      string `json:"cid"`
	RedirectURI   string `json:"ru"`
	ClientState   string `json:"st,omitempty"`
	CodeChallenge string `json:"cc"`
	Resource      string `json:"res,omitempty"`
	Project       string `json:"prj,omitempty"`
	IssuedAt      int64  `json:"iat"`
}

// codeClaims is the sealed authorization code handed to the client's
// redirect URI. It embeds the freshly obtained Google tokens so /token can
// mint our tokens without any lookup.
type codeClaims struct {
	Subject            string   `json:"sub"`
	Email              string   `json:"eml"`
	Domain             string   `json:"hd"`
	ClientID           string   `json:"cid"`
	RedirectURI        string   `json:"ru"`
	CodeChallenge      string   `json:"cc"`
	Resource           string   `json:"res,omitempty"`
	Project            string   `json:"prj,omitempty"`
	Scopes             []string `json:"scp,omitempty"`
	GoogleAccessToken  string   `json:"gat"`
	GoogleExpiry       int64    `json:"gexp"`
	GoogleRefreshToken string   `json:"grt,omitempty"`
	IssuedAt           int64    `json:"iat"`
}

// accessClaims is the payload of our bearer access token (mcp_at_...). It
// carries the user's Google access token; the mint rule ExpiresAt <=
// GoogleExpiry guarantees the embedded token outlives the bearer token, so a
// static per-request TokenSource is always valid.
type accessClaims struct {
	Subject           string   `json:"sub"`
	Email             string   `json:"eml"`
	Domain            string   `json:"hd"`
	ClientID          string   `json:"cid"`
	Resource          string   `json:"res,omitempty"`
	Project           string   `json:"prj,omitempty"`
	Scopes            []string `json:"scp,omitempty"`
	GoogleAccessToken string   `json:"gat"`
	GoogleExpiry      int64    `json:"gexp"`
	IssuedAt          int64    `json:"iat"`
	ExpiresAt         int64    `json:"exp"`
}

// refreshClaims is the payload of our refresh token (mcp_rt_...). Only it
// carries the long-lived Google refresh token, and it is only ever presented
// to /token — a smaller blast radius than embedding it in every MCP request.
type refreshClaims struct {
	Subject            string   `json:"sub"`
	Email              string   `json:"eml"`
	Domain             string   `json:"hd"`
	ClientID           string   `json:"cid"`
	Resource           string   `json:"res,omitempty"`
	Project            string   `json:"prj,omitempty"`
	Scopes             []string `json:"scp,omitempty"`
	GoogleRefreshToken string   `json:"grt"`
	IssuedAt           int64    `json:"iat"`
}

// clientIDClaims is the HMAC-signed payload of a DCR client_id (mcp_cid_...).
// Registered redirect URIs travel inside the client_id itself, giving
// /authorize exact-match validation with no registration store.
type clientIDClaims struct {
	RedirectURIs []string `json:"ru"`
	ClientName   string   `json:"name,omitempty"`
	IssuedAt     int64    `json:"iat"`
}

// expired reports whether a moment iat+ttl has passed at time now.
func expired(iat int64, ttl time.Duration, now time.Time) bool {
	return now.After(time.Unix(iat, 0).Add(ttl))
}

// issuedAtCarrier is implemented by every claims struct so openBlob can
// enforce spec TTLs generically.
type issuedAtCarrier interface{ issuedAt() int64 }

func (c stateClaims) issuedAt() int64   { return c.IssuedAt }
func (c codeClaims) issuedAt() int64    { return c.IssuedAt }
func (c accessClaims) issuedAt() int64  { return c.IssuedAt }
func (c refreshClaims) issuedAt() int64 { return c.IssuedAt }
