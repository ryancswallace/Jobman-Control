package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func enforceNamespaceQuota(
	ctx context.Context,
	tx pgx.Tx,
	namespaceID string,
	additionalJobs, collectionItems, graphNodes int,
) error {
	var maxQueued, maxCollection, maxGraph, current int
	if err := tx.QueryRow(ctx, `
		SELECT policy.max_queued_jobs, policy.max_collection_items,
			policy.max_graph_nodes,
			(SELECT count(*) FROM jobs WHERE namespace_id = policy.namespace_id AND phase != 'terminal')::integer
		FROM namespace_policies AS policy WHERE policy.namespace_id = $1 FOR UPDATE
	`, namespaceID).Scan(&maxQueued, &maxCollection, &maxGraph, &current); err != nil {
		return fmt.Errorf("lock namespace quota: %w", err)
	}
	if additionalJobs < 1 || current+additionalJobs > maxQueued {
		return fmt.Errorf("%w: queued job limit is %d", domain.ErrQuotaExceeded, maxQueued)
	}
	if collectionItems > maxCollection {
		return fmt.Errorf("%w: collection item limit is %d", domain.ErrQuotaExceeded, maxCollection)
	}
	if graphNodes > maxGraph {
		return fmt.Errorf("%w: graph node limit is %d", domain.ErrQuotaExceeded, maxGraph)
	}

	return nil
}

// GetNamespacePolicy returns admission and retention policy to a current
// namespace member.
func (store *Store) GetNamespacePolicy(
	ctx context.Context,
	principal domain.Principal,
	namespace string,
) (domain.NamespacePolicy, error) {
	policy, err := scanNamespacePolicy(store.pool.QueryRow(ctx, `
		SELECT n.name, policy.max_active_jobs, policy.max_queued_jobs,
			policy.max_collection_items, policy.max_graph_nodes,
			EXTRACT(EPOCH FROM policy.idempotency_retention)::bigint,
			EXTRACT(EPOCH FROM policy.published_outbox_retention)::bigint,
			policy.revision, policy.created_at, policy.updated_at
		FROM namespace_policies AS policy
		JOIN namespaces AS n ON n.id = policy.namespace_id
		JOIN memberships AS membership ON membership.namespace_id = n.id
		JOIN principals AS principal ON principal.id = membership.principal_id
		WHERE principal.issuer = $1 AND principal.subject = $2 AND n.name = $3
	`, principal.Issuer, principal.Subject, namespace))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NamespacePolicy{}, domain.ErrForbidden
	}
	if err != nil {
		return domain.NamespacePolicy{}, fmt.Errorf("get namespace policy: %w", err)
	}

	return policy, nil
}

// UpdateNamespacePolicy applies a complete revision-checked replacement.
func (store *Store) UpdateNamespacePolicy(
	ctx context.Context,
	principal domain.Principal,
	namespace string,
	change domain.NamespacePolicyChange,
) (domain.NamespacePolicy, error) {
	if change.MaxActiveJobs < 1 || change.MaxQueuedJobs < 1 ||
		change.MaxCollectionItems < 1 || change.MaxCollectionItems > 10_000 ||
		change.MaxGraphNodes < 1 || change.MaxGraphNodes > 10_000 ||
		change.IdempotencyRetention < time.Hour || change.IdempotencyRetention > 365*24*time.Hour ||
		change.PublishedOutboxRetention < time.Hour || change.PublishedOutboxRetention > 365*24*time.Hour ||
		change.ExpectedRevision < 1 {
		return domain.NamespacePolicy{}, errors.New("namespace policy change is invalid")
	}
	policy, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.NamespacePolicy, error) {
		authorization, authErr := authorizeNamespace(
			ctx, tx, principal, namespace, domain.RoleNamespaceAdmin,
		)
		if authErr != nil {
			return domain.NamespacePolicy{}, authErr
		}
		command, updateErr := tx.Exec(ctx, `
			UPDATE namespace_policies SET
				max_active_jobs = $2, max_queued_jobs = $3,
				max_collection_items = $4, max_graph_nodes = $5,
				idempotency_retention = $6::interval,
				published_outbox_retention = $7::interval,
				revision = revision + 1, updated_at = transaction_timestamp()
			WHERE namespace_id = $1 AND revision = $8
		`, authorization.namespaceID, change.MaxActiveJobs, change.MaxQueuedJobs,
			change.MaxCollectionItems, change.MaxGraphNodes,
			change.IdempotencyRetention.String(), change.PublishedOutboxRetention.String(),
			change.ExpectedRevision)
		if updateErr != nil {
			return domain.NamespacePolicy{}, fmt.Errorf("update namespace policy: %w", updateErr)
		}
		if command.RowsAffected() != 1 {
			return domain.NamespacePolicy{}, domain.ErrConflict
		}
		if _, updateErr = tx.Exec(ctx, `
			INSERT INTO audit_events (
				namespace_id, actor_principal_id, action, resource_type,
				resource_id, details
			) VALUES ($1, $2, 'namespace.policy.updated', 'namespace', $1,
				jsonb_build_object('revision', $3::bigint,
					'maxActiveJobs', $4::integer, 'maxQueuedJobs', $5::integer,
					'maxCollectionItems', $6::integer, 'maxGraphNodes', $7::integer))
		`, authorization.namespaceID, authorization.principalID,
			change.ExpectedRevision+1, change.MaxActiveJobs, change.MaxQueuedJobs,
			change.MaxCollectionItems, change.MaxGraphNodes); updateErr != nil {
			return domain.NamespacePolicy{}, fmt.Errorf("audit namespace policy: %w", updateErr)
		}

		return scanNamespacePolicy(tx.QueryRow(ctx, `
			SELECT $2::text, max_active_jobs, max_queued_jobs,
				max_collection_items, max_graph_nodes,
				EXTRACT(EPOCH FROM idempotency_retention)::bigint,
				EXTRACT(EPOCH FROM published_outbox_retention)::bigint,
				revision, created_at, updated_at
			FROM namespace_policies WHERE namespace_id = $1
		`, authorization.namespaceID, namespace))
	})
	if err != nil {
		return domain.NamespacePolicy{}, fmt.Errorf("update namespace policy: %w", err)
	}

	return policy, nil
}

