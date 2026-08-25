package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

const submitJobOperation = "jobs.create"

type resolvedPlacement struct {
	targetID           string
	targetGenerationID string
	partition          string
	executionBackend   string
	artifactStores     []domain.ArtifactStoreSpec
}

// SubmitJob durably accepts one job and all of its submission evidence in a
// single transaction. The coordinator creates execution intent separately; no
// target I/O occurs in the submission transaction.
func (store *Store) SubmitJob(
	ctx context.Context,
	principal domain.Principal,
	idempotencyKey string,
	submission domain.JobSubmission,
) (domain.SubmitResult, error) {
	if !domain.ValidIdempotencyKey(idempotencyKey) {
		return domain.SubmitResult{}, errors.New("invalid idempotency key")
	}
	if err := validateSubmission(submission); err != nil {
		return domain.SubmitResult{}, err
	}
	jobID, err := store.newID()
	if err != nil {
		return domain.SubmitResult{}, err
	}
	outboxID, err := store.newID()
	if err != nil {
		return domain.SubmitResult{}, err
	}

	result, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.SubmitResult, error) {
		authorization, authorizeErr := authorizeSubmission(ctx, tx, principal, submission.Namespace)
		if authorizeErr != nil {
			return domain.SubmitResult{}, authorizeErr
		}
		replayedJobID, replayed, reserveErr := reserveIdempotency(
			ctx, tx, authorization, submitJobOperation, "job",
			idempotencyKey, submission.RequestDigest, 201,
		)
		if reserveErr != nil {
			return domain.SubmitResult{}, reserveErr
		}
		if replayed {
			job, lookupErr := getJobByID(ctx, tx, authorization.namespaceID, replayedJobID)
			if lookupErr != nil {
				return domain.SubmitResult{}, fmt.Errorf("load idempotent job: %w", lookupErr)
			}

			return domain.SubmitResult{Job: job, Replayed: true}, nil
		}
		if quotaErr := enforceNamespaceQuota(ctx, tx, authorization.namespaceID, 1, 0, 0); quotaErr != nil {
			return domain.SubmitResult{}, quotaErr
		}
		placement, resolveErr := resolveTarget(ctx, tx, authorization.namespaceID, submission)
		if resolveErr != nil {
			return domain.SubmitResult{}, resolveErr
		}

		if insertErr := insertWorkloadRevision(ctx, tx, authorization, submission); insertErr != nil {
			return domain.SubmitResult{}, insertErr
		}
		job, insertErr := insertJob(ctx, tx, authorization, jobID, submission, placement)
		if insertErr != nil {
			return domain.SubmitResult{}, insertErr
		}
		if completeErr := completeIdempotency(
			ctx, tx, authorization, submitJobOperation, idempotencyKey, job.ID, 201,
		); completeErr != nil {
			return domain.SubmitResult{}, completeErr
		}
		if evidenceErr := insertAcceptanceEvidence(
			ctx, tx, authorization, idempotencyKey, outboxID, job,
		); evidenceErr != nil {
			return domain.SubmitResult{}, evidenceErr
		}

		return domain.SubmitResult{Job: job}, nil
	})
	if err != nil {
		return domain.SubmitResult{}, fmt.Errorf("submit job: %w", err)
	}

	return result, nil
}

