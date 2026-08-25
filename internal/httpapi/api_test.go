package httpapi

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jobmanprotocol "github.com/ryancswallace/jobman-control/contracts/jobman/v1alpha1"
	"github.com/ryancswallace/jobman-control/internal/auth"
	"github.com/ryancswallace/jobman-control/internal/domain"
)

const testJobID = "11111111-1111-4111-8111-111111111111"

type fakeRepository struct {
	domain.ControlRepository
	readyErr         error
	submitResult     domain.SubmitResult
	submitErr        error
	collectionResult domain.CollectionResult
	collection       domain.Collection
	collectionSubmit domain.CollectionSubmission
	graphResult      domain.GraphResult
	graph            domain.Graph
	graphSubmit      domain.GraphSubmission
	graphKey         string
	historyResult    domain.HistoryImportResult
	historyImport    domain.CompletedHistoryImport
	historyDryRun    bool
	policy           domain.NamespacePolicy
	policyChange     domain.NamespacePolicyChange
	auditPage        domain.AuditPage
	auditAfterID     int64
	auditLimit       int
	snapshot         domain.OperationalSnapshot
	getResult        domain.Job
	getErr           error
	listResult       domain.JobPage
	listErr          error
	listOptions      domain.JobListOptions
	submission       domain.JobSubmission
	key              string
	targetResult     domain.CreateResult[domain.Target]
	targetSpec       domain.TargetSpec
	targetChange     domain.TargetGenerationChange
	targetErr        error
	agentIdentity    domain.AgentIdentity
	assignments      []domain.Assignment
	committedLog     domain.LogChunk
	commitReplay     bool
	commitErr        error
	logStreams       []domain.LogStream
	logsErr          error
	artifacts        []domain.PublishedArtifact
	artifactsErr     error
}

func (repository *fakeRepository) Ready(context.Context) error {
	return repository.readyErr
}

func (repository *fakeRepository) SubmitJob(
	_ context.Context,
	_ domain.Principal,
	key string,
	submission domain.JobSubmission,
) (domain.SubmitResult, error) {
	repository.key = key
	repository.submission = submission

	return repository.submitResult, repository.submitErr
}

func (repository *fakeRepository) GetJob(
	context.Context,
	domain.Principal,
	string,
	string,
) (domain.Job, error) {
	return repository.getResult, repository.getErr
}

func (repository *fakeRepository) SubmitCollection(
	_ context.Context,
	_ domain.Principal,
	key string,
	submission domain.CollectionSubmission,
) (domain.CollectionResult, error) {
	repository.key = key
	repository.collectionSubmit = submission

	return repository.collectionResult, repository.submitErr
}

func (repository *fakeRepository) GetCollection(
	context.Context,
	domain.Principal,
	string,
	string,
) (domain.Collection, error) {
	return repository.collection, repository.getErr
}

func (repository *fakeRepository) SubmitGraph(
	_ context.Context,
	_ domain.Principal,
	key string,
	submission domain.GraphSubmission,
) (domain.GraphResult, error) {
	repository.graphKey = key
	repository.graphSubmit = submission

	return repository.graphResult, repository.submitErr
}

func (repository *fakeRepository) GetGraph(
	context.Context,
	domain.Principal,
	string,
	string,
) (domain.Graph, error) {
	return repository.graph, repository.getErr
}

func (repository *fakeRepository) CancelGraph(
	_ context.Context,
	_ domain.Principal,
	_ string,
	_ string,
	key string,
	_ string,
) (domain.Graph, error) {
	repository.graphKey = key

	return repository.graph, repository.getErr
}

func (repository *fakeRepository) ImportCompletedHistory(
	_ context.Context,
	_ domain.Principal,
	_ string,
	dryRun bool,
	request domain.CompletedHistoryImport,
) (domain.HistoryImportResult, error) {
	repository.historyDryRun = dryRun
	repository.historyImport = request

	return repository.historyResult, repository.submitErr
}

func (repository *fakeRepository) GetNamespacePolicy(
	context.Context,
	domain.Principal,
	string,
) (domain.NamespacePolicy, error) {
	return repository.policy, repository.getErr
}

func (repository *fakeRepository) UpdateNamespacePolicy(
	_ context.Context,
	_ domain.Principal,
	_ string,
	change domain.NamespacePolicyChange,
) (domain.NamespacePolicy, error) {
	repository.policyChange = change

	return repository.policy, repository.getErr
}

func (repository *fakeRepository) ExportAudit(
	_ context.Context,
	_ domain.Principal,
	_ string,
	afterID int64,
	limit int,
) (domain.AuditPage, error) {
	repository.auditAfterID = afterID
	repository.auditLimit = limit

	return repository.auditPage, repository.getErr
}

func (repository *fakeRepository) OperationalSnapshot(context.Context) (domain.OperationalSnapshot, error) {
	return repository.snapshot, repository.getErr
}

func (repository *fakeRepository) ListJobs(
	_ context.Context,
	_ domain.Principal,
	_ string,
	options domain.JobListOptions,
) (domain.JobPage, error) {
	repository.listOptions = options

	return repository.listResult, repository.listErr
}

