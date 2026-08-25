// Package contracts isolates the service from the currently snapshotted
// Jobman wire-contract implementation.
package contracts

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"

	jobmanprotocol "github.com/ryancswallace/jobman-control/contracts/jobman/v1alpha1"
)

// JobRequest is the validated service-facing projection of one canonical
// Jobman job request.
type JobRequest struct {
	Namespace         string
	Name              string
	Labels            map[string]string
	Target            string
	Partition         string
	RuntimeKind       string
	OperatingSystems  []string
	Architectures     []string
	Capabilities      []string
	ArtifactStores    []string
	WorkloadDigest    string
	WorkloadDocument  json.RawMessage
	RequestDigest     string
	RequestDocument   json.RawMessage
	ExecutionFeatures ExecutionFeatures
}

// CollectionRequest is the validated service-facing projection of one
// collection and its independently sealed children.
type CollectionRequest struct {
	Namespace       string
	Name            string
	Labels          map[string]string
	MaxActive       int
	FailurePolicy   string
	ArrayPolicy     string
	RequestDigest   string
	RequestDocument json.RawMessage
	Items           []JobRequest
}

// GraphRequest is the validated service projection of one immutable DAG.
type GraphRequest struct {
	Namespace         string
	Name              string
	Labels            map[string]string
	MaxActive         int
	UnsatisfiedPolicy string
	RequestDigest     string
	RequestDocument   json.RawMessage
	Nodes             []JobRequest
	Edges             []GraphEdge
}

// GraphEdge is one graph-local upstream readiness predicate.
type GraphEdge struct {
	From      string
	To        string
	Predicate string
	Outcomes  []string
}

// ExecutionFeatures is the service-facing projection used to ensure that a
// selected target can execute the workload before durable intent is accepted.
// It deliberately describes protocol features rather than agent internals.
type ExecutionFeatures struct {
	DirectCommand                bool
	Resources                    bool
	TemporaryStorage             bool
	Artifacts                    bool
	Extensions                   bool
	EnvironmentProfile           bool
	Secrets                      bool
	RetryMaxRuns                 int
	SchedulerEnvironmentOverride bool
}

// DecodeJobRequest bounds and validates a Jobman request using the locked
// contract snapshot, then projects only fields the first control slice owns.
func DecodeJobRequest(source io.Reader) (JobRequest, error) {
	sealed, err := jobmanprotocol.DecodeJobRequest(source, jobmanprotocol.DecodeLimits{})
	if err != nil {
		return JobRequest{}, fmt.Errorf("decode shared job request: %w", err)
	}
	sealedWorkload, err := jobmanprotocol.SealWorkload(sealed.Document.Spec.Workload.Document)
	if err != nil {
		return JobRequest{}, fmt.Errorf("seal canonical workload: %w", err)
	}

	request := JobRequest{
		Namespace:        sealed.Document.Metadata.Namespace,
		Name:             sealed.Document.Metadata.Name,
		Labels:           cloneLabels(sealed.Document.Metadata.Labels),
		Target:           sealed.Document.Spec.Placement.Target,
		Partition:        sealed.Document.Spec.Placement.Partition,
		RuntimeKind:      sealed.Document.Spec.Workload.Document.Spec.Runtime.Kind,
		WorkloadDigest:   sealed.WorkloadDigest,
		WorkloadDocument: append(json.RawMessage(nil), sealedWorkload.CanonicalJSON...),
		RequestDigest:    sealed.RequestDigest,
		RequestDocument:  append(json.RawMessage(nil), sealed.CanonicalJSON...),
		ExecutionFeatures: executionFeatures(
			sealed.Document.Spec.Workload.Document,
		),
	}
	if requirements := sealed.Document.Spec.Workload.Document.Spec.Requirements; requirements != nil {
		request.OperatingSystems = append([]string(nil), requirements.OperatingSystems...)
		request.Architectures = append([]string(nil), requirements.Architectures...)
		request.Capabilities = append([]string(nil), requirements.Capabilities...)
	}
	request.ArtifactStores, err = workloadArtifactStores(sealed.Document.Spec.Workload.Document)
	if err != nil {
		return JobRequest{}, fmt.Errorf("project workload artifact stores: %w", err)
	}

	return request, nil
}

