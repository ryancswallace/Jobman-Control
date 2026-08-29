package e2e_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ryancswallace/jobman-control/internal/app"
)

const controlAPIVersion = "jobman.control/v1alpha1"

func TestOIDCClientAndMTLSAgent(t *testing.T) {
	databaseURL := os.Getenv("JOBMAN_CONTROL_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("JOBMAN_CONTROL_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	databaseURL = isolatedDatabaseURL(ctx, t, databaseURL)

	issuer, token, oidcTransport := testOIDCProvider(t)
	previousDefaultTransport := http.DefaultTransport
	http.DefaultTransport = oidcTransport
	t.Cleanup(func() { http.DefaultTransport = previousDefaultTransport })

	temporaryDirectory := t.TempDir()
	serviceCA := newTestCA(t, "Jobman Control test server CA")
	serviceCertificatePEM, serviceKeyPEM := serviceCA.issueServerCertificate(t)
	serviceCertificateFile := writePrivateFile(t, temporaryDirectory, "server.crt", serviceCertificatePEM)
	serviceKeyFile := writePrivateFile(t, temporaryDirectory, "server.key", serviceKeyPEM)
	agentCA := newTestCA(t, "Jobman Control test agent CA")
	agentCACertificateFile := writePrivateFile(t, temporaryDirectory, "agent-ca.crt", agentCA.certificatePEM)
	agentCAKeyFile := writePrivateFile(t, temporaryDirectory, "agent-ca.key", agentCA.privateKeyPEM)

	listenAddress := availableLoopbackAddress(ctx, t)
	setServiceEnvironment(
		t, databaseURL, listenAddress, issuer,
		serviceCertificateFile, serviceKeyFile, agentCACertificateFile, agentCAKeyFile,
	)

	serviceContext, stopService := context.WithCancel(ctx)
	service := &serviceProcess{done: make(chan struct{})}
	go func() {
		defer close(service.done)
		service.err = app.Run(serviceContext, slog.New(slog.DiscardHandler))
	}()
	t.Cleanup(func() {
		stopService()
		select {
		case <-service.done:
			if service.err != nil {
				t.Errorf("Jobman Control shutdown error = %v", service.err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Jobman Control did not stop within 10 seconds")
		}
	})

	serviceRoots := x509.NewCertPool()
	if !serviceRoots.AppendCertsFromPEM(serviceCA.certificatePEM) {
		t.Fatal("add service CA certificate to client roots")
	}
	client := newTLSClient(serviceRoots)
	serviceURL := "https://" + listenAddress
	waitForReadiness(ctx, t, client, serviceURL, service)

	target := createTarget(ctx, t, client, serviceURL, token)
	enrollmentToken := createEnrollmentToken(
		ctx, t, client, serviceURL, token, issuer, target.GenerationID,
	)
	agentKey, certificatePEM := enrollAgent(
		ctx, t, client, serviceURL, enrollmentToken, target.GenerationID,
	)

	unauthenticated := requestJSON(
		ctx, t, client, http.MethodGet, serviceURL+"/v1/agent/assignments", "", "", nil,
	)
	if unauthenticated.status != http.StatusUnauthorized {
		t.Fatalf("agent endpoint without mTLS status = %d, want %d", unauthenticated.status, http.StatusUnauthorized)
	}

	agentCertificate, err := tls.X509KeyPair(certificatePEM, marshalPrivateKey(t, agentKey))
	if err != nil {
		t.Fatalf("load enrolled agent key pair: %v", err)
	}
	agentClient := newTLSClient(serviceRoots, agentCertificate)
	assignments := requestJSON(
		ctx, t, agentClient, http.MethodGet, serviceURL+"/v1/agent/assignments", "", "", nil,
	)
	if assignments.status != http.StatusOK {
		t.Fatalf("agent endpoint with mTLS status = %d, want %d", assignments.status, http.StatusOK)
	}
	var assignmentList struct {
		Kind string `json:"kind"`
	}
	decodeResponse(t, assignments.body, &assignmentList)
	if assignmentList.Kind != "AgentAssignmentList" {
		t.Fatalf("agent assignment response kind = %q", assignmentList.Kind)
	}
}

type targetIdentity struct {
	GenerationID string
}

type serviceProcess struct {
	done chan struct{}
	err  error
}

func createTarget(
	ctx context.Context,
	t *testing.T,
	client *http.Client,
	serviceURL string,
	token string,
) targetIdentity {
	t.Helper()
	document := map[string]any{
		"apiVersion": controlAPIVersion,
		"kind":       "Target",
		"metadata":   map[string]any{"name": "release-host"},
		"spec": map[string]any{
			"kind": "host", "executionBackend": "subprocess", "runtimes": []string{"native"},
			"operatingSystems": []string{"linux"}, "architectures": []string{"amd64"},
		},
	}
	response := requestJSON(
		ctx, t, client, http.MethodPost, serviceURL+"/v1/namespaces/research/targets",
		"Bearer "+token, "release-target-001", document,
	)
	if response.status != http.StatusCreated {
		t.Fatalf("OIDC target creation status = %d, want %d", response.status, http.StatusCreated)
	}
	var decoded struct {
		Metadata struct {
			GenerationID string `json:"generationId"`
		} `json:"metadata"`
	}
	decodeResponse(t, response.body, &decoded)
	if decoded.Metadata.GenerationID == "" {
		t.Fatal("created target has no generation ID")
	}

	return targetIdentity{GenerationID: decoded.Metadata.GenerationID}
}

func createEnrollmentToken(
	ctx context.Context,
	t *testing.T,
	client *http.Client,
	serviceURL string,
	identityToken string,
	issuer string,
	targetGenerationID string,
) string {
	t.Helper()
	document := map[string]any{
		"apiVersion": controlAPIVersion,
		"kind":       "AgentEnrollmentToken",
		"spec": map[string]any{
			"principal":    map[string]any{"issuer": issuer, "subject": "release-user"},
			"expectedUser": "release-user",
		},
	}
	response := requestJSON(
		ctx, t, client, http.MethodPost,
		serviceURL+"/v1/namespaces/research/targets/release-host/enrollment-tokens",
		"Bearer "+identityToken, "release-enrollment-001", document,
	)
	if response.status != http.StatusCreated {
		t.Fatalf("enrollment-token creation status = %d, want %d", response.status, http.StatusCreated)
	}
	var decoded struct {
		Spec struct {
			TargetGenerationID string `json:"targetGenerationId"`
			Token              string `json:"token"`
		} `json:"spec"`
	}
	decodeResponse(t, response.body, &decoded)
	if decoded.Spec.TargetGenerationID != targetGenerationID || decoded.Spec.Token == "" {
		t.Fatal("enrollment token is not bound to the created target generation")
	}

	return decoded.Spec.Token
}

func enrollAgent(
	ctx context.Context,
	t *testing.T,
	client *http.Client,
	serviceURL string,
	enrollmentToken string,
	targetGenerationID string,
) (privateKey *ecdsa.PrivateKey, certificatePEM []byte) {
	t.Helper()
	agentKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate agent key: %v", err)
	}
	requestDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "release-agent"},
	}, agentKey)
	if err != nil {
		t.Fatalf("create agent certificate request: %v", err)
	}
	requestPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: requestDER})
	document := map[string]any{
		"apiVersion": controlAPIVersion,
		"kind":       "AgentEnrollment",
		"spec": map[string]any{
			"targetGenerationId": targetGenerationID,
			"agentVersion":       "0.1.0",
			"protocolVersions":   []string{"jobman/v1alpha1"},
			"host": map[string]any{
				"operatingSystem": "linux", "architecture": "amd64", "hostname": "release-host",
			},
			"executionUser":             "release-user",
			"executionBackends":         []string{"subprocess"},
			"runtimes":                  []string{"native"},
			"capabilities":              []string{"process-groups"},
			"certificateSigningRequest": string(requestPEM),
		},
	}
	response := requestJSON(
		ctx, t, client, http.MethodPost, serviceURL+"/v1/agent/enroll",
		"Jobman-Enrollment "+enrollmentToken, "", document,
	)
	if response.status != http.StatusCreated {
		t.Fatalf("agent enrollment status = %d, want %d", response.status, http.StatusCreated)
	}
	var decoded struct {
		Metadata struct {
			AgentID string `json:"agentId"`
		} `json:"metadata"`
		Spec struct {
			Certificate struct {
				CertificatePEM string `json:"certificatePem"`
			} `json:"certificate"`
		} `json:"spec"`
	}
	decodeResponse(t, response.body, &decoded)
	if decoded.Metadata.AgentID == "" || decoded.Spec.Certificate.CertificatePEM == "" {
		t.Fatal("agent enrollment did not return an mTLS identity")
	}

	return agentKey, []byte(decoded.Spec.Certificate.CertificatePEM)
}

