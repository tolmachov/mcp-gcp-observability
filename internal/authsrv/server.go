package authsrv

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"golang.org/x/oauth2"
	googleoauth "golang.org/x/oauth2/google"
	"google.golang.org/api/idtoken"
)

// IdentityClaims are the id_token claims the server relies on.
type IdentityClaims struct {
	Subject       string
	Email         string
	EmailVerified bool
	HostedDomain  string
}

// IdentityProvider abstracts the upstream IdP. Production uses Google
// (New); tests inject fakes via NewWithProvider.
type IdentityProvider interface {
	// AuthCodeURL builds the URL the user is sent to for login.
	AuthCodeURL(state string, opts ...oauth2.AuthCodeOption) string
	// Exchange trades an authorization code for tokens.
	Exchange(ctx context.Context, code string) (*oauth2.Token, error)
	// Refresh obtains a fresh access token from a refresh token.
	Refresh(ctx context.Context, refreshToken string) (*oauth2.Token, error)
	// ValidateIDToken verifies the id_token signature and audience and
	// returns the identity claims.
	ValidateIDToken(ctx context.Context, rawIDToken string) (*IdentityClaims, error)
	// Revoke revokes an access or refresh token upstream. It returns nil
	// when the grant is dead (revoked now or already revoked) and an error
	// only for transient failures, where the grant may still be alive.
	Revoke(ctx context.Context, token string) error
}

// AuthServer is the embedded OAuth 2.1 authorization server protecting the
// MCP HTTP transport.
type AuthServer struct {
	cfg    *Config
	sealer *sealer
	policy *redirectPolicy
	idp    IdentityProvider
	access AccessChecker // present only for pinned-project admission checks
	store  oauthStateStore
	logger *slog.Logger
	now    func() time.Time
}

// New validates cfg and builds the authorization server with Google as IdP.
// logger must not be nil: the Firestore state store logs through it too.
func New(ctx context.Context, cfg *Config, logger *slog.Logger) (*AuthServer, error) {
	if cfg == nil {
		return nil, fmt.Errorf("invalid auth config: auth config must not be nil")
	}
	cfgCopy := *cfg
	cfgCopy.AllowedDomains = slices.Clone(cfg.AllowedDomains)
	cfgCopy.TokenKeys = slices.Clone(cfg.TokenKeys)
	cfgCopy.ExtraRedirects = slices.Clone(cfg.ExtraRedirects)
	cfgCopy.Scopes = slices.Clone(cfg.Scopes)
	if err := cfgCopy.Validate(); err != nil {
		return nil, fmt.Errorf("invalid auth config: %w", err)
	}
	store, err := newFirestoreStateStore(ctx, cfgCopy.StateProject, cfgCopy.StateDatabase, logger)
	if err != nil {
		return nil, err
	}
	cfgCopy.stateStore = store
	a, err := newAuthServer(&cfgCopy, logger, nil)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	a.idp = &googleIdP{cfg: a.oauth2Config()}
	return a, nil
}

// NewWithProvider is New with a custom upstream IdP. The primary consumer is
// tests (in this package and in the transport wiring) that substitute a fake
// for Google.
func NewWithProvider(cfg *Config, logger *slog.Logger, idp IdentityProvider) (*AuthServer, error) {
	if cfg == nil {
		return nil, fmt.Errorf("invalid auth config: auth config must not be nil")
	}
	cfgCopy := *cfg
	if cfgCopy.stateStore == nil {
		cfgCopy.stateStore = newMemoryStateStore()
	}
	return newAuthServer(&cfgCopy, logger, idp)
}

