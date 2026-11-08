package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

type agentRecoveryRequest struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		AgentID string `json:"agentId"`
	} `json:"metadata"`
	Spec struct {
		TargetGenerationID        string `json:"targetGenerationId"`
		PreviousCertificateSerial string `json:"previousCertificateSerial"`
		ExecutionUser             string `json:"executionUser"`
		CertificateSigningRequest string `json:"certificateSigningRequest"`
	} `json:"spec"`
}

func (service *api) recoverAgent(writer http.ResponseWriter, request *http.Request) {
	token, valid := authorizationToken(request.Header, "Jobman-Enrollment")
	if !valid {
		writeAgentUnauthenticated(writer, "Jobman-Enrollment")
		return
	}
	if service.certificateAuthority == nil {
		writeError(writer, http.StatusServiceUnavailable, "agent_mtls_unavailable", "agent mTLS is not configured")
		return
	}
	var document agentRecoveryRequest
	if _, err := service.decodeControlJSON(writer, request, &document); err != nil {
		service.writeDecodeError(writer, err)
		return
	}
	if document.APIVersion != apiVersion || document.Kind != "AgentRecovery" ||
		!domain.IsID(document.Metadata.AgentID) || !domain.IsID(document.Spec.TargetGenerationID) ||
		len(document.Spec.PreviousCertificateSerial) < 1 || len(document.Spec.PreviousCertificateSerial) > 64 ||
		strings.TrimSpace(document.Spec.ExecutionUser) == "" || len(document.Spec.ExecutionUser) > 512 ||
		len(document.Spec.CertificateSigningRequest) > 65536 {
		writeError(writer, http.StatusBadRequest, "invalid_request", "agent recovery request is invalid")
		return
	}
	digest, err := semanticDigest(document)
	if err != nil {
		service.writeRepositoryError(writer, request, "digest agent recovery", err)
		return
	}
	// Signing is local, before the transaction. No credential is returned or
	// registered until fresh enrollment authority passes the atomic checks.
	credential, err := service.certificateAuthority.Issue(document.Spec.CertificateSigningRequest, document.Metadata.AgentID, service.agentCertificateLifetime)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_certificate_request", "agent certificate request is invalid")
		return
	}
	result, err := service.repository.RecoverAgent(request.Context(), token, domain.AgentRecovery{
		AgentID: document.Metadata.AgentID, TargetGenerationID: document.Spec.TargetGenerationID,
		PreviousCertificateSerial: document.Spec.PreviousCertificateSerial, ExecutionUser: document.Spec.ExecutionUser,
		RequestDigest: digest, Certificate: domain.AgentCertificate{Serial: credential.Serial, PublicKeyDigest: credential.PublicKeyDigest, ExpiresAt: credential.ExpiresAt.Truncate(time.Second)},
		CertificatePEM: credential.CertificatePEM, CACertificatePEM: credential.CACertificatePEM,
	}, service.agentSessionLifetime)
	if err != nil {
		service.writeAgentRepositoryError(writer, request, "recover agent", err)
		return
	}
	response := newAgentSessionResponse(result.Session)
	response.Spec.InertOnly = false
	response.Spec.Certificate = &agentCertificateResponse{CertificatePEM: result.CertificatePEM, CACertificatePEM: result.CACertificatePEM, Serial: result.Certificate.Serial, ExpiresAt: result.Certificate.ExpiresAt}
	status := http.StatusCreated
	if result.Session.Replayed {
		status = http.StatusOK
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(writer, status, response)
}
