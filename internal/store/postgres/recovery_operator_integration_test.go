package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func TestRecoveryOperatorPrivilegesIntegration(t *testing.T) {
	databaseURL := os.Getenv("JOBMAN_CONTROL_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("JOBMAN_CONTROL_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	admin := newIntegrationPool(ctx, t, databaseURL)
	schema := admin.Config().ConnConfig.RuntimeParams["search_path"]
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	roleSuffix := strings.TrimPrefix(schema, "jobman_control_test_")
	migrator := "jm_migrate_" + roleSuffix
	runtime := "jm_runtime_" + roleSuffix
	operator := "jm_operator_" + roleSuffix
	for _, role := range []string{migrator, runtime, operator} {
		quotedRole := pgx.Identifier{role}.Sanitize()
		if _, err := admin.Exec(ctx, "CREATE ROLE "+quotedRole+" LOGIN"); err != nil {
			t.Fatalf("create disposable role: %v", err)
		}
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer stop()
			if _, err := admin.Exec(cleanup, "DROP OWNED BY "+quotedRole+"; DROP ROLE "+quotedRole); err != nil {
				t.Errorf("remove disposable role: %v", err)
			}
		})
	}
	grant := func(query string) {
		t.Helper()
		if _, err := admin.Exec(ctx, query); err != nil {
			t.Fatalf("configure disposable role: %v", err)
		}
	}
	grant("GRANT USAGE, CREATE ON SCHEMA " + quotedSchema + " TO " + migrator)
	grant("GRANT USAGE ON SCHEMA " + quotedSchema + " TO " + runtime + ", " + operator)
	owner := recoveryTestRolePool(ctx, t, admin, migrator)
	if err := Migrate(ctx, owner); err != nil {
		t.Fatal(err)
	}
	grant("GRANT SELECT ON schema_migrations, service_recovery_state TO " + runtime + ", " + operator)
	grant("GRANT UPDATE (singleton) ON service_recovery_state TO " + runtime)
	grant("GRANT SELECT ON service_operator_audit TO " + operator)
	grant("GRANT EXECUTE ON FUNCTION jobman_control_set_recovery(bigint, bigint, boolean, text) TO " + operator)
	service := recoveryTestRolePool(ctx, t, admin, runtime)
	recovery := recoveryTestRolePool(ctx, t, admin, operator)
	if err := CheckMigrations(ctx, service); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"CREATE TABLE unauthorized (id int)",
		"UPDATE service_recovery_state SET reconciliation_hold=true",
		"DELETE FROM service_operator_audit",
		"SELECT * FROM jobman_control_set_recovery(1, 2, true, 'unauthorized')",
	} {
		_, err := service.Exec(ctx, query)
		var denied *pgconn.PgError
		if !errors.As(err, &denied) || denied.Code != "42501" {
			t.Fatalf("runtime unexpectedly allowed to execute %q: %v", query, err)
		}
	}

	// An interrupted hold waiting on an assignment leaves no audit or state change.
	tx, err := service.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackVisibilityTest(ctx, t, tx)
	if _, err = tx.Exec(ctx, "SELECT reconciliation_hold FROM service_recovery_state FOR SHARE"); err != nil {
		t.Fatal(err)
	}
	deadline, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	_, err = SetRecovery(deadline, recovery, 1, 7, true, "interrupted")
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hold bypassed assignment row lock: %v", err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := RecoveryStatus(ctx, recovery)
	if err != nil || before.Hold || before.RestoreEpoch != 1 {
		t.Fatalf("before hold = %#v, %v", before, err)
	}
	held, err := SetRecovery(ctx, recovery, 1, 7, true, "restore-test")
	if err != nil || !held.Hold || held.RestoreEpoch != 7 || held.Reason != "restore-test" {
		t.Fatalf("held = %#v, %v", held, err)
	}
	if _, err = SetRecovery(ctx, recovery, 1, 1, false, "stale-release"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale release = %v", err)
	}
	released, err := SetRecovery(ctx, recovery, 7, 7, false, "release-test")
	if err != nil || released.Hold || released.RestoreEpoch != 7 {
		t.Fatalf("released = %#v, %v", released, err)
	}
	var actions []string
	err = recovery.QueryRow(ctx, `
		SELECT array_agg(action || ':' || actor_role ORDER BY id) FROM service_operator_audit
	`).Scan(&actions)
	if err != nil || strings.Join(actions, ",") != "restore.hold:"+operator+",restore.release:"+operator {
		t.Fatalf("operator audit = %v, %v", actions, err)
	}
}

func recoveryTestRolePool(ctx context.Context, t *testing.T, admin *pgxpool.Pool, role string) *pgxpool.Pool {
	t.Helper()
	configuration := admin.Config()
	configuration.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET SESSION AUTHORIZATION "+pgx.Identifier{role}.Sanitize())
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
