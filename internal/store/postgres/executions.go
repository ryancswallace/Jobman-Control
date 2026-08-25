package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	jobmanprotocol "github.com/ryancswallace/jobman-control/contracts/jobman/v1alpha1"
	"github.com/ryancswallace/jobman-control/internal/domain"
)

// AcceptAssignment atomically binds an offered execution to its assigned
// agent. A byte-equivalent replay returns the already committed decision.
func (store *Store) AcceptAssignment(
	ctx context.Context,
	identity domain.AgentIdentity,
	acceptance domain.Acceptance,
) (domain.LaunchAuthorization, error) {
	result, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.LaunchAuthorization, error) {
		var state, executionID, agentID, generationID, effectiveDigest, requestDigest string
		var revision int64
		var acceptedAt *time.Time
		var runID, jobID string
		queryErr := tx.QueryRow(ctx, `
			SELECT a.state, a.execution_id::text, a.agent_id::text,
				e.target_generation_id::text, e.effective_spec_digest,
				COALESCE(a.acceptance_digest, ''), e.revision, a.accepted_at,
				e.run_id::text, r.job_id::text
			FROM assignments AS a
			JOIN executions AS e ON e.id = a.execution_id
			JOIN runs AS r ON r.id = e.run_id
			JOIN jobs AS j ON j.id = r.job_id
			WHERE a.id = $1 AND a.namespace_id = $2
			FOR UPDATE OF a, e, r, j
		`, acceptance.DeliveryID, identity.NamespaceID).Scan(
			&state, &executionID, &agentID, &generationID, &effectiveDigest,
			&requestDigest, &revision, &acceptedAt, &runID, &jobID,
		)
		if errors.Is(queryErr, pgx.ErrNoRows) {
			return domain.LaunchAuthorization{}, domain.ErrNotFound
		}
		if queryErr != nil {
			return domain.LaunchAuthorization{}, fmt.Errorf("lock assignment acceptance: %w", queryErr)
		}
		if executionID != acceptance.ExecutionID || agentID != identity.AgentID ||
			acceptance.AgentID != identity.AgentID || generationID != identity.TargetGenerationID ||
			acceptance.TargetGenerationID != generationID || acceptance.EffectiveExecutionDigest != effectiveDigest {
			return domain.LaunchAuthorization{}, domain.ErrForbidden
		}
		if state == "accepted" {
			if requestDigest != acceptance.RequestDigest || acceptedAt == nil {
				return domain.LaunchAuthorization{}, domain.ErrIdempotencyConflict
			}

			return launchAuthorization(
				acceptance.DeliveryID, executionID, agentID, generationID,
				effectiveDigest, revision, acceptedAt.UTC(), true,
			), nil
		}
		if state != "offered" {
			return domain.LaunchAuthorization{}, domain.ErrConflict
		}
		var desiredState string
		if queryErr = tx.QueryRow(ctx, `SELECT desired_state FROM jobs WHERE id = $1`, jobID).Scan(&desiredState); queryErr != nil {
			return domain.LaunchAuthorization{}, fmt.Errorf("read job desire for acceptance: %w", queryErr)
		}
		if desiredState != "run" {
			return domain.LaunchAuthorization{}, domain.ErrConflict
		}
		var committedAt time.Time
		if queryErr = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&committedAt); queryErr != nil {
			return domain.LaunchAuthorization{}, fmt.Errorf("read acceptance time: %w", queryErr)
		}
		if _, queryErr = tx.Exec(ctx, `
			UPDATE assignments
			SET state = 'accepted', accepted_at = $2,
				acceptance_digest = $3, acceptance_document = $4::jsonb
			WHERE id = $1 AND state = 'offered'
		`, acceptance.DeliveryID, committedAt, acceptance.RequestDigest,
			string(acceptance.RequestDocument)); queryErr != nil {
			return domain.LaunchAuthorization{}, fmt.Errorf("accept assignment: %w", queryErr)
		}
		revision++
		if _, queryErr = tx.Exec(ctx, `
			UPDATE executions
			SET phase = 'accepted', revision = $2, accepted_at = $3,
				updated_at = transaction_timestamp()
			WHERE id = $1 AND phase = 'planned'
		`, executionID, revision, committedAt); queryErr != nil {
			return domain.LaunchAuthorization{}, fmt.Errorf("advance accepted execution: %w", queryErr)
		}
		if _, queryErr = tx.Exec(ctx, `
			UPDATE runs SET phase = 'accepted', updated_at = transaction_timestamp()
			WHERE id = $1 AND phase = 'assigning'
		`, runID); queryErr != nil {
			return domain.LaunchAuthorization{}, fmt.Errorf("advance accepted work: %w", queryErr)
		}
		if _, queryErr = tx.Exec(ctx, `
			UPDATE jobs SET phase = 'accepted_execution', revision = revision + 1,
				updated_at = transaction_timestamp()
			WHERE id = $1 AND phase = 'assigning'
		`, jobID); queryErr != nil {
			return domain.LaunchAuthorization{}, fmt.Errorf("advance accepted job: %w", queryErr)
		}
		if _, queryErr = tx.Exec(ctx, `
			INSERT INTO audit_events (
				namespace_id, actor_kind, actor_agent_id, action,
				resource_type, resource_id, request_digest, details
			) VALUES ($1, 'agent', $2, 'execution.accepted', 'execution', $3,
				$4, jsonb_build_object('deliveryId', $5::text, 'revision', $6::bigint))
		`, identity.NamespaceID, identity.AgentID, executionID,
			acceptance.RequestDigest, acceptance.DeliveryID, revision); queryErr != nil {
			return domain.LaunchAuthorization{}, fmt.Errorf("audit execution acceptance: %w", queryErr)
		}

		return launchAuthorization(
			acceptance.DeliveryID, executionID, agentID, generationID,
			effectiveDigest, revision, committedAt.UTC(), false,
		), nil
	})
	if err != nil {
		return domain.LaunchAuthorization{}, fmt.Errorf("accept assignment: %w", err)
	}

	return result, nil
}