func (repository *fakeRepository) CreateTarget(
	_ context.Context,
	_ domain.Principal,
	_ string,
	key string,
	_ string,
	spec domain.TargetSpec,
) (domain.CreateResult[domain.Target], error) {
	repository.key = key
	repository.targetSpec = spec
	return repository.targetResult, repository.targetErr
}

func (repository *fakeRepository) CreateTargetGeneration(
	_ context.Context,
	_ domain.Principal,
	_ string,
	_ string,
	key string,
	_ string,
	change domain.TargetGenerationChange,
) (domain.CreateResult[domain.Target], error) {
	repository.key = key
	repository.targetSpec = change.Spec
	repository.targetChange = change

	return repository.targetResult, repository.targetErr
}

func (repository *fakeRepository) AuthenticateAgent(
	context.Context,
	string,
) (domain.AgentIdentity, error) {
	return repository.agentIdentity, nil
}

func (repository *fakeRepository) AuthenticateAgentCertificate(
	context.Context,
	string,
	string,
	string,
) (domain.AgentIdentity, error) {
	return repository.agentIdentity, nil
}

func (repository *fakeRepository) ListAssignments(
	context.Context,
	domain.AgentIdentity,
	int,
) ([]domain.Assignment, error) {
	return repository.assignments, nil
}

func (repository *fakeRepository) CommitLogChunk(
	_ context.Context,
	_ domain.AgentIdentity,
	chunk domain.LogChunk,
) (bool, error) {
	repository.committedLog = chunk
	return repository.commitReplay, repository.commitErr
}

func (repository *fakeRepository) GetJobLogs(
	context.Context,
	domain.Principal,
	string,
	string,
) ([]domain.LogStream, error) {
	return repository.logStreams, repository.logsErr
}

func (repository *fakeRepository) GetJobArtifacts(
	context.Context,
	domain.Principal,
	string,
	string,
) ([]domain.PublishedArtifact, error) {
	return repository.artifacts, repository.artifactsErr
}

func TestHealthAndReadiness(t *testing.T) {
	t.Parallel()
	repository := &fakeRepository{readyErr: errors.New("database unavailable")}
	handler := newTestHandler(t, repository, 2*1024*1024)

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", http.NoBody))
	if health.Code != http.StatusOK || !strings.Contains(health.Body.String(), `"status":"ok"`) {
		t.Fatalf("health response = %d %s", health.Code, health.Body.String())
	}

	ready := httptest.NewRecorder()
	handler.ServeHTTP(ready, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", http.NoBody))
	if ready.Code != http.StatusServiceUnavailable || strings.Contains(ready.Body.String(), "database") {
		t.Fatalf("readiness response = %d %s", ready.Code, ready.Body.String())
	}
}

func TestSubmitJob(t *testing.T) {
	t.Parallel()
	job := testJob()
	repository := &fakeRepository{submitResult: domain.SubmitResult{Job: job}}
	handler := newTestHandler(t, repository, 2*1024*1024)
	request := validSubmitRequest(t, "/v1/namespaces/research/jobs")
	request.Header.Set("Idempotency-Key", "submit-001")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("submit response = %d %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Location") != "/v1/namespaces/research/jobs/"+testJobID {
		t.Fatalf("Location = %q", response.Header().Get("Location"))
	}
	if response.Header().Get("ETag") != `"revision-1"` {
		t.Fatalf("ETag = %q", response.Header().Get("ETag"))
	}
	if repository.key != "submit-001" || repository.submission.Namespace != "research" ||
		!repository.submission.ExecutionFeatures.DirectCommand {
		t.Fatalf("repository received key %q and submission %#v", repository.key, repository.submission)
	}
	var body jobResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Metadata.ID != testJobID || body.Status.Phase != domain.JobPhaseAccepted {
		t.Fatalf("job response = %#v", body)
	}
}

func TestSubmitJobIdempotentReplay(t *testing.T) {
	t.Parallel()
	repository := &fakeRepository{
		submitResult: domain.SubmitResult{Job: testJob(), Replayed: true},
	}
	handler := newTestHandler(t, repository, 2*1024*1024)
	request := validSubmitRequest(t, "/v1/namespaces/research/jobs")
	request.Header.Set("Idempotency-Key", "submit-001")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay response = %d headers=%v", response.Code, response.Header())
	}
}

