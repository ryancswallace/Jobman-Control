package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

const (
	submitGraphOperation   = "graphs.create"
	cancelledGraphWireName = "cancelled" //nolint:misspell // Frozen v1alpha1 wire spelling.
)

// SubmitGraph atomically accepts an immutable DAG and all independently
// observable node jobs. Placement is resolved before any row is committed.
func (store *Store) SubmitGraph(
	ctx context.Context,
	principal domain.Principal,
	idempotencyKey string,
	submission domain.GraphSubmission,
) (domain.GraphResult, error) {
	if !domain.ValidIdempotencyKey(idempotencyKey) {
		return domain.GraphResult{}, errors.New("invalid idempotency key")
	}
	if err := validateGraphSubmission(submission); err != nil {
		return domain.GraphResult{}, err
	}
	graphID, err := store.newID()
	if err != nil {
		return domain.GraphResult{}, err
	}
	jobIDs := make([]string, len(submission.Nodes))
	outboxIDs := make([]string, len(submission.Nodes)+1)
	for index := range jobIDs {
		jobIDs[index], err = store.newID()
		if err != nil {
			return domain.GraphResult{}, err
		}
	}
	for index := range outboxIDs {
		outboxIDs[index], err = store.newID()
		if err != nil {
			return domain.GraphResult{}, err
		}
	}

	result, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.GraphResult, error) {
		authorization, authorizeErr := authorizeSubmission(ctx, tx, principal, submission.Namespace)
		if authorizeErr != nil {
			return domain.GraphResult{}, authorizeErr
		}
		resourceID, replayed, reserveErr := reserveIdempotency(
			ctx, tx, authorization, submitGraphOperation, "graph",
			idempotencyKey, submission.RequestDigest, 201,
		)
		if reserveErr != nil {
			return domain.GraphResult{}, reserveErr
		}
		if replayed {
			graph, getErr := getGraphByID(ctx, tx, authorization.namespaceID, resourceID)

			return domain.GraphResult{Graph: graph, Replayed: true}, getErr
		}
		if quotaErr := enforceNamespaceQuota(
			ctx, tx, authorization.namespaceID, len(submission.Nodes), 0, len(submission.Nodes),
		); quotaErr != nil {
			return domain.GraphResult{}, quotaErr
		}
		placements := make([]resolvedPlacement, len(submission.Nodes))
		for index, node := range submission.Nodes {
			placement, resolveErr := resolveTarget(ctx, tx, authorization.namespaceID, node)
			if resolveErr != nil {
				return domain.GraphResult{}, fmt.Errorf("resolve graph node %q: %w", node.Name, resolveErr)
			}
			placements[index] = placement
		}
		if insertErr := insertGraph(ctx, tx, authorization, graphID, submission); insertErr != nil {
			return domain.GraphResult{}, insertErr
		}
		jobsByName := make(map[string]string, len(submission.Nodes))
		for index, node := range submission.Nodes {
			if insertErr := insertWorkloadRevision(ctx, tx, authorization, node); insertErr != nil {
				return domain.GraphResult{}, fmt.Errorf("insert graph node %q: %w", node.Name, insertErr)
			}
			job, insertErr := insertJob(ctx, tx, authorization, jobIDs[index], node, placements[index])
			if insertErr != nil {
				return domain.GraphResult{}, fmt.Errorf("insert graph node %q: %w", node.Name, insertErr)
			}
			if _, insertErr = tx.Exec(ctx, `
				UPDATE jobs SET graph_id = $2, graph_index = $3 WHERE id = $1
			`, job.ID, graphID, index); insertErr != nil {
				return domain.GraphResult{}, fmt.Errorf("bind graph node %q: %w", node.Name, insertErr)
			}
			if _, insertErr = tx.Exec(ctx, `
				INSERT INTO graph_nodes (graph_id, node_index, node_name, job_id)
				VALUES ($1, $2, $3, $4)
			`, graphID, index, node.Name, job.ID); insertErr != nil {
				return domain.GraphResult{}, fmt.Errorf("index graph node %q: %w", node.Name, insertErr)
			}
			jobsByName[node.Name] = job.ID
			if evidenceErr := insertAcceptanceEvidence(
				ctx, tx, authorization, idempotencyKey, outboxIDs[index+1], job,
			); evidenceErr != nil {
				return domain.GraphResult{}, fmt.Errorf("record graph node %q: %w", node.Name, evidenceErr)
			}
		}
		for _, edge := range submission.Edges {
			outcomes := edge.Outcomes
			if outcomes == nil {
				outcomes = []string{}
			}
			if _, insertErr := tx.Exec(ctx, `
				INSERT INTO graph_edges (
					graph_id, upstream_job_id, downstream_job_id, predicate, outcomes
				) VALUES ($1, $2, $3, $4, $5)
			`, graphID, jobsByName[edge.From], jobsByName[edge.To], edge.Predicate, outcomes); insertErr != nil {
				return domain.GraphResult{}, fmt.Errorf("insert graph edge %q to %q: %w", edge.From, edge.To, insertErr)
			}
		}
		if completeErr := completeIdempotency(
			ctx, tx, authorization, submitGraphOperation, idempotencyKey, graphID, 201,
		); completeErr != nil {
			return domain.GraphResult{}, completeErr
		}
		if evidenceErr := insertGraphAcceptanceEvidence(
			ctx, tx, authorization, graphID, outboxIDs[0], idempotencyKey, submission,
		); evidenceErr != nil {
			return domain.GraphResult{}, evidenceErr
		}
		graph, getErr := getGraphByID(ctx, tx, authorization.namespaceID, graphID)

		return domain.GraphResult{Graph: graph}, getErr
	})
	if err != nil {
		return domain.GraphResult{}, fmt.Errorf("submit graph: %w", err)
	}

	return result, nil
}

