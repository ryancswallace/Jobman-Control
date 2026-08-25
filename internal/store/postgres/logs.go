package postgres

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

// CommitLogChunk records one immutable object reference after the agent has
// placed and checksummed its bytes. Delivery may be retried or out of order;
// only the contiguous source prefix is exposed through manifests.
func (store *Store) CommitLogChunk(
	ctx context.Context,
	identity domain.AgentIdentity,
	chunk domain.LogChunk,
) (bool, error) {
	replayed, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (bool, error) {
		var namespace, jobID, assignedAgent, phase, approvedStore string
		var approvedVersion int64
		queryErr := tx.QueryRow(ctx, `
			SELECT n.name, r.job_id::text, e.agent_id::text, e.phase,
				COALESCE(tg.log_store_name, ''), COALESCE(tg.log_store_version, 0)
			FROM executions AS e
			JOIN runs AS r ON r.id = e.run_id
			JOIN namespaces AS n ON n.id = e.namespace_id
			JOIN target_generations AS tg ON tg.id = e.target_generation_id
			WHERE e.id = $1 AND e.namespace_id = $2
		`, chunk.ExecutionID, identity.NamespaceID).Scan(
			&namespace, &jobID, &assignedAgent, &phase, &approvedStore, &approvedVersion,
		)
		if errors.Is(queryErr, pgx.ErrNoRows) {
			return false, domain.ErrNotFound
		}
		if queryErr != nil {
			return false, fmt.Errorf("read log execution: %w", queryErr)
		}
		if assignedAgent != identity.AgentID || chunk.AgentID != identity.AgentID {
			return false, domain.ErrForbidden
		}
		if chunk.StoreName != approvedStore || chunk.StoreVersion != approvedVersion {
			return false, domain.ErrForbidden
		}
		if phase == "planned" {
			return false, domain.ErrConflict
		}
		expectedKey := fmt.Sprintf(
			"namespaces/%s/jobs/%s/executions/%s/logs/%s/%08d.chunk",
			namespace, jobID, chunk.ExecutionID, chunk.Stream, chunk.Sequence,
		)
		if chunk.ObjectKey != expectedKey {
			return false, domain.ErrConflict
		}
		if _, queryErr = tx.Exec(ctx, `
			INSERT INTO log_streams (
				namespace_id, execution_id, agent_id, stream, store_name, store_version
			) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (execution_id, stream) DO NOTHING
		`, identity.NamespaceID, chunk.ExecutionID, identity.AgentID, chunk.Stream,
			chunk.StoreName, chunk.StoreVersion); queryErr != nil {
			return false, fmt.Errorf("create log stream: %w", queryErr)
		}
		var state, storeName, existingDigest string
		var storeVersion, byteLength, lastSequence int64
		queryErr = tx.QueryRow(ctx, `
			SELECT stream.state, stream.store_name, stream.store_version,
				stream.byte_length, stream.last_sequence,
				COALESCE(existing.document_digest, '')
			FROM log_streams AS stream
			LEFT JOIN log_chunks AS existing
				ON existing.execution_id = stream.execution_id
				AND existing.stream = stream.stream AND existing.sequence = $3
			WHERE stream.execution_id = $1 AND stream.stream = $2
			FOR UPDATE OF stream
		`, chunk.ExecutionID, chunk.Stream, chunk.Sequence).Scan(
			&state, &storeName, &storeVersion, &byteLength, &lastSequence, &existingDigest,
		)
		if queryErr != nil {
			return false, fmt.Errorf("lock log stream: %w", queryErr)
		}
		if existingDigest != "" {
			if existingDigest != chunk.DocumentDigest {
				return false, domain.ErrIdempotencyConflict
			}

			return true, nil
		}
		if state != "capturing" || storeName != chunk.StoreName || storeVersion != chunk.StoreVersion {
			return false, domain.ErrConflict
		}
		var priorEnd, nextOffset *int64
		var maximumSequence int64
		var terminalSequence *int64
		queryErr = tx.QueryRow(ctx, `
			SELECT MAX(byte_offset + byte_length) FILTER (WHERE sequence = $3 - 1),
				MIN(byte_offset) FILTER (WHERE sequence = $3 + 1),
				COALESCE(MAX(sequence), 0), MAX(sequence) FILTER (WHERE complete)
			FROM log_chunks WHERE execution_id = $1 AND stream = $2
		`, chunk.ExecutionID, chunk.Stream, chunk.Sequence).Scan(
			&priorEnd, &nextOffset, &maximumSequence, &terminalSequence,
		)
		if queryErr != nil {
			return false, fmt.Errorf("inspect log chunk neighbors: %w", queryErr)
		}
		if chunk.Sequence == 1 && chunk.ByteOffset != 0 ||
			priorEnd != nil && *priorEnd != chunk.ByteOffset ||
			nextOffset != nil && chunk.ByteOffset+chunk.ByteLength != *nextOffset ||
			terminalSequence != nil && chunk.Sequence >= *terminalSequence ||
			chunk.Complete && (terminalSequence != nil || chunk.Sequence <= maximumSequence) {
			return false, domain.ErrConflict
		}
		if _, queryErr = tx.Exec(ctx, `
			INSERT INTO log_chunks (
				namespace_id, execution_id, agent_id, stream, sequence,
				store_name, store_version, object_key, byte_offset, byte_length,
				checksum, captured_at, complete, truncated, document_digest, document
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
				$13, $14, $15, $16::jsonb)
		`, identity.NamespaceID, chunk.ExecutionID, identity.AgentID, chunk.Stream,
			chunk.Sequence, chunk.StoreName, chunk.StoreVersion, chunk.ObjectKey,
			chunk.ByteOffset, chunk.ByteLength, chunk.Checksum, chunk.CapturedAt,
			chunk.Complete, chunk.Truncated, chunk.DocumentDigest,
			string(chunk.Document)); queryErr != nil {
			return false, fmt.Errorf("insert log chunk: %w", queryErr)
		}
		lastSequence, byteLength, complete, truncated, queryErr := contiguousLogProjection(ctx, tx, chunk)
		if queryErr != nil {
			return false, queryErr
		}
		if _, queryErr = tx.Exec(ctx, `
			UPDATE log_streams
			SET byte_length = $3, last_sequence = $4,
				state = CASE WHEN $5 THEN 'complete' ELSE state END,
				truncated = CASE WHEN $5 THEN $6 ELSE truncated END,
				completed_at = CASE WHEN $5 THEN transaction_timestamp() ELSE completed_at END,
				updated_at = transaction_timestamp()
			WHERE execution_id = $1 AND stream = $2
		`, chunk.ExecutionID, chunk.Stream, byteLength,
			lastSequence, complete, truncated); queryErr != nil {
			return false, fmt.Errorf("advance log stream: %w", queryErr)
		}
		if complete {
			if _, queryErr = tx.Exec(ctx, `
				INSERT INTO audit_events (
					namespace_id, actor_kind, actor_agent_id, action,
					resource_type, resource_id, request_digest, details
				) VALUES ($1, 'agent', $2, 'log.completed', 'execution', $3, $4,
					jsonb_build_object('stream', $5::text, 'byteLength', $6::bigint,
						'truncated', $7::boolean, 'storeName', $8::text,
						'storeVersion', $9::bigint))
			`, identity.NamespaceID, identity.AgentID, chunk.ExecutionID,
				chunk.DocumentDigest, chunk.Stream, byteLength,
				truncated, chunk.StoreName, chunk.StoreVersion); queryErr != nil {
				return false, fmt.Errorf("audit log completion: %w", queryErr)
			}
		}

		return false, nil
	})
	if err != nil {
		return false, fmt.Errorf("commit log chunk: %w", err)
	}

	return replayed, nil
}

