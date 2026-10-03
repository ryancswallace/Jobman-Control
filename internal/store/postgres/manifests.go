package postgres

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"

	"github.com/jackc/pgx/v5"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

// Leave room for the small authority/envelope fields under downstream 4 MiB limits.
const maximumArtifactItemsJSON = 2 << 20

var manifestChecksumPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func readManifestAuthority(ctx context.Context, tx pgx.Tx, authorization namespaceAuthorization, jobID string) (domain.ManifestAuthority, error) {
	var result domain.ManifestAuthority
	err := tx.QueryRow(ctx, `SELECT transaction_timestamp(),n.name,n.id::text,j.id::text,recovery.restore_epoch,v.revision,transaction_timestamp(),
 CASE WHEN managed.namespace_id IS NULL THEN NULL ELSE LEAST(managed.last_verified_at,account.last_verified_at)+interval '120 seconds' END
 FROM jobs j JOIN namespaces n ON n.id=j.namespace_id
 JOIN authorization_versions v ON v.namespace_id=j.namespace_id AND v.principal_id=$3
 CROSS JOIN service_recovery_state recovery
 LEFT JOIN namespace_directory_state managed ON managed.namespace_id=n.id
 LEFT JOIN directory_accounts account ON account.principal_id=v.principal_id
 WHERE j.id=$1 AND j.namespace_id=$2 AND recovery.singleton`, jobID, authorization.namespaceID, authorization.principalID).Scan(&result.AsOf, &result.Namespace, &result.NamespaceID, &result.JobID, &result.RecoveryEpoch, &result.AuthorizationVersion, &result.AuthorizationCheckedAt, &result.AuthorizationExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, domain.ErrNotFound
	}
	return result, err
}

