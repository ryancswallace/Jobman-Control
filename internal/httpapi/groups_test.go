package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

type groupRepository struct {
	domain.ControlRepository
	options     domain.GroupListOptions
	edgeOptions domain.GraphEdgeOptions
}

func (repository *groupRepository) ListCollections(_ context.Context, _ domain.Principal, _ string, options domain.GroupListOptions) (domain.ResourcePage[domain.Collection], error) {
	repository.options = options
	return domain.ResourcePage[domain.Collection]{Items: []domain.Collection{{ID: testJobID, Namespace: "research", Total: 10000, Items: []domain.CollectionItem{}}}, Total: 3, AsOf: time.Now(), NextCursor: &domain.JobCursor{ID: testJobID, CreatedAt: time.Now()}}, nil
}

func (repository *groupRepository) ListGraphs(_ context.Context, _ domain.Principal, _ string, options domain.GroupListOptions) (domain.ResourcePage[domain.Graph], error) {
	repository.options = options
	return domain.ResourcePage[domain.Graph]{Items: []domain.Graph{{ID: testJobID, Total: 10000}}, Total: 1, AsOf: time.Now()}, nil
}

func (repository *groupRepository) CollectionSummary(context.Context, domain.Principal, string, string) (domain.CollectionSnapshot, error) {
	return domain.CollectionSnapshot{Collection: domain.Collection{ID: testJobID, Total: 10000}}, nil
}

func (repository *groupRepository) GraphSummary(context.Context, domain.Principal, string, string) (domain.GraphSnapshot, error) {
	return domain.GraphSnapshot{Graph: domain.Graph{ID: testJobID, Total: 10000}}, nil
}

func (repository *groupRepository) ListCollectionItems(context.Context, domain.Principal, string, string, int, int) (domain.ResourcePage[domain.CollectionItem], error) {
	index := 99
	return domain.ResourcePage[domain.CollectionItem]{Items: []domain.CollectionItem{{Index: index, ArrayTaskIndex: &index, Job: domain.Job{ID: testJobID}}}, Total: 10000, NextIndex: &index}, nil
}

func (repository *groupRepository) ListGraphNodes(context.Context, domain.Principal, string, string, int, int) (domain.ResourcePage[domain.GraphNodeSnapshot], error) {
	return domain.ResourcePage[domain.GraphNodeSnapshot]{Items: []domain.GraphNodeSnapshot{{Index: 3, Dependencies: domain.DependencyCounts{Total: 40, Waiting: 30, Satisfied: 10}, Job: domain.Job{ID: testJobID}}}, Total: 10000}, nil
}

func (repository *groupRepository) ListGraphEdges(_ context.Context, _ domain.Principal, _, _ string, options domain.GraphEdgeOptions) (domain.GraphEdgePage, error) {
	repository.edgeOptions = options
	return domain.GraphEdgePage{Items: []domain.GraphEdgeSnapshot{{FromJobID: testJobID, ToJobID: testJobID, State: "waiting"}}, NextFromID: testJobID, NextToID: testJobID}, nil
}

func (repository *groupRepository) GraphNeighborhood(context.Context, domain.Principal, string, string, string, int, int) (domain.GraphNeighborhood, error) {
	return domain.GraphNeighborhood{CenterID: testJobID, Nodes: []domain.GraphNodeSnapshot{}, Edges: []domain.GraphEdgeSnapshot{}, TotalNodes: 500, OmittedNodes: 500}, nil
}