func TestSubmitAndGetCollection(t *testing.T) {
	t.Parallel()
	collection := testCollection()
	repository := &fakeRepository{
		collectionResult: domain.CollectionResult{Collection: collection},
		collection:       collection,
	}
	handler := newTestHandler(t, repository, 2*1024*1024)
	request := validCollectionRequest(t, "/v1/namespaces/research/collections")
	request.Header.Set("Idempotency-Key", "collection-001")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("submit response = %d %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Location") != "/v1/namespaces/research/collections/"+collection.ID ||
		response.Header().Get("ETag") != `"revision-1"` {
		t.Fatalf("submit headers = %v", response.Header())
	}
	if repository.key != "collection-001" || repository.collectionSubmit.Name != "sweep" ||
		repository.collectionSubmit.MaxActive != 1 || len(repository.collectionSubmit.Items) != 2 ||
		repository.collectionSubmit.Items[0].Name != "trial-a" {
		t.Fatalf("collection submission = %#v", repository.collectionSubmit)
	}
	var submitted collectionResponse
	if err := json.Unmarshal(response.Body.Bytes(), &submitted); err != nil {
		t.Fatalf("decode submit response: %v", err)
	}
	if submitted.Kind != "Collection" || submitted.Status.Total != 2 || len(submitted.Items) != 2 {
		t.Fatalf("collection response = %#v", submitted)
	}

	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, httptest.NewRequestWithContext(
		t.Context(), http.MethodGet,
		"/v1/namespaces/research/collections/"+collection.ID, http.NoBody,
	))
	if getResponse.Code != http.StatusOK || !strings.Contains(getResponse.Body.String(), `"arrayMode":"individual"`) {
		t.Fatalf("get response = %d %s", getResponse.Code, getResponse.Body.String())
	}
}

func TestSubmitCollectionRejectsNamespaceMismatch(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t, &fakeRepository{}, 2*1024*1024)
	request := validCollectionRequest(t, "/v1/namespaces/other/collections")
	request.Header.Set("Idempotency-Key", "collection-001")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "namespace_mismatch") {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestSubmitGetAndCancelGraph(t *testing.T) {
	t.Parallel()
	graph := testGraph()
	repository := &fakeRepository{
		graphResult: domain.GraphResult{Graph: graph},
		graph:       graph,
	}
	handler := newTestHandler(t, repository, 2*1024*1024)
	request := validGraphRequest(t, "/v1/namespaces/research/graphs")
	request.Header.Set("Idempotency-Key", "graph-001")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated ||
		response.Header().Get("Location") != "/v1/namespaces/research/graphs/"+graph.ID {
		t.Fatalf("submit graph response = %d %s headers=%v", response.Code, response.Body.String(), response.Header())
	}
	if repository.graphKey != "graph-001" || repository.graphSubmit.MaxActive != 1 ||
		len(repository.graphSubmit.Nodes) != 2 || len(repository.graphSubmit.Edges) != 1 {
		t.Fatalf("graph submission = %#v", repository.graphSubmit)
	}

	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "/v1/namespaces/research/graphs/"+graph.ID, http.NoBody,
	))
	if getResponse.Code != http.StatusOK || !strings.Contains(getResponse.Body.String(), `"predicate":"success"`) {
		t.Fatalf("get graph response = %d %s", getResponse.Code, getResponse.Body.String())
	}

	cancelRequest := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, "/v1/namespaces/research/graphs/"+graph.ID+"/cancel", http.NoBody,
	)
	cancelRequest.Header.Set("Idempotency-Key", "graph-cancel-001")
	cancelResponse := httptest.NewRecorder()
	handler.ServeHTTP(cancelResponse, cancelRequest)
	if cancelResponse.Code != http.StatusOK || repository.graphKey != "graph-cancel-001" {
		t.Fatalf("cancel graph response = %d %s", cancelResponse.Code, cancelResponse.Body.String())
	}
}

