package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ReconcileStaleExecutions projects stale confidence when an accepted
// execution's agent has not authenticated within staleAfter. It does not
// infer termination, reassign work, or change the job outcome.
func (store *Store) ReconcileStaleExecutions(
	ctx context.Context,
	staleAfter time.Duration,
	limit int,
) (int, error) {
	if staleAfter < time.Minute || staleAfter > 24*time.Hour {
		return 0, errors.New("stale execution threshold must be between one minute and 24 hours")
	}
	if limit < 1 || limit > 1000 {
		return 0, errors.New("stale execution reconciliation limit must be between 1 and 1000")
	}
	updated := 0
	for updated < limit {
		outboxID, err := store.newID()
		if err != nil {
			return updated, err
		}
		changed, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (bool, error) {
			var executionID, namespaceID, jobID, agentID string
			queryErr := tx.QueryRow(ctx, `
				SELECT e.id::text, e.namespace_id::text, r.job_id::text, e.agent_id::text
				FROM executions AS e
				JOIN runs AS r ON r.id = e.run_id
				JOIN agents AS a ON a.id = e.agent_id
				WHERE e.phase IN ('accepted', 'running')
					AND e.observation_confidence = 'current'
					AND COALESCE(a.last_seen_at, a.updated_at)
						< transaction_timestamp() - $1::interval
				ORDER BY COALESCE(a.last_seen_at, a.updated_at), e.id
				LIMIT 1
				FOR UPDATE OF e SKIP LOCKED
			`, staleAfter.String()).Scan(&executionID, &namespaceID, &jobID, &agentID)
			if errors.Is(queryErr, pgx.ErrNoRows) {
				return false, nil
			}
			if queryErr != nil {
				return false, fmt.Errorf("claim stale execution: %w", queryErr)
			}
			if _, updateErr := tx.Exec(ctx, `
				UPDATE executions
				SET observation_confidence = 'stale', revision = revision + 1,
					confidence_updated_at = transaction_timestamp(),
					updated_at = transaction_timestamp()
				WHERE id = $1 AND observation_confidence = 'current'
			`, executionID); updateErr != nil {
				return false, fmt.Errorf("mark execution stale: %w", updateErr)
			}
			if _, updateErr := tx.Exec(ctx, `
				UPDATE jobs SET revision = revision + 1, updated_at = transaction_timestamp()
				WHERE id = $1
			`, jobID); updateErr != nil {
				return false, fmt.Errorf("project stale job observation: %w", updateErr)
			}
			if _, auditErr := tx.Exec(ctx, `
				INSERT INTO audit_events (
					namespace_id, actor_kind, action, resource_type, resource_id, details
				) VALUES ($1, 'system', 'execution.observation_stale', 'execution', $2,
					jsonb_build_object('agentId', $3::text, 'staleAfter', $4::text))
			`, namespaceID, executionID, agentID, staleAfter.String()); auditErr != nil {
				return false, fmt.Errorf("audit stale execution: %w", auditErr)
			}
			if _, outboxErr := tx.Exec(ctx, `
				INSERT INTO outbox (
					id, namespace_id, topic, aggregate_type, aggregate_id, payload
				) VALUES ($1, $2, 'execution.observation_stale', 'execution', $3,
					jsonb_build_object('executionId', $3::text, 'agentId', $4::text))
			`, outboxID, namespaceID, executionID, agentID); outboxErr != nil {
				return false, fmt.Errorf("insert stale execution outbox event: %w", outboxErr)
			}

			return true, nil
		})
		if err != nil {
			return updated, fmt.Errorf("reconcile stale execution: %w", err)
		}
		if !changed {
			break
		}
		updated++
	}

	return updated, nil
}
