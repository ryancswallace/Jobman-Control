package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

const putMembershipOperation = "memberships.put"

// PutMembership creates or replaces one namespace role binding. Only a
// namespace administrator can perform this operation.
func (store *Store) PutMembership(
	ctx context.Context,
	actor domain.Principal,
	namespace string,
	idempotencyKey string,
	requestDigest string,
	grant domain.MembershipGrant,
) (domain.CreateResult[domain.Membership], error) {
	if !domain.ValidIdempotencyKey(idempotencyKey) {
		return domain.CreateResult[domain.Membership]{}, errors.New("invalid idempotency key")
	}
	if err := domain.ValidateMembershipGrant(grant); err != nil {
		return domain.CreateResult[domain.Membership]{}, err
	}
	principalID, err := store.newID()
	if err != nil {
		return domain.CreateResult[domain.Membership]{}, err
	}

	result, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.CreateResult[domain.Membership], error) {
		authorization, authorizeErr := authorizeNamespace(
			ctx, tx, actor, namespace, domain.RoleNamespaceAdmin,
		)
		if authorizeErr != nil {
			return domain.CreateResult[domain.Membership]{}, authorizeErr
		}
		resourceID, replayed, reserveErr := reserveIdempotency(
			ctx, tx, authorization, putMembershipOperation, "principal",
			idempotencyKey, requestDigest, 200,
		)
		if reserveErr != nil {
			return domain.CreateResult[domain.Membership]{}, reserveErr
		}
		if replayed {
			membership, lookupErr := getMembership(ctx, tx, authorization.namespaceID, resourceID)
			return domain.CreateResult[domain.Membership]{Value: membership, Replayed: true}, lookupErr
		}

		var persistedPrincipalID string
		if queryErr := tx.QueryRow(ctx, `
			INSERT INTO principals (id, issuer, subject, display_name)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (issuer, subject) DO UPDATE
			SET display_name = EXCLUDED.display_name
			RETURNING id::text
		`, principalID, grant.Issuer, grant.Subject, grant.DisplayName).Scan(&persistedPrincipalID); queryErr != nil {
			return domain.CreateResult[domain.Membership]{}, fmt.Errorf("upsert membership principal: %w", queryErr)
		}
		if _, queryErr := tx.Exec(ctx, `
			INSERT INTO memberships (namespace_id, principal_id, role)
			VALUES ($1, $2, $3)
			ON CONFLICT (namespace_id, principal_id) DO UPDATE
			SET role = EXCLUDED.role, updated_at = transaction_timestamp()
		`, authorization.namespaceID, persistedPrincipalID, grant.Role); queryErr != nil {
			return domain.CreateResult[domain.Membership]{}, fmt.Errorf("upsert membership: %w", queryErr)
		}
		if completeErr := completeIdempotency(
			ctx, tx, authorization, putMembershipOperation, idempotencyKey, persistedPrincipalID, 200,
		); completeErr != nil {
			return domain.CreateResult[domain.Membership]{}, completeErr
		}
		details, marshalErr := json.Marshal(map[string]string{
			"issuer": grant.Issuer, "subject": grant.Subject, "role": grant.Role,
		})
		if marshalErr != nil {
			return domain.CreateResult[domain.Membership]{}, fmt.Errorf("encode membership audit details: %w", marshalErr)
		}
		if _, queryErr := tx.Exec(ctx, `
			INSERT INTO audit_events (
				namespace_id, actor_principal_id, action, resource_type,
				resource_id, request_digest, idempotency_key, details
			) VALUES ($1, $2, 'membership.put', 'principal', $3, $4, $5, $6::jsonb)
		`, authorization.namespaceID, authorization.principalID, persistedPrincipalID,
			requestDigest, idempotencyKey, string(details),
		); queryErr != nil {
			return domain.CreateResult[domain.Membership]{}, fmt.Errorf("audit membership: %w", queryErr)
		}
		membership, lookupErr := getMembership(ctx, tx, authorization.namespaceID, persistedPrincipalID)
		return domain.CreateResult[domain.Membership]{Value: membership}, lookupErr
	})
	if err != nil {
		return domain.CreateResult[domain.Membership]{}, fmt.Errorf("put membership: %w", err)
	}

	return result, nil
}

func getMembership(
	ctx context.Context,
	tx pgx.Tx,
	namespaceID string,
	principalID string,
) (domain.Membership, error) {
	var membership domain.Membership
	err := tx.QueryRow(ctx, `
		SELECT n.name, p.id::text, p.issuer, p.subject, p.display_name,
			m.role, m.created_at, m.updated_at
		FROM memberships AS m
		JOIN namespaces AS n ON n.id = m.namespace_id
		JOIN principals AS p ON p.id = m.principal_id
		WHERE m.namespace_id = $1 AND m.principal_id = $2
	`, namespaceID, principalID).Scan(
		&membership.Namespace, &membership.PrincipalID, &membership.Issuer,
		&membership.Subject, &membership.DisplayName, &membership.Role,
		&membership.CreatedAt, &membership.UpdatedAt,
	)
	if err != nil {
		return domain.Membership{}, err
	}
	membership.CreatedAt = membership.CreatedAt.UTC()
	membership.UpdatedAt = membership.UpdatedAt.UTC()

	return membership, nil
}