// DecodeCollectionRequest validates one portable collection and projects each
// child through the same admission facts as a standalone job.
func DecodeCollectionRequest(source io.Reader) (CollectionRequest, error) {
	sealed, err := jobmanprotocol.DecodeCollectionRequest(source, jobmanprotocol.DecodeLimits{})
	if err != nil {
		return CollectionRequest{}, fmt.Errorf("decode shared collection request: %w", err)
	}
	result := CollectionRequest{
		Namespace: sealed.Document.Metadata.Namespace, Name: sealed.Document.Metadata.Name,
		Labels:          cloneLabels(sealed.Document.Metadata.Labels),
		MaxActive:       sealed.Document.Spec.MaxActive,
		FailurePolicy:   sealed.Document.Spec.FailurePolicy,
		ArrayPolicy:     sealed.Document.Spec.ArrayPolicy,
		RequestDigest:   sealed.RequestDigest,
		RequestDocument: append(json.RawMessage(nil), sealed.CanonicalJSON...),
		Items:           make([]JobRequest, 0, len(sealed.Document.Spec.Items)),
	}
	for _, item := range sealed.Document.Spec.Items {
		child, sealErr := jobmanprotocol.SealJobRequest(jobmanprotocol.JobRequest{
			APIVersion: jobmanprotocol.V1Alpha1, Kind: jobmanprotocol.JobRequestKind,
			Metadata: jobmanprotocol.JobRequestMetadata{
				Namespace: sealed.Document.Metadata.Namespace, Name: item.Name,
				Labels: cloneLabels(sealed.Document.Metadata.Labels),
			},
			Spec: jobmanprotocol.JobRequestSpec{
				Workload: item.Workload, Placement: item.Placement,
			},
		})
		if sealErr != nil {
			return CollectionRequest{}, fmt.Errorf("seal collection child %q: %w", item.Name, sealErr)
		}
		projected, projectErr := projectSealedJob(child)
		if projectErr != nil {
			return CollectionRequest{}, fmt.Errorf("project collection child %q: %w", item.Name, projectErr)
		}
		result.Items = append(result.Items, projected)
	}

	return result, nil
}

// DecodeGraphRequest validates the immutable graph and projects each node
// through the same placement-admission boundary as an ordinary job.
func DecodeGraphRequest(source io.Reader) (GraphRequest, error) {
	sealed, err := jobmanprotocol.DecodeGraphRequest(source, jobmanprotocol.DecodeLimits{})
	if err != nil {
		return GraphRequest{}, fmt.Errorf("decode shared graph request: %w", err)
	}
	result := GraphRequest{
		Namespace: sealed.Document.Metadata.Namespace, Name: sealed.Document.Metadata.Name,
		Labels: cloneLabels(sealed.Document.Metadata.Labels), MaxActive: sealed.Document.Spec.MaxActive,
		UnsatisfiedPolicy: sealed.Document.Spec.UnsatisfiedPolicy,
		RequestDigest:     sealed.RequestDigest,
		RequestDocument:   append(json.RawMessage(nil), sealed.CanonicalJSON...),
		Nodes:             make([]JobRequest, 0, len(sealed.Document.Spec.Nodes)),
		Edges:             make([]GraphEdge, 0, len(sealed.Document.Spec.Edges)),
	}
	for _, node := range sealed.Document.Spec.Nodes {
		child, sealErr := jobmanprotocol.SealJobRequest(jobmanprotocol.JobRequest{
			APIVersion: jobmanprotocol.V1Alpha1, Kind: jobmanprotocol.JobRequestKind,
			Metadata: jobmanprotocol.JobRequestMetadata{
				Namespace: sealed.Document.Metadata.Namespace, Name: node.Name,
				Labels: cloneLabels(sealed.Document.Metadata.Labels),
			},
			Spec: jobmanprotocol.JobRequestSpec{Workload: node.Workload, Placement: node.Placement},
		})
		if sealErr != nil {
			return GraphRequest{}, fmt.Errorf("seal graph node %q: %w", node.Name, sealErr)
		}
		projected, projectErr := projectSealedJob(child)
		if projectErr != nil {
			return GraphRequest{}, fmt.Errorf("project graph node %q: %w", node.Name, projectErr)
		}
		result.Nodes = append(result.Nodes, projected)
	}
	for _, edge := range sealed.Document.Spec.Edges {
		result.Edges = append(result.Edges, GraphEdge{
			From: edge.From, To: edge.To, Predicate: edge.Predicate,
			Outcomes: append([]string(nil), edge.Outcomes...),
		})
	}

	return result, nil
}