func newAuthServer(cfg *Config, logger *slog.Logger, idp IdentityProvider) (*AuthServer, error) {
	// Work on a private copy: a caller mutating cfg after construction must
	// not desynchronize the sealer's AAD from the metadata endpoints.
	cfgCopy := *cfg
	cfgCopy.AllowedDomains = slices.Clone(cfg.AllowedDomains)
	cfgCopy.TokenKeys = slices.Clone(cfg.TokenKeys)
	cfgCopy.ExtraRedirects = slices.Clone(cfg.ExtraRedirects)
	cfgCopy.Scopes = slices.Clone(cfg.Scopes)
	if err := cfgCopy.Validate(); err != nil {
		return nil, fmt.Errorf("invalid auth config: %w", err)
	}
	ring, err := newKeyRing(cfgCopy.TokenKeys)
	if err != nil {
		return nil, fmt.Errorf("building key ring: %w", err)
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	access := cfgCopy.accessChecker
	if access == nil && cfgCopy.PinnedProject != "" {
		access = newCRMAccessChecker()
	}
	return &AuthServer{
		cfg:    &cfgCopy,
		sealer: newSealer(ring, cfgCopy.IssuerURL),
		policy: newRedirectPolicy(cfgCopy.ExtraRedirects),
		idp:    idp,
		access: access,
		store:  cfgCopy.stateStore,
		logger: logger,
		now:    time.Now,
	}, nil
}

func (a *AuthServer) Close() error { return a.store.Close() }

func (a *AuthServer) CheckStore(ctx context.Context) error { return a.store.Health(ctx) }

// checkProjectAccess applies the pinned-project admission gate. It is
// trivially true for unpinned deployments, where delegated IAM is enforced by
// every GCP RPC instead.
func (a *AuthServer) checkProjectAccess(ctx context.Context, googleAccessToken, project string) (bool, error) {
	if a.access == nil || project == "" {
		return true, nil
	}
	return a.access.HasProjectAccess(ctx, googleAccessToken, project)
}

// oauth2Config is the Google OAuth client this service is registered as.
func (a *AuthServer) oauth2Config() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     a.cfg.GoogleClientID,
		ClientSecret: a.cfg.GoogleClientSecret,
		Endpoint:     googleoauth.Endpoint,
		RedirectURL:  a.cfg.IssuerURL + CallbackPath,
		Scopes:       a.cfg.scopes(),
	}
}

// Routes mounts every auth endpoint on mux. The MCP handler itself is mounted
// by the caller (wrapped in this server's RequireBearerToken).
func (a *AuthServer) Routes(mux *http.ServeMux) {
	mux.Handle("GET "+ProtectedResourceMetadataPath, a.protectedResourceHandler())
	mux.Handle("GET "+AuthServerMetadataPath, jsonMetadataHandler(a.authServerMetadata()))
	mux.Handle("GET "+OpenIDConfigurationPath, jsonMetadataHandler(a.authServerMetadata()))
	mux.Handle("GET "+JWKSPath, jsonMetadataHandler(emptyJWKS{}))
	mux.HandleFunc("POST "+RegisterPath, a.handleRegister)
	mux.HandleFunc("GET "+AuthorizePath, a.handleAuthorize)
	mux.HandleFunc("POST "+AuthorizeConfirmPath, a.handleAuthorizeConfirm)
	mux.HandleFunc("GET "+CallbackPath, a.handleCallback)
	mux.HandleFunc("POST "+TokenPath, a.handleToken)
	mux.HandleFunc("POST "+RevokePath, a.handleRevoke)
}

// googleIdP is the production IdentityProvider backed by accounts.google.com.
type googleIdP struct {
	cfg *oauth2.Config
}

func (g *googleIdP) AuthCodeURL(state string, opts ...oauth2.AuthCodeOption) string {
	return g.cfg.AuthCodeURL(state, opts...)
}

func (g *googleIdP) Exchange(ctx context.Context, code string) (*oauth2.Token, error) {
	tok, err := g.cfg.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("exchanging code at Google: %w", err)
	}
	return tok, nil
}

func (g *googleIdP) Refresh(ctx context.Context, refreshToken string) (*oauth2.Token, error) {
	tok, err := g.cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken}).Token()
	if err != nil {
		return nil, fmt.Errorf("refreshing token at Google: %w", err)
	}
	return tok, nil
}

func (g *googleIdP) ValidateIDToken(ctx context.Context, rawIDToken string) (*IdentityClaims, error) {
	payload, err := idtoken.Validate(ctx, rawIDToken, g.cfg.ClientID)
	if err != nil {
		return nil, fmt.Errorf("validating id_token: %w", err)
	}
	claims := &IdentityClaims{Subject: payload.Subject}
	if v, ok := payload.Claims["email"].(string); ok {
		claims.Email = v
	}
	if v, ok := payload.Claims["email_verified"].(bool); ok {
		claims.EmailVerified = v
	}
	if v, ok := payload.Claims["hd"].(string); ok {
		claims.HostedDomain = v
	}
	return claims, nil
}

// googleRevokeURL accepts both access and refresh tokens and revokes the
// whole grant lineage.
const googleRevokeURL = "https://oauth2.googleapis.com/revoke"

// Revoke revokes the token upstream. Google answers 200 on success and 400
// when the token is already invalid/revoked — both mean the grant is dead,
// so both are success here. Anything else (network failure, 5xx) means the
// grant may still be alive and is returned as an error so the caller can
// tell the client to retry.
func (g *googleIdP) Revoke(ctx context.Context, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, googleRevokeURL,
		newFormBody("token", token))
	if err != nil {
		return fmt.Errorf("building revoke request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("calling Google revoke endpoint: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close of drained body
	switch resp.StatusCode {
	case http.StatusOK, http.StatusBadRequest:
		return nil
	default:
		return fmt.Errorf("google revoke endpoint returned status %d", resp.StatusCode)
	}
}
