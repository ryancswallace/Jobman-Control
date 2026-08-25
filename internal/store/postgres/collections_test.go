package postgres

import (
	"encoding/json"
	"errors"
	"testing"

	jobmanprotocol "github.com/ryancswallace/jobman-control/contracts/jobman/v1alpha1"
	"github.com/ryancswallace/jobman-control/internal/domain"
)

func TestResolveCollectionArrayMode(t *testing.T) {
	t.Parallel()
	workload := jobmanprotocol.Workload{
		APIVersion: jobmanprotocol.V1Alpha1, Kind: jobmanprotocol.WorkloadKind,
		Metadata: jobmanprotocol.WorkloadMetadata{Name: "array-task"},
		Spec: jobmanprotocol.WorkloadSpec{
			Command: jobmanprotocol.Command{Executable: "true"}, WorkingDirectory: "workspace:/",
			Runtime:   jobmanprotocol.Runtime{Kind: "native"},
			Resources: &jobmanprotocol.Resources{CPU: 2, Memory: "4GiB"},
			Policy:    jobmanprotocol.ExecutionPolicy{Retry: jobmanprotocol.RetryPolicy{MaxRuns: 1}, DuplicateRisk: "reject"},
		},
	}
	document, err := json.Marshal(workload)
	if err != nil {
		t.Fatal(err)
	}
	submission := domain.CollectionSubmission{
		ArrayPolicy: "require",
		Items:       []domain.JobSubmission{{WorkloadDocument: document}, {WorkloadDocument: document}},
	}
	placements := []resolvedPlacement{
		{executionBackend: "slurm", targetGenerationID: "generation", partition: "gpu"},
		{executionBackend: "slurm", targetGenerationID: "generation", partition: "gpu"},
	}
	mode, err := resolveCollectionArrayMode(submission, placements)
	if err != nil || mode != "slurm-array" {
		t.Fatalf("resolveCollectionArrayMode() = %q, %v", mode, err)
	}

	submission.ArrayPolicy = "prefer"
	placements[1].partition = "cpu"
	mode, err = resolveCollectionArrayMode(submission, placements)
	if err != nil || mode != "individual" {
		t.Fatalf("resolveCollectionArrayMode(heterogeneous prefer) = %q, %v", mode, err)
	}
	submission.ArrayPolicy = "require"
	if _, err = resolveCollectionArrayMode(submission, placements); !errors.Is(err, domain.ErrInvalidPlacement) {
		t.Fatalf("resolveCollectionArrayMode(heterogeneous require) error = %v", err)
	}

	submission.ArrayPolicy = "never"
	if mode, err = resolveCollectionArrayMode(submission, placements); err != nil || mode != "individual" {
		t.Fatalf("resolveCollectionArrayMode(never) = %q, %v", mode, err)
	}
}