// ListLogChunks reads bounded immutable metadata after a current scope check.
func (store *Store) ListLogChunks(ctx context.Context, principal domain.Principal, namespace, jobID string, options domain.LogChunkOptions) (domain.LogChunkPage, error) {
	selectors := 0
	if options.TailBytes > 0 {
		selectors++
	}
	if options.FromOffset != nil {
		selectors++
	}
	if options.AfterSequence != nil {
		selectors++
	}
	if !domain.IsID(jobID) || options.Limit < 1 || options.Limit > 100 || options.RunNumber < 0 || (options.ExecutionID != "" && !domain.IsID(options.ExecutionID)) || (options.Stream != "stdout" && options.Stream != "stderr") || options.TailBytes < 0 || options.TailBytes > 262144 || selectors > 1 || (options.FromOffset != nil && *options.FromOffset < 0) || (options.AfterSequence != nil && *options.AfterSequence < 0) {
		return domain.LogChunkPage{}, errors.New("log manifest query is invalid")
	}
	if selectors == 0 {
		options.TailBytes = 65536
	}
	return inReadTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.LogChunkPage, error) {
		result := domain.LogChunkPage{Stream: options.Stream, State: "not_captured", Chunks: []domain.BoundedLogChunk{}}
		authorization, err := authorizeNamespace(ctx, tx, principal, namespace, domain.CapabilityLogsRead)
		if err != nil {
			return result, err
		}
		result.ManifestAuthority, err = readManifestAuthority(ctx, tx, authorization, jobID)
		if err != nil {
			return result, err
		}
		var approvedStore string
		var approvedVersion int64
		err = tx.QueryRow(ctx, `SELECT r.id::text,r.run_number,COALESCE(e.id::text,''),COALESCE(e.target_generation_id::text,''),
 COALESCE(s.state,'not_captured'),COALESCE(s.manifest_revision,0),COALESCE(s.byte_length,0),COALESCE(s.last_sequence,0),COALESCE(s.truncated,false)
 ,COALESCE(tg.log_store_name,''),COALESCE(tg.log_store_version,0)
 FROM runs r LEFT JOIN executions e ON e.run_id=r.id
 LEFT JOIN target_generations tg ON tg.id=e.target_generation_id
 LEFT JOIN log_streams s ON s.execution_id=e.id AND s.stream=$3
 WHERE r.job_id=$1 AND ($2::bigint=0 OR r.run_number=$2) ORDER BY r.run_number DESC LIMIT 1`, jobID, options.RunNumber, options.Stream).Scan(&result.RunID, &result.RunNumber, &result.ExecutionID, &result.TargetGenerationID, &result.State, &result.ManifestRevision, &result.ByteLength, &result.LastSequence, &result.Truncated, &approvedStore, &approvedVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			if options.RunNumber != 0 || options.ExecutionID != "" {
				return result, domain.ErrNotFound
			}
			if options.FromOffset != nil && *options.FromOffset > 0 || options.AfterSequence != nil && *options.AfterSequence > 0 {
				return result, domain.ErrConflict
			}
			return result, nil
		}
		if err != nil {
			return result, err
		}
		if options.ExecutionID != "" && options.ExecutionID != result.ExecutionID {
			return result, domain.ErrNotFound
		}
		if result.State == "capturing" {
			result.State = "open"
		}
		if result.State == "not_captured" {
			if options.FromOffset != nil && *options.FromOffset > 0 || options.AfterSequence != nil && *options.AfterSequence > 0 {
				return result, domain.ErrConflict
			}
			return result, nil
		}
		if options.FromOffset != nil {
			result.FromOffset = *options.FromOffset
		} else if options.TailBytes > 0 {
			result.FromOffset = max(0, result.ByteLength-options.TailBytes)
		}
		if result.FromOffset > result.ByteLength {
			return result, domain.ErrConflict
		}
		startSequence := int64(1)
		if options.AfterSequence != nil {
			if *options.AfterSequence > result.LastSequence {
				return result, domain.ErrConflict
			}
			if *options.AfterSequence == math.MaxInt64 {
				return result, nil
			}
			startSequence = *options.AfterSequence + 1
		} else if result.LastSequence > 0 {
			err = tx.QueryRow(ctx, `SELECT sequence FROM log_chunks WHERE execution_id=$1 AND stream=$2 AND sequence<=$3 AND byte_offset<=$4 ORDER BY byte_offset DESC,sequence DESC LIMIT 1`, result.ExecutionID, result.Stream, result.LastSequence, result.FromOffset).Scan(&startSequence)
			if err != nil {
				return result, fmt.Errorf("seek contiguous log manifest: %w", err)
			}
		}
		rows, err := tx.Query(ctx, `SELECT sequence,byte_offset,byte_length,checksum,store_name,store_version,object_key,captured_at,complete,truncated
 FROM log_chunks WHERE execution_id=$1 AND stream=$2 AND sequence>=$3 AND sequence<=$4
 AND ($5::boolean OR byte_offset+byte_length>$6 OR (byte_length=0 AND complete AND byte_offset>=$6)) ORDER BY sequence LIMIT $7`, result.ExecutionID, result.Stream, startSequence, result.LastSequence, options.AfterSequence != nil, result.FromOffset, options.Limit+1)
		if err != nil {
			return result, err
		}
		defer rows.Close()
		for rows.Next() {
			var chunk domain.BoundedLogChunk
			if scanErr := rows.Scan(&chunk.Sequence, &chunk.ByteOffset, &chunk.ByteLength, &chunk.Checksum, &chunk.StoreName, &chunk.StoreVersion, &chunk.ObjectKey, &chunk.CapturedAt, &chunk.Complete, &chunk.Truncated); scanErr != nil {
				return result, scanErr
			}
			if !validBoundedLogChunk(result, chunk) || chunk.StoreName != approvedStore || chunk.StoreVersion != approvedVersion || chunk.CapturedAt.IsZero() {
				return result, domain.ErrConflict
			}
			if len(result.Chunks) == 0 && chunk.Sequence != startSequence {
				return result, domain.ErrConflict
			}
			if len(result.Chunks) > 0 {
				previous := result.Chunks[len(result.Chunks)-1]
				if chunk.Sequence != previous.Sequence+1 || chunk.ByteOffset != previous.ByteOffset+previous.ByteLength || previous.Complete {
					return result, domain.ErrConflict
				}
			}
			chunk.CapturedAt = chunk.CapturedAt.UTC()
			result.Chunks = append(result.Chunks, chunk)
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			return result, rowsErr
		}
		if len(result.Chunks) > options.Limit {
			result.Chunks = result.Chunks[:options.Limit]
			next := result.Chunks[len(result.Chunks)-1].Sequence
			result.NextAfterSequence = &next
		}
		if options.AfterSequence != nil {
			result.FromOffset = result.ByteLength
			if len(result.Chunks) > 0 {
				result.FromOffset = result.Chunks[0].ByteOffset
			}
		}
		return result, nil
	})
}

