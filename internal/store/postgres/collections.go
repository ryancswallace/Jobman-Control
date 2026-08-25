package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	jobmanprotocol "github.com/ryancswallace/jobman-control/contracts/jobman/v1alpha1"
	"github.com/ryancswallace/jobman-control/internal/domain"
)

const submitCollectionOperation = "collections.create"

// SubmitCollection atomically accepts a bounded set of independently sealed
// child jobs. No target I/O occurs in the transaction.
func (store *Store) SubmitCollection(
	ctx context.Context,
	principal domain.Principal,
	idempotencyKey string,
	submission domain.CollectionSubmission,
) (domain.CollectionResult, error) {
	if !domain.ValidIdempotencyKey(idempotencyKey) {
		return domain.CollectionResult{}, errors.New("invalid idempotency key")
	}
	if err := validateCollectionSubmission(submission); err != nil {
		return domain.CollectionResult{}, err
	}
	collectionID, err := store.newID()
	if err != nil {
		return domain.CollectionResult{}, err
	}
	jobIDs := make([]string, len(submission.Items))
	outboxIDs := make([]string, len(submission.Items)+1)
	for index := range jobIDs {
		jobIDs[index], err = store.newID()
		if err != nil {
			return domain.CollectionResult{}, err
		}
	}
	for index := range outboxIDs {
		outboxIDs[index], err = store.newID()
		if err != nil {
			return domain.CollectionResult{}, err
		}
	}

	result, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.CollectionResult, error) {
		authorization, authorizeErr := authorizeSubmission(ctx, tx, principal, submission.Namespace)
		if authorizeErr != nil {
			return domain.CollectionResult{}, authorizeErr
		}
		resourceID, replayed, reserveErr := reserveIdempotency(
			ctx, tx, authorization, submitCollectionOperation, "collection",
			idempotencyKey, submission.RequestDigest, 201,
		)
		if reserveErr != nil {
			return domain.CollectionResult{}, reserveErr
		}
		if replayed {
			collection, getErr := getCollectionByID(ctx, tx, authorization.namespaceID, resourceID)

			return domain.CollectionResult{Collection: collection, Replayed: true}, getErr
		}
		if quotaErr := enforceNamespaceQuota(
			ctx, tx, authorization.namespaceID, len(submission.Items), len(submission.Items), 0,
		); quotaErr != nil {
			return domain.CollectionResult{}, quotaErr
		}
		placements := make([]resolvedPlacement, len(submission.Items))
		for index, item := range submission.Items {
			placement, resolveErr := resolveTarget(ctx, tx, authorization.namespaceID, item)
			if resolveErr != nil {
				return domain.CollectionResult{}, fmt.Errorf("resolve collection item %q: %w", item.Name, resolveErr)
			}
			placements[index] = placement
		}
		arrayMode, arrayErr := resolveCollectionArrayMode(submission, placements)
		if arrayErr != nil {
			return domain.CollectionResult{}, arrayErr
		}
		if insertErr := insertCollection(
			ctx, tx, authorization, collectionID, submission, arrayMode,
		); insertErr != nil {
			return domain.CollectionResult{}, insertErr
		}
		for index, item := range submission.Items {
			if insertErr := insertWorkloadRevision(ctx, tx, authorization, item); insertErr != nil {
				return domain.CollectionResult{}, fmt.Errorf("insert collection item %q: %w", item.Name, insertErr)
			}
			job, insertErr := insertJob(ctx, tx, authorization, jobIDs[index], item, placements[index])
			if insertErr != nil {
				return domain.CollectionResult{}, fmt.Errorf("insert collection item %q: %w", item.Name, insertErr)
			}
			if _, insertErr = tx.Exec(ctx, `
				UPDATE jobs SET collection_id = $2, collection_index = $3 WHERE id = $1
			`, job.ID, collectionID, index); insertErr != nil {
				return domain.CollectionResult{}, fmt.Errorf("bind collection item %q: %w", item.Name, insertErr)
			}
			if _, insertErr = tx.Exec(ctx, `
				INSERT INTO collection_items (collection_id, item_index, item_name, job_id)
				VALUES ($1, $2, $3, $4)
			`, collectionID, index, item.Name, job.ID); insertErr != nil {
				return domain.CollectionResult{}, fmt.Errorf("index collection item %q: %w", item.Name, insertErr)
			}
			if evidenceErr := insertAcceptanceEvidence(
				ctx, tx, authorization, idempotencyKey, outboxIDs[index+1], job,
			); evidenceErr != nil {
				return domain.CollectionResult{}, fmt.Errorf("record collection item %q: %w", item.Name, evidenceErr)
			}
		}
		if completeErr := completeIdempotency(
			ctx, tx, authorization, submitCollectionOperation,
			idempotencyKey, collectionID, 201,
		); completeErr != nil {
			return domain.CollectionResult{}, completeErr
		}
		if evidenceErr := insertCollectionAcceptanceEvidence(
			ctx, tx, authorization, collectionID, outboxIDs[0], idempotencyKey, submission,
		); evidenceErr != nil {
			return domain.CollectionResult{}, evidenceErr
		}
		collection, getErr := getCollectionByID(ctx, tx, authorization.namespaceID, collectionID)

		return domain.CollectionResult{Collection: collection}, getErr
	})
	if err != nil {
		return domain.CollectionResult{}, fmt.Errorf("submit collection: %w", err)
	}

	return result, nil
}