func scanNamespacePolicy(row rowScanner) (domain.NamespacePolicy, error) {
	var policy domain.NamespacePolicy
	var idempotencySeconds, outboxSeconds int64
	if err := row.Scan(
		&policy.Namespace, &policy.MaxActiveJobs, &policy.MaxQueuedJobs,
		&policy.MaxCollectionItems, &policy.MaxGraphNodes, &idempotencySeconds, &outboxSeconds,
		&policy.Revision, &policy.CreatedAt, &policy.UpdatedAt,
	); err != nil {
		return domain.NamespacePolicy{}, err
	}
	policy.IdempotencyRetention = time.Duration(idempotencySeconds) * time.Second
	policy.PublishedOutboxRetention = time.Duration(outboxSeconds) * time.Second
	policy.CreatedAt = policy.CreatedAt.UTC()
	policy.UpdatedAt = policy.UpdatedAt.UTC()

	return policy, nil
}

// ExportAudit returns an ascending, stable audit page to operators and
// namespace administrators. Stored redacted details are returned verbatim.
func (store *Store) ExportAudit(
	ctx context.Context,
	principal domain.Principal,
	namespace string,
	afterID int64,
	limit int,
) (domain.AuditPage, error) {
	if afterID < 0 || limit < 1 || limit > 1000 {
		return domain.AuditPage{}, errors.New("audit export cursor or limit is invalid")
	}
	var namespaceID string
	var role string
	if err := store.pool.QueryRow(ctx, `
		SELECT n.id::text, membership.role
		FROM namespaces AS n
		JOIN memberships AS membership ON membership.namespace_id = n.id
		JOIN principals AS principal ON principal.id = membership.principal_id
		WHERE principal.issuer = $1 AND principal.subject = $2 AND n.name = $3
	`, principal.Issuer, principal.Subject, namespace).Scan(&namespaceID, &role); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.AuditPage{}, domain.ErrForbidden
		}
		return domain.AuditPage{}, fmt.Errorf("authorize audit export: %w", err)
	}
	if role != domain.RoleOperator && role != domain.RoleNamespaceAdmin {
		return domain.AuditPage{}, domain.ErrForbidden
	}
	rows, err := store.pool.Query(ctx, `
		SELECT event.id, $3::text, event.actor_kind,
			COALESCE(event.actor_principal_id::text, ''), COALESCE(event.actor_agent_id::text, ''),
			event.action, event.resource_type, event.resource_id::text,
			COALESCE(event.request_digest, ''), COALESCE(event.idempotency_key, ''),
			event.details::text, event.occurred_at
		FROM audit_events AS event
		WHERE event.namespace_id = $1 AND event.id > $2
		ORDER BY event.id LIMIT $4
	`, namespaceID, afterID, namespace, limit+1)
	if err != nil {
		return domain.AuditPage{}, fmt.Errorf("export audit events: %w", err)
	}
	defer rows.Close()
	items := make([]domain.AuditEvent, 0, limit+1)
	for rows.Next() {
		var item domain.AuditEvent
		var details string
		if err = rows.Scan(
			&item.ID, &item.Namespace, &item.ActorKind, &item.ActorPrincipalID,
			&item.ActorAgentID, &item.Action, &item.ResourceType, &item.ResourceID,
			&item.RequestDigest, &item.IdempotencyKey, &details, &item.OccurredAt,
		); err != nil {
			return domain.AuditPage{}, fmt.Errorf("scan audit event: %w", err)
		}
		item.Details = json.RawMessage(details)
		item.OccurredAt = item.OccurredAt.UTC()
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		return domain.AuditPage{}, fmt.Errorf("iterate audit events: %w", err)
	}
	page := domain.AuditPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		page.NextAfterID = page.Items[len(page.Items)-1].ID
	}

	return page, nil
}