// GetJob reads one namespace-scoped job only when the principal is currently a
// member of that namespace. A single joined query prevents authorization races.
func (store *Store) GetJob(
	ctx context.Context,
	principal domain.Principal,
	namespace string,
	jobID string,
) (domain.Job, error) {
	job, err := scanJob(store.pool.QueryRow(ctx, `
		SELECT
			j.id::text, n.name, j.name, j.labels::text, j.phase,
			j.desired_state, COALESCE(j.outcome, ''), j.placement_target,
			COALESCE(j.placement_partition, ''), j.workload_digest,
			j.request_digest, j.revision, j.created_at, j.updated_at,
			COALESCE(j.target_id::text, ''), COALESCE(j.target_generation_id::text, ''),
			COALESCE(tg.execution_backend, ''),
			COALESCE(current_execution.native_id, ''),
			COALESCE(current_execution.native_backend, ''),
			COALESCE(current_execution.native_state, ''),
			COALESCE(current_execution.native_reason, ''),
			COALESCE(current_execution.native_cluster, ''),
			current_execution.native_observed_at,
			COALESCE(current_execution.observation_confidence, ''),
			current_execution.confidence_updated_at
		FROM jobs AS j
		JOIN namespaces AS n ON n.id = j.namespace_id
		JOIN memberships AS m ON m.namespace_id = n.id
		JOIN principals AS p ON p.id = m.principal_id
		LEFT JOIN target_generations AS tg ON tg.id = j.target_generation_id
		LEFT JOIN LATERAL (
			SELECT e.native_id, e.native_backend, e.native_state,
				e.native_reason, e.native_cluster, e.native_observed_at,
				e.observation_confidence, e.confidence_updated_at
			FROM runs AS current_run
			JOIN executions AS e ON e.run_id = current_run.id
			WHERE current_run.job_id = j.id
			ORDER BY current_run.run_number DESC LIMIT 1
		) AS current_execution ON true
		WHERE p.issuer = $1 AND p.subject = $2 AND n.name = $3 AND j.id = $4
	`, principal.Issuer, principal.Subject, namespace, jobID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Job{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Job{}, fmt.Errorf("get job: %w", err)
	}

	return job, nil
}

// ListJobs returns a bounded, keyset-paginated namespace history to any
// current namespace member. Ordering by creation time and UUID makes paging
// stable when newer jobs are submitted between requests.
func (store *Store) ListJobs(
	ctx context.Context,
	principal domain.Principal,
	namespace string,
	options domain.JobListOptions,
) (domain.JobPage, error) {
	if options.Limit < 1 || options.Limit > domain.MaximumJobListLimit {
		return domain.JobPage{}, errors.New("job list limit is out of range")
	}
	if options.Phase != "" && !domain.ValidJobPhase(options.Phase) {
		return domain.JobPage{}, errors.New("job list phase is invalid")
	}
	var beforeTime any
	var beforeID any
	if options.Before != nil {
		if options.Before.CreatedAt.IsZero() || !domain.IsID(options.Before.ID) {
			return domain.JobPage{}, errors.New("job list cursor is invalid")
		}
		beforeTime = options.Before.CreatedAt.UTC()
		beforeID = options.Before.ID
	}
	var authorized bool
	if err := store.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM principals AS p
			JOIN memberships AS m ON m.principal_id = p.id
			JOIN namespaces AS n ON n.id = m.namespace_id
			WHERE p.issuer = $1 AND p.subject = $2 AND n.name = $3
		)
	`, principal.Issuer, principal.Subject, namespace).Scan(&authorized); err != nil {
		return domain.JobPage{}, fmt.Errorf("authorize job list: %w", err)
	}
	if !authorized {
		return domain.JobPage{}, domain.ErrForbidden
	}
	rows, err := store.pool.Query(ctx, `
		SELECT
			j.id::text, n.name, j.name, j.labels::text, j.phase,
			j.desired_state, COALESCE(j.outcome, ''), j.placement_target,
			COALESCE(j.placement_partition, ''), j.workload_digest,
			j.request_digest, j.revision, j.created_at, j.updated_at,
			COALESCE(j.target_id::text, ''), COALESCE(j.target_generation_id::text, ''),
			COALESCE(tg.execution_backend, ''),
			COALESCE(current_execution.native_id, ''),
			COALESCE(current_execution.native_backend, ''),
			COALESCE(current_execution.native_state, ''),
			COALESCE(current_execution.native_reason, ''),
			COALESCE(current_execution.native_cluster, ''),
			current_execution.native_observed_at,
			COALESCE(current_execution.observation_confidence, ''),
			current_execution.confidence_updated_at
		FROM jobs AS j
		JOIN namespaces AS n ON n.id = j.namespace_id
		JOIN memberships AS m ON m.namespace_id = n.id
		JOIN principals AS p ON p.id = m.principal_id
		LEFT JOIN target_generations AS tg ON tg.id = j.target_generation_id
		LEFT JOIN LATERAL (
			SELECT e.native_id, e.native_backend, e.native_state,
				e.native_reason, e.native_cluster, e.native_observed_at,
				e.observation_confidence, e.confidence_updated_at
			FROM runs AS current_run
			JOIN executions AS e ON e.run_id = current_run.id
			WHERE current_run.job_id = j.id
			ORDER BY current_run.run_number DESC LIMIT 1
		) AS current_execution ON true
		WHERE p.issuer = $1 AND p.subject = $2 AND n.name = $3
			AND ($4::text = '' OR j.phase = $4)
			AND ($5::timestamptz IS NULL OR (j.created_at, j.id) < ($5, $6::uuid))
		ORDER BY j.created_at DESC, j.id DESC
		LIMIT $7
	`, principal.Issuer, principal.Subject, namespace, options.Phase, beforeTime, beforeID, options.Limit+1)
	if err != nil {
		return domain.JobPage{}, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()
	jobs := make([]domain.Job, 0, options.Limit+1)
	for rows.Next() {
		job, scanErr := scanJob(rows)
		if scanErr != nil {
			return domain.JobPage{}, fmt.Errorf("scan job: %w", scanErr)
		}
		jobs = append(jobs, job)
	}
	if err = rows.Err(); err != nil {
		return domain.JobPage{}, fmt.Errorf("iterate jobs: %w", err)
	}
	page := domain.JobPage{Jobs: jobs}
	if len(jobs) > options.Limit {
		last := jobs[options.Limit-1]
		page.Jobs = jobs[:options.Limit]
		page.NextCursor = &domain.JobCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}

	return page, nil
}

func validateSubmission(submission domain.JobSubmission) error {
	if submission.Namespace == "" || submission.Name == "" || submission.Target == "" {
		return errors.New("submission identity and target must not be empty")
	}
	if submission.WorkloadDigest == "" || submission.RequestDigest == "" {
		return errors.New("submission digests must not be empty")
	}
	if !json.Valid(submission.WorkloadDocument) || !json.Valid(submission.RequestDocument) {
		return errors.New("submission documents must be valid JSON")
	}

	return nil
}

func authorizeSubmission(
	ctx context.Context,
	tx pgx.Tx,
	principal domain.Principal,
	namespace string,
) (namespaceAuthorization, error) {
	return authorizeNamespace(
		ctx, tx, principal, namespace,
		domain.RoleSubmitter, domain.RoleOperator, domain.RoleNamespaceAdmin,
	)
}

func resolveTarget(
	ctx context.Context,
	tx pgx.Tx,
	namespaceID string,
	submission domain.JobSubmission,
) (resolvedPlacement, error) {
	var placement resolvedPlacement
	var state string
	var runtimes []string
	var operatingSystems []string
	var architectures []string
	var capabilities []string
	var artifactStoresJSON string
	err := tx.QueryRow(ctx, `
		SELECT t.id::text, tg.id::text, t.state, tg.execution_backend,
			tg.runtimes, tg.operating_systems, tg.architectures, tg.capabilities,
			tg.artifact_stores::text
		FROM targets AS t
		JOIN target_generations AS tg ON tg.id = t.current_generation_id
		WHERE t.namespace_id = $1 AND t.name = $2
	`, namespaceID, submission.Target).Scan(
		&placement.targetID, &placement.targetGenerationID, &state,
		&placement.executionBackend, &runtimes, &operatingSystems,
		&architectures, &capabilities, &artifactStoresJSON,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return resolvedPlacement{}, fmt.Errorf("%w: target is not registered", domain.ErrInvalidPlacement)
	}
	if err != nil {
		return resolvedPlacement{}, fmt.Errorf("resolve target: %w", err)
	}
	if state != "active" {
		return resolvedPlacement{}, fmt.Errorf("%w: target is not active", domain.ErrInvalidPlacement)
	}
	if !slices.Contains(runtimes, submission.RuntimeKind) ||
		!containsAll(operatingSystems, submission.OperatingSystems) ||
		!containsAll(architectures, submission.Architectures) ||
		!containsAll(capabilities, submission.Capabilities) {
		return resolvedPlacement{}, fmt.Errorf("%w: target capabilities do not satisfy the workload", domain.ErrInvalidPlacement)
	}
	var configuredStores []domain.ArtifactStoreSpec
	if err = json.Unmarshal([]byte(artifactStoresJSON), &configuredStores); err != nil {
		return resolvedPlacement{}, fmt.Errorf("resolve target artifact stores: %w", err)
	}
	placement.artifactStores, err = resolveArtifactStores(configuredStores, submission.ArtifactStores)
	if err != nil {
		return resolvedPlacement{}, err
	}
	if validationErr := validateExecutionFeatures(placement.executionBackend, submission.ExecutionFeatures); validationErr != nil {
		return resolvedPlacement{}, validationErr
	}
	placement.partition, err = resolvePartition(
		ctx, tx, placement.targetGenerationID, submission.Partition,
	)
	if err != nil {
		return resolvedPlacement{}, err
	}

	return placement, nil
}

//nolint:cyclop // Each rejected feature is a distinct user-facing placement constraint.
func validateExecutionFeatures(backend string, features domain.ExecutionFeatures) error {
	invalid := func(reason string) error {
		return fmt.Errorf("%w: %s", domain.ErrInvalidPlacement, reason)
	}
	if !features.DirectCommand {
		return invalid("this execution slice supports only direct executable commands")
	}
	if features.Extensions {
		return invalid("this execution slice does not yet support extensions")
	}
	if features.EnvironmentProfile || features.Secrets {
		return invalid("this execution slice does not yet support environment profiles or secrets")
	}
	if features.RetryMaxRuns != 1 {
		return invalid("this execution slice does not yet support retries")
	}
	switch backend {
	case "subprocess":
		if features.Resources {
			return invalid("subprocess targets do not yet support resource requests")
		}
	case "slurm":
		if features.TemporaryStorage {
			return invalid("Slurm targets do not yet support temporary storage requests")
		}
		if features.SchedulerEnvironmentOverride {
			return invalid("Slurm targets reserve scheduler-provided environment variables")
		}
	default:
		return invalid("target execution backend is not implemented")
	}

	return nil
}

func resolveArtifactStores(
	configured []domain.ArtifactStoreSpec,
	requested []string,
) ([]domain.ArtifactStoreSpec, error) {
	if len(requested) > 1 {
		return nil, fmt.Errorf("%w: this execution slice supports one artifact store per workload", domain.ErrInvalidPlacement)
	}
	result := make([]domain.ArtifactStoreSpec, 0, len(requested))
	for _, name := range requested {
		index := slices.IndexFunc(configured, func(store domain.ArtifactStoreSpec) bool {
			return store.Name == name
		})
		if index < 0 {
			return nil, fmt.Errorf("%w: artifact store %q is not approved by the target", domain.ErrInvalidPlacement, name)
		}
		result = append(result, configured[index])
	}

	return result, nil
}

func resolvePartition(ctx context.Context, tx pgx.Tx, generationID, requested string) (string, error) {
	if requested != "" {
		var partition string
		err := tx.QueryRow(ctx, `
			SELECT name FROM partitions
			WHERE target_generation_id = $1 AND name = $2 AND state = 'active'
		`, generationID, requested).Scan(&partition)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("%w: partition is not active on the selected target", domain.ErrInvalidPlacement)
		}
		if err != nil {
			return "", fmt.Errorf("resolve target partition: %w", err)
		}

		return partition, nil
	}
	var partition string
	err := tx.QueryRow(ctx, `
		SELECT name FROM partitions
		WHERE target_generation_id = $1 AND is_default AND state = 'active'
	`, generationID).Scan(&partition)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve default target partition: %w", err)
	}

	return partition, nil
}

func containsAll(available, required []string) bool {
	for _, value := range required {
		if !slices.Contains(available, value) {
			return false
		}
	}

	return true
}

func reserveIdempotency(
	ctx context.Context,
	tx pgx.Tx,
	authorization namespaceAuthorization,
	operation string,
	resourceTypeExpected string,
	key string,
	requestDigest string,
	responseStatusExpected int,
) (jobID string, replayed bool, resultErr error) {
	commandTag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_records (
			namespace_id, principal_id, operation, idempotency_key,
			request_digest, resource_type
		) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (namespace_id, principal_id, operation, idempotency_key)
		DO NOTHING
	`, authorization.namespaceID, authorization.principalID, operation, key, requestDigest, resourceTypeExpected)
	if err != nil {
		return "", false, fmt.Errorf("reserve idempotency key: %w", err)
	}
	if commandTag.RowsAffected() == 1 {
		return "", false, nil
	}

	var persistedDigest string
	var resourceType string
	var resourceID *string
	var responseStatus *int
	err = tx.QueryRow(ctx, `
		SELECT request_digest, resource_type, resource_id::text, response_status
		FROM idempotency_records
		WHERE namespace_id = $1 AND principal_id = $2
			AND operation = $3 AND idempotency_key = $4
	`, authorization.namespaceID, authorization.principalID, operation, key).Scan(
		&persistedDigest, &resourceType, &resourceID, &responseStatus,
	)
	if err != nil {
		return "", false, fmt.Errorf("read idempotency key: %w", err)
	}
	if persistedDigest != requestDigest {
		return "", false, domain.ErrIdempotencyConflict
	}
	if resourceType != resourceTypeExpected || resourceID == nil || responseStatus == nil ||
		*responseStatus != responseStatusExpected {
		return "", false, errors.New("idempotency record is incomplete")
	}

	return *resourceID, true, nil
}