func contiguousLogProjection(
	ctx context.Context,
	tx pgx.Tx,
	chunk domain.LogChunk,
) (lastSequence, byteLength int64, complete, truncated bool, err error) {
	rows, err := tx.Query(ctx, `
		SELECT sequence, byte_offset, byte_length, complete, truncated
		FROM log_chunks WHERE execution_id = $1 AND stream = $2 ORDER BY sequence
	`, chunk.ExecutionID, chunk.Stream)
	if err != nil {
		return 0, 0, false, false, fmt.Errorf("read log chunk projection: %w", err)
	}
	defer rows.Close()
	expectedSequence := int64(1)
	for rows.Next() {
		var sequence, offset, length int64
		var terminal, wasTruncated bool
		if err = rows.Scan(&sequence, &offset, &length, &terminal, &wasTruncated); err != nil {
			return 0, 0, false, false, fmt.Errorf("scan log chunk projection: %w", err)
		}
		if sequence != expectedSequence {
			break
		}
		if offset != byteLength {
			return 0, 0, false, false, domain.ErrConflict
		}
		lastSequence = sequence
		byteLength += length
		expectedSequence++
		if terminal {
			complete = true
			truncated = wasTruncated
			break
		}
	}
	if err = rows.Err(); err != nil {
		return 0, 0, false, false, fmt.Errorf("iterate log chunk projection: %w", err)
	}

	return lastSequence, byteLength, complete, truncated, nil
}

