package httpapi

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	jobmanprotocol "github.com/ryancswallace/jobman-control/contracts/jobman/v1alpha1"
	"github.com/ryancswallace/jobman-control/internal/agentca"
	"github.com/ryancswallace/jobman-control/internal/domain"
)

type agentEnrollmentRequest struct {
	APIVersion string                     `json:"apiVersion"`
	Kind       string                     `json:"kind"`
	Spec       agentEnrollmentRequestSpec `json:"spec"`
}

type agentEnrollmentRequestSpec struct {
	TargetGenerationID        string    `json:"targetGenerationId"`
	AgentVersion              string    `json:"agentVersion"`
	ProtocolVersions          []string  `json:"protocolVersions"`
	Host                      agentHost `json:"host"`
	ExecutionUser             string    `json:"executionUser"`
	ExecutionBackends         []string  `json:"executionBackends"`
	Runtimes                  []string  `json:"runtimes"`
	Capabilities              []string  `json:"capabilities"`
	CertificateSigningRequest string    `json:"certificateSigningRequest,omitempty"`
}

type agentHost struct {
	OperatingSystem string `json:"operatingSystem"`
	Architecture    string `json:"architecture"`
	Hostname        string `json:"hostname"`
}

type agentCapabilitiesRequest struct {
	APIVersion string                           `json:"apiVersion"`
	Kind       string                           `json:"kind"`
	Metadata   agentCapabilitiesRequestMetadata `json:"metadata"`
	Spec       agentCapabilitiesRequestSpec     `json:"spec"`
}

type agentCapabilitiesRequestMetadata struct {
	AgentID    string    `json:"agentId"`
	ObservedAt time.Time `json:"observedAt"`
}

type agentCapabilitiesRequestSpec struct {
	AcceptingAssignments bool      `json:"acceptingAssignments"`
	AgentVersion         string    `json:"agentVersion"`
	Host                 agentHost `json:"host"`
	ExecutionUser        string    `json:"executionUser"`
	ExecutionBackends    []string  `json:"executionBackends"`
	Runtimes             []string  `json:"runtimes"`
	Capabilities         []string  `json:"capabilities"`
}

