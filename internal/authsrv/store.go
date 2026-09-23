package authsrv

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

var (
	errStateNotFound = errors.New("oauth state not found")
	errStateReplay   = errors.New("oauth state replay")
	errCodeReplay    = errors.New("authorization code replay")
	errGrantReplay   = errors.New("refresh token replay")
	errGrantInactive = errors.New("oauth grant inactive")
	// errGrantCorrupt marks a stored grant record that no longer decodes.
	errGrantCorrupt = errors.New("decoding OAuth grant")
)

type authorizationStateRecord struct {
	Claims    string    `firestore:"claims"`
	Status    string    `firestore:"status"`
	ExpiresAt time.Time `firestore:"expires_at"`
	UsedAt    time.Time `firestore:"used_at,omitempty"`
}

type codeRecord struct {
	Claims    string    `firestore:"claims"`
	Status    string    `firestore:"status"`
	FamilyID  string    `firestore:"grant_id"`
	ExpiresAt time.Time `firestore:"expires_at"`
	UsedAt    time.Time `firestore:"used_at,omitempty"`
}

type grantRecord struct {
	Claims           string    `firestore:"claims"`
	ActiveSecretHash string    `firestore:"active_secret_hash"`
	Generation       int64     `firestore:"generation"`
	Status           string    `firestore:"status"`
	ExpiresAt        time.Time `firestore:"expires_at"`
	UpdatedAt        time.Time `firestore:"updated_at"`
}

type oauthStateStore interface {
	Health(context.Context) error
	PutAuthorizationState(context.Context, string, authorizationStateRecord) error
	GetAuthorizationState(context.Context, string) (authorizationStateRecord, error)
	UseAuthorizationState(context.Context, string, time.Time) (authorizationStateRecord, error)
	PutCode(context.Context, string, codeRecord) error
	GetCode(context.Context, string) (codeRecord, error)
	RedeemCode(context.Context, string, time.Time, grantRecord) error
	GetGrant(context.Context, string) (grantRecord, error)
	RotateGrant(context.Context, string, string, grantRecord, time.Time) error
	RevokeGrant(context.Context, string, time.Time) error
	Close() error
}

