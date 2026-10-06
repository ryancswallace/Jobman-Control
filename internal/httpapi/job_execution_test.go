package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func TestJobExecutionOnlyInDetail(t *testing.T) {
	t.Parallel()
	job := testJob()
	job.Execution = &domain.JobExecution{Command: domain.JobCommand{Executable: "synthetic-executable", Args: []string{"", "a b", "$(literal)"}}, WorkingDirectory: "/synthetic/work"}
	repository := &fakeRepository{getResult: job, listResult: domain.JobPage{Jobs: []domain.Job{job}}}
	handler := newTestHandler(t, repository, 2*1024*1024)
	for _, path := range []string{"/v1/namespaces/research/jobs/" + testJobID, "/v1/namespaces/research/jobs"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, http.NoBody))
		if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("unexpected status/cache policy: %d", response.Code)
		}
		detail := strings.HasSuffix(path, testJobID)
		if strings.Contains(response.Body.String(), "synthetic-executable") != detail || strings.Contains(response.Body.String(), "/synthetic/work") != detail {
			t.Fatal("execution content crossed detail boundary")
		}
		if detail {
			var decoded jobResponse
			if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Spec.Execution == nil || len(decoded.Spec.Execution.Command.Args) != 3 || decoded.Spec.Execution.Command.Args[0] != "" {
				t.Fatal("detail lost argument boundaries")
			}
		}
	}
	job.Execution = nil
	job.ExecutionUnavailableReason = "too_large"
	if detail := newJobDetailResponse(job); detail.Spec.Execution != nil || detail.Spec.ExecutionUnavailableReason != "too_large" || detail.Status.Phase != job.Phase {
		t.Fatal("unavailable metadata lost status")
	}
	if ordinary := newJobResponse(job); ordinary.Spec.Execution != nil || ordinary.Spec.ExecutionUnavailableReason != "" {
		t.Fatal("ordinary projection contains detail metadata")
	}
}
