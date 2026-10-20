package authsrv

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// grantLifecycle owns sealed grant interpretation and transitions. Keep the
// owner reference: test clocks, stores and domain policy can change after the
// server is constructed, and each operation must use its current dependencies.
type grantLifecycle struct{ owner *AuthServer }

func (a *AuthServer) grants() grantLifecycle { return grantLifecycle{owner: a} }

type codeExchange struct {
	Code, ClientID, RedirectURI, CodeVerifier, Resource string
}

// grantTokens is the input for access-token minting, without exposing stored
// grant status, secret hashes, generations or the family's absolute expiry.
type grantTokens struct {
	Subject, Email, Domain, ClientID, Resource, FamilyID string
	Scopes                                               []string
	GoogleAccessToken                                    string
	GoogleExpiry                                         int64
	RefreshToken                                         string
}

type preparedRefresh struct {
	familyID  string
	record    grantRecord
	claims    refreshClaims
	startedAt time.Time
}

func (p preparedRefresh) GoogleRefreshToken() string { return p.claims.GoogleRefreshToken }

type revokedGrant struct {
	FamilyID, Email, GoogleToken string
}

// grantRejection contains only application-owned, static OAuth failure classes.
// HTTP status/formatting belongs to the token and revocation handlers.
type grantRejection struct{ code, reason, description string }

func (e *grantRejection) Error() string { return e.reason }

type grantStoreError struct {
	operation string
	cause     error
}

func (e *grantStoreError) Error() string { return fmt.Sprintf("%s: %v", e.operation, e.cause) }
func (e *grantStoreError) Unwrap() error { return e.cause }

func (g grantLifecycle) codeReplay(familyID string) error {
	g.owner.logger.Warn("grant_replay", "kind", "authorization_code", "family_id", familyID)
	return &grantRejection{"invalid_grant", "authorization_code_reused", "authorization code was already used"}
}

func (g grantLifecycle) refreshReplay(familyID string) error {
	g.owner.logger.Warn("grant_replay", "kind", "refresh_token", "family_id", familyID)
	return &grantRejection{"invalid_grant", "refresh_token_replayed", "refresh token replay revoked this grant family"}
}

func (g grantLifecycle) ExchangeCode(ctx context.Context, exchange codeExchange) (grantTokens, error) {
	a := g.owner
	now := a.now()
	key := tokenHash(exchange.Code)
	rec, err := a.store.GetCode(ctx, key)
	if err != nil {
		return grantTokens{}, &grantStoreError{"get_code", err}
	}
	// The precheck deliberately reports replay before expiry, and ignores a
	// failed best-effort replay revocation just as the token endpoint did.
	if rec.Status != "active" {
		_ = a.store.RedeemCode(ctx, key, now, grantRecord{})
		return grantTokens{}, g.codeReplay(rec.FamilyID)
	}
	if !now.Before(rec.ExpiresAt) {
		return grantTokens{}, &grantRejection{"invalid_grant", "authorization_code_expired", "authorization code expired"}
	}
	cc, err := openBlob(a.sealer, storedCodeBlob, rec.Claims, now)
	if err != nil {
		return grantTokens{}, &grantRejection{"invalid_grant", "invalid_authorization_code", "invalid authorization code"}
	}
	if exchange.ClientID != cc.ClientID || exchange.RedirectURI != cc.RedirectURI || !verifyPKCE(exchange.CodeVerifier, cc.CodeChallenge) {
		return grantTokens{}, &grantRejection{"invalid_grant", "authorization_code_binding_failed", "authorization code binding failed"}
	}
	if exchange.Resource != "" && strings.TrimRight(exchange.Resource, "/") != a.cfg.IssuerURL {
		return grantTokens{}, &grantRejection{"invalid_target", "unknown_resource", "unknown resource"}
	}
	refreshToken, secretHash := makeRefreshToken(rec.FamilyID)
	claims := sealBlob(a.sealer, storedGrantBlob, refreshClaims{
		Subject: cc.Subject, Email: cc.Email, Domain: cc.Domain, ClientID: cc.ClientID,
		Resource: cc.Resource, Scopes: cc.Scopes, GoogleRefreshToken: cc.GoogleRefreshToken, IssuedAt: now.Unix(),
	})
	grant := grantRecord{Claims: claims, ActiveSecretHash: secretHash, Generation: 1, Status: "active", ExpiresAt: now.Add(a.cfg.refreshTokenTTL()), UpdatedAt: now}
	if err := a.store.RedeemCode(ctx, key, now, grant); err != nil {
		if errors.Is(err, errCodeReplay) {
			return grantTokens{}, g.codeReplay(rec.FamilyID)
		}
		return grantTokens{}, &grantStoreError{"redeem_code", err}
	}
	return grantTokens{
		Subject: cc.Subject, Email: cc.Email, Domain: cc.Domain, ClientID: cc.ClientID, Resource: cc.Resource,
		FamilyID: rec.FamilyID, Scopes: cc.Scopes, GoogleAccessToken: cc.GoogleAccessToken, GoogleExpiry: cc.GoogleExpiry, RefreshToken: refreshToken,
	}, nil
}

