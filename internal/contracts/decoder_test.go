package contracts

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	jobmanprotocol "github.com/ryancswallace/jobman-control/contracts/jobman/v1alpha1"
)

func FuzzDecodeJobRequest(f *testing.F) {
	for _, name := range []string{
		"valid/job-request-minimal.json",
		"invalid/job-request-bad-digest.json",
	} {
		fixture := filepath.Join(
			"..", "..", "contracts", "jobman", "v1alpha1", "conformance", name,
		)
		contents, err := os.ReadFile(fixture)
		if err != nil {
			f.Fatalf("read seed fixture: %v", err)
		}
		f.Add(contents)
	}
	f.Fuzz(func(_ *testing.T, data []byte) {
		if _, err := DecodeJobRequest(bytes.NewReader(data)); err != nil {
			return
		}
	})
}

func TestDecodeJobRequestUsesLockedContract(t *testing.T) {
	t.Parallel()
	fixture := filepath.Join(
		"..", "..", "contracts", "jobman", "v1alpha1", "conformance", "valid", "job-request-minimal.json",
	)
	contents, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	request, err := DecodeJobRequest(strings.NewReader(string(contents)))
	if err != nil {
		t.Fatalf("DecodeJobRequest() error = %v", err)
	}
	if request.Namespace != "research" || request.Name != "minimal-job" {
		t.Fatalf("request identity = %q/%q", request.Namespace, request.Name)
	}
	if request.Target != "workstation-a" || request.Partition != "" {
		t.Fatalf("placement = %q/%q", request.Target, request.Partition)
	}
	if request.RequestDigest == "" || request.WorkloadDigest == "" {
		t.Fatalf("missing digests: %#v", request)
	}
	if !request.ExecutionFeatures.DirectCommand ||
		request.ExecutionFeatures.RetryMaxRuns != 1 {
		t.Fatalf("execution features = %#v", request.ExecutionFeatures)
	}
}

func TestDecodeJobRequestRejectsInvalidFixture(t *testing.T) {
	t.Parallel()
	fixture := filepath.Join(
		"..", "..", "contracts", "jobman", "v1alpha1", "conformance", "invalid", "job-request-bad-digest.json",
	)
	contents, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if _, err := DecodeJobRequest(strings.NewReader(string(contents))); err == nil {
		t.Fatal("DecodeJobRequest() unexpectedly accepted an invalid fixture")
	}
}

func TestExecutionFeaturesDetectSchedulerEnvironment(t *testing.T) {
	t.Parallel()
	features := executionFeatures(jobmanprotocol.Workload{Spec: jobmanprotocol.WorkloadSpec{
		Command: jobmanprotocol.Command{Executable: "true"},
		Environment: &jobmanprotocol.Environment{
			Values: map[string]string{"SLURM_JOB_ID": "forged"},
		},
		Policy: jobmanprotocol.ExecutionPolicy{Retry: jobmanprotocol.RetryPolicy{MaxRuns: 1}},
	}})
	if !features.DirectCommand || !features.SchedulerEnvironmentOverride {
		t.Fatalf("executionFeatures() = %#v", features)
	}
}

func TestDecodeJobRequestProjectsArtifactStores(t *testing.T) {
	workload, err := jobmanprotocol.SealWorkload(jobmanprotocol.Workload{
		APIVersion: jobmanprotocol.V1Alpha1, Kind: jobmanprotocol.WorkloadKind,
		Spec: jobmanprotocol.WorkloadSpec{
			Command: jobmanprotocol.Command{Executable: "true"},
			Runtime: jobmanprotocol.Runtime{Kind: "native"},
			Artifacts: &jobmanprotocol.Artifacts{
				Inputs: []jobmanprotocol.InputArtifact{{
					Name: "input", Source: "artifact://department-nfs/research/input",
					Target: "inputs:/input",
				}},
				Outputs: []jobmanprotocol.OutputArtifact{{
					Name: "output", Source: "outputs:/output",
					Destination: "artifact://department-nfs/research/output", Required: true,
				}},
			},
			Policy: jobmanprotocol.ExecutionPolicy{
				Retry: jobmanprotocol.RetryPolicy{MaxRuns: 1}, DuplicateRisk: "reject",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := jobmanprotocol.SealJobRequest(jobmanprotocol.JobRequest{
		APIVersion: jobmanprotocol.V1Alpha1, Kind: jobmanprotocol.JobRequestKind,
		Metadata: jobmanprotocol.JobRequestMetadata{Namespace: "research", Name: "artifacts"},
		Spec: jobmanprotocol.JobRequestSpec{
			Workload:  jobmanprotocol.WorkloadBinding{Digest: workload.Digest, Document: workload.Document},
			Placement: jobmanprotocol.Placement{Target: "workstation-a"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeJobRequest(bytes.NewReader(request.CanonicalJSON))
	if err != nil || len(decoded.ArtifactStores) != 1 || decoded.ArtifactStores[0] != "department-nfs" {
		t.Fatalf("DecodeJobRequest() stores = %#v, %v", decoded.ArtifactStores, err)
	}
}
