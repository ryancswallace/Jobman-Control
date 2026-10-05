package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func groupPageBounds(options domain.GroupListOptions) (beforeTime, beforeID any, err error) {
	if options.Limit < 1 || options.Limit > domain.MaximumJobListLimit || !slices.Contains([]string{"", "individual", "slurm-array"}, options.ArrayMode) {
		return nil, nil, errors.New("group query is invalid")
	}
	if options.Before == nil {
		return nil, nil, nil
	}
	if options.Before.CreatedAt.IsZero() || !domain.IsID(options.Before.ID) {
		return nil, nil, errors.New("group cursor is invalid")
	}
	return options.Before.CreatedAt.UTC(), options.Before.ID, nil
}

// ListCollections returns complete summaries without loading child rows.
func (store *Store) ListCollections(ctx context.Context, principal domain.Principal, namespace string, options domain.GroupListOptions) (domain.ResourcePage[domain.Collection], error) {
	beforeTime, beforeID, err := groupPageBounds(options)
	if err != nil {
		return domain.ResourcePage[domain.Collection]{}, err
	}
	return inReadTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.ResourcePage[domain.Collection], error) {
		authorization, authErr := authorizeNamespace(ctx, tx, principal, namespace, domain.CapabilityGroupsRead)
		if authErr != nil {
			return domain.ResourcePage[domain.Collection]{}, authErr
		}
		result := domain.ResourcePage[domain.Collection]{Items: make([]domain.Collection, 0, options.Limit)}
		if timeErr := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&result.AsOf); timeErr != nil {
			return result, timeErr
		}
		if countErr := tx.QueryRow(ctx, `SELECT count(*) FROM collections WHERE namespace_id=$1 AND ($2::timestamptz IS NULL OR created_at<=$2) AND ($3::text='' OR array_mode=$3)`, authorization.namespaceID, options.CreatedBefore, options.ArrayMode).Scan(&result.Total); countErr != nil {
			return result, countErr
		}
		rows, queryErr := tx.Query(ctx, `WITH selected AS (
 SELECT id FROM collections WHERE namespace_id=$1
 AND ($2::timestamptz IS NULL OR (created_at,id)<($2,$3::uuid))
 AND ($4::timestamptz IS NULL OR created_at<=$4) AND ($5::text='' OR array_mode=$5)
 ORDER BY created_at DESC,id DESC LIMIT $6
 ) `+collectionSelect+` JOIN selected ON selected.id=c.id
 GROUP BY c.id,n.name ORDER BY c.created_at DESC,c.id DESC`, authorization.namespaceID, beforeTime, beforeID, options.CreatedBefore, options.ArrayMode, options.Limit+1)
		if queryErr != nil {
			return result, fmt.Errorf("list collection catalog: %w", queryErr)
		}
		defer rows.Close()
		for rows.Next() {
			item, scanErr := scanCollection(rows)
			if scanErr != nil {
				return result, scanErr
			}
			result.Items = append(result.Items, item)
		}
		if queryErr := rows.Err(); queryErr != nil {
			return result, queryErr
		}
		if len(result.Items) > options.Limit {
			last := result.Items[options.Limit-1]
			result.NextCursor = &domain.JobCursor{CreatedAt: last.CreatedAt, ID: last.ID}
			result.Items = result.Items[:options.Limit]
		}
		result.AsOf = result.AsOf.UTC()
		return result, nil
	})
}

// CollectionSummary reads one authorized complete aggregate without its children.
func (store *Store) CollectionSummary(ctx context.Context, principal domain.Principal, namespace, id string) (domain.CollectionSnapshot, error) {
	return inReadTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.CollectionSnapshot, error) {
		var result domain.CollectionSnapshot
		authorization, err := authorizeNamespace(ctx, tx, principal, namespace, domain.CapabilityGroupsRead)
		if err != nil {
			return result, err
		}
		if readErr := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&result.AsOf); readErr != nil {
			return result, readErr
		}
		result.Collection, err = collectionSummaryByID(ctx, tx, authorization.namespaceID, id)
		result.AsOf = result.AsOf.UTC()
		return result, err
	})
}

