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

type manifestRepository struct {
	domain.ControlRepository
	options domain.LogChunkOptions
}

func (repository *manifestRepository) ListLogChunks(_ context.Context, _ domain.Principal, namespace, jobID string, options domain.LogChunkOptions) (domain.LogChunkPage, error) {
	repository.options = options
	next := int64(9007199254740993)
	return domain.LogChunkPage{ManifestAuthority: domain.ManifestAuthority{Namespace: namespace, JobID: jobID, AsOf: time.Now(), RecoveryEpoch: 1, AuthorizationVersion: 2}, State: "complete", ManifestRevision: 3, RunNumber: 7, Chunks: []domain.BoundedLogChunk{{Sequence: next, ByteLength: 0, Complete: true}}, NextAfterSequence: &next}, nil
}

func (*manifestRepository) ListArtifacts(context.Context, domain.Principal, string, string, domain.ArtifactListOptions) (domain.ArtifactPage, error) {
	return domain.ArtifactPage{Total: 9007199254740993, Items: []domain.BoundedArtifact{}}, nil
}

func TestBoundedManifestHTTP(t *testing.T) {
	t.Parallel()
	repository := &manifestRepository{}
	handler := newTestHandler(t, repository, 1024)
	for _, path := range []string{"log-chunks?stream=stderr&runNumber=7&fromOffset=9007199254740993&limit=1", "artifact-metadata?limit=1"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/namespaces/research/jobs/"+testJobID+"/"+path, nil))
		if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("manifest response=%d,%s", response.Code, response.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(path, "log-chunks") {
			if body["manifestRevision"] != "3" || body["runNumber"] != "7" || body["nextAfterSequence"] != "9007199254740993" || repository.options.FromOffset == nil || *repository.options.FromOffset != 9007199254740993 {
				t.Fatalf("decimal contract=%v,%#v", body, repository.options)
			}
		} else if body["total"] != "9007199254740993" {
			t.Fatalf("artifact count lost precision=%v", body)
		}
	}
	for _, query := range []string{"stream=bad", "limit=101", "tailBytes=0", "tailBytes=262145", "runNumber=0", "fromOffset=-1", "fromOffset=1&tailBytes=1", "afterSequence=1&fromOffset=0", "limit=1&limit=2", "fromOffset=9223372036854775808", "objectKey=arbitrary", "fromOffset=%zz", "fromOffset=1;limit=9"} {
		if _, err := parseLogChunkOptions(query); err == nil {
			t.Errorf("accepted unsafe log query %s", query)
		}
	}
	for _, query := range []string{"pageToken=invalid", "runNumber=0", "limit=101", "name=%zz", "limit=1;runNumber=1"} {
		if _, err := parseArtifactOptions(query); err == nil {
			t.Errorf("accepted unsafe artifact query %s", query)
		}
	}
	for _, route := range []string{"GET /v1/namespaces/{namespace}/jobs/{jobID}/log-chunks", "GET /v1/namespaces/{namespace}/jobs/{jobID}/artifact-metadata"} {
		if delegationRouteOperation(route) == "" {
			t.Fatalf("bounded route missing explicit delegation mapping: %s", route)
		}
	}
}
