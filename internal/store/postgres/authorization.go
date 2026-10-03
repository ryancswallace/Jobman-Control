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
	canonical    domain.Principal
}

func authorizeNamespace(
	ctx context.Context,
	tx pgx.Tx,
	principal domain.Principal,
	namespace string,
	capabilities ...string,
) (namespaceAuthorization, error) {
	var authorization namespaceAuthorization
	if principal.Delegation != nil {
		if len(capabilities) != 1 || !domain.DelegationReadOperation(capabilities[0]) || principal.Delegation.Operation != capabilities[0] {
			return authorization, domain.ErrForbidden
		}
		identity, resolveErr := resolveDelegatedPrincipal(ctx, tx, principal)
		if resolveErr != nil {
			return authorization, resolveErr
		}
		principal = identity.canonical
	} else {
		// Operator-verified aliases also preserve the original owner identity for
		// ordinary Control clients whose OIDC subject changed across audiences.
		var issuer, subject string
		aliasErr := tx.QueryRow(ctx, `SELECT p.issuer,p.subject FROM principal_aliases AS alias JOIN principals AS p ON p.id=alias.principal_id WHERE alias.issuer=$1 AND alias.subject=$2`, principal.Issuer, principal.Subject).Scan(&issuer, &subject)
		if aliasErr == nil {
			principal.Issuer, principal.Subject = issuer, subject
		} else if !errors.Is(aliasErr, pgx.ErrNoRows) {
			return authorization, fmt.Errorf("resolve principal alias: %w", aliasErr)
		}
	}
	var roles []string
	var managed, fresh bool
	err := tx.QueryRow(ctx, `
 SELECT p.id::text,n.id::text,m.roles,managed.namespace_id IS NOT NULL,
 COALESCE(managed.last_verified_at>statement_timestamp()-interval '120 seconds'
 AND managed.last_verified_at<=statement_timestamp()+interval '5 seconds'
 AND account.last_verified_at>statement_timestamp()-interval '120 seconds'
 AND account.last_verified_at<=statement_timestamp()+interval '5 seconds' AND account.enabled,false)
 FROM principals AS p JOIN authorized_memberships AS m ON m.principal_id=p.id
 JOIN namespaces AS n ON n.id=m.namespace_id
 LEFT JOIN namespace_directory_state AS managed ON managed.namespace_id=n.id
 LEFT JOIN directory_accounts AS account ON account.principal_id=p.id
 WHERE p.issuer=$1 AND p.subject=$2 AND n.name=$3
 `, principal.Issuer, principal.Subject, namespace).Scan(&authorization.principalID, &authorization.namespaceID, &roles, &managed, &fresh)
	if errors.Is(err, pgx.ErrNoRows) {
		return authorization, domain.ErrForbidden
	}
	if err != nil {
		return authorization, fmt.Errorf("authorize namespace: %w", err)
	}
	if principal.Delegation != nil && (!managed || !slices.Contains(principal.Delegation.NamespaceIDs, authorization.namespaceID)) {
		return authorization, domain.ErrForbidden
	}
	if managed && !fresh {
		return authorization, domain.ErrAuthorizationUnavailable
	}
	authorization.canonical = principal

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