type jsonResponse struct {
	status int
	body   []byte
}

func requestJSON(
	ctx context.Context,
	t *testing.T,
	client *http.Client,
	method string,
	requestURL string,
	authorization string,
	idempotencyKey string,
	document any,
) jsonResponse {
	t.Helper()
	var body io.Reader = http.NoBody
	if document != nil {
		encoded, err := json.Marshal(document)
		if err != nil {
			t.Fatalf("encode request document: %v", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, requestURL, body)
	if err != nil {
		t.Fatalf("create HTTP request: %v", err)
	}
	if document != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("perform HTTP request: %v", err)
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil {
			t.Errorf("close HTTP response: %v", closeErr)
		}
	}()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024))
	if err != nil {
		t.Fatalf("read HTTP response: %v", err)
	}

	return jsonResponse{status: response.StatusCode, body: responseBody}
}

func decodeResponse(t *testing.T, body []byte, destination any) {
	t.Helper()
	if err := json.Unmarshal(body, destination); err != nil {
		t.Fatalf("decode HTTP response: %v", err)
	}
}

func waitForReadiness(
	ctx context.Context,
	t *testing.T,
	client *http.Client,
	serviceURL string,
	service *serviceProcess,
) {
	t.Helper()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, serviceURL+"/readyz", http.NoBody)
		if err != nil {
			t.Fatalf("create readiness request: %v", err)
		}
		response, requestErr := client.Do(request)
		if requestErr == nil {
			closeErr := response.Body.Close()
			if closeErr != nil {
				t.Fatalf("close readiness response: %v", closeErr)
			}
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-service.done:
			t.Fatalf("Jobman Control stopped before readiness: %v", service.err)
		case <-ctx.Done():
			t.Fatalf("wait for Jobman Control readiness: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func newTLSClient(roots *x509.CertPool, certificates ...tls.Certificate) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			RootCAs:      roots,
			Certificates: certificates,
		}},
	}
}

