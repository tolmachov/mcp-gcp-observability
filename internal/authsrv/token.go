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
		a.tokenError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		a.tokenFromCode(w, r, r.PostForm)
	case "refresh_token":
		a.tokenFromRefresh(w, r, r.PostForm)
	default:
		a.tokenError(w, http.StatusBadRequest, "unsupported_grant_type", "supported grant types: authorization_code, refresh_token")
	}
}

func (a *AuthServer) tokenFromCode(w http.ResponseWriter, r *http.Request, form url.Values) {
	now := a.now()
	key := tokenHash(form.Get("code"))
	rec, err := a.store.GetCode(r.Context(), key)
	if err != nil {
		a.storeTokenError(w, "get_code", err)
		return
	}
	if rec.Status != "active" {
		_ = a.store.RedeemCode(r.Context(), key, now, grantRecord{})
		a.logger.Warn("grant_replay", "kind", "authorization_code", "family_id", rec.FamilyID)
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "authorization code was already used")
		return
	}
	if !now.Before(rec.ExpiresAt) {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "authorization code expired")
		return
	}
	cc, err := openBlob(a.sealer, storedCodeBlob, rec.Claims, now)
	if err != nil {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "invalid authorization code")
		return
	}
	if form.Get("client_id") != cc.ClientID || form.Get("redirect_uri") != cc.RedirectURI || !verifyPKCE(form.Get("code_verifier"), cc.CodeChallenge) {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "authorization code binding failed")
		return
	}
	if res := form.Get("resource"); res != "" && strings.TrimRight(res, "/") != a.cfg.IssuerURL {
		a.tokenError(w, http.StatusBadRequest, "invalid_target", "unknown resource")
		return
	}
	refreshToken, secretHash, err := makeRefreshToken(rec.FamilyID)
	if err != nil {
		a.tokenError(w, http.StatusInternalServerError, "server_error", "internal error")
		return
	}
	grantClaims, err := sealBlob(a.sealer, storedGrantBlob, refreshClaims{
		Subject: cc.Subject, Email: cc.Email, Domain: cc.Domain, ClientID: cc.ClientID,
		Resource: cc.Resource, Scopes: cc.Scopes, GoogleRefreshToken: cc.GoogleRefreshToken, IssuedAt: now.Unix(),
	})
	if err != nil {
		a.tokenError(w, http.StatusInternalServerError, "server_error", "internal error")
		return
	}
	grant := grantRecord{Claims: grantClaims, ActiveSecretHash: secretHash, Generation: 1, Status: "active", ExpiresAt: now.Add(a.cfg.refreshTokenTTL()), UpdatedAt: now}
	if err := a.store.RedeemCode(r.Context(), key, now, grant); err != nil {
		if errors.Is(err, errCodeReplay) {
			a.logger.Warn("grant_replay", "kind", "authorization_code", "family_id", rec.FamilyID)
			a.tokenError(w, http.StatusBadRequest, "invalid_grant", "authorization code was already used")
			return
		}
		a.storeTokenError(w, "redeem_code", err)
		return
	}
	a.writeMintedTokens(w, cc.Subject, cc.Email, cc.Domain, cc.ClientID, cc.Resource, rec.FamilyID, cc.Scopes, cc.GoogleAccessToken, cc.GoogleExpiry, refreshToken)
}

