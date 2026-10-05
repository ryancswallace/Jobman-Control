package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

const maximumTargetItemsJSON = 2 << 20

func readNamespaceAuthority(ctx context.Context, tx pgx.Tx, authorization namespaceAuthorization) (domain.NamespaceReadAuthority, error) {
	var result domain.NamespaceReadAuthority
	err := tx.QueryRow(ctx, `SELECT transaction_timestamp(),n.name,n.id::text,recovery.restore_epoch,v.revision,transaction_timestamp(),
 CASE WHEN managed.namespace_id IS NULL THEN NULL ELSE LEAST(managed.last_verified_at,account.last_verified_at)+interval '120 seconds' END
 FROM namespaces n JOIN authorization_versions v ON v.namespace_id=n.id AND v.principal_id=$2
 CROSS JOIN service_recovery_state recovery
 LEFT JOIN namespace_directory_state managed ON managed.namespace_id=n.id
 LEFT JOIN directory_accounts account ON account.principal_id=v.principal_id
 WHERE n.id=$1 AND recovery.singleton`, authorization.namespaceID, authorization.principalID).Scan(&result.AsOf, &result.Namespace, &result.NamespaceID, &result.RecoveryEpoch, &result.AuthorizationVersion, &result.AuthorizationCheckedAt, &result.AuthorizationExpiresAt)
	return result, err
}

// ListTargetCatalog selects a bounded target page before loading configuration.
func (store *Store) ListTargetCatalog(ctx context.Context, principal domain.Principal, namespace string, options domain.TargetCatalogOptions) (domain.TargetCatalog, error) {
	if options.Limit < 1 || options.Limit > domain.MaximumJobListLimit || options.CreatedBefore != nil && options.CreatedBefore.IsZero() || options.Before != nil && (options.Before.CreatedAt.IsZero() || !domain.IsID(options.Before.ID) || options.CreatedBefore == nil || options.Before.CreatedAt.After(*options.CreatedBefore)) {
		return domain.TargetCatalog{}, errors.New("invalid target catalog query")
	}
	return inReadTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.TargetCatalog, error) {
		result := domain.TargetCatalog{Items: []domain.CatalogTarget{}}
		authorization, err := authorizeNamespace(ctx, tx, principal, namespace, domain.CapabilityTargetsRead)
		if err != nil {
			return result, err
		}
		result.NamespaceReadAuthority, err = readNamespaceAuthority(ctx, tx, authorization)
		if err != nil {
			return result, err
		}
		// A visible creation watermark, together with the transaction-serialized
		// creation clock, excludes inserts that commit after this snapshot.
		// A wall-clock cutoff alone admits older, still-uncommitted inserts.
		if cutoffErr := tx.QueryRow(ctx, `SELECT COALESCE(max(created_at),'epoch'::timestamptz) FROM targets WHERE namespace_id=$1`, authorization.namespaceID).Scan(&result.CreatedBefore); cutoffErr != nil {
			return result, cutoffErr
		}
		if options.CreatedBefore != nil && options.CreatedBefore.Before(result.CreatedBefore) {
			result.CreatedBefore = options.CreatedBefore.UTC()
		}
		if countErr := tx.QueryRow(ctx, `SELECT count(*) FROM targets WHERE namespace_id=$1 AND created_at<=$2`, authorization.namespaceID, result.CreatedBefore).Scan(&result.Total); countErr != nil {
			return result, countErr
		}
		var beforeTime, beforeID any
		if options.Before != nil {
			beforeTime, beforeID = options.Before.CreatedAt.UTC(), options.Before.ID
		}
		rows, err := tx.Query(ctx, `WITH selected AS (SELECT id FROM targets WHERE namespace_id=$1 AND created_at<=$2
 AND ($3::timestamptz IS NULL OR (created_at,id)<($3,$4::uuid)) ORDER BY created_at DESC,id DESC LIMIT $5)
 `+targetSelect+` JOIN selected ON selected.id=t.id ORDER BY t.created_at DESC,t.id DESC`, authorization.namespaceID, result.CreatedBefore, beforeTime, beforeID, options.Limit+1)
		if err != nil {
			return result, err
		}
		targets := make([]domain.Target, 0, options.Limit+1)
		for rows.Next() {
			target, scanErr := scanTarget(rows)
			if scanErr != nil {
				rows.Close()
				return result, scanErr
			}
			targets = append(targets, target)
		}
		rows.Close()
		if rowsErr := rows.Err(); rowsErr != nil {
			return result, rowsErr
		}
		partitionCounts, partitionErr := loadCatalogPartitions(ctx, tx, targets)
		if partitionErr != nil {
			return result, partitionErr
		}
		encodedBytes := 2
		for _, target := range targets {
			item := catalogTarget(target)
			item.Generation.PartitionCount = partitionCounts[target.GenerationID]
			item.Generation.PartitionsTruncated = item.Generation.PartitionCount > int64(len(item.Generation.Partitions))
			encoded, encodeErr := json.Marshal(item)
			if encodeErr != nil || len(encoded)+2 > maximumTargetItemsJSON {
				return result, domain.ErrConflict
			}
			if len(result.Items) == options.Limit || len(result.Items) > 0 && encodedBytes+len(encoded)+1 > maximumTargetItemsJSON {
				last := result.Items[len(result.Items)-1]
				result.NextCursor = &domain.JobCursor{CreatedAt: last.CreatedAt, ID: last.ID}
				break
			}
			encodedBytes += len(encoded) + 1
			result.Items = append(result.Items, item)
		}
		return result, nil
	})
}

