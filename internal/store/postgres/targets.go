package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

const createTargetOperation = "targets.create"

const updateTargetStateOperation = "targets.state"

const createTargetGenerationOperation = "targets.generations.create"

// CreateTarget creates one namespace-owned target and immutable generation.
func (store *Store) CreateTarget(
	ctx context.Context,
	principal domain.Principal,
	namespace string,
	idempotencyKey string,
	requestDigest string,
	spec domain.TargetSpec,
) (domain.CreateResult[domain.Target], error) {
	if !domain.ValidIdempotencyKey(idempotencyKey) {
		return domain.CreateResult[domain.Target]{}, errors.New("invalid idempotency key")
	}
	if err := domain.ValidateTargetSpec(spec); err != nil {
		return domain.CreateResult[domain.Target]{}, err
	}
	targetID, err := store.newID()
	if err != nil {
		return domain.CreateResult[domain.Target]{}, err
	}
	generationID, err := store.newID()
	if err != nil {
		return domain.CreateResult[domain.Target]{}, err
	}
	partitionIDs := make([]string, len(spec.Partitions))
	for index := range partitionIDs {
		partitionIDs[index], err = store.newID()
		if err != nil {
			return domain.CreateResult[domain.Target]{}, err
		}
	}

	result, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.CreateResult[domain.Target], error) {
		authorization, authorizeErr := authorizeNamespace(
			ctx, tx, principal, namespace, domain.RoleNamespaceAdmin,
		)
		if authorizeErr != nil {
			return domain.CreateResult[domain.Target]{}, authorizeErr
		}
		resourceID, replayed, reserveErr := reserveIdempotency(
			ctx, tx, authorization, createTargetOperation, "target",
			idempotencyKey, requestDigest, 201,
		)
		if reserveErr != nil {
			return domain.CreateResult[domain.Target]{}, reserveErr
		}
		if replayed {
			target, lookupErr := getTargetByID(ctx, tx, authorization.namespaceID, resourceID)
			return domain.CreateResult[domain.Target]{Value: target, Replayed: true}, lookupErr
		}
		if insertErr := insertTarget(
			ctx, tx, authorization, targetID, generationID, partitionIDs, spec,
		); insertErr != nil {
			return domain.CreateResult[domain.Target]{}, insertErr
		}
		if completeErr := completeIdempotency(
			ctx, tx, authorization, createTargetOperation, idempotencyKey, targetID, 201,
		); completeErr != nil {
			return domain.CreateResult[domain.Target]{}, completeErr
		}
		if auditErr := auditTargetCreation(
			ctx, tx, authorization, targetID, generationID, idempotencyKey, requestDigest, spec,
		); auditErr != nil {
			return domain.CreateResult[domain.Target]{}, auditErr
		}
		target, lookupErr := getTargetByID(ctx, tx, authorization.namespaceID, targetID)
		return domain.CreateResult[domain.Target]{Value: target}, lookupErr
	})
	if err != nil {
		return domain.CreateResult[domain.Target]{}, fmt.Errorf("create target: %w", err)
	}

	return result, nil
}