func collectionSummaryByID(ctx context.Context, tx pgx.Tx, namespaceID, id string) (domain.Collection, error) {
	result, err := scanCollection(tx.QueryRow(ctx, collectionSelect+` WHERE c.namespace_id=$1 AND c.id=$2 GROUP BY c.id,n.name`, namespaceID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return result, domain.ErrNotFound
	}
	return result, err
}

// ListGraphs returns bounded complete graph summaries, never the full node set.
func (store *Store) ListGraphs(ctx context.Context, principal domain.Principal, namespace string, options domain.GroupListOptions) (domain.ResourcePage[domain.Graph], error) {
	beforeTime, beforeID, err := groupPageBounds(options)
	if err != nil || options.ArrayMode != "" {
		return domain.ResourcePage[domain.Graph]{}, errors.New("graph query is invalid")
	}
	return inReadTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.ResourcePage[domain.Graph], error) {
		authorization, authErr := authorizeNamespace(ctx, tx, principal, namespace, domain.CapabilityGroupsRead)
		if authErr != nil {
			return domain.ResourcePage[domain.Graph]{}, authErr
		}
		result := domain.ResourcePage[domain.Graph]{Items: make([]domain.Graph, 0, options.Limit)}
		if timeErr := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&result.AsOf); timeErr != nil {
			return result, timeErr
		}
		if countErr := tx.QueryRow(ctx, `SELECT count(*) FROM graphs WHERE namespace_id=$1 AND ($2::timestamptz IS NULL OR created_at<=$2)`, authorization.namespaceID, options.CreatedBefore).Scan(&result.Total); countErr != nil {
			return result, countErr
		}
		rows, queryErr := tx.Query(ctx, `WITH selected AS (
 SELECT id FROM graphs WHERE namespace_id=$1
 AND ($2::timestamptz IS NULL OR (created_at,id)<($2,$3::uuid))
 AND ($4::timestamptz IS NULL OR created_at<=$4)
 ORDER BY created_at DESC,id DESC LIMIT $5
 ) `+graphSelect+` JOIN selected ON selected.id=g.id
 GROUP BY g.id,n.name ORDER BY g.created_at DESC,g.id DESC`, authorization.namespaceID, beforeTime, beforeID, options.CreatedBefore, options.Limit+1)
		if queryErr != nil {
			return result, fmt.Errorf("list graph catalog: %w", queryErr)
		}
		defer rows.Close()
		for rows.Next() {
			item, scanErr := scanGraph(rows)
			if scanErr != nil {
				return result, scanErr
			}
			result.Items = append(result.Items, item)
		}
		if queryErr := rows.Err(); queryErr != nil {
			return result, queryErr
		}
		if len(result.Items) > options.Limit {
			last := result.Items[options.Limit-1]
			result.NextCursor = &domain.JobCursor{CreatedAt: last.CreatedAt, ID: last.ID}
			result.Items = result.Items[:options.Limit]
		}
		result.AsOf = result.AsOf.UTC()
		return result, nil
	})
}

// GraphSummary reads one authorized complete graph aggregate without nodes.
func (store *Store) GraphSummary(ctx context.Context, principal domain.Principal, namespace, id string) (domain.GraphSnapshot, error) {
	return inReadTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.GraphSnapshot, error) {
		var result domain.GraphSnapshot
		authorization, err := authorizeNamespace(ctx, tx, principal, namespace, domain.CapabilityGroupsRead)
		if err != nil {
			return result, err
		}
		if readErr := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&result.AsOf); readErr != nil {
			return result, readErr
		}
		result.Graph, err = graphSummaryByID(ctx, tx, authorization.namespaceID, id)
		result.AsOf = result.AsOf.UTC()
		return result, err
	})
}

func graphSummaryByID(ctx context.Context, tx pgx.Tx, namespaceID, id string) (domain.Graph, error) {
	result, err := scanGraph(tx.QueryRow(ctx, graphSelect+` WHERE g.namespace_id=$1 AND g.id=$2 GROUP BY g.id,n.name`, namespaceID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return result, domain.ErrNotFound
	}
	return result, err
}

func validateItemPage(after, limit int) error {
	if after < -1 || after > 9999 || limit < 1 || limit > domain.MaximumJobListLimit {
		return errors.New("group item page is invalid")
	}
	return nil
}

// ListCollectionItems joins one bounded child page into factual job snapshots.
func (store *Store) ListCollectionItems(ctx context.Context, principal domain.Principal, namespace, id string, after, limit int) (domain.ResourcePage[domain.CollectionItem], error) {
	if readErr := validateItemPage(after, limit); readErr != nil {
		return domain.ResourcePage[domain.CollectionItem]{}, readErr
	}
	return inReadTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.ResourcePage[domain.CollectionItem], error) {
		result := domain.ResourcePage[domain.CollectionItem]{Items: make([]domain.CollectionItem, 0, limit)}
		authorization, err := authorizeNamespace(ctx, tx, principal, namespace, domain.CapabilityGroupsRead)
		if err != nil {
			return result, err
		}
		summary, err := collectionSummaryByID(ctx, tx, authorization.namespaceID, id)
		if err != nil {
			return result, err
		}
		if readErr := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&result.AsOf); readErr != nil {
			return result, readErr
		}
		result.Total = summary.Total
		rows, err := tx.Query(ctx, jobSelect+` WHERE j.namespace_id=$1 AND j.collection_id=$2 AND j.collection_index>$3 ORDER BY j.collection_index LIMIT $4`, authorization.namespaceID, id, after, limit+1)
		if err != nil {
			return result, fmt.Errorf("list collection children: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			job, scanErr := scanJob(rows)
			if scanErr != nil {
				return result, scanErr
			}
			item := domain.CollectionItem{Index: *job.Group.CollectionIndex, Name: job.Name, Job: job}
			if summary.ArrayMode == "slurm-array" {
				index := item.Index
				item.ArrayTaskIndex = &index
			}
			result.Items = append(result.Items, item)
		}
		if readErr := rows.Err(); readErr != nil {
			return result, readErr
		}
		if len(result.Items) > limit {
			last := result.Items[limit-1].Index
			result.NextIndex = &last
			result.Items = result.Items[:limit]
		}
		result.AsOf = result.AsOf.UTC()
		return result, nil
	})
}

// querySnapshotTime reads the database transaction time in UTC.
func querySnapshotTime(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var now time.Time
	err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now)
	return now.UTC(), err
}
