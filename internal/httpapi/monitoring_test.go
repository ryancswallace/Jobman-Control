package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

type monitoringRepository struct {
	domain.ControlRepository
	summary      domain.NamespaceSummary
	from, before *time.Time
	principal    domain.Principal
}

func (repository *monitoringRepository) Capabilities(context.Context) (domain.ControlCapabilities, error) {
	return domain.ControlCapabilities{InstanceID: testJobID, RecoveryEpoch: "9007199254740993", ServerTime: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC), Features: []string{"job-monitoring"}}, nil
}

func (repository *monitoringRepository) NamespaceSummary(_ context.Context, principal domain.Principal, _ string, from, before *time.Time) (domain.NamespaceSummary, error) {
	repository.from, repository.before, repository.principal = from, before, principal
	return repository.summary, nil
}

func TestMonitoringHTTPContracts(t *testing.T) {
	t.Parallel()
	repository := &monitoringRepository{summary: domain.NamespaceSummary{Namespace: "research", Total: "9007199254740993", ByPhase: map[string]string{"running": "1"}}}
	handler := newTestHandler(t, repository, 1024)
	for _, path := range []string{"/v1/capabilities", "/v1/namespaces/research/summary?completedFrom=2026-10-02T12:00:00Z&completedBefore=2026-10-03T12:00:00Z"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("response=%d %s", response.Code, response.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["apiVersion"] != apiVersion {
			t.Fatalf("version=%v", body)
		}
	}
	if repository.from == nil || repository.before == nil || repository.principal.Subject != "test-subject" {
		t.Fatalf("summary arguments=%#v", repository)
	}
	for _, query := range []string{"completedFrom=2026-10-03T00:00:00Z", "completedFrom=x&completedBefore=y", "completedFrom=2026-10-03T00:00:00Z&completedBefore=2026-10-02T00:00:00Z", "other=x", "completedFrom=%zz"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/namespaces/research/summary?"+query, nil))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid summary query=%s: %d", query, response.Code)
		}
	}
}

func TestMonitoringFilters(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/namespaces/research/jobs?phase=active&outcome=future&confidence=attention&ownerPrincipalId="+testJobID+"&jobId="+testJobID+"&completedFrom=2026-10-01T00:00:00Z&completedBefore=2026-10-03T00:00:00Z&createdBefore=2026-10-03T12:00:00Z", nil)
	options, err := readJobListOptions(request)
	if err != nil || options.Phase != "active" || options.Outcome != "future" || options.Confidence != "attention" || options.OwnerPrincipalID != testJobID || options.JobID != testJobID || options.CompletedFrom == nil || options.CompletedBefore == nil || options.CreatedBefore == nil {
		t.Fatalf("filter options=%#v,%v", options, err)
	}
	for _, query := range []string{"jobId=bad", "ownerPrincipalId=%zz", "phase=active;outcome=success", "ownerPrincipalId=bad", "confidence=unknown", "createdBefore=bad", "completedFrom=2026-10-03T00:00:00Z&completedBefore=2026-10-02T00:00:00Z", "outcome=success&outcome=failure"} {
		if _, err = readJobListOptions(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/jobs?"+query, nil)); err == nil {
			t.Fatalf("invalid query accepted: %s", query)
		}
	}
}
