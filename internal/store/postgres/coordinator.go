package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	jobmanprotocol "github.com/ryancswallace/jobman-control/contracts/jobman/v1alpha1"
)

// ReconcileAssignments materializes up to limit inert assignments. Competing
// coordinators safely skip locked jobs; no external I/O occurs in a claim
// transaction.
func (store *Store) ReconcileAssignments(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, errors.New("coordinator assignment limit must be between 1 and 1000")
	}
	created := 0
	for created < limit {
		identifiers := make([]string, 4)
		for index := range identifiers {
			identifier, err := store.newID()
			if err != nil {
				return created, err
			}
			identifiers[index] = identifier
		}
		assigned, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (bool, error) {
			return reconcileOneAssignment(ctx, tx, identifiers)
		})
		if err != nil {
			return created, fmt.Errorf("reconcile assignment: %w", err)
		}
		if !assigned {
			break
		}
		created++
	}

	return created, nil
}

type assignmentCandidate struct {
	jobID               string
	namespaceID         string
	namespace           string
	targetID            string
	targetGenerationID  string
	targetName          string
	partition           string
	executionBackend    string
	agentID             string
	workloadDigest      string
	workloadDocument    string
	artifactStores      string
	collectionID        string
	collectionIndex     int
	collectionCount     int
	collectionArrayMode string
	collectionMaxActive int
}

func reconcileOneAssignment(
	ctx context.Context,
	tx pgx.Tx,
	identifiers []string,
) (bool, error) {
	candidate, err := claimAssignmentCandidate(ctx, tx)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	runID, executionID, deliveryID, outboxID := identifiers[0], identifiers[1], identifiers[2], identifiers[3]
	document, digest, err := buildAssignmentDocument(
		candidate, runID, executionID, deliveryID,
	)
	if err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `
		INSERT INTO runs (
			id, namespace_id, job_id, run_number, phase, desired_state
		) VALUES ($1, $2, $3, 1, 'assigning', 'run')
	`, runID, candidate.namespaceID, candidate.jobID); err != nil {
		return false, fmt.Errorf("insert run: %w", err)
	}
	effectiveEncoded, err := json.Marshal(document.Document.Spec.EffectiveExecution)
	if err != nil {
		return false, fmt.Errorf("encode effective execution for persistence: %w", err)
	}
	if _, err = tx.Exec(ctx, `
		INSERT INTO executions (
			id, namespace_id, run_id, target_id, target_generation_id,
			agent_id, phase, effective_spec_digest, effective_spec
		) VALUES ($1, $2, $3, $4, $5, $6, 'planned', $7, $8::jsonb)
	`, executionID, candidate.namespaceID, runID, candidate.targetID,
		candidate.targetGenerationID, candidate.agentID, digest,
		string(effectiveEncoded)); err != nil {
		return false, fmt.Errorf("insert execution: %w", err)
	}
	if _, err = tx.Exec(ctx, `
		INSERT INTO assignments (
			id, namespace_id, execution_id, agent_id, state, document
		) VALUES ($1, $2, $3, $4, 'offered', $5::jsonb)
	`, deliveryID, candidate.namespaceID, executionID, candidate.agentID,
		string(document.CanonicalJSON)); err != nil {
		return false, fmt.Errorf("insert assignment: %w", err)
	}
	if commandTag, updateErr := tx.Exec(ctx, `
		UPDATE jobs
		SET phase = 'assigning', revision = revision + 1,
			updated_at = transaction_timestamp()
		WHERE id = $1 AND phase = 'accepted' AND desired_state = 'run'
	`, candidate.jobID); updateErr != nil || commandTag.RowsAffected() != 1 {
		if updateErr != nil {
			return false, fmt.Errorf("advance assigned job: %w", updateErr)
		}
		return false, errors.New("assignment candidate transition was lost")
	}
	if _, err = tx.Exec(ctx, `
		UPDATE namespace_policies SET last_dispatched_at = transaction_timestamp(),
			updated_at = transaction_timestamp() WHERE namespace_id = $1
	`, candidate.namespaceID); err != nil {
		return false, fmt.Errorf("advance namespace fairness cursor: %w", err)
	}
	if _, err = tx.Exec(ctx, `
		INSERT INTO audit_events (
			namespace_id, actor_kind, action, resource_type,
			resource_id, details
		) VALUES ($1, 'system', 'assignment.offered', 'execution', $2,
			jsonb_build_object(
				'agentId', $3::text,
				'deliveryId', $4::text,
				'effectiveExecutionDigest', $5::text
			))
	`, candidate.namespaceID, executionID, candidate.agentID, deliveryID, digest); err != nil {
		return false, fmt.Errorf("audit assignment: %w", err)
	}
	payload, err := json.Marshal(map[string]string{
		"apiVersion":               "jobman.control/v1alpha1",
		"kind":                     "AssignmentOffered",
		"executionId":              executionID,
		"deliveryId":               deliveryID,
		"agentId":                  candidate.agentID,
		"effectiveExecutionDigest": digest,
	})
	if err != nil {
		return false, fmt.Errorf("encode assignment outbox payload: %w", err)
	}
	if _, err = tx.Exec(ctx, `
		INSERT INTO outbox (
			id, namespace_id, topic, aggregate_type, aggregate_id, payload
		) VALUES ($1, $2, 'assignment.offered', 'execution', $3, $4::jsonb)
	`, outboxID, candidate.namespaceID, executionID, string(payload)); err != nil {
		return false, fmt.Errorf("insert assignment outbox event: %w", err)
	}

	return true, nil
}