// CreateTargetGeneration atomically selects a new immutable target policy.
// Accepted jobs and enrolled agents keep their original generation identity;
// only subsequent placement and enrollment use the replacement.
func (store *Store) CreateTargetGeneration(
	ctx context.Context,
	principal domain.Principal,
	namespace string,
	name string,
	idempotencyKey string,
	requestDigest string,
	change domain.TargetGenerationChange,
) (domain.CreateResult[domain.Target], error) {
	if !domain.ValidIdempotencyKey(idempotencyKey) || !domain.ValidName(name) ||
		change.Spec.Name != name {
		return domain.CreateResult[domain.Target]{}, errors.New("invalid target generation request identity")
	}
	if err := domain.ValidateTargetGenerationChange(change); err != nil {
		return domain.CreateResult[domain.Target]{}, err
	}
	generationID, err := store.newID()
	if err != nil {
		return domain.CreateResult[domain.Target]{}, err
	}
	partitionIDs := make([]string, len(change.Spec.Partitions))
	for index := range partitionIDs {
		partitionIDs[index], err = store.newID()
		if err != nil {
			return domain.CreateResult[domain.Target]{}, err
		}
	}
	outboxID, err := store.newID()
	if err != nil {
		return domain.CreateResult[domain.Target]{}, err
	}

	result, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.CreateResult[domain.Target], error) {
		authorization, authorizeErr := authorizeNamespace(
			ctx, tx, principal, namespace, domain.RoleNamespaceAdmin,
		)
		if authorizeErr != nil {
			return domain.CreateResult[domain.Target]{}, authorizeErr
		}
		var targetID, state, kind string
		var revision, currentGeneration int64
		lookupErr := tx.QueryRow(ctx, `
			SELECT t.id::text, t.state, t.kind, t.revision, tg.generation
			FROM targets AS t
			JOIN target_generations AS tg ON tg.id = t.current_generation_id
			WHERE t.namespace_id = $1 AND t.name = $2
			FOR UPDATE OF t
		`, authorization.namespaceID, name).Scan(
			&targetID, &state, &kind, &revision, &currentGeneration,
		)
		if errors.Is(lookupErr, pgx.ErrNoRows) {
			return domain.CreateResult[domain.Target]{}, domain.ErrNotFound
		}
		if lookupErr != nil {
			return domain.CreateResult[domain.Target]{}, fmt.Errorf("lock target generation: %w", lookupErr)
		}
		operation := createTargetGenerationOperation + ":" + name
		resourceID, replayed, reserveErr := reserveIdempotency(
			ctx, tx, authorization, operation, "target", idempotencyKey, requestDigest, 200,
		)
		if reserveErr != nil {
			return domain.CreateResult[domain.Target]{}, reserveErr
		}
		if replayed {
			target, getErr := getTargetByID(ctx, tx, authorization.namespaceID, resourceID)

			return domain.CreateResult[domain.Target]{Value: target, Replayed: true}, getErr
		}
		if state == "retired" || revision != change.ExpectedRevision || kind != change.Spec.Kind {
			return domain.CreateResult[domain.Target]{}, domain.ErrConflict
		}
		if insertErr := insertTargetGeneration(
			ctx, tx, authorization.namespaceID, targetID, generationID,
			currentGeneration+1, partitionIDs, change.Spec,
		); insertErr != nil {
			return domain.CreateResult[domain.Target]{}, insertErr
		}
		if _, updateErr := tx.Exec(ctx, `
			UPDATE targets
			SET current_generation_id = $3, revision = revision + 1,
				updated_at = transaction_timestamp()
			WHERE namespace_id = $1 AND id = $2
		`, authorization.namespaceID, targetID, generationID); updateErr != nil {
			return domain.CreateResult[domain.Target]{}, fmt.Errorf("select target generation: %w", updateErr)
		}
		if completeErr := completeIdempotency(
			ctx, tx, authorization, operation, idempotencyKey, targetID, 200,
		); completeErr != nil {
			return domain.CreateResult[domain.Target]{}, completeErr
		}
		if _, auditErr := tx.Exec(ctx, `
			INSERT INTO audit_events (
				namespace_id, actor_principal_id, action, resource_type,
				resource_id, request_digest, idempotency_key, details
			) VALUES ($1, $2, 'target.generation_created', 'target', $3, $4, $5,
				jsonb_build_object('generationId', $6::text, 'generation', $7::bigint))
		`, authorization.namespaceID, authorization.principalID, targetID,
			requestDigest, idempotencyKey, generationID, currentGeneration+1); auditErr != nil {
			return domain.CreateResult[domain.Target]{}, fmt.Errorf("audit target generation: %w", auditErr)
		}
		if _, outboxErr := tx.Exec(ctx, `
			INSERT INTO outbox (
				id, namespace_id, topic, aggregate_type, aggregate_id, payload
			) VALUES ($1, $2, 'target.generation_created', 'target', $3::uuid,
				jsonb_build_object('targetId', $3::text, 'generationId', $4::text,
					'generation', $5::bigint))
		`, outboxID, authorization.namespaceID, targetID, generationID,
			currentGeneration+1); outboxErr != nil {
			return domain.CreateResult[domain.Target]{}, fmt.Errorf("insert target generation outbox event: %w", outboxErr)
		}
		target, getErr := getTargetByID(ctx, tx, authorization.namespaceID, targetID)

		return domain.CreateResult[domain.Target]{Value: target}, getErr
	})
	if err != nil {
		return domain.CreateResult[domain.Target]{}, fmt.Errorf("create target generation: %w", err)
	}

	return result, nil
}