func validateGraphSubmission(submission domain.GraphSubmission) error {
	if !domain.ValidName(submission.Namespace) || !domain.ValidName(submission.Name) ||
		len(submission.Nodes) == 0 || len(submission.Nodes) > 10_000 ||
		submission.MaxActive < 1 || submission.MaxActive > len(submission.Nodes) ||
		!slices.Contains([]string{"skip", "cancel", "blocked"}, submission.UnsatisfiedPolicy) ||
		!json.Valid(submission.RequestDocument) || submission.RequestDigest == "" {
		return errors.New("graph submission is invalid")
	}
	names := make(map[string]struct{}, len(submission.Nodes))
	for _, node := range submission.Nodes {
		if !domain.ValidName(node.Name) || node.Namespace != submission.Namespace || validateSubmission(node) != nil {
			return errors.New("graph node submission is invalid")
		}
		if _, exists := names[node.Name]; exists {
			return errors.New("graph node name is duplicated")
		}
		names[node.Name] = struct{}{}
	}
	seenEdges := make(map[string]struct{}, len(submission.Edges))
	for _, edge := range submission.Edges {
		_, fromExists := names[edge.From]
		_, toExists := names[edge.To]
		key := edge.From + "\x00" + edge.To
		if !fromExists || !toExists || edge.From == edge.To ||
			!slices.Contains([]string{"success", "failure", "any-terminal", "outcomes"}, edge.Predicate) {
			return errors.New("graph edge is invalid")
		}
		if _, exists := seenEdges[key]; exists {
			return errors.New("graph edge is duplicated")
		}
		seenEdges[key] = struct{}{}
	}

	return nil
}

func insertGraph(
	ctx context.Context,
	tx pgx.Tx,
	authorization namespaceAuthorization,
	graphID string,
	submission domain.GraphSubmission,
) error {
	labels := submission.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	encodedLabels, err := json.Marshal(labels)
	if err != nil {
		return fmt.Errorf("encode graph labels: %w", err)
	}
	if _, err = tx.Exec(ctx, `
		INSERT INTO graphs (
			id, namespace_id, owner_principal_id, name, labels,
			request_digest, request_document, max_active, unsatisfied_policy
		) VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7::jsonb, $8, $9)
	`, graphID, authorization.namespaceID, authorization.principalID,
		submission.Name, string(encodedLabels), submission.RequestDigest,
		string(submission.RequestDocument), submission.MaxActive,
		submission.UnsatisfiedPolicy); err != nil {
		return fmt.Errorf("insert graph: %w", err)
	}

	return nil
}

