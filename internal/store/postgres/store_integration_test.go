package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	jobmanprotocol "github.com/ryancswallace/jobman-control/contracts/jobman/v1alpha1"
	"github.com/ryancswallace/jobman-control/internal/contracts"
	"github.com/ryancswallace/jobman-control/internal/domain"
)

const (
	cancelledOutcome            = "cancelled"              //nolint:misspell // Frozen v1alpha1 wire value.
	cancelledBeforeStartFailure = "cancelled_before_start" //nolint:misspell // Frozen v1alpha1 failure code.
)

func TestStoreIntegration(t *testing.T) {
	databaseURL := os.Getenv("JOBMAN_CONTROL_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("JOBMAN_CONTROL_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool := newIntegrationPool(ctx, t, databaseURL)

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("second Migrate() error = %v", err)
	}
	store := New(pool, []byte("0123456789abcdef0123456789abcdef"))
	principal := domain.Principal{Issuer: "test-issuer", Subject: "test-subject"}
	if err := store.EnsureDevelopmentIdentity(ctx, domain.DevelopmentIdentity{
		Principal: principal, DisplayName: "Test User", Namespace: "research",
	}); err != nil {
		t.Fatalf("EnsureDevelopmentIdentity() error = %v", err)
	}
	if err := store.Ready(ctx); err != nil {
		t.Fatalf("Ready() error = %v", err)
	}
	targetResult, err := store.CreateTarget(
		ctx, principal, "research", "target-create-001",
		"sha256:"+strings.Repeat("1", 64),
		domain.TargetSpec{
			Name: "workstation-a", Kind: "host", ExecutionBackend: "subprocess",
			Runtimes: []string{"native"}, OperatingSystems: []string{"linux"},
			Architectures: []string{"amd64"}, LogStoreName: "department-nfs", LogStoreVersion: 1,
		},
	)
	if err != nil {
		t.Fatalf("CreateTarget() error = %v", err)
	}
	targetReplay, err := store.CreateTarget(
		ctx, principal, "research", "target-create-001",
		"sha256:"+strings.Repeat("1", 64),
		domain.TargetSpec{
			Name: "workstation-a", Kind: "host", ExecutionBackend: "subprocess",
			Runtimes: []string{"native"}, OperatingSystems: []string{"linux"},
			Architectures: []string{"amd64"}, LogStoreName: "department-nfs", LogStoreVersion: 1,
		},
	)
	if err != nil || !targetReplay.Replayed || targetReplay.Value.ID != targetResult.Value.ID {
		t.Fatalf("CreateTarget() replay = %#v, %v", targetReplay, err)
	}
	targets, err := store.ListTargets(ctx, principal, "research")
	if err != nil || len(targets) != 1 || targets[0].GenerationID != targetResult.Value.GenerationID {
		t.Fatalf("ListTargets() = %#v, %v", targets, err)
	}

	enrollment, err := store.CreateEnrollmentToken(
		ctx, principal, "research", "workstation-a", "enrollment-001",
		"sha256:"+strings.Repeat("2", 64),
		domain.EnrollmentRequest{
			Principal: principal, ExpectedUser: "researcher", Lifetime: 10 * time.Minute,
		},
	)
	if err != nil || enrollment.Token == "" {
		t.Fatalf("CreateEnrollmentToken() = %#v, %v", enrollment, err)
	}
	registration := domain.AgentRegistration{
		TargetGenerationID: targetResult.Value.GenerationID,
		AgentVersion:       "0.1.0", ProtocolVersions: []string{"jobman/v1alpha1"},
		OperatingSystem: "linux", Architecture: "amd64", Hostname: "worker-a",
		ExecutionUser: "researcher", ExecutionBackends: []string{"subprocess"},
		Runtimes: []string{"native"}, Capabilities: []string{"process-groups"},
		RequestDigest: "sha256:" + strings.Repeat("3", 64),
	}
	session, err := store.EnrollAgent(ctx, enrollment.Token, registration, 15*time.Minute)
	if err != nil || session.Token == "" {
		t.Fatalf("EnrollAgent() = %#v, %v", session, err)
	}
	sessionReplay, err := store.EnrollAgent(ctx, enrollment.Token, registration, 15*time.Minute)
	if err != nil || !sessionReplay.Replayed || sessionReplay.SessionID != session.SessionID ||
		sessionReplay.Token != session.Token {
		t.Fatalf("EnrollAgent() replay = %#v, %v", sessionReplay, err)
	}
	submission := integrationSubmission(t)

	created, err := store.SubmitJob(ctx, principal, "integration-submit-001", submission)
	if err != nil {
		t.Fatalf("SubmitJob() error = %v", err)
	}
	if created.Replayed || created.Job.Phase != domain.JobPhaseAccepted {
		t.Fatalf("created result = %#v", created)
	}
	if created.Job.CreatedAt.Location() != time.UTC || created.Job.UpdatedAt.Location() != time.UTC {
		t.Fatalf("created timestamps are not UTC: %#v", created.Job)
	}
	replayed, err := store.SubmitJob(ctx, principal, "integration-submit-001", submission)
	if err != nil {
		t.Fatalf("SubmitJob() replay error = %v", err)
	}
	if !replayed.Replayed || replayed.Job.ID != created.Job.ID {
		t.Fatalf("replayed result = %#v, created ID = %q", replayed, created.Job.ID)
	}

	conflicting := submission
	conflicting.RequestDigest = "sha256:" + strings.Repeat("0", 64)
	if _, err = store.SubmitJob(ctx, principal, "integration-submit-001", conflicting); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("SubmitJob() conflict error = %v", err)
	}
	read, err := store.GetJob(ctx, principal, "research", created.Job.ID)
	if err != nil || read.ID != created.Job.ID || read.RequestDigest != submission.RequestDigest {
		t.Fatalf("GetJob() = %#v, %v", read, err)
	}
	if _, err = store.GetJob(
		ctx,
		domain.Principal{Issuer: "test-issuer", Subject: "other-subject"},
		"research",
		created.Job.ID,
	); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-principal GetJob() error = %v", err)
	}

	concurrentJobID := testConcurrentIdempotency(ctx, t, store, principal, submission)
	firstPage, err := store.ListJobs(ctx, principal, "research", domain.JobListOptions{Limit: 1})
	if err != nil || len(firstPage.Jobs) != 1 || firstPage.NextCursor == nil {
		t.Fatalf("ListJobs(first page) = %#v, %v", firstPage, err)
	}
	secondPage, err := store.ListJobs(ctx, principal, "research", domain.JobListOptions{
		Limit: 1, Before: firstPage.NextCursor,
	})
	if err != nil || len(secondPage.Jobs) != 1 || secondPage.Jobs[0].ID == firstPage.Jobs[0].ID {
		t.Fatalf("ListJobs(second page) = %#v, %v", secondPage, err)
	}
	if _, err = store.ListJobs(
		ctx, domain.Principal{Issuer: "test-issuer", Subject: "other-subject"},
		"research", domain.JobListOptions{Limit: 1},
	); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("cross-principal ListJobs() error = %v", err)
	}
	testAtomicAcceptanceRollback(ctx, t, pool, store, principal, submission)

	assigned, err := store.ReconcileAssignments(ctx, 10)
	if err != nil || assigned != 2 {
		t.Fatalf("ReconcileAssignments() = %d, %v", assigned, err)
	}
	if second, reconcileErr := store.ReconcileAssignments(ctx, 10); reconcileErr != nil || second != 0 {
		t.Fatalf("second ReconcileAssignments() = %d, %v", second, reconcileErr)
	}
	identity, err := store.AuthenticateAgent(ctx, session.Token)
	if err != nil || identity.AgentID != session.AgentID {
		t.Fatalf("AuthenticateAgent() = %#v, %v", identity, err)
	}
	assignments, err := store.ListAssignments(ctx, identity, 10)
	if err != nil || len(assignments) != 2 {
		t.Fatalf("ListAssignments() = %#v, %v", assignments, err)
	}
	redelivered, err := store.ListAssignments(ctx, identity, 10)
	if err != nil || len(redelivered) != 2 || redelivered[0].DeliveryID != assignments[0].DeliveryID {
		t.Fatalf("ListAssignments() redelivery = %#v, %v", redelivered, err)
	}
	if err = store.RecordAgentCertificate(ctx, session.AgentID, domain.AgentCertificate{
		Serial: "1001", PublicKeyDigest: "sha256:" + strings.Repeat("a", 64),
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("RecordAgentCertificate() error = %v", err)
	}
	certificateIdentity, err := store.AuthenticateAgentCertificate(
		ctx, session.AgentID, "1001", "sha256:"+strings.Repeat("a", 64),
	)
	if err != nil || certificateIdentity.CertificateSerial != "1001" {
		t.Fatalf("AuthenticateAgentCertificate() = %#v, %v", certificateIdentity, err)
	}
	if err = store.RotateAgentCertificate(ctx, certificateIdentity, domain.AgentCertificate{
		Serial: "1002", PublicKeyDigest: "sha256:" + strings.Repeat("b", 64),
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("RotateAgentCertificate() error = %v", err)
	}
	if _, err = store.AuthenticateAgentCertificate(
		ctx, session.AgentID, "1001", "sha256:"+strings.Repeat("a", 64),
	); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("replaced certificate authentication error = %v", err)
	}
	certificateIdentity, err = store.AuthenticateAgentCertificate(
		ctx, session.AgentID, "1002", "sha256:"+strings.Repeat("b", 64),
	)
	if err != nil {
		t.Fatalf("rotated certificate authentication error = %v", err)
	}

	assignmentByJob := make(map[string]domain.Assignment, len(assignments))
	for _, assignment := range assignments {
		sealed, decodeErr := jobmanprotocol.DecodeAgentAssignment(
			bytes.NewReader(assignment.Document), jobmanprotocol.DecodeLimits{},
		)
		if decodeErr != nil {
			t.Fatalf("DecodeAgentAssignment() error = %v", decodeErr)
		}
		assignmentByJob[sealed.Document.Spec.EffectiveExecution.Metadata.JobID] = assignment
	}
	testExecutionLifecycle(
		ctx, t, store, principal, certificateIdentity,
		assignmentByJob[created.Job.ID], created.Job.ID,
	)
	testAcceptedCancellation(
		ctx, t, store, principal, certificateIdentity,
		assignmentByJob[concurrentJobID], concurrentJobID,
	)
	renewed, err := store.RenewAgentSession(ctx, session.Token, 15*time.Minute)
	if err != nil || renewed.Token == "" || renewed.Token == session.Token {
		t.Fatalf("RenewAgentSession() = %#v, %v", renewed, err)
	}
	if _, err = store.AuthenticateAgent(ctx, session.Token); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("old agent token authentication error = %v", err)
	}
	if _, err = store.AuthenticateAgent(ctx, renewed.Token); err != nil {
		t.Fatalf("renewed agent token authentication error = %v", err)
	}
	completedJob, err := store.GetJob(ctx, principal, "research", created.Job.ID)
	if err != nil || completedJob.Phase != "terminal" || completedJob.TargetGenerationID != targetResult.Value.GenerationID {
		t.Fatalf("completed GetJob() = %#v, %v", completedJob, err)
	}

	assertTableCount(ctx, t, pool, "jobs", 2)
	assertTableCount(ctx, t, pool, "idempotency_records", 5)
	assertTableCount(ctx, t, pool, "outbox", 4)
	assertTableCount(ctx, t, pool, "audit_events", 14)
	assertTableCount(ctx, t, pool, "assignments", 2)
	assertTableCount(ctx, t, pool, "log_streams", 4)
	assertTableCount(ctx, t, pool, "log_chunks", 5)
}