// OperationalSnapshot reads bounded-cardinality service health metrics.
func (store *Store) OperationalSnapshot(ctx context.Context) (domain.OperationalSnapshot, error) {
	snapshot := domain.OperationalSnapshot{JobsByPhase: map[string]int64{}, AgentsByStatus: map[string]int64{}}
	rows, err := store.pool.Query(ctx, `SELECT phase, count(*) FROM jobs GROUP BY phase`)
	if err != nil {
		return domain.OperationalSnapshot{}, fmt.Errorf("count jobs by phase: %w", err)
	}
	for rows.Next() {
		var name string
		var count int64
		if err = rows.Scan(&name, &count); err != nil {
			rows.Close()
			return domain.OperationalSnapshot{}, fmt.Errorf("scan job metric: %w", err)
		}
		snapshot.JobsByPhase[name] = count
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return domain.OperationalSnapshot{}, fmt.Errorf("iterate job metrics: %w", err)
	}
	rows, err = store.pool.Query(ctx, `SELECT status, count(*) FROM agents GROUP BY status`)
	if err != nil {
		return domain.OperationalSnapshot{}, fmt.Errorf("count agents by status: %w", err)
	}
	for rows.Next() {
		var name string
		var count int64
		if err = rows.Scan(&name, &count); err != nil {
			rows.Close()
			return domain.OperationalSnapshot{}, fmt.Errorf("scan agent metric: %w", err)
		}
		snapshot.AgentsByStatus[name] = count
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return domain.OperationalSnapshot{}, fmt.Errorf("iterate agent metrics: %w", err)
	}
	var queueAgeSeconds float64
	if err = store.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM outbox WHERE published_at IS NULL),
			(SELECT count(*) FROM executions WHERE observation_confidence = 'stale'),
			COALESCE((SELECT EXTRACT(EPOCH FROM transaction_timestamp() - min(created_at))
				FROM jobs WHERE phase = 'accepted'), 0),
			reconciliation_hold, restore_epoch
		FROM service_recovery_state
	`).Scan(
		&snapshot.UnpublishedOutbox, &snapshot.StaleExecutions, &queueAgeSeconds,
		&snapshot.RecoveryHold, &snapshot.RestoreEpoch,
	); err != nil {
		return domain.OperationalSnapshot{}, fmt.Errorf("read operational metrics: %w", err)
	}
	snapshot.OldestQueueAge = time.Duration(queueAgeSeconds * float64(time.Second))

	return snapshot, nil
}

// PruneOperationalData deletes only completed idempotency and published
// outbox records past each namespace's policy. Active state and audit evidence
// are never touched.
func (store *Store) PruneOperationalData(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 10_000 {
		return 0, errors.New("operational prune limit must be between 1 and 10000")
	}
	removed, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (int, error) {
		idempotency, deleteErr := tx.Exec(ctx, `
			DELETE FROM idempotency_records WHERE ctid IN (
				SELECT record.ctid FROM idempotency_records AS record
				JOIN namespace_policies AS policy ON policy.namespace_id = record.namespace_id
				WHERE record.completed_at IS NOT NULL
					AND record.completed_at < transaction_timestamp() - policy.idempotency_retention
				ORDER BY record.completed_at LIMIT $1
			)
		`, limit)
		if deleteErr != nil {
			return 0, fmt.Errorf("prune idempotency records: %w", deleteErr)
		}
		remaining := limit - int(idempotency.RowsAffected())
		if remaining == 0 {
			return int(idempotency.RowsAffected()), nil
		}
		outbox, deleteErr := tx.Exec(ctx, `
			DELETE FROM outbox WHERE ctid IN (
				SELECT event.ctid FROM outbox AS event
				JOIN namespace_policies AS policy ON policy.namespace_id = event.namespace_id
				WHERE event.published_at IS NOT NULL
					AND event.published_at < transaction_timestamp() - policy.published_outbox_retention
				ORDER BY event.published_at LIMIT $1
			)
		`, remaining)
		if deleteErr != nil {
			return 0, fmt.Errorf("prune outbox records: %w", deleteErr)
		}

		return int(idempotency.RowsAffected() + outbox.RowsAffected()), nil
	})
	if err != nil {
		return 0, fmt.Errorf("prune operational data: %w", err)
	}

	return removed, nil
}
