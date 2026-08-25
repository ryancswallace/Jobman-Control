// Package httpapi implements the versioned Jobman Control client API.
package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"time"

	jobmanprotocol "github.com/ryancswallace/jobman-control/contracts/jobman/v1alpha1"
	"github.com/ryancswallace/jobman-control/internal/agentca"
	"github.com/ryancswallace/jobman-control/internal/auth"
	"github.com/ryancswallace/jobman-control/internal/contracts"
	"github.com/ryancswallace/jobman-control/internal/domain"
)

const apiVersion = "jobman.control/v1alpha1"

// Options contains the dependencies and bounds for one API handler.
type Options struct {
	Repository               domain.ControlRepository
	Authenticator            auth.Authenticator
	MaxRequestBytes          int64
	ReadinessTimeout         time.Duration
	EnrollmentLifetime       time.Duration
	AgentSessionLifetime     time.Duration
	AgentCertificateLifetime time.Duration
	CertificateAuthority     *agentca.Authority
	Logger                   *slog.Logger
}

type api struct {
	repository               domain.ControlRepository
	authenticator            auth.Authenticator
	maxRequestBytes          int64
	readinessTimeout         time.Duration
	logger                   *slog.Logger
	enrollmentLifetime       time.Duration
	agentSessionLifetime     time.Duration
	agentCertificateLifetime time.Duration
	certificateAuthority     *agentca.Authority
}

// New returns the complete first-slice HTTP handler.
func New(options Options) (http.Handler, error) {
	if options.Repository == nil {
		return nil, errors.New("HTTP API repository is required")
	}
	if options.Authenticator == nil {
		return nil, errors.New("HTTP API authenticator is required")
	}
	if options.MaxRequestBytes < 1 {
		return nil, errors.New("HTTP API maximum request bytes must be positive")
	}
	if options.ReadinessTimeout <= 0 {
		return nil, errors.New("HTTP API readiness timeout must be positive")
	}
	if options.Logger == nil {
		return nil, errors.New("HTTP API logger is required")
	}
	if options.EnrollmentLifetime <= 0 || options.AgentSessionLifetime <= 0 {
		return nil, errors.New("HTTP API agent credential lifetimes must be positive")
	}
	if options.CertificateAuthority != nil && options.AgentCertificateLifetime <= 0 {
		return nil, errors.New("HTTP API agent certificate lifetime must be positive")
	}

	serverAPI := &api{
		repository:               options.Repository,
		authenticator:            options.Authenticator,
		maxRequestBytes:          options.MaxRequestBytes,
		readinessTimeout:         options.ReadinessTimeout,
		logger:                   options.Logger,
		enrollmentLifetime:       options.EnrollmentLifetime,
		agentSessionLifetime:     options.AgentSessionLifetime,
		agentCertificateLifetime: options.AgentCertificateLifetime,
		certificateAuthority:     options.CertificateAuthority,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", serverAPI.health)
	mux.HandleFunc("GET /readyz", serverAPI.ready)
	mux.HandleFunc("GET /metrics", serverAPI.metrics)
	mux.Handle("POST /v1/namespaces/{namespace}/jobs", serverAPI.client(serverAPI.submitJob))
	mux.Handle("POST /v1/namespaces/{namespace}/collections", serverAPI.client(serverAPI.submitCollection))
	mux.Handle("GET /v1/namespaces/{namespace}/collections/{collectionID}", serverAPI.client(serverAPI.getCollection))
	mux.Handle("POST /v1/namespaces/{namespace}/graphs", serverAPI.client(serverAPI.submitGraph))
	mux.Handle("GET /v1/namespaces/{namespace}/graphs/{graphID}", serverAPI.client(serverAPI.getGraph))
	mux.Handle("POST /v1/namespaces/{namespace}/graphs/{graphID}/cancel", serverAPI.client(serverAPI.cancelGraph))
	mux.Handle("GET /v1/namespaces/{namespace}/jobs", serverAPI.client(serverAPI.listJobs))
	mux.Handle("GET /v1/namespaces/{namespace}/jobs/{jobID}", serverAPI.client(serverAPI.getJob))
	mux.Handle("GET /v1/namespaces/{namespace}/jobs/{jobID}/logs", serverAPI.client(serverAPI.getJobLogs))
	mux.Handle("GET /v1/namespaces/{namespace}/jobs/{jobID}/artifacts", serverAPI.client(serverAPI.getJobArtifacts))
	mux.Handle("PUT /v1/namespaces/{namespace}/memberships", serverAPI.client(serverAPI.putMembership))
	mux.Handle("GET /v1/namespaces/{namespace}/policy", serverAPI.client(serverAPI.getNamespacePolicy))
	mux.Handle("PUT /v1/namespaces/{namespace}/policy", serverAPI.client(serverAPI.updateNamespacePolicy))
	mux.Handle("GET /v1/namespaces/{namespace}/audit", serverAPI.client(serverAPI.exportAudit))
	mux.Handle("POST /v1/namespaces/{namespace}/history/imports", serverAPI.client(serverAPI.importCompletedHistory))
	mux.Handle("POST /v1/namespaces/{namespace}/targets", serverAPI.client(serverAPI.createTarget))
	mux.Handle("GET /v1/namespaces/{namespace}/targets", serverAPI.client(serverAPI.listTargets))
	mux.Handle("GET /v1/namespaces/{namespace}/targets/{target}", serverAPI.client(serverAPI.getTarget))
	mux.Handle("POST /v1/namespaces/{namespace}/targets/{target}/generations", serverAPI.client(serverAPI.createTargetGeneration))
	mux.Handle("PUT /v1/namespaces/{namespace}/targets/{target}/state", serverAPI.client(serverAPI.updateTargetState))
	mux.Handle(
		"POST /v1/namespaces/{namespace}/targets/{target}/enrollment-tokens",
		serverAPI.client(serverAPI.createEnrollmentToken),
	)
	mux.HandleFunc("POST /v1/agent/enroll", serverAPI.enrollAgent)
	mux.HandleFunc("POST /v1/agent/session/renew", serverAPI.renewAgentSession)
	mux.HandleFunc("GET /v1/agent/assignments", serverAPI.listAgentAssignments)
	mux.HandleFunc("POST /v1/agent/certificate/renew", serverAPI.renewAgentCertificate)
	mux.HandleFunc("PUT /v1/agent/capabilities", serverAPI.recordAgentCapabilities)
	mux.HandleFunc("POST /v1/agent/assignments/{deliveryID}/accept", serverAPI.acceptAgentAssignment)
	mux.HandleFunc("POST /v1/agent/executions/{executionID}/events", serverAPI.recordAgentExecutionEvent)
	mux.HandleFunc("PUT /v1/agent/executions/{executionID}/logs/{stream}/chunks/{sequence}", serverAPI.commitAgentLogChunk)
	mux.HandleFunc("GET /v1/agent/actions", serverAPI.listAgentActions)
	mux.HandleFunc("POST /v1/agent/actions/{actionID}/acknowledge", serverAPI.acknowledgeAgentAction)
	mux.Handle("POST /v1/namespaces/{namespace}/jobs/{jobID}/cancel", serverAPI.client(serverAPI.cancelJob))
	mux.HandleFunc("/", serverAPI.notFound)

	return securityHeaders(mux), nil
}

func (service *api) health(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

func (service *api) ready(writer http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), service.readinessTimeout)
	defer cancel()
	if err := service.repository.Ready(ctx); err != nil {
		service.logger.WarnContext(request.Context(), "readiness check failed")
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})

		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ready"})
}