func claimAssignmentCandidate(ctx context.Context, tx pgx.Tx) (assignmentCandidate, error) {
	var candidate assignmentCandidate
	err := tx.QueryRow(ctx, `
		SELECT j.id::text, j.namespace_id::text, n.name, j.target_id::text,
			j.target_generation_id::text, j.placement_target,
			COALESCE(j.placement_partition, ''), tg.execution_backend,
			a.id::text, j.workload_digest, w.document::text,
			j.resolved_artifact_stores::text,
			COALESCE(j.collection_id::text, ''), COALESCE(j.collection_index, -1),
			COALESCE(collection_limit.array_mode, ''), COALESCE(collection_limit.max_active, 0),
			CASE WHEN j.collection_id IS NULL THEN 0 ELSE (
				SELECT count(*) FROM jobs AS collection_child
				WHERE collection_child.collection_id = j.collection_id
			) END
		FROM jobs AS j
		JOIN namespaces AS n ON n.id = j.namespace_id
		JOIN namespace_policies AS namespace_policy ON namespace_policy.namespace_id = n.id
		CROSS JOIN service_recovery_state AS recovery
		JOIN targets AS t ON t.id = j.target_id
			AND t.state = 'active'
		JOIN target_generations AS tg ON tg.id = j.target_generation_id
		LEFT JOIN LATERAL (
			SELECT candidate_collection.id, candidate_collection.max_active,
				candidate_collection.array_mode
			FROM collections AS candidate_collection
			WHERE candidate_collection.id = j.collection_id
			FOR UPDATE SKIP LOCKED
		) AS collection_limit ON true
		LEFT JOIN LATERAL (
			SELECT candidate_graph.id, candidate_graph.max_active
			FROM graphs AS candidate_graph
			WHERE candidate_graph.id = j.graph_id
			FOR UPDATE SKIP LOCKED
		) AS graph_limit ON true
		JOIN workload_revisions AS w
			ON w.namespace_id = j.namespace_id AND w.digest = j.workload_digest
		JOIN LATERAL (
			SELECT candidate_agent.id
			FROM agents AS candidate_agent
			WHERE candidate_agent.namespace_id = j.namespace_id
				AND candidate_agent.target_id = j.target_id
				AND candidate_agent.target_generation_id = j.target_generation_id
				AND candidate_agent.principal_id = j.owner_principal_id
				AND candidate_agent.status = 'active'
				AND candidate_agent.accepting_assignments
				AND candidate_agent.last_capability_at > transaction_timestamp() - interval '10 minutes'
				AND tg.execution_backend = ANY(candidate_agent.execution_backends)
				AND (w.document #>> '{spec,runtime,kind}') = ANY(candidate_agent.runtimes)
				AND (
					jsonb_array_length(COALESCE(
						w.document #> '{spec,requirements,operatingSystems}', '[]'::jsonb
					)) = 0
					OR candidate_agent.operating_system IN (
						SELECT jsonb_array_elements_text(
							w.document #> '{spec,requirements,operatingSystems}'
						)
					)
				)
				AND (
					jsonb_array_length(COALESCE(
						w.document #> '{spec,requirements,architectures}', '[]'::jsonb
					)) = 0
					OR candidate_agent.architecture IN (
						SELECT jsonb_array_elements_text(
							w.document #> '{spec,requirements,architectures}'
						)
					)
				)
				AND NOT EXISTS (
					SELECT 1
					FROM jsonb_array_elements_text(
						COALESCE(w.document #> '{spec,requirements,capabilities}', '[]'::jsonb)
					) AS required(capability)
					WHERE NOT required.capability = ANY(candidate_agent.capabilities)
				)
				AND EXISTS (
					SELECT 1 FROM agent_sessions AS session
					WHERE session.agent_id = candidate_agent.id
						AND session.revoked_at IS NULL
						AND session.expires_at > transaction_timestamp()
				)
				AND (
					collection_limit.array_mode IS DISTINCT FROM 'slurm-array'
					OR candidate_agent.id = COALESCE((
						SELECT array_execution.agent_id
						FROM executions AS array_execution
						JOIN runs AS array_run ON array_run.id = array_execution.run_id
						JOIN jobs AS array_job ON array_job.id = array_run.job_id
						WHERE array_job.collection_id = j.collection_id
						ORDER BY array_execution.created_at, array_execution.id
						LIMIT 1
					), candidate_agent.id)
				)
			ORDER BY candidate_agent.created_at, candidate_agent.id
			LIMIT 1
		) AS a ON true
		WHERE NOT recovery.reconciliation_hold
			AND j.phase = 'accepted' AND j.desired_state = 'run'
			AND (
				SELECT count(*) FROM jobs AS active_namespace_job
				WHERE active_namespace_job.namespace_id = j.namespace_id
					AND active_namespace_job.phase NOT IN ('accepted', 'terminal')
			) < namespace_policy.max_active_jobs
			AND (
				j.collection_id IS NULL
				OR (
					collection_limit.id IS NOT NULL
					AND (
						collection_limit.array_mode = 'slurm-array'
						OR (
							SELECT count(*) FROM jobs AS active_child
							WHERE active_child.collection_id = j.collection_id
								AND active_child.phase NOT IN ('accepted', 'terminal')
							) < collection_limit.max_active
					)
				)
			)
			AND (
				j.graph_id IS NULL
				OR (
					graph_limit.id IS NOT NULL
					AND (
						SELECT count(*) FROM jobs AS active_graph_node
						WHERE active_graph_node.graph_id = j.graph_id
							AND active_graph_node.phase NOT IN ('accepted', 'terminal')
					) < graph_limit.max_active
					AND NOT EXISTS (
						SELECT 1 FROM graph_edges AS edge
						JOIN jobs AS upstream ON upstream.id = edge.upstream_job_id
						WHERE edge.downstream_job_id = j.id AND (
							upstream.phase != 'terminal' OR NOT (
								edge.predicate = 'any-terminal'
								OR edge.predicate = 'success' AND upstream.outcome = 'success'
								OR edge.predicate = 'failure' AND upstream.outcome IN ('failure', 'timed_out', 'aborted', 'lost')
								OR edge.predicate = 'outcomes' AND upstream.outcome = ANY(edge.outcomes)
							)
						)
					)
				)
			)
		ORDER BY namespace_policy.last_dispatched_at NULLS FIRST, j.created_at, j.id
		LIMIT 1
		FOR UPDATE OF j, namespace_policy SKIP LOCKED
	`).Scan(
		&candidate.jobID, &candidate.namespaceID, &candidate.namespace,
		&candidate.targetID, &candidate.targetGenerationID, &candidate.targetName,
		&candidate.partition, &candidate.executionBackend, &candidate.agentID,
		&candidate.workloadDigest, &candidate.workloadDocument, &candidate.artifactStores,
		&candidate.collectionID, &candidate.collectionIndex,
		&candidate.collectionArrayMode, &candidate.collectionMaxActive, &candidate.collectionCount,
	)
	if err != nil {
		return assignmentCandidate{}, err
	}

	return candidate, nil
}

