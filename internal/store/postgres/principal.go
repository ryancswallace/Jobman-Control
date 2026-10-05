package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

// CurrentPrincipal exposes only current authorized namespaces. Directory proof
// is explicit per namespace; the top-level timestamp is only a DB snapshot.
func (store *Store) CurrentPrincipal(ctx context.Context, principal domain.Principal, afterID string, limit int) (domain.PrincipalAccess, error) {
	if limit < 1 || limit > domain.MaximumJobListLimit || (afterID != "" && !domain.IsID(afterID)) {
		return domain.PrincipalAccess{}, errors.New("namespace discovery cursor or limit is invalid")
	}
	return inReadTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.PrincipalAccess, error) {
		result := domain.PrincipalAccess{Principal: principal, Namespaces: make([]domain.NamespaceAccess, 0, limit)}
		var allowed any
		delegated := principal.Delegation != nil
		if delegated {
			if principal.Delegation.Operation != domain.CapabilityNamespaceRead {
				return result, domain.ErrForbidden
			}
			identity, resolveErr := resolveDelegatedPrincipal(ctx, tx, principal)
			if resolveErr != nil {
				return result, resolveErr
			}
			principal = identity.canonical
			allowed = principal.Delegation.NamespaceIDs
		} else {
			var issuer, subject string
			aliasErr := tx.QueryRow(ctx, `SELECT p.issuer,p.subject FROM principal_aliases AS alias JOIN principals AS p ON p.id=alias.principal_id WHERE alias.issuer=$1 AND alias.subject=$2`, principal.Issuer, principal.Subject).Scan(&issuer, &subject)
			if aliasErr == nil {
				principal.Issuer, principal.Subject = issuer, subject
			} else if !errors.Is(aliasErr, pgx.ErrNoRows) {
				return result, fmt.Errorf("resolve discovery alias: %w", aliasErr)
			}
		}
		rows, err := tx.Query(ctx, `
 SELECT COALESCE(p.id::text,''),COALESCE(p.display_name,''),COALESCE(directory.directory_id::text,''),transaction_timestamp(),
  COALESCE(access.id::text,''),COALESCE(access.name,''),COALESCE(access.roles,'{}'::text[]),COALESCE(access.revision::text,''),
  COALESCE(access.managed,false),access.verified_at,COALESCE(access.fresh,false)
 FROM (VALUES($1::text,$2::text)) AS caller(issuer,subject)
 LEFT JOIN principals AS p ON p.issuer=caller.issuer AND p.subject=caller.subject
 LEFT JOIN directory_accounts AS directory ON directory.principal_id=p.id
 LEFT JOIN LATERAL(
  SELECT n.id,n.name,m.roles,v.revision,managed.namespace_id IS NOT NULL AS managed,
   LEAST(managed.last_verified_at,account.last_verified_at) AS verified_at,
   managed.last_verified_at>statement_timestamp()-interval '120 seconds' AND managed.last_verified_at<=statement_timestamp()+interval '5 seconds'
   AND account.last_verified_at>statement_timestamp()-interval '120 seconds' AND account.last_verified_at<=statement_timestamp()+interval '5 seconds' AND account.enabled AS fresh
  FROM authorized_memberships AS m JOIN namespaces AS n ON n.id=m.namespace_id
  JOIN authorization_versions AS v ON v.namespace_id=m.namespace_id AND v.principal_id=m.principal_id
  LEFT JOIN namespace_directory_state AS managed ON managed.namespace_id=n.id
  LEFT JOIN directory_accounts AS account ON account.principal_id=p.id
  WHERE m.principal_id=p.id AND (NULLIF($3,'')::uuid IS NULL OR n.id>NULLIF($3,'')::uuid)
   AND ($5::uuid[] IS NULL OR n.id=ANY($5)) AND (NOT $6::boolean OR managed.namespace_id IS NOT NULL)
  ORDER BY n.id LIMIT $4
 ) AS access ON true ORDER BY access.id`, principal.Issuer, principal.Subject, afterID, limit+1, allowed, delegated)
		if err != nil {
			return result, fmt.Errorf("discover current principal: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var access domain.NamespaceAccess
			var managed, fresh bool
			var verified *time.Time
			if scanErr := rows.Scan(&result.PrincipalID, &result.DisplayName, &result.DirectoryID, &result.AuthorizationCheckedAt, &access.ID, &access.Name, &access.Roles, &access.AuthorizationVersion, &managed, &verified, &fresh); scanErr != nil {
				return result, fmt.Errorf("scan current principal: %w", scanErr)
			}
			if access.ID == "" {
				continue
			}
			if managed {
				if !fresh || verified == nil {
					return domain.PrincipalAccess{}, domain.ErrAuthorizationUnavailable
				}
				checked := result.AuthorizationCheckedAt.UTC()
				proof := verified.UTC()
				expires := proof.Add(120 * time.Second)
				access.AuthorizationCheckedAt = &checked
				access.LastDirectoryVerifiedAt = &proof
				access.AuthorizationExpiresAt = &expires
				access.AuthorizationStatus = "verified"
			}
			access.Capabilities = domain.EffectiveCapabilities(access.Roles)
			result.Namespaces = append(result.Namespaces, access)
		}
		if readErr := rows.Err(); readErr != nil {
			return result, fmt.Errorf("iterate current principal: %w", readErr)
		}
		result.AuthorizationCheckedAt = result.AuthorizationCheckedAt.UTC()
		if len(result.Namespaces) > limit {
			result.Namespaces = result.Namespaces[:limit]
			result.NextNamespaceID = result.Namespaces[limit-1].ID
		}
		return result, nil
	})
}