func (service *api) metrics(writer http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), service.readinessTimeout)
	defer cancel()
	snapshot, err := service.repository.OperationalSnapshot(ctx)
	if err != nil {
		service.logger.WarnContext(request.Context(), "metrics snapshot failed")
		writeError(writer, http.StatusServiceUnavailable, "metrics_unavailable", "metrics are temporarily unavailable")
		return
	}
	writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	phases := []string{"accepted", "assigning", "accepted_execution", "running", "terminal"}
	for _, phase := range phases {
		_, _ = fmt.Fprintf(writer, "jobman_control_jobs{phase=%q} %d\n", phase, snapshot.JobsByPhase[phase])
	}
	statuses := []string{"active", "draining", "disabled", "retired"}
	for _, status := range statuses {
		_, _ = fmt.Fprintf(writer, "jobman_control_agents{status=%q} %d\n", status, snapshot.AgentsByStatus[status])
	}
	_, _ = fmt.Fprintf(writer, "jobman_control_outbox_unpublished %d\n", snapshot.UnpublishedOutbox)
	_, _ = fmt.Fprintf(writer, "jobman_control_executions_stale %d\n", snapshot.StaleExecutions)
	_, _ = fmt.Fprintf(writer, "jobman_control_oldest_queue_age_seconds %.3f\n", snapshot.OldestQueueAge.Seconds())
	if snapshot.RecoveryHold {
		_, _ = fmt.Fprintln(writer, "jobman_control_reconciliation_hold 1")
	} else {
		_, _ = fmt.Fprintln(writer, "jobman_control_reconciliation_hold 0")
	}
	_, _ = fmt.Fprintf(writer, "jobman_control_restore_epoch %d\n", snapshot.RestoreEpoch)
}

type clientHandler func(http.ResponseWriter, *http.Request, domain.Principal)

func (service *api) client(next clientHandler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		values := request.Header.Values("Authorization")
		if len(values) > 1 {
			writeUnauthenticated(writer)
			return
		}
		value := ""
		if len(values) == 1 {
			value = values[0]
		}
		principal, err := service.authenticator.Authenticate(request.Context(), value)
		if err != nil {
			writeUnauthenticated(writer)
			return
		}
		next(writer, request, principal)
	})
}