func insertGraphAcceptanceEvidence(
	ctx context.Context,
	tx pgx.Tx,
	authorization namespaceAuthorization,
	graphID, outboxID, idempotencyKey string,
	submission domain.GraphSubmission,
) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_events (
			namespace_id, actor_principal_id, action, resource_type,
			resource_id, request_digest, idempotency_key, details
		) VALUES ($1, $2, 'graph.accepted', 'graph', $3, $4, $5,
			jsonb_build_object('nodes', $6::integer, 'edges', $7::integer,
				'maxActive', $8::integer, 'unsatisfiedPolicy', $9::text))
	`, authorization.namespaceID, authorization.principalID, graphID,
		submission.RequestDigest, idempotencyKey, len(submission.Nodes),
		len(submission.Edges), submission.MaxActive, submission.UnsatisfiedPolicy); err != nil {
		return fmt.Errorf("audit graph acceptance: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox (id, namespace_id, topic, aggregate_type, aggregate_id, payload)
		VALUES ($1, $2, 'graph.accepted', 'graph', $3::uuid,
			jsonb_build_object('graphId', $3::text, 'nodes', $4::integer, 'edges', $5::integer))
	`, outboxID, authorization.namespaceID, graphID,
		len(submission.Nodes), len(submission.Edges)); err != nil {
		return fmt.Errorf("insert graph outbox event: %w", err)
	}

	return nil
}

