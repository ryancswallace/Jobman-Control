package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ryancswallace/jobman-control/internal/domain"
	"github.com/ryancswallace/jobman-control/internal/store/postgres"
)

func TestNotificationScenarioPreflight(t *testing.T) {
	for _, receipt := range []string{"", "../other", strings.Repeat("A", 32), strings.Repeat("a", 33)} {
		if _, err := notificationScenario(t.Context(), t.TempDir(), "unread", diagnosticDeployment, receipt, "prepare", ""); err == nil {
			t.Fatal("invalid receipt reached preparation")
		}
	}
	for _, action := range []string{"reset", "delete", ""} {
		if _, err := notificationScenario(t.Context(), t.TempDir(), "unread", diagnosticDeployment, strings.Repeat("a", 32), action, ""); err == nil {
			t.Fatal("unknown mutation accepted")
		}
	}
}

func TestNotificationScenarioIntegration(t *testing.T) {
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
	schema := "notification_fixture_" + strings.ReplaceAll(id, "-", "")
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
	original, err := seed(ctx, store, testInput(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := store.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	original.InstanceID, original.Synthetic = capabilities.InstanceID, true
	var before []byte
	var ids []string
	if err = pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(j) ORDER BY id),array_agg(id::text ORDER BY id) FROM jobs j`).Scan(&before, &ids); err != nil {
		t.Fatal(err)
	}
	principal := domain.Principal{Issuer: testInput().Issuer, Subject: "alice"}
	fixture, err := seedNotificationScenario(ctx, store, principal, original, strings.Repeat("b", 32))
	if err != nil {
		t.Fatal(err)
	}
	first, err := completeNotificationScenario(ctx, pool, store, principal, fixture, "first")
	if err != nil || !domain.IsID(first.EventID) || first.Outcome != notificationCancelled || first.RecordedAt.IsZero() {
		t.Fatal("normal cancellation did not produce original terminal event", err)
	}
	replay, err := completeNotificationScenario(ctx, pool, store, principal, fixture, "first")
	left, leftErr := json.Marshal(first)
	right, rightErr := json.Marshal(replay)
	if leftErr != nil || rightErr != nil || err != nil || !bytes.Equal(left, right) {
		t.Fatal("uncertain cancellation retry changed original identity", err)
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE topic='monitoring.job_terminal.v1' AND aggregate_id=$1::uuid`, first.JobID).Scan(&count); err != nil || count != 1 {
		t.Fatal("repeated cancellation published duplicate transition", err)
	}
	untouched, err := store.GetJob(ctx, principal, fixture.Namespace, fixture.Jobs[1].JobID)
	if err != nil || untouched.Phase == "terminal" {
		t.Fatal("other prepared case was completed", err)
	}
	if _, err = store.PublishMonitoringEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM monitoring_feed WHERE event_id=$1::uuid AND namespace_id=$2::uuid`, first.EventID, fixture.NamespaceID).Scan(&count); err != nil || count != 1 {
		t.Fatal("real publisher did not preserve original event identity", err)
	}
	var after []byte
	if err = pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(j) ORDER BY id) FROM jobs j WHERE id=ANY($1::uuid[])`, ids).Scan(&after); err != nil || !bytes.Equal(before, after) {
		t.Fatal("scenario changed pre-existing jobs", err)
	}
	altered := fixture
	altered.RecoveryEpoch = "999"
	if verifyNotificationFixture(ctx, store, principal, original, altered, fixture.Receipt) == nil {
		t.Fatal("source recovery accepted stale scenario")
	}
	altered = fixture
	altered.Jobs = append([]notificationJob(nil), fixture.Jobs...)
	altered.Jobs[0].JobID = ids[0]
	if verifyNotificationFixture(ctx, store, principal, original, altered, fixture.Receipt) == nil {
		t.Fatal("unrelated job could replace scenario job")
	}
}