func buildAssignmentDocument(
	candidate assignmentCandidate,
	runID string,
	executionID string,
	deliveryID string,
) (jobmanprotocol.SealedAgentAssignment, string, error) {
	var workload jobmanprotocol.Workload
	if err := json.Unmarshal([]byte(candidate.workloadDocument), &workload); err != nil {
		return jobmanprotocol.SealedAgentAssignment{}, "", fmt.Errorf("decode assignment workload: %w", err)
	}
	var artifactStores []jobmanprotocol.ArtifactStoreBinding
	if err := json.Unmarshal([]byte(candidate.artifactStores), &artifactStores); err != nil {
		return jobmanprotocol.SealedAgentAssignment{}, "", fmt.Errorf("decode resolved artifact stores: %w", err)
	}
	var slurmArray *jobmanprotocol.SlurmArrayBinding
	if candidate.collectionArrayMode == "slurm-array" {
		slurmArray = &jobmanprotocol.SlurmArrayBinding{
			CollectionID: candidate.collectionID, TaskIndex: candidate.collectionIndex,
			TaskCount: candidate.collectionCount, MaxParallel: candidate.collectionMaxActive,
		}
	}
	effective, err := jobmanprotocol.SealEffectiveExecution(jobmanprotocol.EffectiveExecution{
		APIVersion: jobmanprotocol.V1Alpha1,
		Kind:       jobmanprotocol.EffectiveExecutionKind,
		Metadata: jobmanprotocol.EffectiveExecutionMetadata{
			ExecutionID: executionID, RunID: runID,
			JobID: candidate.jobID, Namespace: candidate.namespace, SlurmArray: slurmArray,
		},
		Spec: jobmanprotocol.EffectiveExecutionSpec{
			Workload: jobmanprotocol.WorkloadBinding{
				Digest: candidate.workloadDigest, Document: workload,
			},
			Placement: jobmanprotocol.EffectivePlacement{
				TargetID:           candidate.targetID,
				TargetGenerationID: candidate.targetGenerationID,
				Target:             candidate.targetName, Partition: candidate.partition,
				ExecutionBackend: candidate.executionBackend,
			},
			ArtifactStores: artifactStores,
		},
	})
	if err != nil {
		return jobmanprotocol.SealedAgentAssignment{}, "", fmt.Errorf("seal effective execution: %w", err)
	}
	assignment, err := jobmanprotocol.SealAgentAssignment(jobmanprotocol.AgentAssignment{
		APIVersion: jobmanprotocol.V1Alpha1,
		Kind:       jobmanprotocol.AgentAssignmentKind,
		Metadata: jobmanprotocol.AgentAssignmentMetadata{
			DeliveryID: deliveryID, AgentID: candidate.agentID,
		},
		Spec: jobmanprotocol.AgentAssignmentSpec{
			EffectiveExecutionDigest: effective.Digest,
			EffectiveExecution:       effective.Document,
		},
	})
	if err != nil {
		return jobmanprotocol.SealedAgentAssignment{}, "", fmt.Errorf("seal agent assignment: %w", err)
	}

	return assignment, effective.Digest, nil
}
