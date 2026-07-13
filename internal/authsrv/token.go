package authsrv

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// tokenResponse is the RFC 6749 §5.1 success payload.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

// oauthErrorResponse is the RFC 6749 §5.2 error payload.
type oauthErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// maxFormBody bounds the form bodies of the auth endpoints. Sealed refresh
// tokens are ~1-2 KB, so 64 KiB is generous.
const maxFormBody = 64 << 10

// handleToken implements the token endpoint for the authorization_code and
// refresh_token grants. All clients are public: no client authentication,
// PKCE is the proof of possession. The parsed form is passed down explicitly:
// the body size is bounded exactly once, here.
func (a *AuthServer) handleToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
	if err := r.ParseForm(); err != nil {
		a.tokenError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	form := r.PostForm
	switch form.Get("grant_type") {
	case "authorization_code":
		a.tokenFromCode(w, form)
	case "refresh_token":
		a.tokenFromRefresh(w, r, form)
	default:
		a.tokenError(w, http.StatusBadRequest, "unsupported_grant_type",
			"supported grant types: authorization_code, refresh_token")
	}
}

// tokenFromCode redeems a sealed authorization code.
func (a *AuthServer) tokenFromCode(w http.ResponseWriter, form url.Values) {
	now := a.now()
	cc, err := openBlob(a.sealer, codeBlob, form.Get("code"), now)
	if err != nil {
		a.logger.Warn("authorization code rejected", "reason", err)
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired authorization code")
		return
	}
	if form.Get("client_id") != cc.ClientID {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "client_id mismatch")
		return
	}
	if ru := form.Get("redirect_uri"); ru != "" && ru != cc.RedirectURI {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri mismatch")
		return
	}
	if !verifyPKCE(form.Get("code_verifier"), cc.CodeChallenge) {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}
	if res := form.Get("resource"); res != "" && strings.TrimRight(res, "/") != a.cfg.IssuerURL {
		a.tokenError(w, http.StatusBadRequest, "invalid_target", "unknown resource")
		return
	}

	a.mintTokens(w, mintInput{
		Subject: cc.Subject, Email: cc.Email, Domain: cc.Domain,
		ClientID: cc.ClientID, Resource: cc.Resource, Project: cc.Project,
		Scopes:            cc.Scopes,
		GoogleAccessToken: cc.GoogleAccessToken, GoogleExpiry: cc.GoogleExpiry,
		GoogleRefreshToken: cc.GoogleRefreshToken, LoginAt: now.Unix(),
	})
}

// tokenFromRefresh redeems a sealed refresh token: the embedded Google
// refresh token buys a fresh Google access token, which is re-wrapped into a
// fresh access/refresh token pair. The original login time is preserved so
// the refresh-token TTL is absolute.
func (a *AuthServer) tokenFromRefresh(w http.ResponseWriter, r *http.Request, form url.Values) {
	rc, err := openBlob(a.sealer, refreshBlob, form.Get("refresh_token"), a.now())
	if err != nil {
		a.logger.Warn("refresh token rejected", "reason", err)
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "invalid refresh token")
		return
	}
	if expired(rc.IssuedAt, a.cfg.refreshTokenTTL(), a.now()) {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "refresh token expired, log in again")
		return
	}
	if cid := form.Get("client_id"); cid != "" && cid != rc.ClientID {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "client_id mismatch")
		return
	}
	// Enforce the domain allowlist at the token endpoint too, so a removed
	// domain stops minting fresh tokens instead of relying solely on the
	// verifier rejecting them at use.
	if len(a.cfg.AllowedDomains) > 0 && !a.cfg.domainAllowed(rc.Domain, rc.Email) {
		a.logger.Warn("refresh rejected: domain no longer allowed", "email", rc.Email, "hd", rc.Domain)
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "account domain is not allowed")
		return
	}

	tok, err := a.idp.Refresh(r.Context(), rc.GoogleRefreshToken)
	if err != nil {
		if re, ok := errors.AsType[*oauth2.RetrieveError](err); ok {
			// The user revoked access or the Workspace admin disabled the
			// app; the client must restart the flow. Log the full response —
			// this is the line an operator reads when logins break.
			a.logger.Warn("google refresh rejected", "email", rc.Email, "err", re)
			a.tokenError(w, http.StatusBadRequest, "invalid_grant", "upstream grant revoked, log in again")
			return
		}
		a.logger.Error("google refresh failed", "email", rc.Email, "err", err)
		a.tokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "upstream token refresh failed")
		return
	}

	// Re-probe project access with the fresh Google token: revoking a
	// user's IAM role cuts off token renewal, not just individual GCP RPCs.
	// The pinned project is authoritative when configured (a redeploy that
	// changes the pin re-gates old grants); otherwise the user's choice
	// sealed in the refresh token is re-checked.
	project := rc.Project
	if !a.cfg.AllowProjectChoice {
		project = a.cfg.RequireProjectAccess
	} else if project == "" {
		// A grant minted before project choice was enabled carries no
		// project; its assembly could never build. Force a clean re-login.
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "grant predates project selection, log in again")
		return
	}
	hasAccess, err := a.checkProjectAccess(r.Context(), tok.AccessToken, project)
	if err != nil {
		a.logger.Error("project access check failed on refresh", "email", rc.Email, "err", err)
		a.tokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "access check failed, try again")
		return
	}
	if !hasAccess {
		a.logger.Warn("refresh rejected: no access to project", "email", rc.Email, "project", project)
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "account no longer has access to the GCP project")
		return
	}

	refreshToken := rc.GoogleRefreshToken
	if tok.RefreshToken != "" {
		refreshToken = tok.RefreshToken
	}
	a.mintTokens(w, mintInput{
		Subject: rc.Subject, Email: rc.Email, Domain: rc.Domain,
		ClientID: rc.ClientID, Resource: rc.Resource, Project: project,
		Scopes:            rc.Scopes,
		GoogleAccessToken: tok.AccessToken, GoogleExpiry: tok.Expiry.Unix(),
		GoogleRefreshToken: refreshToken, LoginAt: rc.IssuedAt,
	})
}