// GetJobLogs returns metadata for all published execution streams after one
// current namespace-membership authorization check.
func (store *Store) GetJobLogs(
	ctx context.Context,
	principal domain.Principal,
	namespace, jobID string,
) ([]domain.LogStream, error) {
	var authorized bool
	if err := store.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM jobs AS j
			JOIN namespaces AS n ON n.id = j.namespace_id
			JOIN memberships AS m ON m.namespace_id = n.id
			JOIN principals AS p ON p.id = m.principal_id
			WHERE p.issuer = $1 AND p.subject = $2 AND n.name = $3 AND j.id = $4
		)
	`, principal.Issuer, principal.Subject, namespace, jobID).Scan(&authorized); err != nil {
		return nil, fmt.Errorf("authorize job logs: %w", err)
	}
	if !authorized {
		return nil, domain.ErrNotFound
	}
	rows, err := store.pool.Query(ctx, `
		SELECT e.id::text, r.run_number, stream.stream, stream.state,
			stream.byte_length, stream.truncated,
			chunk.sequence, chunk.store_name, chunk.store_version,
			chunk.object_key, chunk.byte_offset, chunk.byte_length,
			chunk.checksum, chunk.captured_at
		FROM jobs AS j
		JOIN runs AS r ON r.job_id = j.id
		JOIN executions AS e ON e.run_id = r.id
		JOIN log_streams AS stream ON stream.execution_id = e.id
		JOIN log_chunks AS chunk ON chunk.execution_id = stream.execution_id
			AND chunk.stream = stream.stream AND chunk.sequence <= stream.last_sequence
		WHERE j.id = $1
		ORDER BY r.run_number, e.created_at, stream.stream, chunk.sequence
	`, jobID)
	if err != nil {
		return nil, fmt.Errorf("query job logs: %w", err)
	}
	defer rows.Close()
	streams := make(map[string]*domain.LogStream)
	var order []string
	for rows.Next() {
		var chunk domain.LogChunk
		var streamState string
		var runNumber int
		var streamLength int64
		var truncated bool
		if err = rows.Scan(
			&chunk.ExecutionID, &runNumber, &chunk.Stream, &streamState,
			&streamLength, &truncated, &chunk.Sequence, &chunk.StoreName,
			&chunk.StoreVersion, &chunk.ObjectKey, &chunk.ByteOffset,
			&chunk.ByteLength, &chunk.Checksum, &chunk.CapturedAt,
		); err != nil {
			return nil, fmt.Errorf("scan job log manifest: %w", err)
		}
		key := chunk.ExecutionID + "\x00" + chunk.Stream
		stream, found := streams[key]
		if !found {
			stream = &domain.LogStream{
				ExecutionID: chunk.ExecutionID, RunNumber: runNumber, Stream: chunk.Stream,
				State: streamState, ByteLength: streamLength, Truncated: truncated,
			}
			streams[key] = stream
			order = append(order, key)
		}
		stream.Chunks = append(stream.Chunks, chunk)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate job log manifest: %w", err)
	}
	sort.SliceStable(order, func(first, second int) bool {
		return streams[order[first]].RunNumber < streams[order[second]].RunNumber ||
			streams[order[first]].RunNumber == streams[order[second]].RunNumber &&
				streams[order[first]].Stream < streams[order[second]].Stream
	})
	result := make([]domain.LogStream, 0, len(order))
	for _, key := range order {
		result = append(result, *streams[key])
	}

	return result, nil
}