func launchAuthorization(
	authorizationID, executionID, agentID, generationID, digest string,
	revision int64,
	acceptedAt time.Time,
	replayed bool,
) domain.LaunchAuthorization {
	return domain.LaunchAuthorization{
		AuthorizationID: authorizationID, ExecutionID: executionID, AgentID: agentID,
		TargetGenerationID: generationID, EffectiveExecutionDigest: digest,
		Revision: revision, AcceptedAt: acceptedAt, Replayed: replayed,
	}
}

// RecordExecutionEvent appends one source-ordered event and advances its
// execution snapshot in the same transaction.
func (store *Store) RecordExecutionEvent(
	ctx context.Context,
	identity domain.AgentIdentity,
	event domain.ExecutionObservation,
) (bool, error) {
	replayed, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (bool, error) {
		var phase, agentID, priorDigest, runID, jobID, executionBackend, effectiveSpec string
		var logsRequired bool
		var lastSequence int64
		queryErr := tx.QueryRow(ctx, `
			SELECT e.phase, e.agent_id::text, e.last_event_sequence,
				COALESCE(existing.document_digest, ''), e.run_id::text, r.job_id::text,
				tg.log_store_name IS NOT NULL, tg.execution_backend, e.effective_spec::text
			FROM executions AS e
			JOIN runs AS r ON r.id = e.run_id
			JOIN target_generations AS tg ON tg.id = e.target_generation_id
			LEFT JOIN execution_events AS existing
				ON existing.event_id = $3 AND existing.execution_id = e.id
			WHERE e.id = $1 AND e.namespace_id = $2
			FOR UPDATE OF e, r
		`, event.ExecutionID, identity.NamespaceID, event.EventID).Scan(
			&phase, &agentID, &lastSequence, &priorDigest, &runID, &jobID,
			&logsRequired, &executionBackend, &effectiveSpec,
		)
		if errors.Is(queryErr, pgx.ErrNoRows) {
			return false, domain.ErrNotFound
		}
		if queryErr != nil {
			return false, fmt.Errorf("lock execution event stream: %w", queryErr)
		}
		if agentID != identity.AgentID || event.AgentID != identity.AgentID {
			return false, domain.ErrForbidden
		}
		if priorDigest != "" {
			if priorDigest != event.DocumentDigest {
				return false, domain.ErrIdempotencyConflict
			}

			return true, nil
		}
		if event.Sequence != lastSequence+1 {
			return false, domain.ErrConflict
		}
		if (executionBackend == "slurm") != schedulerEvent(event.Type) ||
			schedulerEvent(event.Type) && event.SchedulerBackend != "slurm" {
			return false, domain.ErrConflict
		}
		if !validEventTransition(phase, event.Type) {
			return false, domain.ErrConflict
		}
		if event.Type == "process.completed" || event.Type == "scheduler.completed" {
			if validationErr := validateArtifactPublication(effectiveSpec, event.Document); validationErr != nil {
				return false, domain.ErrConflict
			}
		}
		if (event.Type == "process.completed" || event.Type == "scheduler.completed") && logsRequired {
			var completeStreams int
			queryErr = tx.QueryRow(ctx, `
				SELECT count(*) FROM log_streams
				WHERE execution_id = $1 AND state = 'complete'
			`, event.ExecutionID).Scan(&completeStreams)
			if queryErr != nil {
				return false, fmt.Errorf("inspect completed execution logs: %w", queryErr)
			}
			if completeStreams != 2 {
				return false, domain.ErrConflict
			}
		}
		if _, queryErr = tx.Exec(ctx, `
			INSERT INTO execution_events (
				event_id, namespace_id, execution_id, agent_id, source_sequence,
				event_type, observed_at, document_digest, document
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb)
		`, event.EventID, identity.NamespaceID, event.ExecutionID, identity.AgentID,
			event.Sequence, event.Type, event.ObservedAt, event.DocumentDigest,
			string(event.Document)); queryErr != nil {
			return false, fmt.Errorf("insert execution event: %w", queryErr)
		}
		if event.Type == "process.completed" || event.Type == "scheduler.completed" {
			if _, queryErr = tx.Exec(ctx, `
				INSERT INTO execution_artifacts (
					namespace_id, execution_id, name, store_name, store_version,
					object_key, byte_length, checksum, published_at
				)
				SELECT $1, $2, artifact.name, artifact."storeName",
					artifact."storeVersion", artifact."objectKey",
					artifact."byteLength", artifact.checksum, $4
				FROM jsonb_to_recordset(COALESCE($3::jsonb #> '{spec,artifacts}', '[]'::jsonb)) AS artifact(
					name text, "storeName" text, "storeVersion" bigint,
					"objectKey" text, "byteLength" bigint, checksum text
				)
			`, identity.NamespaceID, event.ExecutionID, string(event.Document), event.ObservedAt); queryErr != nil {
				return false, fmt.Errorf("record published artifacts: %w", queryErr)
			}
		}
		switch event.Type {
		case "process.started":
			if _, queryErr = tx.Exec(ctx, `
				UPDATE executions SET phase = 'running', native_id = $2,
					last_event_sequence = $3, revision = revision + 1,
					updated_at = transaction_timestamp()
				WHERE id = $1
			`, event.ExecutionID, event.NativeID, event.Sequence); queryErr != nil {
				return false, fmt.Errorf("record process start: %w", queryErr)
			}
			if _, queryErr = tx.Exec(ctx, `
				UPDATE runs SET phase = 'running', updated_at = transaction_timestamp()
				WHERE id = $1
			`, runID); queryErr != nil {
				return false, fmt.Errorf("record running run: %w", queryErr)
			}
			if _, queryErr = tx.Exec(ctx, `
				UPDATE jobs SET phase = 'running', revision = revision + 1,
					updated_at = transaction_timestamp()
				WHERE id = $1
			`, jobID); queryErr != nil {
				return false, fmt.Errorf("record running job: %w", queryErr)
			}
		case "process.completed":
			if _, queryErr = tx.Exec(ctx, `
				UPDATE executions SET phase = 'terminal', outcome = $2,
					process_result = $3::jsonb #> '{spec,result}',
					last_event_sequence = $4, revision = revision + 1,
					updated_at = transaction_timestamp()
				WHERE id = $1
			`, event.ExecutionID, event.Outcome, string(event.Document), event.Sequence); queryErr != nil {
				return false, fmt.Errorf("record process completion: %w", queryErr)
			}
			if _, queryErr = tx.Exec(ctx, `
				UPDATE runs SET phase = 'terminal', outcome = $2,
					updated_at = transaction_timestamp() WHERE id = $1
			`, runID, event.Outcome); queryErr != nil {
				return false, fmt.Errorf("record terminal run: %w", queryErr)
			}
			if _, queryErr = tx.Exec(ctx, `
				UPDATE jobs SET phase = 'terminal', outcome = $2,
					revision = revision + 1, updated_at = transaction_timestamp()
				WHERE id = $1
			`, jobID, event.Outcome); queryErr != nil {
				return false, fmt.Errorf("record terminal job: %w", queryErr)
			}
		case "scheduler.uncertain", "scheduler.submitted", "scheduler.observed":
			if _, queryErr = tx.Exec(ctx, `
				UPDATE executions SET
					phase = CASE WHEN $3 = 'running' THEN 'running' ELSE phase END,
					native_id = CASE WHEN $2 = '' THEN native_id ELSE $2 END,
					native_backend = $4, native_state = $3,
					native_reason = NULLIF($5, ''), native_cluster = NULLIF($6, ''),
					native_observed_at = $7, last_event_sequence = $8,
					revision = revision + 1, updated_at = transaction_timestamp()
				WHERE id = $1
			`, event.ExecutionID, event.NativeID, event.SchedulerState,
				event.SchedulerBackend, event.SchedulerReason, event.SchedulerCluster,
				event.ObservedAt, event.Sequence); queryErr != nil {
				return false, fmt.Errorf("record scheduler observation: %w", queryErr)
			}
			if event.SchedulerState == "running" {
				if _, queryErr = tx.Exec(ctx, `
					UPDATE runs SET phase = 'running', updated_at = transaction_timestamp()
					WHERE id = $1 AND phase = 'accepted'
				`, runID); queryErr != nil {
					return false, fmt.Errorf("record scheduler running run: %w", queryErr)
				}
				if _, queryErr = tx.Exec(ctx, `
					UPDATE jobs SET phase = 'running', revision = revision + 1,
						updated_at = transaction_timestamp()
					WHERE id = $1
				`, jobID); queryErr != nil {
					return false, fmt.Errorf("record scheduler running job: %w", queryErr)
				}
			} else if _, queryErr = tx.Exec(ctx, `
				UPDATE jobs SET revision = revision + 1, updated_at = transaction_timestamp()
				WHERE id = $1
			`, jobID); queryErr != nil {
				return false, fmt.Errorf("record scheduler job observation: %w", queryErr)
			}
		case "scheduler.completed":
			if _, queryErr = tx.Exec(ctx, `
				UPDATE executions SET phase = 'terminal', outcome = $2,
					process_result = $3::jsonb #> '{spec,result}', native_id = $4,
					native_backend = $5, native_state = $6,
					native_reason = NULLIF($7, ''), native_cluster = NULLIF($8, ''),
					native_observed_at = $9, last_event_sequence = $10,
					revision = revision + 1, updated_at = transaction_timestamp()
				WHERE id = $1
			`, event.ExecutionID, event.Outcome, string(event.Document), event.NativeID,
				event.SchedulerBackend, event.SchedulerState, event.SchedulerReason,
				event.SchedulerCluster, event.ObservedAt, event.Sequence); queryErr != nil {
				return false, fmt.Errorf("record scheduler completion: %w", queryErr)
			}
			if _, queryErr = tx.Exec(ctx, `
				UPDATE runs SET phase = 'terminal', outcome = $2,
					updated_at = transaction_timestamp() WHERE id = $1
			`, runID, event.Outcome); queryErr != nil {
				return false, fmt.Errorf("record scheduler terminal run: %w", queryErr)
			}
			if _, queryErr = tx.Exec(ctx, `
				UPDATE jobs SET phase = 'terminal', outcome = $2,
					revision = revision + 1, updated_at = transaction_timestamp()
				WHERE id = $1
			`, jobID, event.Outcome); queryErr != nil {
				return false, fmt.Errorf("record scheduler terminal job: %w", queryErr)
			}
		}
		if event.Type == "process.completed" || event.Type == "scheduler.completed" {
			if collectionErr := store.applyCollectionCompletion(
				ctx, tx, identity.NamespaceID, jobID, event.Outcome,
			); collectionErr != nil {
				return false, collectionErr
			}
			if graphErr := store.applyGraphCompletion(
				ctx, tx, identity.NamespaceID, jobID,
			); graphErr != nil {
				return false, graphErr
			}
		}
		confidence := "current"
		if event.Type == "scheduler.uncertain" {
			confidence = "uncertain"
		} else if event.Outcome == "lost" || event.SchedulerState == "lost" {
			confidence = "lost"
		}
		if _, queryErr = tx.Exec(ctx, `
			UPDATE executions
			SET observation_confidence = $2,
				confidence_updated_at = transaction_timestamp()
			WHERE id = $1
		`, event.ExecutionID, confidence); queryErr != nil {
			return false, fmt.Errorf("update observation confidence: %w", queryErr)
		}

		return false, nil
	})
	if err != nil {
		return false, fmt.Errorf("record execution event: %w", err)
	}

	return replayed, nil
}

