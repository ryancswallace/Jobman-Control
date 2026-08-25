package protocol_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	protocol "github.com/ryancswallace/jobman-control/contracts/jobman/v1alpha1"
)

const (
	workloadSchemaID           = "https://jobman.tech/schema/protocol/workload-v1alpha1.schema.json"
	jobRequestSchemaID         = "https://jobman.tech/schema/protocol/job-request-v1alpha1.schema.json"
	effectiveExecutionSchemaID = "https://jobman.tech/schema/protocol/effective-execution-v1alpha1.schema.json"
	agentAssignmentSchemaID    = "https://jobman.tech/schema/protocol/agent-assignment-v1alpha1.schema.json"
)

type manifest struct {
	Contract string         `json:"contract"`
	Cases    []manifestCase `json:"cases"`
}

type manifestCase struct {
	File           string `json:"file"`
	Kind           string `json:"kind"`
	Valid          bool   `json:"valid"`
	Digest         string `json:"digest"`
	WorkloadDigest string `json:"workloadDigest"`
}

func TestContractConformanceSnapshot(t *testing.T) {
	t.Parallel()
	root := "conformance"
	contents, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var fixtures manifest
	if err = json.Unmarshal(contents, &fixtures); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if fixtures.Contract != protocol.V1Alpha1 {
		t.Fatalf("contract = %q", fixtures.Contract)
	}
	schemas := compileSchemas(t)

	for _, fixture := range fixtures.Cases {
		t.Run(fixture.File, func(t *testing.T) {
			t.Parallel()
			document, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(fixture.File)))
			if readErr != nil {
				t.Fatalf("read fixture: %v", readErr)
			}
			if fixture.Valid {
				validateSchema(t, schemas[fixture.Kind], document)
			}
			validateDecoder(t, fixture, document)
		})
	}
}

func compileSchemas(t *testing.T) map[string]*jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	for identifier, contents := range map[string][]byte{
		workloadSchemaID:           protocol.WorkloadSchema(),
		jobRequestSchemaID:         protocol.JobRequestSchema(),
		effectiveExecutionSchemaID: protocol.EffectiveExecutionSchema(),
		agentAssignmentSchemaID:    protocol.AgentAssignmentSchema(),
	} {
		var document any
		decoder := json.NewDecoder(bytes.NewReader(contents))
		decoder.UseNumber()
		if err := decoder.Decode(&document); err != nil {
			t.Fatalf("decode schema %q: %v", identifier, err)
		}
		if err := compiler.AddResource(identifier, document); err != nil {
			t.Fatalf("add schema %q: %v", identifier, err)
		}
	}
	workloadSchema, err := compiler.Compile(workloadSchemaID)
	if err != nil {
		t.Fatalf("compile workload schema: %v", err)
	}
	jobRequestSchema, err := compiler.Compile(jobRequestSchemaID)
	if err != nil {
		t.Fatalf("compile job request schema: %v", err)
	}
	effectiveExecutionSchema, err := compiler.Compile(effectiveExecutionSchemaID)
	if err != nil {
		t.Fatalf("compile effective execution schema: %v", err)
	}
	agentAssignmentSchema, err := compiler.Compile(agentAssignmentSchemaID)
	if err != nil {
		t.Fatalf("compile agent assignment schema: %v", err)
	}

	return map[string]*jsonschema.Schema{
		protocol.WorkloadKind:           workloadSchema,
		protocol.JobRequestKind:         jobRequestSchema,
		protocol.EffectiveExecutionKind: effectiveExecutionSchema,
		protocol.AgentAssignmentKind:    agentAssignmentSchema,
	}
}

func validateSchema(t *testing.T, schema *jsonschema.Schema, document []byte) {
	t.Helper()
	if schema == nil {
		t.Fatal("manifest references an unknown schema kind")
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode schema input: %v", err)
	}
	if err := schema.Validate(value); err != nil {
		t.Fatalf("schema validation failed: %v", err)
	}
}

func validateDecoder(t *testing.T, fixture manifestCase, document []byte) {
	t.Helper()
	switch fixture.Kind {
	case protocol.WorkloadKind:
		sealed, err := protocol.DecodeWorkload(bytes.NewReader(document), protocol.DecodeLimits{})
		assertValidity(t, fixture.Valid, err)
		if err == nil && sealed.Digest != fixture.Digest {
			t.Fatalf("workload digest = %q, want %q", sealed.Digest, fixture.Digest)
		}
	case protocol.JobRequestKind:
		sealed, err := protocol.DecodeJobRequest(bytes.NewReader(document), protocol.DecodeLimits{})
		assertValidity(t, fixture.Valid, err)
		if err == nil && (sealed.RequestDigest != fixture.Digest || sealed.WorkloadDigest != fixture.WorkloadDigest) {
			t.Fatalf("request digests = %q/%q, want %q/%q",
				sealed.RequestDigest, sealed.WorkloadDigest, fixture.Digest, fixture.WorkloadDigest)
		}
	case protocol.EffectiveExecutionKind:
		sealed, err := protocol.DecodeEffectiveExecution(bytes.NewReader(document), protocol.DecodeLimits{})
		assertValidity(t, fixture.Valid, err)
		if err == nil && (sealed.Digest != fixture.Digest || sealed.Document.Spec.Workload.Digest != fixture.WorkloadDigest) {
			t.Fatalf("effective execution digests = %q/%q, want %q/%q",
				sealed.Digest, sealed.Document.Spec.Workload.Digest, fixture.Digest, fixture.WorkloadDigest)
		}
	case protocol.AgentAssignmentKind:
		sealed, err := protocol.DecodeAgentAssignment(bytes.NewReader(document), protocol.DecodeLimits{})
		assertValidity(t, fixture.Valid, err)
		if err == nil && (sealed.EffectiveExecutionDigest != fixture.Digest ||
			sealed.Document.Spec.EffectiveExecution.Spec.Workload.Digest != fixture.WorkloadDigest) {
			t.Fatalf("assignment digests = %q/%q, want %q/%q",
				sealed.EffectiveExecutionDigest,
				sealed.Document.Spec.EffectiveExecution.Spec.Workload.Digest,
				fixture.Digest, fixture.WorkloadDigest)
		}
	default:
		t.Fatalf("unknown manifest kind %q", fixture.Kind)
	}
}

func assertValidity(t *testing.T, valid bool, err error) {
	t.Helper()
	if valid && err != nil {
		t.Fatalf("decoder rejected valid fixture: %v", err)
	}
	if !valid && err == nil {
		t.Fatal("decoder accepted invalid fixture")
	}
	if !valid && err != nil && strings.TrimSpace(err.Error()) == "" {
		t.Fatal("decoder returned an empty error")
	}
}
