package authsrv

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"golang.org/x/oauth2"
)

// handleAuthorize validates the client's authorization request and renders
// the consent interstitial. Failures in client_id / redirect_uri validation
// render an error page and never redirect (open-redirect protection); once
// the redirect target is trusted, protocol errors are returned to it per
// RFC 6749 §4.1.2.1.
func (a *AuthServer) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	client, err := a.parseClientID(q.Get("client_id"))
	if err != nil {
		a.renderErrorPage(w, "Unknown client",
			"The client_id is missing or invalid. Re-register the client and try again.")
		return
	}
	redirectURI := q.Get("redirect_uri")
	if redirectURI == "" || !matchRegistered(client.RedirectURIs, redirectURI) || !a.policy.allowed(redirectURI) {
		a.renderErrorPage(w, "Invalid redirect URI",
			"The redirect_uri does not match the client's registration.")
		return
	}

	state := q.Get("state")
	if q.Get("response_type") != "code" {
		redirectError(w, r, redirectURI, state, "unsupported_response_type", "only response_type=code is supported")
		return
	}
	challenge := q.Get("code_challenge")
	method := q.Get("code_challenge_method")
	if challenge == "" || method != "S256" {
		redirectError(w, r, redirectURI, state, "invalid_request", "PKCE with code_challenge_method=S256 is required")
		return
	}
	if res := q.Get("resource"); res != "" && strings.TrimRight(res, "/") != a.cfg.IssuerURL {
		redirectError(w, r, redirectURI, state, "invalid_target", "unknown resource")
		return
	}

	claims, err := sealBlob(a.sealer, stateBlob, stateClaims{
		ClientID:      q.Get("client_id"),
		RedirectURI:   redirectURI,
		ClientState:   state,
		CodeChallenge: challenge,
		Resource:      q.Get("resource"),
		IssuedAt:      a.now().Unix(),
	})
	if err != nil {
		a.logger.Error("sealing authorize state failed", "err", err)
		redirectError(w, r, redirectURI, state, "server_error", "internal error")
		return
	}
	stateSecret, err := randomOpaque(32)
	if err != nil {
		redirectError(w, r, redirectURI, state, "server_error", "internal error")
		return
	}
	stateToken := prefixState + stateSecret
	if err := a.store.PutAuthorizationState(r.Context(), tokenHash(stateToken), authorizationStateRecord{
		Claims: claims, Status: "active", ExpiresAt: a.now().Add(stateTTL),
	}); err != nil {
		a.logger.Error("oauth_store_failure", "operation", "put_authorization_state", "err", err)
		redirectError(w, r, redirectURI, state, "server_error", "authorization state store unavailable")
		return
	}

	if a.cfg.SkipConsent {
		a.redirectToGoogle(w, r, stateToken)
		return
	}
	a.renderConsent(w, client, redirectURI, stateToken)
}

// handleAuthorizeConfirm receives the consent form. The opaque request token
// names encrypted, TTL-bound server-side state and doubles as the CSRF token.
func (a *AuthServer) handleAuthorizeConfirm(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
	if err := r.ParseForm(); err != nil {
		a.renderErrorPage(w, "Malformed request",
			"The request body could not be parsed. Start over from your MCP client.")
		return
	}
	stateToken := r.PostForm.Get("request")
	rec, err := a.loadAuthorizationState(r.Context(), stateToken, false)
	switch {
	case errors.Is(err, errBlobExpired):
		a.renderErrorPage(w, "Request expired",
			"The authorization request expired. Start over from your MCP client.")
		return
	case err != nil && !errors.Is(err, errStateNotFound) && !errors.Is(err, errStateReplay):
		a.logger.Error("oauth_store_failure", "operation", "get_authorization_state", "err", err)
		a.renderErrorPageStatus(w, http.StatusServiceUnavailable, "Service unavailable",
			"The authorization state store is unavailable. Try again later.")
		return
	case err != nil:
		a.renderErrorPage(w, "Invalid request",
			"The authorization request is invalid or has been tampered with. Start over from your MCP client.")
		return
	}
	if rec.Status != "active" {
		a.renderErrorPage(w, "Invalid request", "The authorization request has already been used. Start over from your MCP client.")
		return
	}
	a.redirectToGoogle(w, r, stateToken)
}

// redirectToGoogle sends the user to the Google login page, carrying the
// opaque Firestore-backed request token as the OAuth state.
func (a *AuthServer) redirectToGoogle(w http.ResponseWriter, r *http.Request, stateToken string) {
	opts := []oauth2.AuthCodeOption{
		// offline + consent guarantee a refresh token on every login.
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
	}
	if len(a.cfg.AllowedDomains) == 1 && !isConsumerDomain(a.cfg.AllowedDomains[0]) {
		// UX hint only; the hd claim is verified server-side at /callback.
		// Skipped for consumer domains: hd selects Workspace accounts, and
		// gmail.com would filter out the very accounts being allowed.
		opts = append(opts, oauth2.SetAuthURLParam("hd", a.cfg.AllowedDomains[0]))
	}
	// The target is Google's authorize endpoint from server configuration;
	// Request input never reaches Google; only the unguessable lookup token does.
	http.Redirect(w, r, a.idp.AuthCodeURL(stateToken, opts...), http.StatusFound) //nolint:gosec // G710: fixed upstream host
}

