package postgres

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

// RegisterDelegationKeys atomically applies the full operator-owned service
// registry. Missing keys are disabled; key material cannot be replaced under
// an existing key ID. Rotation uses a new key ID and an explicit overlap.
func (store *Store) RegisterDelegationKeys(ctx context.Context, keys []domain.DelegationKey) error {
	if len(keys) > 64 {
		return errors.New("too many delegation service keys")
	}
	seen := map[string]bool{}
	for _, key := range keys {
		if err := validateDelegationKey(key); err != nil {
			return err
		}
		id := key.ServiceID + "\x00" + key.KeyID
		if seen[id] {
			return errors.New("duplicate delegation service key")
		}
		seen[id] = true
	}
	_, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (struct{}, error) {
		if _, lockErr := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('jobman-delegation-registry',0))`); lockErr != nil {
			return struct{}{}, lockErr
		}
		if _, disableErr := tx.Exec(ctx, `UPDATE delegation_service_keys SET enabled=false,updated_at=transaction_timestamp() WHERE enabled`); disableErr != nil {
			return struct{}{}, disableErr
		}
		for _, key := range keys {
			var existing []byte
			readErr := tx.QueryRow(ctx, `SELECT public_key FROM delegation_service_keys WHERE service_id=$1 AND key_id=$2`, key.ServiceID, key.KeyID).Scan(&existing)
			if readErr != nil && !errors.Is(readErr, pgx.ErrNoRows) {
				return struct{}{}, readErr
			}
			if len(existing) > 0 && !bytes.Equal(existing, key.PublicKey) {
				return struct{}{}, domain.ErrConflict
			}
			if _, writeErr := tx.Exec(ctx, `INSERT INTO delegation_service_keys(service_id,key_id,audience,public_key,certificate_thumbprints,namespace_ids,operations,enabled) VALUES($1,$2,$3,$4,$5,$6,$7,$8)
    ON CONFLICT(service_id,key_id) DO UPDATE SET audience=EXCLUDED.audience,certificate_thumbprints=EXCLUDED.certificate_thumbprints,namespace_ids=EXCLUDED.namespace_ids,operations=EXCLUDED.operations,enabled=EXCLUDED.enabled,updated_at=transaction_timestamp()`, key.ServiceID, key.KeyID, key.Audience, key.PublicKey, key.CertificateThumbprints, key.NamespaceIDs, key.Operations, key.Enabled); writeErr != nil {
				return struct{}{}, fmt.Errorf("register delegation service: %w", writeErr)
			}
		}
		return struct{}{}, nil
	})
	return err
}

func validateDelegationKey(key domain.DelegationKey) error {
	if key.ServiceID == "" || len(key.ServiceID) > 128 || key.KeyID == "" || len(key.KeyID) > 128 || key.Audience == "" || len(key.Audience) > 512 || len(key.PublicKey) != ed25519.PublicKeySize || len(key.CertificateThumbprints) == 0 || len(key.CertificateThumbprints) > 8 || len(key.NamespaceIDs) == 0 || len(key.NamespaceIDs) > 320 || len(key.Operations) == 0 || len(key.Operations) > 8 {
		return errors.New("delegation service registration is invalid")
	}
	for _, thumbprint := range key.CertificateThumbprints {
		decoded, err := base64.RawURLEncoding.DecodeString(thumbprint)
		if err != nil || len(decoded) != 32 {
			return errors.New("delegation certificate thumbprint is invalid")
		}
	}
	for _, namespaceID := range key.NamespaceIDs {
		if !domain.IsID(namespaceID) {
			return errors.New("delegation namespace ID is invalid")
		}
	}
	for _, operation := range key.Operations {
		if !domain.DelegationOperation(operation) {
			return errors.New("delegation permits only explicit monitoring reads")
		}
	}
	return nil
}

// DelegationKey returns current public registration without caching revocation.
func (store *Store) DelegationKey(ctx context.Context, serviceID, keyID string) (domain.DelegationKey, error) {
	return queryDelegationKey(ctx, store.pool, serviceID, keyID)
}

func queryDelegationKey(ctx context.Context, query collectionQuerier, serviceID, keyID string) (domain.DelegationKey, error) {
	var key domain.DelegationKey
	err := query.QueryRow(ctx, `SELECT service_id,key_id,audience,public_key,certificate_thumbprints,namespace_ids::text[],operations,enabled FROM delegation_service_keys WHERE service_id=$1 AND key_id=$2`, serviceID, keyID).Scan(&key.ServiceID, &key.KeyID, &key.Audience, &key.PublicKey, &key.CertificateThumbprints, &key.NamespaceIDs, &key.Operations, &key.Enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return key, domain.ErrNotFound
	}
	if err != nil {
		return key, fmt.Errorf("read delegation service: %w", err)
	}
	return key, nil
}

// AcceptDelegationAssertion records an authenticated assertion and actor in one
// unique insert. It does not authorize resource access or bootstrap an alias.
func (store *Store) AcceptDelegationAssertion(ctx context.Context, principal domain.Principal) error {
	_, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (struct{}, error) {
		if principal.Delegation != nil && principal.Delegation.ServiceOnly {
			return struct{}{}, acceptServiceAssertion(ctx, tx, principal)
		}
		identity, resolveErr := resolveDelegatedPrincipal(ctx, tx, principal)
		if resolveErr != nil {
			return struct{}{}, resolveErr
		}
		actor := principal.Delegation
		command, insertErr := tx.Exec(ctx, `INSERT INTO delegation_assertions(service_id,assertion_id,key_id,principal_id,directory_id,actor_issuer,actor_subject,operation,namespace_ids,mode,assertion_digest,issued_at,expires_at)
   VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT DO NOTHING`, actor.ServiceID, actor.AssertionID, actor.KeyID, identity.principalID, actor.DirectoryID, principal.Issuer, principal.Subject, actor.Operation, actor.NamespaceIDs, actor.Mode, actor.AssertionDigest, actor.IssuedAt, actor.ExpiresAt)
		if insertErr != nil {
			return struct{}{}, fmt.Errorf("record delegated assertion: %w", insertErr)
		}
		if command.RowsAffected() != 1 {
			return struct{}{}, domain.ErrUnauthenticated
		}
		return struct{}{}, nil
	})
	return err
}

type verifiedDirectoryPrincipal struct {
	principalID string
	canonical   domain.Principal
	verifiedAt  time.Time
}

func resolveDelegatedPrincipal(ctx context.Context, tx pgx.Tx, principal domain.Principal) (verifiedDirectoryPrincipal, error) {
	result := verifiedDirectoryPrincipal{}
	actor := principal.Delegation
	if actor == nil || actor.ServiceOnly || !domain.IsID(actor.DirectoryID) || !domain.DelegationReadOperation(actor.Operation) || len(actor.NamespaceIDs) == 0 || len(actor.NamespaceIDs) > 320 {
		return result, domain.ErrForbidden
	}
	key, err := queryDelegationKey(ctx, tx, actor.ServiceID, actor.KeyID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return result, domain.ErrForbidden
		}
		return result, err
	}
	if !key.Enabled || actor.Audience != key.Audience || !slices.Contains(key.CertificateThumbprints, actor.CertificateThumbprint) || !slices.Contains(key.Operations, actor.Operation) {
		return result, domain.ErrForbidden
	}
	for _, id := range actor.NamespaceIDs {
		if !domain.IsID(id) || !slices.Contains(key.NamespaceIDs, id) {
			return result, domain.ErrForbidden
		}
	}
	var enabled, fresh, validTime bool
	err = tx.QueryRow(ctx, `SELECT account.principal_id::text,p.issuer,p.subject,account.enabled,COALESCE(account.last_verified_at,'epoch'::timestamptz),
 COALESCE(account.last_verified_at<=statement_timestamp()+interval '5 seconds' AND account.last_verified_at>statement_timestamp()-interval '120 seconds',false),
 $4::timestamptz<=statement_timestamp()+interval '5 seconds' AND $5::timestamptz>statement_timestamp()-interval '5 seconds'
 AND $4::timestamptz>(SELECT delegation_issued_after FROM service_recovery_state WHERE singleton)
 FROM principal_aliases AS alias JOIN directory_accounts AS account ON account.directory_id=alias.directory_id AND account.principal_id=alias.principal_id
 JOIN principals AS p ON p.id=account.principal_id
 WHERE alias.issuer=$1 AND alias.subject=$2 AND alias.directory_id=$3`, principal.Issuer, principal.Subject, actor.DirectoryID, actor.IssuedAt, actor.ExpiresAt).Scan(&result.principalID, &result.canonical.Issuer, &result.canonical.Subject, &enabled, &result.verifiedAt, &fresh, &validTime)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, domain.ErrForbidden
	}
	if err != nil {
		return result, fmt.Errorf("resolve delegated identity: %w", err)
	}
	if !validTime {
		return result, domain.ErrAuthorizationUnavailable
	}
	if !enabled {
		return result, domain.ErrForbidden
	}
	if !fresh {
		return result, domain.ErrAuthorizationUnavailable
	}
	result.canonical.Delegation = principal.Delegation
	return result, nil
}

// PruneDelegationAudits removes only records older than the configured content-
// free audit retention (at least 90 days), long after assertion replay expiry.
func (store *Store) PruneDelegationAudits(ctx context.Context, limit int, retention time.Duration) (int, error) {
	if limit < 1 || limit > 10000 || retention < 90*24*time.Hour || retention > 3650*24*time.Hour {
		return 0, errors.New("delegation audit retention bounds are invalid")
	}
	command, err := store.pool.Exec(ctx, `DELETE FROM delegation_assertions WHERE (service_id,assertion_id) IN (SELECT service_id,assertion_id FROM delegation_assertions WHERE accepted_at<statement_timestamp()-($1::bigint*interval '1 second') AND expires_at<statement_timestamp()-interval '5 seconds' ORDER BY accepted_at LIMIT $2)`, int64(retention.Seconds()), limit)
	if err != nil {
		return 0, fmt.Errorf("prune delegation audit: %w", err)
	}
	return int(command.RowsAffected()), nil
}