// UpdateTargetState applies a revision-checked lifecycle transition. Draining
// and disabled targets stop assignment selection without rewriting accepted
// execution facts.
func (store *Store) UpdateTargetState(
	ctx context.Context,
	principal domain.Principal,
	namespace string,
	name string,
	idempotencyKey string,
	requestDigest string,
	change domain.TargetStateChange,
) (domain.CreateResult[domain.Target], error) {
	if !domain.ValidIdempotencyKey(idempotencyKey) || !domain.ValidName(name) {
		return domain.CreateResult[domain.Target]{}, errors.New("invalid target state request identity")
	}
	if err := domain.ValidateTargetStateChange(change); err != nil {
		return domain.CreateResult[domain.Target]{}, err
	}
	outboxID, err := store.newID()
	if err != nil {
		return domain.CreateResult[domain.Target]{}, err
	}
	result, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.CreateResult[domain.Target], error) {
		authorization, authorizeErr := authorizeNamespace(
			ctx, tx, principal, namespace, domain.RoleOperator, domain.RoleNamespaceAdmin,
		)
		if authorizeErr != nil {
			return domain.CreateResult[domain.Target]{}, authorizeErr
		}
		var targetID string
		var currentState string
		var currentRevision int64
		lookupErr := tx.QueryRow(ctx, `
			SELECT id::text, state, revision
			FROM targets
			WHERE namespace_id = $1 AND name = $2
			FOR UPDATE
		`, authorization.namespaceID, name).Scan(&targetID, &currentState, &currentRevision)
		if errors.Is(lookupErr, pgx.ErrNoRows) {
			return domain.CreateResult[domain.Target]{}, domain.ErrNotFound
		}
		if lookupErr != nil {
			return domain.CreateResult[domain.Target]{}, fmt.Errorf("lock target state: %w", lookupErr)
		}
		resourceID, replayed, reserveErr := reserveIdempotency(
			ctx, tx, authorization, updateTargetStateOperation+":"+name, "target",
			idempotencyKey, requestDigest, 200,
		)
		if reserveErr != nil {
			return domain.CreateResult[domain.Target]{}, reserveErr
		}
		if replayed {
			target, getErr := getTargetByID(ctx, tx, authorization.namespaceID, resourceID)
			return domain.CreateResult[domain.Target]{Value: target, Replayed: true}, getErr
		}
		if currentRevision != change.ExpectedRevision || !validTargetStateTransition(currentState, change.State) {
			return domain.CreateResult[domain.Target]{}, domain.ErrConflict
		}
		if _, updateErr := tx.Exec(ctx, `
			UPDATE targets
			SET state = $3, revision = revision + 1, updated_at = transaction_timestamp()
			WHERE namespace_id = $1 AND id = $2
		`, authorization.namespaceID, targetID, change.State); updateErr != nil {
			return domain.CreateResult[domain.Target]{}, fmt.Errorf("update target state: %w", updateErr)
		}
		if completeErr := completeIdempotency(
			ctx, tx, authorization, updateTargetStateOperation+":"+name,
			idempotencyKey, targetID, 200,
		); completeErr != nil {
			return domain.CreateResult[domain.Target]{}, completeErr
		}
		if _, auditErr := tx.Exec(ctx, `
			INSERT INTO audit_events (
				namespace_id, actor_principal_id, action, resource_type,
				resource_id, request_digest, idempotency_key, details
			) VALUES ($1, $2, 'target.state_changed', 'target', $3, $4, $5,
				jsonb_build_object('from', $6::text, 'to', $7::text))
		`, authorization.namespaceID, authorization.principalID, targetID,
			requestDigest, idempotencyKey, currentState, change.State); auditErr != nil {
			return domain.CreateResult[domain.Target]{}, fmt.Errorf("audit target state: %w", auditErr)
		}
		if _, outboxErr := tx.Exec(ctx, `
			INSERT INTO outbox (
				id, namespace_id, topic, aggregate_type, aggregate_id, payload
			) VALUES ($1, $2, 'target.state_changed', 'target', $3,
				jsonb_build_object('targetId', $3::text, 'state', $4::text))
		`, outboxID, authorization.namespaceID, targetID, change.State); outboxErr != nil {
			return domain.CreateResult[domain.Target]{}, fmt.Errorf("insert target state outbox event: %w", outboxErr)
		}
		target, getErr := getTargetByID(ctx, tx, authorization.namespaceID, targetID)
		return domain.CreateResult[domain.Target]{Value: target}, getErr
	})
	if err != nil {
		return domain.CreateResult[domain.Target]{}, fmt.Errorf("update target state: %w", err)
	}

	return result, nil
}

func validTargetStateTransition(current, requested string) bool {
	if current == requested {
		return true
	}
	allowed := map[string][]string{
		"active":   {"draining", "disabled"},
		"draining": {"active", "disabled", "retired"},
		"disabled": {"active", "retired"},
		"retired":  {},
	}
	for _, state := range allowed[current] {
		if requested == state {
			return true
		}
	}

	return false
}