func validateArtifactPublication(effectiveDocument string, eventDocument json.RawMessage) error {
	var effective jobmanprotocol.EffectiveExecution
	if err := json.Unmarshal([]byte(effectiveDocument), &effective); err != nil {
		return err
	}
	var event jobmanprotocol.ExecutionEvent
	if err := json.Unmarshal(eventDocument, &event); err != nil {
		return err
	}
	outputs := make(map[string]jobmanprotocol.OutputArtifact)
	if effective.Spec.Workload.Document.Spec.Artifacts != nil {
		for _, output := range effective.Spec.Workload.Document.Spec.Artifacts.Outputs {
			outputs[output.Name] = output
		}
	}
	published := make(map[string]struct{}, len(event.Spec.Artifacts))
	bindings := make(map[string]int64, len(effective.Spec.ArtifactStores))
	for _, binding := range effective.Spec.ArtifactStores {
		bindings[binding.Name] = binding.Version
	}
	for _, artifact := range event.Spec.Artifacts {
		output, exists := outputs[artifact.Name]
		if !exists {
			return errors.New("published artifact was not declared")
		}
		destination, err := url.Parse(output.Destination)
		if err != nil || destination.Host != artifact.StoreName ||
			strings.TrimPrefix(destination.Path, "/") != artifact.ObjectKey ||
			bindings[artifact.StoreName] != artifact.StoreVersion {
			return errors.New("published artifact does not match resolved destination")
		}
		published[artifact.Name] = struct{}{}
	}
	for name, output := range outputs {
		if _, exists := published[name]; output.Required && !exists &&
			event.Spec.Result != nil && event.Spec.Result.Outcome == "success" {
			return errors.New("required output was not published")
		}
	}

	return nil
}