func projectSealedJob(sealed jobmanprotocol.SealedJobRequest) (JobRequest, error) {
	sealedWorkload, err := jobmanprotocol.SealWorkload(sealed.Document.Spec.Workload.Document)
	if err != nil {
		return JobRequest{}, fmt.Errorf("seal canonical workload: %w", err)
	}
	request := JobRequest{
		Namespace:         sealed.Document.Metadata.Namespace,
		Name:              sealed.Document.Metadata.Name,
		Labels:            cloneLabels(sealed.Document.Metadata.Labels),
		Target:            sealed.Document.Spec.Placement.Target,
		Partition:         sealed.Document.Spec.Placement.Partition,
		RuntimeKind:       sealed.Document.Spec.Workload.Document.Spec.Runtime.Kind,
		WorkloadDigest:    sealed.WorkloadDigest,
		WorkloadDocument:  append(json.RawMessage(nil), sealedWorkload.CanonicalJSON...),
		RequestDigest:     sealed.RequestDigest,
		RequestDocument:   append(json.RawMessage(nil), sealed.CanonicalJSON...),
		ExecutionFeatures: executionFeatures(sealed.Document.Spec.Workload.Document),
	}
	if requirements := sealed.Document.Spec.Workload.Document.Spec.Requirements; requirements != nil {
		request.OperatingSystems = append([]string(nil), requirements.OperatingSystems...)
		request.Architectures = append([]string(nil), requirements.Architectures...)
		request.Capabilities = append([]string(nil), requirements.Capabilities...)
	}
	request.ArtifactStores, err = workloadArtifactStores(sealed.Document.Spec.Workload.Document)
	if err != nil {
		return JobRequest{}, fmt.Errorf("project workload artifact stores: %w", err)
	}

	return request, nil
}

func workloadArtifactStores(workload jobmanprotocol.Workload) ([]string, error) {
	if workload.Spec.Artifacts == nil {
		return nil, nil
	}
	seen := make(map[string]struct{})
	for _, input := range workload.Spec.Artifacts.Inputs {
		parsed, err := url.Parse(input.Source)
		if err != nil {
			return nil, fmt.Errorf("parse input artifact URI: %w", err)
		}
		seen[parsed.Host] = struct{}{}
	}
	for _, output := range workload.Spec.Artifacts.Outputs {
		parsed, err := url.Parse(output.Destination)
		if err != nil {
			return nil, fmt.Errorf("parse output artifact URI: %w", err)
		}
		seen[parsed.Host] = struct{}{}
	}
	result := make([]string, 0, len(seen))
	for name := range seen {
		result = append(result, name)
	}
	sort.Strings(result)

	return result, nil
}

func executionFeatures(workload jobmanprotocol.Workload) ExecutionFeatures {
	features := ExecutionFeatures{
		DirectCommand: workload.Spec.Command.Executable != "" && workload.Spec.Command.Shell == nil,
		Resources:     workload.Spec.Resources != nil,
		Artifacts:     workload.Spec.Artifacts != nil,
		Extensions:    len(workload.Spec.Extensions) != 0,
		RetryMaxRuns:  workload.Spec.Policy.Retry.MaxRuns,
	}
	if workload.Spec.Resources != nil {
		features.TemporaryStorage = workload.Spec.Resources.TemporaryStorage != ""
	}
	if workload.Spec.Environment == nil {
		return features
	}
	features.EnvironmentProfile = workload.Spec.Environment.Profile != ""
	features.Secrets = len(workload.Spec.Environment.Secrets) != 0
	for name := range workload.Spec.Environment.Values {
		if name == "CUDA_VISIBLE_DEVICES" || name == "ROCR_VISIBLE_DEVICES" ||
			strings.HasPrefix(name, "SLURM_") {
			features.SchedulerEnvironmentOverride = true

			break
		}
	}

	return features
}

func cloneLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(labels))
	for key, value := range labels {
		cloned[key] = value
	}

	return cloned
}