func insertWorkloadRevision(
	ctx context.Context,
	tx pgx.Tx,
	authorization namespaceAuthorization,
	submission domain.JobSubmission,
) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO workload_revisions (
			namespace_id, digest, api_version, document, created_by
		) VALUES ($1, $2, 'jobman/v1alpha1', $3::jsonb, $4)
		ON CONFLICT (namespace_id, digest) DO NOTHING
	`, authorization.namespaceID, submission.WorkloadDigest,
		string(submission.WorkloadDocument), authorization.principalID,
	); err != nil {
		return fmt.Errorf("insert workload revision: %w", err)
	}

	var matches bool
	if err := tx.QueryRow(ctx, `
		SELECT document = $3::jsonb
		FROM workload_revisions
		WHERE namespace_id = $1 AND digest = $2
	`, authorization.namespaceID, submission.WorkloadDigest,
		string(submission.WorkloadDocument),
	).Scan(&matches); err != nil {
		return fmt.Errorf("verify workload revision: %w", err)
	}
	if !matches {
		return errors.New("workload digest collision")
	}

	return nil
}

func insertJob(
	ctx context.Context,
	tx pgx.Tx,
	authorization namespaceAuthorization,
	jobID string,
	submission domain.JobSubmission,
	placement resolvedPlacement,
) (domain.Job, error) {
	labels := submission.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	encodedLabels, err := json.Marshal(labels)
	if err != nil {
		return domain.Job{}, fmt.Errorf("encode labels: %w", err)
	}
	stores := placement.artifactStores
	if stores == nil {
		stores = []domain.ArtifactStoreSpec{}
	}
	encodedArtifactStores, err := json.Marshal(stores)
	if err != nil {
		return domain.Job{}, fmt.Errorf("encode resolved artifact stores: %w", err)
	}
	var partition any
	if placement.partition != "" {
		partition = placement.partition
	}

	job, err := scanJob(tx.QueryRow(ctx, `
		INSERT INTO jobs (
			id, namespace_id, owner_principal_id, name, labels, phase,
			desired_state, placement_target, placement_partition,
			workload_digest, request_digest, request_document,
			target_id, target_generation_id, resolved_artifact_stores
		) VALUES (
			$1, $2, $3, $4, $5::jsonb, $6, $7, $8, $9, $10, $11, $12::jsonb,
			$13, $14, $15::jsonb
		)
		RETURNING
			id::text, $16::text, name, labels::text, phase, desired_state,
			COALESCE(outcome, ''),
			placement_target, COALESCE(placement_partition, ''),
			workload_digest, request_digest, revision, created_at, updated_at,
			target_id::text, target_generation_id::text, $17::text,
			''::text, ''::text, ''::text, ''::text, ''::text, NULL::timestamptz,
			''::text, NULL::timestamptz
	`, jobID, authorization.namespaceID, authorization.principalID,
		submission.Name, string(encodedLabels), domain.JobPhaseAccepted,
		domain.JobDesiredStateRun, submission.Target, partition,
		submission.WorkloadDigest, submission.RequestDigest,
		string(submission.RequestDocument), placement.targetID,
		placement.targetGenerationID, string(encodedArtifactStores), submission.Namespace, placement.executionBackend,
	))
	if err != nil {
		return domain.Job{}, fmt.Errorf("insert job: %w", err)
	}

	return job, nil
}

func completeIdempotency(
	ctx context.Context,
	tx pgx.Tx,
	authorization namespaceAuthorization,
	operation string,
	key string,
	jobID string,
	responseStatus int,
) error {
	commandTag, err := tx.Exec(ctx, `
		UPDATE idempotency_records
		SET resource_id = $5, response_status = $6,
			completed_at = transaction_timestamp()
		WHERE namespace_id = $1 AND principal_id = $2
			AND operation = $3 AND idempotency_key = $4
			AND resource_id IS NULL
	`, authorization.namespaceID, authorization.principalID,
		operation, key, jobID, responseStatus,
	)
	if err != nil {
		return fmt.Errorf("complete idempotency key: %w", err)
	}
	if commandTag.RowsAffected() != 1 {
		return errors.New("idempotency reservation was lost")
	}

	return nil
}

func insertAcceptanceEvidence(
	ctx context.Context,
	tx pgx.Tx,
	authorization namespaceAuthorization,
	idempotencyKey string,
	outboxID string,
	job domain.Job,
) error {
	details, err := json.Marshal(map[string]string{
		"phase":           job.Phase,
		"placementTarget": job.Target,
		"workloadDigest":  job.WorkloadDigest,
	})
	if err != nil {
		return fmt.Errorf("encode audit details: %w", err)
	}
	if _, err = tx.Exec(ctx, `
		INSERT INTO audit_events (
			namespace_id, actor_principal_id, action, resource_type,
			resource_id, request_digest, idempotency_key, details
		) VALUES ($1, $2, 'job.accepted', 'job', $3, $4, $5, $6::jsonb)
	`, authorization.namespaceID, authorization.principalID, job.ID,
		job.RequestDigest, idempotencyKey, string(details),
	); err != nil {
		return fmt.Errorf("insert job audit event: %w", err)
	}

	payload, err := json.Marshal(map[string]any{
		"apiVersion":     "jobman.control/v1alpha1",
		"kind":           "JobAccepted",
		"jobId":          job.ID,
		"namespace":      job.Namespace,
		"revision":       job.Revision,
		"requestDigest":  job.RequestDigest,
		"workloadDigest": job.WorkloadDigest,
	})
	if err != nil {
		return fmt.Errorf("encode outbox payload: %w", err)
	}
	if _, err = tx.Exec(ctx, `
		INSERT INTO outbox (
			id, namespace_id, topic, aggregate_type, aggregate_id, payload
		) VALUES ($1, $2, 'job.accepted', 'job', $3, $4::jsonb)
	`, outboxID, authorization.namespaceID, job.ID, string(payload)); err != nil {
		return fmt.Errorf("insert job outbox event: %w", err)
	}

	return nil
}

func getJobByID(
	ctx context.Context,
	tx pgx.Tx,
	namespaceID string,
	jobID string,
) (domain.Job, error) {
	return scanJob(tx.QueryRow(ctx, `
		SELECT
			j.id::text, n.name, j.name, j.labels::text, j.phase,
			j.desired_state, COALESCE(j.outcome, ''), j.placement_target,
			COALESCE(j.placement_partition, ''), j.workload_digest,
			j.request_digest, j.revision, j.created_at, j.updated_at,
			COALESCE(j.target_id::text, ''), COALESCE(j.target_generation_id::text, ''),
			COALESCE(tg.execution_backend, ''),
			COALESCE(current_execution.native_id, ''),
			COALESCE(current_execution.native_backend, ''),
			COALESCE(current_execution.native_state, ''),
			COALESCE(current_execution.native_reason, ''),
			COALESCE(current_execution.native_cluster, ''),
			current_execution.native_observed_at,
			COALESCE(current_execution.observation_confidence, ''),
			current_execution.confidence_updated_at
		FROM jobs AS j
		JOIN namespaces AS n ON n.id = j.namespace_id
		LEFT JOIN target_generations AS tg ON tg.id = j.target_generation_id
		LEFT JOIN LATERAL (
			SELECT e.native_id, e.native_backend, e.native_state,
				e.native_reason, e.native_cluster, e.native_observed_at,
				e.observation_confidence, e.confidence_updated_at
			FROM runs AS current_run
			JOIN executions AS e ON e.run_id = current_run.id
			WHERE current_run.job_id = j.id
			ORDER BY current_run.run_number DESC LIMIT 1
		) AS current_execution ON true
		WHERE j.namespace_id = $1 AND j.id = $2
	`, namespaceID, jobID))
}

type rowScanner interface {
	Scan(...any) error
}

func scanJob(row rowScanner) (domain.Job, error) {
	var job domain.Job
	var labels string
	var schedulerBackend, schedulerState, schedulerReason, schedulerCluster string
	var schedulerObservedAt *time.Time
	var confidenceUpdatedAt *time.Time
	err := row.Scan(
		&job.ID, &job.Namespace, &job.Name, &labels, &job.Phase,
		&job.DesiredState, &job.Outcome, &job.Target, &job.Partition,
		&job.WorkloadDigest, &job.RequestDigest, &job.Revision,
		&job.CreatedAt, &job.UpdatedAt, &job.TargetID,
		&job.TargetGenerationID, &job.ExecutionBackend, &job.NativeID,
		&schedulerBackend, &schedulerState, &schedulerReason, &schedulerCluster,
		&schedulerObservedAt, &job.ObservationConfidence, &confidenceUpdatedAt,
	)
	if err != nil {
		return domain.Job{}, err
	}
	if err = json.Unmarshal([]byte(labels), &job.Labels); err != nil {
		return domain.Job{}, fmt.Errorf("decode job labels: %w", err)
	}
	if len(job.Labels) == 0 {
		job.Labels = nil
	}
	job.CreatedAt = job.CreatedAt.UTC()
	job.UpdatedAt = job.UpdatedAt.UTC()
	if confidenceUpdatedAt != nil {
		job.ConfidenceUpdatedAt = confidenceUpdatedAt.UTC()
	}
	if schedulerBackend != "" && schedulerObservedAt != nil {
		job.Scheduler = &domain.SchedulerStatus{
			Backend: schedulerBackend, State: schedulerState, Reason: schedulerReason,
			Cluster: schedulerCluster, ObservedAt: schedulerObservedAt.UTC(),
		}
	}

	return job, nil
}