func randomOpaque(bytes int) (string, error) {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating random OAuth value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func tokenHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func secretMatches(raw, expected string) bool {
	actual := tokenHash(raw)
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1
}

func makeAuthorizationCode() (raw, key string, err error) {
	secret, err := randomOpaque(32)
	if err != nil {
		return "", "", err
	}
	raw = prefixCode + secret
	return raw, tokenHash(raw), nil
}

func makeRefreshToken(familyID string) (raw, hash string, err error) {
	secret, err := randomOpaque(32)
	if err != nil {
		return "", "", err
	}
	raw = prefixRefresh + familyID + "." + secret
	return raw, tokenHash(secret), nil
}

func parseRefreshToken(raw string) (familyID, secret string, err error) {
	body, ok := strings.CutPrefix(raw, prefixRefresh)
	if !ok {
		return "", "", errGrantInactive
	}
	familyID, secret, ok = strings.Cut(body, ".")
	if !ok || familyID == "" || secret == "" {
		return "", "", errGrantInactive
	}
	return familyID, secret, nil
}

// grant reads a grant family. A record that no longer decodes can never
// authorize anything again, so it is logged and reported as errStateNotFound
// (the token is rejected) rather than as a store outage, which would answer
// 503 on every retry.
func (a *AuthServer) grant(ctx context.Context, familyID string) (grantRecord, error) {
	rec, err := a.store.GetGrant(ctx, familyID)
	if errors.Is(err, errGrantCorrupt) {
		a.logger.Error("oauth_grant_corrupt", "family_id", familyID, "err", err)
		return grantRecord{}, fmt.Errorf("%w: %w", errStateNotFound, err)
	}
	if err != nil {
		return grantRecord{}, fmt.Errorf("reading grant family: %w", err)
	}
	return rec, nil
}

// logStoreFailure logs a failed OAuth state store call. A call that failed
// because the client abandoned the request says nothing about the store and
// is logged at Debug.
func (a *AuthServer) logStoreFailure(ctx context.Context, operation string, err error) {
	level := slog.LevelError
	if errors.Is(ctx.Err(), context.Canceled) {
		level = slog.LevelDebug
	}
	a.logger.Log(ctx, level, "oauth_store_failure", "operation", operation, "err", err)
}

// memoryStateStore is used only by unit tests; production always constructs
// the Firestore implementation.
type memoryStateStore struct {
	mu     sync.Mutex
	states map[string]authorizationStateRecord
	codes  map[string]codeRecord
	grants map[string]grantRecord
}

func newMemoryStateStore() *memoryStateStore {
	return &memoryStateStore{
		states: map[string]authorizationStateRecord{},
		codes:  map[string]codeRecord{},
		grants: map[string]grantRecord{},
	}
}

func (s *memoryStateStore) Health(context.Context) error { return nil }

func (s *memoryStateStore) PutAuthorizationState(_ context.Context, key string, rec authorizationStateRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.states[key]; exists {
		return fmt.Errorf("duplicate authorization state")
	}
	s.states[key] = rec
	return nil
}

func (s *memoryStateStore) GetAuthorizationState(_ context.Context, key string) (authorizationStateRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.states[key]
	if !ok {
		return authorizationStateRecord{}, errStateNotFound
	}
	return rec, nil
}

func (s *memoryStateStore) UseAuthorizationState(_ context.Context, key string, now time.Time) (authorizationStateRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.states[key]
	if !ok {
		return authorizationStateRecord{}, errStateNotFound
	}
	if !now.Before(rec.ExpiresAt) {
		return authorizationStateRecord{}, errBlobExpired
	}
	if rec.Status != "active" {
		return authorizationStateRecord{}, errStateReplay
	}
	rec.Status, rec.UsedAt = "used", now
	s.states[key] = rec
	return rec, nil
}

func (s *memoryStateStore) PutCode(_ context.Context, key string, rec codeRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.codes[key]; exists {
		return fmt.Errorf("duplicate authorization code")
	}
	s.codes[key] = rec
	return nil
}

func (s *memoryStateStore) GetCode(_ context.Context, key string) (codeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.codes[key]
	if !ok {
		return codeRecord{}, errStateNotFound
	}
	return rec, nil
}

func (s *memoryStateStore) RedeemCode(_ context.Context, key string, now time.Time, grant grantRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.codes[key]
	if !ok || !now.Before(rec.ExpiresAt) {
		return errStateNotFound
	}
	if rec.Status != "active" {
		if g, ok := s.grants[rec.FamilyID]; ok {
			g.Status, g.UpdatedAt = "revoked", now
			s.grants[rec.FamilyID] = g
		}
		return errCodeReplay
	}
	rec.Status, rec.UsedAt = "used", now
	s.codes[key] = rec
	s.grants[rec.FamilyID] = grant
	return nil
}

func (s *memoryStateStore) GetGrant(_ context.Context, id string) (grantRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.grants[id]
	if !ok {
		return grantRecord{}, errStateNotFound
	}
	return rec, nil
}

func (s *memoryStateStore) RotateGrant(_ context.Context, id, expected string, next grantRecord, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.grants[id]
	if !ok || rec.Status != "active" || !now.Before(rec.ExpiresAt) {
		return errGrantInactive
	}
	if rec.ActiveSecretHash != expected {
		rec.Status, rec.UpdatedAt = "revoked", now
		s.grants[id] = rec
		return errGrantReplay
	}
	s.grants[id] = next
	return nil
}

func (s *memoryStateStore) RevokeGrant(_ context.Context, id string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.grants[id]
	if !ok {
		return nil
	}
	rec.Status, rec.UpdatedAt = "revoked", now
	s.grants[id] = rec
	return nil
}

func (s *memoryStateStore) Close() error { return nil }