func TestProductionControlEndpoints(t *testing.T) {
	t.Parallel()
	timestamp := time.Date(2026, time.August, 23, 12, 0, 0, 0, time.UTC)
	policy := domain.NamespacePolicy{
		Namespace: "research", MaxActiveJobs: 10, MaxQueuedJobs: 100,
		MaxCollectionItems: 50, MaxGraphNodes: 40,
		IdempotencyRetention: 24 * time.Hour, PublishedOutboxRetention: 48 * time.Hour,
		Revision: 2, CreatedAt: timestamp, UpdatedAt: timestamp,
	}
	repository := &fakeRepository{
		policy: policy,
		auditPage: domain.AuditPage{
			Items: []domain.AuditEvent{{
				ID: 8, Namespace: "research", ActorKind: "system", Action: "graph.node.unsatisfied",
				ResourceType: "job", ResourceID: testJobID, Details: json.RawMessage(`{"disposition":"skipped"}`),
				OccurredAt: timestamp,
			}},
			NextAfterID: 8,
		},
		snapshot: domain.OperationalSnapshot{
			JobsByPhase: map[string]int64{"accepted": 3}, AgentsByStatus: map[string]int64{"active": 2},
			UnpublishedOutbox: 4, StaleExecutions: 1, OldestQueueAge: 5 * time.Second,
			RecoveryHold: true, RestoreEpoch: 7,
		},
		historyResult: domain.HistoryImportResult{DryRun: true},
	}
	handler := newTestHandler(t, repository, 2*1024*1024)

	metrics := httptest.NewRecorder()
	handler.ServeHTTP(metrics, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", http.NoBody))
	if metrics.Code != http.StatusOK || !strings.Contains(metrics.Body.String(), `jobman_control_jobs{phase="accepted"} 3`) ||
		!strings.Contains(metrics.Body.String(), "jobman_control_reconciliation_hold 1") {
		t.Fatalf("metrics response = %d %s", metrics.Code, metrics.Body.String())
	}

	policyGet := httptest.NewRecorder()
	handler.ServeHTTP(policyGet, httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "/v1/namespaces/research/policy", http.NoBody,
	))
	if policyGet.Code != http.StatusOK || policyGet.Header().Get("ETag") != `"revision-2"` {
		t.Fatalf("policy GET response = %d %s headers=%v", policyGet.Code, policyGet.Body.String(), policyGet.Header())
	}
	policyPut := httptest.NewRequestWithContext(
		t.Context(), http.MethodPut, "/v1/namespaces/research/policy", strings.NewReader(`{
			"apiVersion":"jobman.control/v1alpha1","kind":"NamespacePolicy","spec":{
				"maxActiveJobs":11,"maxQueuedJobs":101,"maxCollectionItems":51,"maxGraphNodes":41,
				"idempotencyRetention":"24h","publishedOutboxRetention":"48h"}}
		`),
	)
	policyPut.Header.Set("Content-Type", "application/json")
	policyPut.Header.Set("If-Match", `"revision-2"`)
	policyPutResponse := httptest.NewRecorder()
	handler.ServeHTTP(policyPutResponse, policyPut)
	if policyPutResponse.Code != http.StatusOK || repository.policyChange.ExpectedRevision != 2 ||
		repository.policyChange.MaxActiveJobs != 11 {
		t.Fatalf("policy PUT response = %d %s change=%#v", policyPutResponse.Code, policyPutResponse.Body.String(), repository.policyChange)
	}

	audit := httptest.NewRecorder()
	handler.ServeHTTP(audit, httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "/v1/namespaces/research/audit?afterId=7&limit=1", http.NoBody,
	))
	if audit.Code != http.StatusOK || repository.auditAfterID != 7 || repository.auditLimit != 1 ||
		!strings.Contains(audit.Body.String(), `"nextAfterId":8`) {
		t.Fatalf("audit response = %d %s", audit.Code, audit.Body.String())
	}

	historyRequest := validHistoryImportRequest(t, "/v1/namespaces/research/history/imports?dryRun=true")
	historyResponse := httptest.NewRecorder()
	handler.ServeHTTP(historyResponse, historyRequest)
	if historyResponse.Code != http.StatusOK || !repository.historyDryRun ||
		repository.historyImport.SourceStore != "sqlite" {
		t.Fatalf("history response = %d %s import=%#v", historyResponse.Code, historyResponse.Body.String(), repository.historyImport)
	}
}

func TestJobResponseIncludesSchedulerEvidence(t *testing.T) {
	t.Parallel()
	job := testJob()
	observedAt := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	job.NativeID = "12345"
	job.Scheduler = &domain.SchedulerStatus{
		Backend: "slurm", State: "queued", Reason: "Resources",
		Cluster: "alpha", ObservedAt: observedAt,
	}
	response := newJobResponse(job)
	if response.Status.NativeID != "12345" || response.Status.Scheduler == nil ||
		response.Status.Scheduler.State != "queued" ||
		response.Status.Scheduler.Reason != "Resources" ||
		!response.Status.Scheduler.ObservedAt.Equal(observedAt) {
		t.Fatalf("newJobResponse() = %#v", response)
	}
}

func TestSubmitJobRequestValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		path        string
		contentType string
		key         string
		body        string
		maximum     int64
		wantStatus  int
	}{
		{
			name: "content type", path: "/v1/namespaces/research/jobs",
			contentType: "text/plain", key: "submit-001", body: `{}`, maximum: 1024,
			wantStatus: http.StatusUnsupportedMediaType,
		},
		{
			name: "idempotency key", path: "/v1/namespaces/research/jobs",
			contentType: "application/json", body: `{}`, maximum: 1024,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "invalid document", path: "/v1/namespaces/research/jobs",
			contentType: "application/json", key: "submit-001", body: `{}`, maximum: 1024,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "too large", path: "/v1/namespaces/research/jobs",
			contentType: "application/json", key: "submit-001", body: strings.Repeat("x", 65), maximum: 64,
			wantStatus: http.StatusRequestEntityTooLarge,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			handler := newTestHandler(t, &fakeRepository{}, test.maximum)
			request := httptest.NewRequestWithContext(
				t.Context(), http.MethodPost, test.path, strings.NewReader(test.body),
			)
			request.Header.Set("Content-Type", test.contentType)
			if test.key != "" {
				request.Header.Set("Idempotency-Key", test.key)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("response = %d %s, want %d", response.Code, response.Body.String(), test.wantStatus)
			}
		})
	}
}

