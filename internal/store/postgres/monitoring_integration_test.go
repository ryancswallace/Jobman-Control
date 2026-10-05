package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func TestMonitoringSnapshotsIntegration(t *testing.T) {
	databaseURL := os.Getenv("JOBMAN_CONTROL_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("JOBMAN_CONTROL_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool := newIntegrationPool(ctx, t, databaseURL)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := New(pool, []byte("0123456789abcdef0123456789abcdef"))
	principal := domain.Principal{Issuer: "test-issuer", Subject: "test-subject"}
	if err := store.EnsureDevelopmentIdentity(ctx, domain.DevelopmentIdentity{Principal: principal, DisplayName: "Submitter", Namespace: "research"}); err != nil {
		t.Fatal(err)
	}
	initial, err := store.Capabilities(ctx)
	if err != nil || !domain.IsID(initial.InstanceID) || initial.RecoveryEpoch != "1" || initial.ServerTime.IsZero() {
		t.Fatalf("capabilities=%#v,%v", initial, err)
	}
	if err = Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	repeated, err := store.Capabilities(ctx)
	if err != nil || repeated.InstanceID != initial.InstanceID {
		t.Fatalf("instance identity changed=%#v,%v", repeated, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE service_recovery_state SET restore_epoch=restore_epoch+1`); err != nil {
		t.Fatal(err)
	}
	restored, err := store.Capabilities(ctx)
	if err != nil || restored.InstanceID != initial.InstanceID || restored.RecoveryEpoch != "2" {
		t.Fatalf("restore=%#v,%v", restored, err)
	}
	if _, err = store.CreateTarget(ctx, principal, "research", "monitor-target", "sha256:"+strings.Repeat("1", 64), domain.TargetSpec{Name: "workstation-a", Kind: "host", ExecutionBackend: "subprocess", Runtimes: []string{"native"}, OperatingSystems: []string{"linux"}, Architectures: []string{"amd64"}}); err != nil {
		t.Fatal(err)
	}
	first, err := store.SubmitJob(ctx, principal, "monitor-one", integrationSubmission(t))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.SubmitJob(ctx, principal, "monitor-two", integrationSubmission(t))
	if err != nil {
		t.Fatal(err)
	}
	if first.Job.Owner == nil || first.Job.Owner.Subject != principal.Subject || first.Job.NamespaceID == "" || first.Job.Lifecycle.StartedAt != nil || first.Job.Lifecycle.CompletedAt != nil {
		t.Fatalf("new job metadata=%#v", first.Job)
	}
	terminal, err := store.CancelJob(ctx, principal, "research", first.Job.ID, "monitor-cancel", "sha256:"+strings.Repeat("2", 64))
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Lifecycle.CompletedAt == nil || terminal.Lifecycle.CompletedProvenance != "control_transition" || terminal.Lifecycle.StartedAt != nil {
		t.Fatalf("cancellation lifecycle=%#v", terminal.Lifecycle)
	}
	completed := *terminal.Lifecycle.CompletedAt
	if _, err = pool.Exec(ctx, `UPDATE jobs SET updated_at=updated_at+interval '1 hour',revision=revision+1 WHERE id=$1`, first.Job.ID); err != nil {
		t.Fatal(err)
	}
	refreshed, err := store.GetJob(ctx, principal, "research", first.Job.ID)
	if err != nil || !refreshed.Lifecycle.CompletedAt.Equal(completed) {
		t.Fatalf("metadata update changed completion=%#v,%v", refreshed, err)
	}
	history := domain.CompletedHistoryImport{Job: integrationSubmission(t), Outcome: "success", CompletedAt: completed.Add(-48 * time.Hour), SourceStore: "sqlite", SourceSchema: 1, SourceJobID: "history-monitoring", RequestDigest: "sha256:" + strings.Repeat("3", 64), RequestDocument: json.RawMessage(`{"kind":"CompletedHistoryImport"}`)}
	imported, err := store.ImportCompletedHistory(ctx, principal, "monitor-import", false, history)
	if err != nil {
		t.Fatal(err)
	}
	if !imported.Job.Imported || imported.Job.Owner != nil || imported.Job.CurrentRun != nil || imported.Job.Lifecycle.CompletedProvenance != "history_import" || !imported.Job.Lifecycle.CompletedAt.Equal(history.CompletedAt) {
		t.Fatalf("import facts=%#v", imported.Job)
	}
	from, before := completed.Add(-time.Minute), completed.Add(time.Minute)
	summary, err := store.NamespaceSummary(ctx, principal, "research", &from, &before)
	if err != nil || summary.Total != "3" || summary.Active != "1" || summary.AwaitingExecution != "1" || summary.ByOutcome[cancelledOutcome] != "1" || summary.ByOutcome["success"] != "" || summary.MissingCompletionTime != "0" {
		t.Fatalf("summary=%#v,%v", summary, err)
	}
	cases := []struct {
		name    string
		options domain.JobListOptions
		want    int
	}{
		{"active", domain.JobListOptions{Limit: 50, Phase: "active"}, 1},
		{"awaiting", domain.JobListOptions{Limit: 50, Phase: "awaiting"}, 1},
		{"window", domain.JobListOptions{Limit: 50, CompletedFrom: &from, CompletedBefore: &before}, 1},
		{"half-open", domain.JobListOptions{Limit: 50, CompletedFrom: &from, CompletedBefore: &completed}, 0},
		{"outcome", domain.JobListOptions{Limit: 50, Outcome: cancelledOutcome}, 1},
		{"unknown-outcome", domain.JobListOptions{Limit: 50, Outcome: "future"}, 0},
		{"owner-excludes-importer", domain.JobListOptions{Limit: 50, OwnerPrincipalID: first.Job.Owner.ID}, 2},
		{"exact-job", domain.JobListOptions{Limit: 50, JobID: second.Job.ID}, 1},
		{"creation-cutoff", domain.JobListOptions{Limit: 50, CreatedBefore: &first.Job.CreatedAt}, 1},
	}
	for _, test := range cases {
		page, listErr := store.ListJobs(ctx, principal, "research", test.options)
		if listErr != nil || len(page.Jobs) != test.want {
			t.Fatalf("%s jobs=%d,err=%v", test.name, len(page.Jobs), listErr)
		}
	}
	if _, err = store.NamespaceSummary(ctx, domain.Principal{Issuer: principal.Issuer, Subject: "outsider"}, "research", &from, &before); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("unauthorized summary=%v", err)
	}
	// Retained history with no reliable observed completion time remains missing.
	if _, err = pool.Exec(ctx, `UPDATE jobs SET completed_at=NULL,completed_recorded_at=NULL,completed_provenance=NULL WHERE id=$1`, first.Job.ID); err != nil {
		t.Fatal(err)
	}
	summary, err = store.NamespaceSummary(ctx, principal, "research", &from, &before)
	if err != nil || summary.MissingCompletionTime != "1" || len(summary.ByOutcome) != 0 {
		t.Fatalf("unknown completion summary=%#v,%v", summary, err)
	}
}