func validateCollectionSubmission(submission domain.CollectionSubmission) error {
	if !domain.ValidName(submission.Namespace) || !domain.ValidName(submission.Name) ||
		len(submission.Items) == 0 || len(submission.Items) > 10_000 ||
		submission.MaxActive < 1 || submission.MaxActive > len(submission.Items) ||
		!slices.Contains([]string{"continue", "fail-fast"}, submission.FailurePolicy) ||
		!slices.Contains([]string{"never", "prefer", "require"}, submission.ArrayPolicy) ||
		!json.Valid(submission.RequestDocument) || submission.RequestDigest == "" {
		return errors.New("collection submission is invalid")
	}
	seen := make(map[string]struct{}, len(submission.Items))
	for _, item := range submission.Items {
		if !domain.ValidName(item.Name) {
			return errors.New("collection item name is invalid")
		}
		if _, exists := seen[item.Name]; exists {
			return errors.New("collection item name is duplicated")
		}
		seen[item.Name] = struct{}{}
		if item.Namespace != submission.Namespace || validateSubmission(item) != nil {
			return errors.New("collection item submission is invalid")
		}
	}

	return nil
}

func resolveCollectionArrayMode(
	submission domain.CollectionSubmission,
	placements []resolvedPlacement,
) (string, error) {
	if submission.ArrayPolicy == "never" {
		return "individual", nil
	}
	compatible := len(placements) > 1
	var resources string
	if compatible {
		first := placements[0]
		for index, placement := range placements {
			if placement.executionBackend != "slurm" || placement.targetGenerationID != first.targetGenerationID ||
				placement.partition != first.partition {
				compatible = false
				break
			}
			var workload jobmanprotocol.Workload
			if err := json.Unmarshal(submission.Items[index].WorkloadDocument, &workload); err != nil {
				return "", fmt.Errorf("decode collection item resources: %w", err)
			}
			encoded, err := json.Marshal(workload.Spec.Resources)
			if err != nil {
				return "", fmt.Errorf("encode collection item resources: %w", err)
			}
			if index == 0 {
				resources = string(encoded)
			} else if string(encoded) != resources {
				compatible = false
				break
			}
		}
	}
	if compatible {
		return "slurm-array", nil
	}
	if submission.ArrayPolicy == "require" {
		return "", fmt.Errorf("%w: collection cannot be compiled as one Slurm array", domain.ErrInvalidPlacement)
	}

	return "individual", nil
}

func insertCollection(
	ctx context.Context,
	tx pgx.Tx,
	authorization namespaceAuthorization,
	collectionID string,
	submission domain.CollectionSubmission,
	arrayMode string,
) error {
	labels := submission.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	encodedLabels, err := json.Marshal(labels)
	if err != nil {
		return fmt.Errorf("encode collection labels: %w", err)
	}
	if _, err = tx.Exec(ctx, `
		INSERT INTO collections (
			id, namespace_id, owner_principal_id, name, labels,
			request_digest, request_document, max_active,
			failure_policy, array_policy, array_mode
		) VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7::jsonb, $8, $9, $10, $11)
	`, collectionID, authorization.namespaceID, authorization.principalID,
		submission.Name, string(encodedLabels), submission.RequestDigest,
		string(submission.RequestDocument), submission.MaxActive,
		submission.FailurePolicy, submission.ArrayPolicy, arrayMode); err != nil {
		return fmt.Errorf("insert collection: %w", err)
	}

	return nil
}