func setServiceEnvironment(
	t *testing.T,
	databaseURL string,
	listenAddress string,
	issuer string,
	serviceCertificateFile string,
	serviceKeyFile string,
	agentCACertificateFile string,
	agentCAKeyFile string,
) {
	t.Helper()
	settings := map[string]string{
		"JOBMAN_CONTROL_DATABASE_URL":        databaseURL,
		"JOBMAN_CONTROL_AUTH_MODE":           "oidc",
		"JOBMAN_CONTROL_DEVELOPMENT_AUTH":    "false",
		"JOBMAN_CONTROL_LISTEN":              listenAddress,
		"JOBMAN_CONTROL_OIDC_ISSUER":         issuer,
		"JOBMAN_CONTROL_OIDC_AUDIENCE":       "jobman-control",
		"JOBMAN_CONTROL_AGENT_TOKEN_KEY":     base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32)),
		"JOBMAN_CONTROL_BOOTSTRAP_SUBJECT":   "release-user",
		"JOBMAN_CONTROL_BOOTSTRAP_NAME":      "Release User",
		"JOBMAN_CONTROL_BOOTSTRAP_NAMESPACE": "research",
		"JOBMAN_CONTROL_TLS_CERT_FILE":       serviceCertificateFile,
		"JOBMAN_CONTROL_TLS_KEY_FILE":        serviceKeyFile,
		"JOBMAN_CONTROL_AGENT_CA_CERT_FILE":  agentCACertificateFile,
		"JOBMAN_CONTROL_AGENT_CA_KEY_FILE":   agentCAKeyFile,
		"JOBMAN_CONTROL_MIGRATE_ON_START":    "true",
	}
	for name, value := range settings {
		t.Setenv(name, value)
	}
}

