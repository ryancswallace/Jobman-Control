package postgres

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func TestRecoveryRestoreDrillIntegration(t *testing.T) {
	if os.Getenv("JOBMAN_CONTROL_TEST_RESTORE") != "1" {
		t.Skip("set JOBMAN_CONTROL_TEST_RESTORE=1 with pg_dump and pg_restore for a disposable database")
	}
	fixture := newRecoveryFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	submission := integrationSubmission(t)
	if _, err := fixture.store.SubmitJob(ctx, fixture.principal, "restore-existing", submission); err != nil {
		t.Fatal(err)
	}
	if count, err := fixture.store.ReconcileAssignments(ctx, 1); err != nil || count != 1 {
		t.Fatalf("initial assignment = %d, %v", count, err)
	}
	existing, err := fixture.store.ListAssignments(ctx, fixture.identity, 1)
	if err != nil || len(existing) != 1 {
		t.Fatalf("initial offer count = %d, %v", len(existing), err)
	}
	if _, err = fixture.store.SubmitJob(ctx, fixture.principal, "restore-queued", submission); err != nil {
		t.Fatal(err)
	}
	configuration := fixture.pool.Config()
	identifier, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	restoredDB := "jobman_restore_test_" + strings.ReplaceAll(identifier, "-", "")
	quotedDB := pgx.Identifier{restoredDB}.Sanitize()
	if _, err = fixture.pool.Exec(ctx, "CREATE DATABASE "+quotedDB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer stop()
		if _, dropErr := fixture.pool.Exec(cleanup, "DROP DATABASE "+quotedDB); dropErr != nil {
			t.Errorf("remove disposable restore: %v", dropErr)
		}
	})
	// PostgreSQL utilities obtain their connection settings from the explicitly
	// configured test environment. Never pass a database URI on their argv.
	dump := exec.CommandContext(ctx, "pg_dump", "--format=custom", "--no-password",
		"--dbname="+configuration.ConnConfig.Database,
		"--schema="+configuration.ConnConfig.RuntimeParams["search_path"])
	archive, err := dump.Output()
	if err != nil || len(archive) == 0 {
		t.Fatalf("dump disposable schema: %v", err)
	}
	restore := exec.CommandContext(ctx, "pg_restore", "--exit-on-error", "--no-owner", "--no-acl",
		"--no-password", "--dbname="+restoredDB)
	restore.Stdin = bytes.NewReader(archive)
	if err = restore.Run(); err != nil {
		t.Fatalf("restore disposable schema: %v", err)
	}
	configuration.ConnConfig.Database = restoredDB
	restored, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err = CheckMigrations(ctx, restored); err != nil {
		t.Fatal(err)
	}
	if _, err = SetRecovery(ctx, restored, 1, 7, true, "restore-drill"); err != nil {
		t.Fatal(err)
	}
	store := New(restored, []byte("0123456789abcdef0123456789abcdef"))
	if count, reconcileErr := store.ReconcileAssignments(ctx, 10); reconcileErr != nil || count != 0 {
		t.Fatalf("held assignment count = %d, %v", count, reconcileErr)
	}
	replayed, err := store.ListAssignments(ctx, fixture.identity, 10)
	if err != nil || len(replayed) != 1 || replayed[0].ExecutionID != existing[0].ExecutionID ||
		replayed[0].DeliveryID != existing[0].DeliveryID {
		t.Fatalf("restore changed existing execution or offer identity: %d, %v", len(replayed), err)
	}
	if _, err = SetRecovery(ctx, restored, 7, 7, false, "restore-drill"); err != nil {
		t.Fatal(err)
	}
	if count, reconcileErr := store.ReconcileAssignments(ctx, 10); reconcileErr != nil || count != 1 {
		t.Fatalf("released assignment count = %d, %v", count, reconcileErr)
	}
	assertTableCount(ctx, t, restored, "assignments", 2)
}
