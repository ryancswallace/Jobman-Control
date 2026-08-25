package postgres

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const migrationLockID int64 = 0x4a4f424d414e

//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version  string
	checksum string
	contents string
}

// Migrate applies every known forward-only migration in one transaction.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	return migrate(ctx, pool, migrations)
}

// CheckMigrations verifies that the database and binary have exactly the same
// migration set. It intentionally rejects a database newer than the service.
func CheckMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	rows, err := pool.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("read schema migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[string]string, len(migrations))
	for rows.Next() {
		var version string
		var checksum string
		if scanErr := rows.Scan(&version, &checksum); scanErr != nil {
			return fmt.Errorf("scan schema migration: %w", scanErr)
		}
		applied[version] = checksum
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return fmt.Errorf("iterate schema migrations: %w", rowsErr)
	}

	return compareMigrations(migrations, applied)
}

func loadMigrations() ([]migration, error) {
	names, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return nil, fmt.Errorf("list embedded migrations: %w", err)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, errors.New("no embedded migrations")
	}

	migrations := make([]migration, 0, len(names))
	for _, name := range names {
		contents, readErr := migrationFiles.ReadFile(name)
		if readErr != nil {
			return nil, fmt.Errorf("read embedded migration %q: %w", name, readErr)
		}
		sum := sha256.Sum256(contents)
		migrations = append(migrations, migration{
			version:  path.Base(name),
			checksum: hex.EncodeToString(sum[:]),
			contents: string(contents),
		})
	}

	return migrations, nil
}

func migrate(ctx context.Context, pool *pgxpool.Pool, migrations []migration) (resultErr error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin migration transaction: %w", err)
	}
	defer func() {
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		rollbackErr := tx.Rollback(cleanupContext)
		if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			resultErr = errors.Join(resultErr, fmt.Errorf("rollback migration transaction: %w", rollbackErr))
		}
	}()

	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("lock schema migrations: %w", err)
	}
	if _, err = tx.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version text PRIMARY KEY,
			checksum char(64) NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT transaction_timestamp()
		)
	`); err != nil {
		return fmt.Errorf("create schema migration ledger: %w", err)
	}

	applied, err := readAppliedMigrations(ctx, tx)
	if err != nil {
		return err
	}
	if comparisonErr := rejectUnknownOrChangedMigrations(migrations, applied); comparisonErr != nil {
		return comparisonErr
	}

	for _, item := range migrations {
		if _, exists := applied[item.version]; exists {
			continue
		}
		if _, err = tx.Exec(ctx, item.contents); err != nil {
			return fmt.Errorf("apply migration %q: %w", item.version, err)
		}
		if _, err = tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, checksum) VALUES ($1, $2)`,
			item.version, item.checksum,
		); err != nil {
			return fmt.Errorf("record migration %q: %w", item.version, err)
		}
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit schema migrations: %w", err)
	}

	return nil
}

func readAppliedMigrations(ctx context.Context, tx pgx.Tx) (map[string]string, error) {
	rows, err := tx.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[string]string)
	for rows.Next() {
		var version string
		var checksum string
		if scanErr := rows.Scan(&version, &checksum); scanErr != nil {
			return nil, fmt.Errorf("scan applied migration: %w", scanErr)
		}
		applied[version] = checksum
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate applied migrations: %w", rowsErr)
	}

	return applied, nil
}

func rejectUnknownOrChangedMigrations(migrations []migration, applied map[string]string) error {
	known := make(map[string]string, len(migrations))
	for _, item := range migrations {
		known[item.version] = item.checksum
	}
	for version, checksum := range applied {
		expected, exists := known[version]
		if !exists {
			return fmt.Errorf("database contains migration unknown to this binary: %q", version)
		}
		if checksum != expected {
			return fmt.Errorf("database migration checksum differs from binary: %q", version)
		}
	}

	return nil
}

func compareMigrations(migrations []migration, applied map[string]string) error {
	if err := rejectUnknownOrChangedMigrations(migrations, applied); err != nil {
		return err
	}
	for _, item := range migrations {
		if _, exists := applied[item.version]; !exists {
			return fmt.Errorf("database migration is pending: %q", item.version)
		}
	}

	return nil
}
