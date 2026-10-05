package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

// This is the same terminal predicate used by the graph coordinator. Waiting
// means evidence is nonterminal, not that Control inferred future satisfaction.
const edgeStateSQL = `CASE WHEN upstream.phase<>'terminal' THEN 'waiting'
 WHEN edge.predicate='any-terminal'
 OR (edge.predicate='success' AND upstream.outcome='success')
 OR (edge.predicate='failure' AND upstream.outcome IN ('failure','timed_out','aborted','lost'))
 OR (edge.predicate='outcomes' AND upstream.outcome=ANY(edge.outcomes)) THEN 'satisfied'
 ELSE 'unsatisfied' END`

const graphEdgesSelect = `SELECT source.node_name,destination.node_name,
 edge.upstream_job_id::text,edge.downstream_job_id::text,edge.predicate,edge.outcomes,
 upstream.phase,COALESCE(upstream.outcome,''),` + edgeStateSQL + `
 FROM graph_edges AS edge
 JOIN jobs AS upstream ON upstream.id=edge.upstream_job_id
 JOIN graph_nodes AS source ON source.graph_id=edge.graph_id AND source.job_id=edge.upstream_job_id
 JOIN graph_nodes AS destination ON destination.graph_id=edge.graph_id AND destination.job_id=edge.downstream_job_id `

// ListGraphNodes returns a bounded job page and complete incoming dependency
// counts. The job and predicate snapshots share one read-only transaction.
func (store *Store) ListGraphNodes(ctx context.Context, principal domain.Principal, namespace, id string, after, limit int) (domain.ResourcePage[domain.GraphNodeSnapshot], error) {
	if readErr := validateItemPage(after, limit); readErr != nil {
		return domain.ResourcePage[domain.GraphNodeSnapshot]{}, readErr
	}
	return inReadTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.ResourcePage[domain.GraphNodeSnapshot], error) {
		result := domain.ResourcePage[domain.GraphNodeSnapshot]{}
		authorization, err := authorizeNamespace(ctx, tx, principal, namespace, domain.CapabilityGroupsRead)
		if err != nil {
			return result, err
		}
		summary, err := graphSummaryByID(ctx, tx, authorization.namespaceID, id)
		if err != nil {
			return result, err
		}
		result.Total = summary.Total
		result.AsOf, err = querySnapshotTime(ctx, tx)
		if err != nil {
			return result, err
		}
		result.Items, err = graphNodeSnapshots(ctx, tx, authorization.namespaceID, id, after, limit+1, nil)
		if err != nil {
			return result, err
		}
		if len(result.Items) > limit {
			last := result.Items[limit-1].Index
			result.NextIndex = &last
			result.Items = result.Items[:limit]
		}
		return result, nil
	})
}

