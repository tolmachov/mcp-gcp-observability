package authsrv

import (
	"net/http"
	"net/url"
	"strings"
)

// newFormBody builds an application/x-www-form-urlencoded body.
func newFormBody(key, value string) *strings.Reader {
	return strings.NewReader(url.Values{key: {value}}.Encode())
}

// handleRevoke implements RFC 7009. Sealed tokens cannot be individually
// invalidated (stateless design), so revocation acts on the embedded Google
// token, which kills the whole upstream grant: every access and refresh
// token derived from that login stops working at the GCP API layer
// immediately, and refreshes start failing with invalid_grant.
//
// Responses: 200 for invalid tokens (RFC 7009 §2.2) and for upstream
// "already revoked"; 503 when the upstream call transiently fails (§2.2.1) —
// the grant is still alive then, and telling the client otherwise would be a
// security-relevant lie ("disconnect" after a stolen laptop must be
// retryable, not silently dropped).
func (a *AuthServer) handleRevoke(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
	if err := r.ParseForm(); err != nil {
		a.tokenError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	token := r.PostFormValue("token")
	now := a.now()

	var googleToken, email string
	if ac, err := openBlob(a.sealer, accessBlob, token, now); err == nil {
		googleToken, email = ac.GoogleAccessToken, ac.Email
	} else if rc, err := openBlob(a.sealer, refreshBlob, token, now); err == nil {
		googleToken, email = rc.GoogleRefreshToken, rc.Email
	} else {
		// RFC 7009 §2.2: invalid tokens still yield 200.
		w.WriteHeader(http.StatusOK)
		return
	}

	if err := a.idp.Revoke(r.Context(), googleToken); err != nil {
		a.logger.Warn("upstream revocation failed, telling client to retry", "email", email, "err", err)
		w.Header().Set("Retry-After", "5")
		a.tokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "upstream revocation failed, retry")
		return
	}
	a.logger.Info("grant revoked", "email", email)
	w.WriteHeader(http.StatusOK)
}
