package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

type namespaceAuthorization struct {
	principalID string
	namespaceID string
	role        string
}

func authorizeNamespace(
	ctx context.Context,
	tx pgx.Tx,
	principal domain.Principal,
	namespace string,
	roles ...string,
) (namespaceAuthorization, error) {
	var authorization namespaceAuthorization
	err := tx.QueryRow(ctx, `
		SELECT p.id::text, n.id::text, m.role
		FROM principals AS p
		JOIN memberships AS m ON m.principal_id = p.id
		JOIN namespaces AS n ON n.id = m.namespace_id
		WHERE p.issuer = $1 AND p.subject = $2 AND n.name = $3
	`, principal.Issuer, principal.Subject, namespace).Scan(
		&authorization.principalID, &authorization.namespaceID, &authorization.role,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return namespaceAuthorization{}, domain.ErrForbidden
	}
	if err != nil {
		return namespaceAuthorization{}, fmt.Errorf("authorize namespace: %w", err)
	}
	if len(roles) != 0 && !slices.Contains(roles, authorization.role) {
		return namespaceAuthorization{}, domain.ErrForbidden
	}

	return authorization, nil
}
