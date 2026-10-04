package main

// cspell:words SQLSTATE

// The ceiling graph is admitted metadata, not execution evidence. It has its
// own inert target: no enrollment, agent, coordinator or scheduler call exists.
import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"

	protocol "github.com/ryancswallace/jobman-control/contracts/jobman/v1alpha1"
	"github.com/ryancswallace/jobman-control/internal/contracts"
	"github.com/ryancswallace/jobman-control/internal/domain"
	"github.com/ryancswallace/jobman-control/internal/store/postgres"
)

const (
	graphCeilingName   = "synthetic-ceiling-graph-v1"
	graphCeilingTarget = "dashboard-graph-ceiling-no-executor"
	graphCeilingNodes  = 10000
	graphCeilingEdges  = 100000
)

type graphCeilingNode struct {
	Index int    `json:"index"`
	ID    string `json:"id"`
}
type graphCeilingManifest struct {
	Version            int                `json:"version"`
	Synthetic          bool               `json:"synthetic"`
	Mode               string             `json:"mode"`
	DeploymentID       string             `json:"deploymentId"`
	InstanceID         string             `json:"instanceId"`
	RecoveryEpoch      string             `json:"recoveryEpoch"`
	NamespaceID        string             `json:"namespaceId"`
	Namespace          string             `json:"namespace"`
	TargetID           string             `json:"targetId"`
	TargetGenerationID string             `json:"targetGenerationId"`
	GraphID            string             `json:"graphId"`
	Revision           string             `json:"revision"`
	TotalNodes         string             `json:"totalNodes"`
	TotalEdges         string             `json:"totalEdges"`
	RequestDigest      string             `json:"requestDigest"`
	Nodes              []graphCeilingNode `json:"nodes"`
}

func graphCeilingNodeName(index int) string { return fmt.Sprintf("node-%05d", index) }
func graphCeilingPairs() []domain.GraphEdgeSubmission {
	pairs := make([]domain.GraphEdgeSubmission, 0, graphCeilingEdges)
	add := func(from, to int) {
		pairs = append(pairs, domain.GraphEdgeSubmission{From: graphCeilingNodeName(from), To: graphCeilingNodeName(to), Predicate: "success"})
	}
	for to := 1; to < graphCeilingNodes; to++ {
		add(0, to)
	}
	for from := 1; from <= 9000; from++ {
		for offset := 1; offset <= 10; offset++ {
			add(from, from+offset)
		}
	}
	add(9001, 9002)
	return pairs
}

func graphCeilingSubmission() (domain.GraphSubmission, error) {
	// Seal the complete bounded graph with the shared protocol. The helper does
	// not increase a service HTTP limit. Each child still uses the normal Control
	// contract decoder and repository placement/admission checks.
	request := protocol.GraphRequest{APIVersion: protocol.V1Alpha1, Kind: protocol.GraphRequestKind, Metadata: protocol.GraphRequestMetadata{Namespace: diagnosticNamespace, Name: graphCeilingName, Labels: map[string]string{"fixture": "synthetic-graph-ceiling-no-execution"}}, Spec: protocol.GraphRequestSpec{MaxActive: 1, UnsatisfiedPolicy: "skip"}}
	for index := range graphCeilingNodes {
		request.Spec.Nodes = append(request.Spec.Nodes, protocol.GraphNode{Name: graphCeilingNodeName(index), Workload: protocol.WorkloadBinding{Document: fixtureWorkload()}, Placement: protocol.Placement{Target: graphCeilingTarget}})
	}
	pairs := graphCeilingPairs()
	for _, edge := range pairs {
		request.Spec.Edges = append(request.Spec.Edges, protocol.GraphEdge{From: edge.From, To: edge.To, Predicate: edge.Predicate})
	}
	sealed, err := protocol.SealGraphRequest(request)
	if err != nil {
		return domain.GraphSubmission{}, err
	}
	if len(sealed.CanonicalJSON) > 32<<20 {
		return domain.GraphSubmission{}, errors.New("ceiling graph document exceeds private bound")
	}
	result := domain.GraphSubmission{Namespace: diagnosticNamespace, Name: graphCeilingName, Labels: request.Metadata.Labels, MaxActive: 1, UnsatisfiedPolicy: "skip", RequestDigest: sealed.RequestDigest, RequestDocument: sealed.CanonicalJSON, Edges: pairs}
	for _, node := range sealed.Document.Spec.Nodes {
		child, sealErr := protocol.SealJobRequest(protocol.JobRequest{APIVersion: protocol.V1Alpha1, Kind: protocol.JobRequestKind, Metadata: protocol.JobRequestMetadata{Namespace: diagnosticNamespace, Name: node.Name, Labels: request.Metadata.Labels}, Spec: protocol.JobRequestSpec{Workload: node.Workload, Placement: node.Placement}})
		if sealErr != nil {
			return domain.GraphSubmission{}, sealErr
		}
		decoded, decodeErr := contracts.DecodeJobRequest(bytes.NewReader(child.CanonicalJSON))
		if decodeErr != nil {
			return domain.GraphSubmission{}, decodeErr
		}
		result.Nodes = append(result.Nodes, submissionFrom(decoded))
	}
	return result, nil
}

