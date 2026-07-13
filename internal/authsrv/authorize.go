package authsrv

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"golang.org/x/oauth2"
)

// projectIDRe is the GCP project ID format: 6-30 chars, lowercase letters,
// digits and hyphens, starting with a letter, not ending with a hyphen.
var projectIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)

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

	blob, err := sealBlob(a.sealer, stateBlob, stateClaims{
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

	if a.cfg.SkipConsent {
		a.redirectToGoogle(w, r, blob)
		return
	}
	a.renderConsent(w, client, redirectURI, blob)
}

// handleAuthorizeConfirm receives the consent form. The sealed request blob
// doubles as the CSRF token: it is unguessable, TTL-bound, and usable only
// with this deployment's keys.
func (a *AuthServer) handleAuthorizeConfirm(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
	if err := r.ParseForm(); err != nil {
		a.renderErrorPage(w, "Malformed request",
			"The request body could not be parsed. Start over from your MCP client.")
		return
	}
	sc, err := openBlob(a.sealer, stateBlob, r.PostForm.Get("request"), a.now())
	switch {
	case errors.Is(err, errBlobExpired):
		a.renderErrorPage(w, "Request expired",
			"The authorization request expired. Start over from your MCP client.")
		return
	case err != nil:
		a.renderErrorPage(w, "Invalid request",
			"The authorization request is invalid or has been tampered with. Start over from your MCP client.")
		return
	}
	blob := r.PostForm.Get("request")
	if a.cfg.AllowProjectChoice {
		// The chosen project rides inside the sealed state to /callback,
		// where access to it is verified with the user's own Google token.
		sc.Project = strings.TrimSpace(r.PostForm.Get("project"))
		if !projectIDRe.MatchString(sc.Project) {
			a.renderErrorPage(w, "Invalid project ID",
				"Enter the GCP project ID (lowercase letters, digits and hyphens, e.g. my-project-123), not the project name or number.")
			return
		}
		blob, err = sealBlob(a.sealer, stateBlob, sc)
		if err != nil {
			a.logger.Error("resealing authorize state failed", "err", err)
			a.renderErrorPage(w, "Internal error", "Try again from your MCP client.")
			return
		}
	}
	a.redirectToGoogle(w, r, blob)
}

// redirectToGoogle sends the user to the Google login page, carrying the
// sealed request blob as the OAuth state.
func (a *AuthServer) redirectToGoogle(w http.ResponseWriter, r *http.Request, stateBlob string) {
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
	http.Redirect(w, r, a.idp.AuthCodeURL(stateBlob, opts...), http.StatusFound)
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
	http.Redirect(w, r, u.String(), http.StatusFound)
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
label{display:block;margin-top:16px;font-size:14px;font-weight:600}
input[type=text]{width:100%;box-sizing:border-box;margin-top:6px;padding:10px 12px;font-size:15px;border:1px solid #c4c7cc;border-radius:8px}
</style></head><body><main>
<h1>{{.ClientName}} wants to access Google Cloud data as you</h1>
<p>Signing in grants this MCP client access to Google Cloud on your behalf, <strong>bounded by your own IAM permissions</strong>. This server only reads observability data (Logging, Monitoring, Trace, Error Reporting, Profiler).</p>
<p class="muted">After approval you will be redirected to:<br><code>{{.RedirectURI}}</code></p>
<form method="post" action="/authorize/confirm">
<input type="hidden" name="request" value="{{.Request}}">
{{if .AskProject}}<label for="project">GCP project ID to work with</label>
<input type="text" id="project" name="project" placeholder="my-project-123" required
 pattern="[a-z][a-z0-9-]{4,28}[a-z0-9]" autocomplete="off" spellcheck="false"
 title="Lowercase letters, digits and hyphens (the project ID, not its name or number)">
<p class="muted">Your Google account must have access to this project.</p>
{{end}}<button type="submit">Continue with Google</button>
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
		"AskProject":  a.cfg.AllowProjectChoice,
	})
	if err != nil {
		a.logger.Error("rendering consent page failed", "err", err)
	}
}

// renderErrorPage shows a terminal 400 error page (used when redirecting
// back to the client would be unsafe).
func (a *AuthServer) renderErrorPage(w http.ResponseWriter, title, detail string) {
	setInterstitialHeaders(w)
	w.WriteHeader(http.StatusBadRequest)
	_, _ = fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>%s</title></head>
<body style="font-family:system-ui,sans-serif;padding:15vh 20px;text-align:center">
<h1 style="font-size:20px">%s</h1><p style="color:#5f6368">%s</p></body></html>`,
		template.HTMLEscapeString(title), template.HTMLEscapeString(title), template.HTMLEscapeString(detail))
}