func validBoundedLogChunk(page domain.LogChunkPage, chunk domain.BoundedLogChunk) bool {
	expected := fmt.Sprintf("namespaces/%s/jobs/%s/executions/%s/logs/%s/%08d.chunk", page.Namespace, page.JobID, page.ExecutionID, page.Stream, chunk.Sequence)
	return chunk.Sequence > 0 && chunk.ByteOffset >= 0 && chunk.ByteLength >= 0 && chunk.ByteLength <= 262144 && chunk.ByteOffset <= math.MaxInt64-chunk.ByteLength && chunk.ByteOffset+chunk.ByteLength <= page.ByteLength && chunk.StoreName != "" && chunk.StoreVersion > 0 && chunk.ObjectKey == expected && manifestChecksumPattern.MatchString(chunk.Checksum) && (chunk.ByteLength > 0 || chunk.Complete) && (!chunk.Truncated || chunk.Complete) && (!chunk.Complete || chunk.Sequence == page.LastSequence && page.State == "complete" && chunk.ByteOffset+chunk.ByteLength == page.ByteLength && chunk.Truncated == page.Truncated)
}

// ListArtifacts provides full counts and bounded immutable output metadata.
func (store *Store) ListArtifacts(ctx context.Context, principal domain.Principal, namespace, jobID string, options domain.ArtifactListOptions) (domain.ArtifactPage, error) {
	if !domain.IsID(jobID) || options.Limit < 1 || options.Limit > 100 || options.RunNumber < 0 || (options.AfterExecutionID != "" && !domain.IsID(options.AfterExecutionID)) || (options.AfterExecutionID == "") != (options.AfterName == "") || len(options.AfterName) > 128 {
		return domain.ArtifactPage{}, errors.New("artifact manifest query is invalid")
	}
	return inReadTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.ArtifactPage, error) {
		result := domain.ArtifactPage{Items: []domain.BoundedArtifact{}}
		authorization, err := authorizeNamespace(ctx, tx, principal, namespace, domain.CapabilityArtifactsRead)
		if err != nil {
			return result, err
		}
		result.ManifestAuthority, err = readManifestAuthority(ctx, tx, authorization, jobID)
		if err != nil {
			return result, err
		}
		if countErr := tx.QueryRow(ctx, `SELECT count(*) FROM execution_artifacts a JOIN executions e ON e.id=a.execution_id JOIN runs r ON r.id=e.run_id WHERE r.job_id=$1 AND ($2::bigint=0 OR r.run_number=$2)`, jobID, options.RunNumber).Scan(&result.Total); countErr != nil {
			return result, countErr
		}
		rows, err := tx.Query(ctx, `SELECT r.id::text,r.run_number,e.id::text,e.target_generation_id::text,a.name,a.store_name,a.store_version,a.object_key,a.byte_length,a.checksum,a.published_at
 FROM execution_artifacts a JOIN executions e ON e.id=a.execution_id JOIN runs r ON r.id=e.run_id
 WHERE r.job_id=$1 AND ($2::bigint=0 OR r.run_number=$2) AND (NULLIF($3,'')::uuid IS NULL OR (a.execution_id,a.name)>(NULLIF($3,'')::uuid,$4)) ORDER BY a.execution_id,a.name LIMIT $5`, jobID, options.RunNumber, options.AfterExecutionID, options.AfterName, options.Limit+1)
		if err != nil {
			return result, err
		}
		defer rows.Close()
		encodedBytes := 2 // JSON array brackets.
		for rows.Next() {
			var item domain.BoundedArtifact
			if scanErr := rows.Scan(&item.RunID, &item.RunNumber, &item.ExecutionID, &item.TargetGenerationID, &item.Name, &item.StoreName, &item.StoreVersion, &item.ObjectKey, &item.ByteLength, &item.Checksum, &item.PublishedAt); scanErr != nil {
				return result, scanErr
			}
			item.PublishedAt = item.PublishedAt.UTC()
			encoded, encodeErr := json.Marshal(item)
			if encodeErr != nil || len(encoded)+2 > maximumArtifactItemsJSON {
				return result, domain.ErrConflict
			}
			if len(result.Items) == options.Limit || len(result.Items) > 0 && encodedBytes+len(encoded)+1 > maximumArtifactItemsJSON {
				last := result.Items[len(result.Items)-1]
				result.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(last.ExecutionID + "\n" + last.Name))
				break
			}
			encodedBytes += len(encoded) + 1 // Conservatively include an element separator.
			result.Items = append(result.Items, item)
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			return result, rowsErr
		}
		return result, nil
	})
}