func seedGraphCeiling(ctx context.Context, store *postgres.Store, principal domain.Principal, baseline graphCeilingPreflight) (graphCeilingManifest, error) {
	var result graphCeilingManifest
	request, err := graphCeilingSubmission()
	if err != nil {
		return result, err
	}
	target, err := store.CreateTarget(ctx, principal, diagnosticNamespace, "graph-ceiling-target-v1", fixtureDigest(graphCeilingTarget), domain.TargetSpec{Name: graphCeilingTarget, Kind: "host", ExecutionBackend: "subprocess", Runtimes: []string{"native"}, OperatingSystems: []string{"linux"}, Architectures: []string{"amd64", "arm64"}})
	if err != nil {
		return result, err
	}
	if target.Replayed {
		return result, errors.New("ceiling target was already admitted")
	}
	admitted, err := store.SubmitGraph(ctx, principal, "graph-ceiling-v1", request)
	if err != nil {
		return result, err
	}
	if admitted.Replayed || admitted.Graph.Total != graphCeilingNodes || len(admitted.Graph.Items) != graphCeilingNodes {
		return result, errors.New("unexpected ceiling graph admission")
	}
	result = graphCeilingManifest{Version: 1, Synthetic: true, Mode: "admitted-no-execution", DeploymentID: diagnosticDeployment, InstanceID: baseline.InstanceID, RecoveryEpoch: baseline.RecoveryEpoch, NamespaceID: baseline.NamespaceID, Namespace: diagnosticNamespace, TargetID: target.Value.ID, TargetGenerationID: target.Value.GenerationID, GraphID: admitted.Graph.ID, Revision: strconv.FormatInt(admitted.Graph.Revision, 10), TotalNodes: "10000", TotalEdges: "100000", RequestDigest: request.RequestDigest}
	for _, node := range admitted.Graph.Items {
		result.Nodes = append(result.Nodes, graphCeilingNode{Index: node.Index, ID: node.Job.ID})
	}
	return result, nil
}