func TestSubmitJobRejectsNamespaceMismatch(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t, &fakeRepository{}, 2*1024*1024)
	request := validSubmitRequest(t, "/v1/namespaces/other/jobs")
	request.Header.Set("Idempotency-Key", "submit-001")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "namespace_mismatch") {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestGetJob(t *testing.T) {
	t.Parallel()
	repository := &fakeRepository{getResult: testJob()}
	handler := newTestHandler(t, repository, 2*1024*1024)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "/v1/namespaces/research/jobs/"+testJobID, http.NoBody,
	))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), testJobID) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestGetJobMapsNotFoundAndRejectsBadID(t *testing.T) {
	t.Parallel()
	repository := &fakeRepository{getErr: domain.ErrNotFound}
	handler := newTestHandler(t, repository, 2*1024*1024)
	for _, test := range []struct {
		id         string
		wantStatus int
	}{
		{id: testJobID, wantStatus: http.StatusNotFound},
		{id: "not-an-id", wantStatus: http.StatusBadRequest},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequestWithContext(
			t.Context(), http.MethodGet, "/v1/namespaces/research/jobs/"+test.id, http.NoBody,
		))
		if response.Code != test.wantStatus {
			t.Fatalf("GET %q response = %d %s", test.id, response.Code, response.Body.String())
		}
	}
}

func TestListJobs(t *testing.T) {
	t.Parallel()
	job := testJob()
	nextID := "22222222-2222-4222-8222-222222222222"
	repository := &fakeRepository{listResult: domain.JobPage{
		Jobs: []domain.Job{job},
		NextCursor: &domain.JobCursor{
			CreatedAt: job.CreatedAt, ID: nextID,
		},
	}}
	handler := newTestHandler(t, repository, 2*1024*1024)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(
		t.Context(), http.MethodGet,
		"/v1/namespaces/research/jobs?limit=7&phase=running", http.NoBody,
	))
	if response.Code != http.StatusOK {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if repository.listOptions.Limit != 7 || repository.listOptions.Phase != "running" {
		t.Fatalf("list options = %#v", repository.listOptions)
	}
	var body jobListResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Kind != "JobList" || len(body.Items) != 1 || body.NextPageToken == "" {
		t.Fatalf("job list response = %#v", body)
	}
	cursor, err := decodeJobPageToken(body.NextPageToken)
	if err != nil || cursor.ID != nextID || !cursor.CreatedAt.Equal(job.CreatedAt) {
		t.Fatalf("decoded page token = %#v, %v", cursor, err)
	}
}

func TestListJobsValidatesQueryAndMapsErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		query      string
		repository *fakeRepository
		wantStatus int
	}{
		{name: "invalid limit", query: "?limit=0", repository: &fakeRepository{}, wantStatus: http.StatusBadRequest},
		{name: "invalid phase", query: "?phase=unknown", repository: &fakeRepository{}, wantStatus: http.StatusBadRequest},
		{name: "invalid token", query: "?pageToken=invalid", repository: &fakeRepository{}, wantStatus: http.StatusBadRequest},
		{name: "unknown parameter", query: "?other=x", repository: &fakeRepository{}, wantStatus: http.StatusBadRequest},
		{name: "forbidden", repository: &fakeRepository{listErr: domain.ErrForbidden}, wantStatus: http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			response := httptest.NewRecorder()
			newTestHandler(t, test.repository, 2*1024*1024).ServeHTTP(
				response,
				httptest.NewRequestWithContext(
					t.Context(), http.MethodGet,
					"/v1/namespaces/research/jobs"+test.query, http.NoBody,
				),
			)
			if response.Code != test.wantStatus {
				t.Fatalf("response = %d %s, want %d", response.Code, response.Body.String(), test.wantStatus)
			}
		})
	}
}