// graphNodeSnapshots reads at most limit jobs, then groups predicates only for
// those job IDs; no graph node causes an individual round trip.
func graphNodeSnapshots(ctx context.Context, tx pgx.Tx, namespaceID, graphID string, after, limit int, selected []string) ([]domain.GraphNodeSnapshot, error) {
	result := make([]domain.GraphNodeSnapshot, 0, limit)
	rows, err := tx.Query(ctx, jobSelect+` WHERE j.namespace_id=$1 AND j.graph_id=$2 AND j.graph_index>$3 AND ($4::uuid[] IS NULL OR j.id=ANY($4)) ORDER BY j.graph_index LIMIT $5`, namespaceID, graphID, after, selected, limit)
	if err != nil {
		return nil, fmt.Errorf("query graph nodes: %w", err)
	}
	ids := make([]string, 0, limit)
	positions := make(map[string]int, limit)
	for rows.Next() {
		job, scanErr := scanJob(rows)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		positions[job.ID] = len(result)
		ids = append(ids, job.ID)
		result = append(result, domain.GraphNodeSnapshot{Index: *job.Group.GraphIndex, Name: job.Name, Disposition: job.Group.GraphDisposition, Job: job})
	}
	rows.Close()
	if readErr := rows.Err(); readErr != nil {
		return nil, readErr
	}
	if len(ids) == 0 {
		return result, nil
	}
	rows, err = tx.Query(ctx, `SELECT destination::text,count(*),count(*) FILTER(WHERE state='satisfied'),count(*) FILTER(WHERE state='waiting'),count(*) FILTER(WHERE state='unsatisfied') FROM (SELECT edge.downstream_job_id AS destination,`+edgeStateSQL+` AS state FROM graph_edges AS edge JOIN jobs AS upstream ON upstream.id=edge.upstream_job_id WHERE edge.graph_id=$1 AND edge.downstream_job_id=ANY($2::uuid[])) AS predicates GROUP BY destination`, graphID, ids)
	if err != nil {
		return nil, fmt.Errorf("count graph predicates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var counts domain.DependencyCounts
		if readErr := rows.Scan(&id, &counts.Total, &counts.Satisfied, &counts.Waiting, &counts.Unsatisfied); readErr != nil {
			return nil, readErr
		}
		result[positions[id]].Dependencies = counts
	}
	return result, rows.Err()
}

func validGraphEdgeOptions(options domain.GraphEdgeOptions) bool {
	return options.Limit >= 1 && options.Limit <= 500 && slices.Contains([]string{"", "incoming", "outgoing"}, options.Direction) &&
		(options.NodeID == "" || domain.IsID(options.NodeID)) && (options.Direction == "" || options.NodeID != "") &&
		((options.AfterFromID == "" && options.AfterToID == "") || (domain.IsID(options.AfterFromID) && domain.IsID(options.AfterToID)))
}

// ListGraphEdges pages exact source predicates in immutable UUID-pair order.
func (store *Store) ListGraphEdges(ctx context.Context, principal domain.Principal, namespace, id string, options domain.GraphEdgeOptions) (domain.GraphEdgePage, error) {
	if !validGraphEdgeOptions(options) {
		return domain.GraphEdgePage{}, errors.New("graph dependency query is invalid")
	}
	return inReadTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.GraphEdgePage, error) {
		result := domain.GraphEdgePage{}
		authorization, err := authorizeNamespace(ctx, tx, principal, namespace, domain.CapabilityGroupsRead)
		if err != nil {
			return result, err
		}
		if _, err = graphSummaryByID(ctx, tx, authorization.namespaceID, id); err != nil {
			return result, err
		}
		if options.NodeID != "" {
			if readErr := assertGraphNode(ctx, tx, id, options.NodeID); readErr != nil {
				return result, readErr
			}
		}
		result.AsOf, err = querySnapshotTime(ctx, tx)
		if err != nil {
			return result, err
		}
		var nodeID, afterFrom, afterTo any
		if options.NodeID != "" {
			nodeID = options.NodeID
		}
		if options.AfterFromID != "" {
			afterFrom = options.AfterFromID
			afterTo = options.AfterToID
		}
		if readErr := tx.QueryRow(ctx, `SELECT count(*) FROM graph_edges AS edge WHERE edge.graph_id=$1 AND ($2::uuid IS NULL OR ($3::text<>'outgoing' AND edge.downstream_job_id=$2) OR ($3::text<>'incoming' AND edge.upstream_job_id=$2))`, id, nodeID, options.Direction).Scan(&result.Total); readErr != nil {
			return result, readErr
		}
		result.Items, err = readGraphEdges(ctx, tx, graphEdgesSelect+` WHERE edge.graph_id=$1
   AND ($2::uuid IS NULL OR ($3::text<>'outgoing' AND edge.downstream_job_id=$2) OR ($3::text<>'incoming' AND edge.upstream_job_id=$2))
   AND ($4::uuid IS NULL OR (edge.upstream_job_id,edge.downstream_job_id)>($4,$5::uuid))
   ORDER BY edge.upstream_job_id,edge.downstream_job_id LIMIT $6`, id, nodeID, options.Direction, afterFrom, afterTo, options.Limit+1)
		if err != nil {
			return result, err
		}
		if len(result.Items) > options.Limit {
			last := result.Items[options.Limit-1]
			result.NextFromID = last.FromJobID
			result.NextToID = last.ToJobID
			result.Items = result.Items[:options.Limit]
		}
		return result, nil
	})
}

