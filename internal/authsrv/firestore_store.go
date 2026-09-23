package authsrv

import (
	"context"
	"errors"
	"fmt"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	oauthStatesCollection = "mcp_oauth_states"
	oauthCodesCollection  = "mcp_oauth_codes"
	oauthGrantsCollection = "mcp_oauth_grants"
)

type firestoreStateStore struct{ client *firestore.Client }

func newFirestoreStateStore(ctx context.Context, project, database string) (*firestoreStateStore, error) {
	client, err := firestore.NewClientWithDatabase(ctx, project, database)
	if err != nil {
		return nil, fmt.Errorf("creating Firestore OAuth state client: %w", err)
	}
	return &firestoreStateStore{client: client}, nil
}

func (s *firestoreStateStore) state(key string) *firestore.DocumentRef {
	return s.client.Collection(oauthStatesCollection).Doc(key)
}

func (s *firestoreStateStore) code(key string) *firestore.DocumentRef {
	return s.client.Collection(oauthCodesCollection).Doc(key)
}

func (s *firestoreStateStore) grant(id string) *firestore.DocumentRef {
	return s.client.Collection(oauthGrantsCollection).Doc(id)
}

func (s *firestoreStateStore) Health(ctx context.Context) error {
	_, err := s.client.Collections(ctx).Next()
	if errors.Is(err, iterator.Done) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking Firestore OAuth state store: %w", err)
	}
	return nil
}

func (s *firestoreStateStore) PutAuthorizationState(ctx context.Context, key string, rec authorizationStateRecord) error {
	_, err := s.state(key).Create(ctx, rec)
	if err != nil {
		return fmt.Errorf("creating OAuth authorization state: %w", err)
	}
	return nil
}

func (s *firestoreStateStore) GetAuthorizationState(ctx context.Context, key string) (authorizationStateRecord, error) {
	doc, err := s.state(key).Get(ctx)
	if status.Code(err) == codes.NotFound {
		return authorizationStateRecord{}, errStateNotFound
	}
	if err != nil {
		return authorizationStateRecord{}, fmt.Errorf("reading OAuth authorization state: %w", err)
	}
	var rec authorizationStateRecord
	if err := doc.DataTo(&rec); err != nil {
		return authorizationStateRecord{}, fmt.Errorf("decoding OAuth authorization state: %w", err)
	}
	return rec, nil
}

func (s *firestoreStateStore) UseAuthorizationState(ctx context.Context, key string, now time.Time) (authorizationStateRecord, error) {
	var rec authorizationStateRecord
	err := s.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		ref := s.state(key)
		doc, err := tx.Get(ref)
		if status.Code(err) == codes.NotFound {
			return errStateNotFound
		}
		if err != nil {
			return fmt.Errorf("reading OAuth authorization state in transaction: %w", err)
		}
		if err := doc.DataTo(&rec); err != nil {
			return fmt.Errorf("decoding OAuth authorization state in transaction: %w", err)
		}
		if !now.Before(rec.ExpiresAt) {
			return errBlobExpired
		}
		if rec.Status != "active" {
			return errStateReplay
		}
		if err := tx.Update(ref, []firestore.Update{{Path: "status", Value: "used"}, {Path: "used_at", Value: now}}); err != nil {
			return fmt.Errorf("marking OAuth authorization state used: %w", err)
		}
		return nil
	})
	if err != nil {
		return rec, fmt.Errorf("consuming OAuth authorization state: %w", err)
	}
	return rec, nil
}

func (s *firestoreStateStore) PutCode(ctx context.Context, key string, rec codeRecord) error {
	_, err := s.code(key).Create(ctx, rec)
	if err != nil {
		return fmt.Errorf("creating OAuth authorization code: %w", err)
	}
	return nil
}

func (s *firestoreStateStore) GetCode(ctx context.Context, key string) (codeRecord, error) {
	doc, err := s.code(key).Get(ctx)
	if status.Code(err) == codes.NotFound {
		return codeRecord{}, errStateNotFound
	}
	if err != nil {
		return codeRecord{}, fmt.Errorf("reading OAuth authorization code: %w", err)
	}
	var rec codeRecord
	if err := doc.DataTo(&rec); err != nil {
		return codeRecord{}, fmt.Errorf("decoding OAuth authorization code: %w", err)
	}
	return rec, nil
}