func insertCollectionAcceptanceEvidence(
	ctx context.Context,
	tx pgx.Tx,
	authorization namespaceAuthorization,
	collectionID, outboxID, idempotencyKey string,
	submission domain.CollectionSubmission,
) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_events (
			namespace_id, actor_principal_id, action, resource_type,
			resource_id, request_digest, idempotency_key, details
		) VALUES ($1, $2, 'collection.accepted', 'collection', $3, $4, $5,
			jsonb_build_object('items', $6::integer, 'maxActive', $7::integer,
				'failurePolicy', $8::text, 'arrayPolicy', $9::text))
	`, authorization.namespaceID, authorization.principalID, collectionID,
		submission.RequestDigest, idempotencyKey, len(submission.Items),
		submission.MaxActive, submission.FailurePolicy, submission.ArrayPolicy); err != nil {
		return fmt.Errorf("audit collection acceptance: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox (
			id, namespace_id, topic, aggregate_type, aggregate_id, payload
		) VALUES ($1, $2, 'collection.accepted', 'collection', $3::uuid,
			jsonb_build_object('collectionId', $3::text, 'items', $4::integer))
	`, outboxID, authorization.namespaceID, collectionID, len(submission.Items)); err != nil {
		return fmt.Errorf("insert collection outbox event: %w", err)
	}

	return nil
}

