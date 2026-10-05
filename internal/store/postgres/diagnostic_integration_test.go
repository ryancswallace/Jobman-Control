package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ryancswallace/jobman/diagnostic"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func TestDiagnosticSnapshotIntegration(t *testing.T) {
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
	principal := domain.Principal{Issuer: "test-issuer", Subject: "snapshot-user"}
	for _, namespace := range []string{"research", "other"} {
		if err := store.EnsureDevelopmentIdentity(ctx, domain.DevelopmentIdentity{Principal: principal, DisplayName: "Snapshot user", Namespace: namespace}); err != nil {
			t.Fatal(err)
		}
	}
	source, err := store.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(source.Features, "shared-diagnostic-snapshots") {
		t.Fatal("disabled feature advertised")
	}
	if _, err = store.CreateTarget(ctx, principal, "research", "snapshot-target", "sha256:"+strings.Repeat("1", 64), domain.TargetSpec{Name: "workstation-a", Kind: "host", ExecutionBackend: "subprocess", Runtimes: []string{"native"}}); err != nil {
		t.Fatal(err)
	}
	submitted, err := store.SubmitJob(ctx, principal, "snapshot-job", integrationSubmission(t))
	if err != nil {
		t.Fatal(err)
	}
	selection := diagnostic.SharedSelection{DeploymentID: "79000000-0000-4000-8000-000000000001", ControlInstanceID: source.InstanceID, NamespaceID: submitted.Job.NamespaceID, JobID: submitted.Job.ID}
	if _, err = store.ReadDiagnosticSnapshot(ctx, principal, "research", selection); !errors.Is(err, domain.ErrFeatureUnavailable) {
		t.Fatalf("disabled snapshot=%v", err)
	}
	if err = store.EnableDiagnosticSnapshots(selection.DeploymentID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO runs(id,namespace_id,job_id,run_number,phase,desired_state) SELECT gen_random_uuid(),namespace_id,id,number,'ready','run' FROM jobs CROSS JOIN generate_series(10,49) number WHERE id=$1`, submitted.Job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE jobs SET revision=9007199254740993,labels='{"private-canary":"must-not-appear"}'::jsonb,updated_at=created_at+interval '5 days' WHERE id=$1`, submitted.Job.ID); err != nil {
		t.Fatal(err)
	}
	result, err := store.ReadDiagnosticSnapshot(ctx, principal, "research", selection)
	if err != nil || len(result.Snapshot.Runs) != 32 || result.Snapshot.Runs[0].Number != 18 || result.Snapshot.Runs[31].Number != 49 || result.Snapshot.Job.Revision != 9007199254740993 || !result.Snapshot.CapturedAt.Equal(result.AsOf) {
		t.Fatalf("bounded diagnostic=%#v,%v", result, err)
	}
	if !hasDiagnosticOmission(result.Snapshot, diagnostic.OmissionHistoryTruncated) || len(result.Snapshot.Logs) != 0 {
		t.Fatal("missing truncation or fabricated logs")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-canary") || strings.Contains(string(encoded), "must-not-appear") || !strings.Contains(string(encoded), `"revision":"9007199254740993"`) {
		t.Fatal("diagnostic disclosure or precision changed")
	}
	for _, item := range result.Snapshot.Items {
		if item.Code == diagnostic.CodeRunStartedAt || item.Code == diagnostic.CodeRunCompletedAt || item.Code == diagnostic.CodeRunRevision {
			t.Fatal("invented run timing or revision")
		}
	}
	var oldRun string
	if err = pool.QueryRow(ctx, `SELECT id::text FROM runs WHERE job_id=$1 AND run_number=10`, submitted.Job.ID).Scan(&oldRun); err != nil {
		t.Fatal(err)
	}
	pinned := selection
	pinned.RunID = oldRun
	pinned.ExpectedJobRevision = 9007199254740993
	selected, err := store.ReadDiagnosticSnapshot(ctx, principal, "research", pinned)
	if err != nil || len(selected.Snapshot.Runs) != 1 || selected.Snapshot.Runs[0].Number != 10 || hasDiagnosticOmission(selected.Snapshot, diagnostic.OmissionHistoryTruncated) {
		t.Fatalf("explicit actual run=%#v,%v", selected, err)
	}
	for _, mutate := range []func(*diagnostic.SharedSelection){func(s *diagnostic.SharedSelection) { s.DeploymentID = "79000000-0000-4000-8000-000000000002" }, func(s *diagnostic.SharedSelection) { s.ControlInstanceID = "79000000-0000-4000-8000-000000000002" }, func(s *diagnostic.SharedSelection) { s.NamespaceID = "79000000-0000-4000-8000-000000000002" }, func(s *diagnostic.SharedSelection) { s.ExpectedJobRevision = 2 }} {
		changed := selection
		mutate(&changed)
		if _, err = store.ReadDiagnosticSnapshot(ctx, principal, "research", changed); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("source or revision relabeling accepted: %v", err)
		}
	}
	missing := selection
	missing.RunID = "79000000-0000-4000-8000-000000000002"
	if _, err = store.ReadDiagnosticSnapshot(ctx, principal, "research", missing); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("invented run accepted: %v", err)
	}

	nodes := make([]domain.JobSubmission, 131)
	edges := make([]domain.GraphEdgeSubmission, 0, 130)
	for index := range nodes {
		nodes[index] = integrationSubmission(t)
		nodes[index].Name = fmt.Sprintf("evidence-node-%d", index)
		nodes[index].RequestDigest = fmt.Sprintf("sha256:%064x", index+100)
		if index < 130 {
			edges = append(edges, domain.GraphEdgeSubmission{From: nodes[index].Name, To: "evidence-node-130", Predicate: "success"})
		}
	}
	graph, err := store.SubmitGraph(ctx, principal, "diagnostic-graph", domain.GraphSubmission{Namespace: "research", Name: "diagnostic-graph", MaxActive: 1, UnsatisfiedPolicy: "blocked", RequestDigest: fmt.Sprintf("sha256:%064x", 500), RequestDocument: json.RawMessage(`{}`), Nodes: nodes, Edges: edges})
	if err != nil {
		t.Fatal(err)
	}
	dependencySelection := selection
	dependencySelection.JobID = graph.Graph.Items[130].Job.ID
	dependencySnapshot, err := store.ReadDiagnosticSnapshot(ctx, principal, "research", dependencySelection)
	if err != nil || !hasDiagnosticOmission(dependencySnapshot.Snapshot, "dependencies_truncated") {
		t.Fatalf("dependency bound=%#v,%v", dependencySnapshot, err)
	}
	count := 0
	for _, item := range dependencySnapshot.Snapshot.Items {
		if item.Code == diagnostic.CodeSharedDependencyObservation {
			count++
			var observation diagnostic.SharedDependencyObservation
			if err = json.Unmarshal(item.Value, &observation); err != nil || observation.Satisfied || observation.ObservedOutcome != "" {
				t.Fatal("invented dependency satisfaction")
			}
		}
	}
	if count != 128 {
		t.Fatalf("dependency bound=%d", count)
	}
	if _, err = pool.Exec(ctx, `UPDATE membership_grants SET revoked_at=statement_timestamp() WHERE namespace_id=$1`, selection.NamespaceID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ReadDiagnosticSnapshot(ctx, principal, "research", selection); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("snapshot retained revoked access: %v", err)
	}
}

func hasDiagnosticOmission(snapshot diagnostic.SharedSnapshot, code string) bool {
	for _, omission := range snapshot.Omissions {
		if omission.Code == code {
			return true
		}
	}
	return false
}
