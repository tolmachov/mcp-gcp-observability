package authsrv

import (
	"errors"
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
		a.tokenError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	token := r.PostFormValue("token")
	now := a.now()

	var familyID, googleToken, email string
	if ac, err := openBlob(a.sealer, accessBlob, token, now); err == nil {
		familyID, googleToken, email = ac.FamilyID, ac.GoogleAccessToken, ac.Email
	} else if id, secret, err := parseRefreshToken(token); err == nil {
		familyID = id
		if rec, getErr := a.store.GetGrant(r.Context(), id); getErr == nil {
			// A family identifier is not proof of possession. Keep the hash
			// after revocation so a valid token can retry upstream failures.
			if !secretMatches(secret, rec.ActiveSecretHash) {
				w.WriteHeader(http.StatusOK)
				return
			}
			if rc, openErr := openBlob(a.sealer, storedGrantBlob, rec.Claims, now); openErr == nil {
				googleToken, email = rc.GoogleRefreshToken, rc.Email
			}
		} else if errors.Is(getErr, errStateNotFound) {
			w.WriteHeader(http.StatusOK)
			return
		} else {
			a.storeTokenError(w, "revoke_get_grant", getErr)
			return
		}
	} else {
		// RFC 7009 §2.2: invalid tokens still yield 200.
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := a.store.RevokeGrant(r.Context(), familyID, now); err != nil {
		a.storeTokenError(w, "revoke_grant", err)
		return
	}
	a.logger.Info("grant_revoked", "family_id", familyID, "email", email)

	if googleToken != "" {
		if err := a.idp.Revoke(r.Context(), googleToken); err != nil {
			a.logger.Warn("upstream revocation failed, telling client to retry", "email", email, "err", err)
			w.Header().Set("Retry-After", "5")
			a.tokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "upstream revocation failed, retry")
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}
