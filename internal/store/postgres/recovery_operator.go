package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

// RecoveryStatus reads the persistent hold and epoch without changing either.
func RecoveryStatus(ctx context.Context, pool *pgxpool.Pool) (domain.RecoveryState, error) {
	var result domain.RecoveryState
	err := pool.QueryRow(ctx, `
		SELECT reconciliation_hold, restore_epoch, updated_at
		FROM service_recovery_state WHERE singleton
	`).Scan(&result.Hold, &result.RestoreEpoch, &result.UpdatedAt)
	if err != nil {
		return domain.RecoveryState{}, fmt.Errorf("read recovery state: %w", err)
	}
	return result, nil
}

// SetRecovery atomically changes the hold and records the authenticated
// PostgreSQL login in the operator audit.
func SetRecovery(ctx context.Context, pool *pgxpool.Pool, expected, next int64, hold bool, reference string) (domain.RecoveryState, error) {
	var result domain.RecoveryState
	err := pool.QueryRow(ctx, `
		SELECT hold, epoch, reason, updated_at
		FROM jobman_control_set_recovery($1, $2, $3, $4)
	`, expected, next, hold, reference).Scan(&result.Hold, &result.RestoreEpoch, &result.Reason, &result.UpdatedAt)
	if err != nil {
		var databaseError *pgconn.PgError
		if errors.As(err, &databaseError) && databaseError.Code == "40001" {
			return domain.RecoveryState{}, domain.ErrConflict
		}
		return domain.RecoveryState{}, fmt.Errorf("set recovery state: %w", err)
	}
	return result, nil
}
