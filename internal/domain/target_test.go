package domain

import (
	"strings"
	"testing"
	"time"
)

func TestValidateTargetSpec(t *testing.T) {
	t.Parallel()
	valid := TargetSpec{
		Name: "cluster-a", Kind: "slurm", ExecutionBackend: "slurm",
		Runtimes:   []string{"container", "native"},
		Partitions: []PartitionSpec{{Name: "cpu"}, {Name: "gpu", IsDefault: true}},
	}
	if err := ValidateTargetSpec(valid); err != nil {
		t.Fatalf("ValidateTargetSpec() error = %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*TargetSpec)
		want   string
	}{
		{name: "kind backend", mutate: func(value *TargetSpec) { value.ExecutionBackend = "subprocess" }, want: "incompatible"},
		{name: "runtime duplicate", mutate: func(value *TargetSpec) { value.Runtimes = []string{"native", "native"} }, want: "duplicated"},
		{name: "host partition", mutate: func(value *TargetSpec) { value.Kind = "host" }, want: "incompatible"},
		{name: "default partitions", mutate: func(value *TargetSpec) { value.Partitions[0].IsDefault = true }, want: "more than one"},
		{name: "log store pair", mutate: func(value *TargetSpec) { value.LogStoreName = "department-nfs" }, want: "configured together"},
		{name: "log store name", mutate: func(value *TargetSpec) { value.LogStoreName = "Bad Store"; value.LogStoreVersion = 1 }, want: "mapping is invalid"},
		{name: "provider", mutate: func(value *TargetSpec) { value.Provider.Kind = "unknown" }, want: "unsupported"},
		{name: "on-prem AWS settings", mutate: func(value *TargetSpec) { value.Provider.Region = "us-east-1" }, want: "cannot contain"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			value := valid
			value.Runtimes = append([]string(nil), valid.Runtimes...)
			value.Partitions = append([]PartitionSpec(nil), valid.Partitions...)
			test.mutate(&value)
			err := ValidateTargetSpec(value)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateTargetSpec() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestValidateParallelClusterTargetAndGeneration(t *testing.T) {
	t.Parallel()
	spec := TargetSpec{
		Name: "aws-cluster", Kind: "slurm", ExecutionBackend: "slurm",
		Runtimes: []string{"native"},
		Provider: TargetProvider{
			Kind: "aws-parallelcluster", Region: "us-east-1", ClusterName: "Research-Cluster",
		},
	}
	if err := ValidateTargetGenerationChange(TargetGenerationChange{
		Spec: spec, ExpectedRevision: 2,
	}); err != nil {
		t.Fatalf("ValidateTargetGenerationChange() error = %v", err)
	}
	for _, mutate := range []func(*TargetSpec){
		func(value *TargetSpec) { value.Kind = "host"; value.ExecutionBackend = "subprocess" },
		func(value *TargetSpec) { value.Provider.Region = "invalid" },
		func(value *TargetSpec) { value.Provider.ClusterName = "-invalid" },
	} {
		candidate := spec
		mutate(&candidate)
		if err := ValidateTargetSpec(candidate); err == nil {
			t.Fatalf("ValidateTargetSpec(%#v) unexpectedly succeeded", candidate)
		}
	}
	if err := ValidateTargetGenerationChange(TargetGenerationChange{Spec: spec}); err == nil {
		t.Fatal("ValidateTargetGenerationChange() accepted zero revision")
	}
}

func TestValidateEnrollmentAndAgentRegistration(t *testing.T) {
	t.Parallel()
	if err := ValidateEnrollmentRequest(EnrollmentRequest{
		Principal:    Principal{Issuer: "issuer", Subject: "subject"},
		ExpectedUser: "researcher", Lifetime: 10 * time.Minute,
	}); err != nil {
		t.Fatalf("ValidateEnrollmentRequest() error = %v", err)
	}
	registration := AgentRegistration{
		TargetGenerationID: "55555555-5555-4555-8555-555555555555",
		AgentVersion:       "0.1.0", ProtocolVersions: []string{"jobman/v1alpha1"},
		OperatingSystem: "linux", Architecture: "amd64", Hostname: "host-a",
		ExecutionUser: "researcher", ExecutionBackends: []string{"subprocess"},
		Runtimes:      []string{"native"},
		RequestDigest: "sha256:" + strings.Repeat("1", 64),
	}
	if err := ValidateAgentRegistration(registration); err != nil {
		t.Fatalf("ValidateAgentRegistration() error = %v", err)
	}
	registration.ProtocolVersions = []string{"jobman/v2"}
	if err := ValidateAgentRegistration(registration); err == nil {
		t.Fatal("ValidateAgentRegistration() accepted an incompatible protocol")
	}
}
