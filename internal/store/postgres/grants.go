package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

// PutMembershipGrant adds an immutable manual contribution. Its UUID is also
// its idempotency identity: a revoked UUID cannot be used to recreate access.
func (store *Store) PutMembershipGrant(ctx context.Context, actor domain.Principal, namespace, grantID string, grant domain.MembershipGrant) (domain.ContributingGrant, error) {
	if !domain.IsID(grantID) {
		return domain.ContributingGrant{}, errors.New("membership grant ID is invalid")
	}
	if err := domain.ValidateMembershipGrant(grant); err != nil {
		return domain.ContributingGrant{}, err
	}
	principalID, err := store.newID()
	if err != nil {
		return domain.ContributingGrant{}, err
	}
	return inTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.ContributingGrant, error) {
		authorization, err := authorizeNamespace(ctx, tx, actor, namespace, domain.CapabilityMembershipsManage)
		if err != nil {
			return domain.ContributingGrant{}, err
		}
		// Serialize concurrent creates/deletes for this grant without relying on a
		// sequence allocation to supply ordering. No external I/O occurs here.
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, grantID); err != nil {
			return domain.ContributingGrant{}, fmt.Errorf("lock membership grant: %w", err)
		}
		existing, err := getContributingGrant(ctx, tx, authorization.namespaceID, grantID)
		if err == nil {
			if existing.Provenance != "manual" || existing.Issuer != grant.Issuer || existing.Subject != grant.Subject || existing.Role != grant.Role || existing.RevokedAt != nil {
				return domain.ContributingGrant{}, domain.ErrConflict
			}
			return existing, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return domain.ContributingGrant{}, err
		}
		if err = tx.QueryRow(ctx, `INSERT INTO principals (id, issuer, subject, display_name) VALUES ($1,$2,$3,$4)
   ON CONFLICT (issuer, subject) DO UPDATE SET display_name = EXCLUDED.display_name RETURNING id::text`, principalID, grant.Issuer, grant.Subject, grant.DisplayName).Scan(&principalID); err != nil {
			return domain.ContributingGrant{}, fmt.Errorf("resolve grant principal: %w", err)
		}
		inserted, err := tx.Exec(ctx, `INSERT INTO membership_grants (id, namespace_id, principal_id, role, provenance, source_key)
   VALUES ($1::uuid,$2,$3,$4,'manual',$1::uuid::text) ON CONFLICT (id) DO NOTHING`, grantID, authorization.namespaceID, principalID, grant.Role)
		if err != nil {
			return domain.ContributingGrant{}, fmt.Errorf("insert membership grant: %w", err)
		}
		if inserted.RowsAffected() != 1 {
			return domain.ContributingGrant{}, domain.ErrConflict
		}
		if err := auditGrant(ctx, tx, authorization, grantID, principalID, grant.Role, "membership.grant.created"); err != nil {
			return domain.ContributingGrant{}, err
		}
		return getContributingGrant(ctx, tx, authorization.namespaceID, grantID)
	})
}

// RevokeMembershipGrant tombstones one manual contribution. Repeated deletion
// is harmless; contributions from other grants remain active.
func (store *Store) RevokeMembershipGrant(ctx context.Context, actor domain.Principal, namespace, grantID string) (domain.ContributingGrant, error) {
	if !domain.IsID(grantID) {
		return domain.ContributingGrant{}, errors.New("membership grant ID is invalid")
	}
	return inTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.ContributingGrant, error) {
		authorization, err := authorizeNamespace(ctx, tx, actor, namespace, domain.CapabilityMembershipsManage)
		if err != nil {
			return domain.ContributingGrant{}, err
		}
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, grantID); err != nil {
			return domain.ContributingGrant{}, fmt.Errorf("lock membership grant: %w", err)
		}
		grant, err := getContributingGrant(ctx, tx, authorization.namespaceID, grantID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ContributingGrant{}, domain.ErrNotFound
		}
		if err != nil {
			return domain.ContributingGrant{}, err
		}
		if grant.Provenance != "manual" {
			return domain.ContributingGrant{}, domain.ErrConflict
		}
		if grant.RevokedAt != nil {
			return grant, nil
		}
		if _, err = tx.Exec(ctx, `UPDATE membership_grants SET revoked_at = transaction_timestamp(), updated_at = transaction_timestamp() WHERE id = $1`, grantID); err != nil {
			return domain.ContributingGrant{}, fmt.Errorf("revoke membership grant: %w", err)
		}
		if err := auditGrant(ctx, tx, authorization, grantID, grant.PrincipalID, grant.Role, "membership.grant.revoked"); err != nil {
			return domain.ContributingGrant{}, err
		}
		return getContributingGrant(ctx, tx, authorization.namespaceID, grantID)
	})
}

func getContributingGrant(ctx context.Context, tx pgx.Tx, namespaceID, grantID string) (domain.ContributingGrant, error) {
	var grant domain.ContributingGrant
	err := tx.QueryRow(ctx, `SELECT g.id::text, n.name, p.id::text, p.issuer, p.subject, p.display_name,
  g.role, g.provenance, g.created_at, g.revoked_at
  FROM membership_grants AS g JOIN namespaces AS n ON n.id = g.namespace_id
  JOIN principals AS p ON p.id = g.principal_id WHERE g.namespace_id = $1 AND g.id = $2`, namespaceID, grantID).Scan(&grant.ID, &grant.Namespace, &grant.PrincipalID, &grant.Issuer, &grant.Subject, &grant.DisplayName, &grant.Role, &grant.Provenance, &grant.CreatedAt, &grant.RevokedAt)
	if err != nil {
		return domain.ContributingGrant{}, fmt.Errorf("get membership grant: %w", err)
	}
	grant.CreatedAt = grant.CreatedAt.UTC()
	if grant.RevokedAt != nil {
		value := grant.RevokedAt.UTC()
		grant.RevokedAt = &value
	}
	return grant, nil
}

func auditGrant(ctx context.Context, tx pgx.Tx, authorization namespaceAuthorization, grantID, principalID, role, action string) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_events (namespace_id, actor_principal_id, action, resource_type, resource_id, details)
 VALUES ($1,$2,$3,'membership_grant',$4,jsonb_build_object('principalId',$5::text,'role',$6::text,'provenance','manual'))`, authorization.namespaceID, authorization.principalID, action, grantID, principalID, role)
	if err != nil {
		return fmt.Errorf("audit membership grant: %w", err)
	}
	return nil
}
