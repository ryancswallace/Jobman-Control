package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func authorizeMonitoringService(ctx context.Context, tx pgx.Tx, principal domain.Principal) error {
	actor := principal.Delegation
	if actor == nil || !actor.ServiceOnly || actor.Operation != domain.CapabilityEventsRead || actor.Mode != "worker" || actor.DirectoryID != "" || principal.Issuer != "" || principal.Subject != "" || len(actor.NamespaceIDs) == 0 || len(actor.NamespaceIDs) > 320 {
		return domain.ErrForbidden
	}
	key, err := queryDelegationKey(ctx, tx, actor.ServiceID, actor.KeyID)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.ErrForbidden
	}
	if err != nil {
		return err
	}
	if !key.Enabled || key.Audience != actor.Audience || !slices.Contains(key.Operations, domain.CapabilityEventsRead) || !slices.Contains(key.CertificateThumbprints, actor.CertificateThumbprint) {
		return domain.ErrForbidden
	}
	seen := map[string]bool{}
	for _, id := range actor.NamespaceIDs {
		if !domain.IsID(id) || seen[id] || !slices.Contains(key.NamespaceIDs, id) {
			return domain.ErrForbidden
		}
		seen[id] = true
	}
	var validTime bool
	var namespaceCount int
	err = tx.QueryRow(ctx, `SELECT $1::timestamptz<=statement_timestamp()+interval '5 seconds' AND $2::timestamptz>statement_timestamp()-interval '5 seconds'
 AND $1::timestamptz< $2::timestamptz AND $2::timestamptz-$1::timestamptz<=interval '60 seconds'
 AND $1::timestamptz>delegation_issued_after,
 (SELECT count(*) FROM namespaces WHERE id=ANY($3::uuid[])) FROM service_recovery_state WHERE singleton`, actor.IssuedAt, actor.ExpiresAt, actor.NamespaceIDs).Scan(&validTime, &namespaceCount)
	if err != nil {
		return fmt.Errorf("verify monitoring service authority: %w", err)
	}
	if !validTime {
		return domain.ErrAuthorizationUnavailable
	}
	if namespaceCount != len(actor.NamespaceIDs) {
		return domain.ErrForbidden
	}
	return nil
}

func acceptServiceAssertion(ctx context.Context, tx pgx.Tx, principal domain.Principal) error {
	if err := authorizeMonitoringService(ctx, tx, principal); err != nil {
		return err
	}
	actor := principal.Delegation
	result, err := tx.Exec(ctx, `INSERT INTO delegation_assertions(service_id,assertion_id,key_id,operation,namespace_ids,mode,assertion_digest,issued_at,expires_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`, actor.ServiceID, actor.AssertionID, actor.KeyID, actor.Operation, actor.NamespaceIDs, actor.Mode, actor.AssertionDigest, actor.IssuedAt, actor.ExpiresAt)
	if err != nil {
		return fmt.Errorf("record monitoring service assertion: %w", err)
	}
	if result.RowsAffected() != 1 {
		return domain.ErrUnauthenticated
	}
	return nil
}