func (a *AuthServer) tokenFromRefresh(w http.ResponseWriter, r *http.Request, form url.Values) {
	now := a.now()
	familyID, secret, err := parseRefreshToken(form.Get("refresh_token"))
	if err != nil {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "invalid refresh token")
		return
	}
	rec, err := a.store.GetGrant(r.Context(), familyID)
	if err != nil {
		a.storeTokenError(w, "get_grant", err)
		return
	}
	presentedHash := tokenHash(secret)
	if rec.Status != "active" || !now.Before(rec.ExpiresAt) {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "refresh grant is inactive or expired")
		return
	}
	if !secretMatches(secret, rec.ActiveSecretHash) {
		replayErr := a.store.RotateGrant(r.Context(), familyID, presentedHash, grantRecord{}, now)
		if replayErr != nil && !errors.Is(replayErr, errGrantReplay) && !errors.Is(replayErr, errGrantInactive) {
			a.storeTokenError(w, "revoke_replayed_grant", replayErr)
			return
		}
		a.logger.Warn("grant_replay", "kind", "refresh_token", "family_id", familyID)
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "refresh token replay revoked this grant family")
		return
	}
	rc, err := openBlob(a.sealer, storedGrantBlob, rec.Claims, now)
	if err != nil || rc.GoogleRefreshToken == "" {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "refresh grant is invalid")
		return
	}
	if cid := form.Get("client_id"); cid != "" && cid != rc.ClientID {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "client_id mismatch")
		return
	}
	if len(a.cfg.AllowedDomains) > 0 && !a.cfg.domainAllowed(rc.Domain, rc.Email) {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "account domain is not allowed")
		return
	}
	tok, err := a.idp.Refresh(r.Context(), rc.GoogleRefreshToken)
	if err != nil {
		if _, ok := errors.AsType[*oauth2.RetrieveError](err); ok {
			_ = a.store.RevokeGrant(r.Context(), familyID, now)
			a.tokenError(w, http.StatusBadRequest, "invalid_grant", "upstream grant revoked, log in again")
			return
		}
		a.tokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "upstream token refresh failed")
		return
	}
	if a.cfg.PinnedProject != "" {
		allowed, checkErr := a.checkProjectAccess(r.Context(), tok.AccessToken, a.cfg.PinnedProject)
		if checkErr != nil {
			a.tokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "access check failed")
			return
		}
		if !allowed {
			_ = a.store.RevokeGrant(r.Context(), familyID, now)
			a.tokenError(w, http.StatusBadRequest, "invalid_grant", "account no longer has access to the pinned project")
			return
		}
	}
	googleRefresh := rc.GoogleRefreshToken
	if tok.RefreshToken != "" {
		googleRefresh = tok.RefreshToken
	}
	nextToken, nextHash, err := makeRefreshToken(familyID)
	if err != nil {
		a.tokenError(w, http.StatusInternalServerError, "server_error", "internal error")
		return
	}
	nextClaims, err := sealBlob(a.sealer, storedGrantBlob, refreshClaims{Subject: rc.Subject, Email: rc.Email, Domain: rc.Domain, ClientID: rc.ClientID, Resource: rc.Resource, Scopes: rc.Scopes, GoogleRefreshToken: googleRefresh, IssuedAt: rc.IssuedAt})
	if err != nil {
		a.tokenError(w, http.StatusInternalServerError, "server_error", "internal error")
		return
	}
	next := grantRecord{Claims: nextClaims, ActiveSecretHash: nextHash, Generation: rec.Generation + 1, Status: "active", ExpiresAt: rec.ExpiresAt, UpdatedAt: now}
	if err := a.store.RotateGrant(r.Context(), familyID, presentedHash, next, now); err != nil {
		if errors.Is(err, errGrantReplay) || errors.Is(err, errGrantInactive) {
			a.logger.Warn("grant_replay", "kind", "refresh_token", "family_id", familyID)
			a.tokenError(w, http.StatusBadRequest, "invalid_grant", "refresh token replay revoked this grant family")
			return
		}
		a.storeTokenError(w, "rotate_grant", err)
		return
	}
	a.writeMintedTokens(w, rc.Subject, rc.Email, rc.Domain, rc.ClientID, rc.Resource, familyID, rc.Scopes, tok.AccessToken, tok.Expiry.Unix(), nextToken)
}

func (a *AuthServer) writeMintedTokens(w http.ResponseWriter, subject, email, domain, clientID, resource, familyID string, scopes []string, googleAccess string, googleExpiry int64, refresh string) {
	now := a.now()
	exp := now.Add(accessTokenTTL)
	if upstream := time.Unix(googleExpiry, 0); upstream.Before(exp) {
		exp = upstream
	}
	if !exp.After(now) {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "upstream token already expired")
		return
	}
	accessToken, err := sealBlob(a.sealer, accessBlob, accessClaims{Subject: subject, Email: email, Domain: domain, ClientID: clientID, Resource: resource, FamilyID: familyID, Scopes: scopes, GoogleAccessToken: googleAccess, GoogleExpiry: googleExpiry, IssuedAt: now.Unix(), ExpiresAt: exp.Unix()})
	if err != nil {
		a.tokenError(w, http.StatusInternalServerError, "server_error", "internal error")
		return
	}
	a.writeJSON(w, http.StatusOK, &tokenResponse{AccessToken: accessToken, TokenType: "Bearer", ExpiresIn: int64(exp.Sub(now).Seconds()), RefreshToken: refresh, Scope: strings.Join(scopes, " ")})
}

func (a *AuthServer) storeTokenError(w http.ResponseWriter, operation string, err error) {
	if errors.Is(err, errStateNotFound) {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired OAuth grant")
		return
	}
	a.logger.Error("oauth_store_failure", "operation", operation, "err", err)
	w.Header().Set("Retry-After", "5")
	a.tokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "OAuth state store unavailable")
}

func verifyPKCE(verifier, challenge string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}

func (a *AuthServer) tokenError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Cache-Control", "no-store")
	a.writeJSON(w, status, &oauthErrorResponse{Error: code, ErrorDescription: description})
}
