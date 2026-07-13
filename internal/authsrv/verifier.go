package authsrv

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"golang.org/x/oauth2"
)

// extraIdentityKey is the single TokenInfo.Extra key this package sets. All
// identity material travels as one typed identityExtra value, so a value-type
// drift is a compile error rather than a silently-zero type assertion.
const extraIdentityKey = "mcp-gcp-observability/identity"

// identityExtra is the identity payload the verifier stores in
// TokenInfo.Extra and Identity/GoogleTokenSource read back.
type identityExtra struct {
	Email             string
	Domain            string
	Project           string
	GoogleAccessToken string
	GoogleExpiry      time.Time
}

// Verifier returns the auth.TokenVerifier for RequireBearerToken. It opens
// the sealed access token and exposes the user identity plus the embedded
// Google access token via TokenInfo.Extra. TokenInfo.UserID is the Google
// subject, which the streamable transport uses to bind MCP sessions to one
// user.
//
// Rejections are logged server-side (reason class only, never the token):
// a mass 401 after a botched key rotation or an issuer change must be
// diagnosable from the logs.
func (a *AuthServer) Verifier() auth.TokenVerifier {
	return func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		now := a.now()
		c, err := openBlob(a.sealer, accessBlob, token, now)
		if err != nil {
			a.logger.Warn("access token rejected", "reason", err)
			return nil, fmt.Errorf("%w: not a valid access token", auth.ErrInvalidToken)
		}
		if !now.Before(time.Unix(c.ExpiresAt, 0)) {
			return nil, fmt.Errorf("%w: token expired", auth.ErrInvalidToken)
		}
		// Re-check the domain at use time: this is the enforcement point
		// that cuts off already-issued tokens after a domain is removed
		// from the allowlist (and redeployed). RequireProjectAccess is
		// deliberately NOT probed here — an API call per MCP request — its
		// revocation path is IAM itself denying every GCP RPC, plus the
		// re-probe on refresh.
		if len(a.cfg.AllowedDomains) > 0 && !a.cfg.domainAllowed(c.Domain, c.Email) {
			a.logger.Warn("access token rejected: domain no longer allowed", "email", c.Email, "hd", c.Domain)
			return nil, fmt.Errorf("%w: domain not allowed", auth.ErrInvalidToken)
		}
		return &auth.TokenInfo{
			Scopes:     c.Scopes,
			Expiration: time.Unix(c.ExpiresAt, 0),
			UserID:     c.Subject,
			Extra: map[string]any{
				extraIdentityKey: identityExtra{
					Email:             c.Email,
					Domain:            c.Domain,
					Project:           c.Project,
					GoogleAccessToken: c.GoogleAccessToken,
					GoogleExpiry:      time.Unix(c.GoogleExpiry, 0),
				},
			},
		}, nil
	}
}

// UserIdentity describes the authenticated user of the current request.
type UserIdentity struct {
	// Subject is the stable Google account ID (`sub` claim). Use it as the
	// key for per-user resources.
	Subject string
	// Email is the user's Workspace email.
	Email string
	// Domain is the Workspace domain (`hd` claim).
	Domain string
	// Project is the GCP project this grant is bound to: the server-pinned
	// project or the one the user chose at authorization. Empty in
	// domain-only deployments without a project gate.
	Project string
}

// identityFromContext extracts the typed identity payload, if present.
func identityFromContext(ctx context.Context) (*auth.TokenInfo, identityExtra, bool) {
	info := auth.TokenInfoFromContext(ctx)
	if info == nil {
		return nil, identityExtra{}, false
	}
	extra, ok := info.Extra[extraIdentityKey].(identityExtra)
	return info, extra, ok
}

// Identity returns the authenticated user of the request, or ok=false when
// the request was not authenticated by this package (stdio transport or
// --auth none).
func Identity(ctx context.Context) (*UserIdentity, bool) {
	info, extra, ok := identityFromContext(ctx)
	if !ok || info.UserID == "" {
		return nil, false
	}
	return &UserIdentity{Subject: info.UserID, Email: extra.Email, Domain: extra.Domain, Project: extra.Project}, true
}

// GoogleTokenSource returns a TokenSource yielding the user's Google access
// token, or ok=false when the request was not authenticated by this package.
// The minting rule (access token expiry <= Google token expiry) guarantees
// the token is valid for at least the lifetime of the bearer token that
// carried it, so a static source is sufficient for request-scoped use.
func GoogleTokenSource(ctx context.Context) (oauth2.TokenSource, bool) {
	_, extra, ok := identityFromContext(ctx)
	if !ok || extra.GoogleAccessToken == "" {
		return nil, false
	}
	return oauth2.StaticTokenSource(&oauth2.Token{
		AccessToken: extra.GoogleAccessToken,
		TokenType:   "Bearer",
		Expiry:      extra.GoogleExpiry,
	}), true
}

// NewTokenInfoForTesting fabricates the TokenInfo this package's Verifier
// would produce. It exists so other packages can unit-test handlers that sit
// behind auth.RequireBearerToken (e.g. the per-user pool) without running the
// OAuth flow.
func NewTokenInfoForTesting(subject, email, domain, project, googleAccessToken string, expiry time.Time) *auth.TokenInfo {
	return &auth.TokenInfo{
		UserID:     subject,
		Expiration: expiry,
		Extra: map[string]any{
			extraIdentityKey: identityExtra{
				Email:             email,
				Domain:            domain,
				Project:           project,
				GoogleAccessToken: googleAccessToken,
				GoogleExpiry:      expiry,
			},
		},
	}
}