// GetGraph returns one authorized graph aggregate and ordered node snapshots.
func (store *Store) GetGraph(
	ctx context.Context,
	principal domain.Principal,
	namespace, graphID string,
) (domain.Graph, error) {
	graph, err := scanGraph(store.pool.QueryRow(ctx, graphSelect+`
		JOIN memberships AS m ON m.namespace_id = n.id
		JOIN principals AS p ON p.id = m.principal_id
		WHERE p.issuer = $1 AND p.subject = $2 AND n.name = $3 AND g.id = $4
		GROUP BY g.id, n.name
	`, principal.Issuer, principal.Subject, namespace, graphID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Graph{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Graph{}, fmt.Errorf("get graph: %w", err)
	}
	if loadErr := loadGraphItems(ctx, store.pool, &graph); loadErr != nil {
		return domain.Graph{}, loadErr
	}

	return graph, nil
}

const graphSelect = `
	SELECT g.id::text, n.name, g.name, g.labels::text, g.max_active,
		g.unsatisfied_policy, g.revision, g.created_at, g.updated_at,
		count(j.id)::integer,
		count(j.id) FILTER (WHERE j.phase = 'accepted')::integer,
		count(j.id) FILTER (WHERE j.phase NOT IN ('accepted', 'terminal'))::integer,
		count(j.id) FILTER (WHERE j.phase = 'terminal')::integer,
		count(j.id) FILTER (WHERE j.phase = 'terminal' AND j.outcome = 'success')::integer,
		count(j.id) FILTER (WHERE j.phase = 'terminal' AND j.outcome = 'cancelled')::integer,
		count(j.id) FILTER (WHERE j.phase = 'terminal' AND j.graph_disposition = 'skipped')::integer,
		count(j.id) FILTER (WHERE j.phase = 'terminal' AND j.graph_disposition = 'blocked')::integer,
		count(j.id) FILTER (WHERE j.phase = 'terminal'
			AND j.outcome NOT IN ('success', 'cancelled') AND j.graph_disposition IS NULL)::integer
	FROM graphs AS g
	JOIN namespaces AS n ON n.id = g.namespace_id
	LEFT JOIN jobs AS j ON j.graph_id = g.id
`

func getGraphByID(ctx context.Context, tx pgx.Tx, namespaceID, graphID string) (domain.Graph, error) {
	graph, err := scanGraph(tx.QueryRow(ctx, graphSelect+`
		WHERE g.namespace_id = $1 AND g.id = $2 GROUP BY g.id, n.name
	`, namespaceID, graphID))
	if err != nil {
		return domain.Graph{}, err
	}
	if loadErr := loadGraphItems(ctx, tx, &graph); loadErr != nil {
		return domain.Graph{}, loadErr
	}

	return graph, nil
}

func scanGraph(row rowScanner) (domain.Graph, error) {
	var value domain.Graph
	var labels string
	if err := row.Scan(
		&value.ID, &value.Namespace, &value.Name, &labels, &value.MaxActive,
		&value.UnsatisfiedPolicy, &value.Revision, &value.CreatedAt, &value.UpdatedAt,
		&value.Total, &value.Waiting, &value.Active, &value.Terminal, &value.Succeeded,
		&value.Canceled, &value.Skipped, &value.Blocked, &value.Failed,
	); err != nil {
		return domain.Graph{}, err
	}
	if err := json.Unmarshal([]byte(labels), &value.Labels); err != nil {
		return domain.Graph{}, fmt.Errorf("decode graph labels: %w", err)
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
		case value.Failed+value.Skipped+value.Blocked > 0:
			value.Outcome = "failure"
		case value.Canceled > 0:
			value.Outcome = "cancelled" //nolint:misspell // Frozen v1alpha1 wire spelling.
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

func loadGraphItems(ctx context.Context, querier collectionQuerier, graph *domain.Graph) error {
	rows, err := querier.Query(ctx, `
		SELECT node.node_index, node.node_name, node.job_id::text,
			COALESCE(job.graph_disposition, '')
		FROM graph_nodes AS node JOIN jobs AS job ON job.id = node.job_id
		WHERE node.graph_id = $1 ORDER BY node.node_index
	`, graph.ID)
	if err != nil {
		return fmt.Errorf("list graph nodes: %w", err)
	}
	type identity struct {
		index                    int
		name, jobID, disposition string
	}
	identities := make([]identity, 0, graph.Total)
	for rows.Next() {
		var value identity
		if err = rows.Scan(&value.index, &value.name, &value.jobID, &value.disposition); err != nil {
			rows.Close()
			return fmt.Errorf("scan graph node: %w", err)
		}
		identities = append(identities, value)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("iterate graph nodes: %w", err)
	}
	graph.Items = make([]domain.GraphItem, 0, len(identities))
	for _, node := range identities {
		job, getErr := getJobWithQuerier(ctx, querier, graph.Namespace, node.jobID)
		if getErr != nil {
			return fmt.Errorf("load graph node %q: %w", node.name, getErr)
		}
		dependencies, getErr := loadGraphDependencies(ctx, querier, graph.ID, node.jobID)
		if getErr != nil {
			return fmt.Errorf("load graph node %q dependencies: %w", node.name, getErr)
		}
		graph.Items = append(graph.Items, domain.GraphItem{
			Index: node.index, Name: node.name, Disposition: node.disposition,
			Dependencies: dependencies, Job: job,
		})
	}

	return nil
}

func loadGraphDependencies(
	ctx context.Context,
	querier collectionQuerier,
	graphID, downstreamID string,
) ([]domain.GraphDependency, error) {
	rows, err := querier.Query(ctx, `
		SELECT upstream_node.node_name, edge.predicate, edge.outcomes,
			upstream.phase = 'terminal' AND (
				edge.predicate = 'any-terminal'
				OR edge.predicate = 'success' AND upstream.outcome = 'success'
				OR edge.predicate = 'failure' AND upstream.outcome IN ('failure', 'timed_out', 'aborted', 'lost')
				OR edge.predicate = 'outcomes' AND upstream.outcome = ANY(edge.outcomes)
			)
		FROM graph_edges AS edge
		JOIN jobs AS upstream ON upstream.id = edge.upstream_job_id
		JOIN graph_nodes AS upstream_node ON upstream_node.job_id = upstream.id
		WHERE edge.graph_id = $1 AND edge.downstream_job_id = $2
		ORDER BY upstream_node.node_index
	`, graphID, downstreamID)
	if err != nil {
		return nil, err
	}
	dependencies := make([]domain.GraphDependency, 0)
	for rows.Next() {
		var value domain.GraphDependency
		if err = rows.Scan(&value.From, &value.Predicate, &value.Outcomes, &value.Satisfied); err != nil {
			rows.Close()
			return nil, err
		}
		dependencies = append(dependencies, value)
	}
	err = rows.Err()
	rows.Close()

	return dependencies, err
}

// applyGraphCompletion resolves newly terminal unsatisfied descendants. The
// loop also propagates terminal dispositions through multiple DAG levels.
func (store *Store) applyGraphCompletion(
	ctx context.Context,
	tx pgx.Tx,
	namespaceID, completedJobID string,
) error {
	var graphID, policy string
	err := tx.QueryRow(ctx, `
		SELECT g.id::text, g.unsatisfied_policy
		FROM jobs AS j JOIN graphs AS g ON g.id = j.graph_id
		WHERE j.id = $1 AND j.namespace_id = $2 FOR UPDATE OF g
	`, completedJobID, namespaceID).Scan(&graphID, &policy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock completed graph: %w", err)
	}
	for {
		var jobID string
		queryErr := tx.QueryRow(ctx, `
			SELECT candidate.id::text
			FROM jobs AS candidate
			WHERE candidate.graph_id = $1 AND candidate.phase = 'accepted'
				AND EXISTS (
					SELECT 1 FROM graph_edges WHERE downstream_job_id = candidate.id
				)
				AND NOT EXISTS (
					SELECT 1 FROM graph_edges AS edge
					JOIN jobs AS upstream ON upstream.id = edge.upstream_job_id
					WHERE edge.downstream_job_id = candidate.id AND upstream.phase != 'terminal'
				)
				AND EXISTS (
					SELECT 1 FROM graph_edges AS edge
					JOIN jobs AS upstream ON upstream.id = edge.upstream_job_id
					WHERE edge.downstream_job_id = candidate.id AND NOT (
						edge.predicate = 'any-terminal'
						OR edge.predicate = 'success' AND upstream.outcome = 'success'
						OR edge.predicate = 'failure' AND upstream.outcome IN ('failure', 'timed_out', 'aborted', 'lost')
						OR edge.predicate = 'outcomes' AND upstream.outcome = ANY(edge.outcomes)
					)
				)
			ORDER BY candidate.graph_index LIMIT 1 FOR UPDATE OF candidate SKIP LOCKED
		`, graphID).Scan(&jobID)
		if errors.Is(queryErr, pgx.ErrNoRows) {
			break
		}
		if queryErr != nil {
			return fmt.Errorf("resolve graph readiness: %w", queryErr)
		}
		disposition, outcome, desiredState := policy, "aborted", "run"
		if policy == "skip" {
			disposition = "skipped"
		}
		if policy == "cancel" {
			disposition, outcome, desiredState = cancelledGraphWireName, cancelledGraphWireName, "cancel"
		}
		if _, queryErr = tx.Exec(ctx, `
			UPDATE jobs SET phase = 'terminal', desired_state = $2, outcome = $3,
				graph_disposition = $4, revision = revision + 1,
				updated_at = transaction_timestamp() WHERE id = $1
		`, jobID, desiredState, outcome, disposition); queryErr != nil {
			return fmt.Errorf("terminalize unsatisfied graph node: %w", queryErr)
		}
		if _, queryErr = tx.Exec(ctx, `
			INSERT INTO audit_events (
				namespace_id, actor_kind, action, resource_type, resource_id, details
			) VALUES ($1, 'system', 'graph.node.unsatisfied', 'job', $2,
				jsonb_build_object('graphId', $3::text, 'disposition', $4::text))
		`, namespaceID, jobID, graphID, disposition); queryErr != nil {
			return fmt.Errorf("audit unsatisfied graph node: %w", queryErr)
		}
	}
	if _, err = tx.Exec(ctx, `
		UPDATE graphs SET revision = revision + 1,
			updated_at = transaction_timestamp() WHERE id = $1
	`, graphID); err != nil {
		return fmt.Errorf("update completed graph: %w", err)
	}

	return nil
}

// CancelGraph durably applies ordinary per-job cancellation to every
// nonterminal node while preserving any completion that wins the race.
func (store *Store) CancelGraph(
	ctx context.Context,
	principal domain.Principal,
	namespace, graphID, idempotencyKey, requestDigest string,
) (domain.Graph, error) {
	result, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.Graph, error) {
		authorization, authErr := authorizeNamespace(ctx, tx, principal, namespace)
		if authErr != nil {
			return domain.Graph{}, authErr
		}
		var ownerID string
		queryErr := tx.QueryRow(ctx, `
			SELECT owner_principal_id::text FROM graphs
			WHERE id = $1 AND namespace_id = $2 FOR UPDATE
		`, graphID, authorization.namespaceID).Scan(&ownerID)
		if errors.Is(queryErr, pgx.ErrNoRows) {
			return domain.Graph{}, domain.ErrNotFound
		}
		if queryErr != nil {
			return domain.Graph{}, fmt.Errorf("lock graph cancellation: %w", queryErr)
		}
		if ownerID != authorization.principalID &&
			!slices.Contains([]string{domain.RoleOperator, domain.RoleNamespaceAdmin}, authorization.role) {
			return domain.Graph{}, domain.ErrForbidden
		}
		resourceID, replayed, reserveErr := reserveIdempotency(
			ctx, tx, authorization, "graphs.cancel", "graph", idempotencyKey, requestDigest, 200,
		)
		if reserveErr != nil {
			return domain.Graph{}, reserveErr
		}
		if replayed {
			if resourceID != graphID {
				return domain.Graph{}, domain.ErrIdempotencyConflict
			}

			return getGraphByID(ctx, tx, authorization.namespaceID, graphID)
		}
		rows, queryErr := tx.Query(ctx, `
			SELECT j.id::text, j.phase, COALESCE(e.id::text, ''), COALESCE(r.id::text, ''),
				COALESCE(e.agent_id::text, ''), COALESCE(e.phase, '')
			FROM jobs AS j
			LEFT JOIN runs AS r ON r.job_id = j.id
			LEFT JOIN executions AS e ON e.run_id = r.id
			WHERE j.graph_id = $1 AND j.phase != 'terminal'
			ORDER BY j.graph_index FOR UPDATE OF j
		`, graphID)
		if queryErr != nil {
			return domain.Graph{}, fmt.Errorf("list graph cancellation nodes: %w", queryErr)
		}
		var nodes []collectionSibling
		for rows.Next() {
			var node collectionSibling
			if queryErr = rows.Scan(
				&node.jobID, &node.phase, &node.executionID, &node.runID,
				&node.agentID, &node.executionPhase,
			); queryErr != nil {
				rows.Close()
				return domain.Graph{}, fmt.Errorf("scan graph cancellation node: %w", queryErr)
			}
			nodes = append(nodes, node)
		}
		queryErr = rows.Err()
		rows.Close()
		if queryErr != nil {
			return domain.Graph{}, fmt.Errorf("iterate graph cancellation nodes: %w", queryErr)
		}
		for _, node := range nodes {
			if cancelErr := store.cancelCollectionSibling(ctx, tx, authorization.namespaceID, node); cancelErr != nil {
				return domain.Graph{}, fmt.Errorf("cancel graph node: %w", cancelErr)
			}
		}
		if _, queryErr = tx.Exec(ctx, `
			UPDATE graphs SET revision = revision + 1,
				updated_at = transaction_timestamp() WHERE id = $1
		`, graphID); queryErr != nil {
			return domain.Graph{}, fmt.Errorf("update canceled graph: %w", queryErr)
		}
		if _, queryErr = tx.Exec(ctx, `
			INSERT INTO audit_events (
				namespace_id, actor_principal_id, action, resource_type,
				resource_id, request_digest, idempotency_key, details
			) VALUES ($1, $2, 'graph.cancelled', 'graph', $3, $4, $5,
				jsonb_build_object('nodes', $6::integer))
		`, authorization.namespaceID, authorization.principalID, graphID,
			requestDigest, idempotencyKey, len(nodes)); queryErr != nil {
			return domain.Graph{}, fmt.Errorf("audit graph cancellation: %w", queryErr)
		}
		if completeErr := completeIdempotency(
			ctx, tx, authorization, "graphs.cancel", idempotencyKey, graphID, 200,
		); completeErr != nil {
			return domain.Graph{}, completeErr
		}

		return getGraphByID(ctx, tx, authorization.namespaceID, graphID)
	})
	if err != nil {
		return domain.Graph{}, fmt.Errorf("cancel graph: %w", err)
	}

	return result, nil
}