// GetCollection returns one aggregate and all ordered child job snapshots to
// a current namespace member.
func (store *Store) GetCollection(
	ctx context.Context,
	principal domain.Principal,
	namespace, collectionID string,
) (domain.Collection, error) {
	collection, err := scanCollection(store.pool.QueryRow(ctx, collectionSelect+`
		JOIN memberships AS m ON m.namespace_id = n.id
		JOIN principals AS p ON p.id = m.principal_id
		WHERE p.issuer = $1 AND p.subject = $2 AND n.name = $3 AND c.id = $4
		GROUP BY c.id, n.name
	`, principal.Issuer, principal.Subject, namespace, collectionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Collection{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Collection{}, fmt.Errorf("get collection: %w", err)
	}
	if loadErr := loadCollectionItems(ctx, store.pool, &collection); loadErr != nil {
		return domain.Collection{}, loadErr
	}

	return collection, nil
}

const collectionSelect = `
	SELECT c.id::text, n.name, c.name, c.labels::text, c.max_active,
		c.failure_policy, c.array_policy, c.array_mode, c.revision,
		c.created_at, c.updated_at,
		count(j.id)::integer,
		count(j.id) FILTER (WHERE j.phase NOT IN ('accepted', 'terminal'))::integer,
		count(j.id) FILTER (WHERE j.phase = 'terminal')::integer,
		count(j.id) FILTER (WHERE j.phase = 'terminal' AND j.outcome = 'success')::integer,
		count(j.id) FILTER (WHERE j.phase = 'terminal' AND j.outcome = 'cancelled')::integer,
		count(j.id) FILTER (WHERE j.phase = 'terminal' AND j.outcome NOT IN ('success', 'cancelled'))::integer
	FROM collections AS c
	JOIN namespaces AS n ON n.id = c.namespace_id
	LEFT JOIN jobs AS j ON j.collection_id = c.id
`

func getCollectionByID(
	ctx context.Context,
	tx pgx.Tx,
	namespaceID, collectionID string,
) (domain.Collection, error) {
	collection, err := scanCollection(tx.QueryRow(ctx, collectionSelect+`
		WHERE c.namespace_id = $1 AND c.id = $2
		GROUP BY c.id, n.name
	`, namespaceID, collectionID))
	if err != nil {
		return domain.Collection{}, err
	}
	if loadErr := loadCollectionItems(ctx, tx, &collection); loadErr != nil {
		return domain.Collection{}, loadErr
	}

	return collection, nil
}

func scanCollection(row rowScanner) (domain.Collection, error) {
	var value domain.Collection
	var labels string
	if err := row.Scan(
		&value.ID, &value.Namespace, &value.Name, &labels, &value.MaxActive,
		&value.FailurePolicy, &value.ArrayPolicy, &value.ArrayMode, &value.Revision,
		&value.CreatedAt, &value.UpdatedAt, &value.Total, &value.Active,
		&value.Terminal, &value.Succeeded, &value.Canceled, &value.Failed,
	); err != nil {
		return domain.Collection{}, err
	}
	if err := json.Unmarshal([]byte(labels), &value.Labels); err != nil {
		return domain.Collection{}, fmt.Errorf("decode collection labels: %w", err)
	}
	if len(value.Labels) == 0 {
		value.Labels = nil
	}
	value.CreatedAt = value.CreatedAt.UTC()
	value.UpdatedAt = value.UpdatedAt.UTC()
	switch {
	case value.Total > 0 && value.Terminal == value.Total:
		value.Phase = "terminal"
		switch {
		case value.Failed > 0:
			value.Outcome = "failure"
		case value.Canceled > 0:
			value.Outcome = "cancelled" //nolint:misspell // Frozen v1alpha1 wire value.
		default:
			value.Outcome = "success"
		}
	case value.Active > 0:
		value.Phase = "running"
	default:
		value.Phase = "accepted"
	}

	return value, nil
}

type collectionQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func loadCollectionItems(
	ctx context.Context,
	querier collectionQuerier,
	collection *domain.Collection,
) error {
	rows, err := querier.Query(ctx, `
		SELECT item_index, item_name, job_id::text
		FROM collection_items WHERE collection_id = $1 ORDER BY item_index
	`, collection.ID)
	if err != nil {
		return fmt.Errorf("list collection items: %w", err)
	}
	defer rows.Close()
	type itemIdentity struct {
		index int
		name  string
		jobID string
	}
	identities := make([]itemIdentity, 0, collection.Total)
	for rows.Next() {
		var identity itemIdentity
		if err = rows.Scan(&identity.index, &identity.name, &identity.jobID); err != nil {
			return fmt.Errorf("scan collection item: %w", err)
		}
		identities = append(identities, identity)
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("iterate collection items: %w", err)
	}
	collection.Items = make([]domain.CollectionItem, 0, len(identities))
	for _, identity := range identities {
		job, getErr := getJobWithQuerier(ctx, querier, collection.Namespace, identity.jobID)
		if getErr != nil {
			return fmt.Errorf("load collection item %q: %w", identity.name, getErr)
		}
		collection.Items = append(collection.Items, domain.CollectionItem{
			Index: identity.index, Name: identity.name, Job: job,
		})
	}

	return nil
}

func getJobWithQuerier(
	ctx context.Context,
	querier collectionQuerier,
	namespace, jobID string,
) (domain.Job, error) {
	// Collection loading already authorized the namespace or occurs inside an
	// authorized transaction; retain the common scanner shape used by GetJob.
	return scanJob(querier.QueryRow(ctx, `
		SELECT
			j.id::text, $1::text, j.name, j.labels::text, j.phase,
			j.desired_state, COALESCE(j.outcome, ''), j.placement_target,
			COALESCE(j.placement_partition, ''), j.workload_digest,
			j.request_digest, j.revision, j.created_at, j.updated_at,
			COALESCE(j.target_id::text, ''), COALESCE(j.target_generation_id::text, ''),
			COALESCE(tg.execution_backend, ''), '', '', '', '', '', NULL::timestamptz,
			'', NULL::timestamptz
		FROM jobs AS j
		LEFT JOIN target_generations AS tg ON tg.id = j.target_generation_id
		WHERE j.id = $2
	`, namespace, jobID))
}

type collectionSibling struct {
	jobID, phase, executionID, runID, agentID, executionPhase string
}

// applyCollectionCompletion enforces fail-fast only through ordinary Jobman
// cancellation transitions. It never treats an array as one lifecycle unit.
func (store *Store) applyCollectionCompletion(
	ctx context.Context,
	tx pgx.Tx,
	namespaceID, completedJobID, outcome string,
) error {
	var collectionID, failurePolicy string
	err := tx.QueryRow(ctx, `
		SELECT c.id::text, c.failure_policy
		FROM jobs AS j JOIN collections AS c ON c.id = j.collection_id
		WHERE j.id = $1 AND j.namespace_id = $2
		FOR UPDATE OF c
	`, completedJobID, namespaceID).Scan(&collectionID, &failurePolicy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock completed collection: %w", err)
	}
	if failurePolicy == "fail-fast" && outcome != "success" {
		rows, queryErr := tx.Query(ctx, `
			SELECT j.id::text, j.phase,
				COALESCE(e.id::text, ''), COALESCE(r.id::text, ''),
				COALESCE(e.agent_id::text, ''), COALESCE(e.phase, '')
			FROM jobs AS j
			LEFT JOIN runs AS r ON r.job_id = j.id
			LEFT JOIN executions AS e ON e.run_id = r.id
			WHERE j.collection_id = $1 AND j.id != $2 AND j.phase != 'terminal'
			ORDER BY j.collection_index
			FOR UPDATE OF j
		`, collectionID, completedJobID)
		if queryErr != nil {
			return fmt.Errorf("list fail-fast collection siblings: %w", queryErr)
		}
		var siblings []collectionSibling
		for rows.Next() {
			var sibling collectionSibling
			if queryErr = rows.Scan(
				&sibling.jobID, &sibling.phase, &sibling.executionID,
				&sibling.runID, &sibling.agentID, &sibling.executionPhase,
			); queryErr != nil {
				rows.Close()

				return fmt.Errorf("scan fail-fast collection sibling: %w", queryErr)
			}
			siblings = append(siblings, sibling)
		}
		queryErr = rows.Err()
		rows.Close()
		if queryErr != nil {
			return fmt.Errorf("iterate fail-fast collection siblings: %w", queryErr)
		}
		for _, sibling := range siblings {
			if cancelErr := store.cancelCollectionSibling(ctx, tx, namespaceID, sibling); cancelErr != nil {
				return cancelErr
			}
		}
	}
	if _, err = tx.Exec(ctx, `
		UPDATE collections SET revision = revision + 1,
			updated_at = transaction_timestamp() WHERE id = $1
	`, collectionID); err != nil {
		return fmt.Errorf("update completed collection: %w", err)
	}

	return nil
}

func (store *Store) cancelCollectionSibling(
	ctx context.Context,
	tx pgx.Tx,
	namespaceID string,
	sibling collectionSibling,
) error {
	switch {
	case sibling.executionID == "":
		_, err := tx.Exec(ctx, `
			UPDATE jobs SET phase = 'terminal', desired_state = 'cancel',
				outcome = 'cancelled', revision = revision + 1,
				updated_at = transaction_timestamp() WHERE id = $1
		`, sibling.jobID)
		if err != nil {
			return fmt.Errorf("cancel undispatched collection sibling: %w", err)
		}
	case sibling.executionPhase == "planned":
		if _, err := tx.Exec(ctx, `
			UPDATE assignments SET state = 'withdrawn'
			WHERE execution_id = $1 AND state = 'offered'
		`, sibling.executionID); err != nil {
			return fmt.Errorf("withdraw collection sibling assignment: %w", err)
		}
		//nolint:misspell // Frozen v1alpha1 outcome and failure code.
		if _, err := tx.Exec(ctx, `
			UPDATE executions SET phase = 'terminal', outcome = 'cancelled',
				process_result = '{"outcome":"cancelled","failureCode":"cancelled_before_launch"}'::jsonb,
				revision = revision + 1, updated_at = transaction_timestamp()
			WHERE id = $1
		`, sibling.executionID); err != nil {
			return fmt.Errorf("cancel planned collection sibling execution: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE runs SET phase = 'terminal', desired_state = 'cancel', outcome = 'cancelled',
				updated_at = transaction_timestamp() WHERE id = $1
		`, sibling.runID); err != nil {
			return fmt.Errorf("cancel planned collection sibling run: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE jobs SET phase = 'terminal', desired_state = 'cancel', outcome = 'cancelled',
				revision = revision + 1, updated_at = transaction_timestamp() WHERE id = $1
		`, sibling.jobID); err != nil {
			return fmt.Errorf("cancel planned collection sibling job: %w", err)
		}
	default:
		actionID, err := store.newID()
		if err != nil {
			return fmt.Errorf("create collection cancellation identity: %w", err)
		}
		action := jobmanprotocol.DesiredAction{
			APIVersion: jobmanprotocol.V1Alpha1, Kind: jobmanprotocol.DesiredActionKind,
			Metadata: jobmanprotocol.DesiredActionMetadata{
				ActionID: actionID, ExecutionID: sibling.executionID,
				AgentID: sibling.agentID, Revision: 1, RequestedAt: time.Now().UTC(),
			},
			Spec: jobmanprotocol.DesiredActionSpec{Type: "cancel"},
		}
		encoded, err := json.Marshal(action)
		if err != nil {
			return fmt.Errorf("encode collection cancellation: %w", err)
		}
		if _, err = tx.Exec(ctx, `
			INSERT INTO desired_actions (
				id, namespace_id, execution_id, agent_id, action_type, document
			) VALUES ($1, $2, $3, $4, 'cancel', $5::jsonb)
			ON CONFLICT (execution_id, action_type) DO NOTHING
		`, actionID, namespaceID, sibling.executionID, sibling.agentID, string(encoded)); err != nil {
			return fmt.Errorf("request collection sibling cancellation: %w", err)
		}
		if _, err = tx.Exec(ctx, `
			UPDATE runs SET desired_state = 'cancel', updated_at = transaction_timestamp()
			WHERE id = $1
		`, sibling.runID); err != nil {
			return fmt.Errorf("update collection sibling run desire: %w", err)
		}
		if _, err = tx.Exec(ctx, `
			UPDATE jobs SET desired_state = 'cancel', revision = revision + 1,
				updated_at = transaction_timestamp() WHERE id = $1
		`, sibling.jobID); err != nil {
			return fmt.Errorf("update collection sibling job desire: %w", err)
		}
	}

	return nil
}
