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

const jobSelect = `
 SELECT j.id::text, n.name, j.name, j.labels::text, j.phase,
   j.desired_state, COALESCE(j.outcome, ''), j.placement_target,
   COALESCE(j.placement_partition, ''), j.workload_digest,
   j.request_digest, j.revision, j.created_at, j.updated_at,
   COALESCE(j.target_id::text, ''), COALESCE(j.target_generation_id::text, ''),
   COALESCE(tg.execution_backend, ''), COALESCE(current_execution.native_id, ''),
   COALESCE(current_execution.native_backend, ''), COALESCE(current_execution.native_state, ''),
   COALESCE(current_execution.native_reason, ''), COALESCE(current_execution.native_cluster, ''),
   current_execution.native_observed_at, COALESCE(current_execution.observation_confidence, ''),
   current_execution.confidence_updated_at,
   n.id::text, j.imported, job_owner.id::text, job_owner.issuer, job_owner.subject, job_owner.display_name,
   j.started_at, j.started_recorded_at, COALESCE(j.started_provenance, ''),
   j.completed_at, j.completed_recorded_at, COALESCE(j.completed_provenance, ''),
   COALESCE(current_execution.run_id, ''), COALESCE(current_execution.run_number, ''),
   COALESCE(current_execution.execution_id, ''), COALESCE(j.collection_id::text, ''), j.collection_index,
   COALESCE(j.graph_id::text, ''), j.graph_index, COALESCE(j.graph_disposition, '')
 FROM jobs AS j JOIN namespaces AS n ON n.id = j.namespace_id
 JOIN principals AS job_owner ON job_owner.id = j.owner_principal_id
 LEFT JOIN target_generations AS tg ON tg.id = j.target_generation_id
 LEFT JOIN LATERAL (
   SELECT current_run.id::text AS run_id, current_run.run_number::text,
     e.id::text AS execution_id, e.native_id, e.native_backend, e.native_state,
     e.native_reason, e.native_cluster, e.native_observed_at, e.observation_confidence, e.confidence_updated_at
   FROM runs AS current_run LEFT JOIN executions AS e ON e.run_id = current_run.id
   WHERE current_run.job_id = j.id ORDER BY current_run.run_number DESC LIMIT 1
 ) AS current_execution ON true
`

// Capabilities returns non-sensitive source identity and implemented features.
func (store *Store) Capabilities(ctx context.Context) (domain.ControlCapabilities, error) {
	result := domain.ControlCapabilities{ContractVersions: []string{"jobman.control/v1alpha1"}, Features: []string{"namespace-discovery", "role-unions", "job-monitoring", "namespace-summary", "group-catalogs", "bounded-graph-monitoring"}, MaximumPageSize: domain.MaximumJobListLimit}
	if err := store.pool.QueryRow(ctx, `SELECT i.id::text, r.restore_epoch::text, statement_timestamp()
 FROM control_instance AS i CROSS JOIN service_recovery_state AS r WHERE i.singleton AND r.singleton`).Scan(&result.InstanceID, &result.RecoveryEpoch, &result.ServerTime); err != nil {
		return domain.ControlCapabilities{}, fmt.Errorf("discover Control capabilities: %w", err)
	}
	result.ServerTime = result.ServerTime.UTC()
	return result, nil
}

// NamespaceSummary computes authorized complete counts in one statement.
func (store *Store) NamespaceSummary(ctx context.Context, principal domain.Principal, namespace string, from, before *time.Time) (domain.NamespaceSummary, error) {
	if (from == nil) != (before == nil) || (from != nil && !from.Before(*before)) {
		return domain.NamespaceSummary{}, errors.New("completion window is invalid")
	}
	var result domain.NamespaceSummary
	var phases, outcomes []byte
	err := store.pool.QueryRow(ctx, `
 WITH scope AS (
   SELECT n.id,n.name FROM namespaces AS n
   JOIN authorized_memberships AS m ON m.namespace_id=n.id
   JOIN principals AS p ON p.id=m.principal_id
   WHERE p.issuer=$1 AND p.subject=$2 AND n.name=$3
 ), bounds AS (
   SELECT COALESCE($4::timestamptz, statement_timestamp()-interval '24 hours') AS since,
     COALESCE($5::timestamptz, statement_timestamp()) AS until
 ), scoped_jobs AS (
   SELECT j.*, e.observation_confidence FROM scope JOIN jobs AS j ON j.namespace_id=scope.id
   LEFT JOIN LATERAL (
     SELECT execution.observation_confidence FROM runs AS r
     JOIN executions AS execution ON execution.run_id=r.id WHERE r.job_id=j.id
     ORDER BY r.run_number DESC LIMIT 1
   ) AS e ON true
 )
 SELECT scope.id::text,scope.name,statement_timestamp(),bounds.since,bounds.until,
   (SELECT count(*)::text FROM scoped_jobs),
   (SELECT count(*)::text FROM scoped_jobs WHERE phase <> 'terminal'),
   (SELECT count(*)::text FROM scoped_jobs WHERE phase IN ('accepted','assigning','accepted_execution')),
   (SELECT count(*)::text FROM scoped_jobs WHERE phase <> 'terminal' AND observation_confidence IN ('stale','uncertain','lost')),
   (SELECT count(*)::text FROM scoped_jobs WHERE phase = 'terminal' AND completed_at IS NULL),
   COALESCE((SELECT jsonb_object_agg(phase,total) FROM (SELECT phase,count(*)::text AS total FROM scoped_jobs GROUP BY phase) AS counts),'{}'::jsonb),
   COALESCE((SELECT jsonb_object_agg(outcome,total) FROM (SELECT outcome,count(*)::text AS total FROM scoped_jobs WHERE phase='terminal' AND completed_at >= bounds.since AND completed_at < bounds.until GROUP BY outcome) AS counts),'{}'::jsonb)
 FROM scope CROSS JOIN bounds
 `, principal.Issuer, principal.Subject, namespace, from, before).Scan(&result.NamespaceID, &result.Namespace, &result.AsOf, &result.CompletedFrom, &result.CompletedBefore, &result.Total, &result.Active, &result.AwaitingExecution, &result.EvidenceAttention, &result.MissingCompletionTime, &phases, &outcomes)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NamespaceSummary{}, domain.ErrForbidden
	}
	if err != nil {
		return domain.NamespaceSummary{}, fmt.Errorf("query namespace summary: %w", err)
	}
	if err = json.Unmarshal(phases, &result.ByPhase); err != nil {
		return domain.NamespaceSummary{}, fmt.Errorf("decode phase counts: %w", err)
	}
	if err = json.Unmarshal(outcomes, &result.ByOutcome); err != nil {
		return domain.NamespaceSummary{}, fmt.Errorf("decode outcome counts: %w", err)
	}
	result.AsOf = result.AsOf.UTC()
	result.CompletedFrom = result.CompletedFrom.UTC()
	result.CompletedBefore = result.CompletedBefore.UTC()
	return result, nil
}
