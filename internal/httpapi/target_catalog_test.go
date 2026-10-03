package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

type targetCatalogRepository struct{ domain.ControlRepository }

func (*targetCatalogRepository) ListTargetCatalog(context.Context, domain.Principal, string, domain.TargetCatalogOptions) (domain.TargetCatalog, error) {
	when := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	return domain.TargetCatalog{NamespaceReadAuthority: domain.NamespaceReadAuthority{Namespace: "research", AsOf: when}, CreatedBefore: when, Total: 9007199254740993, Items: []domain.CatalogTarget{{ID: testJobID, Revision: 9007199254740993, Generation: domain.CatalogTargetGeneration{ID: testJobID, Number: 9007199254740993, LogStore: &domain.CatalogStoreReference{Name: "store", Version: 9007199254740993}}}}, NextCursor: &domain.JobCursor{CreatedAt: when, ID: testJobID}}, nil
}

func (*targetCatalogRepository) GetTargetSnapshot(context.Context, domain.Principal, string, string) (domain.TargetSnapshot, error) {
	return domain.TargetSnapshot{Target: domain.CatalogTarget{ID: testJobID}}, nil
}

func (*targetCatalogRepository) ListTargetPartitions(_ context.Context, _ domain.Principal, _, id string, options domain.TargetPartitionOptions) (domain.TargetPartitionPage, error) {
	return domain.TargetPartitionPage{TargetID: id, GenerationID: options.GenerationID, Total: 201, Items: []domain.PartitionSpec{{Name: "cpu"}}, NextName: "cpu"}, nil
}

func TestTargetCatalogHTTP(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t, &targetCatalogRepository{}, 1024)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/namespaces/research/target-catalog?limit=200", nil))
	if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("catalog=%d,%s", response.Code, response.Body.String())
	}
	var body struct {
		Total string `json:"total"`
		Next  string `json:"nextPageToken"`
		Items []struct {
			Revision   string `json:"revision"`
			Generation struct {
				Number   string `json:"number"`
				LogStore struct {
					Version string `json:"version"`
				} `json:"logStore"`
			} `json:"generation"`
		} `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Total != "9007199254740993" || len(body.Items) != 1 || body.Items[0].Revision != body.Total || body.Items[0].Generation.Number != body.Total || body.Items[0].Generation.LogStore.Version != body.Total {
		t.Fatal("target decimal precision lost")
	}
	options, err := parseTargetCatalogOptions("pageToken=" + body.Next)
	if err != nil || options.Before == nil || options.CreatedBefore == nil || !options.Before.CreatedAt.Equal(*options.CreatedBefore) {
		t.Fatalf("target cursor=%#v,%v", options, err)
	}
	for _, query := range []string{"limit=201", "limit=0", "limit=1&limit=2", "createdBefore=invalid", "createdBefore=%zz", "pageToken=" + strings.Repeat("x", 1025), "pageToken=" + body.Next + "&createdBefore=2026-10-04T00:00:00Z", "target=unexpected", "limit=1;pageToken=foo", "pageToken=" + base64.RawURLEncoding.EncodeToString([]byte(`{"createdBefore":"2026-10-03T12:00:00Z","createdAt":"2026-10-03T12:00:00Z","id":"`+testJobID+`","unknown":true}`))} {
		if _, err := parseTargetCatalogOptions(query); err == nil {
			t.Errorf("accepted malformed target query=%s", query)
		}
	}
	for _, suffix := range []string{"/" + testJobID, "/" + testJobID + "?unknown=1", "/" + testJobID + "?bad=%zz"} {
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/namespaces/research/target-catalog"+suffix, nil))
		expected := 200
		if strings.Contains(suffix, "?") {
			expected = 400
		}
		if response.Code != expected {
			t.Fatalf("target detail status=%d,want%d", response.Code, expected)
		}
	}
	for _, route := range []string{"GET /v1/namespaces/{namespace}/target-catalog/{targetID}/partitions", "GET /v1/namespaces/{namespace}/target-catalog", "GET /v1/namespaces/{namespace}/target-catalog/{targetID}"} {
		if delegationRouteOperation(route) != domain.CapabilityTargetsRead {
			t.Fatalf("target route lacks exact operation: %s", route)
		}
	}
}

func TestTargetPartitionsHTTP(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t, &targetCatalogRepository{}, 1024)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/namespaces/research/target-catalog/"+testJobID+"/partitions?generationId="+testJobID+"&limit=200", nil))
	if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("partitions=%d,%s", response.Code, response.Body.String())
	}
	var body struct {
		Kind         string `json:"kind"`
		Total        string `json:"total"`
		Next         string `json:"nextPageToken"`
		GenerationID string `json:"generationId"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Kind != "TargetPartitionList" || body.Total != "201" || body.GenerationID != testJobID {
		t.Fatal("partition authority or count lost")
	}
	options, err := parseTargetPartitionOptions("pageToken=" + body.Next)
	if err != nil || options.GenerationID != testJobID || options.AfterName != "cpu" {
		t.Fatalf("partition cursor=%#v,%v", options, err)
	}
	for _, query := range []string{"", "generationId=bad", "generationId=" + testJobID + "&limit=201", "generationId=" + testJobID + "&generationId=" + testJobID, "generationId=" + testJobID + "&unknown=x", "generationId=%zz", "pageToken=" + strings.Repeat("x", 1025), "pageToken=" + body.Next + "&generationId=22222222-2222-4222-8222-222222222222", "pageToken=" + base64.RawURLEncoding.EncodeToString([]byte(`{"generationId":"`+testJobID+`","afterName":"../secret"}`)), "pageToken=" + base64.RawURLEncoding.EncodeToString([]byte(`{"generationId":"`+testJobID+`","afterName":"cpu","unknown":true}`))} {
		if _, err := parseTargetPartitionOptions(query); err == nil {
			t.Errorf("accepted malformed partition query=%s", query)
		}
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/namespaces/research/target-catalog/not-a-uuid/partitions?generationId="+testJobID, nil))
	if response.Code != 400 {
		t.Fatalf("invalid target UUID=%d", response.Code)
	}
}