func (a *AuthServer) loadAuthorizationState(ctx context.Context, raw string, consume bool) (authorizationStateRecord, error) {
	if !strings.HasPrefix(raw, prefixState) {
		return authorizationStateRecord{}, errStateNotFound
	}
	if consume {
		return a.store.UseAuthorizationState(ctx, tokenHash(raw), a.now())
	}
	rec, err := a.store.GetAuthorizationState(ctx, tokenHash(raw))
	if err != nil {
		return authorizationStateRecord{}, err
	}
	if !a.now().Before(rec.ExpiresAt) {
		return authorizationStateRecord{}, errBlobExpired
	}
	return rec, nil
}

// redirectError returns a protocol error to an already-validated redirect URI.
func redirectError(w http.ResponseWriter, r *http.Request, redirectURI, state, code, description string) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "invalid redirect", http.StatusBadRequest)
		return
	}
	q := u.Query()
	q.Set("error", code)
	q.Set("error_description", description)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	// Callers pass only redirect URIs already validated against the
	// client's registration and the redirect policy (see handleAuthorize)
	// or recovered from encrypted server-side state (see handleCallback).
	http.Redirect(w, r, u.String(), http.StatusFound) //nolint:gosec // G710: pre-validated redirect target
}

var consentTemplate = sync.OnceValue(func() *template.Template {
	return template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Authorize {{.ClientName}}</title>
<style>
body{font-family:system-ui,sans-serif;display:flex;justify-content:center;padding-top:10vh;background:#f6f7f9;color:#1a1a2e;margin:0}
main{background:#fff;border:1px solid #e2e4e8;border-radius:12px;padding:32px;max-width:460px;box-shadow:0 4px 16px rgba(0,0,0,.06)}
h1{font-size:20px;margin:0 0 12px}
p{line-height:1.5;margin:8px 0}
code{background:#f0f1f4;border-radius:4px;padding:2px 6px;font-size:13px;word-break:break-all}
button{background:#1a73e8;color:#fff;border:0;border-radius:8px;padding:12px 24px;font-size:15px;cursor:pointer;margin-top:16px;width:100%}
button:hover{background:#1765cc}
.muted{color:#5f6368;font-size:13px}
</style></head><body><main>
<h1>{{.ClientName}} wants to access Google Cloud data as you</h1>
<p>Signing in grants this MCP client access to Google Cloud on your behalf, <strong>bounded by your own IAM permissions</strong>. This server only reads observability data (Logging, Monitoring, Trace, Error Reporting, Profiler).</p>
<p class="muted">After approval you will be redirected to:<br><code>{{.RedirectURI}}</code></p>
<form method="post" action="` + AuthorizeConfirmPath + `">
<input type="hidden" name="request" value="{{.Request}}">
<button type="submit">Continue with Google</button>
</form>
</main></body></html>`))
})

// setInterstitialHeaders hardens the consent/error pages: never cached,
// never frameable (the consent page is the confused-deputy defense — a
// click on it must not be hijackable via an embedding frame).
func setInterstitialHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "frame-ancestors 'none'")
}

// renderConsent shows the confirmation interstitial before the Google login.
func (a *AuthServer) renderConsent(w http.ResponseWriter, client *clientIDClaims, redirectURI, blob string) {
	setInterstitialHeaders(w)
	err := consentTemplate().Execute(w, map[string]any{
		"ClientName":  clientDisplayName(client),
		"RedirectURI": redirectURI,
		"Request":     blob,
	})
	if err != nil {
		a.logger.Error("rendering consent page failed", "err", err)
	}
}

// renderErrorPage shows a terminal 400 error page (used when redirecting
// back to the client would be unsafe).
func (a *AuthServer) renderErrorPage(w http.ResponseWriter, title, detail string) {
	a.renderErrorPageStatus(w, http.StatusBadRequest, title, detail)
}

func (a *AuthServer) renderErrorPageStatus(w http.ResponseWriter, status int, title, detail string) {
	setInterstitialHeaders(w)
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>%s</title></head>
<body style="font-family:system-ui,sans-serif;padding:15vh 20px;text-align:center">
<h1 style="font-size:20px">%s</h1><p style="color:#5f6368">%s</p></body></html>`,
		template.HTMLEscapeString(title), template.HTMLEscapeString(title), template.HTMLEscapeString(detail))
}
