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
	principalID  string
	namespaceID  string
	capabilities []string
}

func authorizeNamespace(
	ctx context.Context,
	tx pgx.Tx,
	principal domain.Principal,
	namespace string,
	capabilities ...string,
) (namespaceAuthorization, error) {
	var authorization namespaceAuthorization
	var roles []string
	err := tx.QueryRow(ctx, `
		SELECT p.id::text, n.id::text, m.roles
		FROM principals AS p
		JOIN authorized_memberships AS m ON m.principal_id = p.id
		JOIN namespaces AS n ON n.id = m.namespace_id
		WHERE p.issuer = $1 AND p.subject = $2 AND n.name = $3
	`, principal.Issuer, principal.Subject, namespace).Scan(
		&authorization.principalID, &authorization.namespaceID, &roles,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return namespaceAuthorization{}, domain.ErrForbidden
	}
	if err != nil {
		return namespaceAuthorization{}, fmt.Errorf("authorize namespace: %w", err)
	}
	authorization.capabilities = domain.EffectiveCapabilities(roles)
	for _, capability := range capabilities {
		if !slices.Contains(authorization.capabilities, capability) {
			return namespaceAuthorization{}, domain.ErrForbidden
		}
	}

	return authorization, nil
}

func (authorization namespaceAuthorization) permits(capability string) bool {
	return slices.Contains(authorization.capabilities, capability)
}