func (g grantLifecycle) PrepareRefresh(ctx context.Context, token, clientID string) (preparedRefresh, error) {
	a := g.owner
	now := a.now()
	familyID, secret, err := parseRefreshToken(token)
	if err != nil {
		return preparedRefresh{}, &grantRejection{"invalid_grant", "invalid_refresh_token", "invalid refresh token"}
	}
	rec, err := a.store.GetGrant(ctx, familyID)
	if err != nil {
		return preparedRefresh{}, &grantStoreError{"get_grant", err}
	}
	if !grantActive(rec, now) {
		return preparedRefresh{}, &grantRejection{"invalid_grant", "grant_inactive", "refresh grant is inactive or expired"}
	}
	if !secretMatches(secret, rec.ActiveSecretHash) {
		replayErr := a.store.RotateGrant(ctx, familyID, tokenHash(secret), grantRecord{}, now)
		if replayErr != nil && !errors.Is(replayErr, errGrantReplay) && !errors.Is(replayErr, errGrantInactive) {
			return preparedRefresh{}, &grantStoreError{"revoke_replayed_grant", replayErr}
		}
		return preparedRefresh{}, g.refreshReplay(familyID)
	}
	rc, err := openBlob(a.sealer, storedGrantBlob, rec.Claims, now)
	if err != nil || rc.GoogleRefreshToken == "" {
		return preparedRefresh{}, &grantRejection{"invalid_grant", "invalid_grant_record", "refresh grant is invalid"}
	}
	if clientID != "" && clientID != rc.ClientID {
		return preparedRefresh{}, &grantRejection{"invalid_grant", "client_id_mismatch", "client_id mismatch"}
	}
	if len(a.cfg.AllowedDomains) > 0 && !a.cfg.domainAllowed(rc.Domain, rc.Email) {
		return preparedRefresh{}, &grantRejection{"invalid_grant", "domain_not_allowed", "account domain is not allowed"}
	}
	return preparedRefresh{familyID: familyID, record: rec, claims: rc, startedAt: now}, nil
}

func (g grantLifecycle) RotateRefresh(ctx context.Context, prepared preparedRefresh, googleRefresh string) (grantTokens, error) {
	rc := prepared.claims
	if googleRefresh != "" {
		rc.GoogleRefreshToken = googleRefresh
	}
	nextToken, nextHash := makeRefreshToken(prepared.familyID)
	next := grantRecord{
		Claims: sealBlob(g.owner.sealer, storedGrantBlob, rc), ActiveSecretHash: nextHash,
		Generation: prepared.record.Generation + 1, Status: "active", ExpiresAt: prepared.record.ExpiresAt, UpdatedAt: prepared.startedAt,
	}
	if err := g.owner.store.RotateGrant(ctx, prepared.familyID, prepared.record.ActiveSecretHash, next, prepared.startedAt); err != nil {
		if errors.Is(err, errGrantReplay) || errors.Is(err, errGrantInactive) {
			return grantTokens{}, g.refreshReplay(prepared.familyID)
		}
		return grantTokens{}, &grantStoreError{"rotate_grant", err}
	}
	return grantTokens{
		Subject: rc.Subject, Email: rc.Email, Domain: rc.Domain, ClientID: rc.ClientID, Resource: rc.Resource,
		FamilyID: prepared.familyID, Scopes: rc.Scopes, RefreshToken: nextToken,
	}, nil
}

// RevokeFamily handles terminal upstream refresh/admission loss. The caller
// intentionally treats local revocation as best effort on these error paths.
func (g grantLifecycle) RevokeFamily(ctx context.Context, prepared preparedRefresh) error {
	if err := g.owner.store.RevokeGrant(ctx, prepared.familyID, prepared.startedAt); err != nil {
		return &grantStoreError{"revoke_grant", err}
	}
	return nil
}

