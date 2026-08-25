// Package postgres implements shared control-plane persistence in PostgreSQL.
package postgres

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

// Store is a PostgreSQL implementation of the shared job repository.
type Store struct {
	pool     *pgxpool.Pool
	newID    func() (string, error)
	tokenKey []byte
}

// New returns a store backed by pool.
func New(pool *pgxpool.Pool, tokenKey []byte) *Store {
	return &Store{pool: pool, newID: domain.NewID, tokenKey: append([]byte(nil), tokenKey...)}
}

func (store *Store) deriveToken(prefix, identifier string) string {
	mac := hmac.New(sha256.New, store.tokenKey)
	_, _ = mac.Write([]byte(prefix))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(identifier))

	return prefix + "_" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))

	return sum[:]
}

// Ready checks database connectivity and exact migration compatibility.
func (store *Store) Ready(ctx context.Context) error {
	if err := store.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping PostgreSQL: %w", err)
	}
	if err := CheckMigrations(ctx, store.pool); err != nil {
		return fmt.Errorf("check PostgreSQL schema: %w", err)
	}

	return nil
}

func inTransaction[T any](
	ctx context.Context,
	pool *pgxpool.Pool,
	operation func(pgx.Tx) (T, error),
) (result T, resultErr error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return result, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		rollbackErr := tx.Rollback(cleanupContext)
		if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			resultErr = errors.Join(resultErr, fmt.Errorf("rollback transaction: %w", rollbackErr))
		}
	}()

	result, err = operation(tx)
	if err != nil {
		return result, err
	}
	if err = tx.Commit(ctx); err != nil {
		return result, fmt.Errorf("commit transaction: %w", err)
	}

	return result, nil
}
