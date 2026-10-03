package postgres

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/ryancswallace/jobman-control/internal/auth"
	"github.com/ryancswallace/jobman-control/internal/domain"
	"github.com/ryancswallace/jobman-control/internal/httpapi"
)

// Called only inside the explicitly enabled isolated PostgreSQL integration
// fixture. Both the TLS certificates and asserted user are synthetic.
func exerciseDelegationHTTP(ctx context.Context, t *testing.T, store *Store, existing domain.DelegationKey, principal domain.Principal, jobID string) {
	t.Helper()
	now := time.Now().UTC()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(der)
	fingerprint := base64.RawURLEncoding.EncodeToString(hash[:])
	registration := existing
	registration.KeyID = "http-test-key"
	registration.PublicKey = public
	registration.CertificateThumbprints = []string{fingerprint}
	if err = store.RegisterDelegationKeys(ctx, []domain.DelegationKey{existing, registration}); err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: private}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", registration.KeyID))
	if err != nil {
		t.Fatal(err)
	}
	mint := func(operation, subject string) string {
		t.Helper()
		var random [24]byte
		if _, randomErr := rand.Read(random[:]); randomErr != nil {
			t.Fatal(randomErr)
		}
		claims := struct {
			jwt.Claims
			Actor        map[string]string `json:"actor"`
			Confirmation map[string]string `json:"cnf"`
			Operation    string            `json:"operation"`
			NamespaceIDs []string          `json:"namespaceIds"`
			Mode         string            `json:"mode"`
		}{
			Claims: jwt.Claims{Issuer: registration.ServiceID, Subject: principal.Delegation.DirectoryID, Audience: jwt.Audience{registration.Audience}, IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(time.Minute)), ID: base64.RawURLEncoding.EncodeToString(random[:])},
			Actor:  map[string]string{"directoryId": principal.Delegation.DirectoryID, "issuer": principal.Issuer, "subject": subject}, Confirmation: map[string]string{"x5t#S256": fingerprint}, Operation: operation, NamespaceIDs: registration.NamespaceIDs, Mode: "interactive",
		}
		compact, signErr := jwt.Signed(signer).Claims(claims).Serialize()
		if signErr != nil {
			t.Fatal(signErr)
		}
		return auth.DelegationScheme + " " + compact
	}
	handler, err := httpapi.New(httpapi.Options{Repository: store, Authenticator: auth.DevelopmentAuthenticator{Principal: domain.Principal{Issuer: "unused", Subject: "unused"}}, DelegationAuthenticator: &auth.DelegationAuthenticator{Registry: store}, MaxRequestBytes: 1024 * 1024, ReadinessTimeout: time.Second, EnrollmentLifetime: time.Minute, AgentSessionLifetime: time.Minute, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	clientCAs := x509.NewCertPool()
	clientCAs.AddCert(certificate)
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: clientCAs}
	server.StartTLS()
	defer server.Close()
	noCertificate := server.Client()
	noCertificate.Timeout = 5 * time.Second
	baseTransport, ok := noCertificate.Transport.(*http.Transport)
	if !ok {
		t.Fatal("unexpected test transport")
	}
	transport := baseTransport.Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.Certificates = []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: private}}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	request := func(sender *http.Client, method, path, authorization string, want int) map[string]any {
		t.Helper()
		req, requestErr := http.NewRequestWithContext(ctx, method, server.URL+path, http.NoBody)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		req.Header.Set("Authorization", authorization)
		response, requestErr := sender.Do(req)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		defer response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("delegated HTTP %s %s=%d, want %d", method, path, response.StatusCode, want)
		}
		var body map[string]any
		if decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(&body); decodeErr != nil {
			t.Fatal(decodeErr)
		}
		return body
	}
	path := "/v1/namespaces/research/jobs/" + jobID
	authorization := mint(domain.CapabilityJobsRead, principal.Subject)
	body := request(client, http.MethodGet, path, authorization, http.StatusOK)
	if body["kind"] != "Job" || body["asOf"] == nil {
		t.Fatalf("delegated job envelope=%v", body)
	}
	request(client, http.MethodGet, path, authorization, http.StatusUnauthorized)
	request(noCertificate, http.MethodGet, path, mint(domain.CapabilityJobsRead, principal.Subject), http.StatusUnauthorized)
	request(client, http.MethodGet, path, mint(domain.CapabilityJobsRead, "unverified-alias"), http.StatusForbidden)
	request(client, http.MethodPost, path+"/cancel", mint(domain.CapabilityJobsRead, principal.Subject), http.StatusUnauthorized)
	request(client, http.MethodGet, "/v1/namespaces/outside/jobs", mint(domain.CapabilityJobsRead, principal.Subject), http.StatusForbidden)
	request(client, http.MethodGet, "/v1/me", mint(domain.CapabilityJobsRead, principal.Subject), http.StatusUnauthorized)
	body = request(client, http.MethodGet, "/v1/me", mint(domain.CapabilityNamespaceRead, principal.Subject), http.StatusOK)
	identity, ok := body["principal"].(map[string]any)
	if !ok || identity["directoryId"] != principal.Delegation.DirectoryID {
		t.Fatalf("verified HTTP directory identity=%v", body)
	}
}