func testOIDCProvider(t *testing.T) (issuer, token string, transport http.RoundTripper) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate OIDC signing key: %v", err)
	}
	const keyID = "release-key"
	keySet := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &privateKey.PublicKey, KeyID: keyID, Algorithm: string(jose.RS256), Use: "sig",
	}}}
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		var value any
		switch request.URL.Path {
		case "/.well-known/openid-configuration":
			value = map[string]any{
				"issuer": issuer, "jwks_uri": issuer + "/keys",
				"id_token_signing_alg_values_supported": []string{"RS256"},
			}
		case "/keys":
			value = keySet
		default:
			http.NotFound(writer, request)
			return
		}
		if encodeErr := json.NewEncoder(writer).Encode(value); encodeErr != nil {
			t.Errorf("encode OIDC response: %v", encodeErr)
		}
	}))
	t.Cleanup(server.Close)
	issuer = server.URL
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: privateKey},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader(jose.HeaderKey("kid"), keyID),
	)
	if err != nil {
		t.Fatalf("create OIDC signer: %v", err)
	}
	token, err = jwt.Signed(signer).Claims(jwt.Claims{
		Issuer: issuer, Subject: "release-user", Audience: jwt.Audience{"jobman-control"},
		Expiry: jwt.NewNumericDate(time.Now().Add(5 * time.Minute)),
	}).Serialize()
	if err != nil {
		t.Fatalf("sign OIDC token: %v", err)
	}

	return issuer, token, server.Client().Transport
}

type testCA struct {
	certificate    *x509.Certificate
	privateKey     *ecdsa.PrivateKey
	certificatePEM []byte
	privateKeyPEM  []byte
}

func newTestCA(t *testing.T, commonName string) testCA {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate test CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: commonName},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, privateKey.Public(), privateKey)
	if err != nil {
		t.Fatalf("create test CA certificate: %v", err)
	}
	certificate, err := x509.ParseCertificate(certificateDER)
	if err != nil {
		t.Fatalf("parse test CA certificate: %v", err)
	}

	return testCA{
		certificate: certificate, privateKey: privateKey,
		certificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}),
		privateKeyPEM:  marshalPrivateKey(t, privateKey),
	}
}

func (authority testCA) issueServerCertificate(t *testing.T) (certificatePEM, privateKeyPEM []byte) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	certificateDER, err := x509.CreateCertificate(
		rand.Reader, template, authority.certificate, privateKey.Public(), authority.privateKey,
	)
	if err != nil {
		t.Fatalf("create server certificate: %v", err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}),
		marshalPrivateKey(t, privateKey)
}

func marshalPrivateKey(t *testing.T, privateKey *ecdsa.PrivateKey) []byte {
	t.Helper()
	encoded, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})
}

func writePrivateFile(t *testing.T, directory, name string, contents []byte) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write private test file: %v", err)
	}

	return path
}

func availableLoopbackAddress(ctx context.Context, t *testing.T) string {
	t.Helper()
	var listener net.ListenConfig
	reserved, err := listener.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback address: %v", err)
	}
	address := reserved.Addr().String()
	if err = reserved.Close(); err != nil {
		t.Fatalf("release loopback address: %v", err)
	}

	return address
}

func isolatedDatabaseURL(ctx context.Context, t *testing.T, databaseURL string) string {
	t.Helper()
	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal("parse integration database URL")
	}
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatalf("open integration administration pool: %v", err)
	}
	randomBytes := make([]byte, 8)
	if _, err = rand.Read(randomBytes); err != nil {
		t.Fatalf("create integration schema identifier: %v", err)
	}
	schema := "jobman_control_e2e_" + hex.EncodeToString(randomBytes)
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err = pool.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatalf("create integration schema: %v", err)
	}
	cleanupBaseContext := context.WithoutCancel(ctx)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(cleanupBaseContext, 10*time.Second)
		defer cleanupCancel()
		if _, cleanupErr := pool.Exec(cleanupContext, "DROP SCHEMA "+quotedSchema+" CASCADE"); cleanupErr != nil {
			t.Errorf("drop integration schema: %v", cleanupErr)
		}
		pool.Close()
	})
	parsed, err := url.Parse(databaseURL)
	if err != nil || parsed.Scheme == "" {
		t.Fatal("integration database setting must be a PostgreSQL URL")
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()

	return parsed.String()
}
