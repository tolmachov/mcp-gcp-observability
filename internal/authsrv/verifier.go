package authsrv

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"golang.org/x/oauth2"
)

// extraIdentityKey is the single TokenInfo.Extra key this package sets. All
// identity material travels as one typed identityExtra value, so a value-type
// drift is a compile error rather than a silently-zero type assertion.
const extraIdentityKey = "mcp-gcp-observability/identity"

// identityExtra is the identity payload verifyAccessToken stores in
// TokenInfo.Extra and Identity/GoogleTokenSource read back.
type identityExtra struct {
	Email             string
	Domain            string
	GoogleAccessToken string
	GoogleExpiry      time.Time
}

// errStoreUnavailable marks a verification that could not consult the grant
// store; RequireBearerToken answers it with 503 instead of 401.
var errStoreUnavailable = errors.New("OAuth state store unavailable")

// RequireBearerToken guards next with the SDK bearer-token middleware backed
// by this server's sealed access tokens. Each request's token is verified
// once, before the SDK middleware runs, so an unavailable grant store is
// answered with 503 and Retry-After (the SDK maps every non-token verifier
// error to 500) while token rejections keep the SDK's 401 and
// WWW-Authenticate handling.
func (a *AuthServer) RequireBearerToken(opts *auth.RequireBearerTokenOptions) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var info *auth.TokenInfo
			var verifyErr error
			// Same header parsing as the SDK middleware, which rejects any
			// other shape as "no bearer token" without calling the verifier.
			fields := strings.Fields(r.Header.Get("Authorization"))
			if len(fields) == 2 && strings.EqualFold(fields[0], "bearer") {
				info, verifyErr = a.verifyAccessToken(r.Context(), fields[1])
				if errors.Is(verifyErr, errStoreUnavailable) {
					w.Header().Set("Retry-After", "5")
					http.Error(w, errStoreUnavailable.Error(), http.StatusServiceUnavailable)
					return
				}
			}
			verified := func(context.Context, string, *http.Request) (*auth.TokenInfo, error) {
				return info, verifyErr
			}
			auth.RequireBearerToken(verified, opts)(next).ServeHTTP(w, r)
		})
	}
}

// verifyAccessToken opens the sealed access token and exposes the user
// identity plus the embedded Google access token via TokenInfo.Extra.
// TokenInfo.UserID is the Google subject, which the streamable transport uses
// to bind a reusable GCP client pool to one user. MCP transport requests
// themselves remain stateless.
//
// The grant is read before any other check, so a store outage is reported
// as errStoreUnavailable for every well-formed token, expired or not.
// Rejections are logged server-side (reason class only, never the token):
// a mass 401 after a botched key rotation or an issuer change must be
// diagnosable from the logs.
func (a *AuthServer) verifyAccessToken(ctx context.Context, token string) (*auth.TokenInfo, error) {
	now := a.now()
	c, err := openBlob(a.sealer, accessBlob, token, now)
	if err != nil {
		a.logger.Warn("access token rejected", "reason", err)
		return nil, fmt.Errorf("%w: not a valid access token", auth.ErrInvalidToken)
	}
	grant, grantErr := a.grant(ctx, c.FamilyID)
	if grantErr != nil && !errors.Is(grantErr, errStateNotFound) {
		a.logStoreFailure(ctx, "verify_grant", grantErr)
		return nil, fmt.Errorf("%w: %w", errStoreUnavailable, grantErr)
	}
	if !now.Before(time.Unix(c.ExpiresAt, 0)) {
		return nil, fmt.Errorf("%w: token expired", auth.ErrInvalidToken)
	}
	if grantErr != nil {
		return nil, fmt.Errorf("%w: grant not found", auth.ErrInvalidToken)
	}
	if grant.Status != "active" || !now.Before(grant.ExpiresAt) {
		return nil, fmt.Errorf("%w: grant revoked or expired", auth.ErrInvalidToken)
	}
	// Re-check the domain at use time: this is the enforcement point
	// that cuts off already-issued tokens after a domain is removed
	// from the allowlist (and redeployed). Project IAM remains enforced by
	// every delegated GCP RPC, with an extra pinned-project probe on refresh.
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
				GoogleAccessToken: c.GoogleAccessToken,
				GoogleExpiry:      time.Unix(c.GoogleExpiry, 0),
			},
		},
	}, nil
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
// the request was not authenticated by this package (stdio transport).
func Identity(ctx context.Context) (*UserIdentity, bool) {
	info, extra, ok := identityFromContext(ctx)
	if !ok || info.UserID == "" {
		return nil, false
	}
	return &UserIdentity{Subject: info.UserID, Email: extra.Email, Domain: extra.Domain}, true
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

// NewTokenInfoForTesting is test-only. It fabricates the TokenInfo
// verifyAccessToken produces, so other packages can unit-test handlers that
// sit behind RequireBearerToken (e.g. the per-user pool) without running the
// OAuth flow.
func NewTokenInfoForTesting(subject, email, domain, googleAccessToken string, expiry time.Time) *auth.TokenInfo {
	return &auth.TokenInfo{
		UserID:     subject,
		Expiration: expiry,
		Extra: map[string]any{
			extraIdentityKey: identityExtra{
				Email:             email,
				Domain:            domain,
				GoogleAccessToken: googleAccessToken,
				GoogleExpiry:      expiry,
			},
		},
	}
}