// GetTarget returns one target to any current namespace member.
func (store *Store) GetTarget(
	ctx context.Context,
	principal domain.Principal,
	namespace string,
	name string,
) (domain.Target, error) {
	target, err := scanTarget(store.pool.QueryRow(ctx, targetSelect+`
		JOIN memberships AS m ON m.namespace_id = n.id
		JOIN principals AS p ON p.id = m.principal_id
		WHERE p.issuer = $1 AND p.subject = $2 AND n.name = $3 AND t.name = $4
	`, principal.Issuer, principal.Subject, namespace, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Target{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Target{}, fmt.Errorf("get target: %w", err)
	}
	if partitionErr := store.loadPartitions(ctx, &target); partitionErr != nil {
		return domain.Target{}, partitionErr
	}

	return target, nil
}

// ListTargets returns a bounded name-ordered target list to namespace members.
func (store *Store) ListTargets(
	ctx context.Context,
	principal domain.Principal,
	namespace string,
) ([]domain.Target, error) {
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
		return nil, fmt.Errorf("authorize target list: %w", err)
	}
	if !authorized {
		return nil, domain.ErrForbidden
	}
	rows, err := store.pool.Query(ctx, targetSelect+`
		JOIN memberships AS m ON m.namespace_id = n.id
		JOIN principals AS p ON p.id = m.principal_id
		WHERE p.issuer = $1 AND p.subject = $2 AND n.name = $3
		ORDER BY t.name
		LIMIT 1000
	`, principal.Issuer, principal.Subject, namespace)
	if err != nil {
		return nil, fmt.Errorf("list targets: %w", err)
	}
	defer rows.Close()
	targets := make([]domain.Target, 0)
	for rows.Next() {
		target, scanErr := scanTarget(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan target: %w", scanErr)
		}
		targets = append(targets, target)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate targets: %w", err)
	}
	for index := range targets {
		if partitionErr := store.loadPartitions(ctx, &targets[index]); partitionErr != nil {
			return nil, partitionErr
		}
	}

	return targets, nil
}

func insertTarget(
	ctx context.Context,
	tx pgx.Tx,
	authorization namespaceAuthorization,
	targetID string,
	generationID string,
	partitionIDs []string,
	spec domain.TargetSpec,
) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO targets (id, namespace_id, name, kind)
		VALUES ($1, $2, $3, $4)
	`, targetID, authorization.namespaceID, spec.Name, spec.Kind); err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) && postgresError.Code == "23505" {
			return domain.ErrConflict
		}
		return fmt.Errorf("insert target: %w", err)
	}
	if err := insertTargetGeneration(
		ctx, tx, authorization.namespaceID, targetID, generationID, 1, partitionIDs, spec,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE targets SET current_generation_id = $2 WHERE id = $1
	`, targetID, generationID); err != nil {
		return fmt.Errorf("select target generation: %w", err)
	}

	return nil
}

func insertTargetGeneration(
	ctx context.Context,
	tx pgx.Tx,
	namespaceID, targetID, generationID string,
	generation int64,
	partitionIDs []string,
	spec domain.TargetSpec,
) error {
	stores := spec.ArtifactStores
	if stores == nil {
		stores = []domain.ArtifactStoreSpec{}
	}
	artifactStores, err := json.Marshal(stores)
	if err != nil {
		return fmt.Errorf("encode target artifact stores: %w", err)
	}
	provider, err := json.Marshal(domain.NormalizeTargetProvider(spec.Provider))
	if err != nil {
		return fmt.Errorf("encode target provider: %w", err)
	}
	if _, err = tx.Exec(ctx, `
		INSERT INTO target_generations (
			id, namespace_id, target_id, generation, execution_backend,
			control_transport, runtimes, operating_systems, architectures, capabilities,
			log_store_name, log_store_version, artifact_stores, provider
		) VALUES ($1, $2, $3, $4, $5, 'agent-api', $6, $7, $8, $9,
			NULLIF($10, ''), NULLIF($11, 0), $12::jsonb, $13::jsonb)
	`, generationID, namespaceID, targetID, generation, spec.ExecutionBackend,
		nonNilStrings(spec.Runtimes), nonNilStrings(spec.OperatingSystems),
		nonNilStrings(spec.Architectures), nonNilStrings(spec.Capabilities),
		spec.LogStoreName, spec.LogStoreVersion, string(artifactStores), string(provider),
	); err != nil {
		return fmt.Errorf("insert target generation: %w", err)
	}
	for index, partition := range spec.Partitions {
		if _, err := tx.Exec(ctx, `
			INSERT INTO partitions (
				id, namespace_id, target_generation_id, name, is_default
			) VALUES ($1, $2, $3, $4, $5)
		`, partitionIDs[index], namespaceID, generationID,
			partition.Name, partition.IsDefault,
		); err != nil {
			return fmt.Errorf("insert target partition: %w", err)
		}
	}

	return nil
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}

	return values
}

