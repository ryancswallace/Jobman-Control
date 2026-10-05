package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

type runsRepository struct {
	domain.ControlRepository
	query domain.RunListOptions
	calls int
}

func (r *runsRepository) ListRuns(_ context.Context, _ domain.Principal, _, _ string, q domain.RunListOptions) (domain.RunPage, error) {
	r.calls++
	r.query = q
	return domain.RunPage{Items: []domain.JobRun{{ID: testJobID, Number: 9007199254740993, Phase: "future", DesiredState: "run", CreatedAt: time.Now(), UpdatedAt: time.Now()}}, Total: 1}, nil
}

func (r *runsRepository) GetRun(_ context.Context, _ domain.Principal, _, _, id string) (domain.RunDetail, error) {
	r.calls++
	return domain.RunDetail{Run: domain.JobRun{ID: id, Number: 2}}, nil
}

func TestRunHTTP(t *testing.T) {
	t.Parallel()
	repo := &runsRepository{}
	handler := newTestHandler(t, repo, 1024)
	for _, path := range []string{"runs?limit=1&pageToken=opaque", "runs/" + testJobID} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/namespaces/research/jobs/"+testJobID+"/"+path, nil))
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("status=%d", w.Code)
		}
		if strings.Contains(path, "?") && (!strings.Contains(w.Body.String(), `"number":"9007199254740993"`) || repo.query.Limit != 1 || repo.query.PageToken != "opaque") {
			t.Fatal("run query or decimal contract lost")
		}
	}
	before := repo.calls
	for _, path := range []string{"runs?limit=0", "runs?limit=101", "runs?limit=01", "runs?limit=1&limit=2", "runs?offset=1", "runs?limit=%zz", "runs?pageToken=", "runs?pageToken=" + strings.Repeat("a", 1025), "runs/invalid", "runs/" + testJobID + "?anything=1"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/namespaces/research/jobs/"+testJobID+"/"+path, nil))
		if w.Code != 400 {
			t.Fatalf("query %q status=%d", path, w.Code)
		}
	}
	if repo.calls != before {
		t.Fatal("invalid query reached repository")
	}
	for _, route := range []string{"GET /v1/namespaces/{namespace}/jobs/{jobID}/runs", "GET /v1/namespaces/{namespace}/jobs/{jobID}/runs/{runID}"} {
		if delegationRouteOperation(route) != domain.CapabilityJobsRead {
			t.Fatal("run read is not mapped to jobs.read")
		}
	}
	if delegationRouteOperation("POST /v1/namespaces/{namespace}/jobs/{jobID}/runs") != "" {
		t.Fatal("mutation was delegated")
	}
}