func (s *firestoreStateStore) RedeemCode(ctx context.Context, key string, now time.Time, grant grantRecord) error {
	replayed := false
	err := s.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		replayed = false
		ref := s.code(key)
		doc, err := tx.Get(ref)
		if status.Code(err) == codes.NotFound {
			return errStateNotFound
		}
		if err != nil {
			return fmt.Errorf("reading OAuth authorization code in transaction: %w", err)
		}
		var rec codeRecord
		if err := doc.DataTo(&rec); err != nil {
			return fmt.Errorf("decoding OAuth authorization code in transaction: %w", err)
		}
		if !now.Before(rec.ExpiresAt) {
			return errStateNotFound
		}
		if rec.Status != "active" {
			replayed = true
			if err := tx.Set(s.grant(rec.FamilyID), map[string]any{"status": "revoked", "updated_at": now}, firestore.MergeAll); err != nil {
				return fmt.Errorf("revoking replayed OAuth grant: %w", err)
			}
			return nil
		}
		if err := tx.Update(ref, []firestore.Update{{Path: "status", Value: "used"}, {Path: "used_at", Value: now}}); err != nil {
			return fmt.Errorf("marking OAuth authorization code used: %w", err)
		}
		if err := tx.Create(s.grant(rec.FamilyID), grant); err != nil {
			return fmt.Errorf("creating OAuth grant: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("redeeming OAuth authorization code: %w", err)
	}
	if replayed {
		return errCodeReplay
	}
	return nil
}

func (s *firestoreStateStore) GetGrant(ctx context.Context, id string) (grantRecord, error) {
	doc, err := s.grant(id).Get(ctx)
	if status.Code(err) == codes.NotFound {
		return grantRecord{}, errStateNotFound
	}
	if err != nil {
		return grantRecord{}, fmt.Errorf("reading OAuth grant: %w", err)
	}
	var rec grantRecord
	if err := doc.DataTo(&rec); err != nil {
		return grantRecord{}, fmt.Errorf("%w: %w", errGrantCorrupt, err)
	}
	return rec, nil
}

func (s *firestoreStateStore) RotateGrant(ctx context.Context, id, expected string, next grantRecord, now time.Time) error {
	replayed := false
	err := s.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		replayed = false
		ref := s.grant(id)
		doc, err := tx.Get(ref)
		if status.Code(err) == codes.NotFound {
			return errGrantInactive
		}
		if err != nil {
			return fmt.Errorf("reading OAuth grant in transaction: %w", err)
		}
		var current grantRecord
		if err := doc.DataTo(&current); err != nil {
			return fmt.Errorf("decoding OAuth grant in transaction: %w", err)
		}
		if current.Status != "active" || !now.Before(current.ExpiresAt) {
			return errGrantInactive
		}
		if current.ActiveSecretHash != expected {
			replayed = true
			if err := tx.Update(ref, []firestore.Update{{Path: "status", Value: "revoked"}, {Path: "updated_at", Value: now}}); err != nil {
				return fmt.Errorf("revoking replayed OAuth grant: %w", err)
			}
			return nil
		}
		if err := tx.Set(ref, next); err != nil {
			return fmt.Errorf("rotating OAuth grant: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("rotating OAuth refresh token: %w", err)
	}
	if replayed {
		return errGrantReplay
	}
	return nil
}

func (s *firestoreStateStore) RevokeGrant(ctx context.Context, id string, now time.Time) error {
	err := s.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		ref := s.grant(id)
		if _, err := tx.Get(ref); status.Code(err) == codes.NotFound {
			return nil
		} else if err != nil {
			return fmt.Errorf("reading OAuth grant for revocation: %w", err)
		}
		if err := tx.Update(ref, []firestore.Update{{Path: "status", Value: "revoked"}, {Path: "updated_at", Value: now}}); err != nil {
			return fmt.Errorf("revoking OAuth grant: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("running OAuth grant revocation transaction: %w", err)
	}
	return nil
}

func (s *firestoreStateStore) Close() error {
	if s == nil || s.client == nil {
		return nil
	}
	if err := s.client.Close(); err != nil {
		return fmt.Errorf("closing Firestore OAuth state client: %w", err)
	}
	return nil
}

var _ oauthStateStore = (*firestoreStateStore)(nil)