func auditTargetCreation(
	ctx context.Context,
	tx pgx.Tx,
	authorization namespaceAuthorization,
	targetID string,
	generationID string,
	idempotencyKey string,
	requestDigest string,
	spec domain.TargetSpec,
) error {
	details, err := json.Marshal(map[string]any{
		"generationId":     generationID,
		"kind":             spec.Kind,
		"executionBackend": spec.ExecutionBackend,
		"provider":         domain.NormalizeTargetProvider(spec.Provider),
	})
	if err != nil {
		return fmt.Errorf("encode target audit details: %w", err)
	}
	if _, err = tx.Exec(ctx, `
		INSERT INTO audit_events (
			namespace_id, actor_principal_id, action, resource_type,
			resource_id, request_digest, idempotency_key, details
		) VALUES ($1, $2, 'target.created', 'target', $3, $4, $5, $6::jsonb)
	`, authorization.namespaceID, authorization.principalID, targetID,
		requestDigest, idempotencyKey, string(details),
	); err != nil {
		return fmt.Errorf("insert target audit event: %w", err)
	}

	return nil
}

const targetSelect = `
	SELECT t.id::text, tg.id::text, tg.generation, n.name, t.name, t.kind,
		t.state, tg.execution_backend, tg.control_transport, tg.runtimes,
		tg.operating_systems, tg.architectures, tg.capabilities,
		COALESCE(tg.log_store_name, ''), COALESCE(tg.log_store_version, 0),
		tg.artifact_stores::text, tg.provider::text,
		t.revision, t.created_at, t.updated_at
	FROM targets AS t
	JOIN namespaces AS n ON n.id = t.namespace_id
	JOIN target_generations AS tg ON tg.id = t.current_generation_id
`

func getTargetByID(
	ctx context.Context,
	tx pgx.Tx,
	namespaceID string,
	targetID string,
) (domain.Target, error) {
	target, err := scanTarget(tx.QueryRow(ctx, targetSelect+`
		WHERE t.namespace_id = $1 AND t.id = $2
	`, namespaceID, targetID))
	if err != nil {
		return domain.Target{}, err
	}
	if partitionErr := loadPartitionsWithQuerier(ctx, tx, &target); partitionErr != nil {
		return domain.Target{}, partitionErr
	}

	return target, nil
}

func scanTarget(row rowScanner) (domain.Target, error) {
	var target domain.Target
	var artifactStores string
	var provider string
	if err := row.Scan(
		&target.ID, &target.GenerationID, &target.Generation, &target.Namespace,
		&target.Name, &target.Kind, &target.State, &target.ExecutionBackend,
		&target.Transport, &target.Runtimes, &target.OperatingSystems,
		&target.Architectures, &target.Capabilities, &target.LogStoreName,
		&target.LogStoreVersion, &artifactStores, &provider, &target.Revision,
		&target.CreatedAt, &target.UpdatedAt,
	); err != nil {
		return domain.Target{}, err
	}
	if err := json.Unmarshal([]byte(artifactStores), &target.ArtifactStores); err != nil {
		return domain.Target{}, fmt.Errorf("decode target artifact stores: %w", err)
	}
	if err := json.Unmarshal([]byte(provider), &target.Provider); err != nil {
		return domain.Target{}, fmt.Errorf("decode target provider: %w", err)
	}
	target.CreatedAt = target.CreatedAt.UTC()
	target.UpdatedAt = target.UpdatedAt.UTC()

	return target, nil
}

type queryRowQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func (store *Store) loadPartitions(ctx context.Context, target *domain.Target) error {
	return loadPartitionsWithQuerier(ctx, store.pool, target)
}

func loadPartitionsWithQuerier(ctx context.Context, querier queryRowQuerier, target *domain.Target) error {
	rows, err := querier.Query(ctx, `
		SELECT name, is_default
		FROM partitions
		WHERE target_generation_id = $1 AND state != 'retired'
		ORDER BY name
	`, target.GenerationID)
	if err != nil {
		return fmt.Errorf("list target partitions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var partition domain.PartitionSpec
		if err = rows.Scan(&partition.Name, &partition.IsDefault); err != nil {
			return fmt.Errorf("scan target partition: %w", err)
		}
		target.Partitions = append(target.Partitions, partition)
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("iterate target partitions: %w", err)
	}
	sort.Slice(target.Partitions, func(left, right int) bool {
		return target.Partitions[left].Name < target.Partitions[right].Name
	})

	return nil
}
