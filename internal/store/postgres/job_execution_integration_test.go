package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	protocol "github.com/ryancswallace/jobman-control/contracts/jobman/v1alpha1"
	"github.com/ryancswallace/jobman-control/internal/domain"
)

func TestJobExecutionDetailIntegration(t *testing.T) {
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
	owner := domain.Principal{Issuer: "test-issuer", Subject: "execution-owner"}
	other := domain.Principal{Issuer: "test-issuer", Subject: "other-owner"}
	for _, entry := range []struct {
		namespace string
		principal domain.Principal
	}{{"research", owner}, {"other", other}} {
		if err := store.EnsureDevelopmentIdentity(ctx, domain.DevelopmentIdentity{Principal: entry.principal, DisplayName: "Synthetic", Namespace: entry.namespace}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateTarget(ctx, entry.principal, entry.namespace, "execution-target", "sha256:"+strings.Repeat("1", 64), domain.TargetSpec{Name: "workstation-a", Kind: "host", ExecutionBackend: "subprocess", Runtimes: []string{"native"}, OperatingSystems: []string{"linux"}, Architectures: []string{"amd64"}}); err != nil {
			t.Fatal(err)
		}
	}
	submission := integrationSubmission(t)
	var workload protocol.Workload
	if err := json.Unmarshal(submission.WorkloadDocument, &workload); err != nil {
		t.Fatal(err)
	}
	workload.Spec.Command = protocol.Command{Executable: "/bin/sh", Args: []string{"-c", "printf '%s' \"$1\"", "", "a b", "line\nnext"}}
	workload.Spec.WorkingDirectory = "workspace:/synthetic-work"
	workload.Spec.Environment = &protocol.Environment{Values: map[string]string{"SYNTHETIC_SECRET": "synthetic-env-never-project"}}
	sealed, err := protocol.SealWorkload(workload)
	if err != nil {
		t.Fatal(err)
	}
	submission.WorkloadDocument, submission.WorkloadDigest = sealed.CanonicalJSON, sealed.Digest
	created, err := store.SubmitJob(ctx, owner, "execution-detail", submission)
	if err != nil {
		t.Fatal(err)
	}
	if created.Job.Execution != nil {
		t.Fatal("submission exposed command")
	}
	copySubmission := submission
	copySubmission.Namespace = "other"
	otherJob, err := store.SubmitJob(ctx, other, "execution-detail", copySubmission)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt only the disposable other-namespace fixture to prove that equal
	// digests cannot make a read join across namespace ancestry.
	if _, err = pool.Exec(ctx, `UPDATE workload_revisions SET document=jsonb_set(document,'{spec,command,executable}','"wrong-namespace-command"') WHERE namespace_id=$1`, otherJob.Job.NamespaceID); err != nil {
		t.Fatal(err)
	}
	job, err := store.GetJob(ctx, owner, "research", created.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Execution == nil || job.ExecutionUnavailableReason != "" || job.Execution.Command.Executable != "/bin/sh" || !reflect.DeepEqual(job.Execution.Command.Args, workload.Spec.Command.Args) || job.Execution.WorkingDirectory != "workspace:/synthetic-work" {
		t.Fatal("job detail did not preserve submitted invocation")
	}
	encoded, err := json.Marshal(job.Execution)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "synthetic-env-never-project") || strings.Contains(string(encoded), "environment") {
		t.Fatal("execution projection exposed environment")
	}
	page, err := store.ListJobs(ctx, owner, "research", domain.JobListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Jobs) != 1 || page.Jobs[0].Execution != nil || page.Jobs[0].ExecutionUnavailableReason != "" {
		t.Fatal("list exposed execution detail")
	}
	for _, p := range []domain.Principal{owner, other} {
		namespace := "other"
		if p.Subject == other.Subject {
			namespace = "research"
		}
		if _, readErr := store.GetJob(ctx, p, namespace, created.Job.ID); !errors.Is(readErr, domain.ErrNotFound) {
			t.Fatalf("cross-namespace read was not denied: %v", readErr)
		}
	}
	_, err = inReadTransaction(ctx, pool, func(tx pgx.Tx) (bool, error) {
		for _, binding := range []struct{ namespace, id, digest string }{
			{created.Job.NamespaceID, created.Job.ID, "sha256:" + strings.Repeat("f", 64)},
			{otherJob.Job.NamespaceID, created.Job.ID, created.Job.WorkloadDigest},
			{created.Job.NamespaceID, otherJob.Job.ID, created.Job.WorkloadDigest},
		} {
			execution, reason, readErr := readJobExecution(ctx, tx, binding.namespace, binding.id, binding.digest)
			if readErr != nil {
				return false, readErr
			}
			if execution != nil || reason != "missing" {
				t.Fatal("execution lookup ignored namespace/job/digest binding")
			}
		}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, document := range []struct{ value, reason string }{
		{`{"spec":{"environment":{"values":{"SECRET":"synthetic-env-never-project"}}}}`, "missing"},
		{`{"spec":{"command":{"shell":{"capability":"sh","script":"true"}},"workingDirectory":"."}}`, "unsupported"},
		{`{"spec":{"command":{"executable":"true","args":[null]},"workingDirectory":"workspace:/"}}`, "unsupported"},
		{`{"spec":{"command":{"executable":"true","args":["` + strings.Repeat("x", maximumJobExecutionBytes) + `"]},"workingDirectory":"."}}`, "too_large"},
	} {
		if _, err = pool.Exec(ctx, `UPDATE workload_revisions SET document=$1::jsonb WHERE namespace_id=$2 AND digest=$3`, document.value, created.Job.NamespaceID, created.Job.WorkloadDigest); err != nil {
			t.Fatal(err)
		}
		got, readErr := store.GetJob(ctx, owner, "research", created.Job.ID)
		if readErr != nil || got.Execution != nil || got.ExecutionUnavailableReason != document.reason || got.ID != created.Job.ID || got.Phase != created.Job.Phase {
			t.Fatalf("unavailable execution failed to preserve status: %v", readErr)
		}
	}
	capabilities, err := store.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(capabilities.Features, "job-execution-detail") {
		t.Fatal("capability missing")
	}
}