func (service *api) enrollAgent(writer http.ResponseWriter, request *http.Request) {
	token, valid := authorizationToken(request.Header, "Jobman-Enrollment")
	if !valid {
		writeAgentUnauthenticated(writer, "Jobman-Enrollment")
		return
	}
	var document agentEnrollmentRequest
	_, err := service.decodeControlJSON(writer, request, &document)
	if err != nil {
		service.writeDecodeError(writer, err)
		return
	}
	normalizeAgentEnrollment(&document)
	digest, err := semanticDigest(document)
	if err != nil {
		service.writeRepositoryError(writer, request, "digest agent enrollment", err)
		return
	}
	registration := domain.AgentRegistration{
		TargetGenerationID: document.Spec.TargetGenerationID,
		AgentVersion:       document.Spec.AgentVersion,
		ProtocolVersions:   document.Spec.ProtocolVersions,
		OperatingSystem:    document.Spec.Host.OperatingSystem,
		Architecture:       document.Spec.Host.Architecture,
		Hostname:           document.Spec.Host.Hostname,
		ExecutionUser:      document.Spec.ExecutionUser,
		ExecutionBackends:  document.Spec.ExecutionBackends,
		Runtimes:           document.Spec.Runtimes,
		Capabilities:       document.Spec.Capabilities,
		RequestDigest:      digest,
	}
	if document.APIVersion != apiVersion || document.Kind != "AgentEnrollment" ||
		domain.ValidateAgentRegistration(registration) != nil ||
		(service.certificateAuthority != nil && document.Spec.CertificateSigningRequest == "") {
		writeError(writer, http.StatusBadRequest, "invalid_request", "agent enrollment request is invalid")
		return
	}
	session, err := service.repository.EnrollAgent(
		request.Context(), token, registration, service.agentSessionLifetime,
	)
	if err != nil {
		service.writeAgentRepositoryError(writer, request, "enroll agent", err)
		return
	}
	status := http.StatusCreated
	if session.Replayed {
		status = http.StatusOK
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	response := newAgentSessionResponse(session)
	if service.certificateAuthority != nil {
		credential, issueErr := service.certificateAuthority.Issue(
			document.Spec.CertificateSigningRequest, session.AgentID, service.agentCertificateLifetime,
		)
		if issueErr != nil {
			writeError(writer, http.StatusBadRequest, "invalid_certificate_request", "agent certificate request is invalid")
			return
		}
		if recordErr := service.repository.RecordAgentCertificate(
			request.Context(), session.AgentID,
			domain.AgentCertificate{
				Serial: credential.Serial, PublicKeyDigest: credential.PublicKeyDigest,
				ExpiresAt: credential.ExpiresAt,
			},
		); recordErr != nil {
			service.writeAgentRepositoryError(writer, request, "record agent certificate", recordErr)
			return
		}
		response.Spec.Certificate = &agentCertificateResponse{
			CertificatePEM:   credential.CertificatePEM,
			CACertificatePEM: credential.CACertificatePEM,
			Serial:           credential.Serial, ExpiresAt: credential.ExpiresAt,
		}
		response.Spec.InertOnly = false
	}
	writeJSON(writer, status, response)
}

func (service *api) renewAgentSession(writer http.ResponseWriter, request *http.Request) {
	token, valid := authorizationToken(request.Header, "Jobman-Agent")
	if !valid {
		writeAgentUnauthenticated(writer, "Jobman-Agent")
		return
	}
	session, err := service.repository.RenewAgentSession(
		request.Context(), token, service.agentSessionLifetime,
	)
	if err != nil {
		service.writeAgentRepositoryError(writer, request, "renew agent session", err)
		return
	}
	writeJSON(writer, http.StatusOK, newAgentSessionResponse(session))
}

func (service *api) listAgentAssignments(writer http.ResponseWriter, request *http.Request) {
	limit, valid := readLimit(request, 25)
	if !valid {
		writeError(writer, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 100")
		return
	}
	identity, err := service.authenticateAgentCertificate(request)
	if err != nil {
		service.writeAgentRepositoryError(writer, request, "authenticate agent", err)
		return
	}
	assignments, err := service.repository.ListAssignments(request.Context(), identity, limit)
	if err != nil {
		service.writeAgentRepositoryError(writer, request, "list assignments", err)
		return
	}
	items := make([]json.RawMessage, 0, len(assignments))
	for _, assignment := range assignments {
		items = append(items, assignment.Document)
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"apiVersion":         apiVersion,
		"kind":               "AgentAssignmentList",
		"requiresAcceptance": true,
		"items":              items,
	})
}

func normalizeAgentEnrollment(document *agentEnrollmentRequest) {
	sort.Strings(document.Spec.ProtocolVersions)
	sort.Strings(document.Spec.ExecutionBackends)
	sort.Strings(document.Spec.Runtimes)
	sort.Strings(document.Spec.Capabilities)
}

func (service *api) recordAgentCapabilities(writer http.ResponseWriter, request *http.Request) {
	identity, err := service.authenticateAgentCertificate(request)
	if err != nil {
		service.writeAgentRepositoryError(writer, request, "authenticate agent certificate", err)
		return
	}
	var document agentCapabilitiesRequest
	if _, err = service.decodeControlJSON(writer, request, &document); err != nil {
		service.writeDecodeError(writer, err)
		return
	}
	sort.Strings(document.Spec.ExecutionBackends)
	sort.Strings(document.Spec.Runtimes)
	sort.Strings(document.Spec.Capabilities)
	digest, err := semanticDigest(document)
	if err != nil {
		service.writeAgentRepositoryError(writer, request, "digest agent capabilities", err)
		return
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		service.writeAgentRepositoryError(writer, request, "encode agent capabilities", err)
		return
	}
	capabilities := domain.AgentCapabilities{
		AgentID: document.Metadata.AgentID, ObservedAt: document.Metadata.ObservedAt.UTC(),
		AcceptingAssignments: document.Spec.AcceptingAssignments,
		AgentVersion:         document.Spec.AgentVersion,
		OperatingSystem:      document.Spec.Host.OperatingSystem,
		Architecture:         document.Spec.Host.Architecture, Hostname: document.Spec.Host.Hostname,
		ExecutionUser:     document.Spec.ExecutionUser,
		ExecutionBackends: document.Spec.ExecutionBackends, Runtimes: document.Spec.Runtimes,
		Capabilities: document.Spec.Capabilities, DocumentDigest: digest, Document: encoded,
	}
	if document.APIVersion != apiVersion || document.Kind != "AgentCapabilities" ||
		document.Metadata.AgentID != identity.AgentID || domain.ValidateAgentCapabilities(capabilities) != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request", "agent capabilities are invalid")
		return
	}
	snapshot, err := service.repository.RecordAgentCapabilities(request.Context(), identity, capabilities)
	if err != nil {
		service.writeAgentRepositoryError(writer, request, "record agent capabilities", err)
		return
	}
	if snapshot.Replayed {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"apiVersion": apiVersion, "kind": "AgentCapabilityReceipt",
		"agentId": identity.AgentID, "revision": snapshot.Revision,
		"observedAt": capabilities.ObservedAt,
	})
}

