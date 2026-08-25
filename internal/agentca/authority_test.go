package agentca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

func TestAuthorityIssuesBoundClientCertificate(t *testing.T) {
	now := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	certificatePEM, keyPEM := testCA(t, now)
	authority, err := Parse(certificatePEM, keyPEM, func() time.Time { return now })
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	agentKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	requestDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "worker-a"},
	}, agentKey)
	if err != nil {
		t.Fatalf("CreateCertificateRequest() error = %v", err)
	}
	requestPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: requestDER})
	const agentID = "77777777-7777-4777-8777-777777777777"
	credential, err := authority.Issue(string(requestPEM), agentID, time.Hour)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	block, _ := pem.Decode([]byte(credential.CertificatePEM))
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate() error = %v", err)
	}
	if _, err = certificate.Verify(x509.VerifyOptions{
		Roots: authority.CertificatePool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		CurrentTime: now.Add(time.Minute),
	}); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	actualAgentID, err := AgentID(certificate)
	if err != nil || actualAgentID != agentID {
		t.Fatalf("AgentID() = %q, %v", actualAgentID, err)
	}
	digest, err := PublicKeyDigest(agentKey.Public())
	if err != nil || digest != credential.PublicKeyDigest {
		t.Fatalf("PublicKeyDigest() = %q, %v, want %q", digest, err, credential.PublicKeyDigest)
	}
	if !credential.ExpiresAt.Equal(now.Add(time.Hour)) || !certificate.NotAfter.Equal(credential.ExpiresAt) {
		t.Fatalf("credential expiration = %v, certificate = %v", credential.ExpiresAt, certificate.NotAfter)
	}
}

func TestAuthorityRejectsInvalidMaterial(t *testing.T) {
	now := time.Now().UTC()
	certificatePEM, keyPEM := testCA(t, now)
	otherCertificate, _ := testCA(t, now)
	if _, err := Parse(otherCertificate, keyPEM, time.Now); err == nil {
		t.Fatal("Parse() accepted mismatched key")
	}
	authority, err := Parse(certificatePEM, keyPEM, time.Now)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if _, err = authority.Issue("not a request", "agent", time.Hour); err == nil {
		t.Fatal("Issue() accepted malformed CSR")
	}
	if _, err = AgentID(&x509.Certificate{}); err == nil {
		t.Fatal("AgentID() accepted missing URI")
	}
}

func testCA(t *testing.T, now time.Time) (certificatePEM, privateKeyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Jobman agent test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatalf("CreateCertificate() error = %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey() error = %v", err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}