func TestStoreSlurmExecutionIntegration(t *testing.T) {
	databaseURL := os.Getenv("JOBMAN_CONTROL_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("JOBMAN_CONTROL_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool := newIntegrationPool(ctx, t, databaseURL)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	store := New(pool, []byte("0123456789abcdef0123456789abcdef"))
	principal := domain.Principal{Issuer: "test-issuer", Subject: "slurm-subject"}
	if err := store.EnsureDevelopmentIdentity(ctx, domain.DevelopmentIdentity{
		Principal: principal, DisplayName: "Slurm User", Namespace: "research",
	}); err != nil {
		t.Fatalf("EnsureDevelopmentIdentity() error = %v", err)
	}
	target, err := store.CreateTarget(
		ctx, principal, "research", "slurm-target-create",
		"sha256:"+strings.Repeat("7", 64),
		domain.TargetSpec{
			Name: "department-slurm", Kind: "slurm", ExecutionBackend: "slurm",
			Runtimes: []string{"native"}, OperatingSystems: []string{"linux"},
			Architectures: []string{"amd64"},
			Capabilities:  []string{"slurm-accounting", "slurm-cli"},
			Partitions:    []domain.PartitionSpec{{Name: "gpu", IsDefault: true}},
		},
	)
	if err != nil {
		t.Fatalf("CreateTarget() error = %v", err)
	}
	enrollment, err := store.CreateEnrollmentToken(
		ctx, principal, "research", "department-slurm", "slurm-enrollment",
		"sha256:"+strings.Repeat("8", 64),
		domain.EnrollmentRequest{
			Principal: principal, ExpectedUser: "researcher", Lifetime: 10 * time.Minute,
		},
	)
	if err != nil {
		t.Fatalf("CreateEnrollmentToken() error = %v", err)
	}
	session, err := store.EnrollAgent(ctx, enrollment.Token, domain.AgentRegistration{
		TargetGenerationID: target.Value.GenerationID,
		AgentVersion:       "0.1.0", ProtocolVersions: []string{"jobman/v1alpha1"},
		OperatingSystem: "linux", Architecture: "amd64", Hostname: "submit-a",
		ExecutionUser: "researcher", ExecutionBackends: []string{"slurm"},
		Runtimes:      []string{"native"},
		Capabilities:  []string{"slurm-accounting", "slurm-cli"},
		RequestDigest: "sha256:" + strings.Repeat("9", 64),
	}, 15*time.Minute)
	if err != nil {
		t.Fatalf("EnrollAgent() error = %v", err)
	}
	submission := integrationSubmission(t)
	submission.Name = "slurm-job"
	submission.Target = "department-slurm"
	submission.Partition = "gpu"
	submission.RequestDigest = "sha256:" + strings.Repeat("6", 64)
	created, err := store.SubmitJob(ctx, principal, "slurm-submit", submission)
	if err != nil {
		t.Fatalf("SubmitJob() error = %v", err)
	}
	if count, reconcileErr := store.ReconcileAssignments(ctx, 1); reconcileErr != nil || count != 1 {
		t.Fatalf("ReconcileAssignments() = %d, %v", count, reconcileErr)
	}
	identity, err := store.AuthenticateAgent(ctx, session.Token)
	if err != nil {
		t.Fatalf("AuthenticateAgent() error = %v", err)
	}
	assignments, err := store.ListAssignments(ctx, identity, 1)
	if err != nil || len(assignments) != 1 {
		t.Fatalf("ListAssignments() = %#v, %v", assignments, err)
	}
	if _, err = store.AcceptAssignment(ctx, identity, integrationAcceptance(t, assignments[0], identity)); err != nil {
		t.Fatalf("AcceptAssignment() error = %v", err)
	}
	for _, event := range []domain.ExecutionObservation{
		integrationSchedulerEvent(
			t, identity, assignments[0].ExecutionID, 1, "scheduler.submitted",
			"12345", "queued", "Resources", "alpha", nil,
		),
		integrationSchedulerEvent(
			t, identity, assignments[0].ExecutionID, 2, "scheduler.observed",
			"12345", "running", "", "alpha", nil,
		),
		integrationSchedulerEvent(
			t, identity, assignments[0].ExecutionID, 3, "scheduler.completed",
			"12345", "completed", "", "alpha",
			&jobmanprotocol.ProcessResult{Outcome: "success", ExitCode: testIntPointer(0)},
		),
	} {
		if replayed, recordErr := store.RecordExecutionEvent(ctx, identity, event); recordErr != nil || replayed {
			t.Fatalf("RecordExecutionEvent(%s) = %t, %v", event.Type, replayed, recordErr)
		}
	}
	job, err := store.GetJob(ctx, principal, "research", created.Job.ID)
	if err != nil || job.Phase != "terminal" || job.Outcome != "success" ||
		job.NativeID != "12345" || job.Scheduler == nil ||
		job.Scheduler.State != "completed" || job.Scheduler.Cluster != "alpha" {
		t.Fatalf("GetJob() = %#v, %v", job, err)
	}
}

func TestTargetGenerationCollectionsAndArraysIntegration(t *testing.T) {
	databaseURL := os.Getenv("JOBMAN_CONTROL_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("JOBMAN_CONTROL_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool := newIntegrationPool(ctx, t, databaseURL)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	store := New(pool, []byte("0123456789abcdef0123456789abcdef"))
	principal := domain.Principal{Issuer: "test-issuer", Subject: "collection-subject"}
	if err := store.EnsureDevelopmentIdentity(ctx, domain.DevelopmentIdentity{
		Principal: principal, DisplayName: "Collection User", Namespace: "research",
	}); err != nil {
		t.Fatal(err)
	}
	targetSpec := domain.TargetSpec{
		Name: "elastic-slurm", Kind: "slurm", ExecutionBackend: "slurm",
		Runtimes: []string{"native"}, OperatingSystems: []string{"linux"},
		Architectures: []string{"amd64"}, Capabilities: []string{"slurm-accounting", "slurm-cli"},
		Partitions: []domain.PartitionSpec{{Name: "gpu", IsDefault: true}},
		Provider:   domain.TargetProvider{Kind: "on-prem"},
	}
	target, err := store.CreateTarget(
		ctx, principal, "research", "generation-target", "sha256:"+strings.Repeat("1", 64), targetSpec,
	)
	if err != nil {
		t.Fatal(err)
	}
	oldSession := enrollIntegrationAgent(
		ctx, t, store, principal, target.Value, "old-generation-enrollment", "a",
	)
	oldSubmission := integrationSubmission(t)
	oldSubmission.Name = "old-generation-job"
	oldSubmission.Target = "elastic-slurm"
	oldSubmission.Partition = "gpu"
	oldSubmission.RequestDigest = "sha256:" + strings.Repeat("2", 64)
	oldJob, err := store.SubmitJob(ctx, principal, "old-generation-job", oldSubmission)
	if err != nil {
		t.Fatal(err)
	}

	nextSpec := targetSpec
	nextSpec.Provider = domain.TargetProvider{
		Kind: "aws-parallelcluster", Region: "us-east-1", ClusterName: "research-hpc",
	}
	next, err := store.CreateTargetGeneration(
		ctx, principal, "research", "elastic-slurm", "generation-rollover",
		"sha256:"+strings.Repeat("3", 64),
		domain.TargetGenerationChange{Spec: nextSpec, ExpectedRevision: 1},
	)
	if err != nil || next.Value.Generation != 2 || next.Value.Revision != 2 ||
		next.Value.Provider.Kind != "aws-parallelcluster" ||
		next.Value.GenerationID == target.Value.GenerationID {
		t.Fatalf("CreateTargetGeneration() = %#v, %v", next, err)
	}
	if _, err = store.CreateTargetGeneration(
		ctx, principal, "research", "elastic-slurm", "stale-generation-rollover",
		"sha256:"+strings.Repeat("4", 64),
		domain.TargetGenerationChange{Spec: nextSpec, ExpectedRevision: 1},
	); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale CreateTargetGeneration() error = %v", err)
	}
	if count, reconcileErr := store.ReconcileAssignments(ctx, 1); reconcileErr != nil || count != 1 {
		t.Fatalf("old-generation ReconcileAssignments() = %d, %v", count, reconcileErr)
	}
	oldIdentity, err := store.AuthenticateAgent(ctx, oldSession.Token)
	if err != nil {
		t.Fatal(err)
	}
	oldAssignments, err := store.ListAssignments(ctx, oldIdentity, 10)
	if err != nil || len(oldAssignments) != 1 {
		t.Fatalf("old-generation assignments = %#v, %v", oldAssignments, err)
	}
	oldDocument, err := jobmanprotocol.DecodeAgentAssignment(
		bytes.NewReader(oldAssignments[0].Document), jobmanprotocol.DecodeLimits{},
	)
	if err != nil || oldDocument.Document.Spec.EffectiveExecution.Metadata.JobID != oldJob.Job.ID ||
		oldDocument.Document.Spec.EffectiveExecution.Spec.Placement.TargetGenerationID != target.Value.GenerationID {
		t.Fatalf("old-generation assignment = %#v, %v", oldDocument, err)
	}

	newSession := enrollIntegrationAgent(
		ctx, t, store, principal, next.Value, "new-generation-enrollment", "b",
	)
	arraySubmission := integrationCollectionSubmission(t, "array", "require", 1)
	arrayResult, err := store.SubmitCollection(ctx, principal, "array-collection", arraySubmission)
	if err != nil || arrayResult.Collection.ArrayMode != "slurm-array" || len(arrayResult.Collection.Items) != 2 {
		t.Fatalf("SubmitCollection(array) = %#v, %v", arrayResult, err)
	}
	if count, reconcileErr := store.ReconcileAssignments(ctx, 10); reconcileErr != nil || count != 2 {
		t.Fatalf("array ReconcileAssignments() = %d, %v", count, reconcileErr)
	}
	newIdentity, err := store.AuthenticateAgent(ctx, newSession.Token)
	if err != nil {
		t.Fatal(err)
	}
	arrayAssignments, err := store.ListAssignments(ctx, newIdentity, 10)
	if err != nil || len(arrayAssignments) != 2 {
		t.Fatalf("array assignments = %#v, %v", arrayAssignments, err)
	}
	seenIndexes := map[int]bool{}
	for _, assignment := range arrayAssignments {
		document, decodeErr := jobmanprotocol.DecodeAgentAssignment(
			bytes.NewReader(assignment.Document), jobmanprotocol.DecodeLimits{},
		)
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		binding := document.Document.Spec.EffectiveExecution.Metadata.SlurmArray
		if binding == nil || binding.CollectionID != arrayResult.Collection.ID ||
			binding.TaskCount != 2 || binding.MaxParallel != 1 {
			t.Fatalf("array binding = %#v", binding)
		}
		seenIndexes[binding.TaskIndex] = true
	}
	if !seenIndexes[0] || !seenIndexes[1] {
		t.Fatalf("array indexes = %#v", seenIndexes)
	}

	failFastSubmission := integrationCollectionSubmission(t, "fail-fast", "never", 2)
	failFastSubmission.FailurePolicy = "fail-fast"
	failFastResult, err := store.SubmitCollection(ctx, principal, "fail-fast-collection", failFastSubmission)
	if err != nil {
		t.Fatal(err)
	}
	if count, reconcileErr := store.ReconcileAssignments(ctx, 10); reconcileErr != nil || count != 2 {
		t.Fatalf("fail-fast ReconcileAssignments() = %d, %v", count, reconcileErr)
	}
	allAssignments, err := store.ListAssignments(ctx, newIdentity, 20)
	if err != nil {
		t.Fatal(err)
	}
	failFastJobs := make(map[string]domain.Assignment)
	for _, assignment := range allAssignments {
		document, decodeErr := jobmanprotocol.DecodeAgentAssignment(
			bytes.NewReader(assignment.Document), jobmanprotocol.DecodeLimits{},
		)
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		for _, item := range failFastResult.Collection.Items {
			if document.Document.Spec.EffectiveExecution.Metadata.JobID == item.Job.ID {
				failFastJobs[item.Job.ID] = assignment
			}
		}
	}
	failedItem := failFastResult.Collection.Items[0]
	failedAssignment := failFastJobs[failedItem.Job.ID]
	if failedAssignment.ExecutionID == "" {
		t.Fatalf("fail-fast assignments = %#v", failFastJobs)
	}
	if _, err = store.AcceptAssignment(
		ctx, newIdentity, integrationAcceptance(t, failedAssignment, newIdentity),
	); err != nil {
		t.Fatal(err)
	}
	if _, err = store.RecordExecutionEvent(ctx, newIdentity, integrationSchedulerEvent(
		t, newIdentity, failedAssignment.ExecutionID, 1, "scheduler.submitted",
		"222_0", "queued", "", "alpha", nil,
	)); err != nil {
		t.Fatal(err)
	}
	if _, err = store.RecordExecutionEvent(ctx, newIdentity, integrationSchedulerEvent(
		t, newIdentity, failedAssignment.ExecutionID, 2, "scheduler.completed",
		"222_0", "failed", "NonZeroExitCode", "alpha",
		&jobmanprotocol.ProcessResult{Outcome: "failure", ExitCode: testIntPointer(1)},
	)); err != nil {
		t.Fatal(err)
	}
	failFastCollection, err := store.GetCollection(ctx, principal, "research", failFastResult.Collection.ID)
	if err != nil || failFastCollection.Phase != "terminal" || failFastCollection.Failed != 1 ||
		failFastCollection.Canceled != 1 || failFastCollection.Outcome != "failure" {
		t.Fatalf("fail-fast collection = %#v, %v", failFastCollection, err)
	}
}

func TestGraphsAndProductionControlsIntegration(t *testing.T) {
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
	principal := domain.Principal{Issuer: "test-issuer", Subject: "graph-subject"}
	if err := store.EnsureDevelopmentIdentity(ctx, domain.DevelopmentIdentity{
		Principal: principal, DisplayName: "Graph User", Namespace: "research",
	}); err != nil {
		t.Fatal(err)
	}
	target, err := store.CreateTarget(
		ctx, principal, "research", "graph-target", "sha256:"+strings.Repeat("1", 64),
		domain.TargetSpec{
			Name: "workstation-a", Kind: "host", ExecutionBackend: "subprocess",
			Runtimes: []string{"native"}, OperatingSystems: []string{"linux"},
			Architectures: []string{"amd64"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := store.CreateEnrollmentToken(
		ctx, principal, "research", "workstation-a", "graph-enrollment",
		"sha256:"+strings.Repeat("2", 64), domain.EnrollmentRequest{
			Principal: principal, ExpectedUser: "researcher", Lifetime: 10 * time.Minute,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.EnrollAgent(ctx, enrollment.Token, domain.AgentRegistration{
		TargetGenerationID: target.Value.GenerationID, AgentVersion: "0.1.0",
		ProtocolVersions: []string{"jobman/v1alpha1"}, OperatingSystem: "linux",
		Architecture: "amd64", Hostname: "graph-worker", ExecutionUser: "researcher",
		ExecutionBackends: []string{"subprocess"}, Runtimes: []string{"native"},
		Capabilities: []string{"process-groups"}, RequestDigest: "sha256:" + strings.Repeat("3", 64),
	}, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := store.AuthenticateAgent(ctx, session.Token)
	if err != nil {
		t.Fatal(err)
	}
	nodes := make([]domain.JobSubmission, 3)
	for index, name := range []string{"first", "success-path", "failure-path"} {
		nodes[index] = integrationSubmission(t)
		nodes[index].Name = name
		nodes[index].RequestDigest = fmt.Sprintf("sha256:%064x", index+30)
	}
	graphResult, err := store.SubmitGraph(ctx, principal, "graph-submit", domain.GraphSubmission{
		Namespace: "research", Name: "cross-target-pipeline", MaxActive: 2,
		UnsatisfiedPolicy: "skip", RequestDigest: "sha256:" + strings.Repeat("4", 64),
		RequestDocument: json.RawMessage(`{"kind":"GraphRequest"}`), Nodes: nodes,
		Edges: []domain.GraphEdgeSubmission{
			{From: "first", To: "success-path", Predicate: "success"},
			{From: "first", To: "failure-path", Predicate: "failure"},
		},
	})
	if err != nil || graphResult.Graph.Total != 3 || len(graphResult.Graph.Items) != 3 {
		t.Fatalf("SubmitGraph() = %#v, %v", graphResult, err)
	}
	if replay, replayErr := store.SubmitGraph(ctx, principal, "graph-submit", domain.GraphSubmission{
		Namespace: "research", Name: "cross-target-pipeline", MaxActive: 2,
		UnsatisfiedPolicy: "skip", RequestDigest: "sha256:" + strings.Repeat("4", 64),
		RequestDocument: json.RawMessage(`{"kind":"GraphRequest"}`), Nodes: nodes,
		Edges: []domain.GraphEdgeSubmission{
			{From: "first", To: "success-path", Predicate: "success"},
			{From: "first", To: "failure-path", Predicate: "failure"},
		},
	}); replayErr != nil || !replay.Replayed || replay.Graph.ID != graphResult.Graph.ID {
		t.Fatalf("SubmitGraph() replay = %#v, %v", replay, replayErr)
	}
	if count, reconcileErr := store.ReconcileAssignments(ctx, 10); reconcileErr != nil || count != 1 {
		t.Fatalf("root graph ReconcileAssignments() = %d, %v", count, reconcileErr)
	}
	assignments, err := store.ListAssignments(ctx, identity, 10)
	if err != nil || len(assignments) != 1 {
		t.Fatalf("root graph assignments = %#v, %v", assignments, err)
	}
	if _, err = store.AcceptAssignment(ctx, identity, integrationAcceptance(t, assignments[0], identity)); err != nil {
		t.Fatal(err)
	}
	if _, err = store.RecordExecutionEvent(ctx, identity, integrationEvent(
		t, identity, assignments[0].ExecutionID, 1, "process.started", "123", nil,
	)); err != nil {
		t.Fatal(err)
	}
	if _, err = store.RecordExecutionEvent(ctx, identity, integrationEvent(
		t, identity, assignments[0].ExecutionID, 2, "process.completed", "",
		&jobmanprotocol.ProcessResult{Outcome: "failure", ExitCode: testIntPointer(1)},
	)); err != nil {
		t.Fatal(err)
	}
	graph, err := store.GetGraph(ctx, principal, "research", graphResult.Graph.ID)
	if err != nil || graph.Terminal != 2 || graph.Failed != 1 || graph.Skipped != 1 ||
		graph.Items[1].Disposition != "skipped" || graph.Items[2].Dependencies[0].Satisfied != true {
		t.Fatalf("GetGraph(after root failure) = %#v, %v", graph, err)
	}
	if count, reconcileErr := store.ReconcileAssignments(ctx, 10); reconcileErr != nil || count != 1 {
		t.Fatalf("ready graph ReconcileAssignments() = %d, %v", count, reconcileErr)
	}
	canceled, err := store.CancelGraph(
		ctx, principal, "research", graph.ID, "graph-cancel", "sha256:"+strings.Repeat("5", 64),
	)
	if err != nil || canceled.Phase != "terminal" || canceled.Canceled != 1 || canceled.Outcome != "failure" {
		t.Fatalf("CancelGraph() = %#v, %v", canceled, err)
	}

	policy, err := store.GetNamespacePolicy(ctx, principal, "research")
	if err != nil || policy.MaxGraphNodes != 10_000 {
		t.Fatalf("GetNamespacePolicy() = %#v, %v", policy, err)
	}
	policy, err = store.UpdateNamespacePolicy(ctx, principal, "research", domain.NamespacePolicyChange{
		MaxActiveJobs: 1, MaxQueuedJobs: 1, MaxCollectionItems: 1, MaxGraphNodes: 2,
		IdempotencyRetention: time.Hour, PublishedOutboxRetention: time.Hour,
		ExpectedRevision: policy.Revision,
	})
	if err != nil || policy.Revision != 2 || policy.MaxQueuedJobs != 1 {
		t.Fatalf("UpdateNamespacePolicy() = %#v, %v", policy, err)
	}
	collection := integrationCollectionSubmission(t, "over-quota", "never", 1)
	for index := range collection.Items {
		collection.Items[index].Target = "workstation-a"
		collection.Items[index].Partition = ""
		collection.Items[index].ExecutionFeatures.Resources = false
	}
	if _, err = store.SubmitCollection(ctx, principal, "over-quota", collection); !errors.Is(err, domain.ErrQuotaExceeded) {
		t.Fatalf("SubmitCollection(over quota) error = %v", err)
	}

	history := domain.CompletedHistoryImport{
		Job: integrationSubmission(t), Outcome: "success", CompletedAt: time.Now().UTC().Add(-time.Hour),
		SourceStore: "sqlite", SourceSchema: 1, SourceJobID: "local-job-001",
		RequestDigest: "sha256:" + strings.Repeat("6", 64), RequestDocument: json.RawMessage(`{"kind":"CompletedHistoryImport"}`),
	}
	if plan, planErr := store.ImportCompletedHistory(ctx, principal, "", true, history); planErr != nil || !plan.DryRun {
		t.Fatalf("ImportCompletedHistory(dry run) = %#v, %v", plan, planErr)
	}
	imported, err := store.ImportCompletedHistory(ctx, principal, "history-import", false, history)
	if err != nil || imported.Job.Phase != "terminal" || imported.Job.Outcome != "success" {
		t.Fatalf("ImportCompletedHistory() = %#v, %v", imported, err)
	}
	if _, err = store.ImportCompletedHistory(ctx, principal, "history-import-second", false, history); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate history provenance error = %v", err)
	}

	audit, err := store.ExportAudit(ctx, principal, "research", 0, 1000)
	if err != nil || len(audit.Items) == 0 {
		t.Fatalf("ExportAudit() = %#v, %v", audit, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE service_recovery_state SET reconciliation_hold = true, reason = 'restore test'`); err != nil {
		t.Fatal(err)
	}
	if count, reconcileErr := store.ReconcileAssignments(ctx, 10); reconcileErr != nil || count != 0 {
		t.Fatalf("held ReconcileAssignments() = %d, %v", count, reconcileErr)
	}
	snapshot, err := store.OperationalSnapshot(ctx)
	if err != nil || !snapshot.RecoveryHold || snapshot.RestoreEpoch != 1 || snapshot.JobsByPhase["terminal"] != 4 {
		t.Fatalf("OperationalSnapshot() = %#v, %v", snapshot, err)
	}
	if _, err = pool.Exec(ctx, `
		UPDATE idempotency_records SET completed_at = transaction_timestamp() - interval '2 hours'
		WHERE completed_at IS NOT NULL;
		UPDATE outbox SET published_at = transaction_timestamp() - interval '2 hours';
	`); err != nil {
		t.Fatal(err)
	}
	if removed, pruneErr := store.PruneOperationalData(ctx, 1000); pruneErr != nil || removed == 0 {
		t.Fatalf("PruneOperationalData() = %d, %v", removed, pruneErr)
	}
}

func enrollIntegrationAgent(
	ctx context.Context,
	t *testing.T,
	store *Store,
	principal domain.Principal,
	target domain.Target,
	key, digestCharacter string,
) domain.AgentSession {
	t.Helper()
	enrollment, err := store.CreateEnrollmentToken(
		ctx, principal, "research", target.Name, key,
		"sha256:"+strings.Repeat(digestCharacter, 64), domain.EnrollmentRequest{
			Principal: principal, ExpectedUser: "researcher", Lifetime: 10 * time.Minute,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.EnrollAgent(ctx, enrollment.Token, domain.AgentRegistration{
		TargetGenerationID: target.GenerationID,
		AgentVersion:       "0.1.0", ProtocolVersions: []string{"jobman/v1alpha1"},
		OperatingSystem: "linux", Architecture: "amd64", Hostname: "submit-" + digestCharacter,
		ExecutionUser: "researcher", ExecutionBackends: []string{"slurm"}, Runtimes: []string{"native"},
		Capabilities:  []string{"slurm-accounting", "slurm-cli"},
		RequestDigest: "sha256:" + strings.Repeat(digestCharacter, 64),
	}, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	return session
}

func integrationCollectionSubmission(
	t *testing.T,
	name, arrayPolicy string,
	maxActive int,
) domain.CollectionSubmission {
	t.Helper()
	items := make([]domain.JobSubmission, 2)
	for index := range items {
		items[index] = integrationSubmission(t)
		items[index].Name = fmt.Sprintf("%s-%d", name, index)
		items[index].Target = "elastic-slurm"
		items[index].Partition = "gpu"
		items[index].RequestDigest = fmt.Sprintf("sha256:%064x", index+10)
	}

	return domain.CollectionSubmission{
		Namespace: "research", Name: name, MaxActive: maxActive,
		FailurePolicy: "continue", ArrayPolicy: arrayPolicy,
		RequestDigest: "sha256:" + strings.Repeat("e", 64), RequestDocument: json.RawMessage(`{}`),
		Items: items,
	}
}

func TestStoreArtifactIntegration(t *testing.T) {
	databaseURL := os.Getenv("JOBMAN_CONTROL_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("JOBMAN_CONTROL_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool := newIntegrationPool(ctx, t, databaseURL)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	store := New(pool, []byte("0123456789abcdef0123456789abcdef"))
	principal := domain.Principal{Issuer: "test-issuer", Subject: "artifact-subject"}
	if err := store.EnsureDevelopmentIdentity(ctx, domain.DevelopmentIdentity{
		Principal: principal, DisplayName: "Artifact User", Namespace: "research",
	}); err != nil {
		t.Fatal(err)
	}
	target, err := store.CreateTarget(
		ctx, principal, "research", "artifact-target", "sha256:"+strings.Repeat("1", 64),
		domain.TargetSpec{
			Name: "workstation-a", Kind: "host", ExecutionBackend: "subprocess",
			Runtimes: []string{"native"}, OperatingSystems: []string{"linux"},
			Architectures:  []string{"amd64"},
			ArtifactStores: []domain.ArtifactStoreSpec{{Name: "department-nfs", Version: 3}},
		},
	)
	if err != nil || len(target.Value.ArtifactStores) != 1 {
		t.Fatalf("CreateTarget() = %#v, %v", target, err)
	}
	enrollment, err := store.CreateEnrollmentToken(
		ctx, principal, "research", "workstation-a", "artifact-enrollment",
		"sha256:"+strings.Repeat("2", 64), domain.EnrollmentRequest{
			Principal: principal, ExpectedUser: "researcher", Lifetime: 10 * time.Minute,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.EnrollAgent(ctx, enrollment.Token, domain.AgentRegistration{
		TargetGenerationID: target.Value.GenerationID,
		AgentVersion:       "0.1.0", ProtocolVersions: []string{"jobman/v1alpha1"},
		OperatingSystem: "linux", Architecture: "amd64", Hostname: "worker-a",
		ExecutionUser: "researcher", ExecutionBackends: []string{"subprocess"},
		Runtimes: []string{"native"}, Capabilities: []string{"artifact-filesystem"},
		RequestDigest: "sha256:" + strings.Repeat("3", 64),
	}, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	submission := integrationArtifactSubmission(t)
	created, err := store.SubmitJob(ctx, principal, "artifact-submit", submission)
	if err != nil {
		t.Fatalf("SubmitJob() error = %v", err)
	}
	if count, reconcileErr := store.ReconcileAssignments(ctx, 1); reconcileErr != nil || count != 1 {
		t.Fatalf("ReconcileAssignments() = %d, %v", count, reconcileErr)
	}
	identity, err := store.AuthenticateAgent(ctx, session.Token)
	if err != nil {
		t.Fatal(err)
	}
	assignments, err := store.ListAssignments(ctx, identity, 1)
	if err != nil || len(assignments) != 1 {
		t.Fatalf("ListAssignments() = %#v, %v", assignments, err)
	}
	sealed, err := jobmanprotocol.DecodeAgentAssignment(
		bytes.NewReader(assignments[0].Document), jobmanprotocol.DecodeLimits{},
	)
	if err != nil || len(sealed.Document.Spec.EffectiveExecution.Spec.ArtifactStores) != 1 ||
		sealed.Document.Spec.EffectiveExecution.Spec.ArtifactStores[0].Version != 3 {
		t.Fatalf("effective artifact stores = %#v, %v", sealed.Document.Spec.EffectiveExecution.Spec.ArtifactStores, err)
	}
	if _, err = store.AcceptAssignment(ctx, identity, integrationAcceptance(t, assignments[0], identity)); err != nil {
		t.Fatal(err)
	}
	started := integrationEvent(t, identity, assignments[0].ExecutionID, 1, "process.started", "4242", nil)
	if _, err = store.RecordExecutionEvent(ctx, identity, started); err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	artifact := jobmanprotocol.PublishedArtifact{
		Name: "result", StoreName: "department-nfs", StoreVersion: 3,
		ObjectKey: "research/results/result.txt", ByteLength: 7,
		Checksum: "sha256:" + strings.Repeat("a", 64),
	}
	completed := integrationEvent(
		t, identity, assignments[0].ExecutionID, 2, "process.completed", "",
		&jobmanprotocol.ProcessResult{Outcome: "success", ExitCode: &exitCode}, artifact,
	)
	if _, err = store.RecordExecutionEvent(ctx, identity, completed); err != nil {
		t.Fatalf("RecordExecutionEvent(completed) error = %v", err)
	}
	job, err := store.GetJob(ctx, principal, "research", created.Job.ID)
	if err != nil || job.Phase != "terminal" || job.Outcome != "success" {
		t.Fatalf("GetJob() = %#v, %v", job, err)
	}
	var count int
	var key string
	if err = pool.QueryRow(ctx, `
		SELECT count(*), min(object_key) FROM execution_artifacts WHERE execution_id = $1
	`, assignments[0].ExecutionID).Scan(&count, &key); err != nil || count != 1 || key != artifact.ObjectKey {
		t.Fatalf("execution artifact = %d/%q, %v", count, key, err)
	}
	artifacts, err := store.GetJobArtifacts(ctx, principal, "research", created.Job.ID)
	if err != nil || len(artifacts) != 1 || artifacts[0].ExecutionID != assignments[0].ExecutionID ||
		artifacts[0].Name != artifact.Name || artifacts[0].Checksum != artifact.Checksum {
		t.Fatalf("GetJobArtifacts() = %#v, %v", artifacts, err)
	}
}

func testConcurrentIdempotency(
	ctx context.Context,
	t *testing.T,
	store *Store,
	principal domain.Principal,
	submission domain.JobSubmission,
) string {
	t.Helper()
	const callers = 8
	var waitGroup sync.WaitGroup
	var created atomic.Int32
	var replayed atomic.Int32
	errorsChannel := make(chan error, callers)
	identifiers := make(chan string, callers)
	for range callers {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			result, err := store.SubmitJob(ctx, principal, "integration-concurrent", submission)
			if err != nil {
				errorsChannel <- err

				return
			}
			identifiers <- result.Job.ID
			if result.Replayed {
				replayed.Add(1)
			} else {
				created.Add(1)
			}
		}()
	}
	waitGroup.Wait()
	close(errorsChannel)
	close(identifiers)
	for err := range errorsChannel {
		t.Errorf("concurrent SubmitJob() error = %v", err)
	}
	var identifier string
	for current := range identifiers {
		if identifier == "" {
			identifier = current
		}
		if current != identifier {
			t.Errorf("concurrent job ID = %q, want %q", current, identifier)
		}
	}
	if created.Load() != 1 || replayed.Load() != callers-1 {
		t.Errorf("concurrent results created=%d replayed=%d", created.Load(), replayed.Load())
	}

	return identifier
}

func testExecutionLifecycle(
	ctx context.Context,
	t *testing.T,
	store *Store,
	principal domain.Principal,
	identity domain.AgentIdentity,
	assignment domain.Assignment,
	jobID string,
) {
	t.Helper()
	acceptance := integrationAcceptance(t, assignment, identity)
	authorization, err := store.AcceptAssignment(ctx, identity, acceptance)
	if err != nil || authorization.Replayed || authorization.Revision < 2 {
		t.Fatalf("AcceptAssignment() = %#v, %v", authorization, err)
	}
	replayed, err := store.AcceptAssignment(ctx, identity, acceptance)
	if err != nil || !replayed.Replayed || replayed.AuthorizationID != authorization.AuthorizationID {
		t.Fatalf("AcceptAssignment(replay) = %#v, %v", replayed, err)
	}
	started := integrationEvent(t, identity, assignment.ExecutionID, 1, "process.started", "4242", nil)
	replayedEvent, err := store.RecordExecutionEvent(ctx, identity, started)
	if err != nil || replayedEvent {
		t.Fatalf("RecordExecutionEvent(started) = %t, %v", replayedEvent, err)
	}
	replayedEvent, err = store.RecordExecutionEvent(ctx, identity, started)
	if err != nil || !replayedEvent {
		t.Fatalf("RecordExecutionEvent(started replay) = %t, %v", replayedEvent, err)
	}
	logDocument := json.RawMessage(`{"apiVersion":"jobman.control/v1alpha1","kind":"LogChunk"}`)
	terminalChunk := domain.LogChunk{
		ExecutionID: assignment.ExecutionID, AgentID: identity.AgentID,
		Stream: "stdout", Sequence: 2, StoreName: "department-nfs", StoreVersion: 1,
		ObjectKey: fmt.Sprintf(
			"namespaces/research/jobs/%s/executions/%s/logs/stdout/00000002.chunk",
			jobID, assignment.ExecutionID,
		),
		ByteOffset: 3, ByteLength: 3, Checksum: "sha256:" + strings.Repeat("a", 64),
		CapturedAt: time.Now().UTC(), Complete: true,
		DocumentDigest: "sha256:" + strings.Repeat("b", 64), Document: logDocument,
	}
	replayedLog, err := store.CommitLogChunk(ctx, identity, terminalChunk)
	if err != nil || replayedLog {
		t.Fatalf("CommitLogChunk(out of order) = %t, %v", replayedLog, err)
	}
	streams, err := store.GetJobLogs(ctx, principal, "research", jobID)
	if err != nil || len(streams) != 0 {
		t.Fatalf("GetJobLogs(gapped) = %#v, %v", streams, err)
	}
	firstChunk := terminalChunk
	firstChunk.Sequence = 1
	firstChunk.ObjectKey = fmt.Sprintf(
		"namespaces/research/jobs/%s/executions/%s/logs/stdout/00000001.chunk",
		jobID, assignment.ExecutionID,
	)
	firstChunk.ByteOffset = 0
	firstChunk.Complete = false
	firstChunk.DocumentDigest = "sha256:" + strings.Repeat("c", 64)
	replayedLog, err = store.CommitLogChunk(ctx, identity, firstChunk)
	if err != nil || replayedLog {
		t.Fatalf("CommitLogChunk(fill gap) = %t, %v", replayedLog, err)
	}
	replayedLog, err = store.CommitLogChunk(ctx, identity, firstChunk)
	if err != nil || !replayedLog {
		t.Fatalf("CommitLogChunk(replay) = %t, %v", replayedLog, err)
	}
	streams, err = store.GetJobLogs(ctx, principal, "research", jobID)
	if err != nil || len(streams) != 1 || len(streams[0].Chunks) != 2 ||
		streams[0].ByteLength != 6 || streams[0].State != "complete" {
		t.Fatalf("GetJobLogs() = %#v, %v", streams, err)
	}
	exitCode := 0
	completed := integrationEvent(
		t, identity, assignment.ExecutionID, 2, "process.completed", "",
		&jobmanprotocol.ProcessResult{Outcome: "success", ExitCode: &exitCode},
	)
	if replayedEvent, err = store.RecordExecutionEvent(ctx, identity, completed); !errors.Is(err, domain.ErrConflict) || replayedEvent {
		t.Fatalf("RecordExecutionEvent(completed without stderr) = %t, %v", replayedEvent, err)
	}
	commitEmptyTerminalLog(ctx, t, store, identity, assignment.ExecutionID, jobID, "stderr", "d")
	if replayedEvent, err = store.RecordExecutionEvent(ctx, identity, completed); err != nil || replayedEvent {
		t.Fatalf("RecordExecutionEvent(completed) = %t, %v", replayedEvent, err)
	}
	job, err := store.GetJob(ctx, principal, "research", jobID)
	if err != nil || job.Phase != "terminal" || job.Outcome != "success" {
		t.Fatalf("GetJob(completed) = %#v, %v", job, err)
	}
}

func testAcceptedCancellation(
	ctx context.Context,
	t *testing.T,
	store *Store,
	principal domain.Principal,
	identity domain.AgentIdentity,
	assignment domain.Assignment,
	jobID string,
) {
	t.Helper()
	if _, err := store.AcceptAssignment(ctx, identity, integrationAcceptance(t, assignment, identity)); err != nil {
		t.Fatalf("AcceptAssignment(canceled job) error = %v", err)
	}
	job, err := store.CancelJob(
		ctx, principal, "research", jobID, "integration-cancel-001",
		"sha256:"+strings.Repeat("c", 64),
	)
	if err != nil || job.DesiredState != "cancel" || job.Phase == "terminal" {
		t.Fatalf("CancelJob() = %#v, %v", job, err)
	}
	actions, err := store.ListDesiredActions(ctx, identity, 10)
	if err != nil || len(actions) != 1 || actions[0].ExecutionID != assignment.ExecutionID {
		t.Fatalf("ListDesiredActions() = %#v, %v", actions, err)
	}
	action := actions[0]
	acknowledgement := domain.ActionAcknowledgement{
		ActionID: action.ActionID, ExecutionID: action.ExecutionID, AgentID: identity.AgentID,
		Revision: action.Revision, ObservedAt: time.Now().UTC(),
	}
	replayed, err := store.AcknowledgeDesiredAction(ctx, identity, acknowledgement)
	if err != nil || replayed {
		t.Fatalf("AcknowledgeDesiredAction() = %t, %v", replayed, err)
	}
	replayed, err = store.AcknowledgeDesiredAction(ctx, identity, acknowledgement)
	if err != nil || !replayed {
		t.Fatalf("AcknowledgeDesiredAction(replay) = %t, %v", replayed, err)
	}
	completed := integrationEvent(
		t, identity, assignment.ExecutionID, 1, "process.completed", "",
		&jobmanprotocol.ProcessResult{Outcome: cancelledOutcome, FailureCode: cancelledBeforeStartFailure},
	)
	commitEmptyTerminalLog(ctx, t, store, identity, assignment.ExecutionID, jobID, "stdout", "e")
	commitEmptyTerminalLog(ctx, t, store, identity, assignment.ExecutionID, jobID, "stderr", "f")
	if _, err = store.RecordExecutionEvent(ctx, identity, completed); err != nil {
		t.Fatalf("RecordExecutionEvent(canceled) error = %v", err)
	}
	job, err = store.GetJob(ctx, principal, "research", jobID)
	if err != nil || job.Phase != "terminal" || job.Outcome != cancelledOutcome {
		t.Fatalf("GetJob(canceled) = %#v, %v", job, err)
	}
}

func commitEmptyTerminalLog(
	ctx context.Context,
	t *testing.T,
	store *Store,
	identity domain.AgentIdentity,
	executionID, jobID, stream, digestCharacter string,
) {
	t.Helper()
	chunk := domain.LogChunk{
		ExecutionID: executionID, AgentID: identity.AgentID,
		Stream: stream, Sequence: 1, StoreName: "department-nfs", StoreVersion: 1,
		ObjectKey: fmt.Sprintf(
			"namespaces/research/jobs/%s/executions/%s/logs/%s/00000001.chunk",
			jobID, executionID, stream,
		),
		ByteOffset: 0, ByteLength: 0,
		Checksum:       "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		CapturedAt:     time.Now().UTC(),
		Complete:       true,
		DocumentDigest: "sha256:" + strings.Repeat(digestCharacter, 64),
		Document:       json.RawMessage(`{"apiVersion":"jobman.control/v1alpha1","kind":"LogChunk"}`),
	}
	if replayed, err := store.CommitLogChunk(ctx, identity, chunk); err != nil || replayed {
		t.Fatalf("CommitLogChunk(empty %s) = %t, %v", stream, replayed, err)
	}
}

func integrationAcceptance(
	t *testing.T,
	assignment domain.Assignment,
	identity domain.AgentIdentity,
) domain.Acceptance {
	t.Helper()
	sealedAssignment, err := jobmanprotocol.DecodeAgentAssignment(
		bytes.NewReader(assignment.Document), jobmanprotocol.DecodeLimits{},
	)
	if err != nil {
		t.Fatalf("DecodeAgentAssignment() error = %v", err)
	}
	request, err := jobmanprotocol.SealAgentAcceptance(jobmanprotocol.AgentAcceptance{
		APIVersion: jobmanprotocol.V1Alpha1, Kind: jobmanprotocol.AgentAcceptanceKind,
		Metadata: jobmanprotocol.AgentAcceptanceMetadata{
			DeliveryID: assignment.DeliveryID, ExecutionID: assignment.ExecutionID,
			AgentID: identity.AgentID,
		},
		Spec: jobmanprotocol.AgentAcceptanceSpec{
			TargetGenerationID:       identity.TargetGenerationID,
			EffectiveExecutionDigest: sealedAssignment.EffectiveExecutionDigest,
		},
	})
	if err != nil {
		t.Fatalf("SealAgentAcceptance() error = %v", err)
	}

	return domain.Acceptance{
		DeliveryID: assignment.DeliveryID, ExecutionID: assignment.ExecutionID,
		AgentID: identity.AgentID, TargetGenerationID: identity.TargetGenerationID,
		EffectiveExecutionDigest: sealedAssignment.EffectiveExecutionDigest,
		RequestDigest:            request.Digest, RequestDocument: request.CanonicalJSON,
	}
}

func integrationEvent(
	t *testing.T,
	identity domain.AgentIdentity,
	executionID string,
	sequence int64,
	eventType, nativeID string,
	result *jobmanprotocol.ProcessResult,
	artifacts ...jobmanprotocol.PublishedArtifact,
) domain.ExecutionObservation {
	t.Helper()
	eventID, err := domain.NewID()
	if err != nil {
		t.Fatalf("NewID() error = %v", err)
	}
	sealed, err := jobmanprotocol.SealExecutionEvent(jobmanprotocol.ExecutionEvent{
		APIVersion: jobmanprotocol.V1Alpha1, Kind: jobmanprotocol.ExecutionEventKind,
		Metadata: jobmanprotocol.ExecutionEventMetadata{
			EventID: eventID, ExecutionID: executionID, AgentID: identity.AgentID,
			Sequence: sequence, ObservedAt: time.Now().UTC(),
		},
		Spec: jobmanprotocol.ExecutionEventSpec{
			Type: eventType, NativeID: nativeID, Result: result, Artifacts: artifacts,
		},
	})
	if err != nil {
		t.Fatalf("SealExecutionEvent() error = %v", err)
	}
	outcome := ""
	if result != nil {
		outcome = result.Outcome
	}

	return domain.ExecutionObservation{
		EventID: eventID, ExecutionID: executionID, AgentID: identity.AgentID,
		Sequence: sequence, ObservedAt: sealed.Document.Metadata.ObservedAt,
		Type: eventType, NativeID: nativeID, Outcome: outcome,
		DocumentDigest: sealed.Digest, Document: sealed.CanonicalJSON,
	}
}

func integrationArtifactSubmission(t *testing.T) domain.JobSubmission {
	t.Helper()
	sealed, err := jobmanprotocol.SealJobRequest(jobmanprotocol.JobRequest{
		APIVersion: jobmanprotocol.V1Alpha1, Kind: jobmanprotocol.JobRequestKind,
		Metadata: jobmanprotocol.JobRequestMetadata{Namespace: "research", Name: "artifact-job"},
		Spec: jobmanprotocol.JobRequestSpec{
			Workload: jobmanprotocol.WorkloadBinding{Document: jobmanprotocol.Workload{
				APIVersion: jobmanprotocol.V1Alpha1, Kind: jobmanprotocol.WorkloadKind,
				Spec: jobmanprotocol.WorkloadSpec{
					Command: jobmanprotocol.Command{Executable: "produce"},
					Runtime: jobmanprotocol.Runtime{Kind: "native"},
					Artifacts: &jobmanprotocol.Artifacts{
						Inputs: []jobmanprotocol.InputArtifact{{
							Name: "sample", Source: "artifact://department-nfs/research/inputs/sample.txt",
							Target: "inputs:/sample.txt", Checksum: "sha256:" + strings.Repeat("b", 64),
						}},
						Outputs: []jobmanprotocol.OutputArtifact{{
							Name: "result", Source: "outputs:/result.txt",
							Destination: "artifact://department-nfs/research/results/result.txt", Required: true,
						}},
					},
					Policy: jobmanprotocol.ExecutionPolicy{
						Retry: jobmanprotocol.RetryPolicy{MaxRuns: 1}, DuplicateRisk: "reject",
					},
				},
			}},
			Placement: jobmanprotocol.Placement{Target: "workstation-a"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := contracts.DecodeJobRequest(bytes.NewReader(sealed.CanonicalJSON))
	if err != nil {
		t.Fatal(err)
	}

	return domain.JobSubmission{
		Namespace: decoded.Namespace, Name: decoded.Name, Labels: decoded.Labels,
		Target: decoded.Target, Partition: decoded.Partition, RuntimeKind: decoded.RuntimeKind,
		OperatingSystems: decoded.OperatingSystems, Architectures: decoded.Architectures,
		Capabilities: decoded.Capabilities, ArtifactStores: decoded.ArtifactStores,
		WorkloadDigest: decoded.WorkloadDigest, WorkloadDocument: decoded.WorkloadDocument,
		RequestDigest: decoded.RequestDigest, RequestDocument: decoded.RequestDocument,
		ExecutionFeatures: domain.ExecutionFeatures{
			DirectCommand: decoded.ExecutionFeatures.DirectCommand,
			Artifacts:     decoded.ExecutionFeatures.Artifacts, RetryMaxRuns: decoded.ExecutionFeatures.RetryMaxRuns,
		},
	}
}

//nolint:unparam // Keeping cluster explicit makes scheduler fixtures self-describing.
func integrationSchedulerEvent(
	t *testing.T,
	identity domain.AgentIdentity,
	executionID string,
	sequence int64,
	eventType, nativeID, state, reason, cluster string,
	result *jobmanprotocol.ProcessResult,
) domain.ExecutionObservation {
	t.Helper()
	eventID, err := domain.NewID()
	if err != nil {
		t.Fatalf("NewID() error = %v", err)
	}
	sealed, err := jobmanprotocol.SealExecutionEvent(jobmanprotocol.ExecutionEvent{
		APIVersion: jobmanprotocol.V1Alpha1, Kind: jobmanprotocol.ExecutionEventKind,
		Metadata: jobmanprotocol.ExecutionEventMetadata{
			EventID: eventID, ExecutionID: executionID, AgentID: identity.AgentID,
			Sequence: sequence, ObservedAt: time.Now().UTC(),
		},
		Spec: jobmanprotocol.ExecutionEventSpec{
			Type: eventType, NativeID: nativeID,
			Scheduler: &jobmanprotocol.SchedulerObservation{
				Backend: "slurm", State: state, Reason: reason, Cluster: cluster,
			},
			Result: result,
		},
	})
	if err != nil {
		t.Fatalf("SealExecutionEvent() error = %v", err)
	}
	outcome := ""
	if result != nil {
		outcome = result.Outcome
	}

	return domain.ExecutionObservation{
		EventID: eventID, ExecutionID: executionID, AgentID: identity.AgentID,
		Sequence: sequence, ObservedAt: sealed.Document.Metadata.ObservedAt,
		Type: eventType, NativeID: nativeID, Outcome: outcome,
		SchedulerBackend: "slurm", SchedulerState: state,
		SchedulerReason: reason, SchedulerCluster: cluster,
		DocumentDigest: sealed.Digest, Document: sealed.CanonicalJSON,
	}
}

func testIntPointer(value int) *int {
	return &value
}

func testAtomicAcceptanceRollback(
	ctx context.Context,
	t *testing.T,
	pool *pgxpool.Pool,
	store *Store,
	principal domain.Principal,
	submission domain.JobSubmission,
) {
	t.Helper()
	identifiers := []string{"22222222-2222-4222-8222-222222222222", "not-a-uuid"}
	index := 0
	store.newID = func() (string, error) {
		identifier := identifiers[index]
		index++

		return identifier, nil
	}
	if _, err := store.SubmitJob(ctx, principal, "integration-rollback", submission); err == nil {
		t.Fatal("SubmitJob() unexpectedly committed with an invalid outbox ID")
	}
	store.newID = domain.NewID

	assertTableCount(ctx, t, pool, "jobs", 2)
	assertTableCount(ctx, t, pool, "idempotency_records", 4)
	assertTableCount(ctx, t, pool, "outbox", 2)
	assertTableCount(ctx, t, pool, "audit_events", 6)
}

func integrationSubmission(t *testing.T) domain.JobSubmission {
	t.Helper()
	fixture, err := os.Open(filepath.Join(
		"..", "..", "..", "contracts", "jobman", "v1alpha1", "conformance", "valid", "job-request-minimal.json",
	))
	if err != nil {
		t.Fatalf("open job request fixture: %v", err)
	}
	defer func() {
		if closeErr := fixture.Close(); closeErr != nil {
			t.Errorf("close job request fixture: %v", closeErr)
		}
	}()
	decoded, err := contracts.DecodeJobRequest(fixture)
	if err != nil {
		t.Fatalf("DecodeJobRequest() error = %v", err)
	}

	return domain.JobSubmission{
		Namespace:        decoded.Namespace,
		Name:             decoded.Name,
		Labels:           decoded.Labels,
		Target:           decoded.Target,
		Partition:        decoded.Partition,
		RuntimeKind:      decoded.RuntimeKind,
		OperatingSystems: decoded.OperatingSystems,
		Architectures:    decoded.Architectures,
		Capabilities:     decoded.Capabilities,
		ArtifactStores:   decoded.ArtifactStores,
		WorkloadDigest:   decoded.WorkloadDigest,
		WorkloadDocument: decoded.WorkloadDocument,
		RequestDigest:    decoded.RequestDigest,
		RequestDocument:  decoded.RequestDocument,
		ExecutionFeatures: domain.ExecutionFeatures{
			DirectCommand:                decoded.ExecutionFeatures.DirectCommand,
			Resources:                    decoded.ExecutionFeatures.Resources,
			TemporaryStorage:             decoded.ExecutionFeatures.TemporaryStorage,
			Artifacts:                    decoded.ExecutionFeatures.Artifacts,
			Extensions:                   decoded.ExecutionFeatures.Extensions,
			EnvironmentProfile:           decoded.ExecutionFeatures.EnvironmentProfile,
			Secrets:                      decoded.ExecutionFeatures.Secrets,
			RetryMaxRuns:                 decoded.ExecutionFeatures.RetryMaxRuns,
			SchedulerEnvironmentOverride: decoded.ExecutionFeatures.SchedulerEnvironmentOverride,
		},
	}
}

func newIntegrationPool(
	ctx context.Context,
	t *testing.T,
	databaseURL string,
) *pgxpool.Pool {
	t.Helper()
	adminConfiguration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal("parse integration database configuration")
	}
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfiguration)
	if err != nil {
		t.Fatalf("open integration administration pool: %v", err)
	}
	t.Cleanup(adminPool.Close)
	identifier, err := domain.NewID()
	if err != nil {
		t.Fatalf("create integration schema ID: %v", err)
	}
	schema := "jobman_control_test_" + strings.ReplaceAll(identifier, "-", "")
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err = adminPool.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatalf("create integration schema: %v", err)
	}
	cleanupBaseContext := context.WithoutCancel(ctx)
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(cleanupBaseContext, 10*time.Second)
		defer cancel()
		if _, cleanupErr := adminPool.Exec(cleanupContext, "DROP SCHEMA "+quotedSchema+" CASCADE"); cleanupErr != nil {
			t.Errorf("drop integration schema: %v", cleanupErr)
		}
	})

	testConfiguration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal("parse integration test database configuration")
	}
	testConfiguration.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, testConfiguration)
	if err != nil {
		t.Fatalf("open integration test pool: %v", err)
	}
	t.Cleanup(pool.Close)

	return pool
}

func assertTableCount(
	ctx context.Context,
	t *testing.T,
	pool *pgxpool.Pool,
	table string,
	want int,
) {
	t.Helper()
	var query string
	switch table {
	case "assignments":
		query = "SELECT count(*) FROM assignments"
	case "audit_events":
		query = "SELECT count(*) FROM audit_events"
	case "idempotency_records":
		query = "SELECT count(*) FROM idempotency_records"
	case "jobs":
		query = "SELECT count(*) FROM jobs"
	case "log_chunks":
		query = "SELECT count(*) FROM log_chunks"
	case "log_streams":
		query = "SELECT count(*) FROM log_streams"
	case "outbox":
		query = "SELECT count(*) FROM outbox"
	default:
		t.Fatalf("test attempted to query unexpected table %q", table)
	}
	var count int
	if err := pool.QueryRow(ctx, query).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if count != want {
		t.Fatalf("%s count = %d, want %d", table, count, want)
	}
}