func TestSubmitJobMapsRepositoryErrors(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		err        error
		wantStatus int
	}{
		{name: "forbidden", err: domain.ErrForbidden, wantStatus: http.StatusForbidden},
		{name: "conflict", err: domain.ErrIdempotencyConflict, wantStatus: http.StatusConflict},
		{name: "internal", err: errors.New("failed"), wantStatus: http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			handler := newTestHandler(t, &fakeRepository{submitErr: test.err}, 2*1024*1024)
			request := validSubmitRequest(t, "/v1/namespaces/research/jobs")
			request.Header.Set("Idempotency-Key", "submit-001")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestCreateTargetNormalizesCapabilities(t *testing.T) {
	t.Parallel()
	timestamp := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	repository := &fakeRepository{targetResult: domain.CreateResult[domain.Target]{Value: domain.Target{
		ID:           "44444444-4444-4444-8444-444444444444",
		GenerationID: "55555555-5555-4555-8555-555555555555",
		Generation:   1, Namespace: "research", Name: "cluster-a", Kind: "slurm",
		State: "active", ExecutionBackend: "slurm", Transport: "agent-api",
		Runtimes: []string{"container", "native"}, Revision: 1,
		CreatedAt: timestamp, UpdatedAt: timestamp,
	}}}
	handler := newTestHandler(t, repository, 2*1024*1024)
	request := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, "/v1/namespaces/research/targets",
		strings.NewReader(`{
			"apiVersion":"jobman.control/v1alpha1","kind":"Target",
			"metadata":{"name":"cluster-a"},
			"spec":{"kind":"slurm","executionBackend":"slurm",
			"runtimes":["native","container"],"architectures":["arm64","amd64"],
			"logStore":{"name":"department-nfs","version":1},
			"artifactStores":[{"name":"scratch-nfs","version":2},{"name":"department-nfs","version":1}],
			"partitions":[{"name":"gpu","isDefault":true}]}
		}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "target-001")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if repository.key != "target-001" ||
		strings.Join(repository.targetSpec.Runtimes, ",") != "container,native" ||
		strings.Join(repository.targetSpec.Architectures, ",") != "amd64,arm64" ||
		repository.targetSpec.LogStoreName != "department-nfs" || repository.targetSpec.LogStoreVersion != 1 ||
		len(repository.targetSpec.ArtifactStores) != 2 ||
		repository.targetSpec.ArtifactStores[0].Name != "department-nfs" {
		t.Fatalf("normalized target spec = %#v", repository.targetSpec)
	}
}

func TestCreateParallelClusterTargetGeneration(t *testing.T) {
	t.Parallel()
	timestamp := time.Date(2026, time.August, 23, 12, 0, 0, 0, time.UTC)
	repository := &fakeRepository{targetResult: domain.CreateResult[domain.Target]{Value: domain.Target{
		ID: "44444444-4444-4444-8444-444444444444", GenerationID: "66666666-6666-4666-8666-666666666666",
		Generation: 2, Namespace: "research", Name: "cluster-a", Kind: "slurm", State: "active",
		ExecutionBackend: "slurm", Transport: "agent-api", Runtimes: []string{"container", "native"},
		Provider: domain.TargetProvider{Kind: "aws-parallelcluster", Region: "us-east-1", ClusterName: "research-hpc"},
		Revision: 2, CreatedAt: timestamp, UpdatedAt: timestamp,
	}}}
	handler := newTestHandler(t, repository, 2*1024*1024)
	request := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, "/v1/namespaces/research/targets/cluster-a/generations",
		strings.NewReader(`{
			"apiVersion":"jobman.control/v1alpha1","kind":"TargetGeneration",
			"spec":{"kind":"slurm","executionBackend":"slurm","runtimes":["native","container"],
			"provider":{"kind":"aws-parallelcluster","region":"us-east-1","clusterName":"research-hpc"}}
		}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "generation-002")
	request.Header.Set("If-Match", `"revision-1"`)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if repository.targetChange.ExpectedRevision != 1 || repository.targetSpec.Name != "cluster-a" ||
		repository.targetSpec.Provider.Kind != "aws-parallelcluster" ||
		repository.targetSpec.Provider.Region != "us-east-1" ||
		repository.targetSpec.Provider.ClusterName != "research-hpc" {
		t.Fatalf("target generation change = %#v", repository.targetChange)
	}
	if !strings.Contains(response.Body.String(), `"kind":"aws-parallelcluster"`) ||
		response.Header().Get("ETag") != `"revision-2"` {
		t.Fatalf("generation response = %v %s", response.Header(), response.Body.String())
	}
}

func TestAgentAssignmentPollingRequiresAgentScheme(t *testing.T) {
	t.Parallel()
	agentID := "22222222-2222-4222-8222-222222222222"
	repository := &fakeRepository{
		agentIdentity: domain.AgentIdentity{AgentID: agentID},
		assignments:   []domain.Assignment{{Document: json.RawMessage(`{"kind":"AgentAssignment"}`)}},
	}
	handler := newTestHandler(t, repository, 2*1024*1024)
	for _, test := range []struct {
		certificate bool
		wantStatus  int
	}{
		{wantStatus: http.StatusUnauthorized},
		{certificate: true, wantStatus: http.StatusOK},
	} {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/agent/assignments", http.NoBody)
		if test.certificate {
			request.TLS = testAgentTLSState(t, agentID)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.wantStatus {
			t.Fatalf("certificate=%t response = %d %s", test.certificate, response.Code, response.Body.String())
		}
	}
}

func TestLogChunkCommitAndAuthorizedManifest(t *testing.T) {
	timestamp := time.Date(2026, time.August, 23, 12, 0, 0, 0, time.UTC)
	agentID := "22222222-2222-4222-8222-222222222222"
	executionID := "33333333-3333-4333-8333-333333333333"
	repository := &fakeRepository{
		agentIdentity: domain.AgentIdentity{AgentID: agentID},
		logStreams: []domain.LogStream{{
			ExecutionID: executionID, RunNumber: 1, Stream: "stdout",
			State: "complete", ByteLength: 6,
			Chunks: []domain.LogChunk{{
				ExecutionID: executionID, Stream: "stdout", Sequence: 1,
				StoreName: "department-nfs", StoreVersion: 1, ObjectKey: "safe/key",
				ByteLength: 6, Checksum: "sha256:" + strings.Repeat("a", 64),
				CapturedAt: timestamp,
			}},
		}},
	}
	handler := newTestHandler(t, repository, 2*1024*1024)
	body := fmt.Sprintf(`{
		"apiVersion":"jobman.control/v1alpha1","kind":"LogChunk",
		"metadata":{"executionId":%q,"stream":"stdout","sequence":1},
		"spec":{"storeName":"department-nfs","storeVersion":1,
		"objectKey":"safe/key","byteOffset":0,"byteLength":6,
		"checksum":%q,"capturedAt":%q,"complete":true,"truncated":false}
	}`, executionID, "sha256:"+strings.Repeat("a", 64), timestamp.Format(time.RFC3339))
	request := httptest.NewRequestWithContext(
		t.Context(), http.MethodPut,
		"/v1/agent/executions/"+executionID+"/logs/stdout/chunks/1", strings.NewReader(body),
	)
	request.Header.Set("Content-Type", "application/json")
	request.TLS = testAgentTLSState(t, agentID)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || repository.committedLog.ObjectKey != "safe/key" ||
		!repository.committedLog.Complete {
		t.Fatalf("commit response = %d %s chunk=%#v", response.Code, response.Body.String(), repository.committedLog)
	}
	manifestResponse := httptest.NewRecorder()
	handler.ServeHTTP(manifestResponse, httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "/v1/namespaces/research/jobs/"+testJobID+"/logs", http.NoBody,
	))
	if manifestResponse.Code != http.StatusOK ||
		!strings.Contains(manifestResponse.Body.String(), `"kind":"JobLogManifest"`) ||
		!strings.Contains(manifestResponse.Body.String(), `"storeName":"department-nfs"`) {
		t.Fatalf("manifest response = %d %s", manifestResponse.Code, manifestResponse.Body.String())
	}
	artifactResponse := httptest.NewRecorder()
	repository.artifacts = []domain.PublishedArtifact{{
		ExecutionID: executionID, RunNumber: 1, Name: "result",
		StoreName: "department-nfs", StoreVersion: 2, ObjectKey: "results/output.txt",
		ByteLength: 12, Checksum: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		PublishedAt: time.Date(2026, time.August, 23, 12, 0, 0, 0, time.UTC),
	}}
	handler.ServeHTTP(artifactResponse, httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "/v1/namespaces/research/jobs/"+testJobID+"/artifacts", http.NoBody,
	))
	if artifactResponse.Code != http.StatusOK ||
		!strings.Contains(artifactResponse.Body.String(), `"kind":"JobArtifactManifest"`) ||
		!strings.Contains(artifactResponse.Body.String(), `"objectKey":"results/output.txt"`) {
		t.Fatalf("artifact manifest response = %d %s", artifactResponse.Code, artifactResponse.Body.String())
	}
}

func testAgentTLSState(t *testing.T, agentID string) *tls.ConnectionState {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate test agent key: %v", err)
	}
	identityURI, err := url.Parse("urn:jobman:agent:" + agentID)
	if err != nil {
		t.Fatalf("parse test agent identity: %v", err)
	}
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(1), PublicKey: privateKey.Public(), URIs: []*url.URL{identityURI},
	}

	return &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{certificate},
		VerifiedChains:   [][]*x509.Certificate{{certificate}},
	}
}

