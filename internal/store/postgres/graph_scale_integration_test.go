package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

// This explicit disposable-schema fixture measures navigation of the supported
// graph ceiling. Direct SQL seeds synthetic accepted metadata; it is neither a
// graph-submission benchmark nor evidence that any of these jobs ran.
func TestGraphNavigationCeilingIntegration(t *testing.T) {
	if os.Getenv("JOBMAN_CONTROL_TEST_SCALE") != "1" || os.Getenv("JOBMAN_CONTROL_TEST_DATABASE_URL") == "" {
		t.Skip("explicit disposable database and scale-test opt-ins required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	pool := newIntegrationPool(ctx, t, os.Getenv("JOBMAN_CONTROL_TEST_DATABASE_URL"))
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := New(pool, []byte("0123456789abcdef0123456789abcdef"))
	actor := domain.Principal{Issuer: "https://synthetic-scale.invalid", Subject: "graph-viewer"}
	if err := store.EnsureDevelopmentIdentity(ctx, domain.DevelopmentIdentity{Principal: actor, DisplayName: "Graph viewer", Namespace: "research"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateTarget(ctx, actor, "research", "graph-scale-target", "sha256:"+strings.Repeat("1", 64), domain.TargetSpec{Name: "workstation-a", Kind: "host", ExecutionBackend: "subprocess", Runtimes: []string{"native"}, OperatingSystems: []string{"linux"}, Architectures: []string{"amd64"}}); err != nil {
		t.Fatal(err)
	}
	nodes := []domain.JobSubmission{integrationSubmission(t), integrationSubmission(t)}
	nodes[0].Name, nodes[1].Name = "node-0", "node-1"
	seed, err := store.SubmitGraph(ctx, actor, "graph-scale", domain.GraphSubmission{Namespace: "research", Name: "synthetic-ceiling", MaxActive: 1, UnsatisfiedPolicy: "skip", RequestDigest: "sha256:" + strings.Repeat("2", 64), RequestDocument: json.RawMessage(`{}`), Nodes: nodes})
	if err != nil {
		t.Fatal(err)
	}
	graphID, centerID := seed.Graph.ID, seed.Graph.Items[0].Job.ID
	// Clone only metadata in this disposable schema. No agent, coordinator,
	// live source, outbox publication or scheduler is attached to the fixture.
	_, err = pool.Exec(ctx, `
 INSERT INTO jobs(id,namespace_id,owner_principal_id,name,labels,phase,desired_state,
 placement_target,placement_partition,workload_digest,request_digest,request_document,
 target_id,target_generation_id,graph_id,graph_index)
 SELECT gen_random_uuid(),j.namespace_id,j.owner_principal_id,'node-'||g::text,
 jsonb_build_object('fixture','synthetic-graph-ceiling'),'accepted','run',
 j.placement_target,j.placement_partition,j.workload_digest,j.request_digest,j.request_document,
 j.target_id,j.target_generation_id,j.graph_id,g
 FROM jobs j CROSS JOIN generate_series(2,9999) g WHERE j.id=$1::uuid`, centerID)
	if err != nil {
		t.Fatal("seed synthetic nodes:", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO graph_nodes(graph_id,node_index,node_name,job_id)
 SELECT graph_id,graph_index,name,id FROM jobs WHERE graph_id=$1 AND graph_index>=2`, graphID)
	if err != nil {
		t.Fatal(err)
	}
	// A 9,999-edge root star makes the center's one-hop neighborhood the whole
	// graph. Another 90,001 forward edges produce a DAG with exactly 100,000
	// edges, exercising both node and edge truncation in one bounded response.
	_, err = pool.Exec(ctx, `INSERT INTO graph_edges(graph_id,upstream_job_id,downstream_job_id,predicate)
 SELECT $1::uuid,$2::uuid,job_id,'success' FROM graph_nodes WHERE graph_id=$1 AND node_index>0`, graphID, centerID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO graph_edges(graph_id,upstream_job_id,downstream_job_id,predicate)
 SELECT source.graph_id,source.job_id,destination.job_id,'success'
 FROM graph_nodes source CROSS JOIN generate_series(1,10) distance
 JOIN graph_nodes destination ON destination.graph_id=$1 AND destination.node_index=source.node_index+distance
 WHERE source.graph_id=$1 AND source.node_index>0
 ORDER BY source.node_index,distance LIMIT 90001`, graphID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE memberships SET role='viewer'; ANALYZE jobs; ANALYZE graphs; ANALYZE graph_nodes; ANALYZE graph_edges`); err != nil {
		t.Fatal(err)
	}
	summary, err := store.GraphSummary(ctx, actor, "research", graphID)
	if err != nil || summary.Graph.Total != 10000 || len(summary.Graph.Items) != 0 {
		t.Fatal("complete summary unavailable:", err)
	}
	timings := map[string][]time.Duration{}
	maxBytes := map[string]int{}
	measure := func(operation string, began time.Time, value any) {
		t.Helper()
		timings[operation] = append(timings[operation], time.Since(began))
		raw, err := json.Marshal(value)
		if err != nil || len(raw) > 2<<20 {
			t.Fatal("bounded response serialization failed:", err)
		}
		maxBytes[operation] = max(maxBytes[operation], len(raw))
	}
	after, count := -1, 0
	seen := make(map[string]bool, 10000)
	for page := 0; page < 50; page++ {
		began := time.Now()
		result, err := store.ListGraphNodes(ctx, actor, "research", graphID, after, 200)
		measure("nodes", began, result)
		if err != nil || result.Total != 10000 || len(result.Items) != 200 || result.AsOf.IsZero() {
			t.Fatal("bounded node page failed:", err)
		}
		for _, node := range result.Items {
			if node.Index != count || seen[node.Job.ID] || node.Dependencies.Waiting != node.Dependencies.Total || node.Dependencies.Satisfied != 0 || node.Dependencies.Unsatisfied != 0 {
				t.Fatal("node order, uniqueness or exact pending predicates differ")
			}
			seen[node.Job.ID] = true
			count++
		}
		switch {
		case page == 49:
			if result.NextIndex != nil {
				t.Fatal("final node page still has a continuation")
			}
		case result.NextIndex == nil || *result.NextIndex != count-1:
			t.Fatal("node continuation differs")
		default:
			after = *result.NextIndex
		}
	}
	var from, to string
	edgeCount := 0
	for page := 0; page < 200; page++ {
		began := time.Now()
		result, err := store.ListGraphEdges(ctx, actor, "research", graphID, domain.GraphEdgeOptions{Limit: 500, AfterFromID: from, AfterToID: to})
		measure("edges", began, result)
		if err != nil || result.Total != 100000 || len(result.Items) != 500 || result.AsOf.IsZero() {
			t.Fatal("bounded edge page failed:", err)
		}
		for _, edge := range result.Items {
			if edge.FromJobID < from || (edge.FromJobID == from && edge.ToJobID <= to) || !seen[edge.FromJobID] || !seen[edge.ToJobID] || edge.State != "waiting" || edge.Predicate != "success" {
				t.Fatal("edge order, endpoint or pending predicate differs")
			}
			from, to = edge.FromJobID, edge.ToJobID
			edgeCount++
		}
		if page == 199 {
			if result.NextFromID != "" || result.NextToID != "" {
				t.Fatal("final edge page still has a continuation")
			}
		} else if result.NextFromID != from || result.NextToID != to {
			t.Fatal("edge continuation differs")
		}
	}
	if count != 10000 || edgeCount != 100000 {
		t.Fatal("incomplete graph traversal")
	}
	for range 20 {
		began := time.Now()
		result, err := store.GraphNeighborhood(ctx, actor, "research", graphID, centerID, 200, 500)
		measure("neighborhood", began, result)
		if err != nil || result.TotalNodes != 10000 || result.TotalEdges != 100000 || len(result.Nodes) != 200 || len(result.Edges) != 500 || result.OmittedNodes != 9800 || result.OmittedEdges != 99500 || result.AsOf.IsZero() {
			t.Fatal("complete neighborhood omission counts differ:", err)
		}
		selected := map[string]bool{}
		for _, node := range result.Nodes {
			selected[node.Job.ID] = true
		}
		if !selected[centerID] {
			t.Fatal("truncated neighborhood omitted its center")
		}
		for _, edge := range result.Edges {
			if !selected[edge.FromJobID] || !selected[edge.ToJobID] {
				t.Fatal("truncated neighborhood has a dangling edge")
			}
		}
	}
	outsider := domain.Principal{Issuer: actor.Issuer, Subject: "outside-graph"}
	if _, err := store.GraphNeighborhood(ctx, outsider, "research", graphID, centerID, 200, 500); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("unauthorized ceiling graph read was not denied")
	}
	for _, operation := range []string{"nodes", "edges", "neighborhood"} {
		values := timings[operation]
		slices.Sort(values)
		p95 := values[(len(values)*95+99)/100-1]
		t.Logf("source graph navigation: operation=%s samples=%d p95=%s max=%s maxSerializedBytes=%d nodes=10000 edges=100000", operation, len(values), p95, values[len(values)-1], maxBytes[operation])
		if p95 > 2*time.Second {
			t.Errorf("%s repository p95 exceeds two seconds before network/client overhead", operation)
		}
	}
}
