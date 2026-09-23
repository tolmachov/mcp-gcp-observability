package authsrv

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// handleCallback receives the user back from Google, validates the identity,
// and hands an opaque single-use authorization code to the client's redirect URI. The
// redirect URI comes out of encrypted server-side state, so it was validated at
// /authorize and is safe to redirect to.
func (a *AuthServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	rec, err := a.loadAuthorizationState(r.Context(), q.Get("state"), true)
	switch {
	case errors.Is(err, errBlobExpired):
		a.renderErrorPage(w, "Login expired",
			"The login took too long. Start over from your MCP client.")
		return
	case err != nil && !errors.Is(err, errStateNotFound) && !errors.Is(err, errStateReplay):
		a.logger.Error("oauth_store_failure", "operation", "use_authorization_state", "err", err)
		a.renderErrorPageStatus(w, http.StatusServiceUnavailable, "Service unavailable",
			"The authorization state store is unavailable. Try again later.")
		return
	case err != nil:
		a.logger.Warn("callback state rejected", "reason", err)
		a.renderErrorPage(w, "Invalid state",
			"The OAuth state is missing or invalid. Start over from your MCP client.")
		return
	}
	sc, err := openBlob(a.sealer, stateBlob, rec.Claims, a.now())
	if err != nil {
		a.logger.Error("stored OAuth state could not be decrypted", "err", err)
		a.renderErrorPage(w, "Invalid state", "The OAuth state is invalid. Start over from your MCP client.")
		return
	}

	if errCode := q.Get("error"); errCode != "" {
		// The user declined or Google refused; pass it through.
		redirectError(w, r, sc.RedirectURI, sc.ClientState, errCode, q.Get("error_description"))
		return
	}
	googleCode := q.Get("code")
	if googleCode == "" {
		redirectError(w, r, sc.RedirectURI, sc.ClientState, "invalid_request", "missing authorization code")
		return
	}

	tok, err := a.idp.Exchange(r.Context(), googleCode)
	if err != nil {
		a.logger.Error("google code exchange failed", "err", err)
		redirectError(w, r, sc.RedirectURI, sc.ClientState, "server_error", "upstream token exchange failed")
		return
	}
	if tok.RefreshToken == "" {
		redirectError(w, r, sc.RedirectURI, sc.ClientState, "access_denied",
			"Google did not issue an offline refresh token; revoke the app grant and authorize again")
		return
	}
	rawIDToken, _ := tok.Extra("id_token").(string)
	if rawIDToken == "" {
		a.logger.Error("google token response missing id_token")
		redirectError(w, r, sc.RedirectURI, sc.ClientState, "server_error", "upstream response missing id_token")
		return
	}
	id, err := a.idp.ValidateIDToken(r.Context(), rawIDToken)
	if err != nil {
		a.logger.Error("id_token validation failed", "err", err)
		redirectError(w, r, sc.RedirectURI, sc.ClientState, "access_denied", "identity verification failed")
		return
	}
	if !id.EmailVerified || id.Email == "" || id.Subject == "" {
		redirectError(w, r, sc.RedirectURI, sc.ClientState, "access_denied", "account email is not verified")
		return
	}
	if len(a.cfg.AllowedDomains) > 0 && !a.cfg.domainAllowed(id.HostedDomain, id.Email) {
		a.logger.Warn("login from disallowed domain rejected", "email", id.Email, "hd", id.HostedDomain)
		redirectError(w, r, sc.RedirectURI, sc.ClientState, "access_denied", "account domain is not allowed")
		return
	}
	granted := grantedScopes(tok.Extra("scope"))
	if missing := firstMissing(a.cfg.requiredGrantedScopes(), granted); missing != "" {
		// Google's granular consent lets the user untick the Cloud scope;
		// without it every GCP call (and the project-access probe) would
		// fail with a misleading "no access" — name the real cause.
		a.logger.Warn("login rejected: required scope not granted", "email", id.Email, "scope", missing)
		redirectError(w, r, sc.RedirectURI, sc.ClientState, "access_denied",
			"the Google Cloud permission was not granted — log in again and keep all requested permissions ticked")
		return
	}
	project := a.cfg.PinnedProject
	hasAccess, err := a.checkProjectAccess(r.Context(), tok.AccessToken, project)
	if err != nil {
		a.logger.Error("project access check failed", "email", id.Email, "err", err)
		redirectError(w, r, sc.RedirectURI, sc.ClientState, "server_error", "access check failed, try again")
		return
	}
	if !hasAccess {
		a.logger.Warn("login rejected: no access to project", "email", id.Email, "project", project)
		redirectError(w, r, sc.RedirectURI, sc.ClientState, "access_denied", "account has no access to GCP project "+project)
		return
	}

	claims, err := sealBlob(a.sealer, storedCodeBlob, codeClaims{
		Subject:            id.Subject,
		Email:              id.Email,
		Domain:             id.HostedDomain,
		ClientID:           sc.ClientID,
		RedirectURI:        sc.RedirectURI,
		CodeChallenge:      sc.CodeChallenge,
		Resource:           sc.Resource,
		Scopes:             granted,
		GoogleAccessToken:  tok.AccessToken,
		GoogleExpiry:       tok.Expiry.Unix(),
		GoogleRefreshToken: tok.RefreshToken,
		IssuedAt:           a.now().Unix(),
	})
	if err != nil {
		a.logger.Error("sealing authorization code failed", "err", err)
		redirectError(w, r, sc.RedirectURI, sc.ClientState, "server_error", "internal error")
		return
	}
	familyID, err := randomOpaque(18)
	if err != nil {
		redirectError(w, r, sc.RedirectURI, sc.ClientState, "server_error", "internal error")
		return
	}
	code, key, err := makeAuthorizationCode()
	if err != nil {
		redirectError(w, r, sc.RedirectURI, sc.ClientState, "server_error", "internal error")
		return
	}
	if err := a.store.PutCode(r.Context(), key, codeRecord{Claims: claims, Status: "active", FamilyID: familyID, ExpiresAt: a.now().Add(codeTTL)}); err != nil {
		a.logger.Error("oauth_store_failure", "operation", "put_code", "err", err)
		redirectError(w, r, sc.RedirectURI, sc.ClientState, "server_error", "authorization state store unavailable")
		return
	}

	a.logger.Info("login completed", "email", id.Email, "client", sc.ClientID[:min(24, len(sc.ClientID))])
	redirectWithParams(w, r, sc.RedirectURI, sc.ClientState, url.Values{"code": {code}})
}

// firstMissing returns the first element of want absent from got, or "".
func firstMissing(want, got []string) string {
	for _, w := range want {
		if !slices.Contains(got, w) {
			return w
		}
	}
	return ""
}

// grantedScopes parses the space-separated scope string from Google's token
// response, falling back to nil when absent.
func grantedScopes(v any) []string {
	s, _ := v.(string)
	if s == "" {
		return nil
	}
	return strings.Fields(s)
}