func verifyGraphCeiling(ctx context.Context, pool *pgxpool.Pool, store *postgres.Store, principal domain.Principal, value graphCeilingManifest) error {
	if value.Version != 1 || !value.Synthetic || value.Mode != "admitted-no-execution" || value.DeploymentID != diagnosticDeployment || value.Namespace != diagnosticNamespace || !domain.IsID(value.InstanceID) || value.RecoveryEpoch != "1" || !domain.IsID(value.NamespaceID) || !domain.IsID(value.TargetID) || !domain.IsID(value.TargetGenerationID) || !domain.IsID(value.GraphID) || value.Revision != "1" || value.TotalNodes != "10000" || value.TotalEdges != "100000" || len(value.Nodes) != graphCeilingNodes {
		return errors.New("invalid ceiling graph identity")
	}
	caps, err := store.Capabilities(ctx)
	if err != nil || caps.InstanceID != value.InstanceID || caps.RecoveryEpoch != value.RecoveryEpoch {
		return errors.New("ceiling source changed")
	}
	var nodes, edges, unexpected, runs, executions, agents, enrollments, graphIdentity int
	err = pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM jobs WHERE namespace_id=$1 AND graph_id=$2),
 (SELECT count(*) FROM graph_edges WHERE graph_id=$2),
 (SELECT count(*) FROM jobs WHERE graph_id=$2 AND (phase<>'accepted' OR imported OR target_id<>$3 OR target_generation_id<>$4 OR graph_disposition IS NOT NULL)),
 (SELECT count(*) FROM runs WHERE job_id IN(SELECT id FROM jobs WHERE graph_id=$2)),
 (SELECT count(*) FROM executions e JOIN runs r ON r.id=e.run_id WHERE r.job_id IN(SELECT id FROM jobs WHERE graph_id=$2)),
 (SELECT count(*) FROM agents WHERE target_generation_id=$4),
 (SELECT count(*) FROM agent_enrollment_tokens WHERE target_generation_id=$4),
 (SELECT count(*) FROM graphs WHERE id=$2 AND namespace_id=$1 AND name=$5 AND max_active=1 AND unsatisfied_policy='skip' AND revision=1 AND request_digest=$6)`, value.NamespaceID, value.GraphID, value.TargetID, value.TargetGenerationID, graphCeilingName, value.RequestDigest).Scan(&nodes, &edges, &unexpected, &runs, &executions, &agents, &enrollments, &graphIdentity)
	if err != nil {
		return graphCeilingQueryFailure(err)
	}
	if nodes != graphCeilingNodes || edges != graphCeilingEdges || unexpected+runs+executions+agents+enrollments != 0 || graphIdentity != 1 {
		return fmt.Errorf("ceiling graph fact mismatch: nodes=%d edges=%d unexpected=%d runs=%d executions=%d agents=%d enrollments=%d graph_identity=%d", nodes, edges, unexpected, runs, executions, agents, enrollments, graphIdentity)
	}
	seen := map[string]bool{}
	for index := range graphCeilingNodes {
		if value.Nodes[index].Index != index || !domain.IsID(value.Nodes[index].ID) || seen[value.Nodes[index].ID] {
			return errors.New("invalid ceiling node mapping")
		}
		seen[value.Nodes[index].ID] = true
	}
	for after := -1; after < graphCeilingNodes-1; {
		page, pageErr := store.ListGraphNodes(ctx, principal, diagnosticNamespace, value.GraphID, after, 200)
		if pageErr != nil || page.Total != graphCeilingNodes || len(page.Items) == 0 || len(page.Items) > 200 {
			return errors.New("ceiling node read failed")
		}
		for _, node := range page.Items {
			if node.Index != after+1 || node.Index < 0 || node.Index >= graphCeilingNodes || node.Job.ID != value.Nodes[node.Index].ID || node.Name != graphCeilingNodeName(node.Index) || node.Job.Phase != "accepted" || node.Dependencies.Waiting != node.Dependencies.Total || node.Dependencies.Satisfied+node.Dependencies.Unsatisfied != 0 {
				return errors.New("ceiling node provenance differs")
			}
			after = node.Index
		}
	}
	// Compare the complete immutable edge relation without loading job documents.
	expected := map[[2]string]bool{}
	for _, edge := range graphCeilingPairs() {
		from, parseFrom := strconv.Atoi(edge.From[5:])
		to, parseTo := strconv.Atoi(edge.To[5:])
		if parseFrom != nil || parseTo != nil {
			return errors.New("invalid fixed topology")
		}
		expected[[2]string{value.Nodes[from].ID, value.Nodes[to].ID}] = true
	}
	options := domain.GraphEdgeOptions{Limit: 500}
	observed := 0
	for {
		page, pageErr := store.ListGraphEdges(ctx, principal, diagnosticNamespace, value.GraphID, options)
		if pageErr != nil || page.Total != graphCeilingEdges || len(page.Items) == 0 || len(page.Items) > 500 {
			return errors.New("ceiling edge read failed")
		}
		for _, edge := range page.Items {
			pair := [2]string{edge.FromJobID, edge.ToJobID}
			if !expected[pair] || edge.Predicate != "success" || edge.State != "waiting" || edge.UpstreamPhase != "accepted" || len(edge.Outcomes) != 0 {
				return errors.New("ceiling edge provenance differs")
			}
			delete(expected, pair)
			observed++
		}
		if page.NextFromID == "" {
			break
		}
		options.AfterFromID = page.NextFromID
		options.AfterToID = page.NextToID
		if observed >= graphCeilingEdges {
			return errors.New("ceiling edge continuation overflow")
		}
	}
	if len(expected) != 0 || observed != graphCeilingEdges {
		return errors.New("incomplete ceiling edge relation")
	}
	final, err := store.Capabilities(ctx)
	if err != nil || final.InstanceID != value.InstanceID || final.RecoveryEpoch != value.RecoveryEpoch {
		return errors.New("ceiling source changed during verification")
	}
	return nil
}

// Never include a wrapped database error: driver messages can contain SQL or
// configuration. A finite standard SQLSTATE locates schema failures safely.
func graphCeilingQueryFailure(err error) error {
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		code := state.SQLState()
		valid := len(code) == 5
		for _, char := range code {
			if (char < '0' || char > '9') && (char < 'A' || char > 'Z') {
				valid = false
			}
		}
		if valid {
			return fmt.Errorf("ceiling fact query failed (sqlstate=%s)", code)
		}
	}
	return errors.New("ceiling fact query failed")
}
