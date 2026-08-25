package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

// EnsureDevelopmentIdentity creates the explicitly configured local
// development principal, namespace, and administrator membership.
func (store *Store) EnsureDevelopmentIdentity(
	ctx context.Context,
	identity domain.DevelopmentIdentity,
) error {
	return store.EnsureBootstrapIdentity(ctx, domain.BootstrapIdentity{
		Principal: identity.Principal, DisplayName: identity.DisplayName,
		Namespace: identity.Namespace, Mode: "development",
	})
}

// EnsureBootstrapIdentity creates an explicitly configured initial principal,
// namespace, and administrator membership.
func (store *Store) EnsureBootstrapIdentity(
	ctx context.Context,
	identity domain.BootstrapIdentity,
) error {
	principalID, err := store.newID()
	if err != nil {
		return err
	}
	namespaceID, err := store.newID()
	if err != nil {
		return err
	}

	_, err = inTransaction(ctx, store.pool, func(tx pgx.Tx) (struct{}, error) {
		var persistedPrincipalID string
		if queryErr := tx.QueryRow(ctx, `
			INSERT INTO principals (id, issuer, subject, display_name)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (issuer, subject) DO UPDATE
			SET display_name = EXCLUDED.display_name
			RETURNING id::text
		`, principalID, identity.Principal.Issuer, identity.Principal.Subject, identity.DisplayName).
			Scan(&persistedPrincipalID); queryErr != nil {
			return struct{}{}, fmt.Errorf("upsert development principal: %w", queryErr)
		}

		persistedNamespaceID, created, queryErr := ensureBootstrapNamespace(
			ctx, tx, namespaceID, identity.Namespace,
		)
		if queryErr != nil {
			return struct{}{}, queryErr
		}
		if _, queryErr = tx.Exec(ctx, `
			INSERT INTO memberships (namespace_id, principal_id, role)
			VALUES ($1, $2, 'namespace_admin')
			ON CONFLICT (namespace_id, principal_id) DO UPDATE
			SET role = EXCLUDED.role, updated_at = transaction_timestamp()
		`, persistedNamespaceID, persistedPrincipalID); queryErr != nil {
			return struct{}{}, fmt.Errorf("upsert development membership: %w", queryErr)
		}
		if created {
			if _, queryErr = tx.Exec(ctx, `
				INSERT INTO audit_events (
					namespace_id, actor_principal_id, action, resource_type,
					resource_id, details
				) VALUES ($1, $2, 'namespace.created', 'namespace', $1, jsonb_build_object('mode', $3::text))
			`, persistedNamespaceID, persistedPrincipalID, identity.Mode); queryErr != nil {
				return struct{}{}, fmt.Errorf("audit development namespace: %w", queryErr)
			}
		}

		return struct{}{}, nil
	})
	if err != nil {
		return fmt.Errorf("bootstrap identity: %w", err)
	}

	return nil
}

func ensureBootstrapNamespace(
	ctx context.Context,
	tx pgx.Tx,
	candidateID string,
	name string,
) (persistedID string, created bool, resultErr error) {
	var namespaceID string
	err := tx.QueryRow(ctx, `
		INSERT INTO namespaces (id, name)
		VALUES ($1, $2)
		ON CONFLICT (name) DO NOTHING
		RETURNING id::text
	`, candidateID, name).Scan(&namespaceID)
	if err == nil {
		return namespaceID, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, fmt.Errorf("create development namespace: %w", err)
	}
	if err = tx.QueryRow(ctx, `SELECT id::text FROM namespaces WHERE name = $1`, name).
		Scan(&namespaceID); err != nil {
		return "", false, fmt.Errorf("read development namespace: %w", err)
	}

	return namespaceID, false, nil
}
