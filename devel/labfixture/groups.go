package main

import (
	"bytes"
	"context"
	"fmt"

	protocol "github.com/ryancswallace/jobman-control/contracts/jobman/v1alpha1"
	"github.com/ryancswallace/jobman-control/internal/contracts"
	"github.com/ryancswallace/jobman-control/internal/domain"
	"github.com/ryancswallace/jobman-control/internal/store/postgres"
)

func seedGroups(ctx context.Context, store *postgres.Store, principal domain.Principal, namespace *fixtureNamespace) error {
	for _, array := range []bool{false, true} {
		name, target, policy := "synthetic-collection", fixtureTarget, "never"
		if array {
			name, target, policy = "synthetic-array", "synthetic-slurm", "require"
			if _, err := store.CreateTarget(ctx, principal, namespace.Name, "fixture-slurm-v1", fixtureDigest(namespace.Name+"slurm"), domain.TargetSpec{Name: target, Kind: "slurm", ExecutionBackend: "slurm", Runtimes: []string{"native"}, OperatingSystems: []string{"linux"}, Architectures: []string{"amd64"}, Partitions: []domain.PartitionSpec{{Name: "synthetic", IsDefault: true}}}); err != nil {
				return err
			}
		}
		request := protocol.CollectionRequest{APIVersion: protocol.V1Alpha1, Kind: protocol.CollectionRequestKind, Metadata: protocol.CollectionRequestMetadata{Namespace: namespace.Name, Name: name, Labels: map[string]string{"fixture": "synthetic-dashboard-lab"}}, Spec: protocol.CollectionRequestSpec{MaxActive: 2, FailurePolicy: "continue", ArrayPolicy: policy}}
		for index := range 3 {
			request.Spec.Items = append(request.Spec.Items, protocol.CollectionItem{Name: fmt.Sprintf("item-%d", index), Workload: protocol.WorkloadBinding{Document: fixtureWorkload()}, Placement: protocol.Placement{Target: target}})
		}
		sealed, err := protocol.SealCollectionRequest(request)
		if err != nil {
			return err
		}
		decoded, err := contracts.DecodeCollectionRequest(bytes.NewReader(sealed.CanonicalJSON))
		if err != nil {
			return err
		}
		submission := domain.CollectionSubmission{Namespace: decoded.Namespace, Name: decoded.Name, Labels: decoded.Labels, MaxActive: decoded.MaxActive, FailurePolicy: decoded.FailurePolicy, ArrayPolicy: decoded.ArrayPolicy, RequestDigest: decoded.RequestDigest, RequestDocument: decoded.RequestDocument}
		for _, item := range decoded.Items {
			submission.Items = append(submission.Items, submissionFrom(item))
		}
		result, err := store.SubmitCollection(ctx, principal, "fixture-"+name+"-v1", submission)
		if err != nil {
			return err
		}
		if array {
			namespace.ArrayID = result.Collection.ID
		} else {
			namespace.CollectionID = result.Collection.ID
		}
	}
	request := protocol.GraphRequest{APIVersion: protocol.V1Alpha1, Kind: protocol.GraphRequestKind, Metadata: protocol.GraphRequestMetadata{Namespace: namespace.Name, Name: "synthetic-graph", Labels: map[string]string{"fixture": "synthetic-dashboard-lab"}}, Spec: protocol.GraphRequestSpec{MaxActive: 1, UnsatisfiedPolicy: "skip", Edges: []protocol.GraphEdge{{From: "first", To: "second", Predicate: "success"}}}}
	for _, name := range []string{"first", "second"} {
		request.Spec.Nodes = append(request.Spec.Nodes, protocol.GraphNode{Name: name, Workload: protocol.WorkloadBinding{Document: fixtureWorkload()}, Placement: protocol.Placement{Target: fixtureTarget}})
	}
	sealed, err := protocol.SealGraphRequest(request)
	if err != nil {
		return err
	}
	decoded, err := contracts.DecodeGraphRequest(bytes.NewReader(sealed.CanonicalJSON))
	if err != nil {
		return err
	}
	submission := domain.GraphSubmission{Namespace: decoded.Namespace, Name: decoded.Name, Labels: decoded.Labels, MaxActive: decoded.MaxActive, UnsatisfiedPolicy: decoded.UnsatisfiedPolicy, RequestDigest: decoded.RequestDigest, RequestDocument: decoded.RequestDocument}
	for _, node := range decoded.Nodes {
		submission.Nodes = append(submission.Nodes, submissionFrom(node))
	}
	for _, edge := range decoded.Edges {
		submission.Edges = append(submission.Edges, domain.GraphEdgeSubmission{From: edge.From, To: edge.To, Predicate: edge.Predicate, Outcomes: edge.Outcomes})
	}
	result, err := store.SubmitGraph(ctx, principal, "fixture-graph-v1", submission)
	if err != nil {
		return err
	}
	namespace.GraphID = result.Graph.ID
	return nil
}
