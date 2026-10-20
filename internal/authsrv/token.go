package authsrv

import (
	"context"
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

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

type oauthErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

const maxFormBody = 64 << 10

func (a *AuthServer) handleToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
	if err := r.ParseForm(); err != nil {
		a.tokenError(w, http.StatusBadRequest, "invalid_request", "malformed_form_body", "malformed form body")
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		a.tokenFromCode(w, r, r.PostForm)
	case "refresh_token":
		a.tokenFromRefresh(w, r, r.PostForm)
	default:
		a.tokenError(w, http.StatusBadRequest, "unsupported_grant_type", "unsupported_grant_type", "supported grant types: authorization_code, refresh_token")
	}
}

func (a *AuthServer) tokenFromCode(w http.ResponseWriter, r *http.Request, form url.Values) {
	tokens, err := a.grants().ExchangeCode(r.Context(), codeExchange{
		Code: form.Get("code"), ClientID: form.Get("client_id"), RedirectURI: form.Get("redirect_uri"),
		CodeVerifier: form.Get("code_verifier"), Resource: form.Get("resource"),
	})
	if err != nil {
		a.grantTokenError(r.Context(), w, err)
		return
	}
	a.writeMintedTokens(w, tokens)
}

func (a *AuthServer) tokenFromRefresh(w http.ResponseWriter, r *http.Request, form url.Values) {
	grants := a.grants()
	prepared, err := grants.PrepareRefresh(r.Context(), form.Get("refresh_token"), form.Get("client_id"))
	if err != nil {
		a.grantTokenError(r.Context(), w, err)
		return
	}
	tok, err := a.idp.Refresh(r.Context(), prepared.GoogleRefreshToken())
	if err != nil {
		if _, ok := errors.AsType[*oauth2.RetrieveError](err); ok {
			_ = grants.RevokeFamily(r.Context(), prepared)
			a.tokenError(w, http.StatusBadRequest, "invalid_grant", "upstream_grant_revoked", "upstream grant revoked, log in again")
			return
		}
		a.tokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "upstream_refresh_failed", "upstream token refresh failed")
		return
	}
	if a.cfg.PinnedProject != "" {
		allowed, checkErr := a.checkProjectAccess(r.Context(), tok.AccessToken, a.cfg.PinnedProject)
		if checkErr != nil {
			a.tokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "project_access_check_failed", "access check failed")
			return
		}
		if !allowed {
			_ = grants.RevokeFamily(r.Context(), prepared)
			a.tokenError(w, http.StatusBadRequest, "invalid_grant", "project_access_lost", "account no longer has access to the pinned project")
			return
		}
	}
	tokens, err := grants.RotateRefresh(r.Context(), prepared, tok.RefreshToken)
	if err != nil {
		a.grantTokenError(r.Context(), w, err)
		return
	}
	tokens.GoogleAccessToken, tokens.GoogleExpiry = tok.AccessToken, tok.Expiry.Unix()
	a.writeMintedTokens(w, tokens)
}

// grantTokenError keeps OAuth wire formatting and store diagnostics at the
// HTTP boundary; lifecycle operations return only classified domain failures.
func (a *AuthServer) grantTokenError(ctx context.Context, w http.ResponseWriter, err error) {
	if failure, ok := errors.AsType[*grantStoreError](err); ok {
		a.storeTokenError(ctx, w, failure.operation, failure.cause)
		return
	}
	if rejected, ok := errors.AsType[*grantRejection](err); ok {
		a.tokenError(w, http.StatusBadRequest, rejected.code, rejected.reason, rejected.description)
		return
	}
	a.tokenError(w, http.StatusInternalServerError, "server_error", "unexpected_grant_failure", "unexpected grant failure")
}

func (a *AuthServer) writeMintedTokens(w http.ResponseWriter, tokens grantTokens) {
	now := a.now()
	exp := now.Add(accessTokenTTL)
	if upstream := time.Unix(tokens.GoogleExpiry, 0); upstream.Before(exp) {
		exp = upstream
	}
	if !exp.After(now) {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "upstream_token_expired", "upstream token already expired")
		return
	}
	accessToken := sealBlob(a.sealer, accessBlob, accessClaims{Subject: tokens.Subject, Email: tokens.Email, Domain: tokens.Domain, ClientID: tokens.ClientID, Resource: tokens.Resource, FamilyID: tokens.FamilyID, Scopes: tokens.Scopes, GoogleAccessToken: tokens.GoogleAccessToken, GoogleExpiry: tokens.GoogleExpiry, IssuedAt: now.Unix(), ExpiresAt: exp.Unix()})
	a.writeJSON(w, http.StatusOK, &tokenResponse{AccessToken: accessToken, TokenType: "Bearer", ExpiresIn: int64(exp.Sub(now).Seconds()), RefreshToken: tokens.RefreshToken, Scope: strings.Join(tokens.Scopes, " ")})
}

func (a *AuthServer) storeTokenError(ctx context.Context, w http.ResponseWriter, operation string, err error) {
	if errors.Is(err, errStateNotFound) {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "oauth_state_not_found", "invalid or expired OAuth grant")
		return
	}
	a.logStoreFailure(ctx, operation, err)
	w.Header().Set("Retry-After", "5")
	a.tokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "oauth_store_unavailable", "OAuth state store unavailable")
}

func verifyPKCE(verifier, challenge string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}

// tokenError writes an RFC 6749 §5.2 error response. reason is the static
// class the HTTP rejection diagnostic records; description is the
// client-facing error_description. Both are application-owned constants,
// never submitted credentials or upstream error bodies.
func (a *AuthServer) tokenError(w http.ResponseWriter, status int, code, reason, description string) {
	w.Header().Set("Cache-Control", "no-store")
	a.writeOAuthError(w, status, code, reason, &oauthErrorResponse{Error: code, ErrorDescription: description})
}