func (g grantLifecycle) VerifyAccess(ctx context.Context, token string) (accessClaims, error) {
	a := g.owner
	now := a.now()
	c, err := openBlob(a.sealer, accessBlob, token, now)
	if err != nil {
		a.logger.Warn("access token rejected", "reason", err)
		return accessClaims{}, &tokenRejection{reason: "invalid_access_token"}
	}
	grant, grantErr := a.store.GetGrant(ctx, c.FamilyID)
	if grantErr != nil && !errors.Is(grantErr, errStateNotFound) {
		a.logStoreFailure(ctx, "verify_grant", grantErr)
		return accessClaims{}, fmt.Errorf("%w: %w", errStoreUnavailable, grantErr)
	}
	// Store outage precedes access expiry; access expiry precedes missing or
	// inactive family. This ordering determines public rejection diagnostics.
	if !now.Before(time.Unix(c.ExpiresAt, 0)) {
		return accessClaims{}, &tokenRejection{reason: "access_token_expired"}
	}
	if grantErr != nil {
		return accessClaims{}, &tokenRejection{reason: "grant_not_found"}
	}
	if !grantActive(grant, now) {
		return accessClaims{}, &tokenRejection{reason: "grant_inactive"}
	}
	if len(a.cfg.AllowedDomains) > 0 && !a.cfg.domainAllowed(c.Domain, c.Email) {
		a.logger.Warn("access token rejected: domain no longer allowed", "email", c.Email, "hd", c.Domain)
		return accessClaims{}, &tokenRejection{reason: "domain_not_allowed"}
	}
	return c, nil
}

// RevokeToken returns nil for an RFC 7009 no-op. Refresh possession differs
// from refresh replay: a wrong secret never revokes, while a current secret
// remains valid on expired/revoked families so Google revocation can be retried.
func (g grantLifecycle) RevokeToken(ctx context.Context, token string) (*revokedGrant, error) {
	a := g.owner
	now := a.now()
	out := &revokedGrant{}
	if ac, err := openBlob(a.sealer, accessBlob, token, now); err == nil {
		out.FamilyID, out.GoogleToken, out.Email = ac.FamilyID, ac.GoogleAccessToken, ac.Email
		if err := a.store.RevokeGrant(ctx, out.FamilyID, now); err != nil {
			return nil, &grantStoreError{"revoke_grant", err}
		}
		return out, nil
	}
	id, secret, err := parseRefreshToken(token)
	if err != nil {
		return nil, nil
	}
	rec, err := a.store.GetGrant(ctx, id)
	if errors.Is(err, errStateNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, &grantStoreError{"revoke_get_grant", err}
	}
	if !secretMatches(secret, rec.ActiveSecretHash) {
		return nil, nil
	}
	out.FamilyID = id
	if rc, err := openBlob(a.sealer, storedGrantBlob, rec.Claims, now); err == nil {
		out.GoogleToken, out.Email = rc.GoogleRefreshToken, rc.Email
	}
	// Re-check possession in the local write: a rotation between GetGrant
	// and this transaction must turn the old token into a no-op.
	revoked, err := a.store.RevokeGrantIfSecret(ctx, id, tokenHash(secret), now)
	if err != nil {
		return nil, &grantStoreError{"revoke_grant", err}
	}
	if !revoked {
		return nil, nil
	}
	return out, nil
}

func grantActive(rec grantRecord, now time.Time) bool {
	return rec.Status == "active" && now.Before(rec.ExpiresAt)
}

// These transition rules are invoked only with the adapter's current record
// inside its mutex or Firestore transaction. The adapter commits any replay
// revocation before surfacing the classified replay error.
func codeRedemptionDecision(rec codeRecord, now time.Time) error {
	if !now.Before(rec.ExpiresAt) {
		return errStateNotFound
	}
	if rec.Status != "active" {
		return errCodeReplay
	}
	return nil
}

func grantRotationDecision(rec grantRecord, expected string, now time.Time) error {
	if !grantActive(rec, now) {
		return errGrantInactive
	}
	if rec.ActiveSecretHash != expected {
		return errGrantReplay
	}
	return nil
}

func revokeGrantRecord(rec grantRecord, now time.Time) grantRecord {
	rec.Status, rec.UpdatedAt = "revoked", now
	return rec
}

func grantRevokePossessed(rec grantRecord, expected string) bool {
	// No status or expiry condition: retain current-secret upstream retries.
	return rec.ActiveSecretHash == expected
}
