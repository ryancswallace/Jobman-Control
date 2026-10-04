package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ryancswallace/jobman-control/internal/directory"
	"github.com/ryancswallace/jobman-control/internal/domain"
	"github.com/ryancswallace/jobman-control/internal/store/postgres"
)

func secondaryIntegrationPool(ctx context.Context, t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("JOBMAN_CONTROL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("JOBMAN_CONTROL_TEST_DATABASE_URL is not set")
	}
	options, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("parse explicit test database")
	}
	options.MaxConns = 2
	admin, err := pgxpool.NewWithConfig(ctx, options)
	if err != nil {
		t.Fatal("open explicit test database")
	}
	t.Cleanup(admin.Close)
	id, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	schema := "secondary_fixture_" + strings.ReplaceAll(id, "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, cleanupErr := admin.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); cleanupErr != nil {
			t.Error(cleanupErr)
		}
	})
	options.ConnConfig.RuntimeParams["search_path"] = schema
	options.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, options)
	if err != nil {
		t.Fatal("open disposable schema")
	}
	t.Cleanup(pool.Close)
	if err = postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestSecondaryProfileIntegration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX private fixture material")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	pool := secondaryIntegrationPool(ctx, t)
	store := postgres.New(pool, []byte("0123456789abcdef0123456789abcdef"))
	profile, input := secondaryProfile(), testInput()
	original, err := seed(ctx, store, input, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	caps, err := store.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	original.Profile, original.InstanceID, original.Synthetic, original.DelegationAudience = profile.name, caps.InstanceID, true, profile.audience
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, exported, spool := filepath.Join(base, "source"), filepath.Join(base, "directory"), filepath.Join(base, "spool")
	for _, path := range []string{root, exported, spool} {
		if err = os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if secondaryPreparePreflight(ctx, pool, root, exported, spool) == nil {
		t.Fatal("populated schema accepted for secondary preparation")
	}
	if err = emptyPrivateFixtureRoot(root); err != nil {
		t.Fatal("failed preflight changed private root", err)
	}
	if _, err = generateMaterialProfile(root, "10.77.0.21", profile); err != nil {
		t.Fatal(err)
	}
	cfg, state := directoryProfile(root, profile, input, original)
	if err = exportSecondaryDirectory(root, exported, profile, state); err != nil {
		t.Fatal(err)
	}
	if err = store.ConfigureDirectory(ctx, cfg.Mapping); err != nil {
		t.Fatal(err)
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(exported, "directory-server.crt"), filepath.Join(exported, "directory-server.key"))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := new(net.ListenConfig).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		served <- serveDirectoryOnProfile(ctx, tls.NewListener(listener, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}), exported, profile)
	}()
	t.Cleanup(func() {
		cancel()
		if serveErr := <-served; serveErr != nil {
			t.Error(serveErr)
		}
	})
	cfg.URL = "ldaps://" + listener.Addr().String()
	epoch, err := store.DirectoryRecoveryEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := (directory.Reader{Config: cfg}).Read(ctx, epoch)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.ApplyDirectorySnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	alice := domain.Principal{Issuer: input.Issuer, Subject: input.Users[0].Subject}
	bob := domain.Principal{Issuer: input.Issuer, Subject: input.Users[1].Subject}
	aliceAccess, err := store.CurrentPrincipal(ctx, alice, "", 100)
	if err != nil || len(aliceAccess.Namespaces) != 1 || aliceAccess.Namespaces[0].Name != "dashboard-research" || !slices.Equal(aliceAccess.Namespaces[0].Roles, []string{domain.RoleViewer}) {
		t.Fatal("secondary Alice authority differs", err)
	}
	bobAccess, err := store.CurrentPrincipal(ctx, bob, "", 100)
	if err != nil || len(bobAccess.Namespaces) != 2 {
		t.Fatal("secondary Bob scope differs", err)
	}
	for _, ns := range bobAccess.Namespaces {
		expect := []string{domain.RoleNamespaceAdmin}
		if ns.Name == "dashboard-research" {
			expect = []string{domain.RoleSubmitter, domain.RoleViewer}
		}
		if !slices.Equal(ns.Roles, expect) || ns.AuthorizationStatus != "verified" {
			t.Fatal("secondary Bob direct union differs")
		}
	}
	var before []byte
	var ids []string
	if err = pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(j) ORDER BY id),array_agg(id::text ORDER BY id) FROM jobs j`).Scan(&before, &ids); err != nil {
		t.Fatal(err)
	}
	receipt := strings.Repeat("c", 32)
	fixture, err := seedNotificationScenarioProfile(ctx, store, bob, original, receipt, profile)
	if err != nil || fixture.Profile != profile.name || fixture.DeploymentID != profile.deployment {
		t.Fatal("secondary normal admission failed", err)
	}
	for _, job := range fixture.Jobs {
		if _, err = store.GetJob(ctx, alice, fixture.Namespace, job.JobID); err != nil {
			t.Fatal("same-namespace cross-owner read denied", err)
		}
	}
	request, err := seedSubmission(fixture.Namespace, "alice-cannot-submit", fixtureTarget)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SubmitJob(ctx, alice, "secondary-alice-denied", request); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("primary submitter grant survived secondary mapping", err)
	}
	for _, ns := range original.Namespaces {
		if ns.Name == "dashboard-operations" {
			if _, err = store.GetJob(ctx, alice, ns.Name, ns.JobIDs[0]); !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrForbidden) {
				t.Fatal("Alice reached secondary operations", err)
			}
		}
	}
	first, err := completeNotificationScenario(ctx, pool, store, bob, fixture, "first")
	if err != nil || !domain.IsID(first.EventID) || first.Outcome != notificationCancelled {
		t.Fatal("secondary cancellation lacks real terminal event", err)
	}
	if verifyNotificationFixtureProfile(ctx, store, bob, original, fixture, receipt, primaryProfile()) == nil {
		t.Fatal("secondary scenario replayed as primary")
	}
	other, err := seedSubmission(fixture.Namespace, "synthetic-alert-"+receipt+"-first", "synthetic-slurm")
	if err != nil {
		t.Fatal(err)
	}
	wrongTarget, err := store.SubmitJob(ctx, bob, "secondary-target-substitution", other)
	if err != nil {
		t.Fatal(err)
	}
	altered := fixture
	altered.Jobs = append([]notificationJob(nil), fixture.Jobs...)
	altered.Jobs[0].JobID = wrongTarget.Job.ID
	if verifyNotificationFixtureProfile(ctx, store, bob, original, altered, receipt, profile) == nil {
		t.Fatal("same-name different-target job substituted")
	}
	var after []byte
	if err = pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(j) ORDER BY id) FROM jobs j WHERE id=ANY($1::uuid[])`, ids).Scan(&after); err != nil || !bytes.Equal(before, after) {
		t.Fatal("secondary scenario modified preexisting fixture jobs", err)
	}
}
