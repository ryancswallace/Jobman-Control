package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func TestGroupMonitoringIntegration(t *testing.T) {
	databaseURL := os.Getenv("JOBMAN_CONTROL_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("JOBMAN_CONTROL_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	pool := newIntegrationPool(ctx, t, databaseURL)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := New(pool, []byte("0123456789abcdef0123456789abcdef"))
	principal := domain.Principal{Issuer: "test-issuer", Subject: "group-monitor"}
	if err := store.EnsureDevelopmentIdentity(ctx, domain.DevelopmentIdentity{Principal: principal, DisplayName: "Group viewer", Namespace: "research"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateTarget(ctx, principal, "research", "group-target", "sha256:"+strings.Repeat("1", 64), domain.TargetSpec{Name: "workstation-a", Kind: "host", ExecutionBackend: "subprocess", Runtimes: []string{"native"}, OperatingSystems: []string{"linux"}, Architectures: []string{"amd64"}}); err != nil {
		t.Fatal(err)
	}
	nodes := make([]domain.JobSubmission, 5)
	for i := range nodes {
		nodes[i] = integrationSubmission(t)
		nodes[i].Name = fmt.Sprintf("node-%d", i)
		nodes[i].RequestDigest = fmt.Sprintf("sha256:%064x", i+30)
	}
	graph, err := store.SubmitGraph(ctx, principal, "bounded-graph", domain.GraphSubmission{Namespace: "research", Name: "bounded-graph", MaxActive: 2, UnsatisfiedPolicy: "skip", RequestDigest: "sha256:" + strings.Repeat("2", 64), RequestDocument: json.RawMessage(`{}`), Nodes: nodes, Edges: []domain.GraphEdgeSubmission{
		{From: "node-0", To: "node-1", Predicate: "success"}, {From: "node-0", To: "node-2", Predicate: "failure"}, {From: "node-1", To: "node-2", Predicate: "any-terminal"}, {From: "node-2", To: "node-3", Predicate: "outcomes", Outcomes: []string{"success"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	id := graph.Graph.ID
	centerID := graph.Graph.Items[0].Job.ID
	catalog, err := store.ListGraphs(ctx, principal, "research", domain.GroupListOptions{Limit: 1})
	if err != nil || catalog.Total != 1 || len(catalog.Items) != 1 || len(catalog.Items[0].Items) != 0 || catalog.Items[0].Total != 5 || catalog.AsOf.IsZero() {
		t.Fatalf("graph catalog=%#v,%v", catalog, err)
	}
	cutoff := graph.Graph.CreatedAt.Add(-time.Microsecond)
	empty, err := store.ListGraphs(ctx, principal, "research", domain.GroupListOptions{Limit: 1, CreatedBefore: &cutoff})
	if err != nil || len(empty.Items) != 0 || empty.Total != 0 {
		t.Fatalf("creation cutoff=%#v,%v", empty, err)
	}
	summary, err := store.GraphSummary(ctx, principal, "research", id)
	if err != nil || summary.Graph.Total != 5 || len(summary.Graph.Items) != 0 {
		t.Fatalf("graph summary=%#v,%v", summary, err)
	}
	page, err := store.ListGraphNodes(ctx, principal, "research", id, -1, 2)
	if err != nil || len(page.Items) != 2 || page.Total != 5 || page.NextIndex == nil || *page.NextIndex != 1 || page.Items[1].Dependencies.Waiting != 1 {
		t.Fatalf("nodes=%#v,%v", page, err)
	}
	second, err := store.ListGraphNodes(ctx, principal, "research", id, *page.NextIndex, 2)
	if err != nil || len(second.Items) != 2 || second.Items[0].Index != 2 || second.Items[0].Dependencies.Total != 2 {
		t.Fatalf("next nodes=%#v,%v", second, err)
	}
	dependencies, err := store.ListGraphEdges(ctx, principal, "research", id, domain.GraphEdgeOptions{Limit: 1, NodeID: centerID, Direction: "outgoing"})
	if err != nil || len(dependencies.Items) != 1 || dependencies.Items[0].State != "waiting" || dependencies.NextFromID == "" {
		t.Fatalf("dependencies=%#v,%v", dependencies, err)
	}
	nextEdges, err := store.ListGraphEdges(ctx, principal, "research", id, domain.GraphEdgeOptions{Limit: 1, NodeID: centerID, Direction: "outgoing", AfterFromID: dependencies.NextFromID, AfterToID: dependencies.NextToID})
	if err != nil || len(nextEdges.Items) != 1 || nextEdges.Items[0].ToJobID == dependencies.Items[0].ToJobID || nextEdges.NextFromID != "" {
		t.Fatalf("next dependencies=%#v,%v", nextEdges, err)
	}
	// Terminal predicates distinguish satisfied success from unsatisfied failure.
	if _, err = pool.Exec(ctx, `UPDATE jobs SET phase='terminal',outcome='success' WHERE id=$1`, centerID); err != nil {
		t.Fatal(err)
	}
	terminal, err := store.ListGraphNodes(ctx, principal, "research", id, -1, 5)
	if err != nil || terminal.Items[1].Dependencies.Satisfied != 1 || terminal.Items[2].Dependencies.Unsatisfied != 1 || terminal.Items[2].Dependencies.Waiting != 1 {
		t.Fatalf("terminal predicates=%#v,%v", terminal, err)
	}
	neighborhood, err := store.GraphNeighborhood(ctx, principal, "research", id, centerID, 2, 1)
	if err != nil || len(neighborhood.Nodes) != 2 || len(neighborhood.Edges) != 1 || neighborhood.TotalNodes != 3 || neighborhood.TotalEdges != 3 || neighborhood.OmittedNodes != 1 || neighborhood.OmittedEdges != 2 {
		t.Fatalf("bounded neighborhood=%#v,%v", neighborhood, err)
	}
	// Every edge endpoint is present even when node and edge limits both truncate.
	known := map[string]bool{}
	for _, node := range neighborhood.Nodes {
		known[node.Job.ID] = true
	}
	for _, edge := range neighborhood.Edges {
		if !known[edge.FromJobID] || !known[edge.ToJobID] {
			t.Fatal("dangling neighborhood edge")
		}
	}
	if !known[centerID] {
		t.Fatal("neighborhood omitted its center")
	}
	isolated, err := store.GraphNeighborhood(ctx, principal, "research", id, graph.Graph.Items[4].Job.ID, 1, 1)
	if err != nil || isolated.TotalNodes != 1 || len(isolated.Edges) != 0 || len(isolated.Nodes) != 1 {
		t.Fatalf("isolated neighborhood=%#v,%v", isolated, err)
	}
	if _, err = store.ListGraphEdges(ctx, principal, "research", id, domain.GraphEdgeOptions{Limit: 10, NodeID: "11111111-1111-4111-8111-111111111111"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("foreign node=%v", err)
	}
	outsider := domain.Principal{Issuer: principal.Issuer, Subject: "outsider"}
	for _, read := range []func() error{
		func() error {
			_, e := store.ListGraphs(ctx, outsider, "research", domain.GroupListOptions{Limit: 1})
			return e
		},
		func() error { _, e := store.GraphSummary(ctx, outsider, "research", id); return e },
		func() error { _, e := store.ListGraphNodes(ctx, outsider, "research", id, -1, 1); return e },
		func() error {
			_, e := store.ListGraphEdges(ctx, outsider, "research", id, domain.GraphEdgeOptions{Limit: 1})
			return e
		},
		func() error { _, e := store.GraphNeighborhood(ctx, outsider, "research", id, centerID, 1, 1); return e },
	} {
		if e := read(); !errors.Is(e, domain.ErrForbidden) {
			t.Fatalf("unauthorized graph read=%v", e)
		}
	}
	testCollectionMonitoring(ctx, t, store, principal)
}

func testCollectionMonitoring(ctx context.Context, t *testing.T, store *Store, principal domain.Principal) {
	t.Helper()
	submission := domain.CollectionSubmission{Namespace: "research", Name: "bounded-collection", MaxActive: 2, FailurePolicy: "continue", ArrayPolicy: "never", RequestDigest: "sha256:" + strings.Repeat("3", 64), RequestDocument: json.RawMessage(`{}`), Items: []domain.JobSubmission{integrationSubmission(t), integrationSubmission(t)}}
	submission.Items[0].Name = "child-0"
	submission.Items[1].Name = "child-1"
	first, err := store.SubmitCollection(ctx, principal, "collection-one", submission)
	if err != nil {
		t.Fatal(err)
	}
	submission.Name = "second-collection"
	submission.RequestDigest = "sha256:" + strings.Repeat("4", 64)
	second, err := store.SubmitCollection(ctx, principal, "collection-two", submission)
	if err != nil {
		t.Fatal(err)
	}
	// Stored array mapping is the immutable collection index used by assignment.
	if _, err = store.pool.Exec(ctx, `UPDATE collections SET array_mode='slurm-array' WHERE id=$1`, second.Collection.ID); err != nil {
		t.Fatal(err)
	}
	page, err := store.ListCollections(ctx, principal, "research", domain.GroupListOptions{Limit: 1})
	if err != nil || page.Total != 2 || len(page.Items) != 1 || page.NextCursor == nil || len(page.Items[0].Items) != 0 {
		t.Fatalf("collection catalog=%#v,%v", page, err)
	}
	next, err := store.ListCollections(ctx, principal, "research", domain.GroupListOptions{Limit: 1, Before: page.NextCursor})
	if err != nil || len(next.Items) != 1 || next.Items[0].ID != first.Collection.ID || next.Total != 2 || next.NextCursor != nil {
		t.Fatalf("next catalog=%#v,%v", next, err)
	}
	arrays, err := store.ListCollections(ctx, principal, "research", domain.GroupListOptions{Limit: 10, ArrayMode: "slurm-array"})
	if err != nil || arrays.Total != 1 || len(arrays.Items) != 1 || arrays.Items[0].ID != second.Collection.ID {
		t.Fatalf("array catalog=%#v,%v", arrays, err)
	}
	summary, err := store.CollectionSummary(ctx, principal, "research", second.Collection.ID)
	if err != nil || summary.Collection.Total != 2 || len(summary.Collection.Items) != 0 {
		t.Fatalf("collection summary=%#v,%v", summary, err)
	}
	items, err := store.ListCollectionItems(ctx, principal, "research", second.Collection.ID, 0, 1)
	if err != nil || items.Total != 2 || len(items.Items) != 1 || items.Items[0].ArrayTaskIndex == nil || *items.Items[0].ArrayTaskIndex != 1 || items.Items[0].Index != 1 || items.NextIndex != nil {
		t.Fatalf("array source index=%#v,%v", items, err)
	}
	normal, err := store.ListCollectionItems(ctx, principal, "research", first.Collection.ID, -1, 1)
	if err != nil || normal.Items[0].ArrayTaskIndex != nil || normal.NextIndex == nil || *normal.NextIndex != 0 {
		t.Fatalf("ordinary collection=%#v,%v", normal, err)
	}
	outsider := domain.Principal{Issuer: principal.Issuer, Subject: "outsider"}
	if _, err = store.ListCollections(ctx, outsider, "research", domain.GroupListOptions{Limit: 1}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("unauthorized catalog=%v", err)
	}
	if _, err = store.CollectionSummary(ctx, outsider, "research", first.Collection.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("unauthorized summary=%v", err)
	}
	if _, err = store.ListCollectionItems(ctx, outsider, "research", first.Collection.ID, -1, 1); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("unauthorized children=%v", err)
	}
}