func validEventTransition(phase, eventType string) bool {
	switch eventType {
	case "process.started":
		return phase == "accepted"
	case "process.completed", "scheduler.completed":
		return phase == "accepted" || phase == "running"
	case "scheduler.uncertain", "scheduler.submitted":
		return phase == "accepted"
	case "scheduler.observed":
		return phase == "accepted" || phase == "running"
	default:
		return false
	}
}

func schedulerEvent(eventType string) bool {
	switch eventType {
	case "scheduler.uncertain", "scheduler.submitted", "scheduler.observed", "scheduler.completed":
		return true
	default:
		return false
	}
}

// ListDesiredActions redelivers pending control intent.
func (store *Store) ListDesiredActions(
	ctx context.Context,
	identity domain.AgentIdentity,
	limit int,
) ([]domain.DesiredAction, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("action limit must be between 1 and 100")
	}
	rows, err := store.pool.Query(ctx, `
		WITH selected AS (
			SELECT id FROM desired_actions
			WHERE agent_id = $1 AND namespace_id = $2 AND state = 'pending'
			ORDER BY created_at, id LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		UPDATE desired_actions AS action
		SET delivery_count = delivery_count + 1,
			last_delivered_at = transaction_timestamp()
		FROM selected
		WHERE action.id = selected.id
		RETURNING action.id::text, action.execution_id::text,
			action.agent_id::text, action.revision, action.document::text,
			action.created_at
	`, identity.AgentID, identity.NamespaceID, limit)
	if err != nil {
		return nil, fmt.Errorf("list desired actions: %w", err)
	}
	defer rows.Close()
	actions := make([]domain.DesiredAction, 0)
	for rows.Next() {
		var action domain.DesiredAction
		var document string
		if err = rows.Scan(
			&action.ActionID, &action.ExecutionID, &action.AgentID,
			&action.Revision, &document, &action.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan desired action: %w", err)
		}
		action.Document = json.RawMessage(document)
		action.CreatedAt = action.CreatedAt.UTC()
		actions = append(actions, action)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate desired actions: %w", err)
	}

	return actions, nil
}

