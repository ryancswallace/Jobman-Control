package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

// ImportCompletedHistory validates or imports one quiescent terminal job. It
// records provenance but deliberately creates no runnable run or execution.
func (store *Store) ImportCompletedHistory(
	ctx context.Context,
	principal domain.Principal,
	idempotencyKey string,
	dryRun bool,
	request domain.CompletedHistoryImport,
) (domain.HistoryImportResult, error) {
	if err := validateCompletedHistoryImport(request); err != nil {
		return domain.HistoryImportResult{}, err
	}
	if !dryRun && !domain.ValidIdempotencyKey(idempotencyKey) {
		return domain.HistoryImportResult{}, errors.New("invalid idempotency key")
	}
	jobID, err := store.newID()
	if err != nil {
		return domain.HistoryImportResult{}, err
	}
	outboxID, err := store.newID()
	if err != nil {
		return domain.HistoryImportResult{}, err
	}
	result, err := inTransaction(ctx, store.pool, func(tx pgx.Tx) (domain.HistoryImportResult, error) {
		authorization, authErr := authorizeSubmission(ctx, tx, principal, request.Job.Namespace)
		if authErr != nil {
			return domain.HistoryImportResult{}, authErr
		}
		placement, resolveErr := resolveTarget(ctx, tx, authorization.namespaceID, request.Job)
		if resolveErr != nil {
			return domain.HistoryImportResult{}, resolveErr
		}
		if dryRun {
			return domain.HistoryImportResult{DryRun: true}, nil
		}
		resourceID, replayed, reserveErr := reserveIdempotency(
			ctx, tx, authorization, "history.import", "job", idempotencyKey,
			request.RequestDigest, 201,
		)
		if reserveErr != nil {
			return domain.HistoryImportResult{}, reserveErr
		}
		if replayed {
			job, getErr := getJobByID(ctx, tx, authorization.namespaceID, resourceID)

			return domain.HistoryImportResult{Job: job, Replayed: true}, getErr
		}
		var existing bool
		if queryErr := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM jobs WHERE namespace_id = $1 AND imported
					AND import_source->>'store' = $2 AND import_source->>'jobId' = $3
			)
		`, authorization.namespaceID, request.SourceStore, request.SourceJobID).Scan(&existing); queryErr != nil {
			return domain.HistoryImportResult{}, fmt.Errorf("check history provenance: %w", queryErr)
		}
		if existing {
			return domain.HistoryImportResult{}, domain.ErrConflict
		}
		if insertErr := insertWorkloadRevision(ctx, tx, authorization, request.Job); insertErr != nil {
			return domain.HistoryImportResult{}, insertErr
		}
		if _, insertErr := insertJob(ctx, tx, authorization, jobID, request.Job, placement); insertErr != nil {
			return domain.HistoryImportResult{}, insertErr
		}
		provenance, encodeErr := json.Marshal(map[string]any{
			"store": request.SourceStore, "schema": request.SourceSchema,
			"jobId": request.SourceJobID,
		})
		if encodeErr != nil {
			return domain.HistoryImportResult{}, fmt.Errorf("encode history provenance: %w", encodeErr)
		}
		if _, updateErr := tx.Exec(ctx, `
			UPDATE jobs SET phase = 'terminal', outcome = $2, imported = true,
				import_source = $3::jsonb, completed_at = $4,
				revision = revision + 1, updated_at = transaction_timestamp()
			WHERE id = $1 AND phase = 'accepted'
		`, jobID, request.Outcome, string(provenance), request.CompletedAt); updateErr != nil {
			return domain.HistoryImportResult{}, fmt.Errorf("terminalize imported history: %w", updateErr)
		}
		if completeErr := completeIdempotency(
			ctx, tx, authorization, "history.import", idempotencyKey, jobID, 201,
		); completeErr != nil {
			return domain.HistoryImportResult{}, completeErr
		}
		if _, evidenceErr := tx.Exec(ctx, `
			INSERT INTO audit_events (
				namespace_id, actor_principal_id, action, resource_type, resource_id,
				request_digest, idempotency_key, details
			) VALUES ($1, $2, 'history.imported', 'job', $3, $4, $5,
				jsonb_build_object('sourceStore', $6::text, 'sourceSchema', $7::integer,
					'sourceJobId', $8::text, 'outcome', $9::text))
		`, authorization.namespaceID, authorization.principalID, jobID,
			request.RequestDigest, idempotencyKey, request.SourceStore,
			request.SourceSchema, request.SourceJobID, request.Outcome); evidenceErr != nil {
			return domain.HistoryImportResult{}, fmt.Errorf("audit history import: %w", evidenceErr)
		}
		if _, evidenceErr := tx.Exec(ctx, `
			INSERT INTO outbox (id, namespace_id, topic, aggregate_type, aggregate_id, payload)
			VALUES ($1, $2, 'history.imported', 'job', $3::uuid,
				jsonb_build_object('jobId', $3::text, 'sourceStore', $4::text,
					'sourceJobId', $5::text))
		`, outboxID, authorization.namespaceID, jobID,
			request.SourceStore, request.SourceJobID); evidenceErr != nil {
			return domain.HistoryImportResult{}, fmt.Errorf("insert history outbox event: %w", evidenceErr)
		}
		job, getErr := getJobByID(ctx, tx, authorization.namespaceID, jobID)

		return domain.HistoryImportResult{Job: job}, getErr
	})
	if err != nil {
		return domain.HistoryImportResult{}, fmt.Errorf("import completed history: %w", err)
	}

	return result, nil
}

func validateCompletedHistoryImport(request domain.CompletedHistoryImport) error {
	if validateSubmission(request.Job) != nil || request.RequestDigest == "" ||
		!json.Valid(request.RequestDocument) ||
		!slices.Contains([]string{"success", "failure", cancelledGraphWireName, "timed_out", "aborted", "lost"}, request.Outcome) ||
		request.CompletedAt.IsZero() ||
		request.SourceStore != "sqlite" || request.SourceSchema < 1 ||
		strings.TrimSpace(request.SourceJobID) == "" || len(request.SourceJobID) > 128 {
		return errors.New("completed history import is invalid")
	}

	return nil
}
