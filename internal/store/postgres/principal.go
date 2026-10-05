package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

// CurrentPrincipal discovers only the caller's current namespace grants in one
// bounded statement snapshot. Its timestamp is not directory freshness proof.
func (store *Store) CurrentPrincipal(ctx context.Context, principal domain.Principal, afterID string, limit int) (domain.PrincipalAccess, error) {
	if limit < 1 || limit > domain.MaximumJobListLimit || (afterID != "" && !domain.IsID(afterID)) {
		return domain.PrincipalAccess{}, errors.New("namespace discovery cursor or limit is invalid")
	}
	rows, err := store.pool.Query(ctx, `
 SELECT COALESCE(p.id::text, ''), COALESCE(p.display_name, ''), statement_timestamp(),
     COALESCE(access.id::text, ''), COALESCE(access.name, ''),
     COALESCE(access.roles, '{}'::text[]), COALESCE(access.revision::text, '')
 FROM (VALUES ($1::text, $2::text)) AS caller(issuer, subject)
 LEFT JOIN principals AS p ON p.issuer = caller.issuer AND p.subject = caller.subject
 LEFT JOIN LATERAL (
     SELECT n.id, n.name, m.roles, v.revision FROM authorized_memberships AS m
     JOIN namespaces AS n ON n.id = m.namespace_id
     JOIN authorization_versions AS v ON v.namespace_id = m.namespace_id AND v.principal_id = m.principal_id
     WHERE m.principal_id = p.id AND (NULLIF($3, '')::uuid IS NULL OR n.id > NULLIF($3, '')::uuid)
     ORDER BY n.id LIMIT $4
 ) AS access ON true ORDER BY access.id
 `, principal.Issuer, principal.Subject, afterID, limit+1)
	if err != nil {
		return domain.PrincipalAccess{}, fmt.Errorf("discover current principal: %w", err)
	}
	defer rows.Close()
	result := domain.PrincipalAccess{Principal: principal, Namespaces: make([]domain.NamespaceAccess, 0, limit)}
	for rows.Next() {
		var access domain.NamespaceAccess
		if err = rows.Scan(&result.PrincipalID, &result.DisplayName, &result.AuthorizationCheckedAt, &access.ID, &access.Name, &access.Roles, &access.AuthorizationVersion); err != nil {
			return domain.PrincipalAccess{}, fmt.Errorf("scan current principal: %w", err)
		}
		if access.ID != "" {
			access.Capabilities = domain.EffectiveCapabilities(access.Roles)
			result.Namespaces = append(result.Namespaces, access)
		}
	}
	if err = rows.Err(); err != nil {
		return domain.PrincipalAccess{}, fmt.Errorf("iterate current principal: %w", err)
	}
	result.AuthorizationCheckedAt = result.AuthorizationCheckedAt.UTC()
	if len(result.Namespaces) > limit {
		result.Namespaces = result.Namespaces[:limit]
		result.NextNamespaceID = result.Namespaces[limit-1].ID
	}
	return result, nil
}