// AcknowledgeDesiredAction records durable receipt without claiming native
// completion.
func (store *Store) AcknowledgeDesiredAction(
	ctx context.Context,
	identity domain.AgentIdentity,
	acknowledgement domain.ActionAcknowledgement,
) (bool, error) {
	commandTag, err := store.pool.Exec(ctx, `
		UPDATE desired_actions
		SET state = 'acknowledged', acknowledged_at = $5
		WHERE id = $1 AND namespace_id = $2 AND execution_id = $3
			AND agent_id = $4 AND revision = $6 AND state = 'pending'
	`, acknowledgement.ActionID, identity.NamespaceID, acknowledgement.ExecutionID,
		identity.AgentID, acknowledgement.ObservedAt, acknowledgement.Revision)
	if err != nil {
		return false, fmt.Errorf("acknowledge desired action: %w", err)
	}
	if commandTag.RowsAffected() == 1 {
		return false, nil
	}
	var state string
	err = store.pool.QueryRow(ctx, `
		SELECT state FROM desired_actions
		WHERE id = $1 AND namespace_id = $2 AND execution_id = $3
			AND agent_id = $4 AND revision = $5
	`, acknowledgement.ActionID, identity.NamespaceID, acknowledgement.ExecutionID,
		identity.AgentID, acknowledgement.Revision).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, domain.ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("read desired action acknowledgement: %w", err)
	}

	return state == "acknowledged", nil
}