func authorizationToken(header http.Header, scheme string) (string, bool) {
	values := header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	actualScheme, token, found := strings.Cut(values[0], " ")
	if !found || actualScheme != scheme || token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}

	return token, true
}

func writeAgentUnauthenticated(writer http.ResponseWriter, scheme string) {
	writer.Header().Set("WWW-Authenticate", scheme)
	writeError(writer, http.StatusUnauthorized, "unauthenticated", "valid agent authentication is required")
}

func (service *api) writeAgentRepositoryError(
	writer http.ResponseWriter,
	request *http.Request,
	operation string,
	err error,
) {
	switch {
	case errors.Is(err, domain.ErrUnauthenticated):
		writeAgentUnauthenticated(writer, "Mutual")
	case errors.Is(err, domain.ErrForbidden):
		writeError(writer, http.StatusForbidden, "forbidden", "agent is not authorized for this resource")
	case errors.Is(err, domain.ErrNotFound):
		writeError(writer, http.StatusNotFound, "not_found", "agent resource was not found")
	case errors.Is(err, domain.ErrConflict):
		writeError(writer, http.StatusConflict, "conflict", "agent credential or target state conflicts with the request")
	case errors.Is(err, domain.ErrIdempotencyConflict):
		writeError(writer, http.StatusConflict, "idempotency_conflict", "enrollment token was already used with different registration facts")
	default:
		service.logger.ErrorContext(request.Context(), "agent API repository operation failed", "operation", operation)
		writeError(writer, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}

func (service *api) acceptAgentAssignment(writer http.ResponseWriter, request *http.Request) {
	if !isJSONContentType(request.Header.Get("Content-Type")) {
		writeError(writer, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return
	}
	identity, err := service.authenticateAgentCertificate(request)
	if err != nil {
		service.writeAgentRepositoryError(writer, request, "authenticate agent certificate", err)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, service.maxRequestBytes)
	sealed, err := jobmanprotocol.DecodeAgentAcceptance(request.Body, jobmanprotocol.DecodeLimits{})
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request", "agent acceptance is invalid")
		return
	}
	value := sealed.Document
	if value.Metadata.DeliveryID != request.PathValue("deliveryID") ||
		value.Metadata.AgentID != identity.AgentID {
		writeError(writer, http.StatusBadRequest, "identity_mismatch", "agent acceptance identities do not match the request")
		return
	}
	authorization, err := service.repository.AcceptAssignment(
		request.Context(), identity,
		domain.Acceptance{
			DeliveryID: value.Metadata.DeliveryID, ExecutionID: value.Metadata.ExecutionID,
			AgentID: value.Metadata.AgentID, TargetGenerationID: value.Spec.TargetGenerationID,
			EffectiveExecutionDigest: value.Spec.EffectiveExecutionDigest,
			RequestDigest:            sealed.Digest, RequestDocument: sealed.CanonicalJSON,
		},
	)
	if err != nil {
		service.writeAgentRepositoryError(writer, request, "accept assignment", err)
		return
	}
	response := jobmanprotocol.LaunchAuthorization{
		APIVersion: jobmanprotocol.V1Alpha1, Kind: jobmanprotocol.LaunchAuthorizationKind,
		Metadata: jobmanprotocol.LaunchAuthorizationMetadata{
			AuthorizationID: authorization.AuthorizationID,
			ExecutionID:     authorization.ExecutionID, AgentID: authorization.AgentID,
			Revision: authorization.Revision, AcceptedAt: authorization.AcceptedAt,
		},
		Spec: jobmanprotocol.LaunchAuthorizationSpec{
			TargetGenerationID:       authorization.TargetGenerationID,
			EffectiveExecutionDigest: authorization.EffectiveExecutionDigest,
		},
	}
	if err = jobmanprotocol.ValidateLaunchAuthorization(response); err != nil {
		service.writeAgentRepositoryError(writer, request, "encode launch authorization", err)
		return
	}
	if authorization.Replayed {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(writer, http.StatusOK, response)
}

func (service *api) recordAgentExecutionEvent(writer http.ResponseWriter, request *http.Request) {
	if !isJSONContentType(request.Header.Get("Content-Type")) {
		writeError(writer, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return
	}
	identity, err := service.authenticateAgentCertificate(request)
	if err != nil {
		service.writeAgentRepositoryError(writer, request, "authenticate agent certificate", err)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, service.maxRequestBytes)
	sealed, err := jobmanprotocol.DecodeExecutionEvent(request.Body, jobmanprotocol.DecodeLimits{})
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request", "execution event is invalid")
		return
	}
	value := sealed.Document
	if value.Metadata.ExecutionID != request.PathValue("executionID") ||
		value.Metadata.AgentID != identity.AgentID {
		writeError(writer, http.StatusBadRequest, "identity_mismatch", "execution event identities do not match the request")
		return
	}
	outcome := ""
	if value.Spec.Result != nil {
		outcome = value.Spec.Result.Outcome
	}
	schedulerBackend := ""
	schedulerState := ""
	schedulerReason := ""
	schedulerCluster := ""
	if value.Spec.Scheduler != nil {
		schedulerBackend = value.Spec.Scheduler.Backend
		schedulerState = value.Spec.Scheduler.State
		schedulerReason = value.Spec.Scheduler.Reason
		schedulerCluster = value.Spec.Scheduler.Cluster
	}
	replayed, err := service.repository.RecordExecutionEvent(
		request.Context(), identity,
		domain.ExecutionObservation{
			EventID: value.Metadata.EventID, ExecutionID: value.Metadata.ExecutionID,
			AgentID: value.Metadata.AgentID, Sequence: value.Metadata.Sequence,
			ObservedAt: value.Metadata.ObservedAt, Type: value.Spec.Type,
			NativeID: value.Spec.NativeID, Outcome: outcome,
			SchedulerBackend: schedulerBackend, SchedulerState: schedulerState,
			SchedulerReason: schedulerReason, SchedulerCluster: schedulerCluster,
			DocumentDigest: sealed.Digest, Document: sealed.CanonicalJSON,
		},
	)
	if err != nil {
		service.writeAgentRepositoryError(writer, request, "record execution event", err)
		return
	}
	if replayed {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"apiVersion": apiVersion, "kind": "ExecutionEventReceipt",
		"eventId": value.Metadata.EventID, "sequence": value.Metadata.Sequence,
	})
}

func (service *api) listAgentActions(writer http.ResponseWriter, request *http.Request) {
	identity, err := service.authenticateAgentCertificate(request)
	if err != nil {
		service.writeAgentRepositoryError(writer, request, "authenticate agent certificate", err)
		return
	}
	limit, valid := readLimit(request, 25)
	if !valid {
		writeError(writer, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 100")
		return
	}
	actions, err := service.repository.ListDesiredActions(request.Context(), identity, limit)
	if err != nil {
		service.writeAgentRepositoryError(writer, request, "list desired actions", err)
		return
	}
	items := make([]json.RawMessage, 0, len(actions))
	for _, action := range actions {
		items = append(items, action.Document)
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"apiVersion": apiVersion, "kind": "DesiredActionList", "items": items,
	})
}

func (service *api) acknowledgeAgentAction(writer http.ResponseWriter, request *http.Request) {
	identity, err := service.authenticateAgentCertificate(request)
	if err != nil {
		service.writeAgentRepositoryError(writer, request, "authenticate agent certificate", err)
		return
	}
	var acknowledgement jobmanprotocol.ActionAcknowledgement
	if _, err = service.decodeControlJSON(writer, request, &acknowledgement); err != nil {
		service.writeDecodeError(writer, err)
		return
	}
	if jobmanprotocol.ValidateActionAcknowledgement(acknowledgement) != nil ||
		acknowledgement.Metadata.ActionID != request.PathValue("actionID") ||
		acknowledgement.Metadata.AgentID != identity.AgentID {
		writeError(writer, http.StatusBadRequest, "invalid_request", "action acknowledgement is invalid")
		return
	}
	replayed, err := service.repository.AcknowledgeDesiredAction(
		request.Context(), identity,
		domain.ActionAcknowledgement{
			ActionID:    acknowledgement.Metadata.ActionID,
			ExecutionID: acknowledgement.Metadata.ExecutionID,
			AgentID:     acknowledgement.Metadata.AgentID,
			Revision:    acknowledgement.Metadata.Revision,
			ObservedAt:  acknowledgement.Metadata.ObservedAt,
		},
	)
	if err != nil {
		service.writeAgentRepositoryError(writer, request, "acknowledge desired action", err)
		return
	}
	if replayed {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writer.WriteHeader(http.StatusNoContent)
}

type agentSessionResponse struct {
	APIVersion string               `json:"apiVersion"`
	Kind       string               `json:"kind"`
	Metadata   agentSessionMetadata `json:"metadata"`
	Spec       agentSessionSpec     `json:"spec"`
}

type agentSessionMetadata struct {
	AgentID   string    `json:"agentId"`
	SessionID string    `json:"sessionId"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type agentSessionSpec struct {
	Token       string                    `json:"token"`
	AuthScheme  string                    `json:"authScheme"`
	InertOnly   bool                      `json:"inertOnly"`
	Certificate *agentCertificateResponse `json:"certificate,omitempty"`
}

type agentCertificateResponse struct {
	CertificatePEM   string    `json:"certificatePem"`
	CACertificatePEM string    `json:"caCertificatePem"`
	Serial           string    `json:"serial"`
	ExpiresAt        time.Time `json:"expiresAt"`
}

func newAgentSessionResponse(value domain.AgentSession) agentSessionResponse {
	return agentSessionResponse{
		APIVersion: apiVersion, Kind: "AgentSession",
		Metadata: agentSessionMetadata{
			AgentID: value.AgentID, SessionID: value.SessionID, ExpiresAt: value.ExpiresAt,
		},
		Spec: agentSessionSpec{
			Token: value.Token, AuthScheme: "Jobman-Agent", InertOnly: true,
		},
	}
}

type agentCertificateRenewalRequest struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Spec       struct {
		CertificateSigningRequest string `json:"certificateSigningRequest"`
	} `json:"spec"`
}

func (service *api) renewAgentCertificate(writer http.ResponseWriter, request *http.Request) {
	identity, err := service.authenticateAgentCertificate(request)
	if err != nil {
		service.writeAgentRepositoryError(writer, request, "authenticate agent certificate", err)
		return
	}
	if service.certificateAuthority == nil {
		writeError(writer, http.StatusServiceUnavailable, "agent_mtls_unavailable", "agent mTLS is not configured")
		return
	}
	var document agentCertificateRenewalRequest
	if _, err = service.decodeControlJSON(writer, request, &document); err != nil {
		service.writeDecodeError(writer, err)
		return
	}
	if document.APIVersion != apiVersion || document.Kind != "AgentCertificateRenewal" ||
		document.Spec.CertificateSigningRequest == "" {
		writeError(writer, http.StatusBadRequest, "invalid_request", "agent certificate renewal is invalid")
		return
	}
	credential, err := service.certificateAuthority.Issue(
		document.Spec.CertificateSigningRequest, identity.AgentID, service.agentCertificateLifetime,
	)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_certificate_request", "agent certificate request is invalid")
		return
	}
	if err = service.repository.RotateAgentCertificate(
		request.Context(), identity,
		domain.AgentCertificate{
			Serial: credential.Serial, PublicKeyDigest: credential.PublicKeyDigest,
			ExpiresAt: credential.ExpiresAt,
		},
	); err != nil {
		service.writeAgentRepositoryError(writer, request, "rotate agent certificate", err)
		return
	}
	writeJSON(writer, http.StatusOK, agentCertificateResponse{
		CertificatePEM:   credential.CertificatePEM,
		CACertificatePEM: credential.CACertificatePEM,
		Serial:           credential.Serial, ExpiresAt: credential.ExpiresAt,
	})
}

func (service *api) authenticateAgentCertificate(request *http.Request) (domain.AgentIdentity, error) {
	certificate, err := verifiedAgentCertificate(request)
	if err != nil {
		return domain.AgentIdentity{}, domain.ErrUnauthenticated
	}
	agentID, err := agentca.AgentID(certificate)
	if err != nil || !domain.IsID(agentID) {
		return domain.AgentIdentity{}, domain.ErrUnauthenticated
	}
	publicKeyDigest, err := agentca.PublicKeyDigest(certificate.PublicKey)
	if err != nil {
		return domain.AgentIdentity{}, domain.ErrUnauthenticated
	}

	return service.repository.AuthenticateAgentCertificate(
		request.Context(), agentID, certificate.SerialNumber.Text(16), publicKeyDigest,
	)
}

func verifiedAgentCertificate(request *http.Request) (*x509.Certificate, error) {
	if request.TLS == nil || len(request.TLS.VerifiedChains) == 0 ||
		len(request.TLS.VerifiedChains[0]) == 0 || len(request.TLS.PeerCertificates) == 0 {
		return nil, errors.New("verified agent certificate is required")
	}
	certificate := request.TLS.VerifiedChains[0][0]
	if certificate != request.TLS.PeerCertificates[0] {
		return nil, errors.New("verified agent certificate does not match peer")
	}

	return certificate, nil
}