func (service *api) submitJob(
	writer http.ResponseWriter,
	request *http.Request,
	principal domain.Principal,
) {
	if !isJSONContentType(request.Header.Get("Content-Type")) {
		writeError(writer, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")

		return
	}
	idempotencyKey, valid := readIdempotencyKey(request.Header)
	if !valid {
		writeError(writer, http.StatusBadRequest, "invalid_idempotency_key", "exactly one valid Idempotency-Key header is required")

		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, service.maxRequestBytes)
	decoded, err := contracts.DecodeJobRequest(request.Body)
	if err != nil {
		var maximumBytesError *http.MaxBytesError
		if errors.As(err, &maximumBytesError) {
			writeError(writer, http.StatusRequestEntityTooLarge, "request_too_large", "job request exceeds the configured size limit")
		} else {
			writeError(writer, http.StatusBadRequest, "invalid_request", "job request is invalid")
		}

		return
	}
	if decoded.Namespace != request.PathValue("namespace") {
		writeError(writer, http.StatusBadRequest, "namespace_mismatch", "path and document namespaces must match")

		return
	}

	result, err := service.repository.SubmitJob(
		request.Context(), principal, idempotencyKey, domain.JobSubmission{
			Namespace:        decoded.Namespace,
			Name:             decoded.Name,
			Labels:           decoded.Labels,
			Target:           decoded.Target,
			Partition:        decoded.Partition,
			RuntimeKind:      decoded.RuntimeKind,
			OperatingSystems: decoded.OperatingSystems,
			Architectures:    decoded.Architectures,
			Capabilities:     decoded.Capabilities,
			ArtifactStores:   decoded.ArtifactStores,
			WorkloadDigest:   decoded.WorkloadDigest,
			WorkloadDocument: decoded.WorkloadDocument,
			RequestDigest:    decoded.RequestDigest,
			RequestDocument:  decoded.RequestDocument,
			ExecutionFeatures: domain.ExecutionFeatures{
				DirectCommand:                decoded.ExecutionFeatures.DirectCommand,
				Resources:                    decoded.ExecutionFeatures.Resources,
				TemporaryStorage:             decoded.ExecutionFeatures.TemporaryStorage,
				Artifacts:                    decoded.ExecutionFeatures.Artifacts,
				Extensions:                   decoded.ExecutionFeatures.Extensions,
				EnvironmentProfile:           decoded.ExecutionFeatures.EnvironmentProfile,
				Secrets:                      decoded.ExecutionFeatures.Secrets,
				RetryMaxRuns:                 decoded.ExecutionFeatures.RetryMaxRuns,
				SchedulerEnvironmentOverride: decoded.ExecutionFeatures.SchedulerEnvironmentOverride,
			},
		},
	)
	if err != nil {
		service.writeRepositoryError(writer, request, "submit job", err)

		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writer.Header().Set("Location", jobLocation(result.Job))
	writer.Header().Set("ETag", revisionETag(result.Job.Revision))
	writeJSON(writer, status, newJobResponse(result.Job))
}

func (service *api) submitCollection(
	writer http.ResponseWriter,
	request *http.Request,
	principal domain.Principal,
) {
	if !isJSONContentType(request.Header.Get("Content-Type")) {
		writeError(writer, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")

		return
	}
	idempotencyKey, valid := readIdempotencyKey(request.Header)
	if !valid {
		writeError(writer, http.StatusBadRequest, "invalid_idempotency_key", "exactly one valid Idempotency-Key header is required")

		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, service.maxRequestBytes)
	decoded, err := contracts.DecodeCollectionRequest(request.Body)
	if err != nil {
		var maximumBytesError *http.MaxBytesError
		if errors.As(err, &maximumBytesError) {
			writeError(writer, http.StatusRequestEntityTooLarge, "request_too_large", "collection request exceeds the configured size limit")
		} else {
			writeError(writer, http.StatusBadRequest, "invalid_request", "collection request is invalid")
		}

		return
	}
	if decoded.Namespace != request.PathValue("namespace") {
		writeError(writer, http.StatusBadRequest, "namespace_mismatch", "path and document namespaces must match")

		return
	}
	items := make([]domain.JobSubmission, 0, len(decoded.Items))
	for _, item := range decoded.Items {
		items = append(items, domainSubmission(item))
	}
	result, err := service.repository.SubmitCollection(
		request.Context(), principal, idempotencyKey, domain.CollectionSubmission{
			Namespace: decoded.Namespace, Name: decoded.Name, Labels: decoded.Labels,
			MaxActive: decoded.MaxActive, FailurePolicy: decoded.FailurePolicy,
			ArrayPolicy: decoded.ArrayPolicy, RequestDigest: decoded.RequestDigest,
			RequestDocument: decoded.RequestDocument, Items: items,
		},
	)
	if err != nil {
		service.writeRepositoryError(writer, request, "submit collection", err)

		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writer.Header().Set("Location", collectionLocation(result.Collection))
	writer.Header().Set("ETag", revisionETag(result.Collection.Revision))
	writeJSON(writer, status, newCollectionResponse(result.Collection))
}

func domainSubmission(decoded contracts.JobRequest) domain.JobSubmission {
	return domain.JobSubmission{
		Namespace: decoded.Namespace, Name: decoded.Name, Labels: decoded.Labels,
		Target: decoded.Target, Partition: decoded.Partition, RuntimeKind: decoded.RuntimeKind,
		OperatingSystems: decoded.OperatingSystems, Architectures: decoded.Architectures,
		Capabilities: decoded.Capabilities, ArtifactStores: decoded.ArtifactStores,
		WorkloadDigest: decoded.WorkloadDigest, WorkloadDocument: decoded.WorkloadDocument,
		RequestDigest: decoded.RequestDigest, RequestDocument: decoded.RequestDocument,
		ExecutionFeatures: domain.ExecutionFeatures{
			DirectCommand:                decoded.ExecutionFeatures.DirectCommand,
			Resources:                    decoded.ExecutionFeatures.Resources,
			TemporaryStorage:             decoded.ExecutionFeatures.TemporaryStorage,
			Artifacts:                    decoded.ExecutionFeatures.Artifacts,
			Extensions:                   decoded.ExecutionFeatures.Extensions,
			EnvironmentProfile:           decoded.ExecutionFeatures.EnvironmentProfile,
			Secrets:                      decoded.ExecutionFeatures.Secrets,
			RetryMaxRuns:                 decoded.ExecutionFeatures.RetryMaxRuns,
			SchedulerEnvironmentOverride: decoded.ExecutionFeatures.SchedulerEnvironmentOverride,
		},
	}
}

func (service *api) submitGraph(
	writer http.ResponseWriter,
	request *http.Request,
	principal domain.Principal,
) {
	if !isJSONContentType(request.Header.Get("Content-Type")) {
		writeError(writer, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return
	}
	idempotencyKey, valid := readIdempotencyKey(request.Header)
	if !valid {
		writeError(writer, http.StatusBadRequest, "invalid_idempotency_key", "exactly one valid Idempotency-Key header is required")
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, service.maxRequestBytes)
	decoded, err := contracts.DecodeGraphRequest(request.Body)
	if err != nil {
		var maximumBytesError *http.MaxBytesError
		if errors.As(err, &maximumBytesError) {
			writeError(writer, http.StatusRequestEntityTooLarge, "request_too_large", "graph request exceeds the configured size limit")
		} else {
			writeError(writer, http.StatusBadRequest, "invalid_request", "graph request is invalid")
		}
		return
	}
	if decoded.Namespace != request.PathValue("namespace") {
		writeError(writer, http.StatusBadRequest, "namespace_mismatch", "path and document namespaces must match")
		return
	}
	nodes := make([]domain.JobSubmission, 0, len(decoded.Nodes))
	for _, node := range decoded.Nodes {
		nodes = append(nodes, domainSubmission(node))
	}
	edges := make([]domain.GraphEdgeSubmission, 0, len(decoded.Edges))
	for _, edge := range decoded.Edges {
		edges = append(edges, domain.GraphEdgeSubmission{
			From: edge.From, To: edge.To, Predicate: edge.Predicate,
			Outcomes: append([]string(nil), edge.Outcomes...),
		})
	}
	result, err := service.repository.SubmitGraph(
		request.Context(), principal, idempotencyKey, domain.GraphSubmission{
			Namespace: decoded.Namespace, Name: decoded.Name, Labels: decoded.Labels,
			MaxActive: decoded.MaxActive, UnsatisfiedPolicy: decoded.UnsatisfiedPolicy,
			RequestDigest: decoded.RequestDigest, RequestDocument: decoded.RequestDocument,
			Nodes: nodes, Edges: edges,
		},
	)
	if err != nil {
		service.writeRepositoryError(writer, request, "submit graph", err)
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writer.Header().Set("Location", graphLocation(result.Graph))
	writer.Header().Set("ETag", revisionETag(result.Graph.Revision))
	writeJSON(writer, status, newGraphResponse(result.Graph))
}

func (service *api) getGraph(
	writer http.ResponseWriter,
	request *http.Request,
	principal domain.Principal,
) {
	graphID := request.PathValue("graphID")
	if !domain.IsID(graphID) {
		writeError(writer, http.StatusBadRequest, "invalid_graph_id", "graph ID is invalid")
		return
	}
	graph, err := service.repository.GetGraph(
		request.Context(), principal, request.PathValue("namespace"), graphID,
	)
	if err != nil {
		service.writeRepositoryError(writer, request, "get graph", err)
		return
	}
	writer.Header().Set("ETag", revisionETag(graph.Revision))
	writeJSON(writer, http.StatusOK, newGraphResponse(graph))
}

func (service *api) cancelGraph(
	writer http.ResponseWriter,
	request *http.Request,
	principal domain.Principal,
) {
	idempotencyKey, valid := readIdempotencyKey(request.Header)
	if !valid {
		writeError(writer, http.StatusBadRequest, "invalid_idempotency_key", "exactly one valid Idempotency-Key header is required")
		return
	}
	graphID := request.PathValue("graphID")
	if !domain.IsID(graphID) {
		writeError(writer, http.StatusBadRequest, "invalid_graph_id", "graph ID is invalid")
		return
	}
	digest, err := semanticDigest(map[string]string{
		"operation": "cancel", "namespace": request.PathValue("namespace"), "graphId": graphID,
	})
	if err != nil {
		service.writeRepositoryError(writer, request, "digest graph cancellation", err)
		return
	}
	graph, err := service.repository.CancelGraph(
		request.Context(), principal, request.PathValue("namespace"), graphID,
		idempotencyKey, digest,
	)
	if err != nil {
		service.writeRepositoryError(writer, request, "cancel graph", err)
		return
	}
	writer.Header().Set("ETag", revisionETag(graph.Revision))
	writeJSON(writer, http.StatusOK, newGraphResponse(graph))
}

func (service *api) getCollection(
	writer http.ResponseWriter,
	request *http.Request,
	principal domain.Principal,
) {
	collectionID := request.PathValue("collectionID")
	if !domain.IsID(collectionID) {
		writeError(writer, http.StatusBadRequest, "invalid_collection_id", "collection ID is invalid")

		return
	}
	collection, err := service.repository.GetCollection(
		request.Context(), principal, request.PathValue("namespace"), collectionID,
	)
	if err != nil {
		service.writeRepositoryError(writer, request, "get collection", err)

		return
	}
	writer.Header().Set("ETag", revisionETag(collection.Revision))
	writeJSON(writer, http.StatusOK, newCollectionResponse(collection))
}

func (service *api) getJob(
	writer http.ResponseWriter,
	request *http.Request,
	principal domain.Principal,
) {
	jobID := request.PathValue("jobID")
	if !domain.IsID(jobID) {
		writeError(writer, http.StatusBadRequest, "invalid_job_id", "job ID is invalid")

		return
	}
	job, err := service.repository.GetJob(
		request.Context(), principal, request.PathValue("namespace"), jobID,
	)
	if err != nil {
		service.writeRepositoryError(writer, request, "get job", err)

		return
	}
	writer.Header().Set("ETag", revisionETag(job.Revision))
	writeJSON(writer, http.StatusOK, newJobResponse(job))
}

func (service *api) listJobs(
	writer http.ResponseWriter,
	request *http.Request,
	principal domain.Principal,
) {
	options, err := readJobListOptions(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	page, err := service.repository.ListJobs(
		request.Context(), principal, request.PathValue("namespace"), options,
	)
	if err != nil {
		service.writeRepositoryError(writer, request, "list jobs", err)
		return
	}
	items := make([]jobResponse, 0, len(page.Jobs))
	for _, job := range page.Jobs {
		items = append(items, newJobResponse(job))
	}
	response := jobListResponse{APIVersion: apiVersion, Kind: "JobList", Items: items}
	if page.NextCursor != nil {
		response.NextPageToken, err = encodeJobPageToken(*page.NextCursor)
		if err != nil {
			service.writeRepositoryError(writer, request, "encode job page token", err)
			return
		}
	}
	writeJSON(writer, http.StatusOK, response)
}

type jobPageToken struct {
	CreatedAt time.Time `json:"createdAt"`
	ID        string    `json:"id"`
}

func readJobListOptions(request *http.Request) (domain.JobListOptions, error) {
	query := request.URL.Query()
	if len(query) > 3 {
		return domain.JobListOptions{}, errors.New("unsupported query parameter")
	}
	for name := range query {
		if name != "limit" && name != "phase" && name != "pageToken" {
			return domain.JobListOptions{}, errors.New("unsupported query parameter")
		}
		if len(query[name]) != 1 {
			return domain.JobListOptions{}, fmt.Errorf("%s must be specified once", name)
		}
	}
	options := domain.JobListOptions{Limit: domain.DefaultJobListLimit, Phase: query.Get("phase")}
	if value := query.Get("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 || limit > domain.MaximumJobListLimit {
			return domain.JobListOptions{}, fmt.Errorf(
				"limit must be between 1 and %d", domain.MaximumJobListLimit,
			)
		}
		options.Limit = limit
	}
	if options.Phase != "" && !domain.ValidJobPhase(options.Phase) {
		return domain.JobListOptions{}, errors.New("phase is invalid")
	}
	if token := query.Get("pageToken"); token != "" {
		cursor, err := decodeJobPageToken(token)
		if err != nil {
			return domain.JobListOptions{}, errors.New("pageToken is invalid")
		}
		options.Before = &cursor
	}

	return options, nil
}

func encodeJobPageToken(cursor domain.JobCursor) (string, error) {
	encoded, err := json.Marshal(jobPageToken{CreatedAt: cursor.CreatedAt.UTC(), ID: cursor.ID})
	if err != nil {
		return "", fmt.Errorf("encode job page token: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeJobPageToken(value string) (domain.JobCursor, error) {
	if len(value) > 1024 {
		return domain.JobCursor{}, errors.New("page token is too large")
	}
	encoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return domain.JobCursor{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var token jobPageToken
	if decodeErr := decoder.Decode(&token); decodeErr != nil {
		return domain.JobCursor{}, decodeErr
	}
	if trailingErr := decoder.Decode(new(any)); !errors.Is(trailingErr, io.EOF) {
		return domain.JobCursor{}, errors.New("page token contains trailing data")
	}
	if token.CreatedAt.IsZero() || !domain.IsID(token.ID) {
		return domain.JobCursor{}, errors.New("page token content is invalid")
	}

	return domain.JobCursor{CreatedAt: token.CreatedAt.UTC(), ID: token.ID}, nil
}

func (service *api) cancelJob(
	writer http.ResponseWriter,
	request *http.Request,
	principal domain.Principal,
) {
	idempotencyKey, valid := readIdempotencyKey(request.Header)
	if !valid {
		writeError(writer, http.StatusBadRequest, "invalid_idempotency_key", "exactly one valid Idempotency-Key header is required")
		return
	}
	jobID := request.PathValue("jobID")
	if !domain.IsID(jobID) {
		writeError(writer, http.StatusBadRequest, "invalid_job_id", "job ID is invalid")
		return
	}
	digest, err := semanticDigest(map[string]string{
		"operation": "cancel", "namespace": request.PathValue("namespace"), "jobId": jobID,
	})
	if err != nil {
		service.writeRepositoryError(writer, request, "digest job cancellation", err)
		return
	}
	job, err := service.repository.CancelJob(
		request.Context(), principal, request.PathValue("namespace"), jobID,
		idempotencyKey, digest,
	)
	if err != nil {
		service.writeRepositoryError(writer, request, "cancel job", err)
		return
	}
	writer.Header().Set("ETag", revisionETag(job.Revision))
	writeJSON(writer, http.StatusOK, newJobResponse(job))
}

type namespacePolicyRequest struct {
	APIVersion string              `json:"apiVersion"`
	Kind       string              `json:"kind"`
	Spec       namespacePolicySpec `json:"spec"`
}

type namespacePolicySpec struct {
	MaxActiveJobs            int    `json:"maxActiveJobs"`
	MaxQueuedJobs            int    `json:"maxQueuedJobs"`
	MaxCollectionItems       int    `json:"maxCollectionItems"`
	MaxGraphNodes            int    `json:"maxGraphNodes"`
	IdempotencyRetention     string `json:"idempotencyRetention"`
	PublishedOutboxRetention string `json:"publishedOutboxRetention"`
}

type namespacePolicyResponse struct {
	APIVersion string                  `json:"apiVersion"`
	Kind       string                  `json:"kind"`
	Metadata   namespacePolicyMetadata `json:"metadata"`
	Spec       namespacePolicySpec     `json:"spec"`
}

type namespacePolicyMetadata struct {
	Namespace string    `json:"namespace"`
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (service *api) getNamespacePolicy(
	writer http.ResponseWriter,
	request *http.Request,
	principal domain.Principal,
) {
	policy, err := service.repository.GetNamespacePolicy(
		request.Context(), principal, request.PathValue("namespace"),
	)
	if err != nil {
		service.writeRepositoryError(writer, request, "get namespace policy", err)
		return
	}
	writer.Header().Set("ETag", revisionETag(policy.Revision))
	writeJSON(writer, http.StatusOK, newNamespacePolicyResponse(policy))
}

func (service *api) updateNamespacePolicy(
	writer http.ResponseWriter,
	request *http.Request,
	principal domain.Principal,
) {
	expectedRevision, valid := readIfMatchRevision(request.Header)
	if !valid {
		writeError(writer, http.StatusPreconditionRequired, "revision_required", "exactly one policy revision If-Match header is required")
		return
	}
	var document namespacePolicyRequest
	_, err := service.decodeControlJSON(writer, request, &document)
	if err != nil {
		service.writeDecodeError(writer, err)
		return
	}
	idempotencyRetention, idempotencyErr := time.ParseDuration(document.Spec.IdempotencyRetention)
	outboxRetention, outboxErr := time.ParseDuration(document.Spec.PublishedOutboxRetention)
	if document.APIVersion != apiVersion || document.Kind != "NamespacePolicy" ||
		idempotencyErr != nil || outboxErr != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request", "namespace policy request is invalid")
		return
	}
	policy, err := service.repository.UpdateNamespacePolicy(
		request.Context(), principal, request.PathValue("namespace"), domain.NamespacePolicyChange{
			MaxActiveJobs: document.Spec.MaxActiveJobs, MaxQueuedJobs: document.Spec.MaxQueuedJobs,
			MaxCollectionItems: document.Spec.MaxCollectionItems, MaxGraphNodes: document.Spec.MaxGraphNodes,
			IdempotencyRetention:     idempotencyRetention,
			PublishedOutboxRetention: outboxRetention, ExpectedRevision: expectedRevision,
		},
	)
	if err != nil {
		service.writeRepositoryError(writer, request, "update namespace policy", err)
		return
	}
	writer.Header().Set("ETag", revisionETag(policy.Revision))
	writeJSON(writer, http.StatusOK, newNamespacePolicyResponse(policy))
}

func newNamespacePolicyResponse(policy domain.NamespacePolicy) namespacePolicyResponse {
	return namespacePolicyResponse{
		APIVersion: apiVersion, Kind: "NamespacePolicy",
		Metadata: namespacePolicyMetadata{
			Namespace: policy.Namespace, Revision: policy.Revision,
			CreatedAt: policy.CreatedAt, UpdatedAt: policy.UpdatedAt,
		},
		Spec: namespacePolicySpec{
			MaxActiveJobs: policy.MaxActiveJobs, MaxQueuedJobs: policy.MaxQueuedJobs,
			MaxCollectionItems: policy.MaxCollectionItems, MaxGraphNodes: policy.MaxGraphNodes,
			IdempotencyRetention:     policy.IdempotencyRetention.String(),
			PublishedOutboxRetention: policy.PublishedOutboxRetention.String(),
		},
	}
}

type auditEventResponse struct {
	ID               int64           `json:"id"`
	Namespace        string          `json:"namespace"`
	ActorKind        string          `json:"actorKind"`
	ActorPrincipalID string          `json:"actorPrincipalId,omitempty"`
	ActorAgentID     string          `json:"actorAgentId,omitempty"`
	Action           string          `json:"action"`
	ResourceType     string          `json:"resourceType"`
	ResourceID       string          `json:"resourceId"`
	RequestDigest    string          `json:"requestDigest,omitempty"`
	IdempotencyKey   string          `json:"idempotencyKey,omitempty"`
	Details          json.RawMessage `json:"details"`
	OccurredAt       time.Time       `json:"occurredAt"`
}

func (service *api) exportAudit(
	writer http.ResponseWriter,
	request *http.Request,
	principal domain.Principal,
) {
	query := request.URL.Query()
	if len(query) > 2 {
		writeError(writer, http.StatusBadRequest, "invalid_query", "unsupported query parameter")
		return
	}
	for name, values := range query {
		if (name != "afterId" && name != "limit") || len(values) != 1 {
			writeError(writer, http.StatusBadRequest, "invalid_query", "unsupported or repeated query parameter")
			return
		}
	}
	afterID := int64(0)
	limit := 200
	var err error
	if value := query.Get("afterId"); value != "" {
		afterID, err = strconv.ParseInt(value, 10, 64)
		if err != nil || afterID < 0 {
			writeError(writer, http.StatusBadRequest, "invalid_query", "afterId is invalid")
			return
		}
	}
	if value := query.Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 1000 {
			writeError(writer, http.StatusBadRequest, "invalid_query", "limit must be between 1 and 1000")
			return
		}
	}
	page, err := service.repository.ExportAudit(
		request.Context(), principal, request.PathValue("namespace"), afterID, limit,
	)
	if err != nil {
		service.writeRepositoryError(writer, request, "export audit", err)
		return
	}
	items := make([]auditEventResponse, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, auditEventResponse{
			ID: item.ID, Namespace: item.Namespace, ActorKind: item.ActorKind,
			ActorPrincipalID: item.ActorPrincipalID, ActorAgentID: item.ActorAgentID,
			Action: item.Action, ResourceType: item.ResourceType, ResourceID: item.ResourceID,
			RequestDigest: item.RequestDigest, IdempotencyKey: item.IdempotencyKey,
			Details: item.Details, OccurredAt: item.OccurredAt,
		})
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"apiVersion": apiVersion, "kind": "AuditEventList", "items": items,
		"nextAfterId": page.NextAfterID,
	})
}

type completedHistoryImportRequest struct {
	APIVersion string                            `json:"apiVersion"`
	Kind       string                            `json:"kind"`
	Metadata   jobmanprotocol.JobRequestMetadata `json:"metadata"`
	Spec       completedHistoryImportSpec        `json:"spec"`
}

type completedHistoryImportSpec struct {
	Outcome     string                         `json:"outcome"`
	CompletedAt time.Time                      `json:"completedAt"`
	Source      completedHistorySource         `json:"source"`
	Workload    jobmanprotocol.WorkloadBinding `json:"workload"`
	Placement   jobmanprotocol.Placement       `json:"placement"`
}

type completedHistorySource struct {
	Store  string `json:"store"`
	Schema int    `json:"schema"`
	JobID  string `json:"jobId"`
}

func (service *api) importCompletedHistory(
	writer http.ResponseWriter,
	request *http.Request,
	principal domain.Principal,
) {
	query := request.URL.Query()
	if len(query) > 1 || len(query["dryRun"]) > 1 {
		writeError(writer, http.StatusBadRequest, "invalid_query", "only one dryRun query parameter is supported")
		return
	}
	dryRun := false
	if value := query.Get("dryRun"); value != "" {
		var err error
		dryRun, err = strconv.ParseBool(value)
		if err != nil {
			writeError(writer, http.StatusBadRequest, "invalid_query", "dryRun must be true or false")
			return
		}
	}
	idempotencyKey := ""
	if !dryRun {
		var valid bool
		idempotencyKey, valid = readIdempotencyKey(request.Header)
		if !valid {
			writeError(writer, http.StatusBadRequest, "invalid_idempotency_key", "exactly one valid Idempotency-Key header is required")
			return
		}
	}
	var document completedHistoryImportRequest
	digest, err := service.decodeControlJSON(writer, request, &document)
	if err != nil {
		service.writeDecodeError(writer, err)
		return
	}
	if document.APIVersion != apiVersion || document.Kind != "CompletedHistoryImport" ||
		document.Metadata.Namespace != request.PathValue("namespace") {
		writeError(writer, http.StatusBadRequest, "invalid_request", "completed history import is invalid")
		return
	}
	encodedJob, err := json.Marshal(jobmanprotocol.JobRequest{
		APIVersion: jobmanprotocol.V1Alpha1, Kind: jobmanprotocol.JobRequestKind,
		Metadata: document.Metadata,
		Spec: jobmanprotocol.JobRequestSpec{
			Workload: document.Spec.Workload, Placement: document.Spec.Placement,
		},
	})
	if err != nil {
		service.writeRepositoryError(writer, request, "encode imported history", err)
		return
	}
	projected, err := contracts.DecodeJobRequest(bytes.NewReader(encodedJob))
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request", "completed history workload is invalid")
		return
	}
	requestDocument, err := json.Marshal(document)
	if err != nil {
		service.writeRepositoryError(writer, request, "encode history import request", err)
		return
	}
	result, err := service.repository.ImportCompletedHistory(
		request.Context(), principal, idempotencyKey, dryRun, domain.CompletedHistoryImport{
			Job: domainSubmission(projected), Outcome: document.Spec.Outcome,
			CompletedAt: document.Spec.CompletedAt.UTC(), SourceStore: document.Spec.Source.Store,
			SourceSchema: document.Spec.Source.Schema, SourceJobID: document.Spec.Source.JobID,
			RequestDigest: digest, RequestDocument: requestDocument,
		},
	)
	if err != nil {
		service.writeRepositoryError(writer, request, "import completed history", err)
		return
	}
	if result.DryRun {
		writeJSON(writer, http.StatusOK, map[string]any{
			"apiVersion": apiVersion, "kind": "CompletedHistoryImportPlan",
			"status": map[string]string{"result": "valid"},
		})
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writer.Header().Set("Location", jobLocation(result.Job))
	writeJSON(writer, status, newJobResponse(result.Job))
}

func (service *api) notFound(writer http.ResponseWriter, _ *http.Request) {
	writeError(writer, http.StatusNotFound, "not_found", "resource not found")
}

func (service *api) writeRepositoryError(
	writer http.ResponseWriter,
	request *http.Request,
	operation string,
	err error,
) {
	switch {
	case errors.Is(err, domain.ErrUnauthenticated):
		writeUnauthenticated(writer)
	case errors.Is(err, domain.ErrForbidden):
		writeError(writer, http.StatusForbidden, "forbidden", "principal is not authorized for this namespace")
	case errors.Is(err, domain.ErrNotFound):
		writeError(writer, http.StatusNotFound, "not_found", "resource not found")
	case errors.Is(err, domain.ErrIdempotencyConflict):
		writeError(writer, http.StatusConflict, "idempotency_conflict", "Idempotency-Key was already used for a different request")
	case errors.Is(err, domain.ErrConflict):
		writeError(writer, http.StatusConflict, "conflict", "resource conflicts with current state")
	case errors.Is(err, domain.ErrInvalidPlacement):
		writeError(writer, http.StatusUnprocessableEntity, "invalid_placement", "selected target cannot satisfy the workload")
	case errors.Is(err, domain.ErrQuotaExceeded):
		writeError(writer, http.StatusUnprocessableEntity, "quota_exceeded", "namespace quota does not permit this request")
	default:
		service.logger.ErrorContext(request.Context(), "API repository operation failed", "operation", operation)
		writeError(writer, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}

func isJSONContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)

	return err == nil && mediaType == "application/json"
}

func readIdempotencyKey(header http.Header) (string, bool) {
	values := header.Values("Idempotency-Key")
	if len(values) != 1 || !domain.ValidIdempotencyKey(values[0]) {
		return "", false
	}

	return values[0], true
}

func jobLocation(job domain.Job) string {
	return "/v1/namespaces/" + job.Namespace + "/jobs/" + job.ID
}

func collectionLocation(collection domain.Collection) string {
	return "/v1/namespaces/" + collection.Namespace + "/collections/" + collection.ID
}

func graphLocation(graph domain.Graph) string {
	return "/v1/namespaces/" + graph.Namespace + "/graphs/" + graph.ID
}

func revisionETag(revision int64) string {
	return `"revision-` + strconv.FormatInt(revision, 10) + `"`
}

type jobResponse struct {
	APIVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	Metadata   jobMetadata `json:"metadata"`
	Spec       jobSpec     `json:"spec"`
	Status     jobStatus   `json:"status"`
}

type jobListResponse struct {
	APIVersion    string        `json:"apiVersion"`
	Kind          string        `json:"kind"`
	Items         []jobResponse `json:"items"`
	NextPageToken string        `json:"nextPageToken,omitempty"`
}

type jobMetadata struct {
	ID        string            `json:"id"`
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels,omitempty"`
	Revision  int64             `json:"revision"`
	CreatedAt time.Time         `json:"createdAt"`
	UpdatedAt time.Time         `json:"updatedAt"`
}

type jobSpec struct {
	WorkloadDigest string       `json:"workloadDigest"`
	Placement      jobPlacement `json:"placement"`
}

type jobPlacement struct {
	Target             string `json:"target"`
	Partition          string `json:"partition,omitempty"`
	TargetID           string `json:"targetId,omitempty"`
	TargetGenerationID string `json:"targetGenerationId,omitempty"`
	ExecutionBackend   string `json:"executionBackend,omitempty"`
}

type jobStatus struct {
	Phase                 string              `json:"phase"`
	DesiredState          string              `json:"desiredState"`
	Outcome               string              `json:"outcome,omitempty"`
	ObservationConfidence string              `json:"observationConfidence,omitempty"`
	ConfidenceUpdatedAt   *time.Time          `json:"confidenceUpdatedAt,omitempty"`
	NativeID              string              `json:"nativeId,omitempty"`
	Scheduler             *jobSchedulerStatus `json:"scheduler,omitempty"`
}

type jobSchedulerStatus struct {
	Backend    string    `json:"backend"`
	State      string    `json:"state"`
	Reason     string    `json:"reason,omitempty"`
	Cluster    string    `json:"cluster,omitempty"`
	ObservedAt time.Time `json:"observedAt"`
}

type collectionResponse struct {
	APIVersion string             `json:"apiVersion"`
	Kind       string             `json:"kind"`
	Metadata   collectionMetadata `json:"metadata"`
	Spec       collectionSpec     `json:"spec"`
	Status     collectionStatus   `json:"status"`
	Items      []collectionItem   `json:"items"`
}

type collectionMetadata struct {
	ID        string            `json:"id"`
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels,omitempty"`
	Revision  int64             `json:"revision"`
	CreatedAt time.Time         `json:"createdAt"`
	UpdatedAt time.Time         `json:"updatedAt"`
}

type collectionSpec struct {
	MaxActive     int    `json:"maxActive"`
	FailurePolicy string `json:"failurePolicy"`
	ArrayPolicy   string `json:"arrayPolicy"`
}

type collectionStatus struct {
	Phase     string `json:"phase"`
	Outcome   string `json:"outcome,omitempty"`
	ArrayMode string `json:"arrayMode"`
	Total     int    `json:"total"`
	Active    int    `json:"active"`
	Terminal  int    `json:"terminal"`
	Succeeded int    `json:"succeeded"`
	Failed    int    `json:"failed"`
	Canceled  int    `json:"cancelled"` //nolint:misspell // Frozen v1alpha1 wire field.
}

type collectionItem struct {
	Index int         `json:"index"`
	Name  string      `json:"name"`
	Job   jobResponse `json:"job"`
}

type graphResponse struct {
	APIVersion string        `json:"apiVersion"`
	Kind       string        `json:"kind"`
	Metadata   graphMetadata `json:"metadata"`
	Spec       graphSpec     `json:"spec"`
	Status     graphStatus   `json:"status"`
	Items      []graphItem   `json:"items"`
}

type graphMetadata struct {
	ID        string            `json:"id"`
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels,omitempty"`
	Revision  int64             `json:"revision"`
	CreatedAt time.Time         `json:"createdAt"`
	UpdatedAt time.Time         `json:"updatedAt"`
}

type graphSpec struct {
	MaxActive         int    `json:"maxActive"`
	UnsatisfiedPolicy string `json:"unsatisfiedPolicy"`
}

type graphStatus struct {
	Phase     string `json:"phase"`
	Outcome   string `json:"outcome,omitempty"`
	Total     int    `json:"total"`
	Waiting   int    `json:"waiting"`
	Active    int    `json:"active"`
	Terminal  int    `json:"terminal"`
	Succeeded int    `json:"succeeded"`
	Failed    int    `json:"failed"`
	Canceled  int    `json:"cancelled"` //nolint:misspell // Frozen v1alpha1 wire spelling.
	Skipped   int    `json:"skipped"`
	Blocked   int    `json:"blocked"`
}

type graphDependency struct {
	From      string   `json:"from"`
	Predicate string   `json:"predicate"`
	Outcomes  []string `json:"outcomes,omitempty"`
	Satisfied bool     `json:"satisfied"`
}

type graphItem struct {
	Index        int               `json:"index"`
	Name         string            `json:"name"`
	Disposition  string            `json:"disposition,omitempty"`
	Dependencies []graphDependency `json:"dependencies,omitempty"`
	Job          jobResponse       `json:"job"`
}

type errorResponse struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func newJobResponse(job domain.Job) jobResponse {
	response := jobResponse{
		APIVersion: apiVersion,
		Kind:       "Job",
		Metadata: jobMetadata{
			ID:        job.ID,
			Namespace: job.Namespace,
			Name:      job.Name,
			Labels:    job.Labels,
			Revision:  job.Revision,
			CreatedAt: job.CreatedAt,
			UpdatedAt: job.UpdatedAt,
		},
		Spec: jobSpec{
			WorkloadDigest: job.WorkloadDigest,
			Placement: jobPlacement{
				Target: job.Target, Partition: job.Partition,
				TargetID: job.TargetID, TargetGenerationID: job.TargetGenerationID,
				ExecutionBackend: job.ExecutionBackend,
			},
		},
		Status: jobStatus{
			Phase: job.Phase, DesiredState: job.DesiredState,
			Outcome: job.Outcome, ObservationConfidence: job.ObservationConfidence,
			NativeID: job.NativeID,
		},
	}
	if !job.ConfidenceUpdatedAt.IsZero() {
		value := job.ConfidenceUpdatedAt
		response.Status.ConfidenceUpdatedAt = &value
	}
	if job.Scheduler != nil {
		response.Status.Scheduler = &jobSchedulerStatus{
			Backend: job.Scheduler.Backend, State: job.Scheduler.State,
			Reason: job.Scheduler.Reason, Cluster: job.Scheduler.Cluster,
			ObservedAt: job.Scheduler.ObservedAt,
		}
	}

	return response
}

func newCollectionResponse(collection domain.Collection) collectionResponse {
	items := make([]collectionItem, 0, len(collection.Items))
	for _, item := range collection.Items {
		items = append(items, collectionItem{
			Index: item.Index, Name: item.Name, Job: newJobResponse(item.Job),
		})
	}

	return collectionResponse{
		APIVersion: apiVersion, Kind: "Collection",
		Metadata: collectionMetadata{
			ID: collection.ID, Namespace: collection.Namespace, Name: collection.Name,
			Labels: collection.Labels, Revision: collection.Revision,
			CreatedAt: collection.CreatedAt, UpdatedAt: collection.UpdatedAt,
		},
		Spec: collectionSpec{
			MaxActive: collection.MaxActive, FailurePolicy: collection.FailurePolicy,
			ArrayPolicy: collection.ArrayPolicy,
		},
		Status: collectionStatus{
			Phase: collection.Phase, Outcome: collection.Outcome, ArrayMode: collection.ArrayMode,
			Total: collection.Total, Active: collection.Active, Terminal: collection.Terminal,
			Succeeded: collection.Succeeded, Failed: collection.Failed, Canceled: collection.Canceled,
		},
		Items: items,
	}
}

func newGraphResponse(graph domain.Graph) graphResponse {
	items := make([]graphItem, 0, len(graph.Items))
	for _, item := range graph.Items {
		dependencies := make([]graphDependency, 0, len(item.Dependencies))
		for _, dependency := range item.Dependencies {
			dependencies = append(dependencies, graphDependency{
				From: dependency.From, Predicate: dependency.Predicate,
				Outcomes: dependency.Outcomes, Satisfied: dependency.Satisfied,
			})
		}
		items = append(items, graphItem{
			Index: item.Index, Name: item.Name, Disposition: item.Disposition,
			Dependencies: dependencies, Job: newJobResponse(item.Job),
		})
	}

	return graphResponse{
		APIVersion: apiVersion, Kind: "Graph",
		Metadata: graphMetadata{
			ID: graph.ID, Namespace: graph.Namespace, Name: graph.Name,
			Labels: graph.Labels, Revision: graph.Revision,
			CreatedAt: graph.CreatedAt, UpdatedAt: graph.UpdatedAt,
		},
		Spec: graphSpec{MaxActive: graph.MaxActive, UnsatisfiedPolicy: graph.UnsatisfiedPolicy},
		Status: graphStatus{
			Phase: graph.Phase, Outcome: graph.Outcome, Total: graph.Total,
			Waiting: graph.Waiting, Active: graph.Active, Terminal: graph.Terminal,
			Succeeded: graph.Succeeded, Failed: graph.Failed, Canceled: graph.Canceled,
			Skipped: graph.Skipped, Blocked: graph.Blocked,
		},
		Items: items,
	}
}

func writeError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, errorResponse{Error: apiError{Code: code, Message: message}})
}

func writeUnauthenticated(writer http.ResponseWriter) {
	writer.Header().Set("WWW-Authenticate", `Bearer realm="jobman-control"`)
	writeError(writer, http.StatusUnauthorized, "unauthenticated", "valid authentication is required")
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		http.Error(writer, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)

		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if _, err = writer.Write(append(encoded, '\n')); err != nil {
		return
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Jobman-Control-Api-Version", apiVersion)
		next.ServeHTTP(writer, request)
	})
}
