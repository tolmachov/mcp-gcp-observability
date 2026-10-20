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

// handleRevoke implements RFC 7009. Local family revocation is committed in
// Firestore before the best-effort upstream Google revocation.
//
// Responses: 200 for invalid tokens (RFC 7009 §2.2) and for upstream
// "already revoked"; 503 when upstream revocation transiently fails so the
// client retries. Local access is already cut off before that call.
func (a *AuthServer) handleRevoke(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
	if err := r.ParseForm(); err != nil {
		a.tokenError(w, http.StatusBadRequest, "invalid_request", "malformed_form_body", "malformed form body")
		return
	}
	revoked, err := a.grants().RevokeToken(r.Context(), r.PostFormValue("token"))
	if err != nil {
		a.grantTokenError(r.Context(), w, err)
		return
	}
	if revoked == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	a.logger.Info("grant_revoked", "family_id", revoked.FamilyID, "email", revoked.Email)

	if revoked.GoogleToken != "" {
		if err := a.idp.Revoke(r.Context(), revoked.GoogleToken); err != nil {
			a.logger.Warn("upstream revocation failed, telling client to retry", "email", revoked.Email, "err", err)
			w.Header().Set("Retry-After", "5")
			a.tokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "upstream_revocation_failed", "upstream revocation failed, retry")
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}
