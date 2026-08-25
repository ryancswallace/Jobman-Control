package postgres

import (
	"context"
	"fmt"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

// GetJobArtifacts returns immutable output metadata after one current
// namespace-membership authorization check. Artifact bytes are never proxied
// through Control.
func (store *Store) GetJobArtifacts(
	ctx context.Context,
	principal domain.Principal,
	namespace, jobID string,
) ([]domain.PublishedArtifact, error) {
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
		return nil, fmt.Errorf("authorize job artifacts: %w", err)
	}
	if !authorized {
		return nil, domain.ErrNotFound
	}

	rows, err := store.pool.Query(ctx, `
		SELECT artifact.execution_id::text, run.run_number, artifact.name,
			artifact.store_name, artifact.store_version, artifact.object_key,
			artifact.byte_length, artifact.checksum, artifact.created_at
		FROM execution_artifacts AS artifact
		JOIN executions AS execution ON execution.id = artifact.execution_id
		JOIN runs AS run ON run.id = execution.run_id
		WHERE run.job_id = $1
		ORDER BY run.run_number, execution.created_at, artifact.name
	`, jobID)
	if err != nil {
		return nil, fmt.Errorf("query job artifacts: %w", err)
	}
	defer rows.Close()

	result := []domain.PublishedArtifact{}
	for rows.Next() {
		var artifact domain.PublishedArtifact
		if err = rows.Scan(
			&artifact.ExecutionID, &artifact.RunNumber, &artifact.Name,
			&artifact.StoreName, &artifact.StoreVersion, &artifact.ObjectKey,
			&artifact.ByteLength, &artifact.Checksum, &artifact.PublishedAt,
		); err != nil {
			return nil, fmt.Errorf("scan job artifact manifest: %w", err)
		}
		result = append(result, artifact)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate job artifact manifest: %w", err)
	}
	return result, nil
}
