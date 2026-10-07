package authsrv

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/tolmachov/mcp-gcp-observability/internal/httpdiag"
)

// maxRegistrationBody bounds the /register request body.
const maxRegistrationBody = 64 << 10

// handleRegister implements Dynamic Client Registration (RFC 7591). Nothing
// is stored: the issued client_id is an HMAC-signed blob embedding the
// registered redirect URIs, which /authorize later verifies and matches
// against. All clients are public (PKCE, no secret).
func (a *AuthServer) handleRegister(w http.ResponseWriter, r *http.Request) {
	var meta oauthex.ClientRegistrationMetadata
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRegistrationBody)).Decode(&meta); err != nil {
		a.registrationError(w, "invalid_client_metadata", "invalid_client_metadata_json", "request body is not valid client metadata JSON", err.Error())
		return
	}
	if len(meta.RedirectURIs) == 0 {
		a.registrationError(w, "invalid_redirect_uri", "missing_redirect_uri", "at least one redirect_uri is required", "")
		return
	}
	for _, u := range meta.RedirectURIs {
		if !a.policy.allowed(u) {
			a.registrationError(w, "invalid_redirect_uri", "redirect_uri_not_allowed",
				"redirect_uri is not allowed: use a loopback http URI or ask the server operator to allowlist it", u)
			return
		}
	}
	for _, gt := range meta.GrantTypes {
		if gt != "authorization_code" && gt != "refresh_token" {
			a.registrationError(w, "invalid_client_metadata", "unsupported_grant_type", "unsupported grant_type", gt)
			return
		}
	}
	for _, rt := range meta.ResponseTypes {
		if rt != "code" {
			a.registrationError(w, "invalid_client_metadata", "unsupported_response_type", "unsupported response_type", rt)
			return
		}
	}

	now := a.now()
	clientID := a.sealer.signClientID(clientIDClaims{
		RedirectURIs: meta.RedirectURIs,
		ClientName:   meta.ClientName,
		IssuedAt:     now.Unix(),
	})

	// Echo the metadata back with the values this server enforces.
	meta.TokenEndpointAuthMethod = "none"
	meta.GrantTypes = []string{"authorization_code", "refresh_token"}
	meta.ResponseTypes = []string{"code"}
	resp := &oauthex.ClientRegistrationResponse{
		ClientRegistrationMetadata: meta,
		ClientID:                   clientID,
		ClientIDIssuedAt:           now,
	}
	a.writeJSON(w, http.StatusCreated, resp)
}

// parseClientID verifies a client_id blob and returns its embedded claims.
func (a *AuthServer) parseClientID(clientID string) (*clientIDClaims, error) {
	var claims clientIDClaims
	if err := a.sealer.verifyClientID(clientID, &claims); err != nil {
		return nil, err
	}
	return &claims, nil
}

// registrationError writes an RFC 7591 §3.2.2 error response. reason is the
// static class the HTTP rejection diagnostic records and description the
// client-facing error_description, both application-owned constants; value
// is the offending client metadata (public, never a credential), logged
// separately so an operator can see what a client actually sent.
func (a *AuthServer) registrationError(w http.ResponseWriter, code, reason, description, value string) {
	if value != "" {
		a.logger.Warn("client registration rejected", "oauth_error", code, "reason", reason, "value", value)
		description = fmt.Sprintf("%s: %q", description, value)
	}
	a.writeOAuthError(w, http.StatusBadRequest, code, reason, &oauthex.ClientRegistrationError{
		ErrorCode:        code,
		ErrorDescription: description,
	})
}

// writeOAuthError writes an OAuth error body with an error status, recording
// its code and static reason class for the HTTP rejection diagnostic.
func (a *AuthServer) writeOAuthError(w http.ResponseWriter, status int, code, reason string, body any) {
	httpdiag.RejectOAuth(w, code, reason)
	a.writeJSON(w, status, body)
}

// writeJSON writes v as a JSON response with the given status; error
// responses go through writeOAuthError so they carry a rejection reason.
// Encode failures cannot be reported to the client (headers are out) but are
// logged: a marshal bug must not be permanently invisible.
func (a *AuthServer) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		a.logger.Debug("writing JSON response failed", "err", err)
	}
}

// clientDisplayName returns the registered client name or a fallback.
func clientDisplayName(c *clientIDClaims) string {
	if c.ClientName != "" {
		return c.ClientName
	}
	return "An MCP client"
}