// GetTargetSnapshot authorizes the namespace and resolves the immutable UUID.
func (store *Store) GetTargetSnapshot(ctx context.Context, principal domain.Principal, namespace, id string) (domain.TargetSnapshot, error) {
	if !domain.IsID(id) {
		return domain.TargetSnapshot{}, errors.New("invalid target ID")
	}
	return inReadTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.TargetSnapshot, error) {
		var result domain.TargetSnapshot
		authorization, err := authorizeNamespace(ctx, tx, principal, namespace, domain.CapabilityTargetsRead)
		if err != nil {
			return result, err
		}
		result.NamespaceReadAuthority, err = readNamespaceAuthority(ctx, tx, authorization)
		if err != nil {
			return result, err
		}
		target, err := scanTarget(tx.QueryRow(ctx, targetSelect+` WHERE t.namespace_id=$1 AND t.id=$2`, authorization.namespaceID, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return result, domain.ErrNotFound
		}
		if err != nil {
			return result, err
		}
		targets := []domain.Target{target}
		partitionCounts, partitionErr := loadCatalogPartitions(ctx, tx, targets)
		if partitionErr != nil {
			return result, partitionErr
		}
		result.Target = catalogTarget(targets[0])
		result.Target.Generation.PartitionCount = partitionCounts[target.GenerationID]
		result.Target.Generation.PartitionsTruncated = result.Target.Generation.PartitionCount > int64(len(result.Target.Generation.Partitions))
		return result, nil
	})
}

func loadCatalogPartitions(ctx context.Context, tx pgx.Tx, targets []domain.Target) (map[string]int64, error) {
	totals := map[string]int64{}
	if len(targets) == 0 {
		return totals, nil
	}
	ids := make([]string, 0, len(targets))
	indexes := map[string]int{}
	for index := range targets {
		ids = append(ids, targets[index].GenerationID)
		indexes[targets[index].GenerationID] = index
		targets[index].Partitions = []domain.PartitionSpec{}
	}
	counts, err := tx.Query(ctx, `SELECT target_generation_id::text,count(*) FROM partitions WHERE target_generation_id=ANY($1::uuid[]) AND state!='retired' GROUP BY target_generation_id`, ids)
	if err != nil {
		return nil, err
	}
	for counts.Next() {
		var id string
		var count int64
		if scanErr := counts.Scan(&id, &count); scanErr != nil {
			counts.Close()
			return nil, scanErr
		}
		totals[id] = count
	}
	counts.Close()
	if rowsErr := counts.Err(); rowsErr != nil {
		return nil, rowsErr
	}
	rows, err := tx.Query(ctx, `SELECT generation.id::text,p.name,p.is_default FROM unnest($1::uuid[]) AS generation(id)
 JOIN LATERAL (SELECT name,is_default FROM partitions WHERE target_generation_id=generation.id AND state!='retired' ORDER BY name COLLATE "C" LIMIT 200) p ON true
 ORDER BY generation.id,p.name COLLATE "C"`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var partition domain.PartitionSpec
		if scanErr := rows.Scan(&id, &partition.Name, &partition.IsDefault); scanErr != nil {
			return nil, scanErr
		}
		index, ok := indexes[id]
		if !ok {
			return nil, fmt.Errorf("partition outside selected generation")
		}
		targets[index].Partitions = append(targets[index].Partitions, partition)
	}
	return totals, rows.Err()
}