// CancelJob records desired cancellation. Unaccepted work is terminalized
// without launch; accepted work receives one durable cancel action.
func (store *Store) CancelJob(
	ctx context.Context,
	principal domain.Principal,
	namespace string,
	jobID string,
	idempotencyKey string,
	requestDigest string,
) (domain.Job, error) {
	actionID, err := store.newID()
	if err != nil {
		return domain.Job{}, err
	}
	result, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.Job, error) {
		authorization, authErr := authorizeNamespace(ctx, tx, principal, namespace)
		if authErr != nil {
			return domain.Job{}, authErr
		}
		var ownerID, phase, desiredState string
		queryErr := tx.QueryRow(ctx, `
			SELECT owner_principal_id::text, phase, desired_state
			FROM jobs WHERE id = $1 AND namespace_id = $2 FOR UPDATE
		`, jobID, authorization.namespaceID).Scan(&ownerID, &phase, &desiredState)
		if errors.Is(queryErr, pgx.ErrNoRows) {
			return domain.Job{}, domain.ErrNotFound
		}
		if queryErr != nil {
			return domain.Job{}, fmt.Errorf("lock job cancellation: %w", queryErr)
		}
		if ownerID != authorization.principalID &&
			!slices.Contains([]string{domain.RoleOperator, domain.RoleNamespaceAdmin}, authorization.role) {
			return domain.Job{}, domain.ErrForbidden
		}
		replayedJobID, replayed, replayErr := reserveIdempotency(
			ctx, tx, authorization, "jobs.cancel", "job", idempotencyKey,
			requestDigest, 200,
		)
		if replayErr != nil {
			return domain.Job{}, replayErr
		}
		if replayed {
			if replayedJobID != jobID {
				return domain.Job{}, domain.ErrIdempotencyConflict
			}
			return getJobByID(ctx, tx, authorization.namespaceID, jobID)
		}
		if phase == "terminal" || desiredState == "cancel" {
			if completeErr := completeIdempotency(
				ctx, tx, authorization, "jobs.cancel", idempotencyKey, jobID, 200,
			); completeErr != nil {
				return domain.Job{}, completeErr
			}
			return getJobByID(ctx, tx, authorization.namespaceID, jobID)
		}
		var executionID, runID, agentID, executionPhase string
		queryErr = tx.QueryRow(ctx, `
			SELECT e.id::text, e.run_id::text, e.agent_id::text, e.phase
			FROM executions AS e JOIN runs AS r ON r.id = e.run_id
			WHERE r.job_id = $1
		`, jobID).Scan(&executionID, &runID, &agentID, &executionPhase)
		switch {
		case errors.Is(queryErr, pgx.ErrNoRows):
			_, queryErr = tx.Exec(ctx, `
				UPDATE jobs SET phase = 'terminal', desired_state = 'cancel',
					outcome = 'cancelled', revision = revision + 1,
					updated_at = transaction_timestamp() WHERE id = $1
			`, jobID)
		case queryErr == nil && executionPhase == "planned":
			_, queryErr = tx.Exec(ctx, `
				UPDATE assignments SET state = 'withdrawn' WHERE execution_id = $1 AND state = 'offered'
			`, executionID)
			if queryErr == nil {
				//nolint:misspell // Process outcome and failure code are frozen v1alpha1 wire values.
				_, queryErr = tx.Exec(ctx, `
				UPDATE executions SET phase = 'terminal', outcome = 'cancelled',
					process_result = '{"outcome":"cancelled","failureCode":"cancelled_before_launch"}'::jsonb,
					revision = revision + 1, updated_at = transaction_timestamp() WHERE id = $1
				`, executionID)
			}
			if queryErr == nil {
				_, queryErr = tx.Exec(ctx, `
				UPDATE runs SET phase = 'terminal', desired_state = 'cancel', outcome = 'cancelled',
					updated_at = transaction_timestamp() WHERE id = $1
				`, runID)
			}
			if queryErr == nil {
				_, queryErr = tx.Exec(ctx, `
				UPDATE jobs SET phase = 'terminal', desired_state = 'cancel', outcome = 'cancelled',
					revision = revision + 1, updated_at = transaction_timestamp() WHERE id = $1
				`, jobID)
			}
		case queryErr == nil:
			var requestedAt time.Time
			if queryErr = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&requestedAt); queryErr == nil {
				action := jobmanprotocol.DesiredAction{
					APIVersion: jobmanprotocol.V1Alpha1, Kind: jobmanprotocol.DesiredActionKind,
					Metadata: jobmanprotocol.DesiredActionMetadata{
						ActionID: actionID, ExecutionID: executionID, AgentID: agentID,
						Revision: 1, RequestedAt: requestedAt.UTC(),
					},
					Spec: jobmanprotocol.DesiredActionSpec{Type: "cancel"},
				}
				encoded, encodeErr := json.Marshal(action)
				if encodeErr != nil {
					return domain.Job{}, fmt.Errorf("encode desired action: %w", encodeErr)
				}
				_, queryErr = tx.Exec(ctx, `
					INSERT INTO desired_actions (
						id, namespace_id, execution_id, agent_id, action_type, document
					) VALUES ($1, $2, $3, $4, 'cancel', $5::jsonb)
					ON CONFLICT (execution_id, action_type) DO NOTHING
				`, actionID, authorization.namespaceID, executionID, agentID, string(encoded))
				if queryErr == nil {
					_, queryErr = tx.Exec(ctx, `
					UPDATE runs SET desired_state = 'cancel', updated_at = transaction_timestamp()
					WHERE id = $1
					`, runID)
				}
				if queryErr == nil {
					_, queryErr = tx.Exec(ctx, `
					UPDATE jobs SET desired_state = 'cancel', revision = revision + 1,
						updated_at = transaction_timestamp() WHERE id = $1
					`, jobID)
				}
			}
		}
		if queryErr != nil {
			return domain.Job{}, fmt.Errorf("apply job cancellation: %w", queryErr)
		}
		if completeErr := completeIdempotency(
			ctx, tx, authorization, "jobs.cancel", idempotencyKey, jobID, 200,
		); completeErr != nil {
			return domain.Job{}, completeErr
		}
		job, getErr := getJobByID(ctx, tx, authorization.namespaceID, jobID)
		if getErr != nil {
			return domain.Job{}, getErr
		}
		if job.Phase == "terminal" {
			if graphErr := store.applyGraphCompletion(ctx, tx, authorization.namespaceID, jobID); graphErr != nil {
				return domain.Job{}, graphErr
			}
		}

		return job, nil
	})
	if err != nil {
		return domain.Job{}, fmt.Errorf("cancel job: %w", err)
	}

	return result, nil
}
