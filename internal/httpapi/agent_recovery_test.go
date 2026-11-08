package httpapi

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ryancswallace/jobman-control/internal/agentca"
	"github.com/ryancswallace/jobman-control/internal/auth"
	"github.com/ryancswallace/jobman-control/internal/domain"
)

type recoveryRepository struct {
	fakeRepository
	recovery                                     domain.AgentRecovery
	recoveryResult                               domain.AgentRecoveryResult
	recoveryErr                                  error
	certificateErr                               error
	bearerCalls, certificateCalls, recoveryCalls int
}

func (r *recoveryRepository) RecoverAgent(_ context.Context, _ string, request domain.AgentRecovery, _ time.Duration) (domain.AgentRecoveryResult, error) {
	r.recoveryCalls++
	r.recovery = request
	return r.recoveryResult, r.recoveryErr
}

func (r *recoveryRepository) RenewAgentSession(context.Context, string, time.Duration) (domain.AgentSession, error) {
	r.bearerCalls++
	return domain.AgentSession{}, nil
}

func (r *recoveryRepository) RenewAgentSessionWithCertificate(context.Context, domain.AgentIdentity, string, time.Duration) (domain.AgentSession, error) {
	r.certificateCalls++
	return domain.AgentSession{}, nil
}

func (r *recoveryRepository) AuthenticateAgentCertificate(context.Context, string, string, string) (domain.AgentIdentity, error) {
	return r.agentIdentity, r.certificateErr
}

func TestSessionRenewalAuthenticationDispatch(t *testing.T) {
	for _, mode := range []string{"bearer", "verified certificate", "unverified certificate", "revoked certificate"} {
		t.Run(mode, func(t *testing.T) {
			r := &recoveryRepository{}
			r.agentIdentity.AgentID = testJobID
			handler := newTestHandler(t, r, 65536)
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/agent/session/renew", http.NoBody)
			request.Header.Set("Authorization", "Jobman-Agent synthetic-token")
			if mode != "bearer" {
				request.TLS = testAgentTLSState(t, testJobID)
			}
			if mode == "unverified certificate" {
				request.TLS.VerifiedChains = nil
			}
			if mode == "revoked certificate" {
				r.certificateErr = domain.ErrUnauthenticated
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			switch mode {
			case "bearer":
				if response.Code != 200 || r.bearerCalls != 1 || r.certificateCalls != 0 {
					t.Fatal("legacy dispatch changed")
				}
			case "verified certificate":
				if response.Code != 200 || r.bearerCalls != 0 || r.certificateCalls != 1 {
					t.Fatal("verified mTLS not dispatched")
				}
			default:
				if response.Code != 401 || r.bearerCalls != 0 || r.certificateCalls != 0 {
					t.Fatal("invalid certificate fell back to bearer")
				}
			}
		})
	}
}

func recoveryTestAuthority(t *testing.T) (testAuthority *agentca.Authority, testCSR string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Synthetic test CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := agentca.Parse(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	agentKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, agentKey)
	if err != nil {
		t.Fatal(err)
	}
	return authority, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}))
}

func TestRecoverAgentHTTPContract(t *testing.T) {
	authority, csr := recoveryTestAuthority(t)
	repository := &recoveryRepository{recoveryResult: domain.AgentRecoveryResult{Session: domain.AgentSession{AgentID: testJobID, SessionID: testJobID, Token: "synthetic-result", ExpiresAt: time.Now().Add(time.Minute)}, Certificate: domain.AgentCertificate{Serial: "2002", ExpiresAt: time.Now().Add(time.Hour)}, CertificatePEM: "saved-public-cert", CACertificatePEM: "saved-public-ca"}}
	handler, err := New(Options{Repository: repository, Authenticator: auth.DevelopmentAuthenticator{Principal: domain.Principal{Issuer: "synthetic", Subject: "owner"}}, MaxRequestBytes: 131072, ReadinessTimeout: time.Second, EnrollmentLifetime: 10 * time.Minute, AgentSessionLifetime: 15 * time.Minute, CertificateAuthority: authority, AgentCertificateLifetime: 2 * time.Hour, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	var document agentRecoveryRequest
	document.APIVersion = apiVersion
	document.Kind = "AgentRecovery"
	document.Metadata.AgentID = testJobID
	document.Spec.TargetGenerationID = testJobID
	document.Spec.PreviousCertificateSerial = "1001"
	document.Spec.ExecutionUser = "worker"
	document.Spec.CertificateSigningRequest = csr
	send := func(doc agentRecoveryRequest, header string) *httptest.ResponseRecorder {
		t.Helper()
		encoded, encodeErr := json.Marshal(doc)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/agent/recover", bytes.NewReader(encoded))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", header)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	initial := send(document, "Jobman-Enrollment synthetic-token")
	if initial.Code != 201 {
		t.Fatalf("initial status %d", initial.Code)
	}
	if repository.recovery.AgentID != testJobID || repository.recovery.ExecutionUser != "worker" || repository.recovery.PreviousCertificateSerial != "1001" || repository.recovery.Certificate.PublicKeyDigest == "" {
		t.Fatal("recovery binding lost")
	}
	repository.recoveryResult.Session.Replayed = true
	replay := send(document, "Jobman-Enrollment synthetic-token")
	if replay.Code != 200 || replay.Header().Get("Idempotency-Replayed") != "true" || !bytes.Equal(initial.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatal("replay response changed")
	}
	var response agentSessionResponse
	if err = json.Unmarshal(replay.Body.Bytes(), &response); err != nil || response.Spec.InertOnly || response.Spec.Certificate == nil || response.Spec.Certificate.Serial != "2002" {
		t.Fatal("recovery certificate envelope invalid")
	}
	for _, test := range []struct {
		name   string
		header string
		mutate func(*agentRecoveryRequest)
		want   int
	}{
		{"missing authority", "", func(*agentRecoveryRequest) {}, 401},
		{"bearer not enrollment", "Bearer synthetic", func(*agentRecoveryRequest) {}, 401},
		{"invalid agent", "Jobman-Enrollment synthetic", func(d *agentRecoveryRequest) { d.Metadata.AgentID = "invalid" }, 400},
		{"missing user", "Jobman-Enrollment synthetic", func(d *agentRecoveryRequest) { d.Spec.ExecutionUser = "" }, 400},
		{"bad CSR", "Jobman-Enrollment synthetic", func(d *agentRecoveryRequest) { d.Spec.CertificateSigningRequest = "invalid" }, 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := repository.recoveryCalls
			doc := document
			test.mutate(&doc)
			got := send(doc, test.header)
			if got.Code != test.want || repository.recoveryCalls != before {
				t.Fatalf("invalid request reached repository: status%d", got.Code)
			}
		})
	}
	repository.recoveryErr = domain.ErrForbidden
	if got := send(document, "Jobman-Enrollment synthetic"); got.Code != 403 {
		t.Fatal("repository rejection ignored")
	}
	repository.recoveryErr = errors.New("synthetic internal detail")
	if got := send(document, "Jobman-Enrollment synthetic"); got.Code != 500 || bytes.Contains(got.Body.Bytes(), []byte("synthetic internal detail")) {
		t.Fatal("internal recovery error exposed")
	}
}
