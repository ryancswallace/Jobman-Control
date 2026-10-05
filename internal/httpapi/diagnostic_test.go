package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ryancswallace/jobman/diagnostic"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

type diagnosticRepository struct {
	domain.ControlRepository
	selection diagnostic.SharedSelection
}

func (repository *diagnosticRepository) ReadDiagnosticSnapshot(_ context.Context, _ domain.Principal, _ string, selection diagnostic.SharedSelection) (domain.DiagnosticSnapshot, error) {
	repository.selection = selection
	return domain.DiagnosticSnapshot{Snapshot: diagnostic.SharedSnapshot{Job: diagnostic.SharedJob{ID: selection.JobID, Revision: 9007199254740993}}}, nil
}

func TestDiagnosticSnapshotHTTP(t *testing.T) {
	t.Parallel()
	repository := &diagnosticRepository{}
	handler := newTestHandler(t, repository, 1024)
	query := "deploymentId=" + testJobID + "&controlInstanceId=" + testJobID + "&namespaceId=" + testJobID
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/namespaces/research/jobs/"+testJobID+"/diagnostic-snapshot?"+query+"&expectedJobRevision=9007199254740993&runId="+testJobID, nil))
	if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" || repository.selection.ExpectedJobRevision != 9007199254740993 || repository.selection.RunID != testJobID {
		t.Fatalf("snapshot=%d,%s", response.Code, response.Body.String())
	}
	var body struct {
		Kind     string `json:"kind"`
		Snapshot struct {
			Job struct {
				Revision string `json:"revision"`
			} `json:"job"`
		} `json:"snapshot"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Kind != "DiagnosticSnapshot" || body.Snapshot.Job.Revision != "9007199254740993" {
		t.Fatalf("snapshot precision=%#v,%v", body, err)
	}
	for _, invalid := range []string{"", "deploymentId=" + testJobID, query + "&namespaceId=" + testJobID, query + "&runId=", query + "&runId=bad", query + "&unknown=true", query + "&expectedJobRevision=0", query + "&expectedJobRevision=01", query + "&expectedJobRevision=9223372036854775808", query + "&expectedJobRevision=-1", query + "&expectedJobRevision=%zz"} {
		if _, err := parseDiagnosticSelection(invalid, testJobID); err == nil {
			t.Errorf("accepted diagnostic query %s", invalid)
		}
	}
	if delegationRouteOperation("GET /v1/namespaces/{namespace}/jobs/{jobID}/diagnostic-snapshot") != domain.CapabilityEvidenceRead {
		t.Fatal("diagnostic route has wrong delegation operation")
	}
}