// ListTargetPartitions rejects changed generations and reauthorizes every page.
func (store *Store) ListTargetPartitions(ctx context.Context, principal domain.Principal, namespace, id string, options domain.TargetPartitionOptions) (domain.TargetPartitionPage, error) {
	if !domain.IsID(id) || !domain.IsID(options.GenerationID) || options.Limit < 1 || options.Limit > 200 || options.AfterName != "" && !domain.ValidName(options.AfterName) {
		return domain.TargetPartitionPage{}, errors.New("invalid target partition query")
	}
	return inReadTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.TargetPartitionPage, error) {
		result := domain.TargetPartitionPage{TargetID: id, GenerationID: options.GenerationID, Items: []domain.PartitionSpec{}}
		authorization, err := authorizeNamespace(ctx, tx, principal, namespace, domain.CapabilityTargetsRead)
		if err != nil {
			return result, err
		}
		result.NamespaceReadAuthority, err = readNamespaceAuthority(ctx, tx, authorization)
		if err != nil {
			return result, err
		}
		var current string
		err = tx.QueryRow(ctx, `SELECT current_generation_id::text FROM targets WHERE namespace_id=$1 AND id=$2`, authorization.namespaceID, id).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			return result, domain.ErrNotFound
		}
		if err != nil {
			return result, err
		}
		if current != options.GenerationID {
			return result, domain.ErrConflict
		}
		if countErr := tx.QueryRow(ctx, `SELECT count(*) FROM partitions WHERE target_generation_id=$1 AND state!='retired'`, current).Scan(&result.Total); countErr != nil {
			return result, countErr
		}
		rows, err := tx.Query(ctx, `SELECT name,is_default FROM partitions WHERE target_generation_id=$1 AND state!='retired' AND name COLLATE "C">$2::text COLLATE "C" ORDER BY name COLLATE "C" LIMIT $3`, current, options.AfterName, options.Limit+1)
		if err != nil {
			return result, err
		}
		defer rows.Close()
		for rows.Next() {
			var partition domain.PartitionSpec
			if scanErr := rows.Scan(&partition.Name, &partition.IsDefault); scanErr != nil {
				return result, scanErr
			}
			result.Items = append(result.Items, partition)
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			return result, rowsErr
		}
		if len(result.Items) > options.Limit {
			result.Items = result.Items[:options.Limit]
			result.NextName = result.Items[len(result.Items)-1].Name
		}
		return result, nil
	})
}

func catalogTarget(target domain.Target) domain.CatalogTarget {
	generation := domain.CatalogTargetGeneration{ID: target.GenerationID, Number: target.Generation, ExecutionBackend: target.ExecutionBackend, Transport: target.Transport, Runtimes: nonNilStrings(target.Runtimes), OperatingSystems: nonNilStrings(target.OperatingSystems), Architectures: nonNilStrings(target.Architectures), Capabilities: nonNilStrings(target.Capabilities), Partitions: target.Partitions, ArtifactStores: []domain.CatalogStoreReference{}, Provider: target.Provider}
	if generation.Partitions == nil {
		generation.Partitions = []domain.PartitionSpec{}
	}
	if target.LogStoreName != "" {
		generation.LogStore = &domain.CatalogStoreReference{Name: target.LogStoreName, Version: target.LogStoreVersion}
	}
	for _, store := range target.ArtifactStores {
		generation.ArtifactStores = append(generation.ArtifactStores, domain.CatalogStoreReference(store))
	}
	return domain.CatalogTarget{ID: target.ID, Name: target.Name, Kind: target.Kind, State: target.State, Revision: target.Revision, CreatedAt: target.CreatedAt.UTC(), UpdatedAt: target.UpdatedAt.UTC(), Generation: generation}
}
