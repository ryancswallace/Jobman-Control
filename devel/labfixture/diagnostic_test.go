package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ryancswallace/jobman-control/internal/domain"
	"github.com/ryancswallace/jobman-control/internal/store/postgres"
)

func TestDiagnosticPrivateInputAndExclusiveReceipt(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "receipt.json")
	if err := writeDiagnosticJSON(root, "receipt.json", map[string]bool{"synthetic": true}); err != nil {
		t.Fatal(err)
	}
	before, err := readDiagnosticPrivate(path, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(writeDiagnosticJSON(root, "receipt.json", nil), os.ErrExist) {
		t.Fatal("existing receipt overwritten")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("receipt changed")
	}
	if _, err := readDiagnosticPrivate(path, 1); err == nil {
		t.Fatal("oversized file accepted")
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readDiagnosticPrivate(link, 1024); err == nil {
		t.Fatal("private symlink accepted")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readDiagnosticPrivate(path, 1024); err == nil {
		t.Fatal("public input accepted")
	}
	for _, input := range []string{"malformed", "OTHER=\"value\"", "JOBMAN_CONTROL_A=\"a\"\nJOBMAN_CONTROL_A=\"b\"", "JOBMAN_CONTROL_A=null", "JOBMAN_CONTROL_A=\"line\\nsecret\""} {
		if _, err := diagnosticEnvironment([]byte(input)); err == nil {
			t.Fatal("ambiguous environment accepted")
		}
	}
	if values, err := diagnosticEnvironment([]byte("JOBMAN_CONTROL_EXAMPLE=\"synthetic\"\n")); err != nil || values["JOBMAN_CONTROL_EXAMPLE"] != "synthetic" {
		t.Fatal("valid fixture environment rejected", err)
	}
}

func TestDiagnosticPreflightDoesNotTouchExistingFixture(t *testing.T) {
	root, spool := t.TempDir(), t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(spool, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := writeDiagnosticJSON(root, diagnosticReceiptName, map[string]bool{"synthetic": true}); err != nil {
		t.Fatal(err)
	}
	if err := prepareDiagnostic(t.Context(), root, "absent", spool, diagnosticDeployment); err == nil || !strings.Contains(err.Error(), "pending fixture") {
		t.Fatal("pending receipt did not stop preparation", err)
	}
	if err := prepareDiagnostic(t.Context(), root, "absent", spool, "other"); err == nil {
		t.Fatal("unapproved deployment accepted")
	}
	entries, err := os.ReadDir(spool)
	if err != nil || len(entries) != 0 {
		t.Fatal("preflight wrote spool")
	}
}

func TestDiagnosticFixtureIntegration(t *testing.T) {
	dsn := os.Getenv("JOBMAN_CONTROL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("JOBMAN_CONTROL_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("open isolated test database")
	}
	defer admin.Close()
	id, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	schema := "diagnostic_fixture_" + strings.ReplaceAll(id, "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, dropErr := admin.Exec(context.WithoutCancel(ctx), "DROP SCHEMA "+quoted+" CASCADE"); dropErr != nil {
			t.Error(dropErr)
		}
	}()
	options, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("parse isolated database")
	}
	options.ConnConfig.RuntimeParams["search_path"] = schema
	options.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, options)
	if err != nil {
		t.Fatal("open isolated schema")
	}
	defer pool.Close()
	if err = postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := postgres.New(pool, []byte("0123456789abcdef0123456789abcdef"))
	originalRoot := t.TempDir()
	original, err := seed(ctx, store, testInput(), originalRoot)
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := store.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	original.InstanceID, original.Synthetic = capabilities.InstanceID, true
	// Model the old fixture agents' natural expiry inside this disposable schema.
	// The live helper never changes an existing agent or runs reconciliation.
	if _, updateErr := pool.Exec(ctx, "UPDATE agents SET last_capability_at=statement_timestamp()-interval '1 hour'"); updateErr != nil {
		t.Fatal(updateErr)
	}
	var before []byte
	var originalIDs []string
	if queryErr := pool.QueryRow(ctx, "SELECT jsonb_agg(to_jsonb(j) ORDER BY id),array_agg(id::text ORDER BY id) FROM jobs j").Scan(&before, &originalIDs); queryErr != nil {
		t.Fatal(queryErr)
	}
	coordinator, stopCoordinator := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-coordinator.Done():
				done <- nil
				return
			case <-ticker.C:
				if _, reconcileErr := store.ReconcileAssignments(coordinator, 1); reconcileErr != nil {
					if errors.Is(reconcileErr, context.Canceled) {
						reconcileErr = nil
					}
					done <- reconcileErr
					return
				}
			}
		}
	}()
	defer func() {
		stopCoordinator()
		if coordinatorErr := <-done; coordinatorErr != nil {
			t.Error(coordinatorErr)
		}
	}()
	principal := domain.Principal{Issuer: testInput().Issuer, Subject: "alice"}
	spool := t.TempDir()
	fixture, err := seedDiagnostic(ctx, store, principal, original, spool)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	var restored diagnosticFixture
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if err := verifyDiagnostic(ctx, store, principal, original, restored, spool); err != nil {
		t.Fatal(err)
	}
	var after []byte
	if err := pool.QueryRow(ctx, "SELECT jsonb_agg(to_jsonb(j) ORDER BY id) FROM jobs j WHERE id=ANY($1::uuid[])", originalIDs).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("existing fixture jobs changed")
	}
	if fixture.TargetName != diagnosticTarget || fixture.ObservationMode != "synthetic-store-observations-no-execution" || !domain.IsID(fixture.RunID) || !domain.IsID(fixture.ExecutionID) || len(fixture.Streams) != 2 {
		t.Fatal("supplemental manifest lacks synthetic provenance")
	}
	if _, err := seedDiagnostic(ctx, store, principal, original, spool); err == nil {
		t.Fatal("duplicate target preparation did not fail closed")
	}
	altered := restored
	altered.RecoveryEpoch = "999"
	if err := verifyDiagnostic(ctx, store, principal, original, altered, spool); err == nil {
		t.Fatal("recovered source accepted old fixture")
	}
	chunk := restored.Streams[1].Chunks[0]
	path := filepath.Join(spool, filepath.FromSlash(chunk.ObjectKey))
	if err := os.WriteFile(path, []byte("changed synthetic bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyDiagnostic(ctx, store, principal, original, restored, spool); err == nil {
		t.Fatal("altered immutable diagnostic bytes accepted")
	}
}