func TestBoundedGroupHTTPContracts(t *testing.T) {
	t.Parallel()
	repository := &groupRepository{}
	handler := newTestHandler(t, repository, 1024)
	paths := []struct{ path, kind string }{
		{"collections?arrayMode=slurm-array&limit=1&createdBefore=2026-10-03T12:00:00Z", "CollectionList"},
		{"graphs", "GraphList"},
		{"collections/" + testJobID + "/summary", "CollectionSummary"},
		{"graphs/" + testJobID + "/summary", "GraphSummary"},
		{"collections/" + testJobID + "/items?afterIndex=98&limit=1", "CollectionItemList"},
		{"graphs/" + testJobID + "/nodes", "GraphNodeList"},
		{"graphs/" + testJobID + "/dependencies?nodeId=" + testJobID + "&direction=incoming", "GraphDependencyList"},
		{"graphs/" + testJobID + "/neighborhood?nodeId=" + testJobID, "GraphNeighborhood"},
	}
	for _, test := range paths {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/namespaces/research/"+test.path, nil))
		if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d %s", test.path, response.Code, response.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["kind"] != test.kind {
			t.Fatalf("kind=%v", body)
		}
		if test.kind == "CollectionList" {
			if repository.options.ArrayMode != "slurm-array" || repository.options.Limit != 1 || repository.options.CreatedBefore == nil || body["total"] != "3" {
				t.Fatalf("catalog args/body=%#v,%v", repository.options, body)
			}
			item := requireGroupValue[map[string]any](t, requireGroupValue[[]any](t, body["items"])[0])
			if _, present := item["items"]; present {
				t.Fatal("summary includes child items")
			}
			if _, err := decodeJobPageToken(requireGroupValue[string](t, body["nextPageToken"])); err != nil {
				t.Fatal(err)
			}
		}
		if test.kind == "CollectionSummary" || test.kind == "GraphSummary" {
			if _, present := requireGroupValue[map[string]any](t, body["summary"])["items"]; present {
				t.Fatal("summary includes children")
			}
		}
		if test.kind == "CollectionItemList" {
			item := requireGroupValue[map[string]any](t, requireGroupValue[[]any](t, body["items"])[0])
			if item["arrayTaskIndex"] != float64(99) || body["nextAfterIndex"] != float64(99) {
				t.Fatalf("array index=%v", body)
			}
		}
		if test.kind == "GraphDependencyList" {
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/dependencies?pageToken="+requireGroupValue[string](t, body["nextPageToken"]), nil)
			options, err := readGraphEdgeOptions(request)
			if err != nil || options.AfterFromID != testJobID {
				t.Fatalf("edge cursor=%#v,%v", options, err)
			}
		}
	}
}

func TestInvalidGroupQueries(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t, &groupRepository{}, 1024)
	for _, path := range []string{
		"collections?limit=201", "collections?limit=%zz", "graphs/" + testJobID + "/summary?ignored=%zz", "graphs/" + testJobID + "/nodes?afterIndex=%zz", "graphs/" + testJobID + "/dependencies?nodeId=%zz", "graphs/" + testJobID + "/neighborhood?nodeId=" + testJobID + "&maxNodes=%zz", "collections?limit=1&limit=2", "collections?unknown=x", "collections?arrayMode=unknown", "collections?createdBefore=no", "collections?pageToken=invalid", "collections?limit=", "graphs?arrayMode=individual",
		"collections/bad/summary", "graphs/" + testJobID + "/summary?limit=1",
		"collections/" + testJobID + "/items?afterIndex=10000", "graphs/" + testJobID + "/nodes?limit=0",
		"graphs/" + testJobID + "/dependencies?direction=incoming", "graphs/" + testJobID + "/dependencies?nodeId=bad", "graphs/" + testJobID + "/dependencies?limit=501", "graphs/" + testJobID + "/dependencies?pageToken=" + strings.Repeat("a", 257),
		"graphs/" + testJobID + "/neighborhood?nodeId=" + testJobID + "&maxNodes=201", "graphs/" + testJobID + "/neighborhood?nodeId=" + testJobID + "&maxEdges=501", "graphs/" + testJobID + "/neighborhood",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/namespaces/research/"+path, nil))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid %s: %d", path, response.Code)
		}
	}
}

func requireGroupValue[T any](t *testing.T, value any) T {
	t.Helper()
	result, ok := value.(T)
	if !ok {
		t.Fatalf("unexpected group response type: %T", value)
	}
	return result
}
