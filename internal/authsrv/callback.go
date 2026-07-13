package authsrv

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// handleCallback receives the user back from Google, validates the identity,
// and hands a sealed authorization code to the client's redirect URI. The
// redirect URI comes out of the sealed state, so it was validated at
// /authorize and is safe to redirect to.
func (a *AuthServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	sc, err := openBlob(a.sealer, stateBlob, q.Get("state"), a.now())
	switch {
	case errors.Is(err, errBlobExpired):
		a.renderErrorPage(w, "Login expired",
			"The login took too long. Start over from your MCP client.")
		return
	case err != nil:
		a.logger.Warn("callback state rejected", "reason", err)
		a.renderErrorPage(w, "Invalid state",
			"The OAuth state is missing or invalid. Start over from your MCP client.")
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
	project := a.cfg.RequireProjectAccess
	if a.cfg.AllowProjectChoice {
		if project = sc.Project; project == "" {
			redirectError(w, r, sc.RedirectURI, sc.ClientState, "invalid_request", "no GCP project was chosen")
			return
		}
	}
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

	code, err := sealBlob(a.sealer, codeBlob, codeClaims{
		Subject:            id.Subject,
		Email:              id.Email,
		Domain:             id.HostedDomain,
		ClientID:           sc.ClientID,
		RedirectURI:        sc.RedirectURI,
		CodeChallenge:      sc.CodeChallenge,
		Resource:           sc.Resource,
		Project:            project,
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

	a.logger.Info("login completed", "email", id.Email, "client", sc.ClientID[:min(24, len(sc.ClientID))])
	redirectWithCode(w, r, sc.RedirectURI, code, sc.ClientState)
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

// redirectWithCode sends the authorization code to the client's redirect URI.
func redirectWithCode(w http.ResponseWriter, r *http.Request, redirectURI, code, state string) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "invalid redirect", http.StatusBadRequest)
		return
	}
	q := u.Query()
	q.Set("code", code)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}