// mintInput carries everything needed to mint an access/refresh token pair.
type mintInput struct {
	Subject, Email, Domain string
	ClientID, Resource     string
	// Project is the GCP project this grant is bound to: the server pin or
	// the user's validated choice. Empty in domain-only deployments.
	Project            string
	Scopes             []string
	GoogleAccessToken  string
	GoogleExpiry       int64
	GoogleRefreshToken string
	// LoginAt anchors the refresh-token TTL: it must be the ORIGINAL login
	// time (now for the code grant, the previous token's IssuedAt for the
	// refresh grant). Passing "now" on refresh would make refresh tokens
	// infinitely renewable; mintTokens rejects values in the future.
	LoginAt int64
}

// mintTokens seals and writes the token response. The access token expiry is
// capped at the embedded Google token's expiry so the Google token inside a
// live access token is always valid.
func (a *AuthServer) mintTokens(w http.ResponseWriter, in mintInput) {
	now := a.now()
	if in.LoginAt <= 0 || in.LoginAt > now.Unix() {
		a.logger.Error("mint rejected: implausible login time", "email", in.Email, "login_at", in.LoginAt)
		a.tokenError(w, http.StatusInternalServerError, "server_error", "internal error")
		return
	}
	exp := now.Add(accessTokenTTL)
	if gexp := time.Unix(in.GoogleExpiry, 0); gexp.Before(exp) {
		exp = gexp
	}
	if !exp.After(now) {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "upstream token already expired")
		return
	}

	accessToken, err := sealBlob(a.sealer, accessBlob, accessClaims{
		Subject: in.Subject, Email: in.Email, Domain: in.Domain,
		ClientID: in.ClientID, Resource: in.Resource, Project: in.Project,
		Scopes:            in.Scopes,
		GoogleAccessToken: in.GoogleAccessToken, GoogleExpiry: in.GoogleExpiry,
		IssuedAt: now.Unix(), ExpiresAt: exp.Unix(),
	})
	if err != nil {
		a.logger.Error("sealing access token failed", "err", err)
		a.tokenError(w, http.StatusInternalServerError, "server_error", "internal error")
		return
	}

	resp := &tokenResponse{
		AccessToken: accessToken,
		TokenType:   "Bearer",
		ExpiresIn:   int64(exp.Sub(now).Seconds()),
		Scope:       strings.Join(in.Scopes, " "),
	}
	if in.GoogleRefreshToken != "" {
		resp.RefreshToken, err = sealBlob(a.sealer, refreshBlob, refreshClaims{
			Subject: in.Subject, Email: in.Email, Domain: in.Domain,
			ClientID: in.ClientID, Resource: in.Resource, Project: in.Project,
			Scopes:             in.Scopes,
			GoogleRefreshToken: in.GoogleRefreshToken, IssuedAt: in.LoginAt,
		})
		if err != nil {
			a.logger.Error("sealing refresh token failed", "err", err)
			a.tokenError(w, http.StatusInternalServerError, "server_error", "internal error")
			return
		}
	}
	a.writeJSON(w, http.StatusOK, resp)
}

// verifyPKCE checks S256(verifier) == challenge in constant time.
func verifyPKCE(verifier, challenge string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}

// tokenError writes an RFC 6749 §5.2 error response.
func (a *AuthServer) tokenError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Cache-Control", "no-store")
	a.writeJSON(w, status, &oauthErrorResponse{Error: code, ErrorDescription: description})
}