func TestControlDocumentsRejectDuplicateKeys(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t, &fakeRepository{}, 2*1024*1024)
	request := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, "/v1/namespaces/research/targets",
		strings.NewReader(`{
			"apiVersion":"jobman.control/v1alpha1","kind":"Target","kind":"Target",
			"metadata":{"name":"host-a"},
			"spec":{"kind":"host","executionBackend":"subprocess","runtimes":["native"]}
		}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "target-duplicate-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func newTestHandler(t *testing.T, repository domain.ControlRepository, maximum int64) http.Handler {
	t.Helper()
	handler, err := New(Options{
		Repository: repository,
		Authenticator: auth.DevelopmentAuthenticator{Principal: domain.Principal{
			Issuer:  "test-issuer",
			Subject: "test-subject",
		}},
		MaxRequestBytes:      maximum,
		ReadinessTimeout:     time.Second,
		EnrollmentLifetime:   10 * time.Minute,
		AgentSessionLifetime: 15 * time.Minute,
		Logger:               slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	return handler
}

func validSubmitRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(
		"..", "..", "contracts", "jobman", "v1alpha1", "conformance", "valid", "job-request-minimal.json",
	))
	if err != nil {
		t.Fatalf("read request fixture: %v", err)
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, bytes.NewReader(contents))
	request.Header.Set("Content-Type", "application/json")

	return request
}

func validCollectionRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	workload := validWorkload(t)
	sealed, err := jobmanprotocol.SealCollectionRequest(jobmanprotocol.CollectionRequest{
		APIVersion: jobmanprotocol.V1Alpha1, Kind: jobmanprotocol.CollectionRequestKind,
		Metadata: jobmanprotocol.CollectionRequestMetadata{Namespace: "research", Name: "sweep"},
		Spec: jobmanprotocol.CollectionRequestSpec{
			MaxActive: 1, FailurePolicy: "continue", ArrayPolicy: "prefer",
			Items: []jobmanprotocol.CollectionItem{
				{Name: "trial-a", Workload: jobmanprotocol.WorkloadBinding{Document: workload}, Placement: jobmanprotocol.Placement{Target: "workstation-a"}},
				{Name: "trial-b", Workload: jobmanprotocol.WorkloadBinding{Document: workload}, Placement: jobmanprotocol.Placement{Target: "workstation-a"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("seal collection request: %v", err)
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, bytes.NewReader(sealed.CanonicalJSON))
	request.Header.Set("Content-Type", "application/json")

	return request
}

func validGraphRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	workload := validWorkload(t)
	sealed, err := jobmanprotocol.SealGraphRequest(jobmanprotocol.GraphRequest{
		APIVersion: jobmanprotocol.V1Alpha1, Kind: jobmanprotocol.GraphRequestKind,
		Metadata: jobmanprotocol.GraphRequestMetadata{Namespace: "research", Name: "pipeline"},
		Spec: jobmanprotocol.GraphRequestSpec{
			MaxActive: 1, UnsatisfiedPolicy: "skip",
			Nodes: []jobmanprotocol.GraphNode{
				{Name: "prepare", Workload: jobmanprotocol.WorkloadBinding{Document: workload}, Placement: jobmanprotocol.Placement{Target: "workstation-a"}},
				{Name: "analyze", Workload: jobmanprotocol.WorkloadBinding{Document: workload}, Placement: jobmanprotocol.Placement{Target: "workstation-a"}},
			},
			Edges: []jobmanprotocol.GraphEdge{{From: "prepare", To: "analyze", Predicate: "success"}},
		},
	})
	if err != nil {
		t.Fatalf("seal graph request: %v", err)
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, bytes.NewReader(sealed.CanonicalJSON))
	request.Header.Set("Content-Type", "application/json")

	return request
}

func validHistoryImportRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	sealed, err := jobmanprotocol.SealWorkload(validWorkload(t))
	if err != nil {
		t.Fatalf("seal history workload: %v", err)
	}
	document := completedHistoryImportRequest{
		APIVersion: apiVersion, Kind: "CompletedHistoryImport",
		Metadata: jobmanprotocol.JobRequestMetadata{Namespace: "research", Name: "imported-job"},
		Spec: completedHistoryImportSpec{
			Outcome: "success", CompletedAt: time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC),
			Source:    completedHistorySource{Store: "sqlite", Schema: 8, JobID: "local-job-1"},
			Workload:  jobmanprotocol.WorkloadBinding{Digest: sealed.Digest, Document: sealed.Document},
			Placement: jobmanprotocol.Placement{Target: "workstation-a"},
		},
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode history import: %v", err)
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")

	return request
}

func validWorkload(t *testing.T) jobmanprotocol.Workload {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(
		"..", "..", "contracts", "jobman", "v1alpha1", "conformance", "valid", "workload-minimal.json",
	))
	if err != nil {
		t.Fatalf("read workload fixture: %v", err)
	}
	var workload jobmanprotocol.Workload
	if err = json.Unmarshal(contents, &workload); err != nil {
		t.Fatalf("decode workload fixture: %v", err)
	}

	return workload
}

func testJob() domain.Job {
	timestamp := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)

	return domain.Job{
		ID:             testJobID,
		Namespace:      "research",
		Name:           "minimal-job",
		Phase:          domain.JobPhaseAccepted,
		DesiredState:   domain.JobDesiredStateRun,
		Target:         "workstation-a",
		WorkloadDigest: "sha256:eb0a1173adb99e86e4e10059248fd642f6b505702d43765cf7a904684aefde01",
		RequestDigest:  "sha256:9e79d05d21baea1a9f226d8c1b679a680b9edcac7169ccc171f202009ebc23c1",
		Revision:       1,
		CreatedAt:      timestamp,
		UpdatedAt:      timestamp,
	}
}

func testCollection() domain.Collection {
	first := testJob()
	second := testJob()
	second.ID = "22222222-2222-4222-8222-222222222222"
	second.Name = "trial-b"
	first.Name = "trial-a"

	return domain.Collection{
		ID: "33333333-3333-4333-8333-333333333333", Namespace: "research", Name: "sweep",
		MaxActive: 1, FailurePolicy: "continue", ArrayPolicy: "prefer", ArrayMode: "individual",
		Phase: "accepted", Revision: 1, Total: 2,
		Items:     []domain.CollectionItem{{Index: 0, Name: "trial-a", Job: first}, {Index: 1, Name: "trial-b", Job: second}},
		CreatedAt: first.CreatedAt, UpdatedAt: first.UpdatedAt,
	}
}

func testGraph() domain.Graph {
	first := testJob()
	first.Name = "prepare"
	second := testJob()
	second.ID = "22222222-2222-4222-8222-222222222222"
	second.Name = "analyze"

	return domain.Graph{
		ID: "44444444-4444-4444-8444-444444444444", Namespace: "research", Name: "pipeline",
		MaxActive: 1, UnsatisfiedPolicy: "skip", Phase: "accepted", Revision: 1,
		Total: 2, Waiting: 2,
		Items: []domain.GraphItem{
			{Index: 0, Name: "prepare", Job: first},
			{Index: 1, Name: "analyze", Dependencies: []domain.GraphDependency{{From: "prepare", Predicate: "success"}}, Job: second},
		},
		CreatedAt: first.CreatedAt, UpdatedAt: first.UpdatedAt,
	}
}