func readGraphEdges(ctx context.Context, tx pgx.Tx, query string, args ...any) ([]domain.GraphEdgeSnapshot, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query graph dependencies: %w", err)
	}
	defer rows.Close()
	result := make([]domain.GraphEdgeSnapshot, 0)
	for rows.Next() {
		var edge domain.GraphEdgeSnapshot
		if readErr := rows.Scan(&edge.From, &edge.To, &edge.FromJobID, &edge.ToJobID, &edge.Predicate, &edge.Outcomes, &edge.UpstreamPhase, &edge.UpstreamOutcome, &edge.State); readErr != nil {
			return nil, readErr
		}
		result = append(result, edge)
	}
	return result, rows.Err()
}

func assertGraphNode(ctx context.Context, tx pgx.Tx, graphID, nodeID string) error {
	var exists bool
	if readErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM graph_nodes WHERE graph_id=$1 AND job_id=$2)`, graphID, nodeID).Scan(&exists); readErr != nil {
		return readErr
	}
	if !exists {
		return domain.ErrNotFound
	}
	return nil
}

const neighborhoodCTE = `WITH neighbors AS (
 SELECT $2::uuid AS id UNION SELECT upstream_job_id FROM graph_edges WHERE graph_id=$1 AND downstream_job_id=$2
 UNION SELECT downstream_job_id FROM graph_edges WHERE graph_id=$1 AND upstream_job_id=$2
) `

// GraphNeighborhood returns a one-hop induced neighborhood. The center is
// always included; all omission counts refer to the full one-hop neighborhood.
func (store *Store) GraphNeighborhood(ctx context.Context, principal domain.Principal, namespace, id, nodeID string, maxNodes, maxEdges int) (domain.GraphNeighborhood, error) {
	if !domain.IsID(nodeID) || maxNodes < 1 || maxNodes > 200 || maxEdges < 1 || maxEdges > 500 {
		return domain.GraphNeighborhood{}, errors.New("graph neighborhood query is invalid")
	}
	return inReadTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.GraphNeighborhood, error) {
		result := domain.GraphNeighborhood{CenterID: nodeID}
		authorization, err := authorizeNamespace(ctx, tx, principal, namespace, domain.CapabilityGroupsRead)
		if err != nil {
			return result, err
		}
		if _, err = graphSummaryByID(ctx, tx, authorization.namespaceID, id); err != nil {
			return result, err
		}
		if readErr := assertGraphNode(ctx, tx, id, nodeID); readErr != nil {
			return result, readErr
		}
		result.AsOf, err = querySnapshotTime(ctx, tx)
		if err != nil {
			return result, err
		}
		if err = tx.QueryRow(ctx, neighborhoodCTE+`SELECT (SELECT count(*) FROM neighbors),(SELECT count(*) FROM graph_edges WHERE graph_id=$1 AND upstream_job_id IN (SELECT id FROM neighbors) AND downstream_job_id IN (SELECT id FROM neighbors))`, id, nodeID).Scan(&result.TotalNodes, &result.TotalEdges); err != nil {
			return result, fmt.Errorf("count graph neighborhood: %w", err)
		}
		rows, err := tx.Query(ctx, neighborhoodCTE+`SELECT neighbors.id::text FROM neighbors JOIN graph_nodes AS node ON node.graph_id=$1 AND node.job_id=neighbors.id ORDER BY (neighbors.id=$2) DESC,node.node_index LIMIT $3`, id, nodeID, maxNodes)
		if err != nil {
			return result, err
		}
		ids := make([]string, 0, maxNodes)
		for rows.Next() {
			var selectedID string
			if readErr := rows.Scan(&selectedID); readErr != nil {
				rows.Close()
				return result, readErr
			}
			ids = append(ids, selectedID)
		}
		rows.Close()
		if readErr := rows.Err(); readErr != nil {
			return result, readErr
		}
		result.Nodes, err = graphNodeSnapshots(ctx, tx, authorization.namespaceID, id, -1, maxNodes, ids)
		if err != nil {
			return result, err
		}
		result.Edges, err = readGraphEdges(ctx, tx, graphEdgesSelect+` WHERE edge.graph_id=$1 AND edge.upstream_job_id=ANY($2::uuid[]) AND edge.downstream_job_id=ANY($2::uuid[]) ORDER BY edge.upstream_job_id,edge.downstream_job_id LIMIT $3`, id, ids, maxEdges)
		if err != nil {
			return result, err
		}
		result.OmittedNodes = result.TotalNodes - len(result.Nodes)
		result.OmittedEdges = result.TotalEdges - len(result.Edges)
		return result, nil
	})
}
